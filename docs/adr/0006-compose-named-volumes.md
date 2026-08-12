# ADR 0006: Named volumes for compose

**Status**: Accepted
**Date**: 2026-07-30
**Context**: The second hard load failure every stateful compose file hits — `pgdata:/var/lib/postgresql/data` is rejected outright, so no database-backed stack can even parse

---

## Context

The brain-go gap analysis puts named volumes right behind interpolation:
`volumes: [pgdata:/var/lib/postgresql/data]` plus a top-level
`volumes: {pgdata:}` fails at load ("named volumes are not supported").
The pattern is the backbone of every stateful dev stack — data that must
survive `down`/`up` cycles, dropped only by an explicit `down -v`.

What exists: bind mounts ride virtiofs via `run --shared-dir` (one tag per
entry, mounted by the init wrapper); `validateVolume` rejects anything
whose host side is not a path; the top-level `volumes:` element is warned
about and dropped; `down` has no flags.

What a VM-per-service runtime can honestly offer: a named volume is a
host directory managed by the store, mounted into guests over virtiofs.
Two guests sharing one volume see host-filesystem semantics through two
independent virtiofs devices — not the single-kernel page-cache coherence
Docker containers get. For the dominant use (one service owns the volume,
e.g. postgres) this is indistinguishable; for concurrent writers it is a
real, documentable divergence.

## Decision

### 1. Semantics

- **Declaration required**: a service volume whose source is not a path
  (`pgdata:/var/lib/...`) must be declared under top-level `volumes:`,
  else load fails naming the typo'd volume — the spec's rule, and it
  preserves ADR-0002's "nothing silently invented".
- **Storage**: `<store>/volumes/<project>_<name>` — docker's naming
  scheme, created on first `up` (0755), reused thereafter. Contents
  persist across `down`/`up`.
- **Mounting**: resolved to the managed directory and passed through the
  existing `--shared-dir` plumbing; nothing new below the compose layer.
- **`down --volumes` (alias `-v`)**: removes the project's named-volume
  directories after teardown. Plain `down` never touches them.
- **`compose config`** prints the volume entry with its resolved managed
  path visible in `volumes:` service entries (docker prints the volume
  name; we print name and keep the divergence documented).

### 2. Scope fences (all warn or error loudly, per ADR-0002)

- Top-level declaration options (`driver`, `driver_opts`, `external`,
  `labels`, `name`) are unsupported and warn individually; only the bare
  `volname:` / `volname: {}` form is honored.
- Anonymous volumes (`- /just/a/guest/path`) stay unsupported (error, as
  today).
- Long-syntax `volumes:` entries stay unsupported (raw type error today;
  unchanged by this epic).
- `:ro` on a named volume carries the same not-enforced warning binds
  have.

### 3. Bookkeeping

- Manifest: `toplevel.volumes` → partial (bare declarations; options
  warn); `service.volumes` notes gain the named-volume support and the
  multi-writer coherence divergence. Security notes: named volumes are
  plain host directories under the store — readable/writable by any
  process with store access and shared across all services that mount
  them; `down -v` is the only deletion path and there are no quotas.
- Static cases: declaration enforcement (undeclared name fails), managed
  path resolution in `config`, option warnings, `down -v` flag parsing.
  Runtime case: data written by a guest survives `down`/`up` and is gone
  after `down -v`.
- The brain-go file's `pgdata` stanza becomes loadable; with the
  already-scoped exit-codes epic, that file's parse phase is fully clear.

## Rejected alternatives

- **Auto-create undeclared named volumes.** Hides typos (a misspelled
  volume silently forks the data), violates the spec's declaration rule,
  and contradicts the suite's loud-failure invariant.
- **Block-device volumes (ext4 images per volume).** Real isolation and
  quota story, but single-writer only, needs mkfs/resize plumbing, and
  kills the postgres-in-one-service case no better than virtiofs; the
  block path can arrive later behind the same names without breaking the
  layout contract.
- **Docker-compatible `local` driver options.** The options configure
  Linux mount(8) semantics that have no meaning for a host-dir-over-
  virtiofs volume; accepting-and-ignoring them would be silent
  approximation.
- **Anonymous volumes in this epic.** They require container-lifecycle
  garbage collection to be anything but a leak generator; the dev-stack
  value is almost entirely in named volumes.

## Consequences

- Stateful dev stacks (postgres et al.) get durable data with docker's
  lifecycle (`down` keeps, `down -v` drops).
- A new managed directory tree under the store; `<store>/volumes` becomes
  part of the store's disk footprint with no quota — documented.
- Multi-writer coherence divergence is documented and test-pinned rather
  than papered over.
- `down` gains its first flag; the CLI verb stays `partial` with the
  remaining docker flags still absent.
