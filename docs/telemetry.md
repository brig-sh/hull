# Telemetry

hull, and the wrappers that drive it, collect anonymous usage
events and crash reports to help us understand what people run and fix what
breaks. This page is the canonical reference for exactly what is sent. If a
field is not listed here, it is not collected.

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
| `schema_version` | `1` | bumped on any schema change, with this page updated |
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
| `command` | `run` | the first non-flag token of the command line, skipping the value of `--store-dir`. See the caveat under [What is never sent](#what-is-never-sent) |
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

Sampled every 30 seconds per running VMM while a CLI is attached to it.
"Attached" means the foreground `run`, or an `exec` session on a
detached VM -- so a sandbox is sampled while a session is using it, but an
idle detached VM that nobody is attached to reports nothing.

The reading is the actual VM process: for qemu that is the launcher
(the guest runs in-process); for vz the guest runs in Apple
Virtualization.framework's separate XPC helper, so the sampler measures
that helper rather than the thin `vz-runner` launcher, which reports
almost no CPU or memory of its own.

| field | example | notes |
|---|---|---|
| `backend` | `vz` | |
| `rss_kb` | `524288` | VMM process resident set size |
| `cpu_pct` | `12.3` | VMM process CPU usage |
| `uptime_s` | `90` | seconds since launch |

### `crash` reports

| field | notes |
|---|---|
| `command` | the first non-flag token, as above |
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

- command arguments, flag values, environment variables
- file paths, directory names, hostnames, usernames
- image digests or registry references
- error message text (only coarse error classes)
- anything from macOS DiagnosticReports
- your IP address is not stored: it is stripped at ingestion and never
  written down

Two entries on that list need a caveat, because taken flatly they are wrong:

- **Process readings are sent.** A `metrics` event carries `rss_kb` and
  `cpu_pct`, and both are sampled from the VMM process, which is another
  process. The `metrics` section above says which process and when. What is
  not read is macOS DiagnosticReports.
- **A malformed invocation can send an image name.** The `command` field is
  simply the first non-flag token, and it is not checked against the real
  subcommand list. So `hull run ubuntu:latest` sends `run`, but
  `hull ubuntu:latest`, with the subcommand left out, sends `ubuntu:latest`.
  The same applies to a mistyped subcommand. If that matters to you, opt out.

## Where it goes and how long it stays

Events go to an OpenTelemetry collector operated by NOFire AI (OTLP/HTTP,
each event one log record with the payload above as its body) -- no
third-party analytics service ever receives them. Payloads with a
mismatching `checksum` are dropped at ingestion. The client sends with a 2 second
timeout and gives up silently: telemetry can never slow down or break a
command. Raw events and crash reports are retained for 365 days; only
aggregate statistics are kept longer.
