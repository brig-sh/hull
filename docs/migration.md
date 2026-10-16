# Moving an instance to another host

`hull migrate NAME --to USER@HOST` moves a running `hvi` instance to another
host over ssh. The guest keeps its memory, its disk, its MAC and its open
files. It does not reboot.

This page is the protocol: the steps, the states each host records, the
rules that keep the VM from running on two hosts at once, what you trust,
what can go wrong, and how to recover.

> **Status, 2026-10-14.** On macOS (HVF) a move runs end to end with the
> `hvi` on `feat/teleport` at 4ea836f, the first with a `restore`
> subcommand. That commit is not pushed yet, and the `hvi-vmm` submodule of
> this branch is still pinned at 890f9e0, which has neither the control
> flags nor `restore`: a hull built with its pinned `hvi` refuses
> `hull migrate` at preflight. The pin moves once `feat/teleport` is pushed.
> On Linux (KVM) `hvi` cannot restore a bundle yet (H5), so a move stops
> before the commit with the destination's answer "restore refused" (see
> [Verified](#verified)). Without `--allow-no-restore`, `hull migrate`
> refuses at preflight on an `hvi` with no restore, before anything is
> fenced.
>
> A `--profile portable-arm64-v1` instance moves through the commit on
> this Mac with the `hvi` at c580223, which checkpoints and restores the
> portable machine (see [The portable profile on this
> Mac](#the-portable-profile-on-this-mac)). That move ran between two
> stores through the `HULL_MIGRATE_SSH` stand-in, not real ssh. With the
> `hvi` at f4594f0 a portable move stops at the fence on the Linux pair (see
> [The portable profile on the Linux
> pair](#the-portable-profile-on-the-linux-pair)). A portable move between
> this Mac and a Pi has not run.

> **Streamed moves, 2026-10-15.** With the pinned `hvi` (c4131f5), whose
> `migrate` stream carries the disk rounds and takes the last one in the
> pause, `hull migrate` relays that stream and the destination hands it
> whole to `hvi restore --from-stream`. Between the Pis an ubuntu guest
> moved in 2.3 to 5.1 s with 79 to 124 ms of downtime, idle or writing
> 10 MiB/s to its disk, against 7.43 s for the whole bundle. A claude-code
> guest moved from this Mac to rp2 through `brig teleport` with 6.8 to
> 8.0 s of downtime on a 1.0 MiB/s link, sending 71 to 76 MiB where the
> whole bundle had sent 1.8 GB. See [Measured](#measured).
>
> **One session, 2026-10-15.** With the pin at `hvi` ba9b2d2 and a
> destination that lists `stream-session`, a move holds one ssh session
> from the stream's header to the resume and keeps two round trips of the
> stream on its way. On rp1 to rp2 with 100 ms and 10 Mbit/s of netem,
> the time from hvi's seal to the resume went from 2.9 to 3.9 s to 0.59
> and 0.60 s at bed4d7d (0.65 to 1.12 s at the hulls before it). That
> is over the target of 300 ms plus two round trips, 501 ms: about
> 0.45 s of it is the stream ahead of the manifest at the seal, in
> hvi's socket and in the window, and 0.1 s the commit's way to the
> destination. Counted from hull's read of the seal it is 0.44 and
> 0.53 s. The downtime there went from 4.6 to 4.8 s to 2.2 to 2.7 s. On
> the LAN the idle guest's downtime went from 103 to 164 ms to 47 to
> 69 ms. Under the O_DIRECT hog the source's pause no longer waits for a
> vCPU to park; the downtime there is now the disk syncs on both hosts,
> 11.0 and 28.8 s in the last two moves. One move of three in the set
> before them failed with the guest running on. See [From the seal to
> the resume](#from-the-seal-to-the-resume).
>
> **hvi 94faf8f, 2026-10-16.** The pin is at `hvi` 94faf8f. Its pause
> waits up to 10 s for the I/O threads and 2 s for the vCPUs, and a
> request on another connection waits behind it, so hull now waits 15 s
> for the reply to `pause`, `resume`, `status` and `profile`. A migrate
> that fails once its stream has begun closes the connection, which hull
> takes as hvi's failure, with hvi's refusal when it finds it. With disk
> rounds hvi no longer syncs the source's disk in the pause
> (`pause_sync_ms` 0, `disk.json` says `synced: false`). Between the Pis
> an idle guest moved in 2.20 s with 57.8 ms of downtime and back in
> 3.12 s with 67.6 ms, and the writer in 4.12 s with 90.9 ms on quiet
> disks. A destination stopped with SIGSTOP in the paused part made hvi
> give up and close the stream, and hull reported hvi's refusal within
> 20 ms of the SIGCONT, with the guest running on. See [With hvi
> 94faf8f](#with-hvi-94faf8f).

## Usage

```bash
hull migrate vm1 --to pm@rp2                       # move vm1 to rp2
hull migrate vm1 --to pm@rp2 --dry-run             # check both hosts, change nothing
hull migrate vm1 --to pm@rp2 \
    --remote-store /srv/hull --remote-hull /usr/local/bin/hull
hull migrate vm1 --txid 3f...e1                    # resume a move that stopped
hull migrate --status vm1                          # this host's record, and the other's
hull migrate --abort 3f...e1                       # end a move that has not committed
hull migrate-prepare vm1 --to pm@rp2               # send the base, kernel and image ahead of a move
hull migrate vm1 --to pm@rp2 --whole-bundle        # the whole checkpoint and disk, as before streaming
```

| Flag | Meaning |
|---|---|
| `--to USER@HOST` | the destination, as ssh names it |
| `--remote-store DIR` | the destination hull's `--store-dir`; default its own default |
| `--remote-hull PATH` | hull on the destination; default `hull` on the remote PATH |
| `--remote-hvi PATH` | hvi on the destination; default the one its hull would run |
| `--remote-gateway-sock PATH` | a network gateway on the destination for the guest's NIC |
| `--txid TXID` | resume this move from its recorded state, or start a move with this txid |
| `--dry-run` | run every check on both hosts and print the plan; write nothing anywhere |
| `--abort` | with NAME or TXID: abort the move, if it has not committed |
| `--status` | with NAME or TXID: print the move's record here and on the other host |
| `--timeout SECS` | how long hvi may take to write the fenced checkpoint; default 120 |
| `--allow-no-restore` | testing only: go on although an `hvi` on either host cannot restore |
| `--whole-bundle` | send the whole fenced checkpoint and disk even when both hosts can stream |

The instance must be an `hvi` instance on a block rootfs, with no shared
host directory, running or paused, started with a control socket. Both hosts
must be the same architecture, and for a native instance the same OS. An
instance started with `--profile portable-arm64-v1` (see
[backends.md](backends.md#machine-profiles)) may move between macOS and
Linux arm64, since its checkpoint holds the profile's machine rather than
the host's; the probe then also needs the destination's `hvi` to take
`--profile`, and refuses before the fence when it does not. That is a
proxy: `hvi` has no answer that says it can restore a portable bundle, and
one that takes `--profile` but cannot checkpoint or restore the portable
machine (f4594f0) passes the probe, so the move stops later, at the fence
or at the restore. On receive, the destination reads the bundle's own
`profile.json` and refuses the bundle, as a failed verification, when its
profile is not the one the source named; the profile it records is the
one `hvi` captured. Everything else about the host is left
to `hvi restore` on the destination, which by its specification (SPEC H4)
compares the bundle's machine profile with the host's (CPU, counter
frequency, interrupt controller, hvi build) and refuses before it creates
anything. The moves below ran between identical hosts, so that refusal has
not been seen to run.

## The steps

The source drives every step. Each one is a hidden hull verb on the
destination, run as `ssh USER@HOST hull <verb> ...`, which prints one JSON
reply on stdout and nothing else.

| # | Source state after it | What happens | Destination state after it |
|---|---|---|---|
| 1 | `preflight` | local checks; `migrate-probe` reports the destination's hull, hvi, hypervisor, free space and whether the name is free. A refusal here leaves no record on either host | (nothing) |
| 2 | `reserved` | `migrate-receive --reserve` creates the instance directory and its `migration.json` | `reserved` |
| 3 | `fenced` | `hull checkpoint --then fence` with the move's txid and epoch: the guest stops for good on this host | `reserved` |
| 4 | `transferring`, then `transferred` | the bundle and the disk go over ssh to `migrate-receive`, which checks every sha256, puts them in place and runs `hvi checkpoint-verify` | `receiving`, `received`, `verified` |
| 5 | `committing` | `migrate-prepare` runs `hull restore --paused` on the destination; only when that worked does the source record `committing` | `restored-paused` |
| 6 | `committed` | `migrate-commit --txid T --epoch E`: the destination records `running`, then resumes the guest | `running` |
| 7 | | the source marks its checkpoint stale and stops its fenced VMM | |

Either side can also end in `aborted`.

A streamed move (see [The streamed move](#the-streamed-move)) has no
separate fence step. The source goes from `reserved` to `transferring`, and
hvi fences the guest inside the transfer, once it has sent the RAM rounds.
The destination restores the guest paused while it receives, so the
streamed receive ends in `restored-paused`, and the source skips step 5.

The plan listed the transfer before the fence. In this version the fence
comes first, because the bundle a move carries is the fenced checkpoint: hvi
has no pre-copy yet, so there is nothing to send until the guest has stopped.
The streamed move below does that: it sends most of the RAM and of the
disk while the guest runs, then fences it and sends what changed.

### Epochs and txids

A move's **txid** is 128 random bits in hex. It names the move on both hosts,
the bundle directory, and the checkpoint inside the bundle (`session.json`).

Every instance has an **owner epoch** in its `state.json` (`ownerEpoch`), 0
where it was first started. A move hands over the next epoch. The source
sends it in the fence checkpoint, in the reservation and in the commit. The
destination takes it as its own when it commits, and refuses a commit for any
other epoch. Moving the instance back is a new move from the new host, with
the next epoch again.

## The rules

**At most one host runs the VM.**

1. The source fences before the destination may resume. The fence is a
   checkpoint taken with `then=fence`: hvi parks every vCPU and refuses every
   later `resume`.
2. The destination resumes only on `migrate-commit` with the move's txid and
   epoch. Before that, its instance is `incoming`: a plain `hull restore`,
   `hull resume` or `hull checkpoint` of it is refused, and the move's own
   restore is always `--paused`.
3. The source sends the commit only after it has recorded `fenced` and then
   `committing` on stable storage (temp file, fsync, rename, directory fsync).
4. The destination records `running` before it sends the resume. From then on
   it refuses an abort.
5. A source that never hears back from the commit stays fenced, in
   `committing`. Nothing resumes it automatically. Running the move again
   sends the commit again; the destination answers it the same way.
6. `hull migrate --abort` works only before the commit. After the fence it
   first asks the destination, which records `aborted` (so a commit that
   arrives later is refused), keeps that in its ledger, stops any paused VM
   it restored and removes the instance. It answers ok only when all of that
   is done. Only on that answer does the source restore the instance locally.
   With no answer, a failure, or the answer `running`, the source stays
   fenced and says why. A destination that has no record of the move answers
   `unknown_txid`: before `committing` no commit was sent, so the abort goes
   ahead; in `committing` the source stays fenced, since the commit may have
   run there. What an aborted move leaves on the destination, until the
   abort removes it, refuses every verb but `hull rm`.
7. After the commit the source's checkpoint is `migrated` (stale): it cannot
   be restored, resumed, checkpointed or moved. `hull migrate --status`
   explains.
8. The destination writes the move to its ledger,
   `<store>/migrations/<txid>.json`, before its instance record says
   `running`. `hull rm` and a later move of the instance leave the ledger,
   so the destination answers for the txid of every move that committed or
   aborted on it, whatever became of the instance.

**Every step can be run again.** A re-run of `hull migrate NAME --txid T`
starts from the recorded state. A step that finds its effect already there
succeeds: a reserve of a name this move reserved, a fence the record already
holds (or one hvi took but hull did not live to record, which is adopted
after `hvi checkpoint-verify`), a transfer the destination has verified, a
restore that is up and paused, a commit that is running. An abort run
again on an aborted move asks the destination again if it never confirmed
the release, and restores the instance if the first run recorded the abort
but did not live to restore it.

**Locks.** On each host a move takes `<instance>/migrate.lock` for each step.
The source holds it for the whole run of `hull migrate`, and the fence also
takes the checkpoint lock `<instance>/migration.lock` that `hull checkpoint`
and `hull restore` use. The move's own restore runs as a child `hull restore`,
which takes `migration.lock` itself, so the two locks nest in one order only:
`migrate.lock` first.

## Trust

- **ssh authenticates both ends.** The source runs `ssh -T -o BatchMode=yes`
  with your ssh configuration: the destination's host key is checked against
  `known_hosts`, and the destination's `authorized_keys` decides who may run
  hull there. hull adds no authentication of its own. Whoever can run hull as
  that user on the destination can receive the guest's RAM, which holds
  whatever the guest held, credentials included.
- **The stream is checked, not trusted.** The destination checks every file
  name, size and extent against fixed limits before it writes, every sha256
  against the stream's trailer and against the bundle's manifest, and the
  manifest's own sha256 against the one hvi reported at the fence. Then hvi
  verifies the bundle again.
- **The records are the authority on each host.** `migration.json` and
  `state.json` live in the 0700 instance directory, outside the checkpoint
  root hvi may write to.
- **A partition does not leave two hosts running.** If the network fails
  during the commit, the source stays fenced in `committing` and the
  destination is either `restored-paused` (paused, not running) or `running`.
  Both are visible with `hull migrate --status` and the hidden
  `migrate-status` verb. Either the commit is sent again, or the destination
  confirms the abort with its record of this move, aborted. That stays true
  after the destination moved the instance on or removed it, because its
  ledger keeps the move. A destination with no record at all (its store lost
  or replaced) leaves the source fenced; the operator decides, see
  [Operator recovery](#operator-recovery).
- **Operators can still break it.** Deleting a `migration.json` by hand, or
  running hvi directly on a bundle, is outside these rules.

## Network and sockets

The destination gets its own `agent.sock` and `control.sock` in its instance
directory, its own checkpoint root, and its copy of the disk at
`<instance>/bundle/rootfs.ext4`. The MAC is kept.

A guest that ran on a network gateway needs a gateway on the destination
with the same subnet, so that its address, its routes and its DNS stay valid.
Start one there (`hull network-gateway`, or brig) and pass its socket with
`--remote-gateway-sock`; the probe dials it before anything is fenced. Without
it, the moved guest gets `hvi`'s stub NIC, which reaches nothing, and the
source prints a warning. SPEC H4 says `hvi restore` refuses a net mode that
differs from the bundle's, so this fallback will likely end in "restore
refused" once hvi has restore; the move can then be aborted. Neither gateway
case has been run live.

Agent connections over vsock do not survive a move: the guest sees them reset
on restore. External connections may reconnect, since the guest keeps its
address only if the new gateway gives it the same one.

## The stream

`migrate-receive` reads a hull-specific stream on stdin. Go's `archive/tar`
cannot write sparse entries, and GNU tar and bsdtar do sparse files
differently, so hull frames its own:

```
"HULLMIG1"
'H' u32 len  {"txid", "manifestSha256"}
'F' u32 len  {"name": "bundle/<file>", "size"}  then size bytes      (each bundle file)
'S' u32 len  {"name": "disk", "size"}            then extents:        (the disk)
             u64 offset, u64 length, length bytes ... u64 0, u64 0
'T' u32 len  {"files": [{"name", "size", "sha256", "dataBytes"}]}
```

Integers are big-endian. The disk's extents are what `SEEK_DATA` and
`SEEK_HOLE` find (on APFS and ext4), less every 64 KiB chunk that is all
zeros. The receiver sizes the file first and writes only the extents, so the
holes stay holes. The disk's sha256 is over the extent records as sent
(offset, length and bytes, then the terminator), not over the image with its
holes read as zeros: both ends compute it without reading the holes, which
on a mostly empty image is most of it. It is recorded in `migration.json` on
both hosts. A stream with no trailer, a trailer that disagrees, or bytes after
it is refused.

The source checks the disk's size and modification time against the ones it
recorded at the fence before and after it sends, and refuses a disk that
changed: something wrote it while the guest was fenced. It records the
disk's sha256 as sent before it reads the destination's reply. A re-run that
finds the destination already verified (the reply was lost) checks the size
and modification time again and compares the destination's sha256 with the
source's own, hashing the disk again if hull did not live to record it. It
never takes the destination's sha256 as its own. The source also checks
each bundle file against the manifest as it reads it.

`migration.json` records the stream's bytes, its duration and its rate.

## The streamed move

A move streams when both hosts can, and otherwise takes the whole-bundle
path above and says why on its second line (`Transport: ...`). It needs:

- an `hvi` here of checkpoint format 3 (the `checkpoint_format` in the
  instance's machine profile), whose `migrate` stream carries the disk
  rounds, and that was booted against the instance's base: its profile's
  `disk_base_sha256` is the base `state.json` records (see
  [Bases](#bases));
- a hull there whose probe lists the capability `stream-v3`, and an `hvi`
  there that takes `restore --from-stream` and `restore
  --disk-base-sha256`;
- the instance's base in this store. The destination need not hold it
  yet: the move sends it before the guest stops.

A destination whose probe also lists `stream-session` takes the move in
one session, from the stream's header to the resume (see
[One session](#one-session)); with one that does not, the source polls
for the header and sends the commit as its own verb, as before.

`--whole-bundle` asks for the old path. A hull from before streamed moves
lists no capabilities, so the two still move instances the old way. A hull
that lists only `stream-v1` or `stream-v2` (whose streams carried disk
blocks hull read itself, beside hvi's RAM rounds) gets the whole bundle,
and so does this hull from one of them: neither reads the other's stream.
An instance started by an older hull, whose `hvi` was not given its base,
moves as a whole bundle until it runs again under this hull; the
`Transport:` line says so. So does one whose `hull run` could not boot
`hvi` against the base (an `hvi` without `--disk-written`, or a failed
comparison of the disk with the base): `hull run` warns, records the
reason on the instance's base (`hviWhy`), and the `Transport:` line
gives it.

### Bases

A base is the ext4 image `hull run` builds from an OCI image's rootfs, with
the ownership and modes the image records and nothing of any one
instance: no `/urunit.conf` (which carries the guest's environment and
credentials), no init wrapper, no `/etc/hosts` entries. For an `hvi`
instance on a block rootfs, `hull run` builds it once per image digest and
size on a host, keeps it read-only under `<store>/bases/<digest>.ext4`, and
records which base an image has in `<store>/bases/images/<key>.json`. Every
instance of the image then starts from a copy of that base (APFS
`clonefile` on macOS, `FICLONE` on Linux, else a copy that keeps the holes)
with its own files written in by `debugfs`, and `state.json` records the
base (`base`). A local image, which has no content digest, gets a base of
its own at every run.

So the instances of one image on a host share one base, and a destination
that holds it takes a move of any of them by the blocks that differ.
`mke2fs` writes a fresh UUID, hash seed and timestamps into every image,
so two hosts build two bases of one image: a base goes to another host
once per image, host it was built on and destination, and moves back of
instances born on the source need no base at all.

`hull run` boots `hvi` against the base: `--disk-base-sha256 <digest>`,
and `--disk-written <instance>/disk-written.bitmap`, the bitmap of the
64 KiB blocks where the instance's disk differs from the base once its own
files are written in. hvi starts its blocks written since boot from that
bitmap, so they are every block that differs from the base, and its
`migrate` stream sends them as its disk rounds. hull finds them by reading
each data block of the disk once and comparing it with the base's, then
the base's data blocks that the disk holds as holes: 0.63 s for the
1.6 GB claude-code disk on this Mac. A restore drops both flags; the
restored hvi carries the blocks written since boot on from `disk.json`, so
a second move from the destination still sends every block that differs
from the base. An `hvi` that does not know `--disk-written` runs without
either flag, and a move of the instance sends the whole bundle.

A base is kept while something on the host needs it: an instance or a
move record that names it, an image still in the store whose base it is,
or a `migrate-prepare` that received it ahead of a move, or a streamed
move that committed away from this host, for the way back (both
`bases/received/<digest>`). `hull rm` removes the instance's base when
nothing else needs it. `hull prune` removes every base nothing needs, the
entries of images gone from the store, bases set aside as corrupt,
half-written ones left by a run or a prepare that died, and in
`bases/spare` the half-written copy of a spare's maker that died (older
than an hour) and the spare and lock of a base no longer held; `hull prune
--bases` also removes the bases kept for images and received ahead of a
move when no instance or move uses them. A base younger than ten minutes
is left alone, since a run may be copying it into a disk it has not
recorded yet.

The disk digest is the sha256 of `HULLDISK1\0`, the image's size and the
block size (64 KiB, big-endian u64 each), then, for every 64 KiB block on
an absolute boundary that holds a byte that is not zero, the block's index
(u64) and its bytes; the last block may be short. A hole and a block of
written zeros hash alike, so a clone, a sparse copy and a received copy of
one image have one digest. It is not the image's plain sha256, which would
have to read every hole. hvi does not read the disk to check the value of
`--disk-base-sha256`; it records it in the machine profile, and a restore
given `--disk-base-sha256` refuses a stream whose profile names another.
From hull it is this digest.

### Preparing a destination

```bash
hull migrate-prepare vm1 --to pm@rp2 [--remote-store DIR] [--remote-hull PATH] [--json]
```

puts on the destination, while the guest keeps running, what a streamed
move then does not carry:

| Step | What it does |
|---|---|
| image | `hull pull REF` there by the reference this store pulled it by, unless that store has the digest. The move does not need it; a later `hull run` there does. A failure is reported and fails nothing |
| profile kernel | for a `--profile` instance, this store's kernel of that profile, when the destination has none. It is checked against the sha256 and installed through the same import as `HULL_PORTABLE_KERNEL`, from a copy kept under `bases/kernels/`. A destination that holds another kernel of the profile keeps it, and the step says so: a profile run there boots only its installed kernel, and whoever installed it expects those bytes. The move does not need the kernel |
| base | the base, as lz4-compressed non-zero blocks (`HULLBAS1`); the destination writes it to a temporary file, keeps it only if what arrived has the base's digest, and makes it read-only. It also keeps a spare copy (`bases/spare/<digest>.ext4`), made again by every prepare after a move took it, and by the source of a move once it commits ([The way back](#the-way-back)), which a streamed receive takes as the instance's disk: on the Pis, without clones, copying the 1.6 GB claude-code base took 22 s, all of it before the stream could start. The copy is synced before it is kept, and two makers of one spare take turns on its lock (`bases/spare/<digest>.lock`), so the second finds it made; a streamed receive waits on that lock for a maker at work, for at most a minute, and takes its spare |
| gateway | with `--remote-gateway-sock`, whether it answers. hull does not start one: brig starts the gateway with the session's policy |

Each step finds its effect already there and sends nothing, so the command
can run any number of times; the destination's probe lists the bases and
the profile kernels it holds. On rp2 the first run took 5.9 s (the image
pull 3.7 s, the 53 MB kernel 0.8 s, the base 110.8 MiB of data as 49.2 MiB
in 1.1 s) and the second 0.24 s, every step `held`.

The destination's own step of a move keeps the name: `migrate-prepare
--txid T` still brings a received checkpoint up paused. The prepare verbs
it runs there are hidden: `migrate-take-image`, `migrate-take-kernel`,
`migrate-take-base` and `migrate-spare-base`.

#### The way back

A move from A to B leaves A as the likeliest destination of the next
move: the way back. A has the base already, but a receive there finds
no spare and copies the base first, and a base A built itself was never
checked, so the receive hashes it while the stream arrives. The move
waits for that hash before it is verified, with the guest paused when
the hash outlasts the stream. The source prepares the way back itself. Once a streamed move commits, the
source's `hull migrate` starts `hull migrate-spare-base --sha256 <digest>
--for NAME` and does not wait for it:

- It runs in a session and process group of its own, with its output
  appended to the instance's log (`<store>/instances/NAME/log`), and the
  move's output does not wait for it. The hangup at the end of the ssh
  session that drove the move, or a ^C to the brig that did, does not
  reach it. A kill of the whole cgroup does: with systemd-logind's
  `KillUserProcesses=yes` the end of the ssh session's scope ends it, and
  so does the stop of a systemd unit with `KillMode=control-group` that
  ran the move. A move back then finds no spare and copies the base, as
  with no step at all. hull does not start the step under `systemd-run`:
  a user scope ends with the user's manager, which `KillUserProcesses=yes`
  stops with the last session, and a system unit needs root. Both Pis
  have `KillUserProcesses=no`.
- It makes the spare unless there is one, then checks the base and
  records the check beside it (`<digest>.ext4.checked`), unless a check
  stands. It holds the spare's lock (`bases/spare/<digest>.lock`) through
  both. Each step writes a dated line to the log (`hull: making a
  spare copy of base 83d3e4711f0a for a move back of hb2 (pid 979024)`,
  `made a spare copy ... (copy, 0.4 s)`, `checked base ... (0.1 s)`),
  then the step's JSON reply.
- With the spare there and the check on record, the move starts nothing
  and says `move: base ... has a spare copy here for a move back`. With
  the base gone from the store it says so, and starts nothing.
- The base is marked as kept for a move, as a prepare's is
  (`bases/received/<digest>`). The move back must first `hull rm NAME`
  on A, the stale copy (see [Operator recovery](#operator-recovery));
  that leaves the base and the spare. `hull prune --bases` removes both
  when nothing else uses the base.

- A move back that arrives while the step is at work waits for it, at
  most a minute, before it accepts the stream, so before hvi's migrate
  fences the guest. It then takes the spare and finds the check on
  record, and the destination's steps start with `waited for the base's
  spare being made`. A step that holds the lock past the minute is not
  waited for: the receive copies the base and hashes it while the stream
  arrives, as with no step, and records `gave up waiting for the base's
  spare being made`. Two checks of one base also take turns, on a lock of
  the base file, so the second finds the first's on record.

So a streamed move back from B to A needs no `migrate-prepare` while A
holds the base, from the commit on: a move back that comes before the
step has finished waits for it outside the pause. A `hull prune --bases`
on A, or a move that went by bundle, leaves A without the spare, and a
prepare makes it again. The move to B took B's spare, the one its
prepare made, and B makes the next one itself once the move back
commits, so a later move to B again needs no prepare either. The
destination's record says which way the receive made the disk: the step
`disk made from the base's spare`, or `disk made from the base by copy,
with no spare` (or `by clone`), after `waited for the base's spare being
made` when it waited for a maker.

### The order on the wire

One ssh session runs `hull migrate-receive --stream --txid T --name N`
(with `--session` when the destination takes it), and its stdin carries
hull's header, then hvi's `migrate` stream relayed frame by frame as hvi
writes it, then hull's trailer and, in a session, the commit. hull drives no disk round
of its own: hvi's stream holds them (hvi
[checkpoint.md](../hvi-vmm/docs/checkpoint.md), "Live moves"):

1. `profile.json`.
2. While the guest runs, for each round `k`: `disk-round-k`, the blocks
   written since the disk round before (round 0: since boot, which with
   `--disk-written` is every block that differs from the base), then
   `ram-round-k`. The rounds stop when the RAM written during the last one
   is small (`converged`), after 5 rounds (`round_cap`) or 60 s
   (`time_cap`).
3. hvi syncs the guest's disk with the guest still running, then pauses it.
   Since 65a929f it does not sync it again in the pause when the stream
   carries disk rounds: the destination holds every block and syncs its
   own copy, so `pause_sync_ms` is 0 and `disk.json` says `synced: false`.
   An `--abort` that takes the guest back restores the spool, sealed and
   synced first, onto the source's disk, whose rounds rewrite every block
   written since the boot.
4. With the guest paused: `disk-round-final`, the blocks written since the
   last disk round, then the last RAM round, the machine's documents
   (`cpu.json`, `platform.json`, `devices.json`, `disk.json`,
   `session.json`) and `manifest.json`, which lists every component with
   its size and sha256, disk rounds included. hvi fences the guest once the
   manifest is written to its socket, and its reply line follows.

So the last disk round is taken inside the pause, after the sync, and holds
only what the guest wrote during the last running round, and its bytes are
covered by hvi's manifest like everything else. A stream-v2 move read its
last disk round after hvi's seal, with every block written during the RAM
rounds in it: 30 to 64 MiB, and up to 1.41 s of the downtime, for a guest
rewriting 10 MiB/s.

### The format

```
"HULLMIG3"
'H' u32 len  {"txid", "name", "epoch", "baseSha256", "diskSize", "replay"?, "attempt"?}
'C' u32 len  hvi's frame header line, as hvi wrote it, then its bytes
'T' u32 len  {"records", "chain", "hviReply"}
'K' u32 len  {"txid", "epoch"}    in a session only, after the trailer
```

Integers are big-endian. An hvi frame is relayed with hvi's own header
line, checked by the source as it reads it from hvi and by the destination
as it passes it on; hvi checks it a third time, and its manifest over all
of them. `chain` is a running sha256 over every record's kind and header,
and `records` their number, so a record lost, added, moved or altered
fails the move even when each payload is intact. The reader holds every
length and name to limits before it allocates, and refuses a stream
without its trailer or with bytes after it, except, in a session, the one
`K` record, which is not in the chain. `hviReply` is hvi's reply to
`migrate`, with its numbers.

hull treats a frame named `ram-round-*` or `disk-round-*` as a round: bulk
data it relays and spools but never holds whole. Every other frame is a
document, and the documents of one stream are held to 64 MiB together.

### The destination

`migrate-receive --stream` waits for a maker of the base's spare at work
(at most a minute; see [The way back](#the-way-back)), takes the spare as
the instance's disk (or copies the base when there is none), syncs it,
and records that it took this attempt's header (`streamAccepted`); the
source waits for that before it asks hvi to `migrate` (in a session the
destination says so on its stdout; otherwise the source polls
`migrate-status` every 50 ms), so a refusal of the header comes while the guest still runs. A copy whose writeback is still under way
would cost the downtime: hvi syncs the disk after the rounds it writes and
waits for that before the guest may run, and on rp2 an unsynced 64 MiB
copy made that 4 to 20 s.

Once `profile.json` has arrived and matched the move's machine profile, it
starts `hull restore --detach --paused --move T`, which runs `hvi restore
--from-stream - --disk <the copy> --disk-base-sha256 <base>` with a pipe as
its stdin, and from then on hands every frame to that pipe as it arrives.
hvi writes the disk rounds into the copy and the RAM rounds into guest RAM
as they come, so no RAM reaches the destination's disk. Once it has the
manifest it checks it against every frame, checks the disk rounds against
`disk.json` (each round's bitmap and digest, the last one taken paused, at
the generation the disk was captured at), syncs the disk and builds the
machine; told the base, it refuses a stream from another base or one with
no disk rounds, either of which would leave the bare base as the guest's
disk. While the stream arrives hull reads the base and checks that its
blocks hash to the digest it is named by; one that does not is set aside
as `<digest>.ext4.bad-<time>` and the move refused with `no_base`, so the
source sends the base again and goes on from its spool. The move is
`verified`, and the guest `restored-paused`, once hull has checked the
trailer, hvi has restored and the base has checked out. The documents are
kept in the move's checkpoint directory for the record. The instance's
record takes the manifest and the move's epoch in one synced write then,
so the commit does not save the epoch again with the guest stopped;
nothing runs the instance before the commit, and an abort removes it.

### One session

From the stream's header to the destination's resume a move with a
destination that lists `stream-session` opens no ssh session but the
receive's, and polls nothing. The destination writes lines on its stdout
as the move goes, each a reply object with an `event`:

| `event` | When |
|---|---|
| `accepted` | it took this attempt's header and synced its copy of the base (`detail`: the attempt) |
| `acked` | every 64 KiB it reads of the stream, or 5 ms after the last if it read anything: `bytes`, what it has read so far |
| `verified` | hvi restored the guest paused from a stream checked whole: `record` and `vm`, as the reply of a receive outside a session |

and a last line with no event. After `verified` the source checks the
destination's record against what it sent and records its commit point,
as it would after a receive's reply, and only then sends the commit as
the `K` record after the trailer; the destination runs it as
`migrate-commit` would, under the move's lock it already holds, and the
last line is the commit's reply. A source that ends the stream instead,
because a check failed, or sends nothing for 20 s, leaves the guest
restored paused there, and the last line is the verified reply again.
The 20 s is shorter than the 30 s that `migrate-commit` and `--abort`
wait for the move's lock, so a source whose session was cut, or an
operator, finds the lock free once the receive gives up.
The order is the one the separate verbs keep: the source commits only
after the destination said verified, and the destination resumes only on
the commit. A session that ends without the commit's answer (the link
lost after the commit went) leaves the source in `committing`, and it
sends the commit again as `migrate-commit` at once, which the destination
answers from its record; a re-run does the same. So does a refusal in the
session with code `failed`: a commit that came damaged after the
trailer, or a step of the commit that failed, is no verdict on the
move. The receive ignores
SIGPIPE in a session, so a source that is gone fails a line rather than
killing the receive half way.

The source holds what it has handed to ssh and the destination has not
yet said it read to a window: the fastest delivery rate of the last eight
samples times twice the round trip, or times 20 ms if that is longer,
between 128 KiB and 64 MiB; the stream's write buffer is 64 KiB in a
session, against 1 MiB outside one, and so is hull's read of hvi's
socket. Without the window the source wrote into ssh as fast as ssh took
it, and ssh's 2 MiB channel window, the kernels' socket buffers and the
link's queue held about 3 MB on their way when hvi sealed the stream: on
a link of 1 MiB/s that is 3 s, and the stopped guest waited for it (see
[Measured](#measured)). The window keeps two round trips' worth on the
way; on a LAN, where the round trip is a fraction of a millisecond, the
20 ms keeps the rate at the link's.

The round trip is the shortest the acks showed in the last second, or
in the last ten round trips if that is longer. Then the window drains to
half of what the link holds at the latest rate, or 128 KiB if that is
more, until a write sent into the drained window is read, and that
write's round trip replaces it, whether longer or shorter. Before
2d3caa8 the window kept the shortest round trip of the whole move, and
a round trip that grew during the stream shrank the window with each
sample down to 128 KiB: with 40 ms of delay added 0.3 s into a move on
rp1 to rp2, the RAM rounds ran at 2.8 to 3.1 MiB/s and the move took
28.5 s, against 5.3 s with the delay there from the start. The latest
round trips alone would not do: while the window is full each write's
round trip includes the queue the window made, and taken as the link's
it would double the window with each sample. A drain costs the link
about half a round trip.

hull reads hvi's socket through a 64 KiB buffer in a session (1 MiB
before e852118): what hull has read and not yet relayed is behind the
window, which does not count it, and at the seal the guest waits for it
too. What hvi's side of the socket holds is not hull's to bound.

### Failures and the spool

The source keeps hvi's stream as hvi wrote it, disk rounds included, in
`<instance>/checkpoint/<txid>/stream`, with the documents beside it and a
`sealed` file once it is complete and synced. It reads hvi's stream to its
end whatever the destination does: hvi fences the guest as soon as its
manifest is in the socket's buffer, and a fenced source must hold a whole
copy to restore from. So:

- a stream that ends before hvi's manifest (a refusal, a failed capture,
  a pause whose vCPUs did not park in time, a reader hvi cut for stalling,
  a lost connection) means hvi did not fence: the guest runs on here. The
  move's error and its record say "hvi's stream ended before its manifest,
  so the guest runs on here", `--abort` leaves it running, and running the
  move again streams it from the start;
- hvi fails a stream that has begun by closing the connection (since hvi
  2e7fb90). It writes its refusal first if the socket has room for it at
  once, so the refusal may come where a frame header was due, among a
  frame's bytes when the failure came part way through one, or cut short.
  hull reads the end of the stream, or `ECONNRESET` on Linux when hvi
  closed with bytes of hull's unread, as hvi's failure: the error says
  "hvi failed its migrate stream and closed it" with hvi's refusal when
  hull found it at the end of the stream, whole or as it arrived, or
  "hvi closed its migrate stream, as it does when the stream fails, and
  hull read no refusal". hull then asks hvi's `status` as below, and says
  what hvi says, even when it found hvi's refusal whole: the end of the
  stream may be a frame's bytes, which the guest wrote, and they must not
  decide whether the guest is fenced. Only a whole refusal read where a
  frame header was due is taken as "runs on" without asking, as before;
- a stream hull stops reading before the manifest on a failure of its own
  (a spool write on a full disk, a document too large to keep, a frame
  that does not match its sha256) may still have fenced the guest: hvi
  fences once its manifest is in the socket's buffer, and with a small last
  round the rest of the stream can be there before hull reads it. hull
  closes the connection, so an hvi still writing stops and resumes the
  guest, and then asks hvi's `status`, which hvi answers once its op has
  ended. Only hvi's refusal is taken as "runs on" without asking. Running
  on, the move says so as above; fenced, it says "hvi fenced the guest,
  though hull did not take its whole stream", and a re-run or `--abort`
  has hvi stream it again (below); with no answer, the error says hvi did
  not say whether it fenced the guest, and not to stop the instance;
- hvi gives the part of the stream it writes with the guest paused, from
  `disk-round-final` to the manifest, 10 s plus twice the last round with
  the guest running (its RAM round and the disk round before it). Past
  that every write of hvi's fails and the guest runs on. In the middle of
  a frame hvi cannot say so: its refusal lands among the frame's bytes.
  Before 2e7fb90 it then waited for the next request on the open
  connection; it now closes it, as above. hull waits up to 2 minutes for
  a byte from hvi while the guest runs.
  Once it has the first frame of `disk-round-final` (in a stream with no
  disk rounds, the first document after the RAM rounds) it waits only as
  long as that budget, from the rounds as it timed them, plus 5 s, for an
  hvi that stops writing without closing. hull
  times a round by its frames' arrival, so the last running round
  includes hvi's sync and pause before `disk-round-final`, and the bound
  is at least hvi's. A stall past it fails with `hvi sent nothing for
  B s in the part of its stream it writes with the guest paused, longer
  than it gives that part (P s: 10 s and twice the last round with the
  guest running, R s)`, and hull asks hvi's `status` as above. Running
  on, the move says the guest runs on here; fenced, it says so. A second
  stream of a fenced guest has no paused budget in
  hvi, so the 2 minutes stand for it. Before this bound, one move under
  the hog waited out the 2 minutes after `ram-round-2` stalled (see
  [From the seal to the resume](#from-the-seal-to-the-resume));
- a destination that takes none of the stream, such as a hull stopped
  with SIGSTOP whose ssh session stays open, acks nothing and reads
  nothing, so the source's window, or the pipe to ssh, holds the relay
  and hull reads nothing of hvi's socket. A write to the destination that
  waits longer than the bound hull gives hvi at that point (2 minutes
  while the guest runs, the paused part's budget plus 5 s after it)
  ends the link: hull prints `the destination took none of the stream
  for B s, and hull cut it off`, ends the destination's ssh session, and
  reads the rest of hvi's stream into the spool. hvi has given up on the
  stalled stream by then and closed it, so hull asks hvi's `status` and
  the guest runs on here, as above; an hvi still writing seals the
  stream, and the guest is fenced with a whole spool, for a re-run or
  `--abort` as below. A send that fails for another reason gives the
  destination 30 s to answer before hull ends its session;
- one that fails after the manifest leaves the guest fenced with its spool
  recorded as the instance's checkpoint (`migrating`). Running the move
  again sends the spool (`"replay": true`) to a fresh copy of the base
  there; `--abort` restores the guest here from the spool with `hvi
  restore --from-stream`, which writes the spool's disk rounds back onto
  the guest's own disk, in order, and so leaves it as it was at the fence
  (every block they hold, the disk already holds; doing it again after a
  crash ends the same way).

From hvi's `migrate` op until the fence is recorded, the stream hull is
reading is the guest. In that window:

- `hull migrate` holds SIGINT, SIGTERM and SIGHUP, says so, and stops once
  the fence is recorded. Outside it they stop the move at its next step.
  SIGPIPE never ends hull, and ssh runs in its own process group, so a
  Ctrl-C reaches hull alone.
- The spool's file gets every byte as soon as hvi's manifest has been
  relayed. A hull killed before the seal (`kill -9`, a crash) leaves a spool
  with no `sealed` file. A re-run, an abort and `hull restore` check such a
  spool frame by frame against the manifest it ends with, and seal it when
  it is whole.
- A spool that is not whole (cut short, or never written) is taken again
  from hvi while it holds the fenced guest. hvi streams a fenced guest
  again only for the `txid` and `epoch` that fenced it, which hull sends
  from the move's record; any other is refused with `fenced`. That second
  stream is the same machine, the RAM and disk of the fence, in other
  bytes: one RAM round, one disk round (`disk-round-final`) of every block
  written since boot, the documents as first sent, and a manifest of its
  own, whose sha256 is never the first one's. Its reply names the first
  manifest (`stats.first_manifest_sha256`); hull holds that to the one on
  record, when the instance's record or the move's has it. It also checks
  the sha256 the new manifest lists for `profile.json`, `cpu.json`,
  `platform.json`, `devices.json` and `session.json` against the first
  manifest, which the instance's record of the fence (`latest.json`)
  keeps, or, for a hull killed before that record, against those of them
  the cut spool holds whole; with neither, it warns that it takes hvi's
  word. It refuses a second stream of another fence, or of other
  documents, emptying the spool so no later check seals it. A second
  stream that fails leaves the guest fenced, and the error says so. The
  instance's record and the move's then name the new manifest, and the
  move's keeps the first as `firstManifestSha256`.
- An abort stops the fenced `hvi` only once the spool is whole. If the
  spool is not whole and that `hvi` is gone, this host has no copy of the
  guest; the error says so, and does not suggest an abort that cannot
  work.

The second stream is restored only where the first never ran: on this host
by an abort, which first has the destination's word that it keeps nothing
of the move, or on the destination by a re-run, whose receive stops any hvi
an earlier attempt started there before it reads the new stream. A
destination never resumes a guest before the source's commit, and the
source commits only after its record holds what the destination verified.

A source killed after the destination verified the stream, before its own
record took what it sent, leaves the destination `restored-paused` and the
move's record without an account of the stream. Running the move again
then adopts the fence from the spool (whole, or streamed again as above)
and commits the destination's copy, without sending the guest again, when
the manifest the destination verified is the one the guest was fenced
with; any other manifest is refused and the source stays fenced. That
check compares the manifest's sha256 with the one hvi's second stream
names as the first; the destination's manifest itself stays there.

On a busy disk every synced write is a wait the stopped guest sits
through, so from hvi's seal to the commit the source makes one:

- The source puts the fence (the spool, its manifest, the disk as fenced)
  on the move's record in memory, and writes it with the commit point
  (`transferred` and `committing` together), the one synced write the
  commit waits for. The move's record, synced at `transferring` before the
  stream began, already keeps the instance from running here meanwhile.
  The instance's own records of the fence (`latest.json`, `state.json`)
  are written beside the trailer and the destination's checks, and the
  commit does not wait for them.
- The spool is not synced while the stream runs (its writeback is started
  every 32 MiB), and its seal syncs it only once the commit is answered or
  the move stops. `hull migrate` waits for the seal before it exits.
- The destination writes hvi's documents beside its record without a sync
  (hvi has the machine from the stream) and records `received`,
  `verified` and `restored-paused` in one write, after the instance's
  record with the manifest and the epoch. Its commit still makes three
  synced writes before the resume (the ledger, the record, and the
  instance's record of the running guest); on a disk whose 4 KiB `fsync`
  takes a second or more they cost seconds of downtime (see
  [Measured](#measured)).

A destination whose `hvi` refuses the machine stops reading, and the
refusal is the move's error, with code `restore_refused`.

### Numbers

`migration.json` on the source gains `stream`:

| Field | Meaning |
|---|---|
| `mode` | `live`; `replay` for a spool sent again; `adopted` for a destination's copy verified in an earlier attempt and committed without sending |
| `rounds` | each of hvi's rounds and documents as relayed: `kind` (`disk`, `ram` or `state`), `name` (hvi's component name), `bytes`, `wireBytes`, `seconds`, and from hvi's reply `final` (taken paused) and, for a disk round, `blocks` and `bytes` as raw block bytes |
| `diskBytes`, `diskWireBytes`, `ramBytes`, `stateBytes`, `wireBytes`, `seconds` | totals: the disk rounds' raw bytes (hvi's `disk_raw_bytes`) and their bytes on the wire, RAM as hvi framed it, documents, the whole stream |
| `roundsEndAt`, `pausedAt`, `sealedMs` | when hvi's last round with the guest running ended (`rounds_end_unix_ms`), when it stopped the guest after the sync that follows and its wait for the I/O threads, which the guest runs through (`precopy_end_unix_ms` plus `gate_ms`), and how long from the pause to its seal. A replay takes the pause of the attempt that fenced the guest: from the spool's reply when that is hvi's reply to the first stream, else from `latest.json` or the earlier attempt's record; with none of them `pausedAt` and `downtimeMs` are left out |
| `finalDiskMs`, `finalDiskBytes` | `disk-round-final`: hvi's time for it and its raw bytes |
| `stopReason`, `hviPausedMs`, `hviGateMs`, `diskSyncMs`, `pauseSyncMs` | from hvi's reply: why the running rounds stopped, the source's part of the downtime, the wait for the I/O threads at the pause, the disk's sync before the pause and the one in it (0 when the stream carries disk rounds, since hvi 65a929f) |
| `downtimeMs` | from `pausedAt` to the destination's `resumed` step, on two hosts' clocks; from a destination without steps, to its record of running plus the resume's own time (the destination's `timingsMs.resume`) |
| `chain` | the trailer's hash, which the destination's record must match |
| `hviReply` | hvi's reply to `migrate`, as hvi wrote it |
| `steps` | when this host reached each step from hvi's seal to the commit, by its own clock: `destination accepted`, `hvi sealed the stream`, `instance's fence recorded`, `trailer sent`, `destination answered`, `commit point recorded`, `commit sent`, `commit answered` |
| `destSteps` | the destination's steps, by its clock, from its replies: `waited for the base's spare being made` (or `gave up waiting for ...`; only when a maker of the spare was at work), `disk made from the base's spare` (or `... by copy, with no spare`), `manifest read`, `trailer read`, `hvi restored the guest paused`, `base checked`, `verified recorded`, `commit received`, `epoch saved` (only when the receive did not save it), `ledger written`, `running recorded`, `resumed`; its own record has them too |

A source that took the spool from hvi again also records
`firstManifestSha256` beside `manifestSha256`.

Progress goes to stderr prefixed `move: `: one line per round as it ends,
named as hvi names it (`disk-round-0: 98.5 MiB in 1.73 s, 56.8 MiB/s`,
`ram-round-0: ...`, `disk-round-final: ...`), a line every two seconds of a
long round (`... so far ...`), then `hvi: 5 RAM rounds (converged), disk
rounds of 5426, 487, 0, 0, 0 blocks (369.6 MiB, 128.5 MiB on the wire);
paused 8 ms, disk sync 0 ms before the pause and 0 ms in it` from hvi's
reply, `hvi sealed the stream and fenced NAME (5 ms after the pause)`, and
the summary after the commit (`...; sealed 5 ms after the pause, last disk
round 0 bytes in 0 ms; downtime about 102 ms`), and then both hosts'
steps from the seal in milliseconds (`from the seal, ms (there: the
destination's clock): trailer sent 0, there manifest read 2, ..., there
resumed 33, commit answered 41`). A replay whose pause is on
no record ends its summary `the guest was paused in an earlier attempt, at
a time not on record`, with no seal and no downtime. A stream that ends
before the manifest with the guest not fenced prints `hvi did not fence
NAME: ... the guest runs on here`.

### Measured

2026-10-15. hull at ef78fb6 (the stream-v3 move), `hvi` c4131f5, the
`hvi-vmm` pin, built on each host. The guest's downtime is the rise in the
`downtime total` its `hvi-time` driver logs across the move, which counts
the time its clock was stopped. After every move the guest's counter had no
gap, and its `boot_id`, the blob's sha256 and the deleted file still open on
fd 4 were unchanged.

Guests: `docker.io/library/ubuntu:latest` (1 GiB, 1 vCPU) on the Pis and
`ghcr.io/brig-sh/claude-code-stock:root` (4 GiB, 4 vCPUs) from the Mac,
both `--profile portable-arm64-v1`, block rootfs, `--net none`. "Idle" and
"light" append a counter ten times a second and sync every tenth; "writer"
also rewrites 1 MiB of a 64 MiB file every 0.1 s (about 10 MiB/s); "256 MiB
dirtier" adds a loop rewriting 256 MiB of tmpfs with random bytes to the
writer. Every destination was prepared (`hull migrate-prepare`) first.

| Move | Guest | Total | Downtime | Sent | Disk rounds (blocks) | RAM rounds |
|---|---|---|---|---|---|---|
| rp1 to rp2, before: hull 331bf8c, `--whole-bundle` | ubuntu, writer | 8.2 s | 7.43 s | 288.2 MiB | the whole disk | the whole RAM, paused |
| rp1 to rp2 | ubuntu, idle | 2.29 s | 78.6 ms | 109.5 MiB: disk 275.0 MiB as 34.5 MiB, RAM 74.9 MiB | 4393, 7, 0 | 3 (converged) |
| rp2 to rp1 | ubuntu, idle | 2.29 s | 84.0 ms | 109.6 MiB: disk 274.9 MiB as 34.5 MiB, RAM 75.0 MiB | 4396, 3, 0 | 3 (converged) |
| rp1 to rp2 | ubuntu, writer | 5.13 s | 94.7 ms | 310.4 MiB: disk 369.6 MiB as 128.5 MiB, RAM 181.8 MiB | 5426, 487, 0, 0, 0 | 5 (converged) |
| rp2 to rp1 | ubuntu, writer | 4.55 s | 123.7 ms | 305.9 MiB: disk 369.8 MiB as 128.5 MiB, RAM 177.3 MiB | 5430, 486, 0, 0, 0 | 5 (converged) |
| rp1 to rp2 | ubuntu, 256 MiB dirtier | 65.0 s | 3.49 s | 1.7 GiB: disk 449.8 MiB as 208.6 MiB, RAM 1.5 GiB | 5419, 487, 324, 483, 484 | 5 (time_cap), the last 294 MiB |
| rp2 to rp1 | ubuntu, 256 MiB dirtier | 43.2 s | 3.40 s | 2.1 GiB: disk 500.2 MiB as 258.6 MiB, RAM 1.8 GiB | 5425, 804, 324, 483, 484, 484 | 6 (round_cap), the last 293 MiB |
| this Mac to rp1, a 1 MB/s link | claude-code, light | 129 s | 6.45 s | 122.7 MiB: disk 284.5 MiB as 34.7 MiB, RAM 87.9 MiB | 4523, 29 | 2 (time_cap) |
| this Mac to rp2, a 1 MB/s link, through `brig teleport` | claude-code, brig's demo | 70.1 s | 7.96 s | 70.9 MiB: disk 245.9 MiB as 2.6 MiB, RAM 68.2 MiB | 3834, 101 | 2 (time_cap) |
| the same, again | claude-code, brig's demo | 73.1 s | 6.82 s | 75.9 MiB: disk 246.8 MiB as 2.6 MiB, RAM 73.2 MiB | 3839, 102, 7 | 3 (time_cap) |

- The writer's final disk round was empty, and the dirtier's 484 blocks
  (30.2 MiB) took 0.29 to 0.33 s to read and send inside the pause: the
  last running round is a few seconds long, so the round taken paused
  holds what the guest wrote in those seconds. With stream-v2 the same
  writer's last disk round, read after the seal, was 20 to 64 MiB.
- Disk round 0 of the ubuntu guest is 4393 to 5509 blocks (275 to
  360 MiB) though it wrote little: ext4's lazy init zeroes the inode
  tables after the first boot, and those blocks compress to 34 MiB.
  Moved back, round 0 sends them again: the restored hvi carries the
  blocks written since boot, so every move sends every block that
  differs from the base.
- The dirtier never converges: the downtime is its last RAM round
  (293 MiB) and the last disk round at the link's rate, as with
  stream-v2's 4.51 s for the same guest.
- The Mac's link to rp1 gave 1.0 MB/s that day (16 to 53 ms round trip),
  against 10 to 46 MB/s on earlier days, so that row measures the link:
  RAM round 0 took 90 s and hit the time cap, and the RAM round taken
  paused (3.1 MiB) took 3.05 s to send. The prepare that sent the 1.5 GiB
  base as 740.8 MiB took 935 s on the same link. `hull run` of
  claude-code on this Mac compared its disk with the base in 0.63 s.
- hull's own part of the downtime, from hvi's seal to the destination's
  resume, was 55 to 90 ms in the idle and writer moves on the Pis.
- The two moves to rp2 ran on 2026-10-15 through brig's
  `script/teleport-demo-claude.sh --prepare` (brig 06340ae, the script
  as of 2b39954), with hull d2bb5e9 and `hvi` c4131f5 built with
  `HVI_GIT_SHA` on each host. The Mac reached rp2 from another network
  at 1.0 to 1.1 MiB/s. RAM round 0 (63 MiB) took about 60 s and hit the
  time cap. The RAM round taken paused, 5.4 and 4.4 MiB, took 5.05 and
  3.66 s to send, so hvi sealed 5087 and 3677 ms after the pause. From
  the seal to the destination's resume took 2.9 and 3.1 s, against 55 to
  90 ms between the Pis: the stream still on its way at the seal (see
  "From the seal to the resume" below). The guest's counter,
  `boot_id`, sum and deleted file held on fd 3 checked out after each
  move, and every exec into the moved guest worked.
- On the Mac, four sessions of claude-code booted against one base,
  9890a88a4845. The first `migrate-prepare` of rp2 sent it, 1.5 GiB as
  742.8 MiB in 733.8 s; the prepares after it found it held, in 12.1 and
  47.1 s, each making the spare copy for the next move. A move that
  found no spare copied the base first, and the destination took the
  stream 23.5 s late.

The source's controller killed during RAM round 0 of a claude-code move
from this Mac to rp2, through `brig teleport`:

- `kill -KILL` of brig alone, 10 s into the move: hull, its child,
  outlived it, went on and committed 55 s after the move began. An abort
  then was refused: the move had committed.
- `kill -KILL` of brig's process group, hull and its ssh with it, 33 s
  into the move: rp2's record said `receiving`, "receive: the stream ended
  in hvi's ram-round-0: unexpected EOF". The guest ran on here, never
  fenced: its counter went on with no gap over 0.5 s and its kernel logged
  no restore. `hull migrate --abort`, through `brig teleport --abort`,
  took 0.90 s; rp2 removed its copy.
- Until that abort, `hull migrate --status` printed `transferring` with
  "The source is fenced and stays so until the move commits or is
  aborted", although the guest was running: a streamed move's source is
  fenced only from hvi's seal. Since 10c9106 it asks: a fence on the
  instance's record or from hvi is a fence, hvi saying running is "The
  guest still runs here", and hvi giving no status, which it does not
  while its `migrate` streams, was "The guest still runs here; hvi is
  streaming it". That was wrong for part of that time: hvi answers
  nothing from before the pause of its last round to after hull has read
  the stream's tail, and fences the guest at its seal. Since bed4d7d a
  silent hvi is "hvi is busy with the move's migrate op and answers
  nothing else: the guest runs until hvi pauses it for the last round,
  and is fenced from hvi's seal on", and "fenced" once the move's spool
  holds hvi's manifest or is sealed. hvi saying paused has its own text.

Under a busy disk, an O_DIRECT writer (`dd oflag=direct`, 4 MiB blocks)
on both Pis, with the ubuntu writer. A 4 KiB `fsync` took 0.25 to 0.95 s
on rp1 and 2.0 to 8.2 s on rp2 during these runs.

| hull | rp1 to rp2 | rp2 to rp1 |
|---|---|---|
| 331bf8c (stream-v1) | 91.3 s down, 338.6 s total | 38.9 s down, 84.4 s total |
| d4ae775 (stream-v2) | 7.39 s down, 50.1 s total | |
| ef78fb6 (stream-v3) | 10.98 s down, 96.8 s total (after the failed attempt below) | 0.66 s down, 19.9 s total |

- rp1 to rp2: hvi's pause took 3.35 s of it (`paused_ms`; 2.25 s of that
  the sync in the pause, and 13.8 s the sync before it, with the guest
  running); the destination's record and its answer took 1.6 s, the
  source's commit point 2.4 s and the destination's commit, four synced writes and the
  resume, 3.8 s. rp2's disk was slower than in the d4ae775 run (a 4 KiB
  `fsync` 2.0 s, against 1.4 s), and the synced writes the commit makes
  on both hosts are now most of the downtime.
- The first attempt of that move failed with the guest running on: rp2
  had no spare base, so the receive copied it (83.6 s), and the
  destination's hvi, writing the disk rounds into its copy, stalled
  reading for over 30 s; the source's hvi cut the stream in its second
  RAM round, before the manifest, and hull reported "hvi's stream ended
  before its manifest, so the guest runs on here". A prepare made the
  spare and the move run again with the same txid went through.

hull killed with SIGKILL as hvi's seal line appeared (rp1 to rp2):

- The move's record had no fence and the spool no `sealed`. `hull migrate
  --abort` found the spool whole, sealed it, stopped the fenced hvi and
  restored the guest from the spool with `hvi restore --from-stream`,
  which wrote the spool's disk rounds back onto the guest's own disk.
- In a second run the kill landed after the destination had verified the
  stream (`restored-paused` on rp2). With the spool then cut to 1 MB by
  hand, the re-run had the fenced hvi stream the guest again (one RAM
  round, one disk round of 5515 blocks, in 1.4 s), found its reply's first
  manifest equal to the fence's and the destination's, adopted the fence
  and committed the destination's copy without sending it again.

- In a third run (rp2 to rp1), the kill also landed after rp1 had
  verified the stream, and the spool was cut to 1 MB. `--abort` had rp1
  release its copy, had the fenced hvi stream the guest again (one disk
  round of 5751 blocks, 98.9 MiB on the wire, and one RAM round of
  138.4 MiB, in 1.4 s), checked its reply's first manifest against the
  instance's record, recorded the new manifest on the instance and the
  move, and restored the guest here from that second stream, 3.3 s after
  the abort began.

Each time the guest went on with no gap in its counter and the same
`boot_id`, blob and open file.

A guest whose run wrote an `/etc/hosts` entry over the base (`--add-host`)
booted with 4 blocks set in its `--disk-written` bitmap (compared in 53 ms
on rp1) and moved from rp1 to rp2 in 4.5 s with 127 ms of downtime. On
rp2, with the guest stopped, a copy of its disk replayed its journal
(`e2fsck -E journal_only`), passed `e2fsck -fn` (only the free block and
inode counts differ, as on any disk of a running guest) and held the
entry.

#### From the seal to the resume

2026-10-15, later. `hvi` ba9b2d2 (the pin since 9886afb) built on each
Pi with `HVI_GIT_SHA`; hull 7c71ed4 (the steps above, nothing else
changed) against the session hulls after it. The guest is the ubuntu
guest above, moved from rp1 to rp2, its downtime the rise in `hvi-time`'s
`downtime total`; "seal to resume" is from hull reading hvi's seal to the
destination's `resumed` step, on the two hosts' clocks. Those are not
within a millisecond of each other: a UDP probe at its lowest round trip
(0.40 to 0.43 ms) put rp2 3.2 to 4.9 ms behind rp1 that evening, and
chrony on rp1 gave itself 27 ms of root dispersion. The figures up to the
bed4d7d set are as the clocks gave them, so a cross-host figure there
(seal to resume, hull's `downtimeMs`) is about 3 to 5 ms short; the
bed4d7d set corrects them by the probe run before each move.
rp1 to rp2 runs over the tailnet (`tailscale0`). For the emulated WAN
only that traffic was shaped on rp1, and the shaping was removed after
each set of runs:

```
sudo tc qdisc add dev tailscale0 root handle 1: prio bands 4 priomap 1 2 2 2 1 2 0 0 1 1 1 1 1 1 1 1
sudo tc qdisc add dev tailscale0 parent 1:4 handle 40: netem delay 100ms rate 10mbit
sudo tc filter add dev tailscale0 parent 1: protocol ip prio 1 u32 match ip dst 100.81.202.90/32 flowid 1:4
```

which made the round trip to rp2 100.7 ms (0.6 ms without it), and the
stream ran at 1.1 MiB/s.

| Link | hull | Seal to resume | Downtime | Total |
|---|---|---|---|---|
| emulated WAN, idle | 7c71ed4, before | 3043, 2898 ms | 4792, 4642 ms | 98.4, 98.2 s |
| emulated WAN, idle | 1144920 (one session) | 445, 478 ms | 2418, 2449 ms | 99.0, 98.0 s |
| emulated WAN, idle | 612899a | 434 ms | 2242 ms | 97.9 s |
| LAN, idle | 7c71ed4, before | 79, 68, 158, 73 ms | 115, 105, 164, 103 ms | 2.21 to 2.52 s |
| LAN, idle | 612899a | 34, 33 ms | 49.0, 57.4 ms | 2.16, 2.12 s |

Where the time went from the seal, on the WAN:

- Before: the trailer left hull 653 ms after the seal (hull's 1 MiB
  write buffer and the pipes to ssh), and the destination read the
  manifest 2566 ms after it (ssh's window, the socket buffers and the
  link's queue). Its restore took 45 ms more, its verified record 5 ms,
  the source's commit point 184 ms, and the commit, in a new ssh session,
  230 ms before the destination had it. From hvi's own seal, which can
  come before hull reads it with the stream behind hull's buffer in hvi's
  socket, to the resume took 3.9 and 2.9 s.
- One session: the manifest arrived 300 to 344 ms after the seal, 100 ms
  of it the link's one-way delay, then 13 to 19 ms of restore, 5 ms to
  record verified, 3 to 4 ms for the source's commit point, the link's
  100 ms for the commit in the session, and 8 to 10 ms for the
  destination's commit. From hvi's own seal to the resume: 0.65 to
  0.81 s.
- From hull's read of the seal that is under 300 ms plus two round
  trips (501 ms) on that link. From hvi's own seal, the measure the
  target was set in, it is 0.65 to 0.81 s, over it.
  The downtime fell by about 2.3 s, the stream on its way at the seal;
  the rest is hvi's pause (`paused_ms` 1588 to 1644 ms), its last rounds
  at the link's rate.
- The session hulls before 612899a held the LAN's RAM rounds to 45.5 to
  46.5 MiB/s against 52.3 to 58.6 MiB/s without a session: the window of
  twice the shortest round trip was 128 KiB there. With the 20 ms floor
  (612899a) they ran at 57.4 and 58.2 MiB/s.

On the LAN, hull 612899a and after, rp1 to rp2:

| Guest | Total | Downtime | Seal to resume | Sent | Rounds |
|---|---|---|---|---|---|
| idle | 2.16 s | 49.0 ms | 34 ms | 109.4 MiB at 59.9 MiB/s | disk 4390, 7, 0; 3 RAM (converged) |
| writer | 4.10 s | 51.4 ms | 48 ms | 299.4 MiB at 78.8 MiB/s | disk 5413, 487, 0, 0; 4 RAM (converged) |
| 256 MiB dirtier | 27.5 s | 3.37 s | 43 ms | 2.1 GiB at 78.3 MiB/s | disk 5416, 808, 484, 323, 484, 484; 6 RAM (round_cap) |
| writer, O_DIRECT hog on both | 59.5, 59.1, 53.0, 65.3 s | 6.93, 19.40, 13.60, 22.04 s | 6.92, 19.09, 13.31, 22.02 s | 238 to 280 MiB at 5.9 to 6.9 MiB/s | 2 to 4 RAM (converged) |
| writer, O_DIRECT hog, rp2 to rp1 | 69.4, 77.8 s | 6.18, 4.45 s | 3.33, 4.13 s | 263, 283 MiB | 3 to 4 RAM (converged) |

- Under the hog the source's pause no longer waits for a park: hvi's
  `paused_ms` was 10, 313, 291 and 13 ms from rp1 and 2847 and 327 ms from
  rp2, with `park_ms` and `gate_ms` 0 in the two runs that printed them;
  the 2847 ms was the sync in the pause (2701 ms). Before ba9b2d2 H8b's
  run had 3.35 s of `paused_ms`. The downtime is now the destination's
  disk: in the 22.04 s run, hvi's restore on rp2 ended 15.8 s after the
  seal (it waits for its sync of the disk rounds it wrote), the verified
  record took 1.66 s, the source's commit point 1.79 s, and the commit's
  ledger, running record and resume 1.06, 1.02 and 0.72 s. The spread
  from 6.9 to 22 s is in the destination's restore (2.6 to 15.8 s) and
  the synced writes, all on disks the hog keeps busy.
- In two more idle runs, from rp1, `hvi`'s sync of the guest's disk took
  6.9 and 7.0 s before the pause and 1.0 and 3.2 s in it, and the RAM
  round ran at 8.7 and 8.1 MiB/s, with no hog: most likely rp1's disk was
  still writing out the guest's disk, copied from the base at its boot.
  The later runs wait for a `sync` after the boot, and none of them saw
  it again.
- SIGKILL of hull at "hvi sealed the stream": `--status` then said the
  source is fenced, as hvi did; rp2 had the guest restored paused, the
  session's stream having reached it whole, and waited for the commit
  until the stream ended. The re-run adopted the fence and committed rp2's
  copy in 0.73 s (the guest was down 1.02 s). In a second run, where rp2
  was still receiving at the kill, `--abort` released rp2 and restored the
  guest on rp1 from the spool in 5.2 s, the guest down 5.63 s. Both times
  the counter had no gap and `boot_id`, the blob and the held file were
  the same.
- On the emulated WAN, the writer (10 MiB/s of disk against 1.1 MiB/s of
  link) never converged: hvi stopped the rounds at the time cap and took
  the last disk round (66.4 MiB) and RAM round (67.2 MiB) with the guest
  paused, 115.9 s of downtime, of which seal to resume was 615 ms.
- Against H8b's run under the same hog (`ef78fb6`, see above: 10.98 s
  down from rp1, 0.66 s from rp2), the downtime is not lower: 2.5 to
  22.0 s from rp1 and 4.5 and 6.2 s from rp2 in the runs here. The park
  is gone, and the time went to the destination's restore and the synced
  writes on both hosts, whose length on these disks varies from run to
  run.

Again at hull 9cb3440, the code this page describes, with `hvi` ba9b2d2,
rp1 to rp2, one run each unless said. From the third move under the hog
on, rp1 also ran another project's guest.

| Guest | Total | Downtime | Seal to resume |
|---|---|---|---|
| idle | 2.19 s | 46.8 ms | 37 ms |
| writer | 4.12 s | 76.8 ms | 43 ms |
| writer, O_DIRECT hog on both | 83.7, 72.1 s | 6.04, 2.52 s | 5.69, 1.85 s |
| idle, emulated WAN | 103.3, 98.1 s | 2.71, 2.39 s | 793, 472 ms |

- Under the hog `park_ms` and `gate_ms` were 0 in both moves that went
  through, and `paused_ms` 357 and 305 ms, of which the sync in the pause
  took 349 and 300 ms.
- A third move under the hog failed, and the guest ran on at rp1. Its
  stream had slowed to 0.3 MiB/s in the last RAM round, taken with the
  guest paused (`ram-round-2`, 4.6 MiB in 14 s). That run was not
  sampled, and why the round slowed was not confirmed. Two causes fit.
  In the next move under the hog, `disk-round-2` ran at 1.7 MiB/s for
  5.8 s while a hull thread on rp1 sat in the ext4 journal
  (`wait_on_buffer`, `do_get_write_access`), maybe writing the spool. And
  the window of these hulls shrank to its 128 KiB floor when the acks'
  round trip rose during a stream; 128 KiB per round trip of about 0.4 s
  is the 0.3 MiB/s seen. 2d3caa8 fixed the window. hvi most likely gave
  up at its bound for that round,
  10 s plus twice the last round with the guest running, and resumed the
  guest. hull, part way through that frame, only noticed when its own
  2 minute bound for a silent hvi ran out: rp2 got the stream cut short
  in `ram-round-2`, refused it, and the move stopped
  at `transferring` 173.6 s after it began, with the guest unfenced.
  `--status` said "The guest still runs here", the counter had no gap
  and `boot_id`, the blob and the held file were the same, and
  `--abort` released rp2.
- On the emulated WAN the 793 ms run spent 348 ms on the source's synced
  commit point; in the other run it took 4 ms, and seal to resume was
  472 ms, under the 501 ms of 300 ms plus two round trips. From hvi's
  own seal, `pausedAt` plus `sealedMs`, the two runs took 1119 and
  801 ms: hull read the seal 325 and 328 ms after hvi wrote it, the tail
  of the stream still in hvi's socket behind the window. Both runs
  stopped the RAM rounds at the time cap, as the earlier ones did.
- SIGKILL of hull at "hvi sealed the stream" (writer, no hog): `--status`
  said the source is fenced, and rp2 had the guest restored paused. The
  re-run adopted the fence and committed rp2's copy in 1.59 s. The guest
  was down 4.69 s: hvi's pause took 1984 ms (1695 ms of it the sync in
  the pause, on rp1's busy disk), and the rest went from the kill to the
  re-run's commit, with the test's status checks between. The counter
  had no gap and `boot_id`, the blob and the held file were the same.

Again at hull bed4d7d, after the review of 9cb3440, with `hvi` ba9b2d2,
rp1 to rp2, one run each unless said. The changes: the window measures
its round trip again each second (2d3caa8), hull reads hvi's socket
through 64 KiB in a session (e852118), the destination waits 20 s for
the commit in a session, and a silent hvi gets its own `--status` text.
Seal to resume is corrected for rp2's clock, 3.7 to 4.9 ms behind rp1's
by the UDP probe before each move; "from hvi's seal" counts from
`pausedAt` plus `sealedMs`.

| Guest, link | Total | Downtime | Seal to resume, from hull's read / hvi's seal |
|---|---|---|---|
| idle, LAN | 2.20, 2.31 s | 69.3, 74.0 ms | 47 / 53, 46 / 50 ms |
| idle, 40 ms delay from the start | 5.08 s | 180 ms | |
| idle, 40 ms delay added 0.37 s into the stream | 3.57 s | 130 ms | |
| idle, emulated WAN | 103.7, 103.8 s | 2331, 2242 ms | 436 / 601, 529 / 586 ms |
| writer, O_DIRECT hog on both | 69.3, 49.9 s | 28.8, 11.0 s | |
| idle, SIGKILL at the seal, then re-run | re-run 0.41 s | 702 ms | |

- With the delay added during the stream, its RAM round ran at
  29.4 MiB/s, the same as with the delay there from the start
  (28.7 MiB/s). The review's run of that shape at 7b3f7b1 ran its RAM
  rounds at 2.8 to 3.1 MiB/s, took 28.5 s and was down 760 ms. The delay
  went on during `disk-round-0`, once the window had measured the LAN's
  round trip.
- On the emulated WAN, of the 601 and 586 ms from hvi's seal: hull read
  the seal 165 and 57 ms after hvi wrote it (170 to 328 ms with the
  1 MiB reader), and the destination read the manifest 468 and 444 ms
  after the seal. That is the stream ahead of the manifest, in hvi's
  socket and the window, and the link's 100 ms one-way delay. The rest,
  in the first run: 13 ms for the restore, 6 ms to record verified, 3 ms
  for the source's commit point, 101 ms for the commit's way through the
  link, and 10 ms for the destination's commit and resume; in the second,
  28, 4, 101 and 8 ms. The window allows twice a round trip of about
  153 ms: the link's 100 ms, and 52 ms to put one 64 KiB write on a
  10 Mbit/s link. So 300 ms plus two round trips is not met
  from hvi's seal: that needs the window under one round trip at the
  seal, and hvi's side of its socket smaller.
- `--status` polled every 5 s through the second WAN move said "hvi is
  busy with the move's migrate op and answers nothing else" each time
  during the rounds, and `committed` after.
- Under the hog both moves went through. hvi's `park_ms` was 0; its
  `paused_ms` was 4503 and 2097 ms, of which its wait for the I/O
  threads (`gate_ms`) was 2497 and 1249 ms. The rounds taken in the pause
  ran at 2.3 and 63.1 MiB/s, and 9.5 and 12.1 MiB/s. The downtime was the
  destination's disk: in the 28.8 s move hvi's restore ended 7.8 s after
  the seal, the verified record took 5.4 s, the source's commit point
  1.2 s, and the commit's ledger, running record and resume 4.2, 3.3 and
  2.5 s; in the 11.0 s move those were 2.7, 1.5, 1.9, 0.9, 1.3 and
  0.6 s.
- hull's progress line for the last round before the pause counts until
  the next frame, so it includes hvi's sync of the disk before the pause
  and its gate: `ram-round-3` of the 28.8 s move, 6.2 MiB "in 5.72 s,
  1.1 MiB/s", was 3157 ms of sync and 2497 ms of gate. In the 11.0 s
  move hull printed 4.50 s for `ram-round-1`, and hvi's own stats give
  it 128 ms.
- SIGKILL of hull at "hvi sealed the stream": `--status` said the source
  is fenced and rp2 held the guest restored paused. The re-run sealed the
  spool, adopted the fence and committed rp2's copy in 414 ms. The counter
  had no gap and `boot_id`, the blob and the held file were the same.
- The writer on the LAN, after the runs under the hog, with this hull
  and with 7b3f7b1 built the same way, in turn. With both disks quiet
  this hull took 4.19 to 4.45 s at 74.5 to 77.7 MiB/s (three moves) and
  7b3f7b1 4.06 s at 80.5 MiB/s, against 4.10 s at 78.8 MiB/s in the
  612899a set. The other moves of both hulls were slower, and the time
  went to the disks: this hull took 8.7 to 32.4 s in five, 7b3f7b1 8.3
  and 22.4 s in two. In the one slow move sampled, rp1's rounds slowed
  while its page cache had up to 160 MB under writeback, draining at
  about 8 MB/s. On rp2, hvi's restore took 3.7 and 4.2 s after the seal
  in one move of each hull, and in one move of this hull rp2 read the
  stream's tail, what the window had let on, 27.7 s after the seal, and
  the guest was down 27.9 s. rp2's disk is a DRAM-less 512 GB NVMe. The downtime of the moves that did not wait on
  rp2 was 37 to 89 ms with this hull and 34 and 38 ms with 7b3f7b1. No
  move lost a line of its counter.

#### Moving back with no prepare

2026-10-16, rp1 to rp2 and back with no `migrate-prepare` for the way
back. hull at aae490a (the spare only) and 71cc8c4 (the spare and the
base's check), built on rp1, hvi ba9b2d2. The guest is the idle ubuntu
one above (1 GiB, 1 vCPU, a counter ten times a second); its base is the
15 GiB image with 110.8 MiB of data. Each run: boot on rp1, prepare
rp2, move to rp2, wait for rp1's background step, `hull rm` the stale
copy on rp1, move back from rp2. Downtime is the rise in the guest's
`hvi-time` `downtime total` over each move.

| Run | Away: total, downtime | rp1's background step | Back: total, downtime | rp1's disk for the move back |
|---|---|---|---|---|
| aae490a | 2.14 s, 57.3 ms | spare copied in 0.4 s | 2.45 s, 46.4 ms | the spare, 1.86 s before the pause |
| 71cc8c4 | 2.16 s, 57.3 ms | spare copied in 0.4 s, base checked in 0.1 s | 2.37 s, 46.6 ms | the spare, 1.79 s before the pause |
| 71cc8c4, spare removed by hand before the move back | 2.18 s, 73.2 ms | spare copied in 0.3 s | 2.78 s, 87.2 ms | copied from the base, 1.74 s before the pause |

- The background step had written its lines to the instance's log by
  the time `hull migrate` had returned and the run script had checked
  the guest on rp2. `hull rm` on rp1 left the base, the spare and the
  `received` mark.
- The receive on rp1 made the disk before it accepted the header, so
  neither the spare nor the copy fell inside the pause (the times before
  the pause are across the two clocks, a few ms apart). That holds for
  the hull the recording ran (of the 9cb3440 era) too: since f43490c the
  receive copies the base and syncs the copy before it accepts the
  header, and the source starts hvi's migrate only after that. So a copy
  cannot be what paused the brig recording's move back 22.9 s. The
  likely cause is the receive's hash of rp1's base, which rp1 had built
  itself and never checked: the hash runs while the stream arrives, and
  the move waits for it before it is verified, with the guest paused
  once it outlasts the stream. The recorder's workaround, running
  `migrate-spare-base` by hand first, reads the whole base, which likely
  left it in rp1's page cache and made the hash after it fast. No run
  here shows the 22.9 s: this base hashed in 0.1 s.
- The guest's counter had no gap after any move, and `boot_id`, the
  blob and the held file were the same.
- The bound on the paused part of the stream was tested with a fake
  hvi only (`TestStreamedMoveStallInThePausedPart`), not live.

A move back that comes while rp1's background step is still at work,
2026-10-16, hull at 9b4d52e on both Pis, hvi ba9b2d2, the same guest and
base. With this base the step takes 0.5 s, so a holder of the spare's
lock on rp1 stood in for a slow copy: rp1's `.checked` and spare were
removed, the holder took the lock before the move away, and let it go 4
s after the move back started. The move away ran in an ssh session to
rp1 that ended at the commit. Another agent's benchmark was running on
rp2 (load 5 to 8, O_DIRECT writes with fsync), so the downtimes below
are not counted against the target.

- After the ssh session of the move away had ended, `ps` showed the step
  (`hull migrate-spare-base --for hc2`) with parent 1 in a session of its
  own. It waited on the holder for 4.6 s more, then made the spare and
  wrote the check 0.48 s after the holder let go.
- The receive of the move back on rp1 recorded `waited for the base's
  spare being made` 8 ms after the check was written, and `disk made
  from the base's spare` 1.7 ms after that. rp2 printed `the destination
  took the stream in 4.0 s`. `base checked` came 3 µs after `hvi
  restored the guest paused`: no hash in the pause.
- The move back's downtime was 48.8 s, by hvi-time. rp2, the source,
  took 46.9 s to record its commit point, a synced write, and hvi synced
  rp2's disk for 1.8 s in the pause. rp1's part after the manifest took
  33 ms.
- In a first run the move away took 50 s (rp2 restored the guest 41.5 s
  after the manifest), and the holder had let go before it committed.
  The step then finished before the move back came, and the move back
  took the spare, with 70.1 ms of downtime by hvi-time.

#### With hvi 94faf8f

2026-10-16, hull at fe874fb (this page's code before these numbers),
built on rp1 and copied to rp2, with `hvi` 94faf8f built on each Pi
(`HVI_GIT_SHA` set; the two builds have the same sha256, and each host's
instance record shows it ran that hvi). rp1 to
rp2 unless said, the guests and workloads above, every destination
prepared first. Downtime is the rise in the guest's `hvi-time` `downtime
total`; hull's own figure (`downtimeMs`) is beside it. A UDP probe put
rp2's clock 1.3 ms behind rp1's.

| Guest | Total | Downtime | hull said | hvi `paused_ms`, `pause_sync_ms` |
|---|---|---|---|---|
| idle | 2.20 s | 57.8 ms | 58 ms | 20, 0 ms |
| idle, back from rp2 with no prepare | 3.12 s | 67.6 ms | 69 ms | 27, 0 ms |
| writer | 10.10 s | 156.5 ms | 157 ms | 56, 0 ms |
| writer | 25.69 s | 223.7 ms | 227 ms | 187, 0 ms |
| writer, both disks quiet | 4.12 s | 90.9 ms | 91 ms | 49, 0 ms |
| idle, SIGKILL at the seal, then re-run | re-run 0.42 s | 380.2 ms | | 4, 0 ms |

- The move back took the spare rp1 made after the move away (`disk made
  from the base's spare`), and from hvi's seal to the destination's
  resume the move away took 37 ms, corrected for the clocks.
- `pause_sync_ms` was 0 in every move, so the source's pause is the
  final rounds: in the 25.69 s writer move the final disk round, 10.1 MiB
  in 103 ms, and the RAM round after it. In the 10.10 s move the source's
  synced commit point took 69 ms of the downtime (`destination answered`
  23 ms after the seal, `commit point recorded` 92 ms).
- The first two writer moves were slow while the stream ran: RAM round 0
  at 15 and 11 MiB/s, disk round 0 at 13 MiB/s in the second, and in
  that move hvi's sync of the disk before the pause took 3528 ms, with
  the guest running. After it a 4 KiB `dd oflag=dsync` took 1.5 to
  1.7 ms on both Pis, and the third move ran at 77.6 MiB/s. What held up
  the first two was not found.
- SIGKILL of hull at "hvi sealed the stream", 2278 ms into the move:
  `--status` said the source is fenced and rp2 had the guest restored
  paused. The re-run sealed the spool, adopted the fence and committed
  rp2's copy in 424 ms.
- A stalled reader: the 256 MiB dirtier, and rp2's `migrate-receive` hull
  for the move stopped with SIGSTOP 0.8 s after the source printed
  `disk-round-final`, then sent SIGCONT 25.3 s later. With no acks the
  source's window held back the relay, so hull did not read hvi's socket
  meanwhile; hvi's budget for the paused part, 10 s plus twice the last
  running round (4.46 s as hull timed it), ran out, and hvi failed the
  stream and closed it. 19 ms after the SIGCONT, on the two clocks, hull
  read the end and reported "hvi failed its migrate stream and closed it
  (hvi's ram-round-3 ended at 439669 of its 1048672 bytes): hvi refused
  migrate: the reader did not take the stream within the time the guest
  may stay paused; the stream is not sealed, and the VM is as it was
  (capture_failed)", and then, without asking hvi's `status` (hull at
  68c8bc1 took a whole refusal at the end of the stream as hvi's answer;
  it now asks), "hvi did not fence h12d: its stream ended before the
  manifest, and the guest runs on here". The
  move took 87.2 s, most of it RAM round 0 at 10 MiB/s. rp2's record said
  "the stream ended in hvi's ram-round-3: unexpected EOF", `--status`
  said the guest still runs here, and `--abort` said it never stopped
  running here. hvi resumed the guest in place, and `hvi-time` logged no
  downtime for it, so how long it was paused was not measured; hvi's
  budget, from the rounds as hull timed them, gives about 19 s.
- After every move the guest's counter had no gap, and its `boot_id`, the
  blob and the held file were the same.

## Records

Both hosts keep `<store>/instances/<name>/migration.json`, 0600:

| Field | Meaning |
|---|---|
| `version` | 1 |
| `txid`, `role` (`source` or `destination`), `name`, `epoch`, `state`, `peer` | the move |
| `remote` | source only: `target`, `store`, `hull`, `hvi`, `gatewaySock`, `allowNoRestore` |
| `destReleased` | source only, once aborted: the destination confirmed it keeps nothing of the move |
| `bundleDir`, `manifestSha256` | the fenced checkpoint, on either host |
| `firstManifestSha256` | source of a streamed move only, once hvi streamed the fenced guest again: the manifest the fence came with |
| `disk` | `path`, `size`, `modNs` (at the fence), `sha256`, `dataBytes` |
| `transfer` | `bytes`, `seconds`, `bytesPerSec`, `startedAt` |
| `spec` | destination only: what the source asked it to build (MAC, IP, sandbox id, network) |
| `verify` | destination only: `hvi checkpoint-verify`'s verdict |
| `timingsMs` | milliseconds per step: `fence`, `transfer`, `prepare`, `commit`, `restore`, `resume`; on a streamed destination `resume` is the resume alone and `commit` its whole commit, synced records included |
| `transport` | `stream` for a streamed move, absent for the whole bundle; chosen at preflight and kept for retries |
| `transportWhy` | why the move took the whole bundle |
| `baseSha256` | the base a streamed move's disk rounds apply to |
| `stream` | a streamed move's numbers, below |
| `error` | why the last step stopped |
| `history` | every state, with its time |

A destination keeps a ledger of the moves that ended on it, committed or
aborted, at `<store>/migrations/<txid>.json`. A commit writes its entry
before the instance record says `running`; an abort writes it before it
removes the instance directory, so the name is free again. Lookups by txid
read the instance's `migration.json` first and the ledger after it. Nothing
removes a ledger entry.

The instance record's `migration` field (see [storage.md](storage.md)) gains
three states: `migrating` (the source's fenced checkpoint, held by a move),
`migrated` (the source after the commit: stale) and `incoming` (the
destination before the commit), and two fields, `moveTxid` and `peer`.

## Failures

| Where it stops | Source | Destination | What to do |
|---|---|---|---|
| preflight or the probe refuses | running, nothing written | nothing | fix what it names |
| the reserve gets no answer | running, `preflight` | maybe `reserved` | run it again, or `--abort` (it warns if the destination does not confirm) |
| the fence fails | running (hvi resumes after a failed capture), `reserved` | `reserved` | run it again, or `--abort` |
| the transfer breaks | fenced, `transferring` | `receiving`, staging removed | run it again; it sends everything again |
| a checksum differs | fenced, `transferring` | `receiving` with the error | find out why; run it again or `--abort` |
| the disk changed after the fence | fenced, `transferring` | | `--abort`; something wrote the disk |
| `hvi checkpoint-verify` fails | fenced, `transferring` | `received` with the error | `--abort` |
| restore refused | fenced, `transferred` | `verified` with the error | fix the destination and run it again, or `--abort` |
| the commit's reply is lost | fenced, `committing` | `restored-paused` or `running` | run it again: it learns the commit |
| streamed: hvi's stream ends before its manifest (a refusal, a failed capture or park, a reader hvi cut for stalling, the paused part past hvi's budget; hvi closes the connection and hull asks hvi's `status`, or, with an hvi that does not close, reports the stall within that budget plus 5 s and then asks) | running, unfenced, `transferring`; the error says the guest runs on here | `receiving` with the error | run it again: it streams from the start; or `--abort`, which leaves the guest running |
| streamed: hull fails on one of hvi's frames before the manifest (a full spool disk, a document too large, a frame whose sha256 is wrong) | as hvi's `status` says: running and unfenced (the error says the guest runs on here), or fenced with the spool cut short (the error says hvi fenced the guest); `transferring` | `receiving` with the error | running: as above. Fenced: run it again or `--abort`: hvi streams the fenced guest again first |
| streamed: the stream fails after hvi's manifest | fenced, with the whole stream in its spool, `transferring` | `receiving` with the error | run it again: it sends the spool, disk rounds and all; or `--abort`, which restores the guest here from the spool |
| streamed: hull is killed after hvi's manifest, before its own seal | fenced, `transferring`, the spool without `sealed` | `receiving` | run it again or `--abort`: each checks the spool frame by frame and seals it, or has hvi stream the fenced guest again for the move's txid and epoch; with neither possible the error says no copy is left here |
| streamed: hull is killed after the destination verified the stream | fenced, `transferring`, no account of the stream on record | `restored-paused` | run it again: it adopts the fence from the spool and commits the destination's copy when its manifest is the fence's; or `--abort` |
| streamed: the destination lacks the base, or refuses the header | running, `transferring` | `receiving`, code `no_base` (or why) | before hvi's `migrate` starts the source waits until the destination's record says it took this attempt's header and copied the base (`streamAccepted`), so a refusal there comes while the guest runs. On `no_base` the source sends the base and tries once more by itself |
| commit refused (wrong epoch, aborted) | fenced, `committing` | `aborted` or other | `--abort` |
| the resume fails after the commit | `committed` (the destination owns it) | `running` with the error | `hull resume NAME` on the destination |
| `--abort` before the fence gets no answer | running, `aborted` | maybe `reserved` | `--abort` again releases the reservation |
| `--abort` after the fence gets no answer | fenced, unchanged | unknown | retry when the destination answers |
| `--abort` answered `running` (the destination committed, even if it has since moved the instance on or removed it) | fenced, unchanged | `running`, or only in the ledger | `hull migrate NAME --txid T` records the commit |
| `--abort` in `committing` answered `unknown_txid` | fenced, unchanged | no record of the move | find where the guest runs; see [Operator recovery](#operator-recovery) |
| the destination's cleanup after an abort fails | fenced, unchanged | `aborted`, its copy refused every verb but `rm` | `--abort` again |
| hull dies after recording the abort | `aborted`, checkpoint still `migrating` | `aborted` | `--abort` again restores it |
| the local restore after an abort fails | `aborted`, stopped, `fenced` checkpoint | `aborted` | `hull restore NAME` once hvi can |

## Operator recovery

- **Where is a move?** `hull migrate --status NAME` (or TXID) on the source
  prints the record, its history, the last error, what the state means and the
  destination's state. On the destination,
  `hull --store-dir DIR migrate-status --txid T` prints its record as JSON.
- **The source host is gone for good, before the commit.** The destination
  never runs the VM on its own. Abort it there with
  `hull migrate-abort --txid T`, which removes the received copy. The fenced
  checkpoint on the dead source was the only runnable copy.
- **The destination is gone for good, after the fence**, or in `committing`
  it answers that it has no record of the move (its store was lost or
  replaced). `--abort` cannot be confirmed, so it refuses. In `committing`,
  first make sure no host runs the guest: the commit may have run on the
  destination before its record was lost, and the guest may have moved on
  from there. If you know the destination will never come back,
  stop the source's fenced VMM, set the source `migration.json` state to
  `aborted` and the instance record's `migration.state` to `fenced` by hand,
  and `hull restore NAME`. This is the one recovery that relies on your word
  instead of the other host's.
- **A committed source.** `hull rm NAME` removes what is left. `hull migrate`
  of it is refused: move it back from where it runs.
- **Moving back.** A move from B back to A is a new move, run on B. A still
  has the stale copy the first move left under the same name, so the probe
  refuses until `hull rm NAME` on A removes it. The owner epoch goes on
  counting: 0 on A, 1 on B, 2 back on A. A streamed move back needs no
  `migrate-prepare` while A holds the base: A makes the spare and checks
  the base after the first move commits, a move back that comes first
  waits for that ([The way back](#the-way-back)), and `hull rm` leaves
  both.

## Testing a move

`HULL_MIGRATE_SSH` names a program to run instead of `ssh`, with the same
arguments. A script that runs its last argument with `sh -c` runs the
destination's verbs locally, so with `--remote-store` naming a second store a
whole move runs on one machine without a network:

```sh
#!/bin/sh
for a; do last=$a; done
exec sh -c "$last"
```

The unit tests (`cmd/hull/migrate_test.go`) run every step in one process
with the destination behind an interface that can drop a reply or answer for
it: a commit whose acknowledgement is lost, then the destination moving the
instance on or removing it before the source aborts, an abort before and
after the commit, a destination that answers `running` or `unknown_txid` to
an abort, a destination whose cleanup fails, an abort run again, a transfer
cut off midway, a stream whose reply is lost, a disk changed after the
fence. `TestMoveOverFakeSSH` runs the
destination as a separate hull process through that transport, with a fake
hvi that has no restore, and ends in "restore refused" and an abort.

Live runs are recorded in [Verified](#verified).

## Verified

2026-10-14, hull at this branch. Guest: alpine, block rootfs (15 GiB image,
about 13 MiB of it data), 512 MiB, 2 vCPUs, `--net none`, printing a counter
every second. This Mac has no sshd, so the Mac runs used the
`HULL_MIGRATE_SSH` stand-in with two stores; rp1 to rp2 used real ssh.

**Through the commit, Mac to Mac (HVF), hvi 4ea836f, without
`--allow-no-restore`**, run twice:

- `hull migrate` took 1.5 s: fence 300 to 308 ms (capture 269 ms, 56.6 of
  512 MiB RAM stored), stream 69.8 MiB in 0.39 to 0.40 s with 13.0 MiB of
  the disk, restore paused 352 to 362 ms, commit 59 to 64 ms.
- The guest went on counting on the destination (TICK 10, 11, 12, ...) and
  never printed its boot line there again. A file written and synced before
  the move read back on the destination with `hull exec`.
- Afterwards: the source stopped its fenced VMM; its record says
  `committed` and its checkpoint `migrated`; a new move of it and an abort
  were refused; the destination says `running`, owner epoch 1.
- Moving it back was refused while the stale copy was there, with the
  `hull rm` remedy; after `hull rm` the move back committed, the guest
  counted on at home (TICK 17, 19), the file was still there, owner epoch 2.

**Up to the restore, hvi 263713c (no restore subcommand)**, with
`--allow-no-restore`:

| | Mac to Mac (HVF) | rp1 to rp2 (KVM, Raspberry Pi 5) |
|---|---|---|
| Preflight without `--allow-no-restore` | refused: the source hvi has no restore | |
| Fence | capture 266 ms, 56.4 of 512 MiB RAM stored | capture 634 ms, 56.8 MiB |
| Counter after the fence | held (TICK 10 to 10 over 3 s) | held (TICK 11 to 11) |
| Stream | 69.6 MiB in 0.36 s | 69.5 MiB in 1.45 s (48 MiB/s) |
| Disk carried | 13.0 MiB of 15.0 GiB | 12.7 MiB of 15.0 GiB |
| Disk allocated, source / destination (`du`) | 151 / 13 MiB | 235 / 13 MiB |
| Bundle and disk on both ends | `cmp` and `diff -r` identical | sha256 identical |
| Destination's `hvi checkpoint-verify` | `complete: every part a restore needs is captured` | `partial: vCPU registers, interrupt controller and timer not captured` (H5 not done) |
| Restore on the destination | refused: no `restore` subcommand; destination `verified`, source `transferred` | the same |
| Plain `hull restore` of either side | refused by the move | refused by the move |
| `--abort` | destination removed the instance, kept the tombstone; source stopped its fenced VMM and tried `hull restore`, refused for the same reason | the same |

**A lost commit acknowledgement, Mac (HVF), hvi 4ea836f.** A stand-in for
ssh ran `migrate-commit` on the destination and then failed as a cut link
would, so the source never read the reply:

- The source stopped in `committing`, fenced (its VM `fenced`, its
  checkpoint `migrating`); the destination recorded `running` and the guest
  counted on there (TICK 12, 14).
- The destination then moved the guest on to a third store, which committed
  at epoch 2 and replaced the destination's `migration.json`. Its ledger
  still held the first move as `running`, and `migrate-status` answered
  from it. In a second run the destination's operator ran
  `hull rm --force` instead, with the same ledger answer.
- In both runs the source's `hull migrate --abort` was refused, saying the
  move committed there, and the source stayed fenced. `hull migrate NAME
  --txid T` then recorded `committed` and stopped the fenced VMM. After the
  move on, the third store went on counting (TICK 18, 21) with no boot line.

**An abort after the transfer, Mac (HVF), hvi 4ea836f.** The stand-in
refused `migrate-prepare` once, so the move stopped in `transferred`. On
the destination a plain `hull restore` was refused by the move. The abort
removed the destination's instance, left its ledger entry `aborted` and
recorded `destReleased` on the source. The source then restored the guest
from its fenced checkpoint, and it counted on (TICK 9 at the fence, then 13,
15) with no boot line. A late `migrate-commit` on the destination was
refused with `aborted`, and a second abort said "already aborted".

Not run live: an abort answered `unknown_txid`, a destination whose cleanup
fails, a transfer cut midway, a stream whose reply is lost and a disk
changed after the fence (the unit tests cover each), any gateway case, and
a move on KVM past the restore.

### The portable profile on this Mac

2026-10-14, hull at this branch, `hvi` at `feat/teleport` c580223 (built
from `git archive` with `cargo build --release`, signed ad hoc with
`hvi.entitlements`), HVF. The store held the default boot assets and the
portable kernel (sha256 `32725b9b...`) installed from
`HULL_PORTABLE_KERNEL`. Guest: alpine on a block rootfs, 512 MiB, 2
vCPUs, `--net none`, printing a counter every second. Each run below was
done twice, in the review of the profile commits and again after the
fixes that followed it; the numbers are the range of the two.

- **Run.** `hull run --hypervisor hvi --profile portable-arm64-v1
  --rootfs-type block --net none` passed `--control-sock ... --profile
  portable-arm64-v1` and recorded `"profile": "portable-arm64-v1"`. In the
  guest: clocksource `hvi-time`, kernel 6.12.95-00002, CPU part 0xd0b.
- **Same host, fence and restore.** `hull checkpoint --then fence` took
  426 to 433 ms (capture 398 to 408 ms, 80 to 81 of 512 MiB RAM stored).
  The counter held at TICK 10 for 3 s, and exec and resume were refused.
  After `hull stop`, `hull restore --detach` took 0.55 to 0.57 s, with no
  `--profile` on the restore argv. The guest went on (counter 19, then
  21) on `hvi-time`, a file written before the fence was there, and it
  printed no boot line.
- **A move between two stores.** Through the `HULL_MIGRATE_SSH` stand-in
  (see [Testing a move](#testing-a-move)), not real ssh. The destination's
  probe answered `hviProfile` true. A dry run with `--remote-hvi` naming a
  wrapper that rejects `--profile` was refused before the fence, and no
  record was written. `hull migrate` then ran through the commit in 2.2 s:
  fence 472 ms, transfer 639 to 643 ms (95.9 to 96.6 MiB), restore paused
  585 to 586 ms, commit 88 to 93 ms. On the destination the counter went
  on from 11 to 15 to 17 on `hvi-time`, the file was intact, and there
  was no boot line. The destination recorded that profile and owner
  epoch 1. After the fixes, the bundle's `profile.json` was also read on
  both stores: it named `portable-arm64-v1`. The source was stopped and `migrated`, and
  its `hull restore` was refused as stale.
- **The kernel rule** (after the fixes only). With `--profile`, a
  bootKernel annotation naming the default bundle's kernel was refused
  before the disk was built, with no instance left. The annotation naming
  the installed portable kernel, and naming a copy of it at another path,
  booted on `hvi-time`. `--no-boot-assets` was refused, and so was an
  `hvi` wrapper that takes `--profile` but not `--checkpoint-root`. A
  portable kernel changed by one byte was refused, and the refusal named
  `hull assets pull --profile portable-arm64-v1 --force`; after that
  command the next run booted.

Not run: real ssh, KVM, and a move between this Mac and a Pi.

### The portable profile on the Linux pair

2026-10-14, hull at this branch (f2a7a5c), between two Raspberry Pi 5
hosts (rp1, rp2: Linux 6.12.25, KVM with a vGICv2) over real ssh with a
key from each Pi to the other. Each store held the default boot assets
and the portable kernel (Linux 6.12.95 with `hvi-time`, sha256
`32725b9b...`) installed by `hull assets pull --profile
portable-arm64-v1` from `HULL_PORTABLE_KERNEL`. Guest: alpine on a block
rootfs, 512 MiB, 1 vCPU, `--net none`. Its init only sleeps; `hull exec`
starts a detached shell that counts in a variable and writes the count
to `/dev/shm/counter` every second, and writes a 5.5 KiB random file to
`/root/marker` with `sync`.

- **Run.** With `hvi` at `feat/teleport` f4594f0 (the first with
  `--profile` and the control flags), `hull run --hypervisor hvi
  --profile portable-arm64-v1 --rootfs-type block --net none` booted in
  0.9 s on hvi-time (`Switched to clocksource hvi-time`, the A76 CPU
  baseline, PSCI 1.1), recorded `"profile": "portable-arm64-v1"` and
  passed `--control-sock ... --profile portable-arm64-v1`. `hull pause`
  and `hull resume` worked. The same run on this Mac (HVF, `hvi` from the
  P1 to P3 series) booted on the software GICv2 with hvi-time at 24 MHz.
- **Probe.** A dry run to rp2 passed. With `--remote-hvi` naming an
  `hvi` without profiles (5aafb49), the probe refused before the fence:
  "its hvi ... has no machine profiles, so it cannot restore a
  portable-arm64-v1 instance".
- **Fence.** `hvi` f4594f0 refuses to checkpoint a portable guest ("no
  backend checkpoints yet"). `hull migrate` stopped at `reserved` in
  0.6 s with that reason, the guest kept running on rp1, and `--abort`
  released rp2's reservation.
- **Native, up to the restore** (`hvi` 5aafb49): fence 655 ms (capture
  649 ms, 55.5 of 512 MiB RAM stored), 70.8 MiB in 1.44 s (49 MiB/s)
  with 15.2 MiB of the 15 GiB disk, bundle sha256 identical on both
  hosts, rp2 `verified`. rp2's `hvi restore` refused ("the KVM restore is
  not built yet"); the move stopped at `transferred`, and the abort
  removed rp2's copy and tried the source's restore, refused the same way.
  Before f2a7a5c each of those two refusals took the full two-minute
  restore deadline (`hull migrate` 123 s); after it, 3.4 s, with hvi's
  reason in the error.

Not run yet: a portable move past the fence on the Linux pair, and a
portable move between this Mac and a Pi. The `hvi` branch at c580223
is meant to checkpoint and restore the portable profile on HVF and on
KVM; only HVF was run here, and its move on this Mac is in [The portable profile on this
Mac](#the-portable-profile-on-this-mac). This Mac's same-host move over
real ssh was not run either, as enabling Remote Login needs a password
for sudo.
