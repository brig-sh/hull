# Troubleshooting

Errors are grouped by the message you see. If your problem is a capability that
is simply absent on your backend, check [backends.md](backends.md) first: a
feature that does not exist there produces a refusal, not a bug.

## `hull run` fails immediately

### `generic virtiofs container boot requires the vz or hvi backend`

You asked a backend other than `vz` or `hvi` to boot an image that carries no
kernel. hull only fetches the generic boot assets for those two.

Pass `--hypervisor vz`, or use an image that carries its own kernel.

This also explains a confusing failure with no flag at all. With no
`--hypervisor` and no annotation in the image, hull falls back to `qemu`, so
`hull run ubuntu:latest` fails while `hull run --hypervisor vz ubuntu:latest`
works.

### `qemu-system-aarch64 not found in PATH; install with: brew install qemu`

hull does not install QEMU. Install it, or pick another backend:

```bash
brew install qemu
```

`--qemu-path` names a specific binary. Note that it moves only the executable:
the firmware search path stays at Homebrew's prefix, so a QEMU installed
elsewhere is still pointed at Homebrew's firmware.

### `--gui is only supported with the Vz hypervisor`

Also `--rosetta is only supported with the Vz hypervisor`, and `--gui-title`
without `--gui`. These are enforced, not just documented. Use `vz`.

### `--rosetta` is refused on `vz`

Four conditions must all hold, and hull checks them before boot:

1. the rootfs mode is virtiofs, not block and not initrd
2. the image's init is `urunit`
3. the image ships an executable static arm64 `busybox` at `/.rosetta/busybox`
4. Rosetta is installed on the host

For the last one, run `softwareupdate --install-rosetta` once. `vz-runner` does
not install it and fails with a named reason.

### hull refuses to use the store directory

hull will not mount its store over a case-insensitive directory that already
contains files, because mounting would hide them. Move the contents, or point
`--store-dir` at an empty directory or one already on a case-sensitive volume.
Four filenames hull writes itself do not count as content: `telemetry.json`,
`telemetry.lock`, `.case-sensitive-apfs` and `.DS_Store`.

### `Resource busy` attaching the store

Two stores under one parent directory used to resolve to the same backing
image. Current hull names each non-default store's image after the store
itself, so check you are not on an older build, and check nothing else has the
volume attached. `hull store detach --force` releases one that is held open.

## Entitlement and signing errors

### `The process doesn't have the com.apple.security.virtualization entitlement`

The `vz-runner` executable is not signed, or its signature no longer carries
the entitlement. Re-sign it:

```bash
make sign CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
```

A release installed from the Homebrew cask is signed correctly already. If you
hit this from a cask install, check that `vz-runner` still sits next to `hull`:
hull locates the runner beside its own executable.

For which identity you need and why, read [signing.md](signing.md). Do not
reach for SIP or AMFI changes as a first step.

### `codesign` fails with `unable to build chain to self-signed root`

Also `errSecInternalComponent`. The Apple WWDR intermediate certificate, the
Apple Root CA, or both are missing from your keychain. Install them once; see
[signing.md](signing.md#keychain-prerequisites).

`errSecInternalComponent` part way through a build has a second cause worth
knowing. `security list-keychains -d user -s` is a per-user setting, so a
concurrent build that sets its own search list evicts yours. Pin the keychain
with `CODESIGN_KEYCHAIN` when other jobs may share the login.

### The app is blocked, or macOS offers "Open Anyway"

The binary carries a `com.apple.quarantine` attribute, set when it was
AirDropped, downloaded or file-shared. Strip it:

```bash
xattr -dr com.apple.quarantine ./hull ./vz-runner ./hvi
```

`make sign` does this for you. Locally built binaries are never quarantined.

## Boot failures

### `failed to start VMM: ... operation not supported by device`

A foreground `hull run` makes your stdin the runner's controlling terminal,
which fails with `ENODEV` when stdin is not a real TTY. That happens under a
script, a pipe, `nohup`, or CI.

Use `--detach` and read the output with `hull logs`, or run from an interactive
terminal.

### `VZLinuxBootLoader` fails with `VZErrorDomain Code=1`

The kernel is in PE32+/EFI stub format. `VZLinuxBootLoader` requires the
uncompressed arm64 `Image` format, which carries the magic `ARMd` at offset
0x38. Use a kernel in that format, or let hull supply the generic boot assets
by not overriding them.

### Kernel panic: `not syncing: VFS: Unable to mount root fs`

The kernel command line does not match the rootfs mode, or the kernel lacks the
driver. Each mode needs both:

| `--rootfs-type` | Kernel command line | Kernel must have |
|---|---|---|
| `virtiofs` | `root=rootfs rootfstype=virtiofs` | virtiofs built in |
| `9pfs` | `root=rootfs rootfstype=9p rootflags=trans=virtio,version=9p2000.L` | 9p built in |
| `block` | `root=/dev/vda rw` | ext4 and virtio-blk built in |

This usually means a custom kernel or a hand-set
`com.urunc.unikernel.cmdline`. hull builds a matching command line itself when
it chooses the mode.

### The hvi boot needs the store and instance on one volume

`hvi` generic container boot replaces the instance rootfs with an APFS
copy-on-write clone, which requires the image store and the instance directory
on the same APFS volume. hull refuses the boot otherwise. There is deliberately
no plain-copy fallback. Keep both under one `--store-dir`.

## Networking

### The guest has an address but reaches nothing, on `hvi`

This is expected with `--net shared` on `hvi`, and it is the single most
common surprise in hull.

`hvi`'s built-in stack answers ARP, ICMP, DHCP and DNS, and **drops guest
TCP**. `hvi`'s own documentation lists "no egress from the built-in network
stack" among its known limits. Its built-in DNS is also expected to return
empty answers, because the resolver calls the host's `getaddrinfo` after `hvi`
installs a deny-by-default Seatbelt sandbox that blocks outbound sockets.

Use the gateway instead:

```bash
hull network-gateway --socket /tmp/gw.sock &
hull run --hypervisor hvi --net shared \
  --gateway-sock /tmp/gw.sock --gateway-cidr 10.87.0.10/24 IMAGE
```

Or use `hull compose`, which starts a gateway per project. See
[networking.md](networking.md).

### `cannot create vmnet interface: general failure`

QEMU only. The `vmnet` framework requires the caller to be **root** or to hold
`com.apple.vm.networking`. Options, best first:

1. **Use `vz`.** `--hypervisor vz --net shared` gives NAT with only the
   virtualization entitlement, no root, and measurably more throughput.
2. **Use the gateway.** `--gateway-sock` needs no entitlement and no root, and
   works on every backend.
3. **[socket_vmnet](https://github.com/lima-vm/socket_vmnet).** A small root
   launchd daemon owns the interface and hands unprivileged QEMU a socket.
4. **Run QEMU as root.** vmnet shared mode is allowed for root.
5. Sign QEMU with `com.apple.vm.networking`. This needs Apple to grant your
   team the managed entitlement plus a provisioning profile. A plain Apple
   Development certificate cannot do it.

### `failed to reach network gateway at <path>`

Nothing is listening on that socket. Start `hull network-gateway --socket
<path>` first, or let compose manage it.

hull dials the socket before starting the VM on purpose. Without that check,
`hvi` would only log a warning and fall back to its no-egress built-in stack,
so the run would look networked and carry no traffic.

## Console and terminal

### `can't access tty; job control turned off`

Expected inside the guest. The guest serial console is not a real TTY.
Interactive shells work; job control, meaning Ctrl-Z, `fg` and `bg`, is not
available there.

### Guest output does odd things to your terminal

hull puts a filter between guest output and your terminal, which blocks OSC 52
clipboard reads, DCS passthrough and cursor-position queries. If the filter is
in the way:

```bash
HULL_TERMINAL_FILTER=off hull run ...
```

Values `off`, `0`, `none` and `false` turn it off. Anything else leaves it on.
Do not leave it set: the filter exists so a hostile image cannot drive your
terminal.

## exec

### `hull exec` fails on a `qemu` instance

`exec` on `qemu` needs all three: a 9pfs root, a `urunit` entrypoint, and
`/urunit-agent` in the image. The `/.qemu-init` wrapper is the only thing on
that path that starts the agent, and `--rootfs-type block` starts nothing.

Use `vz` or `hvi` if you need `exec` against an arbitrary image.

### `hull exec` fails right after `hull run -d`

The runner process being up is not the guest being ready. Poll:

```bash
until hull exec "$ID" /bin/true 2>/dev/null; do sleep 0.2; done
```

## Checkpoint and restore

### `checkpoint requires the Vz backend`

Also `restore requires the Vz backend`. Only `vz` has this. `hvi` has no code
that saves or reloads VM state, and `qemu` is refused too.

### Checkpoint refuses a virtiofs instance

Checkpointing needs a block rootfs. Virtualization.framework does not rehydrate
the guest's FUSE state in a new runner process, so every inode would go stale
at restore. Start the instance with `--rootfs-type block`.

### Restore refuses an instance that used `--shared-dir-fd`

It cannot be restored. The recorded command line carries a
`/.vol/<device>/<inode>` identity path, and the descriptor that made that
identity trustworthy died with the process it was given to. Start a fresh
instance with a fresh descriptor.

### A checkpoint fails only on a headless machine

Recorded from CI rather than derived from code: machine-state saves have never
succeeded on a runner without a console session, 0 in 4 attempts on one machine
against 15 in 16 on another, while ordinary boots on the same machine pass. SIP
state is not the discriminator. Why a save needs a console session is
unresolved. Run checkpoints from a machine with a console session.

## Exit statuses

### A foreground `hull run` exits 1 and my workload exited 3

Expected, and worth designing around. A foreground `hull run` returns the
runner process's wait error, so any nonzero exit becomes hull exit status 1
with `error: exit status N`.

`hull exec` does propagate the guest's exit code, and so does
`hull compose exec`. If you need the workload's status, run detached and use
`hull exec`, or read the EXIT column from `hull ps`.

Other statuses: 2 is a panic on the main goroutine, and 3 is an unknown
command. See [cli.md](cli.md#exit-statuses).

## Store and cleanup

### A CI job leaves a mounted volume behind

hull mounts a store when it opens one and never unmounts it. A runner giving
every job its own `--store-dir` accumulates one mounted sparse image per job,
and a script cannot delete its temporary directory while the volume is
attached.

Add `hull store detach` to your cleanup. Detaching where nothing is mounted
prints a message and succeeds, so the step cannot fail a build for having
nothing to do.

### Deleting data inside the store does not free host disk space

Nothing calls `hdiutil compact`, so the sparse image never shrinks. It only
grows. To reclaim the space, delete the store and its backing image:

```bash
hull store detach
rm -rf ~/.hull
```

CAUTION: That destroys every image, instance and checkpoint in the default
store.

### An instance name stays taken

`hull rm <id>` clears an instance directory whose `state.json` is missing or
unparseable, which is the way out of a name squatted by a run that died before
its record was written. A name with no directory at all is reported as not
found, which means it is already free.

## Still stuck

Open an issue with the template's Environment section filled in: backend, macOS
version and chip, `hull --version` or the commit, the guest image, and the
store directory in use. Run the failing command with `--debug` and include the
output.

For boot, console or run-path problems, the harnesses under
[test/](../test/README.md) are the usual reproduction path.
