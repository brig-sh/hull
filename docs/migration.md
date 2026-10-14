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

> **Streamed moves, 2026-10-15.** With an `hvi` that streams its
> checkpoint (`feat/teleport-fast` merged with `feat/teleport-fast-disk`,
> built as ef8833a in a scratch clone), `hull migrate` sends the guest's
> RAM in pre-copy rounds while it runs and only the disk blocks that differ
> from its base. Between rp1 and rp2 the 1 GiB ubuntu guest moved in 5.4 s
> and 5.7 s, and was stopped for 279 ms and 340 ms as the guest's own clock
> counts it; the whole-bundle move of the same guest on the same hosts and
> binaries stopped it for 7.43 s. See [The streamed move](#the-streamed-move).

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

- an `hvi` here of checkpoint format 2 (the `checkpoint_format` in the
  instance's machine profile), which has the `migrate` op;
- a hull there whose probe lists the capability `stream-v1`, and an `hvi`
  there that takes `restore --from-stream`;
- a base of the instance's disk in this store (below). The destination
  need not hold it yet: the move sends it before the guest stops.

`--whole-bundle` asks for the old path. A hull from before streamed moves
lists no capabilities, so the two still move instances the old way.

### Bases

A base is the disk image exactly as `hull run` built it, before the guest
first wrote to it. `hull run` keeps one for every `hvi` instance on a block
rootfs that has a control socket: it clones the new image (APFS
`clonefile` on macOS, `FICLONE` on Linux, else a copy that keeps the
holes) into `<store>/bases/`, hashes the copy, makes it read-only and names
it `<digest>.ext4`. The digest is recorded in `state.json` (`base`) and
passed to an `hvi` that takes `--disk-base-sha256`, which records it in
its profile and in `disk.json`. On the Pis keeping the 111 MiB ubuntu base
took 0.45 s of `hull run` (a copy: ext4 has no reflink).

The disk digest is the sha256 of `HULLDISK1\0`, the image's size and the
block size (64 KiB, big-endian u64 each), then, for every 64 KiB block on
an absolute boundary that holds a byte that is not zero, the block's index
(u64) and its bytes; the last block may be short. A hole and a block of
written zeros hash alike, so a clone, a sparse copy and a received copy of
one image have one digest. It is not the image's plain sha256, which would
have to read every hole.

`mke2fs` writes a fresh UUID and timestamps into every image it builds, so
two runs of one OCI image get two bases. A base therefore serves one
instance and the moves of it, not every instance of an image. Nothing
removes a base yet: `hull rm` and `hull prune` leave `<store>/bases` alone.

### Preparing a destination

```bash
hull migrate-prepare vm1 --to pm@rp2 [--remote-store DIR] [--remote-hull PATH] [--json]
```

puts on the destination, while the guest keeps running, what a streamed
move then does not carry:

| Step | What it does |
|---|---|
| image | `hull pull REF` there by the reference this store pulled it by, unless that store has the digest. The move does not need it; a later `hull run` there does. A failure is reported and fails nothing |
| profile kernel | for a `--profile` instance, this store's kernel of that profile, unless the destination's record has the same sha256. It is checked against the sha256 and installed through the same import as `HULL_PORTABLE_KERNEL`, from a copy kept under `bases/kernels/` |
| base | the base, as lz4-compressed non-zero blocks (`HULLBAS1`); the destination writes it to a temporary file, keeps it only if what arrived has the base's digest, and makes it read-only |
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
its stdin carries, in this order:

1. The disk rounds while the guest runs. Round 0 is every block that
   differs from the base: hvi's blocks written since boot when the running
   `hvi` booted from the base or names it in its profile (an `hvi` that
   does carries those blocks across a restore), otherwise every block of
   the disk compared with the base. Rounds 1 to 4 are hvi's blocks written
   since the round before, read with `disk-dirty` and `disk-blocks`, until
   a round finds at most 32 blocks (2 MiB). Without hvi's rounds there is
   only round 0.
2. hvi's `migrate` stream, relayed frame by frame as hvi writes it: RAM
   rounds while the guest runs, then the pause, the last RAM round, the
   machine's documents and the manifest. hvi fences the guest once the
   manifest is written to its socket.
3. The last disk round: hvi's blocks written since the last round, read
   once hvi has fenced the guest and synced its disk; or, without hvi's
   rounds, the whole disk compared with what the destination holds.
4. The trailer.

hvi serves one control connection at a time, so hull cannot read disk
rounds while `migrate` holds the socket: the last disk round is read after
the seal, not inside the pause. The guest never runs again after the
pause, so that round and the RAM still describe one machine. It also means
the `disk.json` hvi writes inside the pause does not list the last round,
so the destination's restore is not given `--disk-rounds`.

### The format

```
"HULLMIG2"
'H' u32 len  {"txid", "name", "epoch", "baseSha256", "diskSize", "blockSize": 65536, "replay"?}
'D' u32 len  {"round", "final"?, "first", "count", "codec": "lz4"|"raw", "rawSize", "size", "sha256"}
             then size bytes: count adjacent blocks from first, lz4 when smaller
'C' u32 len  hvi's frame header line, as hvi wrote it, then its bytes
'T' u32 len  {"records", "chain", "hviReply"}
```

A disk batch's sha256 is of its raw bytes and is checked before a byte is
written. An hvi frame is relayed with hvi's own header line, checked by the
source as it reads it from hvi and by the destination as it passes it on;
hvi checks it a third time. `chain` is a running sha256 over every record's
kind and header, and `records` their number, so a record lost, added,
moved or altered fails the move even when each payload is intact. The
reader holds every length, index and name to limits before it allocates,
and refuses a stream without its trailer or with bytes after it.

### The destination

`migrate-receive --stream` copies the base to the instance's disk, applies
each disk batch to it, and starts `hull restore --detach --paused --move T`
with a pipe as its stdin once `profile.json` has arrived and matched the
move's machine profile; that restore runs `hvi restore --from-stream -`,
which reads the pipe. RAM frames go to hvi as they arrive, so no RAM
reaches the destination's disk. The machine's documents and the manifest
are held until the trailer has checked out and the disk is synced; only
then does hvi get them, check its manifest and build the machine over the
disk, which is complete by then. The move is `verified`, and the guest
`restored-paused`, when both hull and hvi are done. The documents are kept
in the move's checkpoint directory for the record.

### Failures and the spool

The source keeps hvi's stream as hvi wrote it in
`<instance>/checkpoint/<txid>/stream`, with the documents beside it and a
`sealed` file once it is complete and synced. It reads hvi's stream to its
end whatever the destination does: hvi fences the guest as soon as its
last bytes are in the socket's buffer, and a fenced source must hold a
whole copy to restore from. So:

- a stream that fails before hvi seals it leaves the guest running here,
  and running the move again streams it from the start;
- one that fails after leaves the guest fenced with its spool recorded as
  the instance's checkpoint (`migrating`). Running the move again sends the
  spool (`"replay": true`) and the disk compared with the base; `--abort`
  restores the guest here from the spool with `hvi restore --from-stream`.

The spool and the destination's disk are synced in the background every
32 MiB, and the source syncs the spool's tail while the last disk round and
the trailer go out, so the guest does not wait on a slow disk. The source
pays for the spool with a write of the guest's touched RAM to its own disk.

A destination whose `hvi` refuses the machine stops reading, and the
refusal is the move's error, with code `restore_refused`.

### Numbers

`migration.json` on the source gains `stream`:

| Field | Meaning |
|---|---|
| `mode` | `live`, or `replay` for a spool sent again |
| `diskEngine` | `hvi` (hvi's rounds) or `compare` (hull compared the disk with the base) |
| `rounds` | each disk round, RAM round and document: `kind`, `name`, `blocks`, `bytes`, `wireBytes`, `seconds`, and for a disk round after the first `dirtyPerSec`, the blocks the guest wrote per second during the round before |
| `diskBytes`, `diskWireBytes`, `ramBytes`, `stateBytes`, `wireBytes`, `seconds` | totals |
| `pausedAt`, `sealedMs` | when hvi's pre-copy rounds ended and how long until it sealed, from hvi's reply |
| `finalDiskMs` | the last disk round |
| `downtimeMs` | from `pausedAt` to the destination's resume, on two hosts' clocks |
| `chain` | the trailer's hash, which the destination's record must match |
| `hviReply` | hvi's reply to `migrate`, with its own rounds, dirty rates and pause |

Progress goes to stderr, one line per round prefixed `move: `, and every
two seconds of a long round.

### Measured on the Linux pair

2026-10-15, hull built at 1ecb5c8 (it reports that commit; folding two
fixups into an earlier commit of this series made it 81e118b, with the
same tree) on rp1 and rp2 (Raspberry Pi 5, Linux 6.12.25,
KVM), `hvi` ef8833a (`feat/teleport-fast` 3396415 merged with
`feat/teleport-fast-disk` 4a4cb3d in a scratch clone; the two conflicted
in the control ops and `disk.json`, resolved by keeping both), over real
ssh. Guest: `docker.io/library/ubuntu:latest` on a block rootfs, `--profile
portable-arm64-v1`, 1 GiB, 1 vCPU, `--net none`, running a shell that
writes 32 MiB of random data to `/root/w/blob`, holds a deleted file open
on fd 4 and writes a counter to the disk five times a second, syncing every
fifth. After every move the counter went on, the guest's `boot_id`, the
blob's sha256 and the deleted file's contents were unchanged, and no boot
was logged. Downtime is what the guest's `hvi-time` driver logged for each
move ("wall clock moved"); hull's own estimate (`downtimeMs`) was within
10 ms of it.

| Move | Path | Total (`hull migrate`) | Guest stopped | Sent |
|---|---|---|---|---|
| rp1 to rp2 | streamed | 5.4 s | 279 ms | 109.5 MiB: disk 287 MiB as 34.6 MiB, RAM 74.9 MiB in 3 rounds |
| rp2 to rp1 | streamed | 5.7 s | 340 ms | 109.3 MiB |
| rp1 to rp2 | `--whole-bundle` | 8.2 s | 7.43 s | 288.2 MiB in 5.1 s (fence 1.29 s, restore 0.92 s) |

For comparison, B4's idle move of the same guest with hvi 46a18dc stopped
it for 6.51 s (reserved to `running`), sending 226.8 MB in 3.45 s. U3's
move never got past the restore on KVM.

Two earlier runs show what the disk can cost. Before 81e118b the source
synced its spool before the last disk round, and on rp2, whose NVMe was
slowed by another project's builds (the kernel logged an NVMe abort), a
move back to rp1 kept the guest stopped 16.8 s, 16 s of it in that sync.
The abort of a move whose destination refused the restore brought the
guest back on rp1 from the spool, as far as hvi's own check: that `hvi`
was built without its commit, which every restore refuses.

## Records

Both hosts keep `<store>/instances/<name>/migration.json`, 0600:

| Field | Meaning |
|---|---|
| `version` | 1 |
| `txid`, `role` (`source` or `destination`), `name`, `epoch`, `state`, `peer` | the move |
| `remote` | source only: `target`, `store`, `hull`, `hvi`, `gatewaySock`, `allowNoRestore` |
| `destReleased` | source only, once aborted: the destination confirmed it keeps nothing of the move |
| `bundleDir`, `manifestSha256` | the fenced checkpoint, on either host |
| `disk` | `path`, `size`, `modNs` (at the fence), `sha256`, `dataBytes` |
| `transfer` | `bytes`, `seconds`, `bytesPerSec`, `startedAt` |
| `spec` | destination only: what the source asked it to build (MAC, IP, sandbox id, network) |
| `verify` | destination only: `hvi checkpoint-verify`'s verdict |
| `timingsMs` | milliseconds per step: `fence`, `transfer`, `prepare`, `commit`, `restore`, `resume` |
| `transport` | `stream` for a streamed move, absent for the whole bundle; chosen at preflight and kept for retries |
| `transportWhy` | why the move took the whole bundle |
| `baseSha256` | the base a streamed move's disk blocks apply to |
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
| streamed: the stream fails before hvi seals it | running (hvi resumes it), `transferring` | `receiving` with the error | run it again: it streams from the start; or `--abort` |
| streamed: the stream fails after hvi sealed it | fenced, with the whole stream in its spool, `transferring` | `receiving` with the error | run it again: it sends the spool and the disk compared with the base; or `--abort`, which restores the guest here from the spool |
| streamed: the destination lacks the base | running, `transferring` | `receiving`, code `no_base` | the source sends the base and tries once more by itself |
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
