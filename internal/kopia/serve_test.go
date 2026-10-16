package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

var testServe = ServeOpts{
	Image:      "ghcr.io/yundera/maison-kopia-engine:1.3.0",
	Container:  "kopia-engine",
	Entrypoint: "/usr/local/bin/maison-engine",
	Hostname:   "kopia-pcs",
}

// serveBox is a fresh store-installed engine directory with a stand-in kopia: connect
// fails (the storage is empty), create writes a filesystem config — unless the storage
// is told to hold someone else's repository, in which case create says what kopia says.
// "stub-locked" makes connect refuse the key the way kopia does when a repository is
// there, which since 1.4.0 stops `connect` before any create.
func serveBox(t *testing.T) (*Engine, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	e := &Engine{Dir: t.TempDir(), Out: proto.NewEmitter(&log)}
	e.Bin = filepath.Join(e.Dir, "kopia-stub")
	stub := `#!/bin/sh
cfg=""; host=""; for a in "$@"; do case "$a" in --config-file=*) cfg="${a#--config-file=}";; --override-hostname=*) host="${a#--override-hostname=}";; esac; done
dir="$(dirname "$cfg")"
echo "$1 $2 pw=$KOPIA_PASSWORD" >> "$dir/calls"
case "$1 $2" in
"repository connect")
	[ -f "$dir/stub-locked" ] && echo "unable to create format manager: invalid repository password" >&2
	exit 1;;
"repository create")
	[ -f "$dir/stub-existing" ] && { echo "found existing data in storage" >&2; exit 1; }
	[ -f "$dir/stub-down" ] && { echo "dial tcp: connection refused" >&2; exit 1; }
	echo "{\"hostname\":\"$host\",\"username\":\"pcs\",\"storage\":{\"type\":\"filesystem\",\"config\":{}}}" > "$cfg";;
*) exit 2;;
esac
`
	if err := os.WriteFile(e.Bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return e, &log
}

func readAdapter(t *testing.T, e *Engine) adapterDescriptor {
	t.Helper()
	b, err := os.ReadFile(e.AdapterFile())
	if err != nil {
		t.Fatalf("adapter.json: %v", err)
	}
	var d adapterDescriptor
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestBootstrapMintsConnectsAndDescribesTheEngine(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo"}`)

	done, retry := e.Bootstrap(context.Background(), testServe)
	if !done || retry {
		t.Fatalf("done=%v retry=%v, want a finished bootstrap", done, retry)
	}

	pw, err := os.ReadFile(e.PasswordFile())
	if err != nil || len(pw) != 44 || strings.ContainsAny(string(pw), "\n ") {
		t.Fatalf("password %q (%v), want 44 characters and no newline", pw, err)
	}
	if fi, _ := os.Stat(e.PasswordFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("password mode %v, want 0600", fi.Mode().Perm())
	}
	if !exists(e.ConfigFile()) {
		t.Fatal("no repository.config after a successful create")
	}
	calls, _ := os.ReadFile(filepath.Join(e.Dir, "calls"))
	if !strings.Contains(string(calls), "repository create pw="+string(pw)) {
		t.Errorf("create did not run with the minted password: %s", calls)
	}

	d := readAdapter(t, e)
	want := adapterDescriptor{
		EngineID: "kopia", Image: testServe.Image, Container: "kopia-engine",
		Entrypoint: testServe.Entrypoint,
		// connect.json named no hostname, so the container's own is the identity.
		Hostname: "kopia-pcs",
		Network:  "none",
		Secret:   adapterSecret{Label: "Kopia repository password", File: "repository.password", Escrow: true},
	}
	if d != want {
		t.Errorf("adapter.json = %+v\nwant %+v", d, want)
	}
	if fi, _ := os.Stat(e.AdapterFile()); fi.Mode().Perm() != 0o644 {
		t.Errorf("adapter.json mode %v, want 0644", fi.Mode().Perm())
	}
}

func TestBootstrapStopsAtSomeoneElsesRepository(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo","hostname":"kopia-pcs"}`)
	write(t, filepath.Join(e.Dir, "stub-existing"), "")

	done, retry := e.Bootstrap(context.Background(), testServe)
	if !done || retry {
		t.Fatalf("done=%v retry=%v, want a finished bootstrap", done, retry)
	}
	if exists(e.PasswordFile()) {
		t.Error("the password minted for a create that could not happen was kept")
	}
	if !exists(e.MarkerFile()) {
		t.Error("no needs-recovery marker")
	}
	if exists(e.ConfigFile()) {
		t.Error("a configuration was written over someone else's repository")
	}
	if d := readAdapter(t, e); d.Hostname != "kopia-pcs" {
		t.Errorf("adapter.json hostname %q, want the connect.json one", d.Hostname)
	}
	if !e.Status(context.Background()).NeedsRecovery {
		t.Error("status does not report the box as needing recovery")
	}
}

func TestBootstrapKeepsAPasswordItDidNotMint(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo"}`)
	write(t, e.PasswordFile(), "the-owners-own")
	write(t, filepath.Join(e.Dir, "stub-existing"), "")

	e.Bootstrap(context.Background(), testServe)
	if b, _ := os.ReadFile(e.PasswordFile()); string(b) != "the-owners-own" {
		t.Errorf("password = %q, want the owner's own kept", b)
	}
}

func TestBootstrapNeverTouchesAnExistingConfiguration(t *testing.T) {
	e, _ := serveBox(t)
	cfg := `{"hostname":"host-provisioned","username":"pcs","storage":{"type":"s3","config":{}}}`
	write(t, e.ConfigFile(), cfg)
	write(t, e.PasswordFile(), "k")
	write(t, e.ConnectFile(), `{"path":"/elsewhere","hostname":"other"}`)

	if done, _ := e.Bootstrap(context.Background(), testServe); !done {
		t.Fatal("an already-provisioned box is not done")
	}
	if exists(filepath.Join(e.Dir, "calls")) {
		t.Error("kopia was invoked on a box that already has a repository")
	}
	if b, _ := os.ReadFile(e.ConfigFile()); string(b) != cfg {
		t.Error("repository.config was rewritten")
	}
	d := readAdapter(t, e)
	if d.Hostname != "host-provisioned" || d.Network != "default" {
		t.Errorf("adapter.json = %+v, want the identity and storage read from repository.config", d)
	}
}

func TestBootstrapWaitsForItsFiles(t *testing.T) {
	e, log := serveBox(t)

	done, retry := e.Bootstrap(context.Background(), testServe)
	if done || retry {
		t.Fatalf("done=%v retry=%v, want waiting without a failure", done, retry)
	}
	for _, f := range []string{e.PasswordFile(), e.AdapterFile(), e.ConfigFile()} {
		if exists(f) {
			t.Errorf("%s written before connect.json existed", filepath.Base(f))
		}
	}
	if !strings.Contains(log.String(), "waiting for connect.json") {
		t.Errorf("nothing tells the owner what to drop: %s", log)
	}
}

func TestBootstrapRetriesAnUnreachableStorageWithTheSamePassword(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo"}`)
	write(t, filepath.Join(e.Dir, "stub-down"), "")

	done, retry := e.Bootstrap(context.Background(), testServe)
	if done || !retry {
		t.Fatalf("done=%v retry=%v, want a retry", done, retry)
	}
	first, _ := os.ReadFile(e.PasswordFile())
	if len(first) == 0 {
		t.Fatal("the minted password was dropped after a failed connect")
	}

	os.Remove(filepath.Join(e.Dir, "stub-down"))
	if done, _ := e.Bootstrap(context.Background(), testServe); !done {
		t.Fatal("the second attempt did not finish")
	}
	if second, _ := os.ReadFile(e.PasswordFile()); string(second) != string(first) {
		t.Error("a second password was minted for the same repository")
	}
}

func TestBootstrapOnlyWarnsAboutAHostnameMismatch(t *testing.T) {
	e, log := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo","hostname":"chosen"}`)

	if done, _ := e.Bootstrap(context.Background(), testServe); !done {
		t.Fatal("a hostname mismatch stopped the bootstrap")
	}
	if !strings.Contains(log.String(), `names hostname \"chosen\"`) {
		t.Errorf("no warning about the mismatch: %s", log)
	}
	if d := readAdapter(t, e); d.Hostname != "chosen" {
		t.Errorf("adapter.json hostname %q, want the one the repository was created with", d.Hostname)
	}
}

func TestNoDescriptorWithoutTheContainersDeclaration(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo"}`)

	o := testServe
	o.Container = ""
	e.Bootstrap(context.Background(), o)
	if exists(e.AdapterFile()) {
		t.Error("adapter.json written without MAISON_ENGINE_CONTAINER")
	}
}

func TestServeReturnsCleanlyWhenStopped(t *testing.T) {
	e, _ := serveBox(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- e.Serve(ctx, ServeOpts{Poll: time.Hour}) }()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve = %v, want nil on a stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// Since 1.4.0 `connect` answers a refused key with 14 instead of falling through to a
// create; serve must still read that as someone else's repository and stop.
func TestBootstrapStopsAtARepositoryThatRefusesTheKey(t *testing.T) {
	e, _ := serveBox(t)
	write(t, e.ConnectFile(), `{"path":"/repo","hostname":"kopia-pcs"}`)
	write(t, filepath.Join(e.Dir, "stub-locked"), "")

	done, retry := e.Bootstrap(context.Background(), testServe)
	if !done || retry {
		t.Fatalf("done=%v retry=%v, want a finished bootstrap", done, retry)
	}
	if exists(e.PasswordFile()) || !exists(e.MarkerFile()) || exists(e.ConfigFile()) {
		t.Error("want the minted password gone, a needs-recovery marker and no configuration")
	}
	if calls, _ := os.ReadFile(filepath.Join(e.Dir, "calls")); strings.Contains(string(calls), "repository create") {
		t.Errorf("a create was attempted against a repository that refused the key: %s", calls)
	}
}
