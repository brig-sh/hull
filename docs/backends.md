# Backends

hull has three hypervisor backends. They are not three implementations of one
feature set. Each uses a different interface, carries a different device model,
and is missing things the others have.

## How a backend gets chosen

hull resolves the backend in this order:

1. the `--hypervisor` flag
2. the image's `com.urunc.unikernel.hypervisor` annotation
3. `qemu`

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
| `vz` | the default. You want NAT with no setup, a GUI window, Rosetta, or checkpoint and restore |
| `hvi` | you want a persistent writable root without a disk image, correct Linux file modes over a share, or the same device model hull's guests get on Linux |
| `qemu` | you need a device or a machine option only QEMU has |

## Capability comparison

| | `vz` | `hvi` | `qemu` |
|---|---|---|---|
| Interface | Virtualization.framework | Hypervisor.framework, called directly | Hypervisor.framework, via QEMU's `-accel hvf` |
| Entitlement the runner needs | `com.apple.security.virtualization` | `com.apple.security.hypervisor` | `com.apple.vm.networking`, and only for `--net shared` |
| Ships with hull | yes, as `vz-runner` | yes, as `hvi` | no, `brew install qemu` |
| Guest architecture | arm64 | arm64 | arm64 |
| Device transport | Virtualization.framework's | virtio-mmio, no PCI | QEMU's |
| Rootfs: shared directory | virtiofs | virtiofs | 9pfs |
| Rootfs: block | ext4 image | ext4 image | ext4 image |
| Boots a plain image carrying no kernel | yes | yes | **no** |
| Read-only `--shared-dir` | yes | yes | **refused** |
| `--net shared` | Apple NAT, no extra privilege | **no TCP egress**, see below | vmnet, needs root or a signed QEMU |
| `--gateway-sock` | yes | yes | yes |
| `exec` | yes | yes | yes |
| Checkpoint and restore | yes, with a block rootfs | **no** | **no** |
| `--gui`, `--gui-title` | yes | no | no |
| `--rosetta` | yes | no | no |

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

**Checkpoint and restore.** Only here, and only with a block rootfs. A virtiofs
root goes stale across a restore, so hull refuses the combination. See
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

**No checkpoint or restore.** `hvi` has no code that saves or reloads VM state
at this pin. Its subcommands are `boot`, `dump-fdt`, `smoke`,
`smoke-shm-verify`, `sandbox-selftest`, `seccomp-selftest` and `version`. The
`Quiesce::checkpoint` name inside `hvi` is a vCPU park primitive used so a
plugin can read registers consistently, not a VM snapshot. hull's `checkpoint`
and `restore` commands refuse any backend but `vz`.

**Other limits `hvi` states about itself.** Eight vCPUs on a GICv2 arm64 Linux
host, which does not apply on macOS because Apple's in-kernel GIC is a GICv3.
One disk and one NIC, no hotplug, no PCI. virtio-fs on macOS only. It has had
no external security audit. The boundary it is built to hold is the one around
the guest; it does not claim to defend against a guest that hangs its own VM,
or against CPU side channels.

**No clock device.** `hvi` has no emulated RTC on macOS, so hull seeds the
guest clock once at boot by writing the host epoch into the boot initrd. A
long-running `hvi` guest has no host time source to correct drift against.

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
