# ADR 0005: Variable interpolation and env_file for compose

**Status**: Accepted
**Date**: 2026-07-30
**Context**: No real-world compose file loads today — `${VAR}` passes through literally and `env_file:` is ignored — so conformance work cannot reach the capabilities developers actually hit first

---

## Context

The gap analysis against a production compose file (brain-go's
`deploy/docker-compose.yml`, ~30 interpolations, `env_file: .env` on every
app service) shows the very first thing that breaks is not a capability —
it is the file's vocabulary. `${BIND_ADDR:-127.0.0.1}:5432:5432` dies in
the port parser as a literal string before any service exists. The
conformance manifest records both gaps honestly (`spec.interpolation` and
`service.env_file`, unsupported), but every other roadmap item hides
behind them: a developer cannot get far enough to notice missing restart
policies.

Two related but distinct mechanisms, per the compose spec:

- **Interpolation** substitutes variable references in the compose file's
  values from the caller's environment plus a `.env` file. It feeds the
  *file*.
- **`env_file:`** loads KEY=VALUE files into a *service's* container
  environment at runtime. It feeds the *guest*.

Docker resolves `.env` against the project directory (the compose file's
directory unless overridden) and lets `--env-file` replace it; shell
environment wins over `.env` for interpolation.

## Decision

Implement both on main (no dependency on the exec-support pin; this is
parser and run-wiring work, fully static-tier testable in CI).

### 1. Interpolation

- Substitution over the parsed YAML node tree's scalar values (amended
  during review: the original raw-text plan was based on a false premise —
  compose-go interpolates the parsed model, which is why docker's comments
  are inert, substituted values cannot inject YAML structure, and its
  errors are path-qualified). Sources: process environment first, then the
  `.env` file next to the compose file (or `--env-file PATH`, a new
  parent-level compose flag, repeatable like docker's).
- Forms supported: `$VAR`, `${VAR}`, `${VAR:-def}`, `${VAR-def}`,
  `${VAR:?err}`, `${VAR?err}`, `${VAR:+alt}`, `${VAR+alt}`, and `$$` as
  the literal-dollar escape. One level of nesting in defaults
  (`${A:-${B}}`) — deeper nesting is rejected with a clear error rather
  than mis-substituted.
- An unset variable with no default substitutes empty and WARNS (docker
  behavior); `:?`/`?` fail loudly with the message.
- `compose config` prints the interpolated result, matching docker. The
  manifest's `cli.config` security note changes accordingly (it currently
  and truthfully says interpolation is NOT performed; after this it
  performs it, and printing resolved secrets to stdout becomes the real
  tradeoff it warns about).

### 2. env_file

- Service key, string and list forms; the long form (`path` +
  `required: false`) included. Paths resolve against the compose file's
  directory. A missing required file fails at load, docker-style.
- Format: KEY=VALUE lines, `#` comments, blank lines, single/double
  quoting per dotenv convention (the same tiny parser serves `.env` and
  `env_file`).
- Precedence into the guest: `environment` beats `env_file`, later files
  beat earlier ones — docker's rule.
- Values flow through the existing `--env` plumbing to the guest.

### 3. Bookkeeping

- Manifest: `spec.interpolation` → partial (static tier; three named
  divergences: `.env` values are not self-expanded, nesting caps at one
  level, a non-variable `$` is lenient where docker errors);
  `service.env_file` → supported (static tier); notes and security fields
  rewritten (env files are read by the host and persisted into instance
  state like all environment values — the existing plaintext-state
  tradeoff extends to them). `cli.config`'s note updated as above.
- Conformance: static cases assert substitution forms, precedence, the
  `$$` escape, unset-warns, `:?`-fails, `.env` vs `--env-file`, missing
  required env_file, quoting. The suite's fixtures gain a `.env` sibling
  where needed (the interpolation runtime probe already asserts the OLD
  behavior — literal passthrough — and flips to asserting substitution).
- Score moves 9.3% → 10.6% in-scope on main (one full support, one
  partial). The real payoff is not the number: it is that real files
  begin to load.

## Rejected alternatives

- **Adopt compose-go for interpolation only.** Its interpolation is
  entangled with its model types; vendoring a whole parser to use one
  pass repeats the ADR-0002 decision in reverse. The dotenv+substitution
  surface is small and fully specified.
- **Interpolate the raw file text before parsing.** Tried first, and the
  review refuted the premise with concrete inputs: raw-text substitution
  interpolates comments (a `:?` in a commented-out line fails the load)
  and lets substituted values inject YAML structure (a value containing
  ` # ` is silently truncated as a comment). compose-go substitutes the
  parsed model; so do we.
- **Treat `.env` as guest environment too.** Docker keeps interpolation
  input and container env strictly separate; conflating them is a
  well-known compose footgun the spec explicitly untangled. We follow the
  spec.
- **Reject unset variables instead of warn-and-empty.** Would diverge
  from docker on the most common case and break the many files that rely
  on empty-when-unset; `:?` exists for callers who want strictness.

## Consequences

- Every stanza of the compose surface becomes reachable by real files;
  later epics (exit codes/restart, named volumes) get testable against
  unmodified production compose files.
- `compose config` output changes for files using `${...}` (literal
  before, resolved after) — the conformance golden tests change with it,
  and the static-tier fixture for interpolation flips its assertion.
- A new parent-level `--env-file` flag on `compose`.
- Secrets caveat becomes sharper: resolved values appear in `compose
  config` output and in persisted instance state; the manifest security
  notes say so explicitly.
