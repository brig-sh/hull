<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/hull-lockup-on-dark.svg">
    <img alt="hull" src="assets/hull-lockup-on-light.svg" width="300">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/brig-sh/hull/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/brig-sh/hull?include_prereleases"></a>
  <a href="https://github.com/brig-sh/hull/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/brig-sh/hull/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://codecov.io/gh/brig-sh/hull"><img alt="Coverage" src="https://codecov.io/gh/brig-sh/hull/graph/badge.svg"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/brig-sh/hull"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue.svg"></a>
</p>

Run unikernels and sandboxed Linux containers on macOS with Apple Silicon,
using the same OCI images and workflows as Linux.

hull is the microVM runtime [brig](https://github.com/brig-sh/brig) drives on
macOS, and it is useful on its own: it boots an OCI image as a real VM. Three
backends do the booting: `vz` (Virtualization.framework), `hvi` (hull's own
VMM on Hypervisor.framework) and `qemu`.

Requires Apple Silicon (M1-M5). **macOS 26 (Tahoe) is the recommended
platform and the only one CI tests on**. `vz-runner` is built for macOS 14+
(`vz-runner/Package.swift`), so earlier releases may well work, and neither
the cask nor the binary blocks them. If you run one, tell us how it went.

## Install

### Homebrew

```bash
brew tap brig-sh/brig
brew trust brig-sh/brig   # brew refuses untrusted third-party taps
brew install --cask hull
```

Installing brig brings hull with it, since brig depends on it -- so
`brew install --cask brig` is the other way in.

The tap carries `Casks/hull.rb` at the current release candidate. It is
written by hand for now: the release workflow's cask publishing is set to
`skip_upload: auto`, so goreleaser takes the cask over from the first stable
tag. The cask depends on `cosign`, which checks the signature on the boot
bundle, and installs `hull`, `vz-runner` and `hvi` side by side (the CLI
discovers both runners next to its own executable). Block-rootfs mode wants
`e2fsprogs`, and the [QEMU](https://www.qemu.org/) backend is optional:

```bash
brew install e2fsprogs qemu
```

Check it works:

```bash
hull --help
```

### Verifying a release

Every release ships `checksums.txt` with a keyless Sigstore signature
(`checksums.txt.sig` and `checksums.txt.pem`), and from v0.1.0-rc12 the DMG
carries its own. Nothing was written down about how to check them, which makes
a signature decoration rather than a control -- so:

```bash
gh release download v0.1.0-rc12 --repo brig-sh/hull \
  -p 'checksums.txt*' -p 'hull-*-arm64.tar.gz'

cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/brig-sh/hull/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The identity regexp is anchored at the front and every dot is escaped. Both
matter: `github.com` unescaped matches `githubXcom`, and without the anchor a
certificate naming a workflow in somebody else's repository satisfies it. The
`@refs/tags/v` tail is what confines it to a released tag rather than any
branch or pull request in this repository.

For the DMG, the same command with `hull.dmg`, `hull.dmg.sig` and
`hull.dmg.pem`. `hull.dmg` is also notarized and stapled, so Gatekeeper checks
it with no network -- but that says Apple saw it, not that we published it,
which is what the signature is for.

## Quick start

### Using Bunny (recommended)

[Bunny](https://github.com/nubificus/bunny) is a BuildKit frontend that
automatically packages any Dockerfile as a bootable unikernel image. It
bundles a Linux kernel and the `urunit` init process alongside your container
filesystem.

```dockerfile
#syntax=harbor.nbfc.io/nubificus/bunny:latest
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y nginx
CMD ["nginx", "-g", "daemon off;"]
```

```bash
# Build and push
docker build -t ttl.sh/my-nginx:1h .
docker push ttl.sh/my-nginx:1h

# Run with Virtualization.framework
hull run --hypervisor vz --mem 512 --cpus 2 --net shared ttl.sh/my-nginx:1h

# Run with hvi
hull run --hypervisor hvi --mem 512 --cpus 2 --net shared ttl.sh/my-nginx:1h

# Run with QEMU
hull run --hypervisor qemu --mem 512 --cpus 2 --net shared ttl.sh/my-nginx:1h
```

### Instance Management

```bash
# Run detached
ID=$(hull run -d --hypervisor vz --net shared ttl.sh/my-nginx:1h)

# List instances
hull ps

# View logs
hull logs $ID
hull logs -f $ID   # follow

# Stop and remove
hull stop $ID
hull rm $ID
```

## Commands

`hull <command> --help` prints the options for any of these.

| command | what it does |
| --- | --- |
| `hull pull <image>` | pull an OCI image. `--platform` for the Rosetta path, e.g. `linux/amd64` |
| `hull run <image>` | create and run an instance. `-d` detaches and prints the id |
| `hull exec <id> <cmd…>` | run a command in a running instance. `-t` for a pty, `-u` for the guest user, `-e KEY=VALUE` or a bare `-e KEY` to inherit one from the host without putting it in argv |
| `hull ps` | list running instances |
| `hull logs <id>` | instance logs. `-f` follows |
| `hull inspect <id>` | instance details |
| `hull stop <id>` | stop a running instance |
| `hull rm <id>` | remove a stopped instance |
| `hull images` | list pulled images. `--json` prints the store's records with full manifest and index digests |
| `hull checkpoint <id>` | pause a running Vz instance, save VM and disk state, resume it |
| `hull restore <id>` | restore a stopped Vz instance from its checkpoint |
| `hull compose up\|down\|ps\|logs\|config\|exec\|top` | run a multi-service compose file, one service per VM |
| `hull assets show\|dir\|pull` | the boot assets used for images that carry no kernel of their own |
| `hull store detach` | unmount the store volume, leaving its backing image on disk |
| `hull telemetry on\|off\|status` | anonymous usage and crash telemetry |
| `hull network-gateway` | the user-mode network gateway daemon. Hidden from `--help`: compose starts one per project, and `run --gateway-sock` joins one. Flags in [docs/network-egress.md](docs/network-egress.md#the-gateway-itself) |

Global options apply to every command: `--debug`, `--store-dir` (default
`~/.hull/store`), and the telemetry pair `--unattended` and `--dnt`.

Checkpoint and restore are covered in [docs/checkpoint-restore.md](docs/checkpoint-restore.md),
compose in [docs/compose.md](docs/compose.md), what a guest behind the network
gateway is allowed to reach in
[docs/network-egress.md](docs/network-egress.md), and what telemetry does and
does not collect in [docs/telemetry.md](docs/telemetry.md).

## Supported backends

hull supports three VMM backends; `run --hypervisor` picks one, and the
image's `com.urunc.unikernel.hypervisor` annotation is the default:

| Backend | Technology | `--net shared` | Rootfs Modes | Boot Time |
|---------|-----------|---------|--------------|-----------|
| **vz** | Apple Virtualization.framework, through `vz-runner` (Swift) | VZNATNetworkDeviceAttachment | virtiofs, ext4 block | ~60 ms |
| **hvi** | Hypervisor.framework, through `hvi` (Rust, the `hvi-vmm` submodule) | hvi's own user-space stack | virtiofs (writable APFS clone), ext4 block | not measured here |
| **QEMU** | QEMU + HVF acceleration | vmnet-shared | 9pfs, ext4 block | ~73 ms |

All three sit on the Apple Hypervisor Framework and boot ARM64 Linux kernels
with near-native performance. All three can also join the user-mode network
gateway (`--gateway-sock`), which is how compose networks them.

A `compose` subcommand runs multi-service projects from a docker-compose
subset. [`docs/compose.md`](docs/compose.md) lists the keys it supports, the
places it diverges from docker, and what is out of scope.

## Building from source

Only needed to work on hull itself — to *use* it, install from the tap
above.

### Requirements

- Apple Silicon (M1/M2/M3/M4/M5). macOS 26 (Tahoe) recommended and tested; the runner targets macOS 14+
- Go 1.26+ (see `go.mod`)
- Xcode Command Line Tools (includes Swift 5.9+)

### System Configuration

The Vz backend needs the `com.apple.security.virtualization` entitlement. How
you get that entitlement honored depends on how the binary is signed:

- **Signed with a real Apple identity (recommended)** — an *Apple Development*
  or *Developer ID Application* certificate. AMFI honors the entitlement with
  **SIP enabled**; no boot-arg changes are needed. This is the supported path
  (see [Signing](#signing)). Vz **NAT networking** (`--net shared`) works
  with just this entitlement — `com.apple.vm.networking` is **not** required for
  NAT; it is only needed for *bridged* mode or QEMU's `vmnet` backend.
- **Ad-hoc signed (`codesign --sign -`)** — the entitlement is only honored when
  AMFI is disabled. This is a fallback for when you have no signing identity:

  ```bash
  # 1. Disable SIP: boot into Recovery Mode (hold Power button at startup),
  #    open Terminal, run: csrutil disable
  # 2. Set AMFI boot arg (from normal macOS):
  sudo nvram boot-args="amfi_get_out_of_my_way=1"
  # 3. Reboot
  ```

### The urunc dependency

This module consumes [urunc](https://github.com/urunc-dev/urunc) as a Go
dependency. The darwin work hull depends on -- the Vz backend, exec, console,
the GUI window flags, the HVI backend and the generic container initrd -- lives
on the upstream `feat/initrd-hvi-backend-v0.8.0` branch (the darwin commits
rebased onto the v0.8.0 release), and `go.mod` requires that branch's tip as a
pseudo-version:

```
require github.com/urunc-dev/urunc v0.8.1-0.<date>-<sha>
```

There is no `replace`: the module is the upstream repository itself, and the
comment above that line in `go.mod` says so. **When
`feat/initrd-hvi-backend-v0.8.0` moves**, refresh the pin by commit, not by
branch name -- Go refuses `@feat/initrd-hvi-backend-v0.8.0` as a disallowed
version string, because the name ends in something that parses as one:

```bash
go get github.com/urunc-dev/urunc@<sha of the branch tip>
go mod tidy
```

A pseudo-version lives only while its commit is reachable upstream. If the
branch is deleted once it merges, move the pin to the merge commit or to the
tag that carries the work.

**For local development** against a live checkout, use an uncommitted
`go.work` (gitignored) instead of editing `go.mod`:

```
go 1.26.4

use (
	.
	/path/to/urunc   # checkout on feat/initrd-hvi-backend-v0.8.0
)
```

Once the darwin work is in a tagged urunc release, require that tag.

### The app bundle and installer

`make app` wraps the three binaries into `hull.app` (urunc CNCF mark as the
app icon; vz-runner and hvi ride inside `Contents/MacOS`, so sibling discovery
works from `/Applications`). `make dmg` produces the drag-to-Applications
installer: styled background, NOFire volume icon, `/Applications` drop link,
signed + notarized + stapled. After dragging to Applications, put the CLI on
your PATH with:

```bash
ln -s /Applications/hull.app/Contents/MacOS/hull ~/.local/bin/hull
```

Packaging assets live in `packaging/` and are regenerated from the vendored
logo sources with `scripts/make-packaging-assets.sh` (needs imagemagick).

### Release signing (CI)

Regular CI (`.github/workflows/ci.yml`) lints and builds with ad-hoc
signatures, and its e2e job signs with the Developer ID so the entitlements
hold under SIP. Distributable builds come from `.github/workflows/release.yml`,
which runs on a `v*` tag (or on `workflow_dispatch` with an existing tag).
It builds, signs and notarizes `vz-runner` and `hvi` itself, then runs
[goreleaser](.goreleaser.yaml), which builds and notarizes `hull`, packs all
three into `hull-<version>-arm64.tar.gz`, writes `checksums.txt` with a
keyless cosign signature, drafts the GitHub release, and opens a cask pull
request against the tap (stable tags only; `skip_upload: auto`). A second job
builds the DMG.

The workflow reads these repository secrets:

| Secret | Content | How to produce |
|--------|---------|----------------|
| `MACOS_CERT_P12` | base64 of the Developer ID Application `.p12` (cert + private key) | `base64 -i DeveloperID.p12` — export from Keychain Access → My Certificates |
| `MACOS_CERT_PASSWORD` | password protecting the `.p12` | chosen at export time |
| `NOTARY_KEY_P8` | App Store Connect API private key, plain `.p8` contents | App Store Connect → Users and Access → Integrations → App Store Connect API |
| `NOTARY_KEY_ID` | the API key's ID | shown next to the key |
| `NOTARY_ISSUER_ID` | the issuer UUID | shown on the same page |
| `NOFIRE_BOT_PRIVATE_KEY` | private key of the GitHub App that mints the tap token | the App's settings page |
| `HOMEBREW_TAP_GITHUB_TOKEN` | fallback PAT for the tap, used when the App token is unavailable | a fine-grained PAT with contents and pull-requests write on `homebrew-brig` |

`TELEMETRY_ENDPOINT` is a repository *variable*, not a secret; empty leaves
the telemetry client inert. CI itself reads two more secrets:
`HULL_ASSETS_TOKEN` (pulls the boot bundle on the e2e runner) and
`CODECOV_TOKEN` (coverage upload on pushes to main).

Set a secret with:

```bash
gh secret set MACOS_CERT_P12 --repo brig-sh/hull < cert.p12.b64
```

hull consists of three binaries:

| Binary | Language | Description |
|--------|----------|-------------|
| `hull` | Go | CLI for pulling OCI images and orchestrating VM lifecycle |
| `vz-runner` | Swift | Virtualization.framework backend (launches VZVirtualMachine) |
| `hvi` | Rust | Hypervisor.framework VMM, built from the `hvi-vmm` submodule |

### Prerequisites

```bash
brew install go qemu e2fsprogs
```

`hvi`, the Hypervisor Virtualization Interface, is a submodule and is built by
`make macos`, so
clone with `--recursive` (or run `git submodule update --init` afterwards).
Building it needs a Rust toolchain, which `rustup` installs at the pinned
version named in `hvi-vmm/rust-toolchain.toml`.

### Build All (quick)

```bash
# Build hull + vz-runner + hvi and sign all three with your Apple signing
# identity. Find the identity with: security find-identity -v -p codesigning
make macos CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"

# Verify what got signed (authority chain + entitlements)
make codesign_verify

# Symlink into the current directory (the runners must sit next to hull)
ln -sf dist/hull_arm64 ./hull
ln -sf vz-runner/.build/arm64-apple-macosx/release/vz-runner ./vz-runner
ln -sf hvi-vmm/target/release/hvi ./hvi
```

Omit `CODESIGN_IDENTITY` to fall back to ad-hoc signing (`-`), which requires a
disabled AMFI (see [System Configuration](#system-configuration)).

<a name="signing-macos"></a>
### Signing

`make sign` (invoked by `make macos`) signs `vz-runner` with the
virtualization-only entitlements plist (`vz-runner/Entitlements-novmnet.plist`),
signs `hvi` with `com.apple.security.hypervisor` (`hvi-vmm/hvi.entitlements`),
signs `hull`, and strips the quarantine attribute from all three.

`hvi` talks to Hypervisor.framework directly rather than through
Virtualization.framework, so it needs the hypervisor entitlement and not the
virtualization one. The same rule applies to both: a real Apple identity for
the entitlement to be honored under SIP, ad-hoc only with AMFI disabled.

**Keychain prerequisite.** Signing with an Apple identity needs the full trust
chain in your keychain: your leaf certificate **plus** the *Apple Worldwide
Developer Relations* intermediate and the *Apple Root CA*. If `codesign` fails
with `unable to build chain to self-signed root` / `errSecInternalComponent`,
the intermediates are missing — install them once:

```bash
curl -fsSLO https://www.apple.com/certificateauthority/AppleWWDRCAG3.cer
curl -fsSLO https://www.apple.com/appleca/AppleIncRootCertificate.cer
security import AppleWWDRCAG3.cer -k ~/Library/Keychains/login.keychain-db
security import AppleIncRootCertificate.cer -k ~/Library/Keychains/login.keychain-db
security find-identity -v -p codesigning   # should now list a valid identity
```

**Quarantine ("Open Anyway" prompts).** If binaries were transferred via
AirDrop, a download, or file sharing, macOS tags them with
`com.apple.quarantine` and Gatekeeper prompts you to "Open Anyway". Locally
built binaries are not quarantined. `make sign` strips it; to do it by hand:

```bash
xattr -dr com.apple.quarantine ./hull ./vz-runner
```

An *Apple Development* certificate is enough for local use once quarantine is
stripped. To let **other** machines run the binaries without prompts, sign with
a **Developer ID Application** certificate and notarize (see
[Distribution](#distribution-notarization)).

Or build each component individually:

### 1. hull (Go CLI)

```bash
# Using make
make urunc_macos
# Output: dist/hull_arm64

# Or directly with go build
go build -o hull ./cmd/hull/
```

### 2. vz-runner (Swift — Virtualization.framework)

```bash
# Build release binary
cd vz-runner
swift build -c release
cd ..

# The binary is at:
# vz-runner/.build/arm64-apple-macosx/release/vz-runner
```

**Important:** vz-runner must be re-signed with entitlements every time it is
built or copied. Without the entitlement, macOS rejects the virtualization API
at runtime. Prefer `make sign` (see [Signing](#signing)); to sign by hand:

```bash
codesign --force --options runtime \
  --sign "Apple Development: Your Name (TEAMID)" \
  --entitlements vz-runner/Entitlements-novmnet.plist \
  vz-runner/.build/arm64-apple-macosx/release/vz-runner
```

The default plist (`vz-runner/Entitlements-novmnet.plist`) contains only:
- `com.apple.security.virtualization` — required to create VZVirtualMachine

This is sufficient for the Vz backend, **including NAT networking**
(`VZNATNetworkDeviceAttachment`, `--net shared`). `com.apple.vm.networking` is
**not** needed for NAT; the older `Entitlements.plist` (which adds it) is only
relevant for bridged networking.

### 3. hvi (Rust -- Hypervisor.framework)

```bash
git submodule update --init
make hvi_vmm
# The binary is at hvi-vmm/target/release/hvi; `make sign` signs it with
# hvi-vmm/hvi.entitlements
```

### 4. QEMU

Install QEMU >= 7.2 (`brew install qemu`) — nothing else. Under compose
([`docs/compose.md`](docs/compose.md)) or any `run --gateway-sock ...`
invocation, QEMU networking goes through the
user-mode gateway over a unix-socket stream netdev: **no vmnet, no
`com.apple.vm.networking` entitlement, no root, no re-signed binary**, and
HVF acceleration works out of the box (`com.apple.security.hypervisor` is
not a restricted entitlement).

The legacy vmnet path below applies **only** to standalone
`run --hypervisor qemu --net shared` without a gateway; prefer the gateway.

<details>
<summary>Legacy: re-sign QEMU for direct vmnet networking</summary>

The Homebrew QEMU binary does not have the `com.apple.vm.networking`
entitlement, so vmnet-shared networking will fail. To fix this, copy and
re-sign the binary:

```bash
# Copy QEMU binary
cp $(which qemu-system-aarch64) /usr/local/bin/qemu-system-aarch64-signed

# Create entitlements and sign
codesign --force --sign - --entitlements <(cat <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>com.apple.security.hypervisor</key><true/>
  <key>com.apple.vm.networking</key><true/>
</dict></plist>
EOF
) /usr/local/bin/qemu-system-aarch64-signed
```

Then point hull at it: `hull run --hypervisor qemu --qemu-path /usr/local/bin/qemu-system-aarch64-signed ...`.

</details>

### Installation

Place the binaries where hull can find them. `vz-runner` and `hvi` must be
in the same directory as `hull` (they are located via `os.Executable()`).
`make install` does this for `PREFIX` (default `/usr/local/bin`); by hand:

```bash
# Option A: install to /usr/local/bin
sudo cp dist/hull_arm64 /usr/local/bin/hull
sudo cp vz-runner/.build/arm64-apple-macosx/release/vz-runner /usr/local/bin/vz-runner
sudo cp hvi-vmm/target/release/hvi /usr/local/bin/hvi

# Re-sign the runners after copying (entitlements are stripped on copy)
sudo codesign --force --sign - \
  --entitlements vz-runner/Entitlements-novmnet.plist \
  /usr/local/bin/vz-runner
sudo codesign --force --sign - \
  --entitlements hvi-vmm/hvi.entitlements \
  /usr/local/bin/hvi

# Option B: run from the build directory
ln -sf dist/hull_arm64 ./hull
ln -sf vz-runner/.build/arm64-apple-macosx/release/vz-runner ./vz-runner
ln -sf hvi-vmm/target/release/hvi ./hvi
```

### Verify Installation

```bash
# Check hull
./hull --help

# Check vz-runner entitlements
codesign -d --entitlements :- ./vz-runner 2>/dev/null | grep -c virtualization
# Should print: 1

# Check QEMU (if using QEMU backend)
qemu-system-aarch64 --version
```

## VMM Backends

### Virtualization.framework (Vz)

The Vz backend uses Apple's native Virtualization.framework via a Swift helper
binary (`vz-runner`). It provides the best network throughput and tightest
macOS integration.

```bash
./hull run --hypervisor vz --mem 2048 --cpus 4 --net shared <image>
```

**How it works:**
- `hull` prepares the rootfs and kernel command line
- Launches `vz-runner` with `--kernel`, `--cmdline`, `--share` (virtiofs) or `--rootfs` (block)
- `vz-runner` creates a `VZVirtualMachine`, attaches serial console to stdin/stdout
- Guest kernel boots with `root=rootfs rootfstype=virtiofs` (virtiofs mode) or `root=/dev/vda` (block mode)

**Requirements:**
- `vz-runner` must be signed with the `com.apple.security.virtualization` entitlement (NAT networking needs nothing more; see [Signing](#signing))
- macOS 14+ is what `vz-runner` is built for; macOS 26 is what CI tests. VZLinuxBootLoader requires an ARM64 Image format kernel, not PE32+/EFI stub

#### Booting an unmodified OCI image

An ordinary image such as `ubuntu:latest` can use a host ARM64 Linux `Image`
and the generic container initrd while its unpacked rootfs is shared directly
over Vz virtiofs. Hull exports that image directory read-only; `/vz-init` in
the initrd mounts it as the lower layer of an in-guest tmpfs overlay, restores
the OCI argv/environment, and switches root without modifying the image.

```bash
./hull run --hypervisor vz --rootfs-type virtiofs \
  --annotation com.urunc.unikernel.bootKernel=/host/arm64/Image \
  --annotation com.urunc.unikernel.bootInitrd=/host/container-initrd \
  ubuntu:latest /bin/echo hello-from-vz
```

The two annotations are optional. When the image carries no kernel of its own
and the backend is `vz` or `hvi`, hull resolves them itself from
`<store-dir>/assets` (so `~/.hull/store/assets` by default), downloading the
published bundle if it is not there yet:

```bash
./hull run --hypervisor vz ubuntu:latest /bin/echo hello-from-vz
```

The bundles are published as the OCI artifact `ghcr.io/nofireai/hull-assets`,
one tag per host platform; it pulls anonymously. The repository that builds
them is not public. `hull assets show` says where they are and what is
present; `hull assets pull [REF]` fetches them ahead of time. `--no-boot-assets`
turns the fallback off, and `HULL_BOOT_ASSETS` points at a local build of the
assets repo instead. The assets live under the store so that `--store-dir` is a
complete isolation boundary: two stores never share a kernel, and a `hull assets
pull` in one shell cannot change what a run using another store boots.
`HULL_BOOT_ASSETS_REF` overrides the reference, which is
how you pin a version.

| Variable | What it does |
|----------|--------------|
| `HULL_BOOT_ASSETS` | Use this directory instead of the store's, for a local build of the assets repo |
| `HULL_BOOT_ASSETS_REF` | Fetch this reference instead of the one for this platform, to pin a version or use a mirror |
| `HULL_REGISTRY_TOKEN` | A token with `read:packages`, for a private bundle or a machine whose keychain cannot be unlocked -- a CI runner, or any headless session, where Docker's credential helper cannot prompt |
| `BRIG_BOOT_ASSETS` | Honoured like `HULL_BOOT_ASSETS` (it loses to it), so a machine that already has brig pointed somewhere does not spell the path twice |
| `XDG_DATA_HOME` | Only consulted with no store and off macOS: the assets then live under `$XDG_DATA_HOME/brig/assets` (default `~/.local/share`) |
| `HULL_VERIFY` | How the bundle's cosign signature is checked: `warn` (default; boots and prints a warning when it cannot be verified), `require` or `strict` (refuse anything not positively verified, including a host without cosign), `off`/`none`/`0`. Anything unrecognised is `warn` |
| `HULL_BOOT_ASSETS_ALLOW_FOREIGN` | `1`, or the repository being allowed, to fetch a bundle from a repository other than the published one. Without it such a reference is refused |
| `HULL_BOOT_ASSETS_INSECURE` | `1` to fetch from a registry that resolves to a scheme other than https (a local registry). Without it the fetch is refused |

Passing the annotations explicitly still wins, so nothing that already sets
them changes behaviour -- brig, in particular, keeps supplying its own.

The same generic boot path on HVI uses a persistent writable root without a
block image or a RAM-backed upper layer. Before boot, Hull replaces the
instance bundle's image-cache symlink with a same-volume APFS copy-on-write
clone, restores the source hard-link graph, and exports that private directory
read-write. Changes remain in that instance directory while the cached OCI
rootfs stays unchanged. The image store and instance store must therefore be
on the same APFS volume.

```bash
./hull run --hypervisor hvi --rootfs-type virtiofs \
  --annotation com.urunc.unikernel.bootKernel=/host/arm64/Image \
  --annotation com.urunc.unikernel.bootInitrd=/host/container-initrd \
  ubuntu:latest /bin/echo hello-from-hvi
```

### QEMU + HVF

The QEMU backend uses `qemu-system-aarch64` with `-accel hvf` for
hardware-accelerated virtualization.

```bash
./hull run --hypervisor qemu --mem 2048 --cpus 4 --net shared <image>
```

**How it works:**
- `hull` builds the QEMU command line with HVF, vmnet-shared, and 9pfs/block device
- Guest kernel boots with `root=rootfs rootfstype=9p rootflags=trans=virtio,version=9p2000.L` (9pfs mode) or `root=/dev/vda` (block mode)

**Requirements:**
- `brew install qemu` (>= 7.2). `--qemu-path` picks a specific binary;
  otherwise hull auto-detects one, preferring a signed copy.
- For networking, use the gateway (`--gateway-sock`, or anything under
  compose): a unix-socket stream netdev, no entitlement, no root. Standalone
  `--net shared` goes through vmnet, which needs root or a QEMU signed with
  `com.apple.vm.networking`; see
  [the QEMU build notes](#4-qemu) and the troubleshooting entry below.

## Rootfs Modes

### virtiofs (Vz default)

The container rootfs directory is shared into the guest via virtiofs. An
overlayfs layer with a tmpfs upper dir is mounted on top to handle writes
and work around macOS virtiofs limitations (mode-000 files).

```bash
./hull run --hypervisor vz <image>
```

### 9pfs (QEMU default)

The container rootfs directory is shared via QEMU's built-in 9p filesystem
with `security_model=none`. No overlay needed.

```bash
./hull run --hypervisor qemu <image>
```

### ext4 block device

Creates an ext4 disk image from the container rootfs using `mke2fs -d` and
attaches it as a virtio block device. This provides a native POSIX filesystem
without the limitations of virtiofs or 9pfs.

```bash
# Works with every backend
./hull run --hypervisor vz --rootfs-type block <image>
./hull run --hypervisor hvi --rootfs-type block <image>
./hull run --hypervisor qemu --rootfs-type block <image>
```

Requires: `brew install e2fsprogs`

### sudo, setuid, and who owns a file in the guest

A guest that has to become root needs the **hvi** backend or a **block**
rootfs. It will not work on `vz` with a virtiofs root, and the failure is
quiet rather than loud.

Apple's virtio-fs reports every file as owned by the uid of the guest process
that asked for it, so a setuid binary is one the caller already owns and
`setuid`-exec does nothing. The same mechanism makes ownership look
inconsistent: the answer is cached per inode, so whichever caller looked
first wins, and `ls -la /usr/bin` and `ls -la /usr/bin/sudo` can disagree
about the same file. There is no attribute channel to correct it through --
the daemon is Apple's.

The two paths that do carry real ownership:

```bash
# hvi reads the mode and uid/gid hull records at unpack time
./hull run --hypervisor hvi <image>

# ext4 carries them natively
./hull run --hypervisor vz --rootfs-type block <image>
```

hull warns at boot when an image ships a setuid binary onto a share that
cannot express one.

## CLI Reference

```
hull [global flags] <command> [flags] [args]

Global Flags:
  --debug         Enable debug logging
  --store-dir     Images, instances and boot assets (default: ~/.hull/store)
  --unattended    Skip the telemetry consent prompt
  --dnt           Record a telemetry opt-out

Commands:
  pull            Pull an OCI image
  run             Create and run a unikernel
  exec            Run a command in a running instance
  ps              List running instances
  stop            Stop a running instance
  checkpoint      Checkpoint a running Vz instance
  restore         Restore a stopped Vz instance from its checkpoint
  rm              Remove a stopped instance
  logs            View instance logs
  inspect         Inspect instance details
  images          List pulled images
  assets          Manage the boot assets for images that carry no kernel
  store           Manage the volume the store lives on
  compose         Run multi-service compose files
  telemetry       Control anonymous usage and crash telemetry
```

One more command is not in that list. `hull network-gateway` runs the
user-mode network gateway daemon and is hidden from `--help`, because compose
starts it for you, one per project. It can be run by hand to give a `run
--gateway-sock` instance a network with a policy; its flags are in
[docs/network-egress.md](docs/network-egress.md#the-gateway-itself).

### The store

`--store-dir` is where images, instances and boot assets live, and it is a
complete isolation boundary: two stores never share so much as a kernel.

It is also a **case-sensitive APFS volume**, which hull creates and mounts the
first time a command opens it. That is not decoration. A Linux package tree
contains names that differ only by case -- `xt_CONNMARK.h` and `xt_connmark.h`
sit in the same directory -- and unpacking one where they collide gives a guest
that boots and then goes nowhere. hull refuses to use a case-insensitive store
rather than let that happen, and will not mount over a directory that already
has something in it.

```bash
hull store detach          # give the mount back; the image and its contents stay
```

There is no `attach`. Anything that opens the store mounts it, so the next
command brings it back with everything still in it. One consequence worth
knowing: with the store detached, `hull assets show` reports the assets as
missing, because it only reads a path and will not mount a volume to answer a
question. `hull assets pull` remounts and finds them already there.

```bash
hull assets show           # where the assets are, and whether they are present
hull assets dir            # just the directory, for scripts
hull assets pull [REF]     # fetch them ahead of time
```

### `run` Flags

`hull run --help` is the authority; this is the same list with the defaults.

| Flag | Default | Description |
|------|---------|-------------|
| `--hypervisor` | from image annotation | `vz`, `qemu` or `hvi` |
| `--rootfs-type` | per backend | `virtiofs` (vz default), `9pfs` (QEMU default) or `block` (ext4 disk image) |
| `--mem` | 512 | Memory in MB |
| `--cpus` | 1 | Number of vCPUs |
| `--net` | none | `none` or `shared` (NAT) |
| `--detach, -d` | false | Run in background and print the instance id |
| `--name` | auto-generated | Instance name |
| `--pull` | missing | Pull policy: `missing`, `always` or `never`. A cached tag is not re-resolved, so `always` picks up a republished tag |
| `--platform` | linux/arm64 | Image platform to pull, e.g. `linux/amd64` for the Rosetta path |
| `--shared-dir` | - | Share a host directory: `/host/path:/guest/path[:ro\|rw]` (repeatable) |
| `--shared-dir-fd` | - | Share a directory the caller already holds open: `FD:/guest/path[:ro\|rw]` (repeatable). Clear `FD_CLOEXEC` on it first |
| `--env`, `-e` | - | `KEY=VALUE`, or a bare `KEY` to inherit it from the host without putting the value in argv (repeatable) |
| `--add-host` | - | Add an entry to the guest's `/etc/hosts`: `host:ip` (repeatable) |
| `--annotation` | - | Set an OCI runtime annotation: `KEY=VALUE` (repeatable) |
| `--no-boot-assets` | false | Do not fall back to the published boot assets when the image carries no kernel |
| `--qemu-path` | auto-detect, prefers a signed copy | Path to `qemu-system-aarch64` |
| `--stop-grace` | 10 | Seconds vz-runner waits for the guest to answer a stop request before forcing |
| `--wait-ip` | false | With `--detach` and NAT networking, wait for the DHCP lease and record the IP before returning |
| `--gateway-sock` | - | Join the user-mode network gateway at this control socket (vz, hvi or QEMU) |
| `--gateway-cidr` | - | Static guest CIDR on the gateway subnet, e.g. `10.87.0.10/24` (requires `--gateway-sock`) |
| `--gui` | false | Open a graphical window for the instance (vz only) |
| `--gui-title` | - | Title for the GUI window (requires `--gui`) |
| `--rosetta` | false | Run an amd64 rootfs under Rosetta translation (vz only; the kernel stays arm64). Implies `--platform linux/amd64` for the pull |

### Environment variables

The boot-asset variables are in
[Booting an unmodified OCI image](#booting-an-unmodified-oci-image). The rest:

| Variable | What it does |
|----------|--------------|
| `HULL_TERMINAL_FILTER` | `off`, `0`, `none` or `false` turns off the filter hull puts between guest output and your terminal (OSC 52 clipboard reads, DCS passthrough, cursor-position queries). Anything else leaves it on. Reach for it when the filter is in the way; do not leave it set |
| `HULL_TELEMETRY_DISABLED`, `DO_NOT_TRACK` | Either one turns telemetry off |
| `HULL_TELEMETRY_DEBUG` | Print every telemetry payload to stderr instead of sending it |
| `HULL_TELEMETRY_PRODUCT` | The product a wrapper driving hull reports events under (brig sets it); default `hull` |
| `HULL_TELEMETRY_ENDPOINT` | Override the build-time collector endpoint (tests, staging) |
| `HULL_TELEMETRY_SUPPRESS` | Internal: set on hull's own child invocations (compose self-exec, the gateway daemon) so one command counts once. Not an opt-out |
| `CI` | Counts the session as non-interactive, so the telemetry consent prompt never shows |
| `HULL_BIN`, `HULL_STORE_DIR`, `HULL_TEST_LOG_DIR` | Read by the harnesses under `test/` only, never by hull itself; see [test/README.md](test/README.md) |

[docs/telemetry.md](docs/telemetry.md) has the telemetry ones in full.

## Networking

`--net shared` gives the guest NAT networking; what serves it depends on the
backend:

| | vz | QEMU | hvi |
|---|---|---|---|
| Serviced by | Apple NAT (`VZNATNetworkDeviceAttachment`) | vmnet-shared | hvi's built-in user-space stack |
| Subnet | 192.168.64.x | 192.168.64.x | 10.0.2.x |
| Gateway | 192.168.64.1 | 192.168.64.1 | 10.0.2.2 |
| DHCP | Automatic (`ip=dhcp`) | Automatic (`ip=dhcp`) | static, set by hull |
| DNS | Copies from `/proc/net/pnp` | Copies from `/proc/net/pnp` | 10.0.2.3 |

`--gateway-sock` replaces all of that with the user-mode gateway, on every
backend: a static address on the gateway's subnet (default `10.87.0.0/24`),
DNS served by the gateway, and the egress policy in
[docs/network-egress.md](docs/network-egress.md).

Guest DNS resolution is configured automatically by the init wrapper,
which copies the kernel DHCP response from `/proc/net/pnp` to `/etc/resolv.conf`.

## Performance

Benchmarks on Mac Studio (Mac14,13), macOS 26.3.1, 2 vCPUs, 2048 MB RAM:

### Network Throughput

| Test | Vz | QEMU+HVF | Winner |
|------|-----|----------|--------|
| TCP TX (guest→host) | 25,043 Mbps | 3,057 Mbps | Vz 8.2x |
| TCP RX (host→guest) | 55,179 Mbps | 9,373 Mbps | Vz 5.9x |
| Ping latency | 0.29 ms | 0.28 ms | Tie |

### Storage (ext4 block device)

| Test | Vz | QEMU+HVF | Winner |
|------|-----|----------|--------|
| Sequential Write 1M | 15,419 MB/s | 10,631 MB/s | Vz 1.45x |
| Random Write 4K | 4,394 MB/s | 3,118 MB/s | Vz 1.41x |
| Random Read 4K | 9 MB/s | 23 MB/s | QEMU 2.5x |

### Boot Time

| Metric | Vz | QEMU+HVF |
|--------|-----|----------|
| Kernel to init | ~60 ms | ~73 ms |
| DHCP complete | ~70 ms | ~164 ms |

**Summary:** Vz is the recommended backend for most workloads — it provides
dramatically better network throughput (8x TX) and faster writes, with
sub-100ms boot times.

## Architecture

```
┌──────────────────────────────────────────────────┐
│  hull CLI (Go)                                   │
│  Pull OCI image → prepare rootfs → build cmdline │
└──────────┬───────────────┬───────────────┬───────┘
           │               │               │
     ┌─────▼─────┐   ┌──────────┐   ┌─────▼──────┐
     │ vz-runner  │   │   hvi    │   │   QEMU     │
     │  (Swift)   │   │  (Rust)  │   │ aarch64    │
     │  Vz.fwk    │   │ Hv.fwk   │   │ +HVF      │
     └─────┬──────┘   └────┬─────┘   └─────┬──────┘
           │               │               │
     ┌─────▼───────────────▼───────────────▼──────┐
     │       Apple Hypervisor Framework (HVF)      │
     └─────┬──────────────────────────────────────┘
           │
     ┌─────▼──────────────────────────────┐
     │  ARM64 Linux Kernel (6.18.0urunc)  │
     │  ├── virtiofs / 9pfs / ext4 root   │
     │  ├── .vz-init / .qemu-init / ...   │
     │  └── urunit → user process         │
     └────────────────────────────────────┘
```

### Init Wrappers

hull injects a small shell script as `init=` to set up the guest
environment before handing off to `urunit` (or the user's entrypoint):

| Wrapper | Backend | What it does |
|---------|---------|-------------|
| `/vz-init` in the generic initrd | Vz + generic OCI image | read-only virtiofs lower → tmpfs overlay → OCI metadata → switch_root → entrypoint |
| `.vz-init` | Vz + virtiofs | overlayfs (tmpfs upper) → pivot_root → devtmpfs → devpts → resolv.conf → exec urunit |
| `.qemu-init` | QEMU + 9pfs | devtmpfs → devpts → tmpfs /tmp → resolv.conf → exec urunit |
| `.block-init` | Any + ext4 | devtmpfs → devpts → tmpfs /tmp → resolv.conf → exec urunit |

### Image Annotations

Bunny-built images store configuration in `rootfs/urunc.json` with base64-encoded values:

| Annotation | Description |
|-----------|-------------|
| `com.urunc.unikernel.binary` | Path to kernel (e.g., `/.boot/kernel`) |
| `com.urunc.unikernel.hypervisor` | Default backend (`vz`, `qemu` or `hvi`) |
| `com.urunc.unikernel.unikernelType` | Unikernel type (e.g., `linux`) |
| `com.urunc.unikernel.cmdline` | Custom kernel command line |
| `com.urunc.unikernel.mountRootfs` | `true` to use virtiofs/9pfs rootfs mode |
| `com.urunc.unikernel.bootKernel` | Host ARM64 Linux `Image` used to boot an unmodified OCI image |
| `com.urunc.unikernel.bootInitrd` | Host generic initrd containing `/vz-init`; paired with `bootKernel` |

Runtime annotations can be supplied without rebuilding the image using the
repeatable `hull run --annotation KEY=VALUE` option.

The last two are filled in automatically when the image carries no kernel and
the backend can boot it generically -- see
[Booting an unmodified OCI image](#booting-an-unmodified-oci-image).

## Troubleshooting

### "The process doesn't have the com.apple.security.virtualization entitlement"

Re-sign vz-runner with the entitlements plist (with SIP enabled you must use a
real Apple signing identity, not ad-hoc — see [Signing](#signing)):

```bash
make sign CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
```

### codesign fails: "unable to build chain to self-signed root" / errSecInternalComponent

The Apple WWDR intermediate and/or Apple Root CA are missing from your keychain.
Install them once — see [Signing → Keychain prerequisite](#signing).

### App is blocked / "Open Anyway" in Privacy & Security

The binary carries a `com.apple.quarantine` attribute (set when it was
AirDropped, downloaded, or file-shared). Strip it:

```bash
xattr -dr com.apple.quarantine ./hull ./vz-runner
```

`make sign` does this automatically. Locally built binaries are never
quarantined.

### "failed to start VMM: … operation not supported by device"

Foreground `run` makes vz-runner's controlling terminal your stdin, which fails
(`ENODEV`) when stdin is not a real TTY (e.g. run from a script, pipe, or `nohup`).
Use `--detach` and read output with `hull logs`, or run from an
interactive terminal.

### "cannot create vmnet interface: general failure"

This is QEMU only — the `vmnet` framework requires the caller to be **root** or
hold `com.apple.vm.networking`. Options, best first:

1. **Use the Vz backend** (`--hypervisor vz --net shared`) — NAT works with just
   the virtualization entitlement, no root, and is faster.
2. **[socket_vmnet](https://github.com/lima-vm/socket_vmnet)** — a small root
   launchd daemon owns the interface and hands unprivileged QEMU a socket.
3. **Run QEMU as root** (`sudo`) — vmnet shared mode is allowed for root.
4. Sign QEMU with `com.apple.vm.networking` — requires Apple to grant the
   managed entitlement to your team plus a provisioning profile; not available
   with a plain Apple Development certificate.

### VZLinuxBootLoader fails with "VZErrorDomain Code=1"

The kernel is in PE32+/EFI stub format. VZLinuxBootLoader requires the
uncompressed ARM64 Image format (magic `ARMd` at offset 0x38). Bunny
images include a compatible kernel automatically.

### Kernel panics with "not syncing: VFS: Unable to mount root fs"

Check the rootfs type matches the kernel command line:
- virtiofs: needs `root=rootfs rootfstype=virtiofs` and virtiofs built into the kernel
- 9pfs: needs `root=rootfs rootfstype=9p rootflags=trans=virtio,version=9p2000.L` and 9p built into the kernel
- block: needs `root=/dev/vda rw` and ext4+virtio-blk built into the kernel

### Shell shows "can't access tty; job control turned off"

Normal — the guest serial console is not a real TTY. Interactive shells work
but job control (Ctrl-Z, `fg`, `bg`) is not available.

<a name="distribution-notarization"></a>
## Distribution (Notarization)

An *Apple Development* certificate is fine for building and running on your own
machine (once quarantine is stripped). To distribute binaries that run on
**other** Macs without Gatekeeper prompts, you must sign with a **Developer ID
Application** certificate and notarize:

```bash
# Sign with Developer ID (hardened runtime + timestamp) — same entitlements plist
make sign CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"

# Notarize (needs an app-specific password or an API key stored as a keychain profile)
xcrun notarytool store-credentials urunc-notary \
  --apple-id you@example.com --team-id TEAMID --password <app-specific-password>
ditto -c -k --keepParent ./vz-runner vz-runner.zip
xcrun notarytool submit vz-runner.zip --keychain-profile urunc-notary --wait

# Staple the ticket so it verifies offline
xcrun stapler staple ./vz-runner
```

CLI binaries (not `.app` bundles) can't have a stapled ticket embedded the same
way an app can, but notarization still registers the code with Apple so
Gatekeeper passes. `spctl -a -vv -t exec ./vz-runner` should report `accepted`
after notarization; with a plain Apple Development cert it reports `rejected`,
which is expected and harmless for local use.

### How to get a Developer ID Application certificate

Developer ID certificates require membership in the **Apple Developer Program**
($99/year) and are only issued to the **Account Holder** (or an Admin) of the
team — not available on a free account.

1. Enroll at <https://developer.apple.com/programs/> if you haven't.
2. Create the certificate, either way:
   - **Xcode:** Settings → Accounts → select your team → *Manage Certificates…*
     → **+** → **Developer ID Application**. Xcode creates the private key and
     installs the cert into your login keychain.
   - **Developer portal:** <https://developer.apple.com/account/resources/certificates/list>
     → **+** → *Developer ID Application*. Generate a Certificate Signing Request
     first via *Keychain Access → Certificate Assistant → Request a Certificate
     From a Certificate Authority* (save to disk), upload the CSR, then download
     and double-click the resulting `.cer` to import it.
3. Verify it's usable for signing:

   ```bash
   security find-identity -v -p codesigning   # look for "Developer ID Application: …"
   ```

   As with Development certs, the *Apple WWDR* intermediate and *Apple Root CA*
   must be in your keychain (see [Signing → Keychain prerequisite](#signing)).

## Cutting a release

1. Bump the `VERSION` file (the tag must match it), commit, and tag:
   `git tag v$(cat VERSION) && git push origin v$(cat VERSION)`.
2. `release.yml` runs on the tag: it signs and notarizes the runners, then
   goreleaser builds and notarizes `hull`, uploads
   `hull-<version>-arm64.tar.gz`, `checksums.txt` and its cosign signature,
   and drafts the GitHub release with notes from the PR titles. Publish the
   draft.
3. For a stable tag goreleaser opens a pull request against
   `brig-sh/homebrew-brig` with the new `Casks/hull.rb`; merge it. For a
   release candidate the cask is not published (`skip_upload: auto`), so
   bump `version` and `sha256` in the tap's `Casks/hull.rb` by hand if the
   rc should be installable.
4. `CHANGELOG.md` is not touched by the workflow. Regenerate it with
   `scripts/changelog.sh` (or `git-cliff --config cliff.toml -o
   CHANGELOG.md`) and commit it.

## Built on urunc

hull is based on [urunc](https://github.com/urunc-dev/urunc), a
[CNCF](https://www.cncf.io/) Sandbox project. urunc does the hard part -- it
runs unikernels and lightweight VMs as OCI containers -- and hull carries that
onto macOS, on top of Virtualization.framework.

<p align="center">
  <a href="https://www.cncf.io/">
    <img alt="Cloud Native Computing Foundation" src="assets/cncf-logo.svg" width="220">
  </a>
  &nbsp;&nbsp;&nbsp;
  <a href="https://github.com/urunc-dev/urunc">
    <img alt="urunc" src="assets/urunc-logo.png" width="80">
  </a>
</p>

hull is not a CNCF project and is not endorsed by the CNCF. The Linux
Foundation has registered trademarks and uses trademarks. For a list of trademarks of The Linux Foundation, please see our
[Trademark Usage page](https://www.linuxfoundation.org/trademark-usage).
urunc, CNCF and the CNCF logo are trademarks of The Linux Foundation.


## License

[Apache License 2.0](LICENSE)

---

<p align="center">
  <a href="https://nofire.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="assets/nofire-logo-on-dark.svg">
      <img alt="NOFire AI" src="assets/nofire-logo.svg" width="150">
    </picture>
  </a>
</p>

<p align="center">Powered by <a href="https://nofire.ai">NOFire AI</a></p>
