# Building from source

You only need this to work on hull. To use it, install a release; see
[install.md](install.md).

hull is three executables in three languages. hull builds two of them and
fetches `hvi` from its release. Both runners must be signed before they can
start a VM.

| Executable | Language | Built by |
|---|---|---|
| `hull` | Go | `go build`, from `cmd/hull` |
| `vz-runner` | Swift | `swift build`, from `vz-runner/` |
| `hvi` | Rust | [hvi-vmm](https://github.com/brig-sh/hvi-vmm); hull fetches the release `hvi.pin` names |

## Prerequisites

- An Apple Silicon Mac. On Linux the CLI has only `hull assets`,
  `hull exec`, `hull images`, `hull inspect`, `hull logs`, `hull ps`,
  `hull pull`, `hull rm`, `hull rmi`, `hull run`, `hull stop` and
  `hull version` so far;
  see
  [Building on Linux](#building-on-linux).
- **Go** 1.26 or later. `go.mod` declares 1.26.4.
- **Xcode command line tools**, which supply Swift. `vz-runner` declares
  `swift-tools-version:5.9` and a macOS 14 deployment target.
- **cosign** v3 or later (`brew install cosign`), which verifies the hvi
  release.
- Optional: `brew install e2fsprogs` for the block and overlay-block rootfs
  modes (the cask installs it), and `brew install qemu` for the QEMU backend.

## The hvi release

`hvi.pin` names the hvi release hull ships: its version, its commit and the
sha256 of its `aarch64-apple-darwin` archive. `make fetch_hvi` runs
`scripts/fetch-hvi.sh`, which downloads that release and checks the cosign
signature of its `SHA256SUMS`, the archive's sha against the pin, and the
binary's Developer ID signature and hypervisor entitlement. It then asks
Apple for the notarization ticket of the binary's cdhash, from the service
Gatekeeper queries, and refuses a binary that has none. It installs the
binary to `build/hvi` and caches the archive in `.cache/hvi/`. The cache
saves the download only: both checks need the network on every run.

The release binary is signed and notarized, and no `make` target re-signs it.
To work on hvi itself, build it in a clone of hvi-vmm and point hull at it:

```bash
HVI_BIN=/path/to/hvi-vmm/target/release/hvi make macos
```

`make` copies that binary to `build/hvi` and signs it ad-hoc with
`packaging/hvi.entitlements`.

## Build

```bash
make macos
```

That runs four steps: build `hull`, build `vz-runner`, fetch `hvi`, then sign
`hull` and `vz-runner` ad-hoc and strip the quarantine attribute.

A plain `make` picks the build for the host: `macos` on a Mac, `linux`
anywhere else.

Individual targets:

| Target | What it does |
|---|---|
| `make urunc_macos` | build the Go CLI for macOS to `dist/hull_arm64` |
| `make linux` | build the Go CLI for Linux to `dist/hull_linux_<arch>` |
| `make vz_runner` | `swift build -c release` in `vz-runner/` |
| `make fetch_hvi` | fetch and verify the pinned hvi release into `build/hvi` |
| `make sign` | sign `hull`, `vz-runner` and a local hvi build, and strip quarantine |
| `make codesign_verify` | print each binary's authority chain and entitlements |
| `make test` | `go test -count=1 ./...`, then the same in `pkg/telemetry` |
| `make app` | assemble `hull.app` with both runners inside `Contents/MacOS` |
| `make dmg_image` | build and sign the installer image, no notarization |
| `make dmg` | `dmg_image` plus notarize and staple |
| `make release_tarball` | package the signed binaries into the release archive |
| `make install` | install all three into `PREFIX`, default `/usr/local/bin` |
| `make clean` | remove build artifacts |
| `make help` | list every target |

### Build concurrency

`swift build` parallelizes by default. On a machine running other work, cap
it rather than letting the build assume it owns the box.

### Building on Linux

A Linux port has started. It can start an instance in the background, with
no network or on a CNI bridge, run commands in it, read its log and stop
it, and little else yet. `make linux`, or plain `make` on a Linux host, builds a `hull` whose
only commands are `hull assets`, `hull exec`, `hull images`,
`hull inspect`, `hull logs`, `hull ps`, `hull pull`, `hull rm`, `hull rmi`,
`hull run`, `hull stop` and `hull version`. It builds no runners and signs
nothing.

```bash
make                        # dist/hull_linux_<host arch>
make linux LINUX_ARCH=amd64 # cross-build for another architecture
```

On Linux, hull is a containerd client, and images live in containerd.
`hull pull` fetches them, `hull images` lists them and `hull rmi` removes
them, with the same output as on macOS. All three need a running containerd, which hull does not install or
start. hull looks for its socket in this order:

1. `--containerd-address`
2. `$HULL_CONTAINERD_ADDRESS`
3. `/run/containerd/containerd.sock` when run as root, and
   `$XDG_RUNTIME_DIR/containerd/containerd.sock`, where a rootless containerd
   puts it, for any other user. With `$XDG_RUNTIME_DIR` unset or not an
   absolute path, `/run/user/<uid>` takes its place.

There is no store directory on Linux, and no `--store-dir`. Isolation comes
from the containerd namespace instead: `--namespace`, then
`$HULL_NAMESPACE`, then `hull`. Two namespaces see none of each other's
images, and `ctr -n hull images ls` shows what hull sees.

containerd keeps one record per image name, and it may hold several
platforms of that image. The listing describes one: the host's platform if
all its layers are present, otherwise a platform whose layers are. A pull of
another platform therefore lists as that platform, even though `ctr` also
fetches the host platform's manifest along the way. Entries of an index that
are not images, such as build attestations and signatures, are never listed
as the image.

`hull pull` differs from the macOS pull in these ways:

- `--platform` defaults to the host's, `linux/amd64` or `linux/arm64`, not
  to `linux/arm64`.
- The image is stored under its full name, so `hull pull busybox` lists as
  `docker.io/library/busybox`, the name `ctr` and nerdctl use for it too.
- containerd unpacks the image into a snapshot, with overlayfs unless the
  namespace's `containerd.io/defaults/snapshotter` label names another
  snapshotter, and keeps the layers in its content store. The daemon does
  the unpacking, so its user needs permission to create the snapshot: a
  containerd run as an ordinary user, outside rootlesskit, cannot.
- The progress line counts bytes downloaded, not extracted: the daemon
  unpacks and reports nothing to hull while it does.

The pull itself runs in hull's process, not the daemon's, so it uses the
same credentials as on macOS: the Docker config and its credential helpers.
A credential helper that fails, as one needing a locked keychain does,
leaves the pull anonymous. Registries on loopback, private RFC 1918
addresses or `.localhost` names are tried over HTTPS and then plain HTTP, as
on macOS. A `.local` name is HTTPS only: mDNS can answer for it from anywhere
on the network. A registry reached as `.local` over plain HTTP needs a
`.localhost` name or its address instead.

`hull rmi` takes the macOS arguments and flags and prints the same
`Removed <digest> <name>` lines, but containerd's model changes what some of
them mean:

- A name is one image. `hull rmi busybox` finds
  `docker.io/library/busybox:latest`, in any spelling `hull pull` accepts,
  and removes that name only. On macOS a tag can answer several stored
  digests, and all of them go.
- A digest, or a digest prefix, can match several names of one image. As
  `docker rmi` does for an image ID in several repositories, that is refused
  unless `--force` is given, which removes every one of them. A prefix that
  matches more than one digest is refused as ambiguous, as on macOS.
- `--platform` removes that platform's manifest, and containerd's garbage
  collector takes its config, layers and snapshot, as `docker rmi --platform`
  does. The name stays while it holds another platform. The content store
  holds a manifest once, so every name whose index lists it loses the
  platform too, and that takes `--force`. `--platform linux/arm64` matches an
  image recorded as `linux/arm64/v8`.
- The containerd containers in hull's namespace that were created from an
  image hold it, the role instances play on macOS. A running one, with a task
  that has not stopped, refuses the removal even under `--force`. A stopped
  one refuses it without `--force`. A container keeps its own snapshot, so it
  does not lose its root filesystem when its image goes.
- `hull rmi` waits for containerd's garbage collector, so the space is free
  when it returns. There is no `hull store compact` step.

`hull assets` has the macOS subcommands, output, signature check and
`provenance.json`; see [Boot assets](images.md#boot-assets). It does not use
containerd, and only the directory differs. With no store, the assets live
where brig keeps them on Linux, so a bundle either tool fetched is found by
the other:

1. `$HULL_BOOT_ASSETS`
2. `$BRIG_BOOT_ASSETS`
3. `$XDG_DATA_HOME/brig/assets`, when `XDG_DATA_HOME` is an absolute path. A
   relative one is ignored, as brig ignores it
4. `~/.local/share/brig/assets`

The namespace does not move this directory, so every namespace shares one
kernel with brig. `hull run` does not boot these files on Linux, for the
reason given below. Checking their signature needs cosign on
the `PATH`. On macOS the Homebrew cask installs it; on Linux nothing does, so
install it yourself, as
[Sigstore's installation page](https://docs.sigstore.dev/cosign/system_config/installation/)
describes. Without it a fetch warns that it took the assets unchecked, and
`HULL_VERIFY=require` refuses the fetch.

An instance is a containerd container in hull's namespace. `hull run` gives
the container an ID of its own, 16 random hex digits, and keeps the
instance's name in its `sh.brig.hull/name` label, so a name run again never
meets what containerd, urunc or a plugin kept under the last instance's ID.
Any other container, such as one `ctr` or nerdctl created, or an older hull,
is the instance its ID names. `hull ps`, `hull inspect`, `hull exec`,
`hull logs`, `hull stop` and `hull rm` take the name and act on any
container there, and `hull run` refuses a name either kind holds. A label
that is not a valid name, which another client can set, counts as no label.
Two runs of one name at once under the same log directory, as one user's
are, take its lock in turn, so the second finds the name taken. Two under
different log directories, as root's and a user's are, or a user's with two
`$XDG_STATE_HOME`s, can both get past that check, and each checks again
after creating its container. A run that then finds another container
holding the name removes its own and fails with `instance already exists`.
The run whose second check comes after the other's create fails; when both
creates come first, as they usually do for two runs started together, both
fail, and neither holds the name: run again. They never both succeed. A run
killed between its create and its removal of its own container leaves both.
Every command that acts on the name then fails, naming the two container IDs
and the `ctr` commands that remove one, but `hull ps` does not: it lists
both containers under the name, so brig, which reads `hull ps`, still finds
the instance. A name on Linux must also be a containerd identifier, as
every name was while it was the container's ID: at most 76 characters, with
no two of `.`, `_` and `-` in a row, and ending in a letter or digit.
`hull inspect <name>` prints an instance's container ID as `containerId`.
When inspect fails, as it does for an instance whose task containerd cannot
read, this shows it:

```bash
ctr -n <namespace> containers ls 'labels."sh.brig.hull/name"==<name>'
```

urunc listens on two sockets in `/run/containerd/runc/<namespace>/<id>`,
`urunc.sock` and `reexec.sock`, and a socket's path must fit in 107 bytes,
so an instance cannot start in a namespace longer than 57 characters.
`hull run` refuses one. The commands print what they print on macOS, with
these differences:

- The status comes from the container's task. A container with no task is
  `stopped`: containerd cannot tell one that was never started from one whose
  task was deleted. A task that is created but not started is `starting`, a
  running one `running`, and one that exited `stopped`, with its exit code.
  Any other status containerd reports, such as `paused`, is printed under
  containerd's name. A container whose task containerd cannot read, as when
  its shim does not answer, is listed as `unreadable`, with no PID, and the
  other containers are still listed. `hull inspect` of it fails with
  containerd's error.
- CREATED, and `startTime` in `hull inspect`, is when the container was
  created. PID is the process ID containerd reports for the task. IP is
  always `-`.
- `hull inspect` prints the macOS fields, and `containerId`, the
  container's ID, which the `ctr` commands below take. `imageDigest`,
  `backend` and `logFile` come from labels `hull run` sets, so they are
  empty for a container another client created. Those containerd has no
  counterpart for, among them `cmdLine` and `bundleDir`, are always empty.
- `hull inspect` prints `network`, Linux only: `none`, or the `--net` mode
  that attached the instance, `shared`. It comes from a label `hull run`
  sets, so it stays after the instance stops, when the address is gone. It
  is missing for a container another client created, and for one an older
  hull created, which recorded no network: read that as unknown, not as
  `none`. macOS records no `network`; its network is in `cmdLine`.
- `hull rm` deletes the container's task, the container and its snapshot,
  the instance's log, and the exec processes and FIFOs a killed `hull exec`
  left, described below. A task that has not stopped is refused without
  `--force`. With it, the exec processes are ended first, then the task is
  sent SIGKILL, and `hull rm` fails, keeping the task, the container and
  its snapshot, if the task has not exited within 5 seconds.
- `hull stop` sends the task SIGTERM, then SIGKILL if it has not exited
  within `--timeout` seconds (10 by default), and fails if it has not exited
  5 seconds after that. It first kills and deletes the exec processes a
  killed `hull exec` left, as `hull rm` does, and fails if one has not
  exited 5 seconds after its SIGKILL. It then deletes the task, and
  keeps the container, its snapshot and its log until `hull rm`. A task that
  had already exited is deleted too, since its shim runs until it is, and
  `hull stop` says the instance is already stopped. With its task deleted,
  the instance is `stopped` with no exit code.
- urunc hands the SIGTERM to the monitor, and cloud-hypervisor, the monitor
  a container boot defaults to, exits on it at once. The guest is not asked
  to shut down first, as it is on macOS's `vz` and `qemu` backends; see
  [backends.md](backends.md).
- `hull logs` prints the log `hull run` writes, and takes `--follow` and
  `--tail` as on macOS. It reads the file at the path hull computes for the
  name, as `hull rm` deletes it, and not the one the container's `logFile`
  label names, which any client of the namespace can set. A container
  another client created has no log of its own, and `hull logs` reports the
  file as not found, with one exception: a hull instance removed with `ctr`
  or nerdctl rather than `hull rm` leaves its log behind, and `hull logs`
  prints that log for a later container of the same name. Besides the
  guest's console, the log holds what virtiofsd prints. Printed to a
  terminal, the log goes through the filter `HULL_TERMINAL_FILTER` controls,
  as on macOS; see [cli.md](cli.md).
- `hull logs` reads the file as macOS does, with the same limits.
  `--follow` goes on after the instance stops, and ends when `hull rm`
  deletes the log. It prints the whole log and ignores `--tail`, and a line
  the guest has not finished when the follow reads it comes out as two
  lines. `--tail` drops the carriage return of the console's `\r\n` line
  endings, which the whole log keeps, and fails with `token too long` on a
  line over 64 KiB, such as progress output that only ever returns to the
  start of the line.
- `hull exec` asks containerd to run the command in the instance's task.
  Under urunc, the shim runs `urunc exec`, which passes the command to the
  guest's urunit-agent, the agent macOS hull talks to directly, and exits
  with its status; hull exits with that status too, and only warns when
  the shim then fails to delete the exec process. A command that cannot
  start makes hull exit with status 1. Without `-t` urunc prints why on
  stderr, but with `-t` hull prints nothing: urunc writes the reason only
  to the task's `log.json` in containerd's state directory. An exec sent
  while the guest is still booting waits for the agent: urunc tries to
  reach it for 15 seconds, and over cloud-hypervisor's or Firecracker's
  vsock socket its last try can take 3 seconds more.
- The command gets the instance's environment, with `--env` replacing or
  adding variables, and the instance's working directory and user unless
  `--cwd` or `--user` names others. `--user` takes a uid, a uid:gid or
  root, as `hull run` takes the image's user, since hull reads no file
  from the image; a uid with no gid runs with gid 0.
- SIGINT, SIGTERM and SIGHUP sent to hull go to the command, with `-t` or
  without, and so does the SIGHUP of a terminal that closes. Without `-t`
  the command gets the signal itself, and a command that ignores it keeps
  hull waiting. With `-t`, urunc hangs the command up whatever the signal:
  it gets SIGHUP, and SIGKILL 2 seconds later if it is still running. On
  macOS, where hull passes a signal on, the command gets the signal
  itself. A SIGHUP or SIGINT that hull was started with ignored, as
  `nohup` ignores SIGHUP, stays ignored and does not reach the command.
  hull cannot tell whether it was started with SIGTERM or SIGQUIT
  ignored. A SIGTERM always reaches the command, and hull ignores SIGQUIT
  whenever SIGINT was ignored, as a shell ignores both for a background
  job. A SIGQUIT ignored without SIGINT still kills the command.
- SIGQUIT, which urunc does not pass on, and the SIGPIPE hull gets when
  what reads its output goes away, as `head` does, kill the command, and
  hull exits with status 137.
- A signal sent while exec still waits for the agent goes to the exec
  once the wait ends. It ends the exec, and the command does not run:
  urunc passes signals on only once the session is open. hull exits with
  128 plus the signal's number.
- The signals hull does not catch still end it, and leave behind what
  SIGKILL does, below: SIGKILL itself, and SIGABRT, SIGILL, SIGTRAP,
  SIGBUS, SIGFPE, SIGSEGV, SIGSTKFLT and SIGSYS, on which hull prints a Go
  stack dump.
- Without `-t`, the end of hull's stdin reaches the command. With `-t` on a
  terminal, the terminal is in raw mode, ^C goes to the guest as a byte,
  and changes to the window's size follow. With `-t`, the end of stdin is
  not passed on, since closing the input of a command on a terminal hangs
  the terminal up.
- hull returns once the command has exited and its output has ended. A
  process it leaves in the background that holds its stdout or stderr
  keeps hull waiting: in a runc container until that process exits, as
  docker exec does, and under urunc for at most 2 seconds, after which the
  agent stops waiting for the output. A signal sent to hull during that
  wait goes to the command, which has exited, and does not end it.
- With `-t`, the guest's terminal starts at 24x80, and takes the size of
  hull's terminal within about a quarter of a second. urunc makes a
  terminal of its own rather than take the size hull asks for, and passes
  on the size hull sets right after the start only once it sees it. A
  command that reads its size as the first thing it does gets 24x80.
- With `-t`, urunc filters the guest's output as well, wherever it goes:
  hull filters only output that reaches a terminal, but urunc filters it on
  its way to a pipe or a file too. So `-t` output captured in a file lacks
  the sequences the filter removes, which it keeps on macOS. urunc's
  filter has a switch of its own, `URUNC_TERMINAL_FILTER`, which it reads
  from the daemon's environment, so neither `HULL_TERMINAL_FILTER` nor an
  `--env` passed to the command turns it off.
- The FIFOs that carry the command's input and output go in a directory
  of the instance's own, named after its container ID:
  `/run/hull/fifo/<namespace>/<container-id>` for root, and
  `$XDG_RUNTIME_DIR/hull/fifo/<namespace>/<container-id>` for anyone else,
  where a rootless daemon's shim can open them too. hull killed with
  SIGKILL leaves its FIFOs there, the command running in the guest, and
  its exec process undeleted in the shim.
- `hull stop` and `hull rm` kill and delete the exec processes whose FIFOs
  are in the instance's directory, and remove the directory, before they
  end the instance. An exec process they leave running, described below,
  keeps the directory. A `hull exec` still running in it ends with status
  137, and output the command wrote just before that may not reach it:
  stop and rm read the exec process's FIFOs and throw away what they read,
  so that the shim can delete it. `hull rm` without `--force` refuses a
  running instance and leaves its exec processes alone.
- An exec process whose start has not returned within 5 seconds is left
  running. The shim answers nothing about an exec process while it starts
  it, and under urunc a start waits for the guest's agent, for up to 30
  seconds while the guest boots. Ending the instance ends it too, since the
  agent goes with the VM, but the instance is reported stopped only once
  the start has failed, so `hull stop` and `hull rm --force` wait for that
  too, and fail when it outlasts their own wait for the task. Its FIFOs
  stay in the instance's directory, so a later stop or rm finds it again,
  and the `ctr` commands below can read its exec ID.
- Stop and rm look for the directory as `hull exec` does, so one run as
  another user, or with another `$XDG_RUNTIME_DIR`, does not find the exec
  processes. The directory is named after the namespace and the container's
  ID, not the daemon. The IDs hull gives its containers never repeat, but
  two daemons that both have a container of one ID in one namespace, as
  another client may create, share it: a stop or rm through one removes the
  other's FIFOs, and the other's stop can then no longer find its exec
  processes. An instance removed by another client, such as `ctr`, leaves
  its directory behind, and an exec that fails leaves an empty one until the
  instance is stopped or removed.
- The exec processes have to go first: a command that goes on writing
  after hull is killed with SIGKILL fills its FIFO, and urunc's exec
  process then blocks for good. The shim reports the instance stopped
  only once its exec processes have exited, so the instance would not
  stop, even after the VM has exited. When `hull stop` cannot find such an
  exec process, killing it through containerd frees the instance:

  ```bash
  ctr -a <socket> -n <namespace> task kill -s SIGKILL --exec-id <exec-id> <container-id>
  ```

  where `<socket>` is the daemon's socket, as `--containerd-address`
  names it, and the exec ID, `hull-` and 16 hex digits, is in the names
  of the FIFOs hull left. An exec process hull was killed before starting
  cannot be signalled, and the kill fails with `process not created`;
  `ctr ... task delete --exec-id <exec-id> <container-id>` deletes it instead.

`hull run` boots an image as a VM through urunc, as the containerd runtime
`io.containerd.urunc.v2`. It needs urunc's shim installed where the daemon
finds it, and a monitor urunc is configured for; the brig-standalone-linux
bundle provides both, and hull is tested against its v0.1.0-rc17. A plain
image also needs a urunc that can boot an unmodified container image, as
the bundle's can; urunc-dev/urunc f149eb7 adds that. A urunc without it
requires a unikernel binary in every case, so it runs a plain image with
runc, on the host's kernel. hull cannot tell which urunc the shim runs, so
it cannot refuse that. It takes
`--name`, `--pull`, `--mem`, `--cpus`, `--env`, `--annotation` and
`--no-boot-assets` with their macOS meaning, and differs from macOS in these
ways:

- `--detach` is required. The guest's console goes to a log file, which
  `hull logs` reads, and nothing attaches to it. The log is under
  `$XDG_STATE_HOME/hull/<namespace>`, or `~/.local/state/hull/<namespace>`,
  and under `/var/log/hull/<namespace>` for root.
- `--net` takes `none`, its default, and `shared`. With `none` the
  instance has no network. `shared` attaches it to the CNI network named
  `bridge`, the name of nerdctl's default network, which the bundle's
  configuration keeps: its `brig0` bridge on `10.44.0.0/24` is the one
  brig's nerdctl containers use, so hull's instances and those containers
  share one segment and one address pool. The network gateway does not
  exist on Linux. hull looks for the plugins in `$CNI_PATH`, a
  colon-separated list, and for the network's configuration in
  `$NETCONFPATH`, as nerdctl and the CNI tools do. When those are unset, it
  uses the installed bundle's `libexec/cni` and `etc/cni/net.d`, under
  `$XDG_DATA_HOME/brig/data`, or `~/.local/share/brig/data`, then
  `/var/lib/brig/data`, and with no bundle, `/opt/cni/bin` and
  `/etc/cni/net.d`. For a user other than root, the configuration comes from
  `$XDG_CONFIG_HOME/brig/cni/net.d`, or `~/.config/brig/cni/net.d`, when
  that exists: `brig-rootless-setup.sh` copies the bundle's configuration
  there, and points the rootless nerdctl it sets up at the copy. A run whose
  network or plugins are missing, or whose configuration directory it
  cannot create nerdctl's lock file in, fails before it pulls anything.
- The attachment works as nerdctl's does. The spec carries a `createRuntime`
  and a `postStop` OCI hook that run `hull internal oci-hook`, so the
  runtime attaches the instance's network namespace before urunc starts the
  guest, and detaches it when the task is deleted: by `hull stop`,
  `hull rm`, or the cleanup of a failed run. The hook takes the lock nerdctl
  takes, `.cni-concurrency.lock` in the configuration directory, so the two
  do not run the plugins of one network at once. On a rootless daemon the
  hook runs in rootlesskit's user namespace, and makes the bridge in the
  network namespace rootlesskit keeps for slirp4netns, which hull finds
  under `$ROOTLESSKIT_STATE_DIR`, else `$XDG_RUNTIME_DIR/containerd-rootless`,
  the directory containerd-rootless.sh uses. A state directory where no
  rootlesskit answers on `api.sock`, as one a stopped rootlesskit left, is
  taken for no rootless daemon at all. While the instance is
  attached, the hook keeps its network namespace pinned by a bind mount in
  `<instance>.net`, as CRI pins a pod's: the namespace would otherwise go
  with the guest, before `postStop` runs, and the bridge plugin removes the
  masquerade rule it added only from inside it. A hook that fails fails the
  run with the plugin's error, and leaves no address leased.
- urunc moves the interface's traffic to the guest and gives the guest its
  address on the kernel command line. The guest's `/etc/resolv.conf` comes
  from a file hull writes beside the instance's log, in `<instance>.net`.
  The nameservers are rootlesskit's own, such as
  slirp4netns's `10.0.2.3`, on a rootless daemon, and then the host's:
  those in `/etc/resolv.conf`, or behind systemd-resolved's stub the
  upstream servers it lists in `/run/systemd/resolve/resolv.conf`, less
  loopback and IPv6 ones. With none left, the guest gets `8.8.8.8` and
  `8.8.4.4`, as docker gives a container. On a block device root
  filesystem, urunc copies the file into the guest's disk when the instance
  is created, so hull writes it before then. urunc boots a plain image from
  a block device, a devmapper snapshot, only when it cannot share the root
  filesystem: on a monitor with no shared filesystem, or with no virtiofsd
  where urunc's configuration says. The guest keeps the image's own
  `/etc/hosts`, as on macOS, so it has no line for the instance's own name
  or address.
  `hull ps` and `hull inspect` show the address while the instance is
  attached.
- `--net shared` has limits. There is no `--publish`, no network of a
  sandbox's own (`--net <name>`, `hull network`), no `--dns`, `--add-host`
  or `--hostname`, and no network other than `bridge`. urunc gives the guest
  one interface and only its first IPv4 address. A detach that fails does
  not stop the task's delete: `hull stop`, or `hull rm` when it deletes the
  task, prints the error as a warning, and leaves whatever the detach did
  not remove in place. The OCI runtime spec has a runtime only warn about a
  failed `postStop` hook, but the bundle's urunc fails the delete instead,
  after removing its own state, so a failed hook would leave an instance
  that can never be removed. The hook is the hull binary that ran the
  instance, at the path its symlinks lead to, and urunc cannot start a hook
  that is not there. So moving or removing that file while an instance runs,
  as an upgrade that removes the old version's directory does, makes `hull
  stop` and `hull rm` of the instance fail, and every later try fails too,
  for the same reason. Killing the instance's shim, the
  `containerd-shim-urunc-v2 -namespace <namespace> -id <container-id>`
  process, with the `containerId` `hull inspect` prints, has containerd
  remove the task, and `hull rm` then removes the instance. A task that
  goes without being deleted, this way or at a host reboot, never runs its
  `postStop` hook. `hull rm` detaches such an instance itself: when the
  hook's record of the address in `<instance>.net` is still there once the
  task is gone, it runs the hook as the instance's spec names it, from its
  own binary, so the binary that ran the instance need not exist. For an
  instance of a rootless daemon it runs it in rootlesskit's namespaces
  through `nsenter`, which must be on its `PATH`. It does so before it
  deletes the container, and a detach that fails fails `hull rm` and keeps
  the instance, its record and its pin, so the next `hull rm` tries again;
  `hull rm --force` removes it anyway, with the error as a warning. Until
  then the instance keeps its address leased, on a rootful daemon even
  across reboots, since the bundle's host-local plugin keeps its leases in
  `/var/lib/cni/networks`, and a later run of the name does not free it:
  each run's container has an ID of its own, and so does its attachment.
  `hull stop` of an instance whose task is gone does not detach it, and
  neither does the removal of its container by another client, such as
  `ctr`. hull takes the daemon for a rootless one from the user it runs
  as, not from the daemon it reaches: a user other than root with a rootless
  setup whose `--containerd-address` names a rootful daemon gets that
  setup's configuration directory and rootlesskit's nameserver, which the
  guest cannot reach. Run hull as root for a rootful daemon. Under a
  rootless daemon, `/var/lib` is rootlesskit's own tmpfs, so the plugins'
  state there, the bundle's address store included, goes when rootlesskit
  stops. When a plugin fails partway through an attach, the firewall
  plugin's two `ACCEPT` rules for the address stay in the `CNI-FORWARD`
  chain: it removes its rules using the attach's result, and libcni records
  none for an attach that failed.
- `--shared-dir` takes the macOS form, `/host/path:/guest/path[:ro|rw]`,
  with the same refusals but one: the host path starts with `/`, `.` or `~`
  and names a directory that exists, and the guest path is absolute, not
  `/`, and no other share's. A space or a quote in the guest path, which
  macOS refuses because its init script names the path unquoted, is taken:
  urunc passes the path through no shell. With `--net shared` the guest
  path cannot be `/etc/resolv.conf`, where hull puts the network's
  resolver configuration. hull makes the host path absolute and puts it in the spec as a bind mount,
  `rbind` and `rprivate` as `nerdctl run -v` makes one, with `ro` or `rw`,
  and urunc takes it into the guest. As urunc 3c00b69 does it, that works on
  two roots:
  - On a root urunc shares from the host, over virtio-fs or 9p, the
    directory is bind-mounted into the tree the guest shares. `ro` holds in
    the guest, and a write reaches the host. A container boot gets this root
    on cloud-hypervisor, hvi and qemu, on any snapshotter, devmapper too.
  - On the overlay-block root (`com.urunc.unikernel.rootfsMode` set to
    `overlay-block`), it is a virtio-fs export of its own, and `ro` holds
    the same way.

  On any other root urunc drops the directory with no error, because it
  copies only files into a root it boots from as an initrd or a disk, and
  gives a guest with no root from the host none. So hull refuses the share
  before the instance exists:
  - on a monitor that cannot share a directory, such as firecracker, where
    urunc boots a container boot from the snapshot as a disk;
  - for an image that boots its own kernel, unless
    `com.urunc.unikernel.mountRootfs` is true, neither a
    `com.urunc.unikernel.initrd` nor a block device mounted at `/` is
    named, and the snapshotter is overlayfs. urunc boots such a guest from
    the snapshot whenever that is a block device, as devmapper's is, and
    hull can tell ahead of the run that it is not only for overlayfs.

  urunc fails a container boot itself when it is given an initrd, a block
  image or `com.urunc.unikernel.mountRootfs` false. All of this takes the
  virtiofsd urunc is configured with to be there, as the bundle ships it:
  without it, urunc boots a container boot on cloud-hypervisor or hvi from
  a snapshot that is a block device, and drops the share.

  `ro` is not recursive, as nerdctl's `:ro` is not either; hull has no
  form of nerdctl's `:rro`, which is. urunc remounts only the top of the
  bind read-only, so a file system mounted below a read-only share on the
  host stays writable from the guest on a shared root, and is not shared
  on the overlay-block root. hull checks the host path, and urunc opens it when the task is
  created, so a directory put in its place in between is the one the
  guest gets, as with nerdctl. `--shared-dir-fd` is macOS only.
- `--env-file` adds the variables of a file, and is Linux only: macOS
  `hull run` does not take it. It follows `docker run --env-file`. Each
  line is `KEY=VALUE`, taken as written once its leading white space is
  gone, quotes and trailing white space included, or a bare `KEY`,
  inherited from hull's environment and dropped when hull has none, as a
  bare `--env KEY` is. An empty line, and a line whose first character
  other than white space is `#`, is skipped; a `#` after a value is part of
  the value. A comma in the path is part of the path. A key with white
  space in it, a line with no key, and a line that is not UTF-8 fail the
  run. The flag can be repeated. The files are read in order and `--env`
  comes after them, and the last value given for a variable wins, `--env`
  given twice included. hull reads the file, so a relative path is taken
  from the current directory.
- An image is looked up under the name it was pulled by. A reference pinned
  to a digest, `repo@sha256:…` or `repo:tag@sha256:…`, also finds an image
  of the same repository pulled by its tag when that image is the digest,
  so `--pull never` runs it and `--pull missing` does not fetch it again.
  An image counts as pulled only when containerd holds its manifest and
  config for the host's platform. One pulled only for another platform is
  pulled again with `--pull missing`, and refused with `--pull never`.
- `--hypervisor` names one of the monitors urunc knows: `cloud-hypervisor`,
  `firecracker`, `qemu`, `hvi`, `hvt`, `spt` or `hyperlight-unikraft`. Any
  other name is refused, `vz` among them, whether `--hypervisor` or the
  `com.urunc.unikernel.hypervisor` annotation gives it. Without the flag,
  the monitor comes from that annotation, and a plain image boots on
  `cloud-hypervisor`, since the bundle ships no qemu, urunc's own default.
  The monitor must also be installed where urunc's configuration says; the
  bundle ships `cloud-hypervisor` and `firecracker`, and `hvi` from
  v0.1.0-rc21.
- `hvi` here is urunc's monitor of that name, which boots the guest on KVM,
  not the macOS backend. Only the urunc of the bundle from v0.1.0-rc21 runs
  it. An older urunc refuses it as an unsupported monitor when the task is
  created, after the container exists, and hull cannot tell which urunc the
  shim runs, so it cannot refuse the run sooner.
- `--snapshotter` names the containerd snapshotter that holds the
  instance's root filesystem, such as `devmapper`. Without it, the instance
  gets the namespace's default snapshotter, the one `hull pull` unpacks
  into. The daemon must have the snapshotter loaded. One it does not have,
  or one it skipped for want of configuration, as it skips devmapper by
  default, fails the run with containerd's error, which names the
  snapshotter, before any instance exists. A pull the run makes itself
  unpacks the image into this snapshotter, and not into the default one. An
  image already unpacked into another snapshotter, by `hull pull` or an
  earlier run, is unpacked again into this one, and then takes the disk
  space of both. `hull pull` has no `--snapshotter`, so a namespace that
  runs everything on one snapshotter should name it in its
  `containerd.io/defaults/snapshotter` label instead, which pulls follow
  too. `hull rm` removes the instance's snapshot from the snapshotter that
  holds it, and `hull rmi` the image's layers from every snapshotter.
- A plain image boots on `firecracker` only when the snapshotter gives the
  container a block device, as devmapper does. firecracker has no shared
  filesystem, so on overlayfs, the bundle's default, urunc refuses to
  start the instance.
- A plain image, one that names no unikernel binary in its annotations or
  in `--annotation`, boots a generic kernel and initrd. With the bundle
  installed, they are the bundle's own: under
  `$XDG_DATA_HOME/brig/data/share/guest`, or
  `~/.local/share/brig/data/share/guest`, and then
  `/var/lib/brig/data/share/guest`. With `HULL_BOOT_ASSETS` set, they are
  the files in that directory instead, and the bundle's are not looked at.
  `--annotation` with `com.urunc.unikernel.bootKernel` and `bootInitrd`
  overrides both, as brig passes them. A relative path, in
  `HULL_BOOT_ASSETS` or `--annotation`, is taken from the current
  directory.
- Whichever way a run arrives at its kernel and initrd, they are checked
  before it boots, and half a pair is refused, as on macOS. A file in a
  directory with a `SHA256SUMS`, as the bundle's are, must match it, wherever
  the other file is. Any other file is checked as macOS checks it, against
  the provenance record `hull assets pull` writes beside it; one with no
  record boots with a warning.
- hull on Linux never boots or fetches the boot assets `hull assets pull`
  fetches. The bundle's initrd is built for the bundle's urunc, and under
  that urunc the `linux-amd64` initrd from `ghcr.io/nofireai/hull-assets`
  boots a guest that cannot find the container's command. With no bundle,
  no `HULL_BOOT_ASSETS` and no `--annotation` paths, `hull run` of a plain
  image fails and says to install the bundle or supply the files. This may
  change once a published initrd boots under urunc on Linux.
- hull writes every `com.urunc.unikernel.*` annotation urunc reads into the
  container's spec, empty for one that is not set. urunc's shim copies any of
  them the spec lacks from the image's manifest, and an image could
  otherwise name a host file as the kernel or initrd to boot. The image's
  annotations are still honored, except `bootKernel`, `bootInitrd`,
  `rootfsMode`, `vAccel` and `RPCAddress`, which only `--annotation` sets.
- `hull run` refuses a run urunc would not boot as a VM. urunc needs a
  unikernel type and a monitor in the annotations, and a binary unless a
  boot kernel and initrd are given. When one is missing it looks for a
  `urunc.json` in the image's root filesystem, and when that is missing or
  invalid too, it runs the image with runc, as an ordinary container on the
  host's kernel. hull reads no file from the image, so it cannot tell
  whether a `urunc.json` would save the run. A run with a boot kernel and
  initrd gets the type `linux` unless it names one. That refuses
  `--no-boot-assets` for an image whose annotations name no complete
  unikernel, and an image that names a binary but not its type or monitor. An image that declares its unikernel only in
  a `urunc.json` runs with its unikernel type, monitor and binary given
  with `--annotation`. `--annotation io.kubernetes.cri.container-name` is
  refused too: urunc reads it as a Kubernetes container's name, and runs a
  container named `queue-proxy` with runc.
- The image's command follows docker's rules, which differ from macOS hull
  in two ways. Arguments after the image replace its `Cmd` and keep its
  `Entrypoint`, where macOS replaces both. An image with neither a command
  nor arguments is refused, where macOS runs `/bin/sh`.
- The image's `USER` must be empty, `root`, or a numeric uid and gid. hull
  reads the image's config from containerd's content store and no file from
  its root filesystem, so it cannot look a user name up in `/etc/passwd`. A
  uid with no gid runs with gid 0, as docker runs a uid `/etc/passwd` does
  not list. macOS reads the image's `/etc/passwd`, so it takes a listed
  uid's group from there, and gives an unlisted uid its own value as its
  gid.
- A container's snapshot takes the container's ID as its key, so a snapshot
  left behind with no container holds no name, and nothing reports it. A
  snapshot that a `hull run` killed while creating the container leaves is
  held by containerd's lease for the create, and containerd's garbage
  collector removes it when the lease expires, after 24 hours. One that
  another client leaves outside a lease stays: `hull rm` cannot remove it,
  since there is no instance, and
  `ctr -n <namespace> snapshots --snapshotter <snapshotter> rm <key>` can.

`make test` runs on Linux too. The `internal/` and `pkg/` packages build and
test natively. Of `cmd/hull`, only the files with no darwin dependency compile
there; the rest carry `//go:build darwin`.

## Sibling discovery, and what it means for PATH

hull locates `vz-runner` and `hvi` **next to its own executable**. Wherever one
is staged, the other two must be too.

For a local build, symlink the runners next to the CLI:

```bash
ln -sf vz-runner/.build/arm64-apple-macosx/release/vz-runner .
ln -sf build/hvi .
./dist/hull_arm64 --help
```

Or use `make install`, which copies all three into `PREFIX` and re-signs
`vz-runner` there, and `hvi` when it is a local build. It does not re-sign
`hull`, which needs no entitlement.

Putting only `hull` on your PATH is the most common local-build mistake. The
`vz` and `hvi` backends then fail with a message about not finding the runner.

## Signing

A freshly built runner cannot start a VM until it is signed with the right
entitlement. `make macos` signs ad-hoc, which is the default.

```bash
security find-identity -v -p codesigning
make sign CODESIGN_IDENTITY="Apple Development: Your Name (TEAMID)"
make codesign_verify
```

Prefer a real Apple identity. An *Apple Development* certificate is the one for
local work, and the entitlement is then honored with SIP enabled.

An ad-hoc signature does embed the entitlement, but macOS only honors it with
**SIP disabled**. CI tests both directions on real hardware. So an ad-hoc build
works on a machine with SIP off and will be refused the entitlement on a normal
Mac. **Do not disable SIP as a way around signing.**
[signing.md](signing.md#ad-hoc-signing) has the evidence and the full table.

A **rebuild** does invalidate a signature, because it produces different bytes.
`make macos` handles that by signing after it builds. A **copy** does not: see
[signing.md](signing.md#does-copying-a-binary-break-its-signature).

## What `hull version` reports

No version is stamped at link time. `hull version` and `hull --version` read
what the Go toolchain embedded from the checkout: the version it derives from
the nearest reachable tag, the commit, its time, and whether the tree was
modified. A bare `go build` reports all of it, the same as a `make` build or a
released binary.

The exception is a build in a **linked git worktree**. Its `.git` is a file
rather than a directory, which the toolchain does not read as a repository, so
it embeds no VCS data at all and the binary says:

```
hull dev (go1.26.5, darwin/arm64)
```

`make` passes git's own commit, time, nearest tag and modified flag for that
case, and the binary uses them only when the toolchain embedded nothing. So
build with `make` in a worktree if the version matters. See
`internal/buildinfo`.

## Testing

```bash
make test                                  # Go unit suite
go vet ./...
test/compose-config-smoke.sh dist/hull_arm64
```

`test/compose-config-smoke.sh` runs `hull compose config` against a built
binary and needs no VM, so it works anywhere.

Everything else under [test/](../test/README.md) boots a real VM and needs an
Apple Silicon host with working HVF. Those harnesses **skip with a named reason
and exit status 0** when the host cannot run them.

CAUTION: A zero exit from a PTY, checkpoint, share, hvi-boot or Rosetta harness
means it ran **or skipped**. Read the output. Do not report a skip as a pass.

Use a throwaway store so a test run cannot disturb your own:

```bash
HULL_STORE_DIR=$(mktemp -d) test/compose-config-smoke.sh dist/hull_arm64
```

Remember that hull mounts a store and never unmounts it, so add
`hull store detach` to any cleanup that needs to delete the directory
afterwards.

## The urunc dependency

hull consumes [urunc](https://github.com/urunc-dev/urunc) as a Go dependency.
The darwin work hull needs, the Vz backend, exec, console, the GUI window flags,
the HVI backend and the generic container initrd, lives on the upstream branch
`feat/initrd-hvi-backend-v0.8.0`, which is the darwin commits rebased onto the
v0.8.0 release. `go.mod` requires that branch's tip as a pseudo-version:

```
require github.com/urunc-dev/urunc v0.8.1-0.<date>-<sha>
```

There is no `replace`. The module is the upstream repository itself.

When the branch moves, refresh the pin **by commit**, not by branch name. Go
refuses `@feat/initrd-hvi-backend-v0.8.0` as a disallowed version string,
because the name ends in something that parses as one:

```bash
go get github.com/urunc-dev/urunc@<sha of the branch tip>
go mod tidy
```

A pseudo-version only lives while its commit is reachable upstream. If the
branch is deleted once it merges, move the pin to the merge commit or to the
tag that carries the work. Once the darwin work is in a tagged release, require
that tag.

For local development against a live checkout, use an uncommitted `go.work`,
which is gitignored, rather than editing `go.mod`:

```
go 1.26.4

use (
	.
	/path/to/urunc
)
```

## The telemetry module

`pkg/telemetry` is a Go module of its own,
`github.com/brig-sh/hull/pkg/telemetry`, because brig links it too. Its only
dependency is `golang.org/x/sys`, so brig does not take hull's module graph
along.

hull requires it at a placeholder version and builds it from the tree:

```
require github.com/brig-sh/hull/pkg/telemetry v0.0.0-00010101000000-000000000000
replace github.com/brig-sh/hull/pkg/telemetry => ./pkg/telemetry
```

So a change to the client and its use land in one commit. The `replace` also
means hull itself cannot be installed with `go install <module>@<version>`;
build it from a clone.

`go test ./...` at the root does not reach a nested module. `make test` runs
its tests too, and CI tests it on macOS and Linux.

brig requires the module by version. Tag a release of it as
`pkg/telemetry/vX.Y.Z`; until then brig requires a pseudo-version of a commit
on `main`.

## The app bundle and installer

`make app` wraps the three binaries into `hull.app`, with `vz-runner` and `hvi`
inside `Contents/MacOS` so sibling discovery works from `/Applications`.
`make dmg` produces the drag-to-Applications installer, signed, notarized and
stapled.

After dragging to Applications, put the CLI on your PATH:

```bash
ln -s /Applications/hull.app/Contents/MacOS/hull ~/.local/bin/hull
```

Packaging assets live in `packaging/` and are regenerated from the vendored logo
sources with `scripts/make-packaging-assets.sh`, which needs `imagemagick`.

Note one bundle subtlety. A bundle's **main** executable, `hull` here, is
sealed to the bundle's `Info.plist` and resource directory, so extracted as a
lone file its signature is invalid and AMFI kills it at exec. Nested binaries
like `vz-runner` and `hvi` are not sealed that way and survive extraction.
`make release_tarball` re-signs `hull` and `vz-runner` as standalone code,
which `hull` needs and `vz-runner` does not. The release `hvi` keeps its own
signature. A plain `cp` of an already-standalone binary breaks nothing. See
[signing.md](signing.md#does-copying-a-binary-break-its-signature).

## Changelog

`CHANGELOG.md` is generated, never edited by hand:

```bash
scripts/changelog.sh          # or: git-cliff --config cliff.toml -o CHANGELOG.md
```

The release workflow does not run it, so regenerate and commit it when cutting a
release. See [releasing.md](releasing.md).

## Before you open a pull request

Read [../CONTRIBUTING.md](../CONTRIBUTING.md) for the commit and review
conventions, and [../AI_POLICY.md](../AI_POLICY.md) if AI helped.

The commit scopes in use are `compose`, `run`, `exec`, `store`, `vz-runner`,
`hvi`, `qemu`, `ci` and `docs`.
