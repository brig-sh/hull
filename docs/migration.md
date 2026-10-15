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
> 10 MiB/s to its disk, against 7.43 s for the whole bundle. See
> [Measured](#measured).

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

`--whole-bundle` asks for the old path. A hull from before streamed moves
lists no capabilities, so the two still move instances the old way. A hull
that lists only `stream-v1` or `stream-v2` (whose streams carried disk
blocks hull read itself, beside hvi's RAM rounds) gets the whole bundle,
and so does this hull from one of them: neither reads the other's stream.
An instance started by an older hull, whose `hvi` was not given its base,
moves as a whole bundle until it runs again under this hull; the
`Transport:` line says so.

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
or a `migrate-prepare` that received it ahead of a move
(`bases/received/<digest>`). `hull rm` removes the instance's base when
nothing else needs it. `hull prune` removes every base nothing needs, the
entries of images gone from the store, bases set aside as corrupt and
half-written ones left by a run or a prepare that died; `hull prune
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
| base | the base, as lz4-compressed non-zero blocks (`HULLBAS1`); the destination writes it to a temporary file, keeps it only if what arrived has the base's digest, and makes it read-only. It also keeps a spare copy (`bases/spare/<digest>.ext4`), made again by every prepare after a move took it, which a streamed receive takes as the instance's disk: on the Pis, without clones, copying the 1.6 GB claude-code base took 22 s, all of it before the stream could start. A copy that is not a clone is synced before it is kept |
| gateway | with `--remote-gateway-sock`, whether it answers. hull does not start one: brig starts the gateway with the session's policy |

Each step finds its effect already there and sends nothing, so the command
can run any number of times; the destination's probe lists the bases and
the profile kernels it holds. On rp2 the first run took 5.9 s (the image
pull 3.7 s, the 53 MB kernel 0.8 s, the base 110.8 MiB of data as 49.2 MiB
in 1.1 s) and the second 0.24 s, every step `held`.

The destination's own step of a move keeps the name: `migrate-prepare
--txid T` still brings a received checkpoint up paused. The prepare verbs
it runs there are hidden: `migrate-take-image`, `migrate-take-kernel` and
`migrate-take-base`.

### The order on the wire

One ssh session runs `hull migrate-receive --stream --txid T --name N`, and
its stdin carries hull's header, then hvi's `migrate` stream relayed frame
by frame as hvi writes it, then hull's trailer. hull drives no disk round
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
```

Integers are big-endian. An hvi frame is relayed with hvi's own header
line, checked by the source as it reads it from hvi and by the destination
as it passes it on; hvi checks it a third time, and its manifest over all
of them. `chain` is a running sha256 over every record's kind and header,
and `records` their number, so a record lost, added, moved or altered
fails the move even when each payload is intact. The reader holds every
length and name to limits before it allocates, and refuses a stream
without its trailer or with bytes after it. `hviReply` is hvi's reply to
`migrate`, with its numbers.

hull treats a frame named `ram-round-*` or `disk-round-*` as a round: bulk
data it relays and spools but never holds whole. Every other frame is a
document, and the documents of one stream are held to 64 MiB together.

### The destination

`migrate-receive --stream` takes the base's spare copy as the instance's
disk (or copies the base when there is none), syncs it, and records that
it took this attempt's header (`streamAccepted`); the source waits for
that before it asks hvi to `migrate`, so a refusal of the header comes
while the guest still runs. A copy whose writeback is still under way
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
kept in the move's checkpoint directory for the record.

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
  record, when the instance's record or the move's has it, and refuses a
  second stream of another fence, emptying the spool so no later check
  seals it. The instance's record and the move's then name the new
  manifest, and the move's keeps the first as `firstManifestSha256`.
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
with; any other manifest is refused and the source stays fenced.

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
  `verified` and `restored-paused` in one write. Its commit still makes
  four synced writes before the resume (the instance's epoch, the ledger,
  the record, and the instance's record of the running guest); on a disk
  whose 4 KiB `fsync` takes 2 s they cost seconds of downtime (see
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
| `roundsEndAt`, `pausedAt`, `sealedMs` | when hvi's last round with the guest running ended (`rounds_end_unix_ms`), when it paused the guest after the sync that follows (`precopy_end_unix_ms`), and how long from the pause to its seal |
| `finalDiskMs`, `finalDiskBytes` | `disk-round-final`: hvi's time for it and its raw bytes |
| `stopReason`, `hviPausedMs`, `diskSyncMs`, `pauseSyncMs` | from hvi's reply: why the running rounds stopped, the source's part of the downtime, the disk's sync before the pause and the one in it |
| `downtimeMs` | from `pausedAt` to the destination's resume, on two hosts' clocks; it counts the destination's commit writes twice, so it reads high |
| `chain` | the trailer's hash, which the destination's record must match |
| `hviReply` | hvi's reply to `migrate`, as hvi wrote it |
| `steps` | when this host reached each step from hvi's seal to the commit, by its own clock; the destination's record has its own (`trailer read`, `hvi restored the guest paused`, `base checked`) |

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
round 0 bytes in 0 ms; downtime about 102 ms`). A stream that ends before
the manifest prints `hvi did not fence NAME: ... the guest runs on here`.

### Measured

2026-10-15. hull at c04acb6 (the stream-v3 move), `hvi` c4131f5, the
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

Under a busy disk, an O_DIRECT writer (`dd oflag=direct`, 4 MiB blocks)
on both Pis, with the ubuntu writer. A 4 KiB `fsync` took 0.25 to 0.95 s
on rp1 and 2.0 to 8.2 s on rp2 during these runs.

| hull | rp1 to rp2 | rp2 to rp1 |
|---|---|---|
| 331bf8c (stream-v1) | 91.3 s down, 338.6 s total | 38.9 s down, 84.4 s total |
| d4ae775 (stream-v2) | 7.39 s down, 50.1 s total | |
| c04acb6 (stream-v3) | 10.98 s down, 96.8 s total (after the failed attempt below) | 0.66 s down, 19.9 s total |

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

Both times the guest went on with no gap in its counter and the same
`boot_id`, blob and open file.

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
| `timingsMs` | milliseconds per step: `fence`, `transfer`, `prepare`, `commit`, `restore`, `resume` |
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
| streamed: hvi's stream ends before its manifest (a refusal, a failed capture or park, a reader hvi cut for stalling) | running, unfenced, `transferring`; the error says the guest runs on here | `receiving` with the error | run it again: it streams from the start; or `--abort`, which leaves the guest running |
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
  counting: 0 on A, 1 on B, 2 back on A.

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
