<!--
Title: use a conventional-commit subject, e.g. feat(compose): named volumes
Example scopes: compose, run, exec, store, conformance, vz-runner, qemu, ci, docs.
-->

## Summary

<!-- What this changes and why. Lead with the problem, then the approach. -->

## Related issues

<!-- Closes #NN, Refs #MM. Delete if none. -->

## Changes

<!-- The notable changes, one bullet each. -->

-

## Checklist

<!--
Check items as you complete them; strike through (~~like this~~) any that do
not apply, rather than deleting or rewording them. Keep the reasoning in
Summary or Changes, not here.

`make test` runs the Go unit suite and the static conformance tier;
`make macos` builds and ad-hoc signs urunc-macos + vz-runner. The PTY and
shared-folder harnesses under test/ boot real VMs and need an Apple Silicon
host with working HVF. See CONTRIBUTING.md.
-->

- [ ] `make test` passes (unit + static conformance)
- [ ] `make conformance-report` leaves `docs/compose-conformance.md` unchanged
- [ ] `make macos` builds urunc-macos + vz-runner, if Go or Swift code changed
- [ ] I have added or updated tests covering the change
- [ ] I have run the PTY / shared-folder e2e harnesses (`test/*.py`) for changes touching boot, console, or the run path
- [ ] I have updated the affected docs (README, `docs/`)

## LLM usage

<!--
For transparency: note any AI assistance used in this change, e.g.
"Authored with assistance from Claude Code; I have reviewed every line and am
accountable for the change."
-->
