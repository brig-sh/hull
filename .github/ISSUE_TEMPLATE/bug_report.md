---
name: Bug report
about: Report a defect or incorrect behavior
title: ''
labels: bug
assignees: ''
---

<!--
Write a specific, imperative title, e.g. "compose logs loses --store-dir on
the single-service path". Search open and closed issues first to avoid
duplicates.
-->

## Describe the bug

<!-- A clear description of what is wrong. -->

## To reproduce

<!--
The exact steps. Include the command line and any relevant knobs
(--store-dir, --env-file, x-hypervisor, ...). For boot, console, or run-path
bugs, the PTY harnesses under test/ (see test/pty-terminal-test.py) are the
usual repro path; for compose loading bugs, a minimal compose file plus
`hull compose config` output is ideal.
-->

1.
2.

## Expected behavior

<!-- What you expected to happen. -->

## Actual behavior

<!-- What happened instead. Paste the error verbatim if there is one. -->

## Environment

<!-- Fill in what applies; delete the rest. -->

- Component / scope: <!-- compose, run, exec, store, vz-runner, qemu, ... -->
- Hypervisor: <!-- vz | qemu -->
- macOS version and chip: <!-- e.g. macOS 26.0, M3 Pro -->
- hull version / commit: <!-- hull --version, or git rev-parse HEAD -->
- Guest image: <!-- e.g. harbor.nbfc.io/nubificus/urunc-ubuntu-vz:aarch64 -->
- Store dir: <!-- default, or the --store-dir / HULL_STORE_DIR in use -->

## Logs and additional context

<!--
Relevant log output (fenced), and anything else that helps. Strip ANSI before
pasting log lines. Link related issues inline with #NN.
-->
