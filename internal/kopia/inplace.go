package kopia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Incremental in-place restore.
//
// A restore in place used to rewrite every byte of the backup over the live folder. When
// the folder is the one the backup was taken of minutes earlier — a failed update being
// rolled back — almost nothing differs, and yunderateam's Nextcloud spent its rollback
// rewriting 334 GB it already had at 35 MB/s: a 2.5 h outage to change a few hundred MB.
//
// So the live folder is surveyed first:
//
//  1. survey   a snapshot of the live folder, under its own source. Kopia reuses every
//              file whose size and mtime match the previous snapshot, so on a folder
//              that was just backed up it reads almost nothing and takes minutes.
//  2. compare  kopia's tree is a Merkle tree: a directory's object id hashes its
//              listing — every entry's name, type, mode, owner, mtime, size and content
//              id. Equal ids mean an identical subtree, so the comparison descends only
//              where the two trees differ, one `kopia show` per differing directory.
//  3. remove   every entry both trees hold but with a different content or metadata,
//              and every entry whose type changed.
//  4. restore  one `kopia restore --skip-existing --delete-extra`: it writes back what is
//              now missing (the backup's version of what step 3 removed, plus what the
//              live folder had lost), deletes what the backup does not hold, and skips
//              the rest.
//
// The trust is kopia's own: a file whose content changed while its size and mtime did
// not is reused by the survey and so reads as unchanged — exactly as it would in the
// next nightly backup. Anything the comparison cannot settle (survey failed, too many
// differing directories, a name it cannot trust) falls back to the full restore, so the
// optimisation can make a restore slower than before only by the survey's few minutes.

const (
	// tagSurvey marks a survey snapshot (value: a per-run nonce). It carries no stamp,
	// which is what keeps it out of List, ListAll and snapshotID: it is never a backup.
	tagSurvey = "maison-survey"

	// maxSurveyDirs caps the comparison. Past it the folder has changed too widely for
	// the comparison to pay off, and the full restore is the cheaper honest answer.
	maxSurveyDirs = 20000
)

var errTooDifferent = errors.New("the folder differs from the backup too widely to compare")

// treeEntry is one entry of `kopia show <dir object>`. Verified against kopia 0.23.1:
// uid/gid/size are omitted when zero, mtime is RFC 3339 with nanoseconds.
type treeEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // f, d, s
	Mode  string `json:"mode"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	UID   int64  `json:"uid"`
	GID   int64  `json:"gid"`
	Obj   string `json:"obj"`
}

// sameEntry is true when restoring a over b would change nothing: same content, same
// metadata. Directories are compared by Obj alone — their own metadata is reapplied by
// the restore, which visits every directory.
func sameEntry(a, b treeEntry) bool {
	return a.Type == b.Type && a.Mode == b.Mode && a.Size == b.Size && a.MTime == b.MTime &&
		a.UID == b.UID && a.GID == b.GID && a.Obj == b.Obj
}

// lister reads one directory object; a seam for tests.
type lister func(ctx context.Context, obj string) ([]treeEntry, error)

func (e *Engine) showDir(ctx context.Context, obj string) ([]treeEntry, error) {
	out, err := e.Run(ctx, "show", obj)
	if err != nil {
		return nil, err
	}
	var d struct {
		Stream  string      `json:"stream"`
		Entries []treeEntry `json:"entries"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("unreadable directory %s: %w", obj, err)
	}
	if d.Stream != "kopia:directory" {
		return nil, fmt.Errorf("object %s is not a directory", obj)
	}
	return d.Entries, nil
}

// staleEntries compares the backup tree to the live one and returns, as slash-separated
// paths relative to the root, every entry that must be removed so that a skip-existing
// restore of the backup writes its version back. only, when set, limits the comparison
// to those top-level names.
//
// Entries only the live tree holds are not returned: --delete-extra removes them. Entries
// only the backup holds are not returned either: they are missing, so the restore writes
// them.
func staleEntries(ctx context.Context, show lister, backupRoot, liveRoot string, only map[string]bool) ([]string, error) {
	var stale []string
	visits := 0
	var walk func(rel, a, b string) error
	walk = func(rel, a, b string) error {
		if a == b {
			return nil
		}
		visits++
		if visits > maxSurveyDirs {
			return errTooDifferent
		}
		want, err := show(ctx, a)
		if err != nil {
			return err
		}
		have, err := show(ctx, b)
		if err != nil {
			return err
		}
		live := make(map[string]treeEntry, len(have))
		for _, en := range have {
			live[en.Name] = en
		}
		for _, x := range want {
			// The name becomes a path below the folder being restored, and it came out
			// of a repository: anything that is not one plain path element ends the
			// comparison rather than being skipped, since skipping it would leave
			// that entry unrestored.
			if !plainName(x.Name) {
				return fmt.Errorf("the backup holds an entry named %q", x.Name)
			}
			if rel == "" && only != nil && !only[x.Name] {
				continue
			}
			y, ok := live[x.Name]
			if !ok {
				continue
			}
			p := path.Join(rel, x.Name)
			switch {
			case x.Type != y.Type:
				stale = append(stale, p)
			case x.Type == "d":
				if err := walk(p, x.Obj, y.Obj); err != nil {
					return err
				}
			case !sameEntry(x, y):
				stale = append(stale, p)
			}
		}
		return nil
	}
	if err := walk("", backupRoot, liveRoot); err != nil {
		return nil, err
	}
	return stale, nil
}

func plainName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\\\x00")
}

// removeStale deletes the stale entries under dest. Each path is re-checked against the
// disk: no ancestor between dest and the entry may be a symlink, so a removal can never
// leave the folder. RemoveAll does not follow a symlink that is the entry itself.
func removeStale(dest string, stale []string) error {
	for _, rel := range stale {
		parts := strings.Split(rel, "/")
		cur := dest
		for _, part := range parts[:len(parts)-1] {
			cur = filepath.Join(cur, part)
			fi, err := os.Lstat(cur)
			if err != nil {
				return fmt.Errorf("checking %s: %w", cur, err)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s is no longer a directory", cur)
			}
		}
		if err := os.RemoveAll(filepath.Join(cur, parts[len(parts)-1])); err != nil {
			return err
		}
	}
	return nil
}

// --- the survey snapshot ------------------------------------------------------

type retention struct {
	KeepLatest  int `json:"keepLatest"`
	KeepHourly  int `json:"keepHourly"`
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
	KeepAnnual  int `json:"keepAnnual"`
}

func (r retention) args() []string {
	return []string{
		"--keep-latest", itoa(r.KeepLatest), "--keep-hourly", itoa(r.KeepHourly),
		"--keep-daily", itoa(r.KeepDaily), "--keep-weekly", itoa(r.KeepWeekly),
		"--keep-monthly", itoa(r.KeepMonthly), "--keep-annual", itoa(r.KeepAnnual),
	}
}

// widened is r with room for one more snapshot. Kopia applies a source's retention after
// every `snapshot create`, and counts a pinned snapshot like any other (verified against
// 0.23.1: a pinned extra snapshot under keep-latest 2 evicted the oldest backup). So the
// survey is taken with every tier one wider: the survey takes the newest slot of each
// tier, and every backup that held a slot before still holds one. A tier at 0 keeps
// nothing either way and stays 0.
func (r retention) widened() retention {
	inc := func(n int) int {
		if n > 0 {
			return n + 1
		}
		return 0
	}
	return retention{
		KeepLatest: r.KeepLatest + 1, KeepHourly: inc(r.KeepHourly), KeepDaily: inc(r.KeepDaily),
		KeepWeekly: inc(r.KeepWeekly), KeepMonthly: inc(r.KeepMonthly), KeepAnnual: inc(r.KeepAnnual),
	}
}

// survey snapshots dest under the backup's source tag and returns the snapshot's id and
// root directory object. Retention is widened around it and set back right after — both
// to the EFFECTIVE values, which is what ensure-retention installs explicitly on every
// source anyway; a failure to set them back is repaired by the next ensure-retention.
func (e *Engine) survey(ctx context.Context, sourceID, dest string) (id, root string, err error) {
	tag, err := sourceTag(sourceID)
	if err != nil {
		return "", "", err
	}
	out, err := e.Run(ctx, "policy", "show", "--json", dest)
	if err != nil {
		return "", "", err
	}
	var pol struct {
		Retention retention `json:"retention"`
	}
	if err := json.Unmarshal(out, &pol); err != nil {
		return "", "", fmt.Errorf("unreadable policy: %w", err)
	}
	if _, err := e.Run(ctx, append([]string{"policy", "set", dest}, pol.Retention.widened().args()...)...); err != nil {
		return "", "", fmt.Errorf("widening retention: %w", err)
	}
	defer func() {
		if _, rerr := e.Run(context.WithoutCancel(ctx), append([]string{"policy", "set", dest}, pol.Retention.args()...)...); rerr != nil {
			e.Out.Log("warn", "setting retention back after the survey (the next backup run reinstalls it): "+rerr.Error())
		}
	}()

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	n := hex.EncodeToString(nonce)
	e.Out.Progress("Comparing the app with its backup", proto.PctUnknown, 0, 0)
	if _, err := e.Run(ctx, "snapshot", "create", dest, "--progress",
		"--tags", tagSource+":"+tag, "--tags", tagSurvey+":"+n); err != nil {
		return "", "", err
	}
	snaps, err := e.rootsTagged(ctx, tagSurvey+":"+n)
	if err != nil {
		return "", "", err
	}
	if len(snaps) != 1 {
		return "", "", fmt.Errorf("survey snapshot not found")
	}
	return snaps[0].ID, snaps[0].Root, nil
}

type rootedSnapshot struct {
	ID   string
	Root string
	Path string // the source path it was taken of
}

// rootsTagged lists snapshots carrying a tag, with their root objects. Its own decoding,
// so the shared snapshot struct does not grow a field only this file reads.
func (e *Engine) rootsTagged(ctx context.Context, tags ...string) ([]rootedSnapshot, error) {
	args := []string{"snapshot", "list", "--all", "--json"}
	for _, t := range tags {
		args = append(args, "--tags", t)
	}
	out, err := e.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID     string `json:"id"`
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		RootEntry struct {
			Obj string `json:"obj"`
		} `json:"rootEntry"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("unreadable snapshot list: %w", err)
	}
	res := make([]rootedSnapshot, 0, len(raw))
	for _, r := range raw {
		res = append(res, rootedSnapshot{ID: r.ID, Root: r.RootEntry.Obj, Path: r.Source.Path})
	}
	return res, nil
}

// dropSurveys deletes this source's survey snapshots, the current one and any an
// interrupted run left behind. Their content is shared with the backups, so this frees
// nothing but a manifest; it keeps them from occupying retention slots.
func (e *Engine) dropSurveys(ctx context.Context, sourceID string) {
	tag, err := sourceTag(sourceID)
	if err != nil {
		return
	}
	out, err := e.Run(ctx, "snapshot", "list", "--all", "--json", "--tags", tagSource+":"+tag)
	if err != nil {
		e.Out.Log("warn", "listing survey snapshots: "+err.Error())
		return
	}
	var snaps []snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return
	}
	for _, sn := range ownedBy(snaps, e.identity()) {
		if sn.tag(tagSurvey) == "" {
			continue
		}
		if err := e.deleteByID(ctx, sn.ID); err != nil {
			e.Out.Log("warn", "dropping survey snapshot "+sn.ID+": "+err.Error())
		}
	}
}

// prepareIncremental surveys dest and removes what differs from the backup, leaving a
// folder a skip-existing restore completes. false means it did nothing that matters —
// the caller runs the full restore. Once removal has started, a failure is an error:
// the folder is then neither the live state nor the backup, and only a restore can
// finish it.
func (e *Engine) prepareIncremental(ctx context.Context, sourceID, backupID, dest string, only map[string]bool) (bool, error) {
	tag, err := sourceTag(sourceID)
	if err != nil {
		return false, err
	}
	backups, err := e.rootsTagged(ctx, tagSource+":"+tag)
	if err != nil {
		e.Out.Log("info", "full restore: "+err.Error())
		return false, nil
	}
	backupRoot := ""
	for _, b := range backups {
		if b.ID != backupID {
			continue
		}
		// Surveyed under any other path, the snapshot would have no previous one to
		// reuse — kopia would read every byte of the folder — and the retention
		// written around it would land on a path that is no backup's source.
		if filepath.Clean(b.Path) != filepath.Clean(dest) {
			e.Out.Log("info", "full restore: the backup was taken of "+b.Path+", not of "+dest)
			return false, nil
		}
		backupRoot = b.Root
	}
	if backupRoot == "" {
		e.Out.Log("info", "full restore: the backup's root object is not listed")
		return false, nil
	}
	e.dropSurveys(ctx, sourceID)
	_, liveRoot, err := e.survey(ctx, sourceID, dest)
	defer e.dropSurveys(context.WithoutCancel(ctx), sourceID)
	if err != nil {
		e.Out.Log("info", "full restore: surveying the folder failed: "+err.Error())
		return false, nil
	}
	stale, err := staleEntries(ctx, e.showDir, backupRoot, liveRoot, only)
	if err != nil {
		e.Out.Log("info", "full restore: "+err.Error())
		return false, nil
	}
	e.Out.Log("info", fmt.Sprintf("incremental restore: %d entries differ from the backup", len(stale)))
	if err := removeStale(dest, stale); err != nil {
		return false, fmt.Errorf("removing what differs from the backup: %w", err)
	}
	return true, nil
}
