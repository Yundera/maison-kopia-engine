package kopia

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// connect.json is what lets `recover` repeat the host's connect without the host, so it
// must produce exactly the arguments `connect` would — the identity above all, since a
// recovery that drifted it would open a second lineage on the way back in.
func TestConnectFileBecomesStorageArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want []string
	}{
		{"s3", `{"bucket":"b","endpoint":"https://s3.fr-par.scw.cloud","region":"fr-par","prefix":"s/1/","hostname":"dev1","username":"pcs"}`,
			[]string{"s3", "--bucket=b", "--endpoint=s3.fr-par.scw.cloud", "--region=fr-par", "--prefix=s/1/",
				"--override-hostname=dev1", "--override-username=pcs"}},
		{"filesystem", `{"path":"/r/repo","hostname":"dev1","username":"pcs","bucket":"ignored"}`,
			[]string{"filesystem", "--path=/r/repo", "--override-hostname=dev1", "--override-username=pcs"}},
	} {
		got, err := parseConnect([]byte(tc.json))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// An unusable connect.json is the host not having written what a recovery needs yet —
// not configured, never an ordinary failure.
func TestUnusableConnectFileIsNotConfigured(t *testing.T) {
	for _, raw := range []string{`not json`, `{"bucket":"b","endpoint":"https://s3"}`, `{"hostname":"h","username":"u"}`} {
		if _, err := parseConnect([]byte(raw)); !errors.Is(err, proto.ErrNotConfigured) {
			t.Errorf("parseConnect(%s) = %v, want ErrNotConfigured", raw, err)
		}
	}
}

// The wrong-key answer is read from kopia's own text, so what it matches is pinned.
func TestWrongPasswordIsRecognised(t *testing.T) {
	yes := errors.New("kopia repository: exit status 1: failed to open repository: unable to create format manager: " +
		"invalid repository password | error connecting to repository: unable to create format manager: Invalid Repository Password")
	if !isWrongPassword(yes) {
		t.Error("kopia's wrong-password failure was not recognised")
	}
	for _, no := range []error{nil, errors.New("kopia repository: exit status 1: unable to connect: dial tcp: i/o timeout")} {
		if isWrongPassword(no) {
			t.Errorf("%v was read as a wrong password", no)
		}
	}
}

// A needs-recovery box is neither unprovisioned nor unreachable, and must not be
// reported as either: it is the box that takes no backups while every self-check passes.
func TestStatusReportsTheRecoveryMarker(t *testing.T) {
	e := testEngine(t)
	write(t, e.MarkerFile(), `{"reason":"repository-exists"}`)
	st := e.Status(context.Background())
	if !st.NeedsRecovery || st.Configured || st.Connected || st.Detail == "" {
		t.Errorf("status = %+v, want needsRecovery with a detail", st)
	}
}

// promote is the moment a typed key becomes the repository's, so it has to land whole,
// private, and owned by whoever owns the directory.
func TestPromoteInstallsTheKeyForTheDirectoryOwner(t *testing.T) {
	e := testEngine(t)
	write(t, e.ConfigFile(), `{}`)
	if err := e.promote("s3cret"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.PasswordFile())
	if err != nil || string(b) != "s3cret" {
		t.Fatalf("repository.password = %q, %v", b, err)
	}
	fi, _ := os.Stat(e.PasswordFile())
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("repository.password mode = %v, want 0600", fi.Mode().Perm())
	}
	du, dg, _ := owner(e.Dir)
	for _, f := range []string{e.PasswordFile(), e.ConfigFile()} {
		if u, g, _ := owner(f); u != du || g != dg {
			t.Errorf("%s owned by %d:%d, want the directory's %d:%d", filepath.Base(f), u, g, du, dg)
		}
	}
	if _, err := os.Stat(e.PasswordFile() + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary key file was left behind")
	}
}

// A wrong key changes nothing but the candidate: no configuration, no password, and the
// marker still there for the next attempt.
func TestRecoverWithAWrongKeyChangesNothing(t *testing.T) {
	e := recoveryBox(t, "wrong")
	_, err := e.Recover(context.Background())
	if !errors.Is(err, proto.ErrWrongPassword) || proto.CodeFor(err) != proto.ExitWrongPassword {
		t.Fatalf("Recover = %v, want ErrWrongPassword", err)
	}
	for _, f := range []string{e.CandidateFile(), e.ConfigFile(), e.PasswordFile()} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s exists after a wrong key", filepath.Base(f))
		}
	}
	if !e.needsRecovery() {
		t.Error("the marker was removed after a wrong key")
	}
}

// The right key: promoted, marker gone, and every snapshot pinned in batches.
func TestRecoverWithTheRightKey(t *testing.T) {
	e := recoveryBox(t, "right")
	res, err := e.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshots != 3 || res.Pinned != 3 {
		t.Errorf("result = %+v, want 3 snapshots, 3 pinned", res)
	}
	if b, _ := os.ReadFile(e.PasswordFile()); string(b) != "right" {
		t.Errorf("repository.password = %q", b)
	}
	if e.needsRecovery() {
		t.Error("the marker survived a recovery")
	}
	if _, err := os.Stat(e.CandidateFile()); !os.IsNotExist(err) {
		t.Error("the candidate survived a recovery")
	}
	pins, _ := os.ReadFile(filepath.Join(e.Dir, "pins"))
	if got := strings.TrimSpace(string(pins)); got != "--add=maison-recovered a b c" {
		t.Errorf("pin call = %q", got)
	}
	if st := e.Status(context.Background()); !st.Configured || st.NeedsRecovery {
		t.Errorf("status after recovery = %+v", st)
	}
}

// No candidate is nothing to try, and the configured box is nothing to recover.
func TestRecoverRefusesWithoutACandidateOrOnAConnectedBox(t *testing.T) {
	e := testEngine(t)
	if _, err := e.Recover(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("no candidate: %v, want ErrNotConfigured", err)
	}
	write(t, e.CandidateFile(), "k")
	write(t, e.ConfigFile(), `{"hostname":"h","username":"u"}`)
	_, err := e.Recover(context.Background())
	if err == nil || proto.CodeFor(err) != proto.ExitError {
		t.Errorf("connected box: %v, want an ordinary failure", err)
	}
	if _, err := os.Stat(e.CandidateFile()); !os.IsNotExist(err) {
		t.Error("the candidate survived a refused recovery")
	}
}

func testEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{Dir: t.TempDir(), Out: proto.NewEmitter(&bytes.Buffer{})}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// recoveryBox is a box in the needs-recovery state, with a stand-in kopia whose
// repository opens with "right": connect writes the config, list returns three
// snapshots, pin records its arguments.
func recoveryBox(t *testing.T, candidate string) *Engine {
	t.Helper()
	e := testEngine(t)
	e.Bin = filepath.Join(e.Dir, "kopia-stub")
	write(t, e.MarkerFile(), `{"reason":"repository-exists"}`)
	write(t, e.ConnectFile(), `{"path":"/nowhere","hostname":"dev1","username":"pcs"}`)
	write(t, e.CandidateFile(), candidate+"\n")
	stub := `#!/bin/sh
cfg=""; for a in "$@"; do case "$a" in --config-file=*) cfg="${a#--config-file=}";; esac; done
case "$1 $2" in
"repository connect")
	[ "$KOPIA_PASSWORD" = right ] || { echo "error connecting to repository: unable to create format manager: invalid repository password" >&2; exit 1; }
	echo '{"hostname":"dev1","username":"pcs","storage":{"type":"filesystem","config":{}}}' > "$cfg";;
"repository status") ;;
"snapshot list") echo '[{"id":"a"},{"id":"b"},{"id":"c"}]';;
"snapshot pin") shift 2; echo "$@" | sed 's/ --config-file=.*//' >> "$(dirname "$cfg")/pins";;
*) exit 2;;
esac
`
	if err := os.WriteFile(e.Bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}
