# Checkpoint / restore (Vz backend)

The Vz backend can checkpoint a running microVM — pause, save the machine
state, resume — and later restore it, resuming the guest exactly where it
was instead of cold-booting. Built on Virtualization.framework's
`saveMachineStateTo` / `restoreMachineStateFrom` (macOS 14+, Apple Silicon).

## Usage

Checkpointable instances must use a **block rootfs** (see below):

```bash
hull run --rootfs-type block --hypervisor vz --name demo -- <image>

# while the instance is running: pause → save → resume (the guest keeps going)
hull checkpoint demo

# later, with the instance stopped: boot from the checkpoint instead of cold
hull restore demo            # foreground, console attached
hull restore --detach demo   # background
```

`checkpoint` returns once the snapshot is on disk (typically a few hundred
ms; the state file is roughly the guest's touched memory, not its full RAM
size). It waits up to `--timeout` seconds (default 60) for the runner to
finish, and past that fails with a message that says where to look for the
reason: the guest console, which is `hull logs` for a detached run and the
terminal for a foreground one. The VM keeps running afterwards -- checkpoint-and-continue. Restoring
rewinds the guest to the checkpoint moment; the same checkpoint can be
restored any number of times, which also makes it a fast-boot source: a
golden post-boot checkpoint restores in well under a second, skipping kernel
boot and app init entirely.

## What is captured

Artifacts live in `<store>/instances/<name>/checkpoint/`:

| file         | contents |
|--------------|----------|
| `machine-id` | persisted `VZGenericMachineIdentifier` — restore requires the identical machine identity, so every Vz instance persists one from first boot |
| `vm.vzstate` | machine + memory state (encrypted by the framework, bound to this Mac) |
| `rootfs.img` | APFS copy-on-write clone of the block rootfs at the checkpoint moment |
| `latest.json`| manifest, written last — its mtime marks checkpoint completion |

- **Block-mode rootfs** (`run --rootfs-type block`) is required: the disk is
  cloned at checkpoint (via `clonefile`, effectively free) and put back at
  restore — memory and disk rewind together, fully consistent.
- **virtiofs rootfs** (the Vz default) is refused for checkpoint/restore:
  Virtualization.framework does not rehydrate the guest's FUSE state in a
  new VMM process, so after a restore every inode the guest holds is stale
  ("Stale file handle") and the guest effectively dies. The CLI errors out
  early instead of producing a broken restore.

## Semantics and limits

- **Same Mac, same macOS build only.** The state file is encrypted and
  hardware-bound by Apple; this is suspend/resume and fast-boot, not
  migration.
- **Pause and inspect**: checkpoint, then examine the instance's rootfs (or
  the `rootfs.img` clone) from the host while the guest keeps running — or
  stop the guest and restore later to return to the exact moment.
- The memory balloon device is dropped for checkpoint-ready instances
  (Virtualization.framework refuses to save configurations containing one);
  the runner never drove the balloon, so nothing is lost.
- Instances started on the user-mode network **gateway** re-join it at
  restore: pass `--gateway-sock` to `restore`. NAT instances keep their MAC,
  so the DHCP lease (and recorded IP) carries over.
- An instance that shared a directory by descriptor (`run --shared-dir-fd`)
  is **refused by `restore`**. The descriptor did not survive the process it
  was passed to, so the path it named is no longer pinned: the directory can
  be gone, or on a volume that reuses inode numbers it can name a different
  one. Handing the guest a directory nobody vouched for is the substitution
  `--shared-dir-fd` exists to prevent, so run the instance again with a
  fresh descriptor instead of restoring it.
- `restore` takes `--detach`, `--stop-grace`, `--wait-ip` and
  `--gateway-sock`, with the same meaning as on `run`.
- Restoring requires the stored launch configuration to be identical —
  `restore` re-uses the instance's recorded command line, so this holds
  automatically. Instances created by an older hull (no state dir)
  must be re-run once before they can be checkpointed.

## e2e coverage

`test/pty-checkpoint-test.py` boots the ubuntu test image (block rootfs),
starts a tick counter in the guest shell, checkpoints mid-count, verifies
the VM resumes and keeps counting, stops it, restores, and asserts the
counter continues from the checkpointed value — proving guest memory state
survived the round trip. CI runs it as part of the PTY end-to-end matrix.
