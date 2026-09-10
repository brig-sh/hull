# hull CLI

This directory holds the Go source of the `hull` command. It is not the place
to read about hull.

- [The top-level README](../../README.md) introduces hull and has the quick
  start.
- [docs/](../../docs/README.md) is the documentation index. Start there for
  installing, running workloads, choosing a backend, storage, networking,
  compose, checkpoints, the CLI reference and telemetry.
- [docs/build.md](../../docs/build.md) covers building from source and running
  the tests.
- [docs/architecture.md](../../docs/architecture.md) explains how this package
  relates to the runners, the store and the gateway.

`hull <command> --help` prints the flags of every command, and
[docs/cli.md](../../docs/cli.md) is the same information in one page.

The unit tests sit next to the code and run with `make test`. The end-to-end
harnesses are under [test/](../../test/README.md).
