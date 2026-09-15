package kopia

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Materialize writes a backup into a fresh destination, touching nothing that is
// already there. The caller has already checked there is room.
func (e *Engine) Materialize(ctx context.Context, sourceID, stamp, dest string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("destination must be an absolute path: %s", dest)
	}
	id, err := e.snapshotID(ctx, sourceID, stamp)
	if err != nil {
		return err
	}
	_, err = e.Run(ctx, "restore", id, dest, "--progress")
	return err
}

// RestoreInPlace writes a backup straight over dest.
//
// --delete-extra is what makes this a restore rather than a merge: without it, files
// created after the backup survive it and the result is a state that never existed.
// Overwriting is kopia's default, so only the deletion has to be asked for.
//
// It is not atomic, and the caller knows it: an interruption leaves dest in neither
// state, which is why an undo snapshot is taken first.
//
// With entries named, each is restored into its own path under dest instead of dest
// being one target. That is not a convenience — aimed at a directory whose snapshot
// deliberately excludes part of its contents, --delete-extra would delete exactly what
// was excluded. Per-entry, an excluded path is never a target and cannot be reached.
func (e *Engine) RestoreInPlace(ctx context.Context, sourceID, stamp, dest string, entries []string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("destination must be an absolute path: %s", dest)
	}
	id, err := e.snapshotID(ctx, sourceID, stamp)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		_, err = e.Run(ctx, "restore", id, dest, "--progress", "--delete-extra")
		return err
	}

	have, err := e.entries(ctx, id)
	if err != nil {
		return err
	}
	wanted, err := selectEntries(have, entries)
	if err != nil {
		return err
	}
	for _, en := range wanted {
		args := []string{"restore", id + "/" + en.Name, filepath.Join(dest, en.Name), "--progress"}
		// Only a directory can hold extra files. Aimed at a plain file --delete-extra
		// has nothing to act on, and overwriting is kopia's default.
		if en.Dir {
			args = append(args, "--delete-extra")
		}
		e.Out.Progress("Restoring "+en.Name, proto.PctUnknown, 0, 0)
		if _, err := e.Run(ctx, args...); err != nil {
			return fmt.Errorf("restoring %s: %w", en.Name, err)
		}
	}
	return nil
}

// Entries lists a backup's top-level members, so the caller can decide which of them to
// restore before asking for any of them.
//
// It exists because that decision is the caller's policy, not this engine's: which
// entries are safe to write over on a live box is something Maison knows about its own
// layout and an engine cannot be told once and for all.
func (e *Engine) Entries(ctx context.Context, sourceID, stamp string) ([]proto.Entry, error) {
	id, err := e.snapshotID(ctx, sourceID, stamp)
	if err != nil {
		return nil, err
	}
	return e.entries(ctx, id)
}

// entries parses `kopia ls -l`, which has no --json.
//
// Columns are mode, size, date, time, zone, object id, name. They are padded to align,
// so the separator is a RUN of spaces of unknown width and splitting on single spaces
// yields column fragments. The object id is the field before the name, so everything
// after it is the name — spaces and all. A trailing "/" marks a directory. Verified
// against kopia 0.23.1.
func (e *Engine) entries(ctx context.Context, id string) ([]proto.Entry, error) {
	out, err := e.Run(ctx, "ls", "-l", id)
	if err != nil {
		return nil, err
	}
	list := []proto.Entry{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		oid := fields[5]
		at := strings.Index(line, oid)
		if at < 0 {
			continue
		}
		name := strings.TrimSpace(line[at+len(oid):])
		dir := strings.HasSuffix(name, "/")
		name = strings.TrimSuffix(name, "/")
		// A name that is not a plain member of the snapshot root has no business
		// becoming a path. This value came from a repository.
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		list = append(list, proto.Entry{Name: name, Dir: dir})
	}
	return list, nil
}

// selectEntries narrows a snapshot's members to what was asked for, REFUSING a name the
// snapshot does not hold — so "restore just Documents" can never become "restore
// whatever path you name".
func selectEntries(have []proto.Entry, want []string) ([]proto.Entry, error) {
	byName := map[string]proto.Entry{}
	for _, en := range have {
		byName[en.Name] = en
	}
	out := make([]proto.Entry, 0, len(want))
	for _, w := range want {
		en, ok := byName[w]
		if !ok {
			return nil, fmt.Errorf("this backup has no %q to restore", w)
		}
		out = append(out, en)
	}
	return out, nil
}
