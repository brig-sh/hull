# hull CLI

This directory holds the Go source of the `hull` command. It is not the
place to read about hull: the [top-level README](../../README.md) covers
installing, building and running it, and [docs/](../../docs/) has the
reference pages for checkpoint/restore, compose, the network gateway's
egress policy and telemetry.

`hull <command> --help` prints the flags of every command. The tests next to
the code run with `make test`; the end-to-end harnesses are under
[test/](../../test/README.md).
