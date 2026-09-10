# Storage, shares and what persists

This page is the contract. If you are integrating hull into another tool, this
is the page that tells you where bytes land and how long they live.

## The store

Everything durable lives under one directory. `--store-dir` names it, and it
defaults to `~/.hull/store`.

A store is a complete isolation boundary. Two stores share nothing, not even a
kernel.

### Why it is a case-sensitive volume

Linux package trees contain names that differ only by case. One directory can
hold both `xt_CONNMARK.h` and `xt_connmark.h`. Unpacked onto the
case-insensitive default macOS home volume, those two collapse into one, and
the guest boots and then fails in a way that is hard to trace.

hull refuses to let that happen. It probes the directory by creating a
temporary file whose name ends in a lower-case letter, then checking whether
the upper-case spelling resolves to the same file. The probe removes its own
file.

If the directory is already on a case-sensitive volume, hull uses it as it is
and mounts nothing.

If it is not, hull creates a sparse disk image and mounts it there:

```
hdiutil create -type SPARSE -fs "Case-sensitive APFS" -size 200g -volname hull-store
hdiutil attach <image> -mountpoint <store-dir> -nobrowse -owners on
```

`-owners on` is not decoration. macOS mounts disk images with ownership
ignored by default, and on such a volume the kernel enforces neither uid nor
mode. Every 0600 file hull writes, including instance state and the guest
environment, would then be readable by any other local account.

After mounting, hull re-probes the result and fails with a reason if the volume
somehow came out case-insensitive anyway.

A marker file `.case-sensitive-apfs` records that hull mounted this store
itself, so later commands skip the create-and-mount path.

### Where the backing image sits

| Store | Backing image |
|---|---|
| the default `~/.hull/store` | `~/.hull/hull-store.sparseimage` |
| any other `--store-dir <path>` | `<path>.sparseimage` |

The image sits beside the store directory, not inside it. Naming each
non-default store's image after the store is what keeps two stores under one
parent, such as `_work/store-a` and `_work/store-b`, from resolving to the same
backing image and failing the second attach with `Resource busy`.

### Mounting is lazy

The volume is mounted at the moment a command actually opens the store.
`hull --help` and `hull compose config` never create or mount anything.

hull refuses to run when the store directory is case-insensitive **and already
contains files**, because mounting over it would hide them. Four filenames hull
writes itself do not count as content: `telemetry.json`, `telemetry.lock`,
`.case-sensitive-apfs` and `.DS_Store`.

### Detaching

```bash
hull store detach            # unmount; nothing is deleted
hull store detach --force    # unmount even with files still open
```

`detach` runs `hdiutil detach` and deletes nothing. The sparse image and its
contents stay, so the next command remounts the same store intact.

There is no `hull store attach`. Anything that opens the store mounts it.

`detach` does not itself open the store, so asking to detach never mounts one
first. Detaching a directory with no hull store mounted prints a message and
succeeds, so an unconditional cleanup step cannot fail a build for having
nothing to do. A directory that is not hull's own is left untouched.

### Disk space, and two things that do not reclaim it

hull mounts a store when it opens one and never unmounts it. A CI runner giving
every job its own `--store-dir` accumulates one mounted sparse image per job,
and a script pointing hull at a temporary directory cannot delete that
directory afterwards while the volume is still attached. Run
`hull store detach` in your cleanup step.

Two limits to plan around:

- **Nothing calls `hdiutil compact`.** Deleting data inside the volume does not
  shrink the sparse image on the host. The image only grows.
- **There is no `hull rmi` and no `hull prune`.** The image cache can only be
  reclaimed by removing `<store>/images/<digest>` by hand, or by deleting the
  whole store.

The 200 GB size is a nominal capacity, not an allocation. A sparse image
occupies what its contents occupy.

## Layout

```
<store>/
├── .lock                        exclusive advisory flock, serializes store writes
├── .case-sensitive-apfs         marker: hull mounted this store
├── telemetry.json               telemetry state, with telemetry.lock beside it
├── crashes/                     queued crash reports, 0600 each
├── images/<manifest-digest>/    image cache
│   ├── image.json
│   ├── oci-config.json
│   ├── rootfs/                  the unpacked image
│   └── (unpack-schema stamp)
├── assets/                      generic boot assets, shared across instances
│   ├── Image                    the kernel; bzImage on amd64
│   ├── container-initrd
│   └── provenance.json
├── volumes/<project>_<name>/     compose named volumes
├── compose/<project>.json        compose project state, plus its gateway sockets and log
└── instances/<id>/
    ├── state.json               0600
    ├── bundle/                  config.json plus the rootfs symlink or image
    ├── log                      0600, detached runs only
    ├── checkpoint/              machine-id, vm.vzstate, rootfs.img, latest.json
    └── (QMP socket, agent socket, staged boot files)
```

The store root, `images/` and `instances/` are created 0700. Instance state,
image metadata and the unpack stamp are 0600. `assets/`, `volumes/`,
`compose/` and `crashes/` are created on demand by the code that uses them.

The `.lock` file holds an exclusive advisory flock that serializes store
mutations across processes, because an in-memory mutex only covers one process.
That lock and the atomic state write were added after an instance was measured
dropping out of `hull ps` 86 times in 62,053 probes, back when `state.json` was
written in place with `O_TRUNC`.

Boot assets live under the store rather than at a fixed path in `$HOME` so that
`--store-dir` really is a boundary: an `assets pull` against one store cannot
change what a run using another store boots. `HULL_BOOT_ASSETS` and
`BRIG_BOOT_ASSETS` move them out of the store to a directory you name.

## Where the guest's root filesystem actually is

This differs per backend and rootfs mode, and it decides whether guest writes
survive.

| Mode | Root filesystem | Do guest writes to `/` persist? |
|---|---|---|
| `vz`, generic container boot | the **shared image cache directory**, exported read-only over virtiofs, with a tmpfs overlay in the guest | **No.** They live in guest RAM and vanish when the VM stops |
| `hvi`, generic container boot | a same-volume APFS copy-on-write clone of the cached image rootfs, exported read-write | **Yes**, in that instance's directory, until `hull rm` |
| `virtiofs` or `9pfs`, not generic container boot | a full `cp -c -a` APFS clone of the image rootfs inside the instance directory | **Yes**, until `hull rm` |
| `block` | a per-instance ext4 image at `<instance>/bundle/rootfs.ext4`, mode 0600 | **Yes**, until `hull rm` |

A freshly created instance holds no copy of the image at all: the bundle starts
as a `config.json` plus a `rootfs` symlink into the image cache. What replaces
that symlink is what the table above describes.

Notes on two of those rows:

- On the `vz` generic path, the shared image rootfs is never modified. The
  per-instance `/etc/hosts` is composed in memory and added to the boot initrd
  instead, and a test asserts the shared rootfs stays byte-for-byte unchanged.
- The `hvi` clone requires the image store and the instance directory on the
  same APFS volume. hull refuses the boot otherwise, and there is deliberately
  no plain-copy fallback, so the cost stays APFS clone semantics rather than a
  free-space requirement.

Block mode sizes its ext4 image at 1.5 times the measured rootfs size with a
floor of 15 GiB, so every block-mode instance asks for at least 15 GiB of
nominal filesystem capacity. Building it reads the cached rootfs and never
duplicates it: the per-instance files are injected into the finished image with
`debugfs`.

## Credentials on disk: read this before forwarding a secret

`--env KEY` without a value inherits `KEY` from hull's own environment. It does
keep the value out of `argv`: the only thing hull adds to the kernel command
line on account of the environment is the literal `URUNIT_CONFIG=/urunit.conf`.
That is the whole of what the flag buys. **It does not keep the value out of
everything observable.**

The resolved values are appended to the image's own environment list and travel
onward from there, in plaintext, to one of two files on disk:

| Path | Written | Used by |
|---|---|---|
| `/urunit.conf` inside the instance rootfs | 0600 | `vz` virtiofs, `qemu` 9pfs, and block mode, where it is injected into the ext4 image |
| `/urunc-env` inside the instance's initrd copy, one `KEY=VALUE` per line | cpio entry 0400 owned by root; the host-side staged initrd is 0600 | the generic container-boot path, which is what a plain image on `vz` or `hvi` uses |

Note which of those applies to the quick start. A plain container image takes
the generic boot path, so the sink there is `/urunc-env` in the initrd, not
`urunit.conf`.

In block mode the ext4 image is chmodded 0600 immediately after `mke2fs`
creates it, and the reason is worth stating plainly: the guest environment
inside that image is recoverable from the host with `strings`, whatever mode the
file carries *inside* the filesystem. The 0600 on the image file is the only
thing protecting it.

So a forwarded secret exists in at least three places:

- inside the guest process environment, which is the point
- in the instance's rootfs or initrd on the host, until `hull rm`
- wherever the workload itself writes or prints it

`hull inspect` does not print environment values. It does print the full
recorded monitor command line, which includes every share's host path and the
whole kernel command line.

Two details that decide whether a value is forwarded at all:

- A bare `--env KEY` whose variable is **unset** in hull's environment is
  dropped, not forwarded as empty, because an empty value would shadow whatever
  the image configured for that variable.
- A variable **set to the empty string** is a different case, and is forwarded
  as `KEY=`.

`hull exec --env` follows the same resolution rules but sends the list over the
agent socket instead of writing it to any file. It also applies the host `TERM`
as a default, which an explicit `--env TERM` beats.

The instance log is `<instance>/log` at mode 0600, created only for a detached
run. It is the guest console, so it carries whatever the workload prints, which
for an agent-style workload can include tokens it was given and contents of
files it read. A foreground run gets the terminal instead and writes no log, so
`hull logs` reports the file as not found.

Both 0600 modes depend on the store being mounted with `-owners on`. See above.

## Lifetimes

| Event | What is deleted |
|---|---|
| the guest process exits, foreground run | **Nothing.** hull marks the instance stopped and clears the recorded pid. The instance directory, its rootfs, its log and its checkpoint all remain |
| `hull ps` reconciles a dead record | **Nothing.** It rewrites the status to stopped |
| `hull stop` | **Nothing.** It signals the VMM and rewrites `state.json` |
| `hull rm <id>` | exactly `<store>/instances/<id>`, recursively: the record, the per-instance rootfs, the log, the sockets, the staged boot files and the checkpoint |
| `hull compose down` | `stop` then `rm` for each service in reverse start order, plus the gateway daemon and the project state file. Named volumes stay |
| `hull compose down --volumes` | the above, plus exactly `<store>/volumes/<project>_<name>` for each volume the compose file declares |

`hull rm` never touches the image cache, the boot assets or named volumes, so
the next run of the same image needs no re-pull. It is structurally incapable
of touching anything else in the store: it removes one directory path.

`hull rm` refuses a running instance without `--force`, and removes nothing on
refusal. With `--force`, it verifies the recorded pid really is one of hull's
VMMs before signalling it, sends `SIGKILL`, and waits up to five seconds for the
process to disappear rather than deleting a directory something may still be
writing into. Earlier versions signalled the recorded pid without checking, which
made `--force` a way to kill an arbitrary host process after a pid was recycled.

`hull rm` also clears an instance directory whose `state.json` is missing or
unparseable. That is the only way to free a name squatted by a run that died
before its record was written. A name with no directory at all is still
reported as not found.

### Compose volumes

CAUTION: `compose down --volumes` deletes a volume declared `external: true`
like any other. hull warns about and ignores per-volume declaration options, and
iterates every declared top-level volume. Do not rely on `external` to protect
data.

Two behaviors worth knowing:

- Deletion uses the exact declared names, never a prefix glob, so overlapping
  project names cannot delete each other's data.
- If the compose file the project started from cannot be reloaded, volume
  removal is **skipped with a warning** and the data stays, while the rest of
  the teardown still succeeds.

A `compose up` that fails or is interrupted tears down the instances it
created, the gateway and the project state, but never the named volume
directories it created first. Those survive a failed `up`.

A compose volume is only ever a host directory bind-mounted into the guest.
Only bind mounts and named volumes are supported; tmpfs and anonymous volumes
are rejected. A `:ro` compose volume **warns that read-only is not enforced and
is mounted read-write.**

## Shared host directories

```bash
hull run --shared-dir /host/path:/guest/path:ro  ...
hull run --shared-dir-fd 7:/guest/path:rw        ...
```

Shares are passed to the VMM as host paths. **Nothing about a share is copied
into the store**, so no store category corresponds to one.

### Sharing by file descriptor

`--shared-dir-fd FD:/guest/path[:ro|rw]` takes a descriptor the caller already
holds open. Integrators should understand what it actually does, because the
name suggests something else.

The descriptor **names** the directory; it does not serve it. hull duplicates
the caller's descriptor, marks the copy close-on-exec, converts the directory's
device and inode numbers into a macOS identity path of the form
`/.vol/<device>/<inode>`, and passes that path to the runner. The duplicated
descriptors do reach the runner as extra files, but hull never tells it which
number is which: they exist only to hold the inode alive until the machine has
started. The file server then re-opens the identity path.

hull works on a copy and never closes the caller's own descriptor, so the same
number can be named twice, and a failed parse closes only what hull took.

The descriptor must survive the exec, so clear `FD_CLOEXEC` on it first. hull
rejects the following before anything starts: a spec with no guest path, a
non-numeric descriptor, a descriptor number below 3, a relative guest path, a
descriptor naming a regular file rather than a directory, an unknown access
mode, and a number the process does not hold. That last error names
`FD_CLOEXEC` as the likely cause.

Two consequences:

- An instance whose share was named by a descriptor **cannot be restored from a
  checkpoint.** The descriptor that made the identity trustworthy died with the
  process that held it, so the caller must start a fresh instance with a fresh
  descriptor.
- `--shared-dir-fd` is a `vz` feature in practice. The identity path is a macOS
  construct, and nothing currently refuses the flag on the other backends.

Behavior differs by backend:

| | `vz` virtiofs | `hvi` virtiofs | `qemu` 9pfs |
|---|---|---|---|
| Read-only share | honored | honored | **refused** with an error |
| Linux mode, uid, gid | limited by what the host inode expresses | read from a private host xattr under `com.nofire.hvi.` | host ownership reported through `security_model=none` |
| setuid and setgid | see below | preserved | **cleared by hull on the 9pfs root** |

`hvi` is the backend to pick when Linux file modes matter. Its virtio-fs stores
mode, uid, gid and device numbers in a private host xattr rather than reading
them off the host inode, so it can express modes a plain macOS share cannot. A
read-only `hvi` share answers `EROFS` to every mutation. A writable one supports
file and directory handles, hard links, atomic renames, timestamps, xattrs, OFD
advisory locks, allocation, seek, `copy_file_range`, `statx`, `statfs`, FIFOs,
tmpfiles and Unix sockets. Device nodes are always refused.

On the `qemu` 9pfs root, hull clears every setuid and setgid bit on purpose.
`security_model=none` reports the host's ownership to the guest, so a setuid
binary such as `/usr/bin/mount` would drop the guest's init from root to
whichever uid ran hull. `sudo` does not work there. Use `--rootfs-type block`,
or `vz` or `hvi`.

hull warns at boot when an image ships a setuid binary onto a share that cannot
express one.

Each `hvi` share takes an optional trailing cache mode, `cache=auto`,
`cache=always` or `cache=none`. hull never passes one, so every share hull
creates runs on `auto`. `always` adds the guest-owned writeback cache and is
correct only when the guest is the sole writer.

## The image cache

A cached image lives at `<store>/images/<manifest-digest>/`. It counts as a hit
only when its `image.json`, its unpacked `rootfs/` and a matching unpack-schema
stamp are all present. Metadata alone is a miss, so an interrupted pull heals
itself on the next run instead of poisoning every later one. The current layout
version is 3; bumping it makes every older rootfs a miss that gets re-unpacked.

A pull commits by directory rename, not by deleting in place. Layers unpack into
`<digest>.tmp-<pid>`, any existing image is displaced to `<digest>.old-<pid>`,
and the slow recursive delete happens only after the new image is published.
Leftover staging directories are swept at the start of the next pull, never
satisfy a cache lookup, and never appear in `hull images`.

Pulling a republished tag adds a new digest without retiring the old one, so
several image directories can answer one tag. The most recently pulled complete
entry wins, and an incomplete newer entry does not hide an older usable one.

## Instance ids

An id must be a single safe path element: at most 255 characters, starting with
a letter or digit, containing only letters, digits, dot, underscore and hyphen.
The store lays every instance out as `<store>/instances/<id>`, so an id
carrying a separator or a `..` component could place a directory outside the
store.

Instance state is written to a temp file in the instance directory, synced, then
renamed over `state.json`, so a reader never sees a half-written record.

## Checkpoints

Checkpoint artifacts live in `<store>/instances/<id>/checkpoint/` as
`machine-id`, `vm.vzstate`, `rootfs.img`, `latest.json` and a failure marker.

Every `vz` instance is started with a checkpoint state directory, because that
directory holds the machine identifier a later restore needs. So
`<instance>/checkpoint/` exists on a `vz` instance that was never checkpointed.

Checkpoint and restore require a block rootfs. A virtiofs directory rootfs is
refused: Virtualization.framework does not rehydrate the guest's FUSE state in
a new VMM process, so every inode would go stale at restore.

See [checkpoint-restore.md](checkpoint-restore.md).
