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
> A `--profile portable-arm64-v1` instance runs on both OSes with the `hvi`
> at f4594f0, but that `hvi` refuses to checkpoint it, so a portable move
> stops at the fence (see [The portable profile on the Linux
> pair](#the-portable-profile-on-the-linux-pair)).

## Usage

```bash
hull migrate vm1 --to pm@rp2                       # move vm1 to rp2
hull migrate vm1 --to pm@rp2 --dry-run             # check both hosts, change nothing
hull migrate vm1 --to pm@rp2 \
    --remote-store /srv/hull --remote-hull /usr/local/bin/hull
hull migrate vm1 --txid 3f...e1                    # resume a move that stopped
hull migrate --status vm1                          # this host's record, and the other's
hull migrate --abort 3f...e1                       # end a move that has not committed
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

The instance must be an `hvi` instance on a block rootfs, with no shared
host directory, running or paused, started with a control socket. Both hosts
must be the same architecture, and for a native instance the same OS. An
instance started with `--profile portable-arm64-v1` (see
[backends.md](backends.md#machine-profiles)) may move between macOS and
Linux arm64, since its checkpoint holds the profile's machine rather than
the host's; the probe then also needs the destination's `hvi` to take
`--profile`, and refuses before the fence when it does not. Everything else
about the host is left
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

The plan listed the transfer before the fence. In this version the fence
comes first, because the bundle a move carries is the fenced checkpoint: hvi
has no pre-copy yet, so there is nothing to send until the guest has stopped.
Pre-copy (hvi H7) would send most of the RAM and the disk while the guest
runs, then fence and send what changed.

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

Not run yet: a portable move past the fence, on either OS. That needs an
`hvi` that checkpoints and restores the portable profile (P4b, and H5 for
KVM); this Mac's same-host move over real ssh was not run either, as
enabling Remote Login needs a password for sudo.
