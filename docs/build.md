# Building from source

You only need this to work on hull. To use it, install a release; see
[install.md](install.md).

hull is three executables in three languages. All three must be built, and the
two runners must be signed before they can start a VM.

| Executable | Language | Built by |
|---|---|---|
| `hull` | Go | `go build`, from `cmd/hull` |
| `vz-runner` | Swift | `swift build`, from `vz-runner/` |
| `hvi` | Rust | `cargo build`, from the `hvi-vmm` submodule |

## Prerequisites

- An Apple Silicon Mac. There is no other build target.
- **Go** 1.26 or later. `go.mod` declares 1.26.4.
- **Xcode command line tools**, which supply Swift. `vz-runner` declares
  `swift-tools-version:5.9` and a macOS 14 deployment target.
- **Rust.** The `hvi-vmm` submodule pins stable **1.95.0** in its
  `rust-toolchain.toml`. Install `rustup` and let it read that pin.
- Optional: `brew install e2fsprogs` for the block rootfs mode, and
  `brew install qemu` for the QEMU backend.

Two Rust version numbers exist and mean different things. `1.95.0` in
`rust-toolchain.toml` is the **build pin**, chosen so a fresh checkout builds
reproducibly against the committed lockfile. `rust-version` in `Cargo.toml`
declares a **1.77 MSRV floor**, which is a separate compatibility contract. A
third toolchain, the nightly `nightly-2026-08-30`, is pinned only for a
comment-reflow check, because `rustfmt`'s `wrap_comments` option is
nightly-only. It is not needed to build hvi.

## Clone with submodules

`hvi-vmm` is a submodule, and `make macos` needs it:

```bash
git clone --recursive https://github.com/brig-sh/hull.git
cd hull
```

If you already cloned without `--recursive`:

```bash
git submodule update --init hvi-vmm
```

`make hvi_vmm` checks for `hvi-vmm/Cargo.toml` and stops with that exact
command in the error when the submodule is missing.

The submodule is pinned to a **commit**, not a branch. `git submodule status`
showing a `+` prefix means the checked-out commit differs from the one this
repository records. Run `git submodule update --init` to get back to the
recorded pin, and do not commit a submodule bump as a side effect of unrelated
work.

## Build

```bash
make macos
```

That runs four steps: build `hull`, build `vz-runner`, build `hvi`, then sign
all three ad-hoc and strip the quarantine attribute.

Individual targets:

| Target | What it does |
|---|---|
| `make urunc_macos` | build the Go CLI to `dist/hull_arm64` |
| `make vz_runner` | `swift build -c release` in `vz-runner/` |
| `make hvi_vmm` | `cargo build --release` in `hvi-vmm/` |
| `make sign` | sign all three and strip quarantine |
| `make codesign_verify` | print each binary's authority chain and entitlements |
| `make test` | `go test -count=1 ./...` |
| `make app` | assemble `hull.app` with both runners inside `Contents/MacOS` |
| `make dmg_image` | build and sign the installer image, no notarization |
| `make dmg` | `dmg_image` plus notarize and staple |
| `make release_tarball` | package the signed binaries into the release archive |
| `make install` | install all three into `PREFIX`, default `/usr/local/bin` |
| `make clean` | remove build artifacts |
| `make help` | list every target |

`make hvi_vmm` runs `cargo` from inside the submodule rather than passing
`--manifest-path`. That is deliberate: `rustup` picks a toolchain from the
working directory, not from the manifest, so building from the repository root
silently ignored the submodule's pin. On a runner with no default toolchain
there is then nothing to fall back to, and cargo refuses to choose.

### Build concurrency

`cargo` and `swift build` both parallelize by default. On a machine running
other work, cap them rather than letting each build assume it owns the box.

## Sibling discovery, and what it means for PATH

hull locates `vz-runner` and `hvi` **next to its own executable**. Wherever one
is staged, the other two must be too.

For a local build, symlink the runners next to the CLI:

```bash
ln -sf vz-runner/.build/arm64-apple-macosx/release/vz-runner .
ln -sf hvi-vmm/target/release/hvi .
./dist/hull_arm64 --help
```

Or use `make install`, which copies all three into `PREFIX` and re-signs
`vz-runner` and `hvi` there. It does not re-sign `hull`, which needs no
entitlement.

Putting only `hull` on your PATH is the most common local-build mistake. The
`vz` and `hvi` backends then fail with a message about not finding the runner.

## Signing

A freshly built runner cannot start a VM until it is signed with the right
entitlement. `make macos` signs ad-hoc, which is the default.

```bash
security find-identity -v -p codesigning
make sign CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
make codesign_verify
```

Prefer a real Apple identity. An *Apple Development* certificate is the one for
local work, and the entitlement is then honored with SIP enabled.

An ad-hoc signature does embed the entitlement, but macOS only honors it with
**SIP disabled**. CI tests both directions on real hardware. So an ad-hoc build
works on a machine with SIP off and will be refused the entitlement on a normal
Mac. **Do not disable SIP as a way around signing.**
[signing.md](signing.md#ad-hoc-signing) has the evidence and the full table.

A **rebuild** does invalidate a signature, because it produces different bytes.
`make macos` handles that by signing after it builds. A **copy** does not: see
[signing.md](signing.md#does-copying-a-binary-break-its-signature).

## `--version` is absent from a plain `go build`

The version string arrives through an `-ldflags -X`, which the Makefile and
goreleaser supply and a bare `go build` does not. The CLI library hides the
version flag when the string is empty, so a hand-built binary answers:

```
flag provided but not defined: -version
```

That is expected. Build with `make` if you need `hull --version`.

## Testing

```bash
make test                                  # Go unit suite
go vet ./...
test/compose-config-smoke.sh dist/hull_arm64
```

`test/compose-config-smoke.sh` runs `hull compose config` against a built
binary and needs no VM, so it works anywhere.

Everything else under [test/](../test/README.md) boots a real VM and needs an
Apple Silicon host with working HVF. Those harnesses **skip with a named reason
and exit status 0** when the host cannot run them.

CAUTION: A zero exit from a PTY, checkpoint, share, hvi-boot or Rosetta harness
means it ran **or skipped**. Read the output. Do not report a skip as a pass.

Use a throwaway store so a test run cannot disturb your own:

```bash
HULL_STORE_DIR=$(mktemp -d) test/compose-config-smoke.sh dist/hull_arm64
```

Remember that hull mounts a store and never unmounts it, so add
`hull store detach` to any cleanup that needs to delete the directory
afterwards.

## The urunc dependency

hull consumes [urunc](https://github.com/urunc-dev/urunc) as a Go dependency.
The darwin work hull needs, the Vz backend, exec, console, the GUI window flags,
the HVI backend and the generic container initrd, lives on the upstream branch
`feat/initrd-hvi-backend-v0.8.0`, which is the darwin commits rebased onto the
v0.8.0 release. `go.mod` requires that branch's tip as a pseudo-version:

```
require github.com/urunc-dev/urunc v0.8.1-0.<date>-<sha>
```

There is no `replace`. The module is the upstream repository itself.

When the branch moves, refresh the pin **by commit**, not by branch name. Go
refuses `@feat/initrd-hvi-backend-v0.8.0` as a disallowed version string,
because the name ends in something that parses as one:

```bash
go get github.com/urunc-dev/urunc@<sha of the branch tip>
go mod tidy
```

A pseudo-version only lives while its commit is reachable upstream. If the
branch is deleted once it merges, move the pin to the merge commit or to the
tag that carries the work. Once the darwin work is in a tagged release, require
that tag.

For local development against a live checkout, use an uncommitted `go.work`,
which is gitignored, rather than editing `go.mod`:

```
go 1.26.4

use (
	.
	/path/to/urunc
)
```

## The app bundle and installer

`make app` wraps the three binaries into `hull.app`, with `vz-runner` and `hvi`
inside `Contents/MacOS` so sibling discovery works from `/Applications`.
`make dmg` produces the drag-to-Applications installer, signed, notarized and
stapled.

After dragging to Applications, put the CLI on your PATH:

```bash
ln -s /Applications/hull.app/Contents/MacOS/hull ~/.local/bin/hull
```

Packaging assets live in `packaging/` and are regenerated from the vendored logo
sources with `scripts/make-packaging-assets.sh`, which needs `imagemagick`.

Note one bundle subtlety. A bundle's **main** executable, `hull` here, is
sealed to the bundle's `Info.plist` and resource directory, so extracted as a
lone file its signature is invalid and AMFI kills it at exec. Nested binaries
like `vz-runner` and `hvi` are not sealed that way and survive extraction.
`make release_tarball` re-signs all three as standalone code, which `hull`
needs and the runners do not. A plain `cp` of an already-standalone binary
breaks nothing. See
[signing.md](signing.md#does-copying-a-binary-break-its-signature).

## Changelog

`CHANGELOG.md` is generated, never edited by hand:

```bash
scripts/changelog.sh          # or: git-cliff --config cliff.toml -o CHANGELOG.md
```

The release workflow does not run it, so regenerate and commit it when cutting a
release. See [releasing.md](releasing.md).

## Before you open a pull request

Read [../CONTRIBUTING.md](../CONTRIBUTING.md) for the commit and review
conventions, and [../AI_POLICY.md](../AI_POLICY.md) if AI helped.

The commit scopes in use are `compose`, `run`, `exec`, `store`, `vz-runner`,
`hvi`, `qemu`, `ci` and `docs`.
