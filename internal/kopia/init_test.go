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

// storageBox is a box with no repository configuration and a stand-in kopia over a
// filesystem "storage" at <dir>/store. The storage holds a repository when
// <dir>/store/key exists, and that file is the repository's key. No <dir>/store at all
// is unreachable storage. Every connect records the config file and cache it was given
// in <dir>/seen, which is how the probe's throwaway paths are checked.
func storageBox(t *testing.T) *Engine {
	t.Helper()
	e := testEngine(t)
	e.Bin = filepath.Join(e.Dir, "kopia-stub")
	write(t, e.ConnectFile(), `{"path":"`+filepath.Join(e.Dir, "store")+`","hostname":"dev1","username":"pcs"}`)
	if err := os.Mkdir(filepath.Join(e.Dir, "store"), 0o700); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
cfg=""; store=""; for a in "$@"; do case "$a" in --config-file=*) cfg="${a#--config-file=}";; --path=*) store="${a#--path=}";; esac; done
seen="$(dirname "$store")/seen"
[ -z "$store" ] || [ -d "$store" ] || { echo "can't connect to storage: cannot access storage path: stat $store: no such file or directory" >&2; exit 1; }
case "$1 $2" in
"repository connect")
	echo "$cfg $KOPIA_CACHE_DIRECTORY" >> "$seen"
	[ -f "$store/key" ] || { echo "error connecting to repository: repository not initialized in the provided storage" >&2; exit 1; }
	[ -f "$store/accept-any" ] || [ "$KOPIA_PASSWORD" = "$(cat "$store/key")" ] || { echo "error connecting to repository: unable to create format manager: invalid repository password" >&2; exit 1; }
	echo '{"hostname":"dev1","username":"pcs","storage":{"type":"filesystem","config":{}}}' > "$cfg";;
"repository create")
	[ -f "$store/key" ] && { echo "unable to get repository storage: found existing data in storage location" >&2; exit 1; }
	printf '%s' "$KOPIA_PASSWORD" > "$store/key"
	echo '{"hostname":"dev1","username":"pcs","storage":{"type":"filesystem","config":{}}}' > "$cfg";;
"repository status") ;;
*) exit 2;;
esac
`
	if err := os.WriteFile(e.Bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *Engine) storeHolds(t *testing.T, key string) {
	t.Helper()
	write(t, filepath.Join(e.Dir, "store", "key"), key)
}

// "Empty" leads the caller to create a repository, so it is read only from kopia's own
// sentence for it — what that sentence is, and what it is not, is pinned here.
func TestNotInitializedIsRecognised(t *testing.T) {
	yes := errors.New("kopia repository: exit status 1: error connecting to repository: repository not initialized in the provided storage")
	if !isNotInitialized(yes) {
		t.Error("kopia's empty-storage failure was not recognised")
	}
	for _, no := range []error{
		nil,
		errors.New("kopia repository: exit status 1: can't connect to storage: cannot access storage path: stat /r: no such file or directory"),
		errors.New("kopia repository: exit status 1: error connecting to repository: unable to create format manager: invalid repository password"),
		errors.New("kopia repository: exit status 1: unable to connect: dial tcp: i/o timeout"),
	} {
		if isNotInitialized(no) {
			t.Errorf("%v was read as empty storage", no)
		}
	}
}

func TestProbeTellsEmptyFromExisting(t *testing.T) {
	e := storageBox(t)
	p, err := e.Probe(context.Background())
	if err != nil || p.Storage != proto.StorageEmpty {
		t.Fatalf("empty storage: %+v, %v", p, err)
	}
	e.storeHolds(t, "theirs")
	p, err = e.Probe(context.Background())
	if err != nil || p.Storage != proto.StorageExisting {
		t.Fatalf("storage with a repository: %+v, %v", p, err)
	}
}

// The probe changes nothing on the box: no configuration, no password, and the config
// file and cache it handed kopia were throwaway ones that no longer exist.
func TestProbeLeavesNoTrace(t *testing.T) {
	e := storageBox(t)
	e.storeHolds(t, "theirs")
	write(t, filepath.Join(e.Dir, "store", "accept-any"), "")
	// A connect that unexpectedly succeeds is the strongest "existing" there is.
	p, err := e.Probe(context.Background())
	if err != nil || p.Storage != proto.StorageExisting {
		t.Fatalf("probe = %+v, %v", p, err)
	}
	for _, f := range []string{e.ConfigFile(), e.PasswordFile()} {
		gone(t, f)
	}
	seen := strings.Fields(read(t, filepath.Join(e.Dir, "seen")))
	if len(seen) != 2 {
		t.Fatalf("seen = %q", seen)
	}
	if seen[0] == e.ConfigFile() || strings.HasPrefix(seen[0], e.Dir) || strings.HasPrefix(seen[1], e.Dir) {
		t.Errorf("the probe used this box's paths: config %s, cache %s", seen[0], seen[1])
	}
	gone(t, filepath.Dir(seen[0]))
}

// Doubt is never "empty": unreachable storage is an ordinary failure.
func TestProbeOnUnreachableStorageFails(t *testing.T) {
	e := storageBox(t)
	if err := os.Remove(filepath.Join(e.Dir, "store")); err != nil {
		t.Fatal(err)
	}
	p, err := e.Probe(context.Background())
	if err == nil || proto.CodeFor(err) != proto.ExitError {
		t.Fatalf("probe = %+v, %v, want an ordinary failure", p, err)
	}
}

// What the host has not written yet is not configured: no connect.json, or storage that
// needs credentials and has no credentials.env.
func TestProbeWithoutItsFilesIsNotConfigured(t *testing.T) {
	e := storageBox(t)
	os.Remove(e.ConnectFile())
	if _, err := e.Probe(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("no connect.json: %v, want ErrNotConfigured", err)
	}
	write(t, e.ConnectFile(), `{"bucket":"b","endpoint":"https://s3","region":"r","prefix":"p/","hostname":"dev1","username":"pcs"}`)
	if _, err := e.Probe(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("s3 without credentials.env: %v, want ErrNotConfigured", err)
	}
}

func TestInitCreatesTheRepositoryWithTheOwnersKey(t *testing.T) {
	e := storageBox(t)
	write(t, e.NeedsSecretFile(), `{"reason":"no-password"}`)
	write(t, e.InitialFile(), "owners-key\n")
	if st := e.Status(context.Background()); !st.NeedsSecret || st.Configured || st.NeedsRecovery {
		t.Fatalf("status before init = %+v, want needsSecret", st)
	}

	if _, err := e.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, e.PasswordFile()); got != "owners-key" {
		t.Errorf("repository.password = %q", got)
	}
	if got := read(t, filepath.Join(e.Dir, "store", "key")); got != "owners-key" {
		t.Errorf("the repository was created with %q", got)
	}
	gone(t, e.InitialFile())
	gone(t, e.NeedsSecretFile())
	if st := e.Status(context.Background()); !st.Configured || !st.Connected || st.NeedsSecret {
		t.Errorf("status after init = %+v", st)
	}
}

// Storage that already holds a repository is 13 and nothing else: no second repository,
// no configuration, no password — and the key the owner typed is not left lying around.
func TestInitOverAnExistingRepositoryIsRefused(t *testing.T) {
	e := storageBox(t)
	e.storeHolds(t, "theirs")
	write(t, e.NeedsSecretFile(), `{}`)
	write(t, e.InitialFile(), "owners-key")

	_, err := e.Init(context.Background())
	if !errors.Is(err, proto.ErrRepositoryExists) || proto.CodeFor(err) != proto.ExitRepositoryExists {
		t.Fatalf("Init = %v, want ErrRepositoryExists", err)
	}
	if got := read(t, filepath.Join(e.Dir, "store", "key")); got != "theirs" {
		t.Errorf("the existing repository's key became %q", got)
	}
	for _, f := range []string{e.InitialFile(), e.ConfigFile(), e.PasswordFile()} {
		gone(t, f)
	}
}

func TestInitRefusals(t *testing.T) {
	e := storageBox(t)
	if _, err := e.Init(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("no initial key: %v, want ErrNotConfigured", err)
	}

	write(t, e.InitialFile(), "  \n")
	if _, err := e.Init(context.Background()); err == nil || proto.CodeFor(err) != proto.ExitError {
		t.Errorf("empty key: %v, want an ordinary failure", err)
	}
	gone(t, e.InitialFile())

	write(t, e.InitialFile(), "k")
	write(t, e.ConfigFile(), `{"hostname":"dev1","username":"pcs"}`)
	if _, err := e.Init(context.Background()); err == nil || proto.CodeFor(err) != proto.ExitError {
		t.Errorf("configured box: %v, want an ordinary failure", err)
	}
	gone(t, e.InitialFile())
	if _, err := os.Stat(filepath.Join(e.Dir, "store", "key")); !os.IsNotExist(err) {
		t.Error("a repository was created on a box that already had a configuration")
	}

	os.Remove(e.ConfigFile())
	os.Remove(e.ConnectFile())
	write(t, e.InitialFile(), "k")
	if _, err := e.Init(context.Background()); !errors.Is(err, proto.ErrNotConfigured) {
		t.Errorf("no connect.json: %v, want ErrNotConfigured", err)
	}
	gone(t, e.InitialFile())
}

// A needs-secret marker beside a configuration is stale, and must not hide the
// repository the box actually has.
func TestStaleNeedsSecretMarkerIsIgnored(t *testing.T) {
	e := storageBox(t)
	e.storeHolds(t, "k")
	write(t, e.PasswordFile(), "k")
	write(t, e.ConfigFile(), `{"hostname":"dev1","username":"pcs"}`)
	write(t, e.NeedsSecretFile(), `{}`)
	if st := e.Status(context.Background()); st.NeedsSecret || !st.Configured {
		t.Errorf("status = %+v, want the configured repository", st)
	}
}

// A key the repository refuses is 14 from `connect`, decided before any create — which
// could only have come back as 13 and read as "rebuilt box" for a box holding a stale key.
func TestConnectWithARefusedKeyIsWrongPassword(t *testing.T) {
	e := storageBox(t)
	e.storeHolds(t, "theirs")
	write(t, e.PasswordFile(), "mine")
	err := e.Connect(context.Background(), ConnectOpts{Path: filepath.Join(e.Dir, "store"), Hostname: "dev1", Username: "pcs"})
	if !errors.Is(err, proto.ErrWrongPassword) || proto.CodeFor(err) != proto.ExitWrongPassword {
		t.Fatalf("Connect = %v, want ErrWrongPassword", err)
	}
	gone(t, e.ConfigFile())
}

func TestCapsAdvertiseInitAndProbe(t *testing.T) {
	c := testEngine(t).Caps(context.Background())
	if !c.Init || !c.Probe || c.AdapterVersion != "1.4.0" {
		t.Errorf("caps = %+v", c)
	}
}
