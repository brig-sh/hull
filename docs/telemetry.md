# Telemetry

hull, and the wrappers that drive it, collect anonymous usage
events and crash reports to help us understand what people run and fix what
breaks. This page is the canonical reference for exactly what is sent. If a
field is not listed here, it is not collected.

Interactive
users are asked before anything is sent, and turning telemetry off takes
one command.

## Turning it off

Any one of these disables all telemetry (usage and crash reports):

```
hull telemetry off        # persisted; `on` re-enables, `status` shows state
hull --dnt ...            # flag twin of DO_NOT_TRACK
export HULL_TELEMETRY_DISABLED=1
export DO_NOT_TRACK=1            # honored per consoledonottrack.com
```

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
improve it: command names, backend choice, versions and stack traces --
never file paths, arguments, image names or anything that identifies you.
Docs: https://github.com/brig-sh/hull/blob/main/docs/telemetry.md
Enable telemetry? [Y/n]
```

(`<product>` is the tool the user installed: `hull` when it is driven
directly, or the name of the wrapper driving it, such as `brig`. The docs URL
the consent prompt prints is this page.)

If a future version ever collects more than what this page lists, the
prompt is asked again with the expanded list, and nothing at all is
sent until you approve.

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

Three more variables exist for the tools that drive hull, not for opting
out:

| variable | what it does |
|---|---|
| `HULL_TELEMETRY_PRODUCT` | the `product` field: a wrapper driving hull (brig) sets it so events count against the tool the user installed. Defaults to `hull` |
| `HULL_TELEMETRY_ENDPOINT` | overrides the collector endpoint baked in at build time (tests, staging). A dev build has none, and sends nothing |
| `HULL_TELEMETRY_SUPPRESS` | internal: hull sets it on its own child invocations (compose self-exec, the `network-gateway` daemon) so one user command counts once. Not an opt-out |

## What is sent

All events share a common envelope:

| field | example | notes |
|---|---|---|
| `schema_version` | `2` | bumped on any schema change, with this page updated |
| `event` | `command` | one of `command`, `start`, `end`, `metrics`, `crash` |
| `product` | `brig` | set by the wrapper driving hull; defaults to `hull` |
| `version` | `0.1.0-rc14` | tool version |
| `os` | `26.0` | macOS major.minor only |
| `arch` | `arm64` | |
| `install_id` | random UUID | generated locally on first run; not derived from the machine; delete `<store>/telemetry.json` to rotate it |
| `uname` | `Darwin 25.3.0 <kernel build> arm64` | full uname, explicitly excluding the hostname |
| `captured_at` | RFC 3339 timestamp | when the event happened (for crash reports: the crash, not the upload) |
| `checksum` | hex SHA-256 | integrity checksum over `event\|product\|version\|install_id\|captured_at` with a fixed salt; ingestion drops payloads whose checksum does not match -- a soft guard against naive forgery, not a security boundary |

### `command` events

| field | example | notes |
|---|---|---|
| `command` | `run` | top-level subcommand name only, never arguments |
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
| `cpu_pct` | `48.5` | share of the guest's vCPUs busy since the previous sample, 0-100 |
| `uptime_s` | `90` | seconds since launch |

`cpu_pct` is a rate measured over the interval between two samples: the CPU
time the VM process accumulated, divided by the real time that passed and
by the guest's vCPU count. 100 means every vCPU was busy for the whole
interval. It can read somewhat above 100: the measurement covers the whole
VMM process, whose device emulation and I/O threads burn host CPU on top of
the vCPU threads. A sample is skipped rather than guessed when the two
readings cannot be compared -- the first one of an attach, which only sets the baseline, or
a vz helper that was replaced between ticks.

Before schema version 2 this field carried the `%cpu` column of `ps`
unchanged. That is a decaying average over up to a minute, summed across
the process' threads, so it ran to about 400 on a busy four-vCPU guest and
overlapped the neighbouring sample's window. Values collected under schema
version 1 are not comparable with later ones and should not be mixed.

### `crash` reports

| field | notes |
|---|---|
| `command` | top-level subcommand name only |
| `backend` | if known at crash time |
| `panic_type` | the Go type of the panic value (eg. `*errors.errorString`); never the panic message, which can embed paths |
| `stack` | Go stack trace, file paths trimmed to module-relative form |

Crash reports are written to `<store>/crashes/` when a panic happens
and uploaded on the next invocation. You can inspect or delete the files at
any time; the directory is the full queue.

## Where the state lives

The consent answer and the install id are in `<store>/telemetry.json`, and
the crash queue in `<store>/crashes/`, where `<store>` is the `--store-dir`
(default `~/.hull/store`). The store is the isolation boundary for
everything else hull keeps, and telemetry follows it: a command run with
another `--store-dir` has its own consent state and its own install id.

## What is never sent

- command arguments, flags values, environment variables
- file paths, directory names, hostnames, usernames
- image names, digests or registry references
- error message text (only coarse error classes)
- anything read from other processes or from macOS DiagnosticReports
- your IP address is not stored: it is stripped at ingestion and never
  written down

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
