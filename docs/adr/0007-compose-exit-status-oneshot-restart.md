# ADR 0007: Exit status, one-shot services, and restart policies

**Status**: Accepted
**Date**: 2026-07-30
**Context**: The last parse blocker for real compose files (`service_completed_successfully`) and the two capabilities behind it (`restart:`, exit-code visibility) all need something the runtime has never had: knowledge of how a guest's process ended

---

## Context

`depends_on: {migrate: {condition: service_completed_successfully}}` is the
migrate-before-app backbone of every database-backed stack, and the last
load error in brain-go's `deploy/docker-compose.yml`. Behind it sit
`restart:` policies (that file's `tworker` relies on "exit non-zero,
compose restarts it" as its whole liveness model) and plain exit-code
visibility in `ps`/`inspect`.

All three need one missing fact: **the exit status of the guest's
process**. What the runtime has today:

- `InstanceState` has no exit-status field at all; `ps` lazily flips a
  record to `stopped` by checking whether the recorded PID is still alive
  (`ps.go:66`). Nothing records *how* it ended.
- The VMM's own exit code is useless as a proxy. Per the graceful-shutdown
  analysis (upstream urunc-dev/urunc#336 and the design gist), QEMU and
  Cloud Hypervisor exit 0 on SIGTERM regardless of what the guest did;
  Firecracker dies by signal today and will also exit 0 once graceful
  shutdown lands.
- The guest side consumes the status: `urunit` runs the app and, on exit,
  reboots the VM. The app's code is not surfaced to the console or
  anywhere the host can read it.
- Nothing supervises a detached instance. `run --detach` returns after
  `vmmCmd.Start()`; no process observes the VM ending, so no policy could
  act on it even if the status were known.

There **is** one channel that already carries an exact exit code: the
urunit-agent's `agentproto` `Exit` frame, used by `hull exec` and
by the compose exec layer (ADR-0004). It reports the code of a process the
agent started — not of the guest's init.

Upstream urunc-dev/urunc#336 ("graceful shutdown of Linux-based VMs") is
adjacent but not the same thing: it makes *stops* clean (power events into
the guest, urunit signal handling, no block-device corruption) and, in the
proposed urunit work, is the natural place for urunit to also report the
app's exit status. It is a real dependency for the *complete* story and
for non-agent images, and it is not a blocker for the parse-clearing
subset — which is why this ADR splits the epic.

## Decision

Ship the subset that needs no upstream change, on an explicit contract,
and record the rest as sequenced follow-up.

### 1. One-shot services (this epic)

A service marked `x-oneshot: true`, or targeted by any dependent's
`service_completed_successfully` condition, runs as a **job**:

- The VM boots with a benign long-lived init, and the service's `command`
  runs through the guest agent, whose `Exit` frame gives the exact code.
- On exit the instance is stopped and its record keeps the code.
- `service_completed_successfully` gates dependents on code 0; a non-zero
  code fails `up` with the code and the job's output, and tears down.
- **Requires an agent-bearing image.** Without `/urunit-agent` the job
  cannot report a status, so `up` fails loudly naming the requirement —
  never a silent "assume success".

### 2. Exit status on the record (this epic)

`InstanceState` gains `ExitCode *int` and `ExitedAt`, set by the job path
above. `stop` records `ExitedAt` and a `StoppedByUser` marker, and leaves
`ExitCode` nil: a signalled VM reports no status, and inventing one would
be exactly the silent approximation ADR-0002 forbids (amended during
implementation; the original text said `stop` would set `ExitCode`, which
contradicted this section's own "a nil code means ended without a
reportable status"). The marker matters beyond bookkeeping: the supervisor
refuses to restart an instance carrying it, for every policy, which is the
distinction docker's restart manager makes and the only thing that stops a
deliberate `stop` from being undone within a poll. `ps` shows it,
`inspect` reports it. A `nil` code on a stopped instance means "ended
without a reportable status" — the honest state for a plain detached
instance whose VM exited on its own, and exactly the gap Phase B closes.

### 3. Restart policies (this epic, poll-based)

`restart: no | always | on-failure[:N] | unless-stopped` is honored by the
**project supervisor**: the per-project gateway daemon (already long-lived
across `up`/`down`) gains a supervision loop that polls instance liveness
on a fixed interval, and on a service's disappearance re-runs it per its
policy with capped exponential backoff. Divergences, all documented:

- Poll-based, so restarts are detected within the interval, not instantly.
- Without an agent the supervisor sees "gone", not "gone with code N", so
  `on-failure` cannot distinguish a failure from a clean exit; a service
  declaring `on-failure` without an agent-bearing image warns that the
  policy degrades to `always`.
- `unless-stopped` and `always` differ only in whether an explicit
  `compose stop` suppresses restart; with no `compose stop` verb yet, they
  behave identically today.

### 4. Sequenced follow-up (explicitly NOT this epic)

Phase B, once upstream #336's urunit work lands: urunit reports the app's
exit status (console sentinel or the agent's channel), and stops become
graceful power events. That removes the agent-image requirement, gives
plain (non-job) services real exit codes, makes `on-failure` exact for
every image, and avoids block-device corruption on stop. This ADR is
written so Phase B *narrows divergences* without changing any of the
names, state fields, or CLI surface introduced here.

## Rejected alternatives

- **Treat "the VM exited" as success.** The obvious shortcut and the worst
  one: a crashed migration would satisfy `service_completed_successfully`
  and let dependents start against a half-migrated schema. Silent
  approximation on a critical path, forbidden by ADR-0002.
- **Use the VMM process's exit code.** Not a proxy for anything: QEMU and
  CH exit 0 whatever the guest did (gist; upstream #336).
- **Block this epic on upstream #336.** #336 is an exploration issue with
  no landed implementation and an arm64/x86 asymmetry still open. Waiting
  leaves the last parse blocker in place indefinitely, when the agent
  channel already carries an exact code for the job case.
- **Parse the guest console for an exit marker.** Needs a urunit change to
  emit one — i.e. it *is* Phase B — and log-scraping as a control path is
  fragile (interleaving, truncation, no framing).
- **A new host-side supervisor daemon per project.** The gateway daemon is
  already exactly that process with the right lifetime; a second daemon
  doubles the failure modes and the teardown paths for no gain.
- **Event-driven restarts via QMP/Vz callbacks.** Attractive and
  monitor-specific: QMP gives `SHUTDOWN` events, Vz gives a delegate
  callback in vz-runner, neither gives an exit code, and both are new
  plumbing in two backends. Polling is honest, uniform, and cheap; the
  event path is a later optimization behind the same behavior.

## Consequences

- brain-go's compose file parses clean after this epic (the `migrate`/
  `seed` one-shots and `tworker`'s restart policy both load); what remains
  for that stack is Bunny-packaged images for its third-party services.
- The gateway daemon becomes load-bearing for service liveness, not just
  networking: its own death now stops restarts happening. Its supervision
  loop must therefore be crash-safe and idempotent against project state.
- New guest-visible contract: one-shot services need `/urunit-agent`, so
  the fixture story from ADR-0004 extends to this epic's runtime tests.
- Manifest movement: `service_completed_successfully` (inside
  `service.depends_on`), `service.restart`, and exit-status visibility in
  `cli.ps`/`cli.events`-adjacent entries; each divergence above becomes a
  named note with a test.
