# ADR 0009: Adopt compose-go as the compose loader

**Status**: Accepted
**Date**: 2026-08-12
**Context**: Four hand-rolled parsing epics since ADR-0002 have pushed
`cmd/hull/compose.go` to ~3,400 lines re-deriving compose-go behavior
by hand; ADR-0002 deferred adopting compose-go as "a separate, much larger
decision," and this ADR makes it

---

## Context

ADR-0002 built the conformance suite against the hand-rolled parser on
purpose, and explicitly deferred the parser question: "Replacing the parser
is a separate, much larger decision... A conformance suite must measure the
implementation we have, not presuppose a rewrite." It vendored only the
compose-spec JSON schema, for the key enumeration, and noted that if
compose-go were adopted later, the manifest and suite would carry over
unchanged because the suite is black-box.

Since then, four epics have each re-derived a slice of compose-go's territory
by hand:

- ADR-0005 (interpolation and `env_file`) started from a raw-text
  substitution plan, found it wrong on concrete inputs (interpolating
  comments, letting substituted values inject YAML structure), and rebuilt
  interpolation over the parsed model instead, arriving by hand at the
  approach compose-go already takes.
- ADR-0006 (named volumes) and ADR-0007 (exit status, one-shot services,
  restart policies) each added their own directive-specific parsing and
  validation.
- Profiles (`eab7538`) and `include` (`3e210bb`, `50bd854`) added further
  hand-rolled directive handling with no ADR of their own.

`cmd/hull/compose.go` plus `exec_compose.go` are now roughly 3,400
lines, most of it schema and validation logic that compose-go already
implements and maintains against the spec. `docs/compose-support.md`'s open
question 3 already names compose-go as the fidelity target, "at the cost of a
heavyweight dependency." That cost is the thing this ADR now weighs
explicitly, and the answer changes: the four epics above are the evidence
that hand-rolling keeps costing more than the dependency would have. The
"separate, much larger decision" ADR-0002 deferred is now ripe, and this ADR
reverses the "adopt compose-go" rejected alternative recorded in both
ADR-0002 and ADR-0005. Those ADRs are immutable and stay correct as records
of the reasoning at the time; this ADR is where the reversal is recorded.

## Decision

Adopt `compose-go` v2.14.0 (`github.com/compose-spec/compose-go/v2`) as the
only compose loader. A new `internal/compose` package wraps it; the runtime
layer consumes its `types.Project` instead of the bespoke representation
built by `cmd/hull/compose.go`. The bespoke loader is deleted once the
runtime is rewired (later tasks on this branch). Where compose-go's behavior
differs from what the bespoke parser did, compose-go's behavior wins: the
target is docker fidelity, not compatibility with our own parser's quirks.

### Divergences from docker (seed list, grows during implementation)

These are the divergences already known. Later implementation work adds to
this list as it finds more; it is a starting point, not a closed set.

- Unknown keys, including typos, now hard-fail under compose-go's strict
  validation, where the bespoke parser warned and continued.
- Schema validation may reject files that previously loaded with warnings.
- `compose config` output becomes compose-go's canonical form: key order and
  normalized value forms change.
- The file-size cap added in `4b11beb` still applies to the `-f` files and to
  env files read before compose-go loads them. A top-level `include:` target
  gets the same streaming-limit protection: `internal/compose.Load` reads and
  caps it itself, before compose-go's own `LoadProject` gets a chance to read
  it unbounded. A nested include (one reached through another include) and
  every `extends: {file: ...}` target, at any depth, are capped only after
  compose-go has already read them in full — a real, weaker gap, not merely
  a stat check, and one this branch has not closed. Docker itself has no cap
  at all; ours is extra hardening, kept wherever it is cheap to keep.
- `toplevel.name` stays unsupported, same as the bespoke parser (this seed
  list originally predicted it would become supported; implementation found
  otherwise). `internal/compose.Load` always passes a non-empty
  `Options.Name` (CLI flag, `COMPOSE_PROJECT_NAME`, or a directory-name
  default), and compose-go's project-naming precedence never lets a `name:`
  field inside the compose file override a caller-supplied name. The `name:`
  key loads and validates, but has no effect.
- `service.extends` is now supported. It and `include` are both handled
  through compose-go's `cli.WithListeners`, but they resolve relative paths
  against different bases: `include` resolves against the listener's own
  reported `workingdir` metadata, `extends` resolves against
  `configDetails.WorkingDir`. `internal/compose` routes both through the
  same read-cap-and-warn-walk logic, using the event-appropriate base for
  each — this is one underlying compose-go behavior, not a divergence, but
  the two events look identical until you read their resolution semantics.
- `compose config` output is not purely `p.MarshalYAML()`. Everything
  `serviceRunArgs` transforms on its way to the VM is rendered as the value
  the VM actually receives, not as declared, so `config` and `up` cannot
  disagree. Four of those are synthesized by `hydrateConfigOutput` before
  the model is marshaled: `x-hypervisor` defaults, `x-oneshot` defaults,
  `cpus` rounded up to whole vCPUs, and `mem_limit` floored to whole
  megabytes. The fifth, resolved named-volume paths, is handled by
  `collapseNamedVolumes`, which runs after `MarshalYAML` and rewrites the
  marshaled bytes rather than the model. That set is exactly the list of
  values `serviceRunArgs` converts, which is what makes it closed rather
  than a running list of one-offs.

  `cpus` and `mem_limit` are where "docker behavior wins" genuinely does
  not apply. Docker renders the declared `0.5` and the declared byte count,
  and docker can deliver a fractional CPU share; urunc allocates whole
  vCPUs and whole megabytes, so `up` passes the rounded and floored values
  and a load-time warning says so for `cpus`. Rendering the declared values
  would contradict both that warning and what the VM gets: a declared
  3.5 MiB boots a 3 MB VM.

- Keys hull ignores now appear in `compose config` output, where the
  bespoke renderer omitted them and printed only the honored surface.
  Warnings on stderr remain the authority on what is ignored. Suppressing
  them from the document would mean rebuilding by hand the whitelist of
  every recognized compose key, which is the maintenance burden adopting
  compose-go removes. `environment` likewise renders as a mapping rather
  than a `KEY=value` list, matching `docker compose config`.

### Known limitations (implementation-observed)

These do not fail the gate — each is a narrow, documented residual, not a
regression from the bespoke parser's own behavior on the same input.

- `extends` chains more than one level deep are read without the same
  recursive size-cap walk that `include` chains get. Same class of gap as
  the file-size-cap divergence above, on the newer of the two events.
- When several services `extends` the same base, compose-go fires one
  listener event per extending service, and the warn-walk's key-dedupe is
  per-call rather than cross-call — the same unsupported-key warning can
  print once per extending service instead of once. Noisy, not incorrect.
- Multiple `-f` files are not implemented: `internal/compose.Options.Files`
  and the CLI flag that fills it are both a single value, not a list. A
  compose invocation that relies on layering more than one `-f` file is a
  real gap, not yet attempted on this branch.

Two limitations recorded in earlier drafts of this list are now fixed, not
deferred: an `include` entry's `project_directory` supplied through a YAML
merge key (`<<`) is detected the same as a literal one
(`includeEntryHasProjectDirectory` now reads it through `mappingEntries`,
which folds merge keys); and `service.mem_limit` under 1 MiB — reachable
with the `k`/`b`-suffixed forms compose-go accepts and the bespoke
`parseMemLimit` used to reject outright — is now rejected by
`validateProject` instead of silently flooring to a 0 MB VM request.
`reloadProject`'s tolerance for a drifted `include:` target was also
extended to cover a drifted `extends: {file: ...}` target the same way,
once `service.extends` itself became supported on this branch: a missing
or mid-save extends base now degrades that one service instead of failing
the whole reload.

### Verified during implementation

- Naming a profile-gated service directly on the CLI activates that
  service's own profile, matching docker (fixed in `b250829`). A sibling
  service that shares the profile but is not itself named still does not
  start. An earlier draft of this list, based on a task report later found
  to contain errors, claimed the opposite; this was checked directly against
  compose-go's `ModelToProject` sequence, compose-go's own tests, and
  docker's published docs before being corrected here.

### Dependency audit

`compose-go` v2.14.0, Apache-2.0 license, requires Go 1.24 (this repo is on
Go 1.26.4, so no toolchain bump is needed). New transitive dependencies it
pulls in: `santhosh-tekuri/jsonschema/v6`, `go-viper/mapstructure/v2`,
`mattn/go-shellwords`, `xhit/go-str2duration/v2`, `go.yaml.in/yaml/v4`,
`distribution/reference`, `docker/go-connections`, `docker/go-units`.
Dependencies already in `go.mod` that compose-go also uses: `logrus`,
`yaml.v3`, `golang.org/x/sys`, `golang.org/x/sync`, `golang.org/x/text`,
`opencontainers/go-digest`. Nothing in the dependency tree pulls in daemon
code or network code.

### Gate

The existing conformance suite (ADR-0002) stays the referee, unchanged: a
black-box suite that measures behavior, not implementation, so the swap is
graded the same way the bespoke parser was. Two conditions gate landing this
work: the per-capability manifest diff must be monotonic (no capability
regresses from `supported` or `partial` to a worse status), and the headline
in-scope score must stay at or above its current 15.4%.

**Result:** both conditions held. The 135-capability manifest diff was
checked for monotonicity twice — once during task review, once after a
later fix round that touched no manifest status fields — and found zero
downward moves both times. The headline score rose from 15.4% to 17.1%,
driven by `service.environment`, `service.extends`, and `toplevel.include`
moving to `supported` on real, passing tests.

## Rejected alternatives

- **Keep hand-rolling the parser.** The status quo. Each of the last four
  epics re-derived a piece of compose-go's already-solved schema and
  validation logic by hand, and ADR-0005 shows the cost directly: its first,
  raw-text interpolation design was wrong, and the fix was to rebuild it over
  the parsed model, the same approach compose-go already uses. The pattern is
  the evidence.
- **Adopt compose-go for one area only (e.g., interpolation), keep the
  bespoke loader for the rest.** This is what ADR-0005 rejected, for good
  reason at the time: compose-go's interpolation is entangled with its model
  types, so using one pass of it means vendoring the whole parser anyway.
  That reasoning now argues for full adoption rather than against partial
  adoption: running two models of the same compose file side by side would be
  worse than either alone.
- **Vendor only the JSON schema, as ADR-0002 already does for the manifest,
  without adopting the loader.** Keeps the conformance manifest's key
  enumeration current but does not fix the actual problem: each new epic
  still hand-derives docker's runtime semantics, not just its key names.

## Consequences

- `cmd/hull/compose.go`'s and `exec_compose.go`'s parsing and
  validation logic are deleted; the command and runtime layers are rewritten
  against `types.Project` (tasks 2 through 11 of this branch).
- Behavior changes per the divergence list above. The most visible one: files
  with unknown or misspelled keys that previously loaded with a warning now
  fail to load.
- `compose config` output changes to compose-go's canonical form; conformance
  golden fixtures that assert exact output are updated with it.
- `go.mod` and `go.sum` gain the transitive dependencies listed above.
- The conformance suite and its manifest (ADR-0002) are unchanged in
  structure; individual test cases are updated where the divergence list
  changes expected behavior, but the suite continues to serve as the
  black-box referee it was built to be.
- ADR-0002's and ADR-0005's "reject compose-go" reasoning is superseded going
  forward by the decision recorded here; those ADRs are not edited and remain
  accurate records of the reasoning at the time they were written.
