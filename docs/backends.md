# Backends

hull has three hypervisor backends. They are not three implementations of one
feature set. Each uses a different interface, carries a different device model,
and is missing things the others have.

## How a backend gets chosen

hull resolves the backend in this order:

1. the `--hypervisor` flag
2. the image's `com.urunc.unikernel.hypervisor` annotation
3. `qemu` on macOS, `hvi` on Linux

That last step is worth knowing. If you pass no flag and the image carries no
annotation, hull runs `qemu`, which hull does not install and which cannot boot
a plain container image. So `hull run ubuntu:latest` with no flag fails, while
`hull run --hypervisor vz ubuntu:latest` works. **Pass `--hypervisor`
explicitly** unless you know the image annotates itself.

`hull compose` is the exception: a service with no `x-hypervisor` defaults to
`vz`, not `qemu`.

`qemu-hvf` is accepted as a synonym for `qemu`, on the flag, in an image
annotation, and in compose `x-hypervisor`.

## Choosing one

Start with `vz`. Move to another backend only for a reason on this list.

| Choose | When |
|---|---|
| `vz` | the default. You want NAT with no setup, a GUI window, Rosetta, or a checkpoint you can restore any number of times |
| `hvi` | you want a persistent writable root without a disk image, correct Linux file modes over a share, the same device model hull's guests get on Linux, or pause and a checkpoint bundle |
| `qemu` | you need a device or a machine option only QEMU has |

## Capability comparison

| | `vz` | `hvi` | `qemu` | Linux `hvi` |
|---|---|---|---|---|
| Interface | Virtualization.framework | Hypervisor.framework, called directly | Hypervisor.framework, via QEMU's `-accel hvf` | KVM |
| Entitlement the runner needs | `com.apple.security.virtualization` | `com.apple.security.hypervisor` | `com.apple.vm.networking`, and only for `--net shared` | none; read-write `/dev/kvm` |
| Ships with hull | yes, as `vz-runner` | yes, as `hvi` | no, `brew install qemu` | no release yet, see [build.md](build.md#linux) |
| Guest architecture | arm64 | arm64 | arm64 | arm64 |
| Device transport | Virtualization.framework's | virtio-mmio, no PCI | QEMU's | virtio-mmio, no PCI |
| Rootfs: shared directory | virtiofs | virtiofs | 9pfs | **refused** |
| Rootfs: block | ext4 image | ext4 image | ext4 image | ext4 image, the only mode |
| Boots a plain image carrying no kernel | yes | yes | **no** | yes |
| Read-only `--shared-dir` | yes | yes | **refused** | **refused** |
| `--net shared` | Apple NAT, no extra privilege | **no TCP egress**, see below | vmnet, needs root or a signed QEMU | **refused** without `--gateway-sock` |
| `--gateway-sock` | yes | yes | yes | yes |
| `exec` | yes | yes | yes | yes |
| Checkpoint and restore | yes, with a block rootfs | checkpoint with a block rootfs and no shares, into a [partial bundle](checkpoint-restore.md#what-a-bundle-holds); **no restore yet** | **no** | as on macOS |
| Pause and resume | **no** | yes | **no** | yes |
| Machine profile (`--profile`) | **refused** | `native`, or `portable-arm64-v1` with a block rootfs, no shares and the [profile's kernel](images.md#a-profiles-kernel); needs an `hvi` that takes `--profile` | **refused** | as on macOS |
| `--gui`, `--gui-title` | yes | no | no | no |
| `--rosetta` | yes | no | no | no |

The last column is hull built for Linux, which runs only `hvi`; see
[`hvi` on Linux](#hvi-on-linux).

Read the gaps as gaps. Three backends existing does not make them
interchangeable, and hull does not emulate a missing feature on a backend that
lacks it.

## `vz`

`vz-runner` is a Swift program that uses Apple's Virtualization.framework
(`VZVirtualMachine`). It does not call Hypervisor.framework itself.
Virtualization.framework supplies the device model, the NAT attachment and the
save and restore calls checkpointing is built on.

**Host requirements.** `vz-runner` declares a macOS 14 deployment target and
needs Swift tools 5.9 to build. Nothing in the Go or Swift source gates the
runtime on a macOS version, so an earlier macOS can work and is untested.
macOS 26 is what CI runs.

**Entitlements.** Two plists exist. `Entitlements-novmnet.plist` grants only
`com.apple.security.virtualization`, and the build uses it by default. The
release workflow signs with it and then asserts the entitlement is present on
the shipped binary. `Entitlements.plist` also grants
`com.apple.vm.networking`, which the current runner source never exercises:
there is no bridged-network path in it, and Apple NAT needs nothing beyond the
virtualization entitlement.

**Networking.** `--net shared` gives Apple NAT through
`VZNATNetworkDeviceAttachment`. The guest gets a DHCP lease on the
`192.168.64.0/24` subnet with `192.168.64.1` as the gateway, and the init
wrapper copies the DNS server out of `/proc/net/pnp` into `/etc/resolv.conf`.
No extra entitlement and no root.

**Rosetta.** `--rosetta` runs an amd64 **userspace** under Apple's translator.
The guest kernel stays arm64. There is no x86 kernel here. `vz-runner` exposes
the translator as an extra virtiofs device tagged `rosetta`, which the guest
registers with `binfmt_misc`.

Four conditions must hold before the Rosetta path starts, and hull checks all
four:

1. the backend is `vz`
2. the rootfs mode is virtiofs, not block and not initrd
3. the image's init is `urunit`
4. the image ships an executable static arm64 `busybox` at `/.rosetta/busybox`

`vz-runner` does not install Rosetta. If it is missing, the run fails with a
named reason telling you to run `softwareupdate --install-rosetta` once.

`--platform` and `--rosetta` are not the same switch. `--platform` only selects
the image platform to pull, and defaults to `linux/arm64`. Passing `--rosetta`
without an explicit `--platform` defaults the pull to `linux/amd64`; an
explicit `--platform` still wins. The image annotation
`com.urunc.darwin.rosetta` also turns the path on, but it cannot change the
pull platform, because the platform is decided before the image is pulled.

**Stopping.** `hull stop` sends `SIGTERM` to `vz-runner`, which asks the guest
to shut down and forces a stop after `--stop-grace` seconds, 10 by default. A
second signal forces the stop at once.

**Checkpoint and restore.** Only with a block rootfs. A virtiofs root goes
stale across a restore, so hull refuses the combination. `hvi` checkpoints
too, by a different mechanism and with different rules. See
[checkpoint-restore.md](checkpoint-restore.md).

One operational note, recorded from CI rather than derived from code: machine
state saves have never succeeded on a runner without a console session, at 0
successes in 4 attempts on one machine against 15 in 16 on another, while
ordinary boots on the same machine pass. SIP state is not the discriminator.
CI pins the checkpoint job to a runner with a console session. Why a save needs
a console session is unresolved.

## `hvi`

`hvi` is a Rust program that calls Hypervisor.framework directly through the
`applevisor` crate. It allocates guest RAM itself as a POSIX shared-memory
object, owns the vCPUs, and implements its own virtio device model. That is
what lets brig give a workload the same devices on macOS and on Linux.

hull consumes `hvi` as a git submodule pinned to a commit, not as a released
version. The facts on this page come from that pinned commit.

**Host requirements.** Apple Silicon. On macOS the only guest architecture is
arm64, booted as an unmodified Linux `Image` with a devicetree. There is no
firmware or bootloader stage, and no x86-64 guest on macOS. Building `hvi`
needs the Rust toolchain its submodule pins, currently stable 1.95.0. That pin
is the build toolchain, not a minimum: the manifest declares a 1.77 floor, and
the two are separate contracts.

**Entitlement.** `com.apple.security.hypervisor`, and nothing else.
`com.apple.vm.networking` is deliberately absent, and the entitlements file
says why: virtio-net uses a user-space stack, a gvisor-tap relay or a Linux
tap, and never touches vmnet.

**Devices.** virtio-mmio only, no PCI anywhere. On macOS the model is
virtio-blk, virtio-net, virtio-vsock, virtio-fs and a PL011 serial console.
No hotplug. `hvi` takes exactly one disk, so a workload needing two block
devices cannot be expressed.

**Rootfs.** For a generic container boot, hull defaults `hvi` to virtiofs and
passes it as a **writable** share. Before boot, hull replaces the instance
bundle's rootfs symlink with an APFS copy-on-write clone of the unpacked image
directory. Writes land in that per-instance clone, and the cached image stays
unchanged.

That path has one hard requirement: the image store and the instance directory
must be on the same APFS volume. hull refuses the boot otherwise. There is
deliberately no plain-copy fallback, so the guarantee cannot degrade quietly
into a free-space requirement.

`hvi`'s virtio-fs reads Linux mode, uid, gid and device numbers from a private
host xattr under `com.nofire.hvi.` rather than from the host inode. That is why
`hvi` preserves file modes a plain macOS share cannot express. See
[storage.md](storage.md).

**Networking. Read this before you run a server on `hvi`.**

`--net shared` on `hvi` does not give the guest egress. `hvi`'s built-in stack
is a user-space responder inside the VMM. It answers ARP, ICMP echo, DHCP and
DNS for a fixed address set, guest `10.0.2.15`, gateway `10.0.2.2`, resolver
`10.0.2.3`, and it **drops guest TCP after logging it**. `hvi`'s own
documentation lists "no egress from the built-in network stack" among its known
limits.

There is a second problem with that built-in stack. Its DNS handler resolves
names by calling the host's `getaddrinfo`, and it runs after `hvi` installs its
Seatbelt sandbox. That profile is deny-by-default with no network grant, and
`hvi`'s own selftest asserts that opening an outbound socket fails after the
sandbox is entered. So built-in DNS is expected to return an empty answer
unless the sandbox is off, which hull never turns off.

For real egress on `hvi`, use the gateway:

```bash
hull run --hypervisor hvi --net shared \
  --gateway-sock /path/to/gateway.sock --gateway-cidr 10.87.0.10/24 IMAGE
```

hull dials the gateway socket before the run and fails with a clear error if
nothing is listening. That check matters, because `hvi` on its own only logs a
warning when it cannot reach the gateway and falls back to the built-in
no-egress stack. Without hull's pre-flight, a run would look networked and
carry no traffic.

`--net-tap` and `--net-mac` are Linux-only in `hvi`. On macOS the tap flag is
refused with an error naming the gateway flag, and the MAC flag has no effect.

**Exec** works. `hvi` stands up a host Unix listener and bridges it over
virtio-vsock to an in-guest agent, and hull wires that up on every run.

**Pause, checkpoint and restore** go through the control socket hull gives
every `hvi` run (`--control-sock`, with the machine profile at
`--profile-out`). `hull pause` and `hull resume` stop and restart the I/O
threads and every vCPU. `hull checkpoint` writes a bundle, then continues or
fences the guest. Today that bundle is partial: RAM and device state, without
the vCPU, interrupt controller and timer state a resume needs. `hull restore`
would restore a fenced bundle onto the disk the guest stopped with, and
refuses until `hvi` has a `restore` subcommand, which none has yet. `hull
stop` asks over the socket before it signals. The `hvi` at hull's submodule
pin predates the socket; hull asks each `hvi` before a run and leaves the
flags off one that does not take them, so it boots without pause or
checkpoint. See [checkpoint-restore.md](checkpoint-restore.md#hvi).

### Machine profiles

`hvi` boots one of two machines. `native`, the default, shows the guest the
host's own interrupt controller, timer and CPU, so a checkpoint of it
restores only on the same kind of host. `portable-arm64-v1` shows the guest
the same machine on an Apple host (Hypervisor.framework) and on a Linux
arm64 host (KVM): a GICv2, a Cortex-A76 identity with a reduced feature
set, and the `hvi-time` device for its clocks and timers in place of the
architected timer, whose rate differs between the two (24 MHz on Apple,
54 MHz on a Raspberry Pi 5). `hvi`'s `docs/portable-profile.md` is the
contract.

```bash
HULL_PORTABLE_KERNEL=/path/to/Image hull assets pull --profile portable-arm64-v1
hull run --hypervisor hvi --profile portable-arm64-v1 --rootfs-type block IMAGE
```

hull records the profile in `state.json` (`profile`), passes `--profile` to
`hvi`, and boots the profile's kernel with the default initrd; `hvi` adds
the kernel arguments the profile needs. A native run passes nothing new, so
an `hvi` from before profiles still boots it.

The kernel is checked by its contents, whatever names it. When hull finds
the kernel itself, it takes the profile's. When a
`com.urunc.unikernel.bootKernel` annotation names it (as brig does), that
file must hash to the kernel recorded in `hull assets dir --profile
portable-arm64-v1`, so a copy of that kernel at another path boots and any
other kernel is refused. The check runs before the disk is built.

hull refuses the profile:

- on `vz` and `qemu`;
- without `--rootfs-type block` (on macOS an unset `--rootfs-type` is
  virtiofs for `hvi`, and is refused rather than switched);
- with `--shared-dir` or `--shared-dir-fd`, and for an image that boots from
  its own initrd: none of these lives on the disk a checkpoint carries;
- with an `hvi` that does not take `--profile` (hull asks it, as it asks
  about the control flags). That `hvi` would boot the native machine;
- with an `hvi` that takes `--profile` but has no control socket
  (`--control-sock`): that instance could never be checkpointed or moved;
- with a kernel that is not the profile's: a bootKernel annotation that
  hashes to another kernel, a kernel from the image, or `--no-boot-assets`,
  which leaves the image to supply one.

The annotation `com.nofire.hvi.profile` selects the profile too, but only
from `--annotation`, never from the image: like the boot kernel, the
profile decides what host kernel boots. `hull restore` and a move take the
machine from the checkpoint's own profile, so `hvi restore` gets no
`--profile`. A move carries the profile to the destination's record, and a
portable instance may move between macOS and Linux (see
[migration.md](migration.md), and its runs in [The portable profile on this
Mac](migration.md#the-portable-profile-on-this-mac) and [on the Linux
pair](migration.md#the-portable-profile-on-the-linux-pair)); a native one
only to the same OS.

**Other limits `hvi` states about itself.** Eight vCPUs on a GICv2 arm64 Linux
host, which does not apply on macOS because Apple's in-kernel GIC is a GICv3.
One disk and one NIC, no hotplug, no PCI. virtio-fs on macOS only. It has had
no external security audit. The boundary it is built to hold is the one around
the guest; it does not claim to defend against a guest that hangs its own VM,
or against CPU side channels.

**No clock device.** `hvi` has no emulated RTC on macOS, so hull seeds the
guest clock once at boot by writing the host epoch into the boot initrd. A
long-running `hvi` guest has no host time source to correct drift against.

## `hvi` on Linux

hull also builds for arm64 Linux, where it runs `hvi` on KVM and nothing else:
`hvi`, a block rootfs, and either no network or a gateway. That is the
configuration a sandbox needs to be moved between two KVM hosts. `pause`,
`resume` and `checkpoint` work as on macOS, and `restore` is refused as on
macOS until `hvi` has one (see
[checkpoint-restore.md](checkpoint-restore.md#hvi)); moving a sandbox to
another host is planned and not built yet. brig drives it with the same verbs
it uses on macOS: `run`, `exec`, `ps`, `inspect`, `logs`, `stop`, `rm`,
`assets dir` and `network-gateway`. `pull`, `images`, `rmi`, `prune`, `telemetry` and `version`
work too.

A sandbox started on a Mac cannot be moved to Linux, and none of the planned
work changes that. A guest reads its counter frequency once, at boot, and
KVM cannot change the one a host gives it: 24 MHz on Apple silicon, 54 MHz on
a Raspberry Pi 5. The interrupt controllers differ too (GICv3 under
Hypervisor.framework, a GICv2 on the Pi), and so do the CPU ID registers.

What is different from macOS:

- **The backend is always `hvi`.** With no `--hypervisor` flag and no
  annotation, hull on Linux picks `hvi`, not `qemu`, because nothing else is
  offered. Any other `--hypervisor` is refused before the image is pulled. An
  image whose `com.urunc.unikernel.hypervisor` annotation names another
  backend is refused once it is pulled; `--hypervisor hvi` runs it on `hvi`
  regardless.
- **The rootfs is always a block image.** `--rootfs-type` defaults to `block`;
  `virtiofs` and `9pfs` are refused, and so are `--shared-dir` and
  `--shared-dir-fd`. `hvi`'s KVM backend has no virtio-fs server: it accepts
  the share flags and serves nothing, so a guest would boot without them.
- **The network is the gateway or nothing.** `--net none`, or `--net shared`
  with `--gateway-sock` and `--gateway-cidr`. A bare `--net shared` is refused:
  on macOS it means vmnet, and on Linux it would be `hvi`'s stub NIC, which
  forwards nothing. `hull network-gateway` is the same gvisor gateway as on
  macOS, with the same egress rules.
- **The `hvi` command line is hull's own.** urunc's Linux `hvi` monitor is
  written for the containerd shim and passes no `--agent-sock`,
  `--net-gateway` or `--net-mac`, so `exec`, the gateway and per-guest DHCP
  leases would not work. hull builds the argv itself, the same as urunc's
  darwin builder minus the shares, and a test fails if the two drift.
- **`hvi` is found beside hull first**, then on `PATH`, as on macOS.
- **The store is a plain directory**, created 0700. There is no sparse image
  and no `hull store`. hull refuses a store directory on a case-insensitive
  filesystem.
- **e2fsprogs comes from the distribution.** hull looks for `mke2fs`,
  `debugfs` and `e2fsck` in `/usr/sbin`, then `/sbin`, then `PATH`. Debian and
  Raspberry Pi OS keep them in `/sbin`, which an ordinary login's `PATH` does
  not include.
- **Image ownership is recorded on unpack.** Run as an ordinary user, hull
  cannot chown the files it unpacks, so it records each file's uid, gid and
  mode in a `user.com.nofire.hvi.linux-attr` xattr, as macOS does in its own
  namespace, and writes them into the block image, then removes the xattr
  from the image. Symlinks cannot hold a `user.` xattr on Linux, so their
  owners go in `rootfs.link-owners.json` beside the image's rootfs. On the
  host, unpacked files keep owner read and directories owner rwx even where
  the image asks for less (Fedora's `/etc/shadow` is 0000), so mke2fs can
  read them and `hull rmi` can delete them; the image's own mode is what the
  guest sees. Run as root, the files carry the real owners and modes and
  nothing is recorded.
- **No pull inside a single-id user namespace.** Under `unshare -r`, hull is
  uid 0, so it chowns as root would and the chown fails with `invalid
  argument` on the first file owned by an id the namespace does not map
  (`/etc/shadow`, gid 42, in alpine). The pull stops there. Run hull as an
  ordinary user or as real root instead.
- **Not on Linux:** `compose`, `store`, `--gui` and `--rosetta`.
  `checkpoint` and `restore` take `hvi` instances only, which on Linux is
  every instance.

The user running hull needs read-write access to `/dev/kvm`, usually through
the `kvm` group. A group added to a logged-in user reaches only new sessions.

Tested on a Raspberry Pi 5 (Cortex-A76, KVM with a GICv2) running Raspberry Pi
OS with kernel 6.12, using the `linux-arm64` boot assets.

## `qemu`

`hull` builds a `qemu-system-aarch64` command line using `-accel hvf`. QEMU is
not installed by hull:

```bash
brew install qemu     # 7.2 or later
```

`--qemu-path` names a specific binary. Without it, hull auto-detects one and
prefers a signed copy.

hull runs QEMU as an ordinary child process. It prints the whole command line
to stderr as `Starting VMM: ...`, which is the quickest way to see what it
actually built. The skeleton is the same on every run: `-M virt`, `-cpu host`,
`-accel hvf`, `-display none`, `-vga none`, `-monitor null`, plus `-m` and
`-smp`. The backend refuses to start on anything but Apple Silicon.

`-accel hvf` means QEMU keeps its own userspace device model and asks
Hypervisor.framework only for CPU and memory virtualization. That is a
different arrangement from `vz`, where Virtualization.framework supplies both,
and from `hvi`, which implements its own device model against the framework
directly.

**It cannot boot a plain container image.** hull fetches the generic boot
assets for `vz` and `hvi` only, and refuses the boot-kernel annotations on any
other backend with `generic virtiofs container boot requires the vz or hvi
backend`. On `qemu` the image must carry its own kernel.

**Rootfs.** 9pfs is the default, because macOS has no virtiofs daemon for QEMU
to talk to. The guest boots with
`root=rootfs rootfstype=9p rootflags=trans=virtio,version=9p2000.L`, or
`root=/dev/vda` in block mode. Block mode builds the ext4 image with `mke2fs`
from Homebrew's `e2fsprogs`.

**No sudo on a 9pfs root.** hull clears every setuid and setgid bit on the
9pfs root filesystem, on purpose. The export uses `security_model=none`, which
reports the host's ownership to the guest, so a setuid binary such as
`/usr/bin/mount` would drop the guest's init from root to whichever uid ran
hull. `sudo` and other setuid programs therefore do not work in 9pfs mode. Use
`--rootfs-type block`, or the `vz` or `hvi` backends, if you need them.

**Read-only shares are refused**, rather than quietly mounted read-write. The
error tells you to use `vz` or drop the `:ro` suffix.

**Console.** The guest console is the emulated PL011 UART on `ttyAMA0` wired to
QEMU's stdio, not a virtio console. hull does not put the host terminal into
raw mode for QEMU, because QEMU's character device does that itself. In a
foreground run QEMU owns the foreground process group, so Ctrl-C reaches QEMU
directly and QEMU exits on it.

**exec has extra conditions here.** `hull exec` reaches a QEMU guest over a
virtio-serial port bridged to a per-instance Unix socket, not over vsock as on
`vz`. It works only for a 9pfs-rooted instance whose entrypoint is `urunit` and
whose image ships `/urunit-agent`, because the `/.qemu-init` wrapper is the only
thing on the QEMU path that starts the agent. With `--rootfs-type block`,
nothing starts it.

**`--stop-grace` does nothing here.** It is a `vz-runner` flag. On QEMU, hull
asks for a graceful power-down over a per-instance QMP socket, then falls back
to `SIGTERM` and `SIGKILL`, and reuses the same number only as its own local
timeout.

**Networking.** Two paths, and the difference is about privilege:

- `--gateway-sock` uses a Unix-socket stream netdev. No entitlement, no root.
  This is the recommended path, and the one compose uses.
- `--net shared` goes through vmnet, which needs the caller to be **root** or
  to hold `com.apple.vm.networking`. That entitlement is a managed one: Apple
  must grant it to your team, and it needs a provisioning profile. A plain
  Apple Development certificate cannot sign for it. Treat root or the gateway
  as the two real options, and see
  [troubleshooting.md](troubleshooting.md#cannot-create-vmnet-interface-general-failure)
  for the full list.

**Not available.** No checkpoint or restore, no GUI window, no Rosetta.

## The gateway is the portable answer

`--gateway-sock` works on all three backends, needs no entitlement and no root,
and gives the same addressing, DNS and port-forwarding behavior everywhere. It
is the only network path where the three backends behave alike.

`hull compose` starts one gateway per project, so compose users get this by
default. See [networking.md](networking.md) for the full picture and
[network-egress.md](network-egress.md) for restricting what a guest may reach.
