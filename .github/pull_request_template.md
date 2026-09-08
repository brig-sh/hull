<!--
Title: use a conventional-commit subject, e.g. feat(compose): named volumes
Example scopes: compose, run, exec, store, vz-runner, qemu, ci, docs.
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

`make test` runs the Go unit suite;
`make macos` builds and ad-hoc signs hull, vz-runner and hvi. The harnesses
under test/ (PTY, checkpoint, shared-folder, hvi boot, Rosetta) boot real VMs
and need an Apple Silicon host with working HVF. See CONTRIBUTING.md and
test/README.md.
-->

- [ ] `make test` passes
- [ ] `make macos` builds hull, vz-runner and hvi, if Go, Swift or Rust code changed
- [ ] I have added or updated tests covering the change
- [ ] I have run the e2e harnesses (`test/*.py`) for changes touching boot, console, or the run path
- [ ] I have updated the affected docs (README, `docs/`)
