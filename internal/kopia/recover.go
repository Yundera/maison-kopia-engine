package kopia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Files `recover` works from. The host writes the marker and connect.json; Maison
// writes the candidate.
func (e *Engine) MarkerFile() string    { return filepath.Join(e.Dir, "needs-recovery") }
func (e *Engine) ConnectFile() string   { return filepath.Join(e.Dir, "connect.json") }
func (e *Engine) CandidateFile() string { return filepath.Join(e.Dir, "repository.password.candidate") }

// recoveredTag is the pin every snapshot found by a recovery is given.
//
// A restored box is usually empty when the key goes in, and the next scheduled backups
// would capture those empty apps and push the real ones out of retention. A pin is
// kopia's own "never expire this", so the snapshots the user came back for outlive any
// policy — and the pin is visible, and removable, in kopia's UI.
const recoveredTag = "maison-recovered"

// pinBatch is how many snapshot ids go on one `snapshot pin` command line. A repository
// can hold thousands; one command per id is a round trip each, and one command for all
// of them is an argv limit waiting to happen.
const pinBatch = 50

// needsRecovery reports the host's marker. Its contents are the host's diagnostics and
// are not read here: presence is the whole signal.
func (e *Engine) needsRecovery() bool {
	_, err := os.Stat(e.MarkerFile())
	return err == nil
}

// connectFile is connect.json: the storage parameters the host passed to `connect`,
// written down so `recover` can repeat them without the host. Non-secret — the storage
// key is credentials.env, the repository key is what is being recovered.
type connectFile struct {
	Bucket   string `json:"bucket"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Path     string `json:"path,omitempty"` // filesystem repository, for development
}

// parseConnect reads connect.json into the arguments `repository connect` takes.
//
// Anything unusable is ErrNotConfigured, not a fault: the host has not (yet) written
// what a recovery needs, and the same host run that writes it fixes it.
func parseConnect(b []byte) ([]string, error) {
	var c connectFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("unreadable connect.json: %v: %w", err, proto.ErrNotConfigured)
	}
	args, err := storageArgs(ConnectOpts{
		Bucket: c.Bucket, Endpoint: c.Endpoint, Region: c.Region, Prefix: c.Prefix,
		Hostname: c.Hostname, Username: c.Username, Path: c.Path,
	})
	if err != nil {
		return nil, fmt.Errorf("connect.json: %v: %w", err, proto.ErrNotConfigured)
	}
	return args, nil
}

// isWrongPassword recognises kopia refusing a key. Verified against kopia 0.23.1, whose
// `repository connect` with a wrong --password fails with
//
//	error connecting to repository: unable to create format manager: invalid repository password
//
// and leaves no configuration file behind.
func isWrongPassword(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "invalid repository password")
}

// Recover reconnects a rebuilt box to the repository its storage already holds, using
// the key the user entered.
//
// It is `connect` with the create half removed and the password supplied by the caller,
// and it exists because that is the one state `connect` must refuse: storage holding a
// repository this box has no key for (ErrRepositoryExists, the host's needs-recovery
// marker). Creating there would strand every snapshot behind a key nobody has; the
// mailed key is the only thing that opens them.
//
// The candidate is removed on every path. A wrong key changes nothing else; a right one
// becomes repository.password, the marker goes, and every snapshot found is pinned.
func (e *Engine) Recover(ctx context.Context) (proto.Recovered, error) {
	var res proto.Recovered

	b, err := os.ReadFile(e.CandidateFile())
	if err != nil {
		if os.IsNotExist(err) {
			return res, proto.ErrNotConfigured
		}
		return res, err
	}
	// Whatever happens next, a key the user typed does not stay on disk under a name
	// nothing else owns. On success it has been renamed and this is a no-op.
	defer os.Remove(e.CandidateFile())

	candidate := strings.TrimSpace(string(b))
	if candidate == "" {
		return res, proto.ErrWrongPassword
	}

	// A box that is already connected has nothing to recover, and the connect below
	// would rewrite its configuration — the one thing that must never happen.
	if _, err := e.readConfig(); err == nil && !e.needsRecovery() {
		return res, errors.New("already connected: this box has a working repository configuration")
	}

	cb, err := os.ReadFile(e.ConnectFile())
	if err != nil {
		if os.IsNotExist(err) {
			return res, fmt.Errorf("no connect.json: %w", proto.ErrNotConfigured)
		}
		return res, err
	}
	args, err := parseConnect(cb)
	if err != nil {
		return res, err
	}

	// Connect ONLY. A create here would answer a wrong key with a second, empty
	// repository under the same prefix.
	if _, err := e.runWithPassword(ctx, candidate, append([]string{"repository", "connect"}, args...)...); err != nil {
		if isWrongPassword(err) {
			return res, proto.ErrWrongPassword
		}
		return res, err
	}

	// From here the repository is open. A failure before the key is promoted would leave
	// a configuration with no password beside it, which reads as configured-but-broken
	// rather than as the recovery state it still is — so it is undone.
	if err := e.blankPersistedCredentials(); err != nil {
		e.undoConnect()
		return res, err
	}
	if err := e.promote(candidate); err != nil {
		e.undoConnect()
		return res, err
	}
	if err := os.Remove(e.MarkerFile()); err != nil && !os.IsNotExist(err) {
		// The box is recovered; a marker that outlives it is the host's to clear and
		// only costs a stale status until it does.
		e.Out.Log("warn", "removing the needs-recovery marker: "+err.Error())
	}

	res.Snapshots, res.Pinned = e.pinAll(ctx)
	return res, nil
}

// undoConnect removes what a connect wrote, so a recovery that failed half way leaves
// the box in the state it started in.
func (e *Engine) undoConnect() {
	if err := os.Remove(e.ConfigFile()); err != nil && !os.IsNotExist(err) {
		e.Out.Log("warn", "undoing the connect: "+err.Error())
	}
}

// promote makes the key repository.password: written beside it and renamed over it, so
// no reader ever sees a half-written key, then given to the directory's owner.
//
// The adapter runs as root, but repository.password and repository.config are read by
// kopia's UI container and by the host as the directory's owner. Ownership is copied
// from the directory rather than configured, because the directory is the one thing the
// host is certain to have set.
func (e *Engine) promote(password string) error {
	uid, gid, haveOwner := owner(e.Dir)

	tmp := e.PasswordFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(password), 0o600); err != nil {
		return fmt.Errorf("writing the repository password: %w", err)
	}
	// WriteFile only applies the mode on create.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if haveOwner {
		if err := os.Chown(tmp, uid, gid); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("handing the repository password to its owner: %w", err)
		}
	}
	if err := os.Rename(tmp, e.PasswordFile()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("installing the repository password: %w", err)
	}
	if haveOwner {
		if err := os.Chown(e.ConfigFile(), uid, gid); err != nil {
			return fmt.Errorf("handing the repository config to its owner: %w", err)
		}
	}
	return nil
}

// owner is the uid and gid of a path, when the platform says.
func owner(path string) (uid, gid int, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// pinAll pins every snapshot the repository holds — every source, both passes, whatever
// wrote it — and returns how many there were and how many are now pinned.
//
// Nothing here is fatal. The recovery already succeeded; a pin that failed is logged and
// shows in the result as the difference between the two numbers.
//
// `snapshot pin --add=<tag> <id>...`, verified against kopia 0.23.1: --add is
// repeatable, ids are positional, and adding a pin a snapshot already has is a no-op
// ("No change for snapshot"). Pinning REWRITES the manifest, so a pinned snapshot comes
// back under a new id — harmless here, since nothing outside this adapter holds kopia
// ids, but it means the ids listed below are stale once this returns.
func (e *Engine) pinAll(ctx context.Context) (total, pinned int) {
	snaps, err := e.listSnapshots(ctx)
	if err != nil {
		e.Out.Log("warn", "listing snapshots to pin: "+err.Error())
		return 0, 0
	}
	ids := make([]string, 0, len(snaps))
	for _, sn := range snaps {
		ids = append(ids, sn.ID)
	}
	for start := 0; start < len(ids); start += pinBatch {
		batch := ids[start:min(start+pinBatch, len(ids))]
		args := append([]string{"snapshot", "pin", "--add=" + recoveredTag}, batch...)
		if _, err := e.Run(ctx, args...); err != nil {
			e.Out.Log("warn", fmt.Sprintf("pinning %d snapshots: %s", len(batch), err.Error()))
			continue
		}
		pinned += len(batch)
	}
	return len(ids), pinned
}
