# Checkpoint / restore (Vz backend)

The Vz backend can checkpoint a running microVM, meaning pause, save the machine
state and resume, then later restore it, resuming the guest exactly where it
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

`checkpoint` returns once the snapshot is on disk. It waits up to `--timeout`
seconds (default 60) for the runner to finish, and past that fails with a
message that says where to look for the reason: the guest console, which is
`hull logs` for a detached run and the terminal for a foreground one. The VM
keeps running afterwards, so this is checkpoint-and-continue.

Restoring rewinds the guest to the checkpoint moment. Nothing in hull's restore
path consumes the saved state, so the same checkpoint can be restored more than
once, which also makes it usable as a fast-boot source from a golden post-boot
state.

No timing or size figure is recorded in this repository for either operation.
Earlier versions of this page gave numbers for how long a checkpoint takes, how
large the state file is relative to guest RAM, and how fast a restore is. None
of them had a source in the tree, so they have been removed rather than
restated. Measure on your own hardware if you need a figure.

## What is captured

Artifacts live in `<store>/instances/<name>/checkpoint/`:

| file         | contents |
|--------------|----------|
| `machine-id` | persisted `VZGenericMachineIdentifier`. Restore requires the identical machine identity, so every Vz instance persists one from first boot |
| `vm.vzstate` | machine + memory state (encrypted by the framework, bound to this Mac) |
| `rootfs.img` | APFS copy-on-write clone of the block rootfs at the checkpoint moment |
| `latest.json`| manifest, written last; its mtime marks checkpoint completion |

- **Block-mode rootfs** (`run --rootfs-type block`) is required: the disk is
  cloned at checkpoint via `clonefile` and put back at restore, so memory and
  disk are intended to rewind together.

  Two caveats belong with that, because an earlier version of this page called
  the pair "fully consistent" without qualification:

  - **A failed disk clone still counts as a successful checkpoint.** The clone
    failure is only printed to the console. The manifest then records an empty
    disk-image name, and a later restore keeps the current rootfs and says so.
    Memory rewinds and the disk does not. Read the checkpoint output rather
    than trusting the exit status alone.
  - **The disk half has no end-to-end coverage.** The harness never writes to
    or reads back the rootfs, so disk rewind rests on reading the code, not on
    a test.

  Whether Virtualization.framework has flushed every guest disk write to the
  image by the time the pause callback returns is framework behavior this
  repository does not establish, and the clone's consistency depends on it.

  The disk clone uses `clonefile`, which is an APFS feature. Nothing in hull
  checks the store's filesystem for it.
- **virtiofs rootfs** (the Vz default) is refused for checkpoint/restore:
  Virtualization.framework does not rehydrate the guest's FUSE state in a
  new VMM process, so after a restore every inode the guest holds is stale
  ("Stale file handle") and the guest effectively dies. The CLI errors out
  early instead of producing a broken restore.

## Semantics and limits

- **Treat this as suspend and resume on one machine, not migration.** hull
  records the machine identifier and requires the identical identity at
  restore, so a checkpoint is tied to the instance that made it. The stronger
  claim that the state file is encrypted and hardware-bound, and that restore
  needs the same Mac and the same macOS build, is Apple framework behavior with
  no source in this repository. Do not plan on moving a checkpoint between
  hosts, and do not treat the stronger claim as verified here.
- **Nothing here rolls back the outside world.** A restore rewinds guest memory
  and, when the clone succeeded, the guest disk. It does not undo anything the
  guest already sent out: a request it made, a row it wrote to a remote
  database, a message it published. Those effects stay.
- **No application-level consistency is promised.** The guest is paused
  wherever it happened to be. A process mid-write, a database mid-transaction
  or an open network connection is captured in that state, and the workload
  itself has to tolerate it.
- **A started guest is not a ready guest.** `restore` returning means the
  runner is up. If your script needs readiness, poll for it, for example with
  `until hull exec <id> /bin/true; do sleep 0.2; done`.
- **`hull rm` destroys the checkpoint.** It removes the whole instance
  directory, and `checkpoint/` is inside it. Nothing warns you first.
- **Pause and inspect**: checkpoint, then examine the instance's rootfs (or
  the `rootfs.img` clone) from the host while the guest keeps running, or
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
- `restore` declares `--detach`, `--stop-grace`, `--wait-ip` and
  `--gateway-sock`, but two of them do not behave as they do on `run`:

  | Option | On `restore` |
  |---|---|
  | `--detach`, `-d` | works as on `run` |
  | `--gateway-sock` | works as on `run`; this is how a gateway instance re-joins |
  | `--wait-ip` | **no effect.** `restore` tells the launcher there is no NAT networking, which is the same condition that gates the IP-discovery step off entirely |
  | `--stop-grace` | **does not reach the runner.** The grace value the runner uses comes from the command line recorded at the original `run`. This option only affects hull's own force-kill timer in a foreground restore |
- Restoring requires the stored launch configuration to be identical.
  `restore` re-uses the instance's recorded command line, so this holds
  automatically. Instances created by an older hull (no state dir)
  must be re-run once before they can be checkpointed.

## e2e coverage

`test/pty-checkpoint-test.py` boots the ubuntu test image on a block rootfs,
starts a tick counter in the guest shell, checkpoints mid-count, verifies the VM
resumes and keeps counting, stops it, restores, and asserts the counter
continues from the checkpointed value.

What that establishes is that **guest memory state** survived the round trip.
It does not cover the disk: the harness never writes to or reads back the
rootfs.

CI runs it as one step of the PTY end-to-end matrix, **on self-hosted runners
only**. A failing run uploads the console transcript, because a foreground run
writes no instance log file.

One operational note, recorded from CI rather than derived from code: machine
state saves have never succeeded on a runner without a console session, at 0
successes in 4 attempts on one machine against 15 in 16 on another, while
ordinary boots on the same machine pass. SIP state is not the discriminator,
and the reason is unresolved. CI pins the checkpoint job to a runner with a
console session.
