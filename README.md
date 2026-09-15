# maison-kopia-engine

The **kopia adapter** for [Maison](https://github.com/Yundera/maison)'s backup engine seam.

Maison defines backup as an interface (`internal/apps.Provider`) and owns everything that
is not storage: which paths are a source, the per-app lock, the stop → snapshot → restart
sequence, retention intent, progress derivation. An *engine* owns exactly one thing —
getting bytes to durable storage and back.

This repository is the piece that lets kopia be that engine **without kopia being compiled
into Maison**. It ships:

- `maison-engine` — a small CLI implementing the adapter protocol
  ([`docs/protocol.md`](docs/protocol.md)), which is engine-agnostic by design;
- a container image built `FROM` a pinned `kopia/kopia`, so the adapter and the engine
  binary version together;
- [`docs/kopia-mapping.md`](docs/kopia-mapping.md) — how each protocol verb is realised in
  kopia, and the kopia-specific hazards the adapter exists to absorb.

Maison invokes it as `docker exec <engine-container> maison-engine <verb> …`. There is no
HTTP service, no daemon and no listening socket; see
[Why argv and stdio](docs/protocol.md#why-argv-and-stdio).

## Status

**v0 implemented, not yet deployed.** Every verb is written and the unit tests pass; nothing
has run against a real repository or a real PCS yet. The protocol is authored against
Maison's *current* kopia provider, so v0 covers 100% of what Maison does today and nothing
speculative. It is expected to change as a second adapter is written — that is the point of
writing the second one.

Maison's side is in `internal/backup/adapter`, registered from a descriptor the host writes
at `${DATA_ROOT}/AppDataShared/backup/<engine>/adapter.json`. A box with no descriptor keeps
using the kopia engine compiled into Maison, so this changes nothing until that file exists.

Maison's side of the same design is
[`docs/backup.md` → The engine adapter](https://github.com/Yundera/maison/blob/main/docs/backup.md),
which is authoritative for the split of responsibility. This repository is authoritative for
the wire format.

## Why an adapter at all

Adding restic today means editing Maison, releasing Maison, and shipping a new Maison image
to every PCS — for a change that alters no Maison behaviour. With an adapter, adding an
engine means publishing an image and naming it in the host-side stack.

The secondary win is that a pin currently duplicated across two repositories collapses into
one: `KOPIA_IMAGE` in `template-root/scripts/library/kopia.sh` and `kopia.DefaultImage` in
Maison are held together today by a comment saying they must match. An adapter image built
from a pinned engine carries that version inside it.

## Layout

```
cmd/maison-engine/       verb dispatch, strict unknown-flag rejection, exit codes, signal forwarding
internal/proto/          the wire types and the NDJSON emitter — engine-agnostic
internal/kopia/          the verbs, on kopia's CLI
Dockerfile               FROM golang (build) → FROM kopia/kopia (ship)
docs/protocol.md         the adapter contract — engine-agnostic, the thing a second adapter implements
docs/kopia-mapping.md    verb → kopia command, and the kopia-specific hazards
```

## Build and test

```bash
go test ./...
docker build -t ghcr.io/yundera/maison-kopia-engine:dev .
```

The engine version is pinned in the Dockerfile, so it moves with the adapter rather than
with Maison. Bumping kopia is a change to that one line.

## Publishing

`.github/workflows/publish.yml` builds `ghcr.io/yundera/maison-kopia-engine` for
`linux/amd64` and `linux/arm64` on every push to `main` and on `v*` tags, and tags a `v*`
build `latest`. A pull request runs the tests and the build but publishes nothing.

Every publish is gated on `gofmt`, `go vet` and `go test`, and the pushed image is then
smoke-tested by running `capabilities` against it — the one verb that needs no repository,
no network and no credentials, and the only way to prove the ENTRYPOINT actually works.

**Deployments pin a version, never `latest`.** The PCS template names the image it wants;
an engine that changes under a live repository turns a format surprise into a 3am failure.
