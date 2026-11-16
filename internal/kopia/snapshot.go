package kopia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Tag keys stamped on every snapshot. They are how (source, stamp, pass) — identity the
// protocol carries as fields — survives into a repository that has no notion of any of
// them, and they must never appear on the wire: a protocol carrying kopia's tag names
// would hand every future adapter kopia's metadata design.
//
// THE ASYMMETRY THAT BITES: tags are written as "key:value" but come back from --json
// under a "tag:" prefix. A filter built from the read spelling matches nothing, and
// does so silently.
const (
	tagSource = "maison-app"
	tagStamp  = "maison-stamp"
	tagPass   = "maison-pass"

	jsonTagPrefix = "tag:"
)

// userDataTag is the reserved tag value for the user-data set, which is not an app. A
// leading underscore is unrepresentable in a compose project name, so the name guard
// makes the reservation rather than this having to police it.
const userDataTag = "_userdata"

// sourceTag is the tag value one source id is filed under.
func sourceTag(sourceID string) (string, error) {
	if !proto.ValidSource(sourceID) {
		return "", fmt.Errorf("not a source id: %q", sourceID)
	}
	if sourceID == proto.UserDataID {
		return userDataTag, nil
	}
	app, _ := proto.SplitSource(sourceID)
	return app, nil
}

// sourceIDFor is the inverse, for a tag that came back from the repository.
func sourceIDFor(tag string) string {
	if tag == userDataTag {
		return proto.UserDataID
	}
	return proto.AppPrefix + tag
}

// SnapshotOpts is one `snapshot` invocation.
type SnapshotOpts struct {
	SourceID string
	Path     string // passed by Maison, never derived here
	Stamp    string
	Pass     int
	Consume  bool

	// Exclude is the caller's ignore rules, and ApplyExclude says to install them.
	//
	// The two are separate because an EMPTY rule set is meaningful: installing it clears
	// whatever the repository still holds, which is how a rule an app author removed
	// stops applying. "No rules" and "do not touch the rules" are different requests and
	// cannot share one nil.
	Exclude      []string
	ApplyExclude bool
}

// Snapshot captures Path under (SourceID, Stamp).
//
// Nothing produced here is durable until Commit succeeds: an interrupted snapshot must
// leave nothing that List would return, which is what the pass tag and Commit's sweep
// between them guarantee.
func (e *Engine) Snapshot(ctx context.Context, o SnapshotOpts) error {
	tag, err := sourceTag(o.SourceID)
	if err != nil {
		return err
	}
	if o.Stamp == "" {
		return fmt.Errorf("snapshot needs a stamp")
	}
	if o.Path == "" {
		return fmt.Errorf("snapshot needs a source path")
	}
	pass := o.Pass
	if pass < 1 {
		pass = 1
	}

	// Kopia has no per-run ignore flag: exclusions exist only as policy on the source
	// path, so what the caller declared has to be pushed into the repository before the
	// snapshot reads it.
	//
	// WHEN to do that is the caller's decision, not this engine's — installing a policy
	// is a second round trip, and only the caller knows whether this snapshot is being
	// taken inside an app's downtime window or against something nothing is waiting on.
	if o.ApplyExclude {
		if err := e.EnsureIgnore(ctx, o.Path, o.Exclude); err != nil {
			return fmt.Errorf("applying backup exclusions: %w", err)
		}
	}

	_, err = e.Run(ctx, "snapshot", "create", o.Path,
		"--progress",
		"--tags", tagSource+":"+tag,
		"--tags", tagStamp+":"+o.Stamp,
		"--tags", tagPass+":"+itoa(pass),
	)
	return err
}

// Commit drops the torn first-pass snapshot, leaving the consistent one as the backup.
//
// Content is shared between the two, so removing the manifest frees nothing and loses
// nothing — the point is that a user browsing snapshots can never restore the
// inconsistent one. A failure to drop it is logged rather than returned: the real
// backup exists, and refusing to commit it because a cleanup failed is the worse
// outcome. The stale pass-1 snapshot is invisible to List and is swept later.
func (e *Engine) Commit(ctx context.Context, sourceID, stamp string) (proto.Backup, error) {
	snaps, err := e.snapshotsFor(ctx, sourceID)
	if err != nil {
		return proto.Backup{}, err
	}
	// Only this box's own lineage: another box in the same space can hold a snapshot
	// with the same source and stamp, and it is not this run's to drop.
	snaps = ownedBy(snaps, e.identity())
	var committed *snapshot
	for i := range snaps {
		sn := &snaps[i]
		if sn.tag(tagStamp) != stamp {
			continue
		}
		if sn.tag(tagPass) == "1" {
			if err := e.deleteByID(ctx, sn.ID); err != nil {
				e.Out.Log("warn", "dropping first-pass snapshot "+sn.ID+": "+err.Error())
			}
			continue
		}
		committed = sn
	}
	if committed == nil {
		return proto.Backup{}, fmt.Errorf("no snapshot to commit for %s/%s", sourceID, stamp)
	}
	return committed.backup(sourceID), nil
}

// Abort removes every snapshot carrying this stamp, so an interrupted backup leaves
// nothing a later List could offer for restore.
//
// Best-effort by contract: an unconfigured repository means nothing was written, so
// there is nothing to undo.
func (e *Engine) Abort(ctx context.Context, sourceID, stamp string) error {
	snaps, err := e.snapshotsFor(ctx, sourceID)
	if err != nil {
		if errors.Is(err, proto.ErrNotConfigured) {
			return nil
		}
		return err
	}
	// Abort undoes this box's own run, never a same-stamp snapshot another box wrote.
	return e.deleteStamp(ctx, ownedBy(snaps, e.identity()), stamp)
}

// Delete removes one committed backup: every manifest carrying the stamp. The user
// picked it from List, which shows every lineage — a rebuilt box lists its
// predecessor's backups — so a stamp only another lineage holds is deleted there.
// When this box holds the stamp too, only its own copy goes.
func (e *Engine) Delete(ctx context.Context, sourceID, stamp string) error {
	snaps, err := e.snapshotsFor(ctx, sourceID)
	if err != nil {
		if errors.Is(err, proto.ErrNotConfigured) {
			return nil
		}
		return err
	}
	return e.deleteStamp(ctx, deletionSet(snaps, e.identity(), stamp), stamp)
}

// deletionSet is what Delete removes for a stamp: this box's snapshots carrying it if
// there are any, else every lineage's.
func deletionSet(snaps []snapshot, id identity, stamp string) []snapshot {
	for _, sn := range ownedBy(snaps, id) {
		if sn.tag(tagStamp) == stamp {
			return ownedBy(snaps, id)
		}
	}
	return snaps
}

func (e *Engine) deleteStamp(ctx context.Context, snaps []snapshot, stamp string) error {
	var errs []error
	for _, sn := range snaps {
		if sn.tag(tagStamp) == stamp {
			if err := e.deleteByID(ctx, sn.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// List returns one source's committed backups, newest first.
//
// A pass-1 snapshot is never returned: it was taken while the app was still writing and
// may be torn, and offering it for restore is the one thing the two-pass structure
// exists to prevent.
func (e *Engine) List(ctx context.Context, sourceID string) ([]proto.Backup, error) {
	snaps, err := e.snapshotsFor(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	out := []proto.Backup{}
	for _, sn := range snaps {
		if sn.tag(tagPass) == "1" || sn.tag(tagStamp) == "" {
			continue
		}
		out = append(out, sn.backup(sourceID))
	}
	sortBackups(out)
	return out, nil
}

// ListAll returns every backup this repository holds, grouped by source id.
//
// One query rather than a listing per source: for a repository each call is a round
// trip, and this backs the page that shows everything at once. It also cannot be
// derived from what is installed — on a rebuilt box the repository is the only thing
// that still knows the sources existed, which is exactly when it matters most.
func (e *Engine) ListAll(ctx context.Context) (map[string][]proto.Backup, error) {
	snaps, err := e.listSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]proto.Backup{}
	for _, sn := range snaps {
		tag := sn.tag(tagSource)
		if tag == "" || sn.tag(tagStamp) == "" || sn.tag(tagPass) == "1" {
			continue
		}
		id := sourceIDFor(tag)
		// A source id that is not well-formed is DROPPED, not returned: it came from a
		// repository, which is untrusted input, and it feeds path construction
		// downstream.
		if !proto.ValidSource(id) {
			continue
		}
		out[id] = append(out[id], sn.backup(id))
	}
	for id := range out {
		sortBackups(out[id])
	}
	return out, nil
}

func sortBackups(b []proto.Backup) {
	sort.Slice(b, func(i, j int) bool { return b[i].Stamp > b[j].Stamp })
}

// snapshotID resolves (source, stamp) to kopia's own identifier.
//
// What is handed back to kopia is always a value kopia itself returned, never a name the
// caller supplied — so a crafted stamp cannot reach the command line.
func (e *Engine) snapshotID(ctx context.Context, sourceID, stamp string) (string, error) {
	snaps, err := e.snapshotsFor(ctx, sourceID)
	if err != nil {
		return "", err
	}
	// Every lineage stays restorable — a rebuilt box comes back with a new identity and
	// restores its predecessor's backups — but on a stamp both hold, this box's wins.
	for _, sn := range ownFirst(snaps, e.identity()) {
		if sn.tag(tagStamp) == stamp && sn.tag(tagPass) != "1" {
			return sn.ID, nil
		}
	}
	return "", fmt.Errorf("backup not found: %s", stamp)
}

func (e *Engine) deleteByID(ctx context.Context, id string) error {
	_, err := e.Run(ctx, "snapshot", "delete", id, "--delete")
	return err
}

// --- listing -----------------------------------------------------------------

type snapshot struct {
	ID        string    `json:"id"`
	StartTime time.Time `json:"startTime"`
	RootEntry struct {
		Summ struct {
			Size int64 `json:"size"`
		} `json:"summ"`
	} `json:"rootEntry"`
	Tags map[string]string `json:"tags"`
	// Source is kopia's user@host:path for the snapshot: which box wrote it.
	Source struct {
		Host     string `json:"host"`
		UserName string `json:"userName"`
	} `json:"source"`
}

// identity is the user@host this box files its snapshots under (repository.config).
type identity struct{ host, user string }

// identity reads it; an unreadable configuration gives the zero identity, which
// filters nothing.
func (e *Engine) identity() identity {
	rc, err := e.readConfig()
	if err != nil {
		return identity{}
	}
	return identity{host: rc.Hostname, user: rc.Username}
}

func (s snapshot) writtenBy(id identity) bool {
	return s.Source.Host == id.host && s.Source.UserName == id.user
}

// ownedBy keeps the snapshots this box wrote. With no known identity it keeps all of
// them: the behaviour before boxes could share a repository.
func ownedBy(snaps []snapshot, id identity) []snapshot {
	if id.host == "" {
		return snaps
	}
	var out []snapshot
	for _, sn := range snaps {
		if sn.writtenBy(id) {
			out = append(out, sn)
		}
	}
	return out
}

// ownFirst is every snapshot, this box's before the others', order otherwise kept.
func ownFirst(snaps []snapshot, id identity) []snapshot {
	if id.host == "" {
		return snaps
	}
	out := ownedBy(snaps, id)
	for _, sn := range snaps {
		if !sn.writtenBy(id) {
			out = append(out, sn)
		}
	}
	return out
}

// tag reads one of the adapter's tags, through the "tag:" prefix the JSON carries and
// the command line does not.
func (s snapshot) tag(key string) string { return s.Tags[jsonTagPrefix+key] }

func (s snapshot) backup(sourceID string) proto.Backup {
	return proto.Backup{
		SourceID:  sourceID,
		Stamp:     s.tag(tagStamp),
		CreatedAt: s.StartTime,
		Size:      s.RootEntry.Summ.Size,
	}
}

func (e *Engine) snapshotsFor(ctx context.Context, sourceID string) ([]snapshot, error) {
	tag, err := sourceTag(sourceID)
	if err != nil {
		return nil, err
	}
	return e.listSnapshots(ctx, "--tags", tagSource+":"+tag)
}

func (e *Engine) listSnapshots(ctx context.Context, filter ...string) ([]snapshot, error) {
	out, err := e.Run(ctx, append([]string{"snapshot", "list", "--all", "--json"}, filter...)...)
	if err != nil {
		return nil, err
	}
	var snaps []snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("unreadable snapshot list: %w", err)
	}
	return snaps, nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
