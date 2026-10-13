package kopia

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AdapterFile is the descriptor Maison discovers engines by. It sits in the engine's own
// directory, and the directory's app folder name is the engine id.
func (e *Engine) AdapterFile() string { return filepath.Join(e.Dir, "adapter.json") }

// ServeOpts is what `serve` cannot learn from the repository: how the container it runs
// in was declared. None of it can be discovered from inside without the Docker socket,
// which this container must never have, so whoever declared the container states it.
type ServeOpts struct {
	// Image is the image reference this container was created from, exactly as the
	// compose file spells it. Maison compares it against the container before trusting
	// the descriptor.
	Image string
	// Container is the container's name — the one Maison `docker exec`s into.
	Container string
	// Entrypoint is this binary's path inside the image.
	Entrypoint string
	// Hostname is the container's own hostname: the identity a connect.json that names
	// none is given, and the one the resident container must match.
	Hostname string

	// Poll is how often a box still waiting for its files looks again. Zero is a minute.
	Poll time.Duration
	// Reap, when set, collects orphaned children. It runs on the serve goroutine, never
	// beside a kopia call of serve's own, so it cannot steal that child's exit status.
	Reap func()
	// Child, when set, fires whenever an orphan may be waiting to be reaped.
	Child <-chan os.Signal
}

func (o ServeOpts) poll() time.Duration {
	if o.Poll > 0 {
		return o.Poll
	}
	return time.Minute
}

// maxBackoff caps the retry interval after a connect that failed for a reason that is
// not the files' absence — an unreachable endpoint, a refused key. Long enough not to
// hammer a bucket, short enough that a fixed credentials.env is picked up the same hour.
const maxBackoff = 15 * time.Minute

// Serve is the resident engine container's main process.
//
// On a deployment with a host side, the host provisions the repository and this
// container only sleeps and is exec'd into. On a box without one — a store-installed
// engine — nothing else will ever run `connect`, so this does: it turns connect.json and
// credentials.env, dropped into the engine directory by the owner, into a connected
// repository and a descriptor Maison can discover, and then waits to be exec'd into
// exactly as `sleep infinity` did.
//
// IT NEVER EXITS NON-ZERO. The container restarts on exit, and a provisioning problem is
// not cured by a restart loop — it is cured by the owner fixing a file, which the next
// look picks up. Cancellation (SIGTERM from `docker stop`) returns nil.
func (e *Engine) Serve(ctx context.Context, o ServeOpts) error {
	wait := o.poll()
	done, due := false, true
	for {
		if !done && due {
			due = false
			var retry bool
			done, retry = e.Bootstrap(ctx, o)
			switch {
			case done:
			case retry:
				// A real failure: back off, so a box with a wrong key does not
				// attempt a connect a minute for a week.
				wait = min(wait*2, maxBackoff)
			default:
				wait = o.poll()
			}
		}
		var (
			tick  <-chan time.Time
			timer *time.Timer
		)
		if !done {
			timer = time.NewTimer(wait)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-o.Child:
			if o.Reap != nil {
				o.Reap()
			}
		case <-tick:
			due = true
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Bootstrap is one look at the engine directory. done means there is nothing left to
// provision; retry means a connect was attempted and failed, as opposed to the files not
// being there yet.
//
// It ports the host's ensure-backup-config.sh, and in the same order, because the order
// is the safety: a needs-recovery marker is honoured before anything else, an existing
// configuration is never connected over, and the password is on disk before the create
// that would make it the only key to the repository.
func (e *Engine) Bootstrap(ctx context.Context, o ServeOpts) (done, retry bool) {
	// The storage holds a repository this box has no key for. Nothing here can fix
	// that; Maison's key entry can, and it needs the descriptor to find the engine.
	if e.needsRecovery() {
		e.writeAdapter(o, nil)
		return true, false
	}
	// Already provisioned — by an earlier run, or by a host side. Never reconnected.
	if _, err := e.readConfig(); err == nil {
		e.writeAdapter(o, nil)
		return true, false
	}

	b, err := os.ReadFile(e.ConnectFile())
	if err != nil {
		if os.IsNotExist(err) {
			e.Out.Log("info", "waiting for connect.json and credentials.env in "+e.Dir)
		} else {
			e.Out.Log("warn", "reading connect.json: "+err.Error())
		}
		return false, false
	}
	opts, err := connectOpts(b)
	if err != nil {
		e.Out.Log("warn", err.Error())
		return false, false
	}
	if opts.Username == "" {
		opts.Username = "pcs"
	}
	if opts.Hostname == "" {
		opts.Hostname = o.Hostname
	} else if o.Hostname != "" && opts.Hostname != o.Hostname {
		// Not refused: the snapshots are still filed under one identity. But Maison
		// only execs into a container whose hostname matches, so every command would
		// pay for a one-shot container instead.
		e.Out.Log("warn", fmt.Sprintf("connect.json names hostname %q but this container is %q; Maison will not exec into it", opts.Hostname, o.Hostname))
	}

	minted := false
	if _, err := os.Stat(e.PasswordFile()); os.IsNotExist(err) {
		pw, err := newPassword()
		if err != nil {
			e.Out.Log("error", err.Error())
			return false, true
		}
		// Written BEFORE the create: once the create succeeds this file is the only
		// key to every snapshot, and a crash between the two must not lose it.
		if err := installFile(e.PasswordFile(), e.Dir, []byte(pw), 0o600); err != nil {
			e.Out.Log("error", "writing the repository password: "+err.Error())
			return false, true
		}
		minted = true
	}

	err = e.Connect(ctx, opts)
	switch {
	case err == nil:
		if uid, gid, ok := owner(e.Dir); ok {
			if err := os.Chown(e.ConfigFile(), uid, gid); err != nil {
				e.Out.Log("warn", "handing the repository config to its owner: "+err.Error())
			}
		}
		e.writeAdapter(o, &opts)
		e.Out.Log("info", "repository connected as "+opts.Username+"@"+opts.Hostname)
		return true, false

	case errors.Is(err, ErrRepositoryExists):
		// A password minted a moment ago opens nothing, and leaving it would read as
		// "configured" to everything that only checks the file is there.
		if minted {
			os.Remove(e.PasswordFile())
		}
		e.writeMarker(opts)
		e.writeAdapter(o, &opts)
		e.Out.Log("warn", "the storage already holds a repository this box has no password for; enter the key in Maison")
		return true, false

	case ctx.Err() != nil:
		return false, false

	default:
		// The minted password is kept: the next attempt must use the same one, or a
		// create that half-succeeded would be stranded behind a key nobody has.
		e.Out.Log("error", "connect failed: "+err.Error())
		return false, true
	}
}

// newPassword is the same shape the host mints, `openssl rand -base64 33`: 44 characters
// with no padding and nothing a shell or an env file would mangle.
func newPassword() (string, error) {
	b := make([]byte, 33)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating the repository password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// adapterDescriptor is adapter.json, field for field what the host script writes.
type adapterDescriptor struct {
	EngineID   string        `json:"engineId"`
	Image      string        `json:"image"`
	Container  string        `json:"container"`
	Entrypoint string        `json:"entrypoint"`
	Hostname   string        `json:"hostname"`
	Network    string        `json:"network"`
	Secret     adapterSecret `json:"secret"`
}

type adapterSecret struct {
	Label  string `json:"label"`
	File   string `json:"file"`
	Escrow bool   `json:"escrow"`
}

// writeAdapter writes the descriptor Maison discovers this engine by.
//
// The identity is READ from repository.config when there is one, never recomputed, for
// the reason the host gives: two sides deriving it independently is how one repository
// ends up with two lineages. connect.json, then the container's own hostname, stand in
// only while there is no configuration (the needs-recovery state).
//
// Rewritten only when it would change, so a restart does not look like a new engine.
func (e *Engine) writeAdapter(o ServeOpts, c *ConnectOpts) {
	if o.Image == "" || o.Container == "" {
		e.Out.Log("error", "not writing adapter.json: MAISON_ENGINE_IMAGE and MAISON_ENGINE_CONTAINER must both be set")
		return
	}
	d := adapterDescriptor{
		EngineID:   EngineID,
		Image:      o.Image,
		Container:  o.Container,
		Entrypoint: o.Entrypoint,
		Hostname:   o.Hostname,
		Network:    "default",
		Secret:     adapterSecret{Label: "Kopia repository password", File: "repository.password", Escrow: true},
	}
	if c == nil {
		if b, err := os.ReadFile(e.ConnectFile()); err == nil {
			if parsed, err := connectOpts(b); err == nil {
				c = &parsed
			}
		}
	}
	if c != nil {
		if c.Hostname != "" {
			d.Hostname = c.Hostname
		}
		if c.Path != "" {
			d.Network = "none"
		}
	}
	if rc, err := e.readConfig(); err == nil {
		if rc.Hostname != "" {
			d.Hostname = rc.Hostname
		}
		d.Network = "default"
		if rc.Storage.Type == "filesystem" {
			// A repository on a local path must not be given networking.
			d.Network = "none"
		}
	}

	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		e.Out.Log("error", "encoding adapter.json: "+err.Error())
		return
	}
	b = append(b, '\n')
	if cur, err := os.ReadFile(e.AdapterFile()); err == nil && bytes.Equal(cur, b) {
		return
	}
	if err := installFile(e.AdapterFile(), e.Dir, b, 0o644); err != nil {
		e.Out.Log("error", "writing adapter.json: "+err.Error())
	}
}

// writeMarker records that the storage holds a repository this box cannot open, in the
// shape the host writes. Presence is the signal; the contents are diagnostics.
func (e *Engine) writeMarker(c ConnectOpts) {
	b, _ := json.Marshal(map[string]string{
		"reason":     "repository-exists-without-password",
		"prefix":     c.Prefix,
		"detectedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err := installFile(e.MarkerFile(), e.Dir, append(b, '\n'), 0o644); err != nil {
		e.Out.Log("error", "writing the needs-recovery marker: "+err.Error())
	}
}

// installFile writes a file beside its destination and renames it over it, so no reader
// sees half of it, then gives it to the owner of dir — the same rule promote follows,
// and for the same reason: the directory is the one thing whoever set it up is certain
// to have chowned.
func installFile(path, dir string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	// WriteFile only applies the mode on create.
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if uid, gid, ok := owner(dir); ok {
		if err := os.Chown(tmp, uid, gid); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
