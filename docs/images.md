# Images, boot assets and annotations

This page covers how hull gets an image, how it decides what kernel to boot,
and what is and is not signature-checked.

## Pulling

```bash
hull pull ubuntu:latest
hull pull --platform linux/amd64 ubuntu:latest
hull images
hull images --json          # full digests
```

`hull run` pulls too, so `hull pull` is only for doing it ahead of time.

### Pull policy

`--pull` takes exactly three values, and rejects anything else:

| Value | Behavior |
|---|---|
| `missing` | the default. Pull only if the image is not cached |
| `always` | pull every time |
| `never` | fail if the image is not cached |

**A cached tag is not re-resolved.** If a tag has been republished since you
pulled it, `missing` keeps serving the old digest. Use `always` to pick up the
new one.

Pulling a republished tag adds a new digest without retiring the old one, so
several image directories can answer one tag. The most recently pulled complete
entry wins, and an incomplete newer entry does not hide an older usable one.

### Removing a pulled image

```bash
hull rmi ubuntu:latest         # every stored digest that answers the tag
hull rmi sha256:c408baae42f5  # by digest, or by the prefix `hull images` prints
hull prune                     # pull leftovers, unusable and superseded entries
hull prune --all               # every image no instance refers to
hull store compact             # return the freed space to the host
```

Because a tag can answer several stored images, `hull rmi <tag>` removes all of
them; `--platform` narrows that to one platform's entries. A digest prefix that
matches more than one image is refused as ambiguous.

`hull store compact` is not optional bookkeeping. The store is a sparse image
that only grows, so removing an image inside it frees space on the store volume
and nothing on the host until the image is compacted. See
[storage.md](storage.md#disk-space-and-how-to-get-it-back).

### Tags versus digests

The image cache is keyed by manifest digest, at
`<store>/images/<manifest-digest>/`. A tag is only a way to find a digest.

`hull images --json` prints whole digests, and omits `indexDigest` and
`platform` from a record that has none rather than printing them empty. Use it
when you need to pin a build to what actually ran.

### Platform selection

`--platform` defaults to `linux/arm64` and accepts `os/arch` or
`os/arch/variant`. It selects which image variant is pulled, **and it is part
of the cache lookup key**, so an `arm64` cache entry cannot satisfy a
`--platform linux/amd64` run: that run pulls its own copy.

It is **not** the Rosetta switch. `--rosetta` turns translation on, and if you
pass it without an explicit `--platform` it defaults the pull to
`linux/amd64`. An explicit `--platform` still wins. See
[backends.md](backends.md#vz).

### Private registries

hull reads credentials the way other OCI tools do, through the Docker
credential helpers, so `docker login <registry>` is enough for a workload
image.

There is a fallback worth knowing, and it applies to **both** workload image
pulls and the boot-asset fetch. Docker's credential helper needs an unlocked
login keychain, and a headless session, a CI runner or an ssh shell, does not
have one. It then fails with a message about user interaction that is about the
helper, not the registry. Rather than failing, hull retries the pull with no
credentials at all, so a public image or bundle still works there.

`HULL_REGISTRY_TOKEN` with `read:packages` covers a genuinely private boot
bundle.

## Which kernel gets booted

hull decides in this order:

1. If the run sets `com.urunc.unikernel.bootKernel` and
   `com.urunc.unikernel.bootInitrd`, those are used. Both must be set together
   or hull errors.
2. If the image carries its own kernel, that is used.
3. Otherwise, on `vz` and `hvi`, hull uses the generic boot assets from the
   store, downloading them if they are not there.
4. On any other backend, the run fails with
   `generic virtiofs container boot requires the vz or hvi backend`.

Step 3 is what makes `hull run --hypervisor vz ubuntu:latest` work.

### How hull knows an image carries no kernel

An image with no urunc annotations is given the literal sentinel `unikernel`
for both `com.urunc.unikernel.binary` and `com.urunc.unikernel.kernel`, and
hull treats that sentinel as "no kernel of its own". So a plain container image
takes the generic path without the user configuring anything.

`--no-boot-assets` turns the fallback off, which makes an image with no kernel
fail rather than boot on a generic one.

## Annotations

Most of these are read from the image, and `--annotation KEY=VALUE` sets them
at run time without rebuilding the image. A runtime annotation wins over the
image's own.

**Two are the exception.** `com.urunc.unikernel.bootKernel` and
`com.urunc.unikernel.bootInitrd` are accepted **only** from `--annotation` on
the run, never from image metadata or `urunc.json`. They name files on the
host, and an image must not be able to nominate a host file. That is a
deliberate boundary, not an oversight.

| Annotation | What it does |
|---|---|
| `com.urunc.unikernel.hypervisor` | the default backend. Six values are accepted: `vz`, `hvi`, `qemu`, plus `qemu-hvf` folding to `qemu` and `virtualization` and `apple` both folding to `vz` |
| `com.urunc.unikernel.binary` | path to the kernel inside the image, for example `/.boot/kernel` |
| `com.urunc.unikernel.kernel` | the kernel the image carries |
| `com.urunc.unikernel.initrd` | an initrd the image carries, passed as `-initrd` |
| `com.urunc.unikernel.unikernelType` | the unikernel type, for example `linux` |
| `com.urunc.unikernel.cmdline` | a custom kernel command line |
| `com.urunc.unikernel.mountRootfs` | `true` to use the virtiofs or 9pfs rootfs mode |
| `com.urunc.unikernel.bootKernel` | a host arm64 `Image` to boot a kernel-less OCI image with. **`--annotation` only** |
| `com.urunc.unikernel.bootInitrd` | the paired host initrd. Must be set with `bootKernel`. **`--annotation` only** |
| `com.urunc.darwin.rosetta` | `true` turns on the Rosetta path. Unlike `--rosetta` it cannot change the pull platform, because the platform is decided before the pull |

Precedence, highest first: a `--annotation` on the run, then the image's own
annotations, then hull's own defaults. `--hypervisor` beats the hypervisor
annotation.

Images built by a packaging frontend such as
[Bunny](https://github.com/nubificus/bunny) store these in
`rootfs/urunc.json` with base64-encoded values.

## Boot assets

The generic boot assets are a kernel and an initrd, published as an OCI
artifact:

```
ghcr.io/nofireai/hull-assets:<os>-<arch>
```

On an Apple Silicon Mac that resolves to
`ghcr.io/nofireai/hull-assets:darwin-arm64`. The repository is public and the
pull is anonymous.

```bash
hull assets show           # where they are, and whether they are present
hull assets dir            # just the directory, for scripts
hull assets pull           # fetch ahead of time
hull assets pull --force   # fetch even if present
hull assets pull REF       # fetch a specific reference
```

The kernel file is named `Image` on arm64 and `bzImage` on amd64. The initrd is
`container-initrd`. Both live at `<store>/assets/` alongside a
`provenance.json`.

Assets live **under the store**, not at a fixed path in `$HOME`, so
`--store-dir` is a complete isolation boundary: two stores never share a
kernel, and an `assets pull` against one store cannot change what a run using
another store boots. That was a real defect once. An unrelated command in
another shell could change what a CI job booted.

### Offline preparation

```bash
hull assets pull
hull pull ubuntu:latest
# later, with no network:
hull run --hypervisor vz --pull never ubuntu:latest /bin/echo offline
```

With the store detached, `hull assets show` reports the assets as missing,
because it only reads a path and will not mount a volume to answer a question.
`hull assets pull` remounts and finds them already there.

### Overriding where assets come from

| Variable | What it does |
|---|---|
| `HULL_BOOT_ASSETS` | use this directory instead of the store's, for a local build |
| `BRIG_BOOT_ASSETS` | honored the same way. `HULL_BOOT_ASSETS` wins |
| `HULL_BOOT_ASSETS_REF` | fetch this reference instead of the platform default, to pin a version or use a mirror |
| `HULL_REGISTRY_TOKEN` | a `read:packages` token, for a private bundle or a locked keychain |
| `HULL_BOOT_ASSETS_ALLOW_FOREIGN` | `1`, or the repository being allowed, to fetch from a repository other than the published one. Without it, such a reference is refused |
| `HULL_BOOT_ASSETS_INSECURE` | `1` to fetch from a registry that resolves to a scheme other than https |
| `XDG_DATA_HOME` | consulted only with no store and off macOS. Assets then live under `$XDG_DATA_HOME/brig/assets` |

The last two exist because a kernel is more privileged than the filesystem
above it. Without `HULL_BOOT_ASSETS_INSECURE`, hull refuses a non-https
registry outright, on the grounds that the bundle would arrive unauthenticated
and unencrypted.

## Signature verification

hull verifies **one** of the artifacts in its supply chain. Which one matters,
so here they are separately.

| Artifact | Checked by hull? | How |
|---|---|---|
| the boot-asset bundle | **yes** | cosign at fetch time, then a content re-check on later boots. Controlled by `HULL_VERIFY` |
| a workload OCI image you run | **no** | `pkg/ociclient` contains no signature verification. hull pulls and runs it |
| the release tarball and `checksums.txt` | not by hull | you verify them yourself; see [install.md](install.md#verify-a-release) |
| `hull.dmg` | not by hull | notarized and stapled by Apple, and cosign-signed. You verify it yourself |

Do not assume a guarantee from a tool built on hull also holds for hull on its
own. If you need workload images verified, verify them before you run them.

### If you are driving hull from another tool

[brig](https://github.com/brig-sh/brig) is the worked example, and its own
security documentation describes the split accurately: brig makes the cosign
calls, for images it published, and hull is the runtime whose store answers a
digest. Two contract points fall out of that, and they are hull's side to fix:

- hull performs no signature check on a workload image. A caller that wants one
  must do it itself, before calling hull.
- hull exposes no way to ask **which digest its store currently holds for a
  given reference**. `hull images --json` lists every stored record with full
  digests, so a caller can scan the list, but there is no lookup by reference.
  brig documents this as the reason one of its reports is Linux-only.

Both are noted here so an integrator plans around them rather than discovering
them.

### `HULL_VERIFY`

| Value | Behavior |
|---|---|
| `warn` | the default. Boot, and print a warning when the bundle cannot be verified |
| `require` or `strict` | refuse anything not positively verified, including a host with no cosign installed |
| `off`, `none` or `0` | do not check |
| anything unrecognized | treated as `warn` |

That last row is deliberate. A typo such as `HULL_VERIFY=of` or
`HULL_VERIFY=false` must not be the thing that silently turns the check off, so
the fallback is the safe end.

The Homebrew cask depends on `cosign` for this reason. On a host without it,
`warn` boots with a warning and `require` refuses.

### What `provenance.json` is, and is not

Each fetch writes a record beside the assets holding the reference, the bundle's
manifest digest, the digest a signature was actually checked against, and the
SHA-256 and size of every file written.

`digest` and `verifiedDigest` are kept apart on purpose. "This is what we
downloaded" and "this is what we proved" are different claims, and
`verifiedDigest` is empty when nothing was checked, whether because cosign was
absent, the bundle was unsigned, or verification was off. A cached boot can say
which of the two it stands on.

Re-hashing before a boot catches a file that changed after the fetch: a
half-finished download, a stray `cp` into the asset directory, or a second
runtime writing a different bundle over this one.

**It is not tamper-proofing.** The record lives in the same user-writable
directory as the files it describes and is not itself signed, so anything that
can rewrite the kernel can rewrite `provenance.json` in the same breath and
produce a cache that verifies perfectly.

What it buys is that the attack has to be complete. A partial write, a swapped
file, or a record edited to drop the kernel's entry are all refused. The real
anchor is the signature checked at fetch time; the record carries that check
forward to later boots that reuse the download.
