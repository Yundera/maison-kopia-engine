# Kopia mapping

How each verb of [the adapter protocol](protocol.md) is realised in kopia, and the
kopia-specific hazards this adapter exists to absorb so that Maison never has to know them.

Derived from Maison's current `internal/backup/kopia`, which this adapter replaces. Where
that package already solved something the hard way, it is recorded here rather than
rediscovered.

---

## Environment every invocation needs

| | Source | Note |
|---|---|---|
| `KOPIA_PASSWORD` | `repository.password` in `--repo-dir` | Read per invocation, never cached across one. |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `credentials.env` in `--repo-dir` | **Read fresh every time.** They rotate roughly every 90 days, and an adapter that read them at start would serve errors until it was restarted. |
| `KOPIA_CACHE_DIRECTORY` | `<repo-dir>/cache` | Must be set explicitly — see below. |
| `KOPIA_LOG_DIR` | `<repo-dir>/logs` | Same. |

`--config-file=<repo-dir>/repository.config` on every command.

**The image bakes `KOPIA_CACHE_DIRECTORY=/app/cache` and `KOPIA_LOG_DIR=/app/logs`, and
those outrank `--cache-directory` and `--log-dir` on the command line.** `/app` belongs to
root, so a non-root engine cannot create either and every command fails with
`unable to create cache directory: mkdir /app/cache: permission denied` before it ever
reaches the repository. Both must be overridden through the environment, not through flags.

`cache/` and `logs/` live inside `--repo-dir` and are **excluded from the user-data set by
pattern** (`**/cache/`, `**/logs/`) rather than by name, so a future engine's cache is not
silently shipped offsite forever.

---

## Verb mapping

| Verb | kopia |
|---|---|
| `capabilities` | static; `engineVersion` from `kopia --version` |
| `connect` | `repository create s3 …` / `repository connect s3 …` |
| `status` | `repository status` + `state.json` from `--repo-dir` |
| `prepare` | `repository status` (warms the cache, proves reachability) |
| `snapshot` | `snapshot create <path> --progress --tags maison-app:<src> --tags maison-stamp:<stamp> --tags maison-pass:<n>` |
| `commit` | `snapshot list` for the pair, then `snapshot delete` the pass-1 manifest |
| `abort` | `snapshot delete` every manifest carrying the stamp |
| `list` / `list-all` | `snapshot list --all --json`, filtered by tag |
| `materialize` | `snapshot restore <id> <dest>` |
| `restore-in-place` | `snapshot restore <id> <dest>` over the live folder |
| `delete` | `snapshot delete <id> --delete` |
| `ensure-retention` | `policy set <path> --keep-hourly … --keep-annual …` |

### Tags are the identity carrier — and stay inside this adapter

`(sourceId, stamp, pass)` arrive as protocol fields and become three kopia tags. That
translation is the adapter's entire reason to exist, and the tag names must never appear on
the wire.

**The asymmetry that bites:** tags are *written* as `key:value` but come *back* from `--json`
under a `tag:` prefix. A filter built from the read spelling matches nothing, silently.

The user-data source is filed under the reserved tag value `_userdata`. A leading underscore
is unrepresentable in a compose project name, so it cannot collide with a real app — the
validation makes the reservation rather than the adapter having to police it.

### `commit` deletes the first pass

Both passes are real kopia snapshots against the same source. `commit` removes the pass-1
manifest so a user browsing snapshots can never restore the torn one. Content is shared, so
this frees nothing and loses nothing.

**A failure here is logged, not returned.** The real backup exists; refusing to commit it
because a cleanup failed is the worse outcome. The stale manifest is invisible to `list` and
is swept later.

### Incremental passes are free

kopia is content-addressed, so pass 2 against the same source uploads only what changed
during pass 1. `capabilities.incrementalPasses` is `true` with nothing to implement — this is
the property an rclone-style adapter would *not* have.

### Excludes become an ignore policy

`--exclude-file` maps to `policy set <path> --clear-ignore` followed by one `--add-ignore` per
pattern. Clearing first is what makes it converge: without it, a pattern removed from the app
manifest would linger in the policy forever.

Installing them as policy also means kopia's own UI honours the same exclusions.

The adapter installs them exactly when `--exclude-file` is given and never on its own
judgement. It has no way to know whether this snapshot is the live pass of an app about to
be stopped or the only pass of a set that stops nothing, and guessing wrong in the
permissive direction costs an outage a second round trip while guessing wrong in the other
direction ships data that was declared excluded.

---

## Hazards

### Identity must be written exactly once

kopia keys snapshots `user@host:path`. The configuration is written at first connect with
`--override-hostname` / `--override-username` (pinned to the box's device id), and **never
rewritten**.

Installing a rotated key by re-running `repository connect` rewrites the whole configuration,
and a reconnect that omits the overrides refiles the box under the container's random
hostname and `root`. That costs a full re-hash of every file and leaves per-source retention
pointed at a lineage nothing writes to, while the new one is covered by no policy at all.
`snapshot list --all` keeps showing everything, so none of it is visible until the storage
bill grows.

The resident container's `hostname` must therefore match `repository.config`. Maison compares
`status.identity` before using the container.

### Storage credentials must not be persisted into the config

`repository connect` writes whatever it was given into `repository.config`, and offers no flag
to stop it — `--no-persist-credentials` governs the repository *password*, not the storage
key. The host script blanks the two persisted fields afterwards and rotates `credentials.env`
alone; kopia accepts the key from the environment for every ordinary operation and does not
write it back.

`connect` must preserve this: blank the persisted fields after creating or connecting.

### `--endpoint` is `host[:port]`, not a URL

Given a URL, kopia fails with `Endpoint url is not valid`. The credential API returns a URL
because that contract is engine-independent, so **the scheme is stripped here** — this is the
only place that knows what kopia parses. Today that stripping lives in the host script; it
belongs in `connect`.

### A filesystem repository has no `credentials.env`

That is not an error, and it is how this is developed and tested. Never branch on who wrote
the configuration: a hand-written `repository.config` pointing at a local MinIO or a
filesystem path must exercise every path.

### Never `--enable-actions`

It lets a snapshot policy run arbitrary commands, which turns the repository into a remote
execution surface. It is absent from the UI container for the same reason and must stay absent
here.

---

## Container

Built `FROM kopia/kopia:<pinned>`. Pinning is not caution: an engine that changes under a live
repository turns a format surprise into a 3am failure.

Two containers share the image and the repository — the adapter (`sleep infinity`, exec'd
into) and kopia's own web UI behind an AppShield gate. That is kopia's ordinary desktop
arrangement and is verified to work: a `snapshot create` from one while the other serves
completes normally. They are separate because their lifecycles differ — the UI reads
credentials once at start and must be restarted when they rotate; the adapter reads them per
invocation and never needs restarting.

Runs as root with `cap_drop: ALL` and exactly five capabilities: `DAC_READ_SEARCH`,
`DAC_OVERRIDE` (read an app's private data — postgres' `0700` pgdata is the usual one), and
`CHOWN`, `FOWNER`, `FSETID` (put recorded ownership and setuid bits back, so a restored
database starts instead of coming back as a directory postgres refuses).

**Never on the `pcs` network.** Two independent reasons: the auth-registrar derives an app's
OIDC client id from a reverse-DNS lookup of the calling container's name on that network, so a
named container there is a claimable identity; and this container has root and read access to
the whole data root, which on `pcs` would be reachable by every store app the owner installs.
