package kopia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// keyedBox is a connected box whose stand-in kopia opens the repository only with the
// key held in <dir>/repo-key — the repository's side of the password, which
// change-password rewrites. A file named "die-after-change" makes change-password
// rewrite the key and then fail, which is the interrupted change SettleNext exists for;
// "offline" makes every command fail as unreachable storage does.
func keyedBox(t *testing.T, current string) *Engine {
	t.Helper()
	e := testEngine(t)
	e.Bin = filepath.Join(e.Dir, "kopia-stub")
	write(t, e.ConfigFile(), `{"hostname":"dev1","username":"pcs","storage":{"type":"filesystem"}}`)
	write(t, e.PasswordFile(), current)
	write(t, filepath.Join(e.Dir, "repo-key"), current)
	stub := `#!/bin/sh
cfg=""; for a in "$@"; do case "$a" in --config-file=*) cfg="${a#--config-file=}";; esac; done
dir="$(dirname "$cfg")"
[ -e "$dir/offline" ] && { echo "unable to connect: dial tcp: i/o timeout" >&2; exit 1; }
[ "$KOPIA_PASSWORD" = "$(cat "$dir/repo-key")" ] || { echo "unable to create format manager: invalid repository password" >&2; exit 1; }
case "$1 $2" in
"repository status") ;;
"repository change-password")
	printf '%s' "$KOPIA_NEW_PASSWORD" > "$dir/repo-key"
	echo "$*" >> "$dir/argv"
	[ -e "$dir/die-after-change" ] && { echo "killed" >&2; exit 1; }
	;;
*) exit 2;;
esac
`
	if err := os.WriteFile(e.Bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	return string(b)
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s is still there", filepath.Base(path))
	}
}

// The ordinary change: the repository and the file both move to the new key, the new
// key never appears on kopia's command line, and nothing is left behind.
func TestChangeSecretInstallsTheNewKey(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, e.NextFile(), "new\n")
	if err := e.ChangeSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(e.Dir, "repo-key")); got != "new" {
		t.Errorf("repository key = %q, want new", got)
	}
	if got := read(t, e.PasswordFile()); got != "new" {
		t.Errorf("repository.password = %q, want new", got)
	}
	if fi, _ := os.Stat(e.PasswordFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("repository.password mode = %v", fi.Mode().Perm())
	}
	if strings.Contains(read(t, filepath.Join(e.Dir, "argv")), "new") {
		t.Error("the new key was passed on the command line")
	}
	gone(t, e.NextFile())
}

// A box whose own key no longer opens the repository cannot change it: wrong password,
// nothing changed, and nothing kept that a later settle could misread.
func TestChangeSecretWithAStaleCurrentKeyIsRefused(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, filepath.Join(e.Dir, "repo-key"), "someone-else")
	write(t, e.NextFile(), "new")
	err := e.ChangeSecret(context.Background())
	if !errors.Is(err, proto.ErrWrongPassword) || proto.CodeFor(err) != proto.ExitWrongPassword {
		t.Fatalf("ChangeSecret = %v, want ErrWrongPassword", err)
	}
	if got := read(t, e.PasswordFile()); got != "old" {
		t.Errorf("repository.password = %q, want it untouched", got)
	}
	gone(t, e.NextFile())
}

// Nothing to install, an empty key, and a box waiting for recovery are all refused
// without touching the repository.
func TestChangeSecretRefusals(t *testing.T) {
	e := keyedBox(t, "old")
	if err := e.ChangeSecret(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("no .next: %v, want ErrNotConfigured", err)
	}
	write(t, e.NextFile(), "  \n")
	if err := e.ChangeSecret(context.Background()); !errors.Is(err, proto.ErrWrongPassword) {
		t.Errorf("empty .next: %v, want ErrWrongPassword", err)
	}
	gone(t, e.NextFile())
	write(t, e.MarkerFile(), `{}`)
	write(t, e.NextFile(), "new")
	if err := e.ChangeSecret(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("needs recovery: %v, want ErrNotConfigured", err)
	}
	gone(t, e.NextFile())
	if got := read(t, filepath.Join(e.Dir, "repo-key")); got != "old" {
		t.Errorf("repository key = %q after refusals", got)
	}
}

// kopia rewrote the key and then failed: the change is recognised as done and finished,
// not reported as a failure that leaves the box holding a key nothing accepts.
func TestChangeSecretFinishesAChangeKopiaReportedAsFailed(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, filepath.Join(e.Dir, "die-after-change"), "")
	write(t, e.NextFile(), "new")
	if err := e.ChangeSecret(context.Background()); err != nil {
		t.Fatalf("ChangeSecret = %v, want the change completed", err)
	}
	if got := read(t, e.PasswordFile()); got != "new" {
		t.Errorf("repository.password = %q, want new", got)
	}
	gone(t, e.NextFile())
}

// Unreachable storage: nothing is known, so .next is kept for the next probe.
func TestChangeSecretKeepsTheKeyWhenTheOutcomeIsUnknown(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, filepath.Join(e.Dir, "offline"), "")
	write(t, e.NextFile(), "new")
	if err := e.ChangeSecret(context.Background()); err == nil {
		t.Fatal("ChangeSecret succeeded against unreachable storage")
	}
	if got := read(t, e.NextFile()); got != "new" {
		t.Errorf(".next = %q, want it kept", got)
	}
}

// The crash SettleNext exists for: the repository already wants .next, the file still
// holds the old key. The next status finishes the change and reports connected.
func TestStatusSettlesAnInterruptedChange(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, filepath.Join(e.Dir, "repo-key"), "new")
	write(t, e.NextFile(), "new")
	if st := e.Status(context.Background()); !st.Connected {
		t.Fatalf("status = %+v, want connected after settling", st)
	}
	if got := read(t, e.PasswordFile()); got != "new" {
		t.Errorf("repository.password = %q, want new", got)
	}
	gone(t, e.NextFile())
}

// A .next the repository never adopted is stale and is dropped; while storage is
// unreachable neither key is touched.
func TestSettleDropsAStaleKeyAndWaitsOutAnOutage(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, e.NextFile(), "new")
	write(t, filepath.Join(e.Dir, "offline"), "")
	e.SettleNext(context.Background())
	if got := read(t, e.NextFile()); got != "new" {
		t.Fatalf(".next = %q during an outage, want it kept", got)
	}
	os.Remove(filepath.Join(e.Dir, "offline"))
	e.SettleNext(context.Background())
	gone(t, e.NextFile())
	if got := read(t, e.PasswordFile()); got != "old" {
		t.Errorf("repository.password = %q, want old", got)
	}
}

// A change in progress holds the lock, and a probe arriving meanwhile must not settle
// its .next as stale — the repository has not been told yet.
func TestSettleLeavesAChangeInProgressAlone(t *testing.T) {
	e := keyedBox(t, "old")
	write(t, e.NextFile(), "new")
	unlock, ok, err := e.lockSecret(true)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	e.SettleNext(context.Background())
	unlock()
	if got := read(t, e.NextFile()); got != "new" {
		t.Errorf(".next = %q, want it left for the change holding the lock", got)
	}
}
