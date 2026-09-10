# Performance

This page holds hull's published measurements and the conditions they were
taken under. Read the limits section before you quote any number from it.

## What was measured, and on what

All numbers below come from one recorded run:

| | |
|---|---|
| Host | Mac Studio, model identifier `Mac14,13` |
| Host OS | macOS 26.3.1 |
| Guest | 2 vCPUs, 2048 MB RAM |
| Backends compared | `vz` and `qemu` with the HVF accelerator |
| Backend **not** measured | `hvi` |

The `hvi` backend does not appear in any table on this page. Nothing here
supports a claim about its throughput, its storage behavior or its boot time.

## Network throughput

| Test | `vz` | `qemu` + HVF |
|---|---|---|
| TCP transmit, guest to host | 25,043 Mbit/s | 3,057 Mbit/s |
| TCP receive, host to guest | 55,179 Mbit/s | 9,373 Mbit/s |
| Ping round trip | 0.29 ms | 0.28 ms |

These are the backend-native network paths, not the user-mode gateway. The
gateway moves every packet through a userspace netstack, so its numbers are
different and are not recorded here.

## Storage, ext4 block device

| Test | `vz` | `qemu` + HVF |
|---|---|---|
| Sequential write, 1M blocks | 15,419 MB/s | 10,631 MB/s |
| Random write, 4K blocks | 4,394 MB/s | 3,118 MB/s |
| Random read, 4K blocks | 9 MB/s | 23 MB/s |

Read these with care. The write figures are far above what the host's storage
device can sustain, so they describe writes landing in a cache rather than
writes reaching hardware. The cache state, the queue depth and the total
transfer size were not recorded, and without them the write rows cannot be
compared to a figure measured any other way. The read rows are the only ones
low enough to plausibly reflect device behavior.

## Boot time

| Metric | `vz` | `qemu` + HVF |
|---|---|---|
| Kernel start to init | about 60 ms | about 73 ms |
| DHCP lease complete | about 70 ms | about 164 ms |

**These measure one segment of a boot, not a boot.** "Kernel start to init" is
the time from the kernel getting control to the init wrapper running. It
excludes:

- pulling the image, which dominates any first run and depends on your network
- creating and mounting the store on a first run
- preparing the root filesystem, which differs per rootfs mode
- the runner process starting and the hypervisor setting up the machine
- your application becoming ready to serve

A 60 ms figure does not mean `hull run` returns in 60 ms. It does not.

The DHCP row is on the backend-native network path. On the gateway path,
addressing works differently; see [networking.md](networking.md).

## Limits of this data

- **One host, one run.** No repetitions, no variance, no confidence interval.
  A single sample on a single machine.
- **The commands are not recorded.** The tool, version, flags and workload
  behind each row were not written down with the results. That is why this page
  does not print commands: inventing them would misrepresent what was run.
- **The software versions are partly recorded.** The host OS is known. The hull
  version, guest kernel version and QEMU version at the time of measurement are
  not.
- **`hvi` is absent**, as noted above.
- **The gateway is absent.** Every network row is a backend-native path.

## What this does support

Two conclusions survive the limits above, both narrow:

1. On this host, on the backend-native network path, `vz` moved TCP
   substantially faster than `qemu` with HVF, in both directions.
2. On this host, `vz` reached init sooner than `qemu` with HVF, and completed a
   DHCP lease sooner.

That is a reason to prefer `vz` when you need throughput on the native network
path, which is one of several reasons `vz` is hull's default choice. It is not
a general statement that `vz` is the fastest backend for every workload, and it
says nothing at all about `hvi`. Pick a backend from the capability table in
[backends.md](backends.md) first, because a missing feature matters more than a
throughput ratio.

## Reproducing this

The numbers above predate a written harness, which is why they cannot be
reproduced command for command. If you want to measure your own host, the
end-to-end harnesses under [test/](../test/README.md) boot real VMs and are the
right starting point for a benchmark script. They need an Apple Silicon host
with working HVF, and they skip with a named reason and exit status 0 when they
cannot run. A zero exit from those harnesses means skipped, not passed.

New measurements are welcome as a pull request. Please record the host model,
both OS versions, the hull commit, the backend, the rootfs mode, the network
mode, the exact command with its flags, the units, the cache state and the
number of repetitions.
