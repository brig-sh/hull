# CLI reference

`hull <command> --help` is the authority. This page is the same information in
one place, with the defaults written out.

```
hull [global options] <command> [command options] [arguments]
```

## Global options

These are accepted on every command and subcommand.

| Option | Default | What it does |
|---|---|---|
| `--debug` | off | enable debug logging |
| `--store-dir <dir>` | `~/.hull/store` | where images, instances and boot assets live. A complete isolation boundary |
| `--unattended` | off | skip the telemetry consent prompt. **Telemetry stays on.** Combine with `--dnt` to opt out |
| `--dnt` | off | record a telemetry opt-out, persisted. Same effect as `DO_NOT_TRACK=1` |
| `--help`, `-h` | | show help |
| `--version` | | print the version. **Present only in a release build**, see below |

A plain `go build` leaves the version string empty, and the CLI library then
hides the flag, so a locally built binary answers
`flag provided but not defined: -version`. A `make` or released build injects
the version through ldflags and has the flag.

## Commands

There are eighteen top-level commands. `network-gateway` is the only hidden one:
it does not appear in `hull --help`, but it runs and prints its own help when
named. No flag anywhere in the tree is hidden.

| Command | What it does |
|---|---|
| `pull <image>` | pull an OCI image into the store |
| `run <image> [cmd...]` | create and run an instance |
| `exec <id> <cmd...>` | run a command in a running instance |
| `ps` | list instances |
| `stop <id>` | stop a running instance |
| `checkpoint <id>` | pause a running `vz` instance, save VM and disk state, resume it |
| `restore <id>` | restore a stopped `vz` instance from its checkpoint |
| `rm <id>` | remove a stopped instance |
| `logs <id>` | show an instance's log |
| `inspect <id>` | print instance details as JSON |
| `images` | list pulled images |
| `rmi <image>` | remove images from the store |
| `prune` | remove images and pull leftovers nothing needs |
| `assets` | manage the boot assets used for images that carry no kernel |
| `store` | manage the volume the store lives on |
| `compose` | run a multi-service compose file, one VM per service |
| `telemetry` | control usage and crash telemetry |
| `network-gateway` | run the user-mode network gateway daemon (hidden) |

## `hull run`

The image reference is the first positional argument. Everything after it
replaces the image's `Cmd`, the way `docker run` does. The image entrypoint is
kept when that entrypoint is `urunit`.

Twenty-three flags:

| Flag | Default | What it does |
|---|---|---|
| `--detach`, `-d` | off | run in the background and print the instance id |
| `--net <mode>` | `none` | `none` or `shared` |
| `--pull <policy>` | `missing` | `missing`, `always` or `never`. A cached tag is not re-resolved, so use `always` to pick up a republished tag |
| `--mem <MB>` | 512 | memory in MB |
| `--cpus <n>` | 1 | number of vCPUs |
| `--name <name>` | auto-generated | instance name |
| `--shared-dir <spec>` | | share a host directory: `/host/path:/guest/path[:ro\|rw]`. Repeatable |
| `--shared-dir-fd <spec>` | | share a directory the caller already holds open: `FD:/guest/path[:ro\|rw]`. Clear `FD_CLOEXEC` first. Repeatable |
| `--hypervisor <name>` | from image annotation, else `qemu` | `vz`, `hvi`, `qemu`, or `qemu-hvf` |
| `--qemu-path <path>` | auto-detect, prefers a signed copy | path to `qemu-system-aarch64` |
| `--rootfs-type <mode>` | `virtiofs` on `vz` and `hvi`, `9pfs` on `qemu` | `block`, `virtiofs` or `9pfs` |
| `--annotation <k=v>` | | set an OCI runtime annotation. Repeatable |
| `--no-boot-assets` | off | do not fall back to the published boot assets when the image carries no kernel |
| `--env`, `-e <spec>` | | `KEY=VALUE`, or a bare `KEY` to inherit it from the host without putting the value in argv. Repeatable |
| `--add-host <host:ip>` | | add an entry to the guest's `/etc/hosts`. Repeatable |
| `--stop-grace <secs>` | 10 | seconds `vz-runner` waits for the guest to answer a stop request before forcing. **No effect on `qemu`** |
| `--wait-ip` | off | with `--detach` and NAT networking, wait for the DHCP lease and record the IP before returning |
| `--gateway-sock <path>` | | join the user-mode gateway at this control socket. Works on all three backends |
| `--gateway-cidr <cidr>` | | static guest CIDR on the gateway subnet, for example `10.87.0.10/24` |
| `--gui` | off | open a graphical window. `vz` only |
| `--gui-title <text>` | | title for that window. Requires `--gui` |
| `--rosetta` | off | run an amd64 rootfs under Rosetta translation. `vz` only; the kernel stays arm64 |
| `--platform <os/arch>` | `linux/arm64` | image platform to pull |

### Combinations hull rejects

These fail before anything starts, rather than being documented and ignored:

- `--pull` with any value other than `missing`, `always` or `never`
- `--gateway-sock` without `--gateway-cidr`, and the reverse
- `--gateway-sock` together with `--net none`
- `--gui` or `--rosetta` on any backend but `vz`
- `--gui-title` without `--gui`
- `--rosetta` without the virtiofs rootfs mode, a `urunit` init, and a static
  arm64 `busybox` at `/.rosetta/busybox` in the image

Note what is **not** validated: `--net` is only compared against the literal
`none`, and `--rootfs-type` is only validated on the generic container-boot
path. A typo in either is accepted silently.

### `--platform` and `--rosetta` are different switches

`--platform` selects the image platform to pull, and nothing else. `--rosetta`
turns on translation, and if you give it without an explicit `--platform` it
defaults the pull to `linux/amd64`. An explicit `--platform` wins. The image
annotation `com.urunc.darwin.rosetta` also turns the path on, but cannot change
the pull platform, because the platform is decided before the pull.

## `hull exec`

```
hull exec [options] <id> <cmd> [args...]
```

| Flag | What it does |
|---|---|
| `--tty`, `-t` | allocate a pseudo-terminal |
| `--cwd <dir>` | working directory in the guest |
| `--user`, `-u <user>` | guest user |
| `--env`, `-e <spec>` | `KEY=VALUE` or a bare `KEY`. Repeatable |

`exec --env` resolves the same way `run --env` does, but sends the list over
the agent socket rather than writing it to a file. The host `TERM` is applied
as a default that an explicit `--env TERM` beats.

`hull exec` propagates the guest command's exit code as its own exit status.

## Other commands

| Command | Flags |
|---|---|
| `pull <image>` | `--platform` |
| `ps` | none. Columns: ID, STATUS, EXIT, PID, IP, CREATED. No JSON mode |
| `stop <id>` | `--timeout`, `-t <secs>` force-kill timeout |
| `rm <id>` | `--force`, `-f` remove a running instance |
| `logs <id>` | `--follow`, `-f`; `--tail`, `-n <lines>` |
| `inspect <id>` | none. Always prints JSON |
| `images` | `--json` prints the store's records with full digests |
| `rmi <image>...` | `--force`, `-f` remove one a stopped instance refers to; `--platform <p>` narrow to one platform |
| `prune` | `--all` also remove every image no instance refers to; `--dry-run` print and remove nothing |
| `checkpoint <id>` | `--timeout <secs>`, default 60 |
| `restore <id>` | `--detach`, `-d`; `--stop-grace <secs>`, default 10; `--wait-ip`; `--gateway-sock <path>` |
| `assets show` | none |
| `assets dir` | none. Prints the directory and nothing else, for scripts |
| `assets pull [REF]` | `--force` download even when the assets are present |
| `store detach` | `--force` detach even with files still open |
| `store compact` | `--force` detach even with files still open, before compacting |
| `telemetry on\|off\|status` | none |

`-t` means two different things: a pseudo-terminal on `exec`, and a force-kill
timeout in seconds on `stop`.

`hull checkpoint` refuses an instance that is not running, one whose VMM is not
`vz-runner`, one started without a checkpoint state directory, and one that is
not on a block rootfs.

`hull images` prints `No images found` and `hull ps` prints
`No instances found` on an empty store. Both exit 0. `hull prune` prints
`Nothing to prune` and exits 0 the same way.

`hull rmi` and `hull prune` remove from the image cache; `hull rm` removes an
instance and never touches the cache. Neither returns space to the host on its
own -- the store's sparse image only shrinks under `hull store compact`. See
[storage.md](storage.md#disk-space-and-how-to-get-it-back).

## `hull compose`

| Flag | Environment variable |
|---|---|
| `--file`, `-f <path>` | `COMPOSE_FILE` |
| `--project-name`, `-p <name>` | `COMPOSE_PROJECT_NAME` |
| `--env-file <path>` | |
| `--profile <name>` | `COMPOSE_PROFILES` |

| Subcommand | Flags |
|---|---|
| `up` | `--detach`, `-d` (accepted for compatibility; `up` always detaches); `--subnet <cidr>`, default `10.87.0.0/24` |
| `down` | `--volumes`, `-v` also delete named volumes |
| `ps` | none |
| `logs` | `--follow`, `-f`, for a single service |
| `config` | none. Prints **YAML**, not JSON |
| `exec` | `-T`/`--no-tty`, `-u`/`--user`, `-e`/`--env`, `-w`/`--workdir`, and a `--` terminator |
| `top` | none |

`hull compose exec` parses its own flags rather than using the CLI parser, so
everything after the service name reaches the guest untouched. One consequence:
`hull compose exec --help` prints a usage line to **stderr** and exits nonzero,
unlike every other command.

See [compose.md](compose.md).

## `hull network-gateway`

Hidden from `hull --help`. `hull compose` starts one per project; a standalone
`hull run` joins one with `--gateway-sock`.

| Flag | Default | What it does |
|---|---|---|
| `--socket <path>` | required | control socket path |
| `--api <path>` | | HTTP API socket path, a probe endpoint |
| `--qemu-socket <path>` | derived from `--socket` plus `.qemu` | stream netdev socket. Carries **both QEMU and hvi** members |
| `--subnet <cidr>` | `10.87.0.0/24` | virtual subnet |
| `--gateway-ip <ip>` | `10.87.0.1` | gateway address on that subnet |
| `--forward <spec>` | | host port forward, `hostaddr:port=guestip:port`. Repeatable |
| `--host <name=ip>` | | static DNS A record served by the gateway. Repeatable |
| `--egress-default <verdict>` | unset, meaning unfiltered | `allow` or `deny` for a connection no rule matches |
| `--egress-allow <rule>` | | `host=<glob>` or `cidr=<cidr>`. Repeatable |
| `--egress-deny <rule>` | | same forms. Repeatable |
| `--egress-refresh <dur>` | 30s | how often to re-resolve named hosts in the rules. 0 disables |
| `--project <name>` | | compose project to supervise, which enables restart policies |
| `--supervise-interval <dur>` | | liveness poll interval of the supervision loop |

See [networking.md](networking.md) and
[network-egress.md](network-egress.md).

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | success |
| 1 | any command error, printed as `error: <message>` on stderr. Also an unknown flag |
| 2 | a panic on the main goroutine, after printing the stack and queueing a crash report |
| 3 | an unknown command, `No help topic for '<name>'`, from the CLI library |

`hull --help` and `hull` with no arguments exit 0.

One asymmetry to know when scripting: `hull exec` propagates the guest's exit
code, and `hull compose exec` propagates the code of the `hull exec` it
re-executes. But **`hull run` in the foreground does not.** It returns the VMM
process's wait error, so any nonzero VMM exit becomes hull exit status 1 with
`error: exit status N`. Do not read a foreground `hull run` status as the
workload's status.

## JSON output

Two commands emit JSON: `hull images --json` and `hull inspect`, which always
does and takes no flags.

`hull images --json` prints one record per stored image with whole digests, and
omits `indexDigest` and `platform` from a record that has none rather than
printing them empty.

hull's JSON output escapes the C1 control range, U+0080 to U+009F, as `\u00XX`,
so a hostile image label or instance name cannot drive the reader's terminal.
The result is still valid JSON and decodes to the same string.

`hull inspect` does not print environment values. It does print the full
recorded monitor command line, which includes every share's host path and the
whole kernel command line.

## Environment variables

### Boot assets

| Variable | What it does |
|---|---|
| `HULL_BOOT_ASSETS` | use this directory instead of the store's, for a local build of the assets |
| `BRIG_BOOT_ASSETS` | honored the same way; `HULL_BOOT_ASSETS` wins |
| `HULL_BOOT_ASSETS_REF` | fetch this reference instead of the one for this platform, to pin a version or use a mirror |
| `HULL_REGISTRY_TOKEN` | a token with `read:packages`, for a private bundle or a host whose keychain cannot be unlocked |
| `HULL_VERIFY` | how the bundle's cosign signature is checked: `warn` (default), `require`, `strict`, or `off`/`none`/`0`. Anything unrecognized is `warn` |
| `HULL_BOOT_ASSETS_ALLOW_FOREIGN` | `1`, or the repository being allowed, to fetch from a repository other than the published one |
| `HULL_BOOT_ASSETS_INSECURE` | `1` to fetch from a registry that resolves to a scheme other than https |
| `XDG_DATA_HOME` | consulted only with no store and off macOS; assets then live under `$XDG_DATA_HOME/brig/assets` |

With no store, the fallback asset directory is `~/.hull/assets` on macOS.

### Telemetry

| Variable | What it does |
|---|---|
| `HULL_TELEMETRY_DISABLED` | set to 1 to turn telemetry off |
| `DO_NOT_TRACK` | set to 1 to turn telemetry off |
| `HULL_TELEMETRY_DEBUG` | print every payload to stderr instead of sending it |
| `HULL_TELEMETRY_PRODUCT` | the product a wrapper reports events under. brig sets it. Default `hull` |
| `HULL_TELEMETRY_ENDPOINT` | override the build-time collector endpoint |
| `HULL_TELEMETRY_SUPPRESS` | **internal, not an opt-out.** hull sets it on its own child invocations, such as the compose self-exec and the gateway daemon, so one command counts once |

See [telemetry.md](telemetry.md).

### Everything else hull reads

| Variable | What it does |
|---|---|
| `HULL_TERMINAL_FILTER` | `off`, `0`, `none` or `false` turns off the filter hull puts between guest output and your terminal, which blocks OSC 52 clipboard reads, DCS passthrough and cursor-position queries. Anything else leaves it on |
| `CI` | counts the session as non-interactive, so the consent prompt never shows |
| `TERM` | forwarded into the guest for a pty `exec` |
| `COMPOSE_FILE`, `COMPOSE_PROJECT_NAME`, `COMPOSE_PROFILES`, `COMPOSE_DISABLE_ENV_FILE` | compose equivalents of the flags above |

A session counts as interactive only when `CI` is unset **and** both stdin and
stderr are terminals.

`HULL_BIN`, `HULL_STORE_DIR` and `HULL_TEST_LOG_DIR` are read only by the
harnesses under [test/](../test/README.md), never by hull itself.
`HULL_ASSETS_TOKEN` is a CI secret, not something the binary reads.

The `vz-runner` component reads no environment variables at all. Every
configuration value reaches it as a command-line argument from hull.
