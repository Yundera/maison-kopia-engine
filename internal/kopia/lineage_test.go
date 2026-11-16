package kopia

import (
	"encoding/json"
	"testing"
)

func snap(id, host, stamp string) snapshot {
	var s snapshot
	s.ID = id
	s.Source.Host = host
	s.Source.UserName = "pcs"
	s.Tags = map[string]string{jsonTagPrefix + tagStamp: stamp}
	return s
}

func ids(snaps []snapshot) []string {
	out := []string{}
	for _, s := range snaps {
		out = append(out, s.ID)
	}
	return out
}

// kopia's `snapshot list --json` names the writer in "source"; that is what tells
// two boxes in one repository apart.
func TestSnapshotSourceIsRead(t *testing.T) {
	var s snapshot
	raw := `{"id":"k1","source":{"host":"dev-a","userName":"pcs","path":"/DATA/AppData/x"},"tags":{"tag:maison-stamp":"20261116T030000Z"}}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if !s.writtenBy(identity{host: "dev-a", user: "pcs"}) || s.writtenBy(identity{host: "dev-b", user: "pcs"}) {
		t.Errorf("source not read: %+v", s.Source)
	}
}

// Commit and Abort act on this box's lineage only; with no known identity nothing is
// filtered, as before.
func TestOwnedByKeepsThisBoxOnly(t *testing.T) {
	all := []snapshot{snap("a1", "dev-a", "s"), snap("b1", "dev-b", "s")}
	if got := ids(ownedBy(all, identity{host: "dev-a", user: "pcs"})); len(got) != 1 || got[0] != "a1" {
		t.Errorf("ownedBy = %v, want [a1]", got)
	}
	if got := ownedBy(all, identity{}); len(got) != 2 {
		t.Errorf("no identity must filter nothing, got %v", ids(got))
	}
}

// A restore still reaches every lineage, this box's first.
func TestOwnFirstKeepsEveryLineage(t *testing.T) {
	all := []snapshot{snap("b1", "dev-b", "s"), snap("a1", "dev-a", "s"), snap("b2", "dev-b", "t")}
	got := ids(ownFirst(all, identity{host: "dev-a", user: "pcs"}))
	want := []string{"a1", "b1", "b2"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ownFirst = %v, want %v", got, want)
	}
}

// Delete: this box's copy when it has one, else the predecessor's (a rebuilt box
// deleting a backup it listed from the old lineage).
func TestDeletionSet(t *testing.T) {
	me := identity{host: "dev-a", user: "pcs"}
	both := []snapshot{snap("a1", "dev-a", "s"), snap("b1", "dev-b", "s")}
	if got := ids(deletionSet(both, me, "s")); len(got) != 1 || got[0] != "a1" {
		t.Errorf("shared stamp: deletionSet = %v, want [a1]", got)
	}
	old := []snapshot{snap("b1", "dev-b", "s")}
	if got := ids(deletionSet(old, me, "s")); len(got) != 1 || got[0] != "b1" {
		t.Errorf("foreign-only stamp: deletionSet = %v, want [b1]", got)
	}
}
