# Telemetry

hull, and the wrappers that drive it, collect anonymous usage
events and crash reports to help us understand what people run and fix what
breaks. This page is the canonical reference for exactly what hull sends. If a
field is not listed here, hull does not collect it.

brig sends events of its own through the same client, under the same answer.
[brig's telemetry page](https://github.com/brig-sh/brig/blob/main/docs/telemetry.md)
lists the fields those carry.

The default is not the same in both directions, so both are stated here rather
than in one sentence:

- **Interactive sessions are asked first.** Nothing is sent before you answer,
  and the answer is persisted.
- **Non-interactive sessions are not asked, and telemetry defaults to on.**
  That covers CI, a script, a pipe and `--unattended`. See
  [Unattended installs](#unattended-installs).

Turning it off takes one command, and an opt-out works in both cases.

## Turning it off

Any one of these disables all telemetry (usage and crash reports):

```
hull telemetry off        # persisted; `on` re-enables, `status` shows state
hull --dnt ...            # flag twin of DO_NOT_TRACK
export HULL_TELEMETRY_DISABLED=1
export DO_NOT_TRACK=1            # the consoledonottrack.com convention
```

Both environment variables are compared to the exact string `1`. **A value
like `DO_NOT_TRACK=true` or `DO_NOT_TRACK=yes` does not opt out.** Write `1`.

Either variable suppresses usage and crash telemetry for that invocation and
leaves nothing on disk: no state file is created and no consent answer is
recorded. `hull telemetry off` and `--dnt` do persist the choice.

To see every payload instead of sending it:

```
export HULL_TELEMETRY_DEBUG=1
```

## The consent prompt

Shown on the first interactive invocation. Nothing is sent before you
answer; a single enter (or `y`) approves, `n` opts out, and the answer is
persisted either way.

```
<product> collects anonymous usage events and crash reports to help us
improve it: command names, which agent and backend you run, OS and tool
versions, and stack traces. An agent of your own goes out as a salted
hash of its name. File paths, arguments and image names are never sent.
Docs: https://github.com/brig-sh/hull/blob/main/docs/telemetry.md
Enable telemetry? [Y/n]
```

(`<product>` is the tool that asks. hull asks when it is run directly. A brig
that sends its own events asks with its own name, links its own page, and
leaves hull nothing to ask. One answer covers both tools, so the text names
the agent, which only brig reports.)

If a future version ever collects more than what this page lists, the
prompt is asked again with the expanded list, and nothing at all is
sent until you approve. The current ask is consent version 2, which added
brig's agent, the platform and `runtime_version`. A yes to version 1 is asked
again.

## Unattended installs

Non-interactive invocations never block on a prompt, and telemetry
defaults to on. CI environments (the conventional `CI` env var) count
as non-interactive even on a pty, so test harnesses never see the
prompt. For scripted setups:

```
hull --unattended ...        # skip the y/n even on a TTY; telemetry on
hull --unattended --dnt ...  # skip the y/n and record the opt-out
```

The env vars above work everywhere, no flags needed.

Four more variables exist for the tools that drive hull, not for opting
out:

| variable | what it does |
|---|---|
| `HULL_TELEMETRY_PRODUCT` | the `product` field: a wrapper driving hull (brig) sets it so events count against the tool the user installed. Defaults to `hull` |
| `HULL_TELEMETRY_VERSION` | the `version` field, read only together with `HULL_TELEMETRY_PRODUCT`: the wrapper's own version, kept to the characters a version is spelled with and to 64 of them |
| `HULL_TELEMETRY_ENDPOINT` | overrides the collector endpoint baked in at build time (tests, staging). A dev build has none, and sends nothing |
| `HULL_TELEMETRY_SUPPRESS` | `1` on hull's own child invocations (compose self-exec, the `network-gateway` daemon), so one user command counts once. A comma-separated list of event names, such as `command`, suppresses only those: a wrapper that sends them itself sets it. That invocation never shows the consent prompt, and sends nothing until someone has answered. Any other value suppresses everything. Not an opt-out |

## What is sent

All events share a common envelope:

| field | example | notes |
|---|---|---|
| `schema_version` | `3` | bumped on any schema change, with this page updated |
| `event` | `command` | one of `command`, `start`, `end`, `metrics`, `crash` |
| `product` | `brig` | set by the wrapper driving hull; defaults to `hull` |
| `version` | `0.1.0-rc14` | tool version, as `hull version` reports it without the leading `v`: the tag for a release, a pseudo-version such as `0.1.0-rc28.0.20260916191606-8a431dc1aaae` for a build after one, `+dirty` on a modified tree, `dev` for a build with no VCS data. Under a wrapper that sets `HULL_TELEMETRY_VERSION`, the wrapper's version |
| `runtime_version` | `0.1.0-rc30` | hull's own version, when a wrapper set `HULL_TELEMETRY_PRODUCT` |
| `platform` | `macos` | the OS family: `macos` or `linux` |
| `os` | `26.3.1` | the OS version: the full macOS product version, or `ID VERSION_ID` from os-release on Linux (eg. `ubuntu 24.04`) |
| `arch` | `arm64` | |
| `install_id` | random UUID | generated locally on first run; not derived from the machine; delete `telemetry.json` (see [Where the state lives](#where-the-state-lives)) to rotate it |
| `uname` | `Darwin 25.3.0 arm64` | the kernel's name, the version at the start of its release, and the machine (`Linux 6.8.0-45 aarch64` on Linux, for a release of `6.8.0-45-generic`). Not the hostname, not the kernel's build string, and not the rest of the release, which whoever built the kernel sets |
| `captured_at` | RFC 3339 timestamp | when the event happened (for crash reports: the crash, not the upload) |
| `checksum` | hex SHA-256 | integrity checksum over `event\|product\|version\|install_id\|captured_at` with a fixed salt; ingestion drops payloads whose checksum does not match -- a soft guard against naive forgery, not a security boundary |

### `command` events

| field | example | notes |
|---|---|---|
| `command` | `run` | the subcommand: the first non-flag token of the command line, skipping the value of `--store-dir`, when it names one of hull's commands, and `unknown` when it does not |
| `outcome` | `ok` / `error` | |
| `error_class` | `network` | coarse class on failure, one of `canceled`, `not-found`, `permission`, `network`, `other`; never the error message |

### `start` events

Emitted when a VMM launch is attempted.

| field | example | notes |
|---|---|---|
| `backend` | `qemu` / `vz` / `hvi` | the VMM backend used |
| `backend_source` | `default` | `flag`, `annotation` or `default` |
| `boot` | `ok` / `fail` | whether the VMM process started |

### `end` events

Emitted when the instance exit is observed: by the foreground `run`, or
by `stop` for a detached VM.

| field | example | notes |
|---|---|---|
| `backend` | `vz` | |
| `duration_s` | `312` | instance lifetime in seconds |

### `metrics` events

Sampled per running VMM while a CLI is attached to it. "Attached" means
the foreground `run`, or an `exec` session on a detached VM -- so a sandbox
is sampled while a session is using it, but an idle detached VM that nobody
is attached to reports nothing.

The first sample follows a few seconds after the attach and the rest come
every 30 seconds. The short start is deliberate: a VM or an exec session
that ends inside one interval would otherwise report nothing at all, which
biases the data towards long-lived, mostly idle VMs.

The reading is the actual VM process: for qemu and hvi that is the launcher
(the guest runs in-process); for vz the guest runs in Apple
Virtualization.framework's separate XPC helper, so the sampler measures
that helper rather than the thin `vz-runner` launcher, which reports
almost no CPU or memory of its own.

| field | example | notes |
|---|---|---|
| `backend` | `vz` | |
| `rss_kb` | `524288` | VMM process resident set size |
| `cpu_pct` | `48.5` | share of the guest's vCPUs busy since the previous sample; 100 is all of them |
| `uptime_s` | `90` | seconds since launch |

`cpu_pct` is a rate measured over the interval between two samples: the CPU
time the VM process accumulated, divided by the real time that passed and
by the guest's vCPU count. 100 means every vCPU was busy for the whole
interval. It can read somewhat above 100: the measurement covers the whole
VMM process, whose device emulation and I/O threads burn host CPU on top of
the vCPU threads. A sample is skipped rather than guessed when the two
readings cannot be compared -- the first one of an attach, which only
sets the baseline, or a vz helper that was replaced between ticks.

Before schema version 2 this field carried the `%cpu` column of `ps`
unchanged. That is a decaying average over up to a minute, summed across
the process' threads, so it ran to about 400 on a busy four-vCPU guest and
overlapped the neighbouring sample's window. Values collected under schema
version 1 are not comparable with later ones and should not be mixed.

### `crash` reports

| field | notes |
|---|---|
| `command` | the subcommand, or `unknown`, as above |
| `backend` | if one was resolved at crash time: `qemu`, `vz`, `hvi`, or `unknown` for a name hull does not know |
| `panic_type` | the Go type of the panic value (eg. `*errors.errorString`); never the panic message, which can embed paths |
| `stack` | Go stack trace, file paths trimmed to module-relative form |

Crash reports are written to the `crashes/` directory next to
`telemetry.json` when a panic happens
and uploaded on the next invocation. You can inspect or delete the files at
any time; the directory is the full queue.

## Where the state lives

With the default store, the consent answer and the install id are in
`~/.hull/telemetry.json`, and the crash queue in `~/.hull/crashes/`. They sit
next to the store, `~/.hull/store`, and not inside it: hull mounts a
case-sensitive volume over the store, which would hide a file written there
before the mount and show it again after a reboot.

A command run with another `--store-dir` keeps its own consent state and its
own install id inside that store, as `<store>/telemetry.json` and
`<store>/crashes/`. hull mounts that store too, so an answer recorded there
before the mount is hidden by it, and the next command that cannot ask
sends events as if nobody had answered. `DO_NOT_TRACK=1` is not affected.

Earlier versions kept the default store's state in `~/.hull/store`. The
first run of this version copies the install id and the recorded answer from
there, and leaves the old file in place. A no recorded later is written to the
old file too, so an older hull reads it as well. A no that an older hull
records in the old file after the copy is read too, when that file is the
newer of the two.

The move reads the old file before the store is mounted. If the store is not
mounted at that moment, hull reads the copy underneath the mount, which can
predate an answer you gave while it was mounted. A no given then may need to
be given once more with `hull telemetry off`. A yes is asked again anyway,
because it was given to an older consent version.

brig keeps its answer in `~/.hull` too, and sends its own events through the
same client (`pkg/telemetry`, a Go module of its own). One answer and one
install id cover both tools, and `hull telemetry off` stops brig's events as
well. brig does this on Linux too, where hull does not run yet, and creates
`~/.hull` for those files alone.

## What is never sent

- command arguments, flag values, environment variables (beyond the product
  and version a wrapper names in `HULL_TELEMETRY_PRODUCT` and
  `HULL_TELEMETRY_VERSION`)
- file paths, directory names, hostnames, usernames
- image digests or registry references
- error message text (only coarse error classes)
- anything from macOS DiagnosticReports
- your IP address is not stored: it is stripped at ingestion and never
  written down

One entry on that list needs a caveat, because taken flatly it is wrong:

- **Process readings are sent.** A `metrics` event carries `rss_kb` and
  `cpu_pct`, and both are sampled from the VMM process, which is another
  process. The `metrics` section above says which process and when. What is
  not read is macOS DiagnosticReports.

## Where it goes and how long it stays

Events go to an OpenTelemetry collector operated by NOFire AI (OTLP/HTTP,
each event one log record with the payload above as its body) -- no
third-party analytics service ever receives them. `schema_version`,
`event`, `product`, `version`, `install_id`, `captured_at` and `checksum`
are duplicated as log-record attributes so the collector can route and
filter without parsing the body. Payloads with a mismatching `checksum`
are dropped at ingestion. The client sends with a 2 second
timeout and gives up silently: telemetry can never slow down or break a
command. Raw events and crash reports are retained for 365 days; only
aggregate statistics are kept longer.
