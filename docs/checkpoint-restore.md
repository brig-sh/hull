# Checkpoint and restore

Two backends can save a running guest and bring it back later without a cold
boot: `vz` on macOS, and `hvi` on macOS and Linux. The verbs are the same, and
hull picks the path from the instance's record:

| | `vz` | `hvi` |
|---|---|---|
| How | vz-runner, on a signal | hvi's control socket |
| Captures | Virtualization.framework's machine state, plus an APFS clone of the disk | today a [partial bundle](#what-a-bundle-holds): RAM and device state, not the vCPU, interrupt controller or timer state a resume needs; not the disk |
| After `checkpoint` | the guest keeps running | `--then continue` (the default) or `--then fence` |
| Restore from | the latest checkpoint, any number of times | **not yet**: needs an `hvi` with a `restore` subcommand, which no `hvi` has. hull's side takes a fenced checkpoint, onto the disk the guest stopped with |
| Also | | `hull pause` and `hull resume` |
| Hosts | macOS 14+, Apple silicon | macOS (HVF) and Linux (KVM) |

Both need a block rootfs (`run --rootfs-type block`) and no shared host
directory. For `hvi`, pause, resume and checkpoint work today; restore is
built in hull and refuses until `hvi` gains `restore`, because no bundle `hvi`
writes yet can be resumed.

## Vz

The Vz backend can checkpoint a running microVM, meaning pause, save the machine
state and resume, then later restore it, resuming the guest exactly where it
was instead of cold-booting. Built on Virtualization.framework's
`saveMachineStateTo` / `restoreMachineStateFrom` (macOS 14+, Apple Silicon).

### Usage

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

### What is captured

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

### Semantics and limits

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

### e2e coverage

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

## hvi

hull drives `hvi` through the control socket hvi serves with `--control-sock`.
Every `hvi` run gets one at `<instance>/control.sock`, a machine profile at
`<instance>/profile.json` (`--profile-out`), and `--checkpoint-root
<instance>/checkpoint`, the one directory hvi's confinement lets it write a
bundle under. The record keeps the first two as `controlSock` and
`profilePath`, which `hull inspect` prints; the root is on the recorded
command line. The socket has
no authentication: anyone who can connect to it can pause or stop the VM. It
sits in the instance directory, which is 0700.

The control socket arrived in hvi after the commit hull's `hvi-vmm` submodule
pins (890f9e0), which answers these flags with `unknown boot arg` and would
refuse the boot. So before each run hull asks the `hvi` it is about to start,
with `hvi boot --checkpoint-root` and no value: that fails while parsing on
every `hvi`, and names the flag as unknown only on one without it. An `hvi`
without the flags runs as before, with no control socket: `hull stop` works,
and `pause` and `checkpoint` say that this `hvi` predates the socket. For
pause and checkpoint, put an `hvi` built from hvi's `feat/teleport` branch
(bf3b1aa or later) next to hull.

### Usage

```bash
hull run --detach --hypervisor hvi --rootfs-type block --net none --name demo alpine

hull pause demo                     # the I/O threads and every vCPU stop
hull resume demo

hull checkpoint demo                # --then continue: write a bundle, keep running
hull checkpoint --then fence demo   # write a bundle; the guest never runs again here
hull stop demo
hull restore demo                   # from the fenced bundle, then resume
hull restore --paused --detach demo # from the bundle, left paused for `hull resume`
```

`pause` and `resume` are idempotent. A paused guest's counter does not move,
and neither does anything else in it. `hull ps` shows such an instance as
STATUS `running` (its VMM is alive) and VM `paused` or `fenced`, and `hull
exec` refuses it with the command that gets it running. hull waits 5 s for each reply. hvi gives a pause 2 s in
all and asks clients for at least 3 s; a client that gives up sooner reports
"not paused" for a VM that then pauses. If a pause times out anyway, hull says
the VM may now be paused, and `hull resume` is harmless when it is not.

`hull stop` on an `hvi` instance sends `stop` on the socket first and waits up
to half of `--timeout` for hvi to end the run (it logs `guest stopped:
Requested`), then falls back to SIGTERM and SIGKILL as for any VMM. An `hvi`
from before the socket goes straight to the signals.

### Checkpoint

`hull checkpoint` needs a running `hvi` instance with a control socket, booted
from a block rootfs (`--disk`) with no shared host directory. A virtio-fs root
or share lives in host files and the guest's FUSE caches, which no bundle
holds, so those are refused by name before the VM is paused.

hull sends `{"v":1,"op":"checkpoint","dir":DIR,"then":"continue"|"fence"}`
and waits up to `--timeout` seconds (60). hvi pauses the guest, writes a
bundle into `DIR`, and then either resumes the guest or, with `fence`, keeps
it parked for good: hvi refuses a later `resume` with `fenced`, and only
`stop` ends it. A failed capture resumes the guest (a guest that was paused
stays paused) and leaves no bundle: hull removes a directory it made for one,
and empties a `--dir` you gave so the same one can be retried. With no reply
in time hull leaves everything in place, since hvi may still be writing.

One checkpoint or restore of an instance runs at a time: each takes
`<instance>/migration.lock`, and a second one is refused at once.

`DIR` is a new 0700 directory `<instance>/checkpoint/<txid>`, where the txid
is a random id hull draws, or an empty directory you name with `--dir`, which
has to be below `<instance>/checkpoint` too. hull resolves links in it and in
the root it passes at boot, so the two are compared in one spelling (on macOS
`/tmp` is a link to `/private/tmp`), and sends hvi that resolved path. An instance started without a checkpoint root
is refused. On
success hull writes `<instance>/checkpoint/latest.json`:

```json
{"dir": "...", "manifestSha256": "...", "then": "fence",
 "stats": {"pause_ms": 0, "capture_ms": 0, "ram_present_bytes": 0, "ram_total_bytes": 0},
 "savedAt": "...", "txid": "..."}
```

and sets the record's `migration` to `{"state": "checkpointed"|"fenced",
"bundleDir": "...", "txid": "...", "manifestSha256": "..."}`. The record keeps
the manifest's sha256 too, outside the directory hvi may write to, and a
restore checks the bundle's `manifest.json` against it. `stats` is what hvi reported. hull keeps
one bundle of its own per instance and removes the previous one after a new
checkpoint succeeds; a bundle placed with `--dir` is never removed by hull.

An `hvi` from before the checkpoint op answers `not_implemented`, and hull
reports that this `hvi` cannot checkpoint yet.

### What a bundle holds

A bundle (checkpoint format 1) is a directory of `manifest.json`, written
last with the size and sha256 of every other part, `profile.json`, `ram.map`
and `ram.bin` (the guest's RAM, sparse: all-zero pages take no space),
`cpu.json`, `platform.json`, `devices.json` (the virtio-blk, net and vsock
devices and the UART), `disk.json` and `session.json`, which carries hull's
txid. `hvi checkpoint-verify DIR` checks one.

**Today the bundle is partial.** The hvi that serves the op today
records only part of each vCPU's registers and none of the interrupt
controller or timer state, and `checkpoint-verify` says so: "vCPU registers,
interrupt controller and timer not captured, so no restore can resume this
bundle". The full vCPU, GIC and timer capture comes with hvi's `restore`, and until
then no bundle can be restored. The disk is not in the
bundle: hvi syncs it and records its size and write count, and the restore
reopens the same file. hvi's own
[docs/checkpoint.md](../hvi-vmm/docs/checkpoint.md) describes the format and
the clock rules once that page lands in the commit hull pins. In short: the
guest's monotonic clock continues from the captured value, so the downtime is
invisible to it, timers that expired in the meantime fire on resume, and the
wall clock is behind by the downtime until the guest corrects it. Agent
connections over vsock are reset by a restore.

### Restore

No `hvi` has a `restore` subcommand yet, so `hull restore` on an `hvi`
instance ends at the last refusal below. What follows is what hull does once
one does.

`hull restore` on an `hvi` instance starts `hvi restore` from the bundle the
record names. It refuses, with the reason:

- while the instance's VMM is alive. A fenced source stays alive until `hull
  stop`, and two VMMs must never share one disk.
- unless the checkpoint was fenced, or an earlier restore of it never ran the
  guest. The disk is not in the bundle, so it has to be the disk the guest
  stopped with. After `--then continue` the guest kept writing to it, and
  after a restored guest has run it has moved on: a guest given that disk with
  the older RAM sees a filesystem that disagrees with its own caches.
- when `latest.json` names a later checkpoint than the record. That is a
  checkpoint whose record update failed. If it was fenced, the guest never ran
  after it, and `hull restore --adopt-latest` records it and restores from it,
  once its bundle is below the checkpoint root and its `manifest.json` hashes
  to the sha256 in `latest.json`. One taken with `--then continue` cannot be
  restored, and neither can the record's older one, since the guest ran on
  after both. A `latest.json` that does not parse is refused too.
- when the bundle's `manifest.json` no longer hashes to the sha256 the record
  kept from hvi's reply.
- when the gateway the instance ran on does not answer on its socket.
- when the `hvi` the instance ran has no `restore` subcommand.

`hull rm` removes the instance directory, and the bundles hull placed are
inside it.

The `hvi restore` command line is built from the one the instance last ran.
hull drops `--kernel`, `--initramfs`, `--cmdline`, `--mem-mib`, `--cpus`,
`--fs-uid` and `--fs-gid`, because the bundle's RAM and profile fix them. It
keeps `--disk`, `--net-gateway`, `--net-mac`, `--agent-sock`, `--sandbox-id`
and `--events`, the host resources the devices attach to, and
`--checkpoint-root`, so the restored guest can be checkpointed again. A bare `--net`
becomes `--net-stub`. hull then adds `--from` and the instance's control
socket and profile paths. Any other flag is refused by name rather than
dropped.

The record goes `starting`, then `running`, as for any run, while
`migration.state` goes `restoring`, then `restored-paused` once hvi reports it
on the socket, then `restored` when hull sends `resume`. With `--paused`,
hull stops at `restored-paused` and `hull resume` starts the guest. Whatever
may run the restored guest (the restore's own resume, `hull resume`, any
`hull checkpoint`, since a failed capture resumes the guest) records
`restored` before it is sent, and a resume with no reply counts as one that
ran. The state never moves back, whichever hull process writes it last, so a
guest that may have run is never restored over again. hull waits up to
`--timeout` seconds (120) for hvi to rebuild the VM.

**The gateway.** hvi reaches the gateway by the socket path on its command
line, so a restore on the same host re-joins the gateway the instance left,
with the same MAC and so the same lease. `--gateway-sock` is for Vz only. A
restore on another host needs a gateway there with the same subnet and gateway
address, because the guest resumes with its address and routes in memory. The
planned `hull migrate` sets that up; nothing does yet.

**Other hosts.** A bundle restores only where the machine profile matches:
same hvi commit, backend, CPU identity and ID registers, counter frequency,
interrupt controller layout, RAM layout and devices. hvi checks this before it
creates anything. A Mac to Linux move is refused: the guest's counter runs at
24 MHz on Apple silicon and 54 MHz on a Raspberry Pi 5, and KVM cannot change
it.

### Verified

On this Mac (HVF) and on a Raspberry Pi 5 (KVM, seccomp on), with an alpine
guest on a block rootfs printing a counter every second, hull run with `hvi`
built from hvi's `feat/teleport` at bf3b1aa:

- `hull pause` held the counter for 5 s with every vCPU parked, a second
  `pause` succeeded, and `hull resume` restarted it. While paused, `hull ps`
  showed STATUS `running` and VM `paused`, and `hull exec` refused in under
  0.1 s; it worked after resume.
- Two `hull checkpoint` runs started together: one wrote its bundle, the
  other was refused as in progress. The sha256 of the bundle's
  `manifest.json` matched the one in `latest.json` and in the record.
- `hull checkpoint --then continue` wrote a bundle of about 57 MiB for a
  512 MiB guest in 0.3 s (Mac) and 0.9 s (Pi), `hvi checkpoint-verify`
  accepted it, and the counter kept going.
- `hull restore` on the running instance was refused.
- `hull checkpoint --then fence` wrote a second bundle, removed the first,
  froze the counter, and recorded `fenced`. `hull ps` showed VM and
  CHECKPOINT `fenced`, and `hull exec` and `hull resume` were refused.
- `hull stop` ended the run through the socket (`guest stopped: Requested`)
  in about 0.15 s with no `hvi` left running, and the record kept `fenced`.
- `hull restore` was refused, because that `hvi` has no `restore`
  subcommand yet. With the record's txid rewritten to stand in for a failed
  record update, restore pointed at `--adopt-latest`, which adopted the
  fenced checkpoint. After one byte was added to its `manifest.json`, restore
  was refused as changed since the checkpoint.

With the `hvi` hull pins (890f9e0), on both hosts: the run left the control
flags off, the guest booted and counted, `pause` and `checkpoint` said the
`hvi` predates the socket, and `exec` and `stop` worked. The hull before this
check passed the flags to that `hvi`, which exited with `unknown boot arg
"--control-sock"`.

A live restore waits for hvi's `restore`; until then the restore path is
covered by unit tests against a stub control socket.
