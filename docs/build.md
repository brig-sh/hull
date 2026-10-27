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

- An Apple Silicon Mac. On Linux the CLI has only `hull assets`,
  `hull images`, `hull pull`, `hull rmi` and `hull version` so far; see
  [Building on Linux](#building-on-linux).
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

A plain `make` picks the build for the host: `macos` on a Mac, `linux`
anywhere else.

Individual targets:

| Target | What it does |
|---|---|
| `make urunc_macos` | build the Go CLI for macOS to `dist/hull_arm64` |
| `make linux` | build the Go CLI for Linux to `dist/hull_linux_<arch>` |
| `make vz_runner` | `swift build -c release` in `vz-runner/` |
| `make hvi_vmm` | `cargo build --release` in `hvi-vmm/` |
| `make sign` | sign all three and strip quarantine |
| `make codesign_verify` | print each binary's authority chain and entitlements |
| `make test` | `go test -count=1 ./...`, then the same in `pkg/telemetry` |
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

### Building on Linux

A Linux port has started, and it cannot run anything yet. `make linux`, or
plain `make` on a Linux host, builds a `hull` whose only commands are
`hull assets`, `hull images`, `hull inspect`, `hull ps`, `hull pull`,
`hull rm`, `hull rmi` and `hull version`. It builds no runners and signs
nothing.

```bash
make                        # dist/hull_linux_<host arch>
make linux LINUX_ARCH=amd64 # cross-build for another architecture
```

On Linux, hull is a containerd client, and images live in containerd.
`hull pull` fetches them, `hull images` lists them and `hull rmi` removes
them, with the same output as on macOS. All three need a running containerd, which hull does not install or
start. hull looks for its socket in this order:

1. `--containerd-address`
2. `$HULL_CONTAINERD_ADDRESS`
3. `/run/containerd/containerd.sock` when run as root, and
   `$XDG_RUNTIME_DIR/containerd/containerd.sock`, where a rootless containerd
   puts it, for any other user

There is no store directory on Linux, and no `--store-dir`. Isolation comes
from the containerd namespace instead: `--namespace`, then
`$HULL_NAMESPACE`, then `hull`. Two namespaces see none of each other's
images, and `ctr -n hull images ls` shows what hull sees.

containerd keeps one record per image name, and it may hold several
platforms of that image. The listing describes one: the host's platform if
all its layers are present, otherwise a platform whose layers are. A pull of
another platform therefore lists as that platform, even though `ctr` also
fetches the host platform's manifest along the way. Entries of an index that
are not images, such as build attestations and signatures, are never listed
as the image.

`hull pull` differs from the macOS pull in these ways:

- `--platform` defaults to the host's, `linux/amd64` or `linux/arm64`, not
  to `linux/arm64`.
- The image is stored under its full name, so `hull pull busybox` lists as
  `docker.io/library/busybox`, the name `ctr` and nerdctl use for it too.
- containerd unpacks the image into a snapshot, with overlayfs unless the
  namespace's `containerd.io/defaults/snapshotter` label names another
  snapshotter, and keeps the layers in its content store. The daemon does
  the unpacking, so its user needs permission to create the snapshot: a
  containerd run as an ordinary user, outside rootlesskit, cannot.
- The progress line counts bytes downloaded, not extracted: the daemon
  unpacks and reports nothing to hull while it does.

The pull itself runs in hull's process, not the daemon's, so it uses the
same credentials as on macOS: the Docker config and its credential helpers.
A credential helper that fails, as one needing a locked keychain does,
leaves the pull anonymous. Registries on loopback, private RFC 1918
addresses or `.local` names are tried over HTTPS and then plain HTTP, as on
macOS.

`hull rmi` takes the macOS arguments and flags and prints the same
`Removed <digest> <name>` lines, but containerd's model changes what some of
them mean:

- A name is one image. `hull rmi busybox` finds
  `docker.io/library/busybox:latest`, in any spelling `hull pull` accepts,
  and removes that name only. On macOS a tag can answer several stored
  digests, and all of them go.
- A digest, or a digest prefix, can match several names of one image. As
  `docker rmi` does for an image ID in several repositories, that is refused
  unless `--force` is given, which removes every one of them. A prefix that
  matches more than one digest is refused as ambiguous, as on macOS.
- `--platform` removes that platform's manifest, and containerd's garbage
  collector takes its config, layers and snapshot, as `docker rmi --platform`
  does. The name stays while it holds another platform. The content store
  holds a manifest once, so every name whose index lists it loses the
  platform too, and that takes `--force`. `--platform linux/arm64` matches an
  image recorded as `linux/arm64/v8`.
- The containerd containers in hull's namespace that were created from an
  image hold it, the role instances play on macOS. A running one, with a task
  that has not stopped, refuses the removal even under `--force`. A stopped
  one refuses it without `--force`. A container keeps its own snapshot, so it
  does not lose its root filesystem when its image goes.
- `hull rmi` waits for containerd's garbage collector, so the space is free
  when it returns. There is no `hull store compact` step.

`hull assets` has the macOS subcommands, output, signature check and
`provenance.json`; see [Boot assets](images.md#boot-assets). It does not use
containerd, and only the directory differs. With no store, the assets live
where brig keeps them on Linux, so a bundle either tool fetched is found by
the other:

1. `$HULL_BOOT_ASSETS`
2. `$BRIG_BOOT_ASSETS`
3. `$XDG_DATA_HOME/brig/assets`, when `XDG_DATA_HOME` is an absolute path. A
   relative one is ignored, as brig ignores it
4. `~/.local/share/brig/assets`

The namespace does not move this directory, so every namespace shares one
kernel with brig. There is no `hull run` on Linux yet, so today only brig,
through nerdctl, boots these files. Checking their signature needs cosign on
the `PATH`. On macOS the Homebrew cask installs it; on Linux nothing does, so
install it yourself, as
[Sigstore's installation page](https://docs.sigstore.dev/cosign/system_config/installation/)
describes. Without it a fetch warns that it took the assets unchecked, and
`HULL_VERIFY=require` refuses the fetch.

An instance is a containerd container in hull's namespace, named by its
container ID. There is no `hull run` on Linux yet, so nothing hull does
creates one, but `hull ps`, `hull inspect` and `hull rm` act on any container
there, including those `ctr` or nerdctl created. They print what they print
on macOS, with these differences:

- The status comes from the container's task. A container with no task is
  `stopped`: containerd cannot tell one that was never started from one whose
  task was deleted. A task that is created but not started is `starting`, a
  running one `running`, and one that exited `stopped`, with its exit code.
  Any other status containerd reports, such as `paused`, is printed under
  containerd's name. A container whose task containerd cannot read, as when
  its shim does not answer, is listed as `unreadable`, with no PID, and the
  other containers are still listed. `hull inspect` of it fails with
  containerd's error.
- CREATED, and `startTime` in `hull inspect`, is when the container was
  created. PID is the process ID containerd reports for the task. IP is
  always `-`.
- `hull inspect` prints the macOS fields, and leaves empty those containerd
  has no counterpart for, among them `imageDigest`, `cmdLine` and `logFile`.
- `hull rm` deletes the container's task, the container and its snapshot. A
  task that has not stopped is refused without `--force`. With it, the task is
  sent SIGKILL, and `hull rm` fails rather than delete anything if the task
  has not exited within 5 seconds.

`make test` runs on Linux too. The `internal/` and `pkg/` packages build and
test natively. Of `cmd/hull`, only the files with no darwin dependency compile
there; the rest carry `//go:build darwin`.

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

## What `hull version` reports

No version is stamped at link time. `hull version` and `hull --version` read
what the Go toolchain embedded from the checkout: the version it derives from
the nearest reachable tag, the commit, its time, and whether the tree was
modified. A bare `go build` reports all of it, the same as a `make` build or a
released binary.

The exception is a build in a **linked git worktree**. Its `.git` is a file
rather than a directory, which the toolchain does not read as a repository, so
it embeds no VCS data at all and the binary says:

```
hull dev (go1.26.5, darwin/arm64)
```

`make` passes git's own commit, time, nearest tag and modified flag for that
case, and the binary uses them only when the toolchain embedded nothing. So
build with `make` in a worktree if the version matters. See
`internal/buildinfo`.

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

## The telemetry module

`pkg/telemetry` is a Go module of its own,
`github.com/brig-sh/hull/pkg/telemetry`, because brig links it too. Its only
dependency is `golang.org/x/sys`, so brig does not take hull's module graph
along.

hull requires it at a placeholder version and builds it from the tree:

```
require github.com/brig-sh/hull/pkg/telemetry v0.0.0-00010101000000-000000000000
replace github.com/brig-sh/hull/pkg/telemetry => ./pkg/telemetry
```

So a change to the client and its use land in one commit. The `replace` also
means hull itself cannot be installed with `go install <module>@<version>`;
build it from a clone.

`go test ./...` at the root does not reach a nested module. `make test` runs
its tests too, and CI tests it on macOS and Linux.

brig requires the module by version. Tag a release of it as
`pkg/telemetry/vX.Y.Z`; until then brig requires a pseudo-version of a commit
on `main`.

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
