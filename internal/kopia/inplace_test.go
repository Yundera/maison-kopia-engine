package kopia

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// fakeTree serves directory objects from a map, counting reads.
type fakeTree struct {
	dirs  map[string][]treeEntry
	reads int
}

func (f *fakeTree) show(_ context.Context, obj string) ([]treeEntry, error) {
	f.reads++
	d, ok := f.dirs[obj]
	if !ok {
		return nil, fmt.Errorf("no object %s", obj)
	}
	return d, nil
}

func file(name, obj string) treeEntry {
	return treeEntry{Name: name, Type: "f", Mode: "0644", Size: 1, MTime: "2026-11-16T17:00:00Z", Obj: obj}
}

func dir(name, obj string) treeEntry {
	return treeEntry{Name: name, Type: "d", Mode: "0755", MTime: "2026-11-16T17:00:00Z", Obj: obj}
}

func TestStaleEntriesDescendsOnlyWhereTreesDiffer(t *testing.T) {
	ft := &fakeTree{dirs: map[string][]treeEntry{
		// backup
		"kB":    {dir("data", "kData"), dir("db", "kDbB"), file("compose.yml", "c1"), file("gone-live", "g1")},
		"kDbB":  {file("ibdata1", "i1"), file("same", "s1"), dir("sub", "kSubB")},
		"kSubB": {file("x", "x1")},
		// live: data identical (same object), db changed, compose same, gone-live missing, extra added
		"kL":   {dir("data", "kData"), dir("db", "kDbL"), file("compose.yml", "c1"), file("extra", "e1")},
		"kDbL": {file("ibdata1", "i2"), file("same", "s1"), file("sub", "notadir")},
	}}
	got, err := staleEntries(context.Background(), ft.show, "kB", "kL", nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"db/ibdata1", "db/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}
	// root pair + db pair; the identical data subtree is never read.
	if ft.reads != 4 {
		t.Fatalf("read %d directory objects, want 4", ft.reads)
	}
}

func TestStaleEntriesCatchesMetadataOnlyChanges(t *testing.T) {
	mode := file("f", "o1")
	mode.Mode = "0600"
	owner := file("g", "o2")
	owner.UID = 33
	mtime := file("h", "o3")
	mtime.MTime = "2026-11-16T17:00:00.5Z"
	ft := &fakeTree{dirs: map[string][]treeEntry{
		"kB": {file("f", "o1"), file("g", "o2"), file("h", "o3")},
		"kL": {mode, owner, mtime},
	}}
	got, err := staleEntries(context.Background(), ft.show, "kB", "kL", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"f", "g", "h"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}
}

func TestStaleEntriesHonoursTheEntryFilter(t *testing.T) {
	ft := &fakeTree{dirs: map[string][]treeEntry{
		"kB": {file("Documents", "d1"), file("Media", "m1")},
		"kL": {file("Documents", "d2"), file("Media", "m2")},
	}}
	got, err := staleEntries(context.Background(), ft.show, "kB", "kL", map[string]bool{"Media": true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Media"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}
}

func TestStaleEntriesRefusesAnUntrustableName(t *testing.T) {
	ft := &fakeTree{dirs: map[string][]treeEntry{
		"kB": {file("../etc", "a")},
		"kL": {file("x", "b")},
	}}
	if _, err := staleEntries(context.Background(), ft.show, "kB", "kL", nil); err == nil {
		t.Fatal("a name that is not one path element must end the comparison")
	}
}

func TestStaleEntriesGivesUpWhenTooMuchDiffers(t *testing.T) {
	// A chain deeper than the cap: every level differs.
	ft := &fakeTree{dirs: map[string][]treeEntry{}}
	for i := 0; i <= maxSurveyDirs; i++ {
		ft.dirs[fmt.Sprintf("b%d", i)] = []treeEntry{dir("d", fmt.Sprintf("b%d", i+1))}
		ft.dirs[fmt.Sprintf("l%d", i)] = []treeEntry{dir("d", fmt.Sprintf("l%d", i+1))}
	}
	_, err := staleEntries(context.Background(), ft.show, "b0", "l0", nil)
	if !errors.Is(err, errTooDifferent) {
		t.Fatalf("err = %v, want errTooDifferent", err)
	}
}

func TestWidenedLeavesRoomForOneSnapshotPerTier(t *testing.T) {
	r := retention{KeepLatest: 2, KeepHourly: 0, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12, KeepAnnual: 0}
	got := r.widened()
	want := retention{KeepLatest: 3, KeepHourly: 0, KeepDaily: 8, KeepWeekly: 5, KeepMonthly: 13, KeepAnnual: 0}
	if got != want {
		t.Fatalf("widened = %+v, want %+v", got, want)
	}
}

func TestRemoveStaleNeverLeavesTheFolder(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "app")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(dest, "db"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(dest, "db", "ibdata1"), filepath.Join(outside, "victim")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// "link" is a symlink to a directory outside dest: link/victim must not be reachable.
	if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
		t.Fatal(err)
	}

	if err := removeStale(dest, []string{"db/ibdata1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "db", "ibdata1")); !os.IsNotExist(err) {
		t.Fatal("db/ibdata1 should be gone")
	}
	if err := removeStale(dest, []string{"link/victim"}); err == nil {
		t.Fatal("a path through a symlink must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "victim")); err != nil {
		t.Fatal("the file outside the folder was touched")
	}
	// The symlink itself, named as the entry, is removed — not its target.
	if err := removeStale(dest, []string{"link"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "victim")); err != nil {
		t.Fatal("removing the symlink followed it")
	}
}
