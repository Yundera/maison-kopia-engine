package kopia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Version is this adapter's own version, distinct from the kopia it wraps.
const Version = "0.1.0"

// Protocol is the adapter protocol version implemented here. Maison refuses an adapter
// speaking a dialect it does not know rather than guessing — an unknown dialect is
// exactly the case where "try it and see" costs a backup.
const Protocol = "v0"

// EngineID is permanent. It is recorded on every backup this engine writes and is how
// those backups are found again after a user switches engines, so it can never change.
const EngineID = "kopia"

// Caps is what this engine can do.
func (e *Engine) Caps(ctx context.Context) proto.Caps {
	c := proto.Caps{
		EngineID:       EngineID,
		AdapterVersion: Version,
		Protocol:       Protocol,

		Offsite:   true,
		Encrypted: true,
		// The key is generated on the box and exists nowhere else, so losing the box
		// loses every snapshot in the repository unless a copy left first.
		KeyEscrow: true,
		// Restoring is a download, never a rename.
		InstantRestore: false,
		// Snapshots stream straight to the repository, so a backup needs no free disk
		// proportional to the source.
		NeedsLocalSpace: false,
		InPlaceRestore:  true,
		// Content-addressed, so pass 2 against the same source uploads only what changed
		// during pass 1. Nothing to implement — and precisely what an rclone-style
		// adapter would have to answer false to.
		IncrementalPasses: true,
		// Snapshots are read from the folder and streamed; the folder is left where it
		// is for Maison to remove.
		ConsumesSource: false,
		Retention:      true,
		// Independently deletable generations: any snapshot may be dropped and the ones
		// either side stay restorable, so tiered retention works in full.
		RetentionModel: "snapshot",
	}
	if v, err := e.RunBare(ctx, "--version"); err == nil {
		c.EngineVersion = strings.TrimSpace(strings.SplitN(string(v), " ", 2)[0])
	}
	return c
}

// repoConfig is the part of kopia's own config file this reads. The identity fields are
// written by `repository connect --override-hostname/--username` and never rewritten.
type repoConfig struct {
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Storage  struct {
		Type string `json:"type"`
	} `json:"storage"`
}

func (e *Engine) readConfig() (repoConfig, error) {
	var rc repoConfig
	b, err := os.ReadFile(e.ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return rc, proto.ErrNotConfigured
		}
		return rc, err
	}
	if err := json.Unmarshal(b, &rc); err != nil {
		return rc, fmt.Errorf("unreadable repository config: %w", err)
	}
	return rc, nil
}

// Status probes the repository.
//
// Configured and Connected are answered separately and must stay that way: not
// configured is a box awaiting provisioning, unreachable is a fault.
func (e *Engine) Status(ctx context.Context) proto.Status {
	rc, err := e.readConfig()
	if err != nil {
		return proto.Status{Detail: detailFor(err)}
	}
	identity := rc.Username + "@" + rc.Hostname
	if _, err := e.Run(ctx, "repository", "status", "--json"); err != nil {
		return proto.Status{Configured: true, Identity: identity, Detail: err.Error()}
	}
	return proto.Status{Configured: true, Connected: true, Identity: identity}
}

func detailFor(err error) string {
	if err == proto.ErrNotConfigured {
		return "no repository configured on this box"
	}
	return err.Error()
}

// Prepare is a cheap readiness probe. Its failure is advisory.
func (e *Engine) Prepare(ctx context.Context) error {
	if _, err := e.readConfig(); err != nil {
		return err
	}
	_, err := e.Run(ctx, "repository", "status")
	return err
}

// ConnectOpts is what `connect` needs. It is S3-shaped because that is the only storage
// the deployment issues; a filesystem repository is connected by hand in development.
type ConnectOpts struct {
	Bucket   string
	Endpoint string // a URL — see below
	Region   string
	Prefix   string
	Hostname string // the identity to pin, never derived here
	Username string
	Path     string // filesystem repository, for development
}

// Connect creates the repository if the storage is empty and connects to it if it is
// not, idempotently.
//
// It never touches an existing configuration. Identity is written exactly ONCE, and
// that is the whole point: kopia keys snapshots user@host:path, and a reconnect that
// lets the identity drift opens a second lineage inside one repository — the old one
// covered by policies nothing writes to, the new one covered by nothing at all. Both
// list normally, and nothing surfaces it until the storage bill grows or a restore
// comes back empty.
//
// ErrRepositoryExists is returned when the storage already holds a repository this box
// has no password for: a rebuilt box. Initialising a second repository under the same
// prefix would be the destructive answer, so it stops instead.
func (e *Engine) Connect(ctx context.Context, o ConnectOpts) error {
	if _, err := e.readConfig(); err == nil {
		// Already connected. Nothing to do, and rewriting the file is the one thing that
		// must never happen.
		return nil
	} else if err != proto.ErrNotConfigured {
		return err
	}

	args, err := storageArgs(o)
	if err != nil {
		return err
	}

	// A password must already exist for a connect; only a create mints one, and minting
	// it is the host's job (it has to survive the create failing).
	if _, err := e.password(); err != nil {
		return err
	}

	// Connect first: the storage may already hold this box's repository, from a
	// reinstall that kept the data disk.
	if _, err := e.Run(ctx, append([]string{"repository", "connect"}, args...)...); err == nil {
		return e.blankPersistedCredentials()
	}

	out, err := e.Run(ctx, append([]string{"repository", "create"}, args...)...)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)+err.Error()), "found existing data") {
			return ErrRepositoryExists
		}
		return err
	}
	return e.blankPersistedCredentials()
}

// ErrRepositoryExists means the storage holds a repository this box cannot open. The
// caller writes the needs-recovery marker; creating a second one under the same prefix
// is never the answer.
//
// It is the protocol's error rather than one of this package's own, so that it carries
// the exit code the caller branches on — an adapter that invented its own would report
// a rebuilt box as an ordinary failure.
var ErrRepositoryExists = proto.ErrRepositoryExists

func storageArgs(o ConnectOpts) ([]string, error) {
	if o.Hostname == "" || o.Username == "" {
		return nil, fmt.Errorf("connect needs an explicit identity: hostname and username")
	}
	identity := []string{"--override-hostname=" + o.Hostname, "--override-username=" + o.Username}

	if o.Path != "" {
		return append([]string{"filesystem", "--path=" + o.Path}, identity...), nil
	}
	if o.Bucket == "" || o.Endpoint == "" {
		return nil, fmt.Errorf("connect needs either a filesystem path or a bucket and endpoint")
	}
	host, tls, err := endpointHost(o.Endpoint)
	if err != nil {
		return nil, err
	}
	args := []string{"s3",
		"--bucket=" + o.Bucket,
		"--endpoint=" + host,
		"--region=" + o.Region,
		"--prefix=" + o.Prefix,
	}
	if !tls {
		args = append(args, "--disable-tls")
	}
	return append(args, identity...), nil
}

// endpointHost converts a URL into what kopia's --endpoint actually parses: a
// host[:port], NOT a URL. Given a URL kopia fails with "Endpoint url is not valid".
//
// The credential API returns a URL because that contract is engine-independent, so the
// conversion belongs here — the one place that knows what kopia parses.
func endpointHost(raw string) (host string, tls bool, err error) {
	switch {
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimSuffix(strings.TrimPrefix(raw, "https://"), "/"), true, nil
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimSuffix(strings.TrimPrefix(raw, "http://"), "/"), false, nil
	case strings.Contains(raw, "://"):
		u, perr := url.Parse(raw)
		if perr != nil {
			return "", false, fmt.Errorf("unusable endpoint %q: %w", raw, perr)
		}
		return u.Host, u.Scheme != "http", nil
	default:
		// Already a bare host[:port]. TLS is the safe assumption: the alternative sends
		// credentials in clear to something that may well accept them.
		return strings.TrimSuffix(raw, "/"), true, nil
	}
}

// blankPersistedCredentials removes the storage key kopia wrote into its own config.
//
// kopia persists whatever it was given at connect time and offers no flag to stop it —
// --no-persist-credentials governs the repository *password*. Blanking them leaves the
// environment as their only source, which is what makes a 90-day rotation a rewrite of
// credentials.env alone rather than a reconnect that would rewrite the identity too.
//
// Non-fatal: a kopia release that reformats its configuration leaves the credentials in
// place, which is a warning and not a reason to fail a provision.
func (e *Engine) blankPersistedCredentials() error {
	b, err := os.ReadFile(e.ConfigFile())
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		e.Out.Log("warn", "could not blank the persisted storage credentials: "+err.Error())
		return nil
	}
	storage, _ := doc["storage"].(map[string]any)
	cfg, _ := storage["config"].(map[string]any)
	if cfg == nil {
		return nil
	}
	changed := false
	for _, k := range []string{"accessKeyID", "secretAccessKey", "sessionToken"} {
		if v, ok := cfg[k]; ok && v != "" {
			cfg[k] = ""
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		e.Out.Log("warn", "could not blank the persisted storage credentials: "+err.Error())
		return nil
	}
	return os.WriteFile(e.ConfigFile(), out, 0o600)
}
