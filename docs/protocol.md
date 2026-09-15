# The Maison backup adapter protocol

Version **v0** — design only, nothing implemented.

An *adapter* is a container image holding a backup engine and a `maison-engine` binary that
speaks this protocol. Maison runs one adapter per configured engine and knows nothing about
the engine inside it.

This document is authoritative for the wire format. Maison's
[`docs/backup.md` → The engine adapter](https://github.com/Yundera/maison/blob/main/docs/backup.md)
is authoritative for the split of responsibility, and this document does not restate it
beyond what the wire needs.

---

## Scope

v0 is deliberately sized to **exactly what Maison does today** — every verb maps to a method
on `internal/apps.Provider` or on Maison's current kopia provider, and nothing here is
speculative. It is specified against kopia because kopia is the first tenant; it is shaped
against the `Provider` interface rather than against kopia's CLI, which is what keeps it
generic without anyone having to guess what the second engine needs.

**It will change when the second adapter is written.** That is expected and is why the
version is v0. What must *not* change under it is the division of ownership — see
[Rules that keep this engine-agnostic](#rules-that-keep-this-engine-agnostic), which are the
invariants, not the verbs.

### Not in scope, deliberately

| | Why not |
|---|---|
| Fetching storage credentials | The PCS host does this (`ensure-backup-credentials.sh`). The adapter reads what it finds. |
| Key escrow | Maison reads `repository.password` from disk directly. The key matters most when the box is broken, so the escrow path must not need a working container. |
| Scheduling, retention *policy* | Maison decides what to keep; the adapter only says what expiry its storage can survive. |
| Stopping and starting apps | Maison owns app lifecycle. An adapter that could stop containers could extend an app's downtime. |
| Deriving source paths | Maison passes paths. See [Maison passes paths](#maison-passes-paths-the-adapter-never-derives-them). |

---

## Invocation

```
docker exec <engine-container> maison-engine <verb> [flags…]
```

One process per verb. No daemon, no listening socket, no state between invocations beyond
what lives in the repository directory.

The adapter is also runnable as `docker run --rm <image> maison-engine <verb> …` — the same
binary, the same contract. Maison uses a resident container because container start on a PCS
costs six to seven seconds before the engine's own process begins, which is long enough to
make the store's Install button look broken.

### Why argv and stdio

An HTTP glue service is the obvious shape and the wrong one:

- **`--network none` stays available.** A repository on a local filesystem needs no network
  and must not be given one. A daemon needs one by definition.
- **Cancellation keeps working.** Maison holds a pid file per in-flight exec so a cancelled
  backup cannot leave the engine holding an app's files open while the app is restarted.
  Cancelling an HTTP request does not kill a child process.
- **Progress needs no new plumbing.** Maison already streams engine stdout into its progress
  events.
- **There is no new client to get wrong** — no health checks, no retries, no connection
  state, no second definition of "the engine is up".

Nothing is given up in exchange: the adapter can be written in any language either way, and
a verb that runs for an hour streams progress exactly as a request would.

---

## Streams and framing

**stdout is NDJSON**: one JSON object per line, no other output, ever. A verb that writes a
bare string to stdout is a protocol violation, because Maison parses every line.

```jsonc
{"type":"progress","message":"estimating","pct":-1}
{"type":"progress","message":"uploading","pct":42.5,"done":4194304,"total":9871232}
{"type":"log","level":"warn","message":"cache directory rebuilt"}
{"type":"result","backup":{"stamp":"20260915T031500Z","sourceId":"app:nextcloud","createdAt":"2026-09-15T03:15:00Z","size":9871232}}
```

- `progress` — maps 1:1 onto `apps.Event`. `pct` is `0`–`100`, or `-1` for "unknown", which
  renders as an indeterminate bar. `done`/`total` are bytes; `0` means not reported.
  **`total` may move**: an engine discovering the tree as it walks revises upward, and Maison
  expects that rather than treating it as a fault. Report what you observed and nothing more
   — rate, ETA and elapsed are derived by Maison, identically for every engine.
- `log` — diagnostic, surfaced in Maison's logs, never in the UI.
- `result` — **at most one, and last.** Its payload is per-verb. A verb with no return value
  emits none.

**stderr is free text**, for diagnostics that are not worth a `log` line. Maison keeps the
tail and uses it in the error message when the exit code is non-zero.

---

## Exit codes

| Code | Meaning | Maison's reaction |
|---|---|---|
| `0` | success | — |
| `10` | **not configured** — no repository connected, credentials absent | Renders "not configured". **Not an error**, and not an incident: it is the normal state of a box whose host side has not run. |
| `11` | **not supported** — this engine cannot do this verb | Maps to `ErrNotSupported`. Must agree with `capabilities`. |
| `12` | **not writable** — the repository is reachable but refuses writes (a suspended storage space) | Reads and restores continue; writes are reported as failed. |
| `1` | any other failure | Operation fails, stderr tail is surfaced. |

`10` and `11` must be distinguished from `1`. Collapsing `10` turns an unprovisioned box into
a red page; collapsing `11` turns a capability gap into a fault.

---

## The source model

A **source** is one thing that gets snapshotted. There are exactly two kinds today, and they
are the same kind to the protocol:

| Source id | What it is | Path Maison passes |
|---|---|---|
| `app:<name>` | one app's folder | `${DATA_ROOT}/AppData/<name>` |
| `userdata` | everything under the data root that is not an app | `${DATA_ROOT}` |

`<name>` is a valid compose project name. Maison validates it before passing it and
re-validates anything an adapter returns, because a repository is untrusted input.

A **stamp** is Maison's identifier for one backup of one source. Opaque to the adapter;
never parsed, never ordered, only matched.

`(sourceId, stamp)` is the identity of a backup, and it is the *only* identity Maison uses.

---

## Common flags

Accepted by every verb:

| Flag | Meaning |
|---|---|
| `--repo-dir <path>` | The engine's own directory — `${DATA_ROOT}/AppDataShared/backup/<engine>/`. Holds `repository.config`, `repository.password`, `credentials.env`, caches and logs. The adapter reads it; only the host side writes it. |

**The adapter reads its own secrets from `--repo-dir` and is passed none.** The repository
password and the storage credentials are files in that directory, which the adapter has
mounted, so there is nothing for the caller to plumb through — and re-reading them per
invocation is what makes a credential rotation take effect on the next backup rather than on
the next restart. It also keeps Maison out of the repository-password path entirely, apart
from escrow, which reads the file directly because it has to work when the engine does not.
| `--timeout <duration>` | Advisory. Maison enforces its own; this lets the adapter fail cleanly first with a better message. |

---

## Verbs

### Repository

#### `capabilities`
No flags. Must work with no repository configured — Maison calls it to build the engine
picker. Returns [`Caps`](#caps).

#### `connect`
Creates the repository if the storage is empty, connects to it if it is not, idempotently.
Reads credentials from `--repo-dir`. This is the verb that lets the PCS host side stop
containing engine-specific commands.

**Must never reconnect an already-connected repository**, and must never rewrite a
configuration that exists. Identity is written once; see
[Lineage is written once](#lineage-is-written-once).

Exits `10` if credentials are absent — a box not yet provisioned, not a failure.

#### `status`
Returns [`Status`](#status-1). Must answer without a working repository, distinguishing
*not configured* from *configured but unreachable* — Maison's incident logic depends on
exactly that difference.

#### `prepare`
Best-effort warm-up before a batch of operations (cache validation, a connectivity probe).
May be a no-op. Its failure is advisory.

### Snapshot lifecycle

#### `snapshot --source-id <id> --path <path> --stamp <s> --pass <1|2> [--exclude-file <f>] [--consume]`

Captures `--path` under `(sourceId, stamp)`. **Nothing produced here is durable until
`commit` succeeds** — an interrupted `snapshot` must leave nothing that `list` would return.

Called **twice for one backup** against the same `(sourceId, stamp)`: pass 1 with the app
running, pass 2 with it stopped. **Pass 2 must be incremental against pass 1.** This is not
an optimisation — it is the entire reason an app's downtime is proportional to what changed
during pass 1 rather than to its size. An adapter that cannot be incremental between passes
must say so in `capabilities`, and Maison will not offer it for the app set.

`--exclude-file` is a newline-delimited list of patterns, resolved once by Maison from the
app's `x-compose-app` `backup.exclude`. It is passed on **every** call. The adapter may
additionally install it as an engine-side policy so the engine's own UI agrees, but must
not depend on having done so — two engines disagreeing about what an app declared means the
same app backing up different contents depending on which engine ran, and the difference
only surfaces at restore.

`--consume` says the source folder is being destroyed (an uninstall). An adapter that can
*take* the folder rather than read it may do so — **but only in `commit`, never here**, since
a crash before the commit point must leave the source exactly as it was. An adapter that
streams to a repository ignores the flag and reads as usual; Maison removes the source itself
after a successful commit either way. `--consume` implies a single pass, and that pass is
**pass 2**, because it is the consistent one.

#### `commit --source-id <id> --stamp <s> [--consume]`
The commit point. Makes `(sourceId, stamp)` real and listable. Returns a
[`Backup`](#backup) — the adapter returns it rather than Maison constructing one, because
only the adapter knows what it actually produced.

#### `abort --source-id <id> --stamp <s>`
Discards whatever an interrupted `snapshot` left. Best-effort; its failure is logged and
never surfaced, because a failed cleanup must not mask the failure that caused it.

### Reading

#### `list --source-id <id>`
This engine's backups of one source, newest first. **An unknown source returns an empty list,
not an error** — Maison asks every engine about every source.

#### `list-all`
Every backup this engine holds, as `{sourceId: [Backup, …]}`, each group newest first.

One call rather than "which sources do you have" plus a `list` each, because for a remote
engine every call is a round trip and the global page shows everything at once. It also
**cannot be derived from what is installed**: on a rebuilt box nothing is installed and the
repository is the only thing that still knows the apps existed — which is exactly when the
page matters most.

Source ids that are not well-formed must be **dropped, not returned**. They feed path
construction downstream.

### Restore

#### `materialize --source-id <id> --stamp <s> --dest <path>`
Writes the backup into `--dest`, touching nothing the user already has. Maison has already
checked there is room.

#### `restore-in-place --source-id <id> --stamp <s> --dest <path> [--entries a,b]`
Writes over `--dest` without staging a second copy — the only way to restore something too
large to fit twice on its own disk. **Files present in `--dest` but absent from the backup
must be removed**, or the result is a merge rather than a restore.

Not atomic, and Maison knows it: an interruption leaves `--dest` in neither state, which is
why Maison takes an undo snapshot first. Adapters that cannot do this exit `11` and say so in
`capabilities`.

`--entries` limits the restore to named top-level entries (`Documents`, `Media`). The adapter
**must match them against the snapshot's own listing and reject anything else** — a caller
must not be able to name an arbitrary path through this flag.

#### `entries --source-id <id> --stamp <s>`
Returns the backup's top-level members as `[{"name": "...", "dir": true}]`.

It exists because *which* entries are safe to write over on a live box is the caller's
policy, not the engine's. Maison knows that its own `AppDataShared/` holds the engines'
configuration — including the configuration the engine performing the restore is reading —
and must be left alone in place. An engine cannot be told that once and for all, so it is
asked what the snapshot holds and told what to restore.

#### `delete --source-id <id> --stamp <s>`
Removes one backup.

### Retention

#### `ensure-retention --path <path> --keep <json> [--user-data]`
Installs Maison's retention intent for a source. It takes `--path` rather than `--source-id`
because retention is a property of the source tree, and the caller owns paths.

`--user-data` says this source is the user-data set, which is the one source that may have
other filesystems mounted beneath it. `--keep` is the tier set
(`{"hourly":n,"daily":n,"weekly":n,"monthly":n,"annual":n}`).

Only meaningful for an adapter whose `capabilities` reports `retention: true` — one that
expires backups itself. An adapter that does not exits `11`, and Maison deletes backups
explicitly instead.

**The adapter never decides what to keep.** It reports what expiry its storage can survive
(`retentionModel`) and applies what it is given.

---

## Objects

### `Caps`

```jsonc
{
  "engineId": "kopia",             // permanent. Recorded on every backup this adapter writes.
  "engineVersion": "0.23.1",
  "adapterVersion": "0.1.0",
  "protocol": "v0",

  "offsite": true,                 // survives the loss of this machine
  "encrypted": true,               // at rest — a property of the engine, not of whether it is configured yet
  "instantRestore": false,         // restore is a rename, not a copy or a download
  "needsLocalSpace": false,        // a backup needs free disk proportional to the source
  "inPlaceRestore": true,          // supports restore-in-place
  "incrementalPasses": true,       // pass 2 is incremental against pass 1 — required for the app set
  "consumesSource": false,         // can take the source folder rather than read it
  "retention": true,               // expires backups itself; ensure-retention is meaningful

  "retentionModel": "snapshot"     // snapshot | chain | lifecycle | none
}
```

**Zero values are the conservative answer.** A field an adapter omits means "this engine
cannot", never "can".

`engineId` is permanent and may never change or be reused once shipped: it is written into
every backup and is how a backup finds its way home after the user switches engines.

`retentionModel` says what *kind* of expiry the storage can survive, and it is a different
question from who performs it and from what the user asked for:

- `snapshot` — independently deletable generations (content-addressed repositories,
  self-contained archives). Any generation may be dropped; tiers work in full.
- `chain` — each generation is meaningful only with every newer one (a mirror with a dated
  backup directory). **Only the oldest tail may be truncated.** Tiers are not expressible and
  must not be faked — deleting a middle generation does not lose one day, it breaks every
  restore point behind it, and nothing looks wrong until someone restores.
- `lifecycle` — the storage expires by itself (bucket lifecycle rules). Maison configures and
  deletes nothing.
- `none` — expires nothing. Backups accumulate; the failure mode is a bill, not a lost
  restore point.

### `Status`

```jsonc
{
  "configured": true,              // a repository configuration exists on disk
  "connected": true,               // it was reachable on this probe
  "identity": "pcs@a1b2c3d4",      // the lineage snapshots are filed under
  "detail": ""                     // human-readable, for the settings page when something is wrong
}
```

**The label, writability and credential expiry are deliberately absent.** They describe the
*storage space*, not the engine pointed at it, and they are already written by the host side
into `state.json` in the same directory — a file that is engine-neutral and that Maison
already reads. Asking the adapter for them would be a second answer to a question the
deployment has already answered, and it would make the label disappear at exactly the moment
it is most useful: a box that has been issued a space but cannot reach it yet should still be
able to say whose space it is.

`configured: false` and `connected: false` are **different states** and must not be
conflated: the first is a box awaiting provisioning, the second is a fault.

`identity` is what Maison compares against the resident container before using it. A
container disagreeing would open a second lineage inside one repository — see
[Lineage is written once](#lineage-is-written-once).

### `Backup`

```jsonc
{
  "sourceId": "app:nextcloud",
  "stamp": "20260915T031500Z",
  "createdAt": "2026-09-15T03:15:00Z",
  "size": 9871232,                 // bytes; 0 if the engine cannot say cheaply
  "engineId": "kopia"
}
```

---

## Rules that keep this engine-agnostic

These are the invariants. The verb list above will change; these should not.

### Identity travels as fields, never as the engine's metadata model

`(sourceId, stamp, pass)` are protocol fields. How an adapter persists them is its own
business — kopia tags, restic tags, a sidecar manifest for storage with no metadata at all.

This is the leak most likely to become permanent, because kopia's tag model is right there
and works. If `maison-app` / `maison-stamp` / `maison-pass` appear on the wire, every future
adapter inherits kopia's metadata design whether or not its engine has one.

### Maison passes paths; the adapter never derives them

The adapter is given `--path`. It must not compute one from `--source-id`, even though the
mapping is trivial and documented. An adapter that derived paths would be a second definition
of the on-disk layout, and the two would disagree at restore time — the failure that is
invisible until the moment it matters.

### The three retention questions stay separate

*What deletions are sound* (`retentionModel`, the adapter's answer), *who performs them*
(`retention`, the adapter's answer), *what the user asked for* (Maison's). Confusing the first
with the other two destroys data rather than disappointing someone.

### Lineage is written once

Where an engine files snapshots under an identity (kopia's `user@host`), that identity is
written at first connect and **never rewritten**. A reconnect that lets it drift opens a
second lineage inside one repository: the old one is covered by policies nothing writes to,
the new one by no policy at all, and both list normally. Nothing surfaces it until the storage
bill grows or a restore comes back empty.

Maison compares `status.identity` against what it expects before using a resident container.

### Report, do not compute

An adapter reports what it observed. Percentages, rates and ETAs are derived by Maison, in one
place, the same way for every engine — because how much an engine can report varies *within*
one operation, not just between engines.

---

## Cancellation and signals

On `SIGTERM` or `SIGINT` the adapter **must** terminate the engine process it started and exit
non-zero. It must not exit while leaving a child holding files open: Maison cancels a backup
precisely so it can restart an app, and a surviving engine process holds that app's files.

Maison keeps its own pid-file handle on the exec as the outer guarantee, but an adapter that
ignores signals turns a clean cancel into a forced kill.

---

## Versioning

`capabilities.protocol` is the protocol version the adapter implements. Maison refuses an
adapter whose protocol it does not know rather than guessing — an adapter speaking an unknown
dialect is exactly the case where "try it and see" costs a backup.

Unknown flags must be **rejected**, not ignored. Ignoring `--exclude-file` silently ships data
the app declared derived; ignoring `--entries` silently restores more than was asked for.
