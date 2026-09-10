# Install hull

hull runs on Apple Silicon Macs only. There is no Intel build and no Linux
build of the CLI.

macOS 26 (Tahoe) is the tested platform, and the only one CI runs. The Swift
runner declares a macOS 14 floor in `vz-runner/Package.swift`, so earlier
versions can work. Nothing blocks them, and nothing tests them. If you run one,
please open an issue and say how it went.

## Homebrew, the recommended path

```bash
brew tap brig-sh/brig
brew trust --tap brig-sh/brig
brew install --cask hull
```

Homebrew 6 and later refuse to load a third-party tap until you trust it. That
is what the second command does. It records the tap in
`~/.homebrew/trust.json`, or under `$XDG_CONFIG_HOME/homebrew/` if that
variable is set.

Installing brig also installs hull, because brig depends on it:

```bash
brew install --cask brig
```

Check the result:

```bash
hull --help
```

### What the cask puts on disk

The cask installs three executables and links them into your Homebrew prefix:

| Executable | Language | Role |
|---|---|---|
| `hull` | Go | the CLI you run |
| `vz-runner` | Swift | the `vz` backend, one process per instance |
| `hvi` | Rust | the `hvi` backend, one process per instance |

hull locates `vz-runner` and `hvi` **next to its own executable**. Keep the
three together. If you move `hull` on its own, the `vz` and `hvi` backends stop
working and hull reports that it cannot find the runner.

The cask also depends on `cosign`, which hull uses to check the signature on
the boot-asset bundle it downloads. See
[images.md](images.md#signature-verification) for what that covers.

### Optional extras

```bash
brew install e2fsprogs   # for --rootfs-type block
brew install qemu        # for --hypervisor qemu
```

Neither is needed for the default `vz` backend with the default rootfs mode.

## Verify a release

Every release publishes these files:

| File | What it is |
|---|---|
| `hull-<version>-arm64.tar.gz` | the archive holding `hull`, `vz-runner` and `hvi` |
| `checksums.txt` | SHA-256 sums of the published artifacts |
| `checksums.txt.sig`, `checksums.txt.pem` | a keyless Sigstore signature over `checksums.txt` |
| `hull.dmg` | a drag-to-Applications installer, notarized and stapled |
| `hull.dmg.sig`, `hull.dmg.pem` | a keyless Sigstore signature over the DMG |
| an SBOM | generated for the archive |

The signature covers `checksums.txt`, not the archive directly. Verification
is therefore two steps: check the signature on `checksums.txt`, then check the
archive against `checksums.txt`.

Set the tag you want, then run both steps:

```bash
TAG=v0.1.0-rc27

gh release download "$TAG" --repo brig-sh/hull \
  -p 'checksums.txt*' -p 'hull-*-arm64.tar.gz'

cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature   checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/brig-sh/hull/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
```

The identity pattern is strict on purpose, and each part earns its place:

- The leading `^` anchors it. Without the anchor, a certificate naming a
  workflow in somebody else's repository satisfies the pattern.
- Every `.` is escaped. Unescaped, `github.com` also matches `githubXcom`.
- The `@refs/tags/v` tail confines it to a released tag. Without it, any branch
  or pull request in this repository satisfies the pattern.

To verify the DMG, run the same `cosign verify-blob` with `hull.dmg`,
`hull.dmg.sig` and `hull.dmg.pem`.

`hull.dmg` is also notarized and stapled, so Gatekeeper accepts it with no
network. Note what that does and does not tell you. Notarization says Apple
scanned the file. The Sigstore signature says this project's release workflow
produced it. They are different claims, and you want both.

## The DMG and the app bundle

`hull.dmg` holds `hull.app`. Drag it to Applications, then put the CLI on your
PATH:

```bash
ln -s /Applications/hull.app/Contents/MacOS/hull ~/.local/bin/hull
```

`vz-runner` and `hvi` ride inside `Contents/MacOS` next to `hull`, so sibling
discovery keeps working through the symlink.

The Homebrew cask is the path most people should take. It unpacks the archive
and needs no manual PATH work.

## Where hull keeps its state

By default, everything lives under `~/.hull/store`, on a case-sensitive APFS
volume that hull creates and mounts the first time a command needs it. Images,
boot assets, instance root filesystems, logs and checkpoints all live there.

`--store-dir` moves it, and it is a complete isolation boundary: two stores
share nothing, not even a kernel. [storage.md](storage.md) covers the
consequences, including how to reclaim the space.

## Uninstall

```bash
brew uninstall --cask hull
hull store detach     # run this before uninstalling, to unmount the volume
rm -rf ~/.hull        # deletes every image, instance and checkpoint
```

CAUTION: `rm -rf ~/.hull` destroys every instance and checkpoint in the default
store. Nothing recovers it.

## Build from source instead

See [build.md](build.md). Building needs a recursive clone, a Go toolchain, the
Xcode command line tools and a Rust toolchain. If you build without an Apple
signing identity, read [signing.md](signing.md) before your first run.
