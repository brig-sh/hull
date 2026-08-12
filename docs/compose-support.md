# Supporting docker-compose recipes on urunc-macos

> **Usage docs**: for how to use the shipped `compose` command, see
> [`compose.md`](./compose.md). This document is the design/gap analysis
> that informed it.

Status: exploration (2026-07-08). Based on a code-level inventory of
`cmd/urunc-macos`, `pkg/store`, `pkg/ociclient`, and the urunc darwin
backends (`vz_darwin.go`, `qemu_darwin.go`, vz-runner).

## Model

A compose "service" maps naturally to one urunc instance: one lightweight VM
booting one OCI image with `urunit` as init. A `urunc-compose` layer (new
subcommand or separate binary) parses `docker-compose.yml`, computes the
service graph, and drives repeated `run`/`stop`/`rm` — the same way
docker-compose drives the Docker engine.

## What already maps cleanly

| Compose concept | Existing mechanism |
|---|---|
| `image:` | `pull` + OCI config is fully read (Entrypoint/Cmd/Env/WorkingDir → spec, `bundle.go:129-146`) |
| `mem_limit`, `cpus` | `--mem`, `--cpus` pass through to both backends |
| per-service isolation | rootfs is APFS-cloned per instance (`run.go:472-485`) — same image, independent writable roots |
| `container_name:` | `--name` (with caveats below) |
| logs | per-instance log file + `logs -f` |
| same-subnet networking | both backends land on the shared vmnet NAT subnet (192.168.64.x); inter-VM traffic works |

## Gap analysis

Ordered by how hard they block a compose MVP:

> **Phase-1 field finding (2026-07-08):** inter-VM traffic on the shared NAT
> subnet is **blocked by Apple's NAT attachment**. The bridge100 member ports
> created by `VZNATNetworkDeviceAttachment` carry the kernel bridge `PRIVATE`
> flag (visible in `ifconfig bridge100`), which prevents private-port-to-
> private-port forwarding — guests reach the host and the outside world, but
> ARP between guests never resolves (`Destination Host Unreachable`). macOS
> ships no userland toggle for the flag. Host↔guest traffic is unaffected, so
> discovery, hosts injection and host-mediated proxying all work. The fix —
> and the top Phase 2 item — is a `VZFileHandleNetworkDeviceAttachment`
> backend wired to a small host-side L2 switch (the socket_vmnet approach
> Lima uses for exactly this reason).

### 1. Service discovery (the critical path)
Compose services reach each other **by name**. Today:
- The guest IP is assigned in-kernel via `ip=dhcp` and **never captured
  anywhere** — not in `state.json` (no IP field, `store.go:43-53`), not via
  QMP, not by vz-runner.
- No `/etc/hosts` injection and no DNS beyond the NAT resolver
  (`/proc/net/pnp` → resolv.conf only).

**Recommended mechanism** (the Lima/Colima approach):
1. Deterministic MAC per instance — already implemented for QEMU
   (`run.go:853-865`, derived from the instance name); vz-runner needs a
   `--mac` flag (small Swift change) instead of a random
   `VZMACAddress`.
2. Host-side lease parsing: `/var/db/dhcpd_leases` maps MAC → IP a few
   hundred ms after boot. Record the IP in `state.json` (add an `IP` field)
   and surface it in `ps`/`inspect`.
3. Name resolution, two options:
   - **hosts-file accumulation (MVP):** start services in dependency order;
     before each service boots, write the already-known `name → IP` pairs
     into the clone's `/etc/hosts`. Works for the typical
     `depends_on` topology (db before app). Limitation: earlier services
     don't learn later services' IPs (mitigate: also inject env vars like
     `<SERVICE>_HOST=<ip>` docker-links style).
   - **compose DNS (later):** tiny host-side DNS responder bound on the
     vmnet gateway interface serving `*.compose.internal`; point guest
     resolv.conf at it. Clean, but more moving parts.

### 2. Per-run overrides: `environment:`, `command:`, `working_dir:`
All container config is currently frozen into the image; `run` has **no
`--env`, no command override** (`run.go:51-96` — the only positional is the
image ref). But the plumbing exists: env/uid/gid/cwd already flow through
`urunit.conf` (`buildUrunitConfig`, `run.go:884-903`). Adding `--env KEY=V`
(repeatable) and an optional command override that rewrites `process.args`
in the generated spec is straightforward CLI + spec-merge work.

### 3. `volumes:`
`--shared-dir` exists but: only **one** allowed, the virtiofs tag is
hardcoded (`"shared"`), and the declared guest path is **ignored** — the
guest must mount the tag itself (`run.go:650-682`, `vz_darwin.go:75`).
Needed: repeatable flag, unique tag per share, and init-wrapper mounts of
each tag at its guest path (the wrappers are generated in `run.go`, so this
is host-side templating).

### 4. `ports:`
No port forwarding at all (README documents it as unimplemented) — but the
host can reach guest IPs directly on the vmnet subnet. MVP can piggyback on
that: `ports:` becomes a host-side forwarder process (one `socat`-style
listener per mapping, or a small TCP proxy in the compose layer) targeting
the discovered guest IP. No guest changes required.

### 5. Lifecycle: `depends_on`, `restart:`, healthchecks
- No wait/events primitives; status is reconciled lazily via
  `kill(pid, 0)`. A compose supervisor must poll (or hold the child
  processes itself by running foreground per service).
- No restart policies or healthchecks anywhere; `depends_on` conditions
  (`service_healthy`) need a compose-layer prober (TCP/exec-over-serial is
  not available — TCP probe against the discovered IP is the realistic MVP).

> **Resolved (2026-07-30, [ADR-0007](adr/0007-compose-exit-status-oneshot-restart.md)).**
> The poll-based supervisor was built where this section predicted, inside the
> per-project gateway daemon: `restart: no|always|on-failure[:N]|unless-stopped`
> is parsed, validated and enforced, and `depends_on:
> service_completed_successfully` is gated on a real exit code obtained by
> running a one-shot service's command through the guest agent
> (`x-oneshot: true`). Healthchecks landed earlier (Phase 3, plus the exec form
> via the agent). What is still open is the *source* of the status for a plain
> service: only a job reports a code, so `on-failure` degrades to `always` and
> a plain stopped instance records no code at all — see Phase B below.
- **Vz stop is SIGKILL-only**: the stop path uses QMP which only QEMU
  serves; vz-runner accepts `--qmp` but never opens the socket. Either
  implement the QMP stub in vz-runner or (simpler) send SIGTERM to
  vz-runner, which already triggers `requestStop` graceful shutdown.

### 6. Robustness fixes the orchestrator needs
- `--name` collisions silently overwrite instance state
  (`store.go:180-181`, no existence check) — must error instead.
- The store's mutex is in-process only; concurrent `run` invocations (how a
  compose layer parallelizes) race on the store. Add file locking
  (`flock` on the store dir) or serialize in the compose layer.
- Detached instances never update status on guest exit — acceptable if the
  compose supervisor owns the processes, otherwise needs a reaper.

### Out of scope initially
`build:` (delegate to bunny/docker), named volumes (only bind mounts),
`networks:` beyond the single shared NAT, scale/replicas, secrets/configs.

## Suggested phasing

**Phase 1 — MVP (`up -d`, `down`, `ps`, `logs` for multi-service files):**
deterministic MAC for Vz + lease-based IP capture into `state.json`;
`--env` + command override; name-collision error + store file-lock;
hosts-file injection in dependency order; compose YAML subset parser
(services, image, environment, command, depends_on, mem/cpus, volumes with
single bind, container_name). Roughly: small changes in vz-runner (Swift),
`run.go`, `store.go`, plus a new `compose` package/subcommand.

**Phase 2 — networking via a user-mode gateway (implemented, ADR
[0001](adr/0001-single-nic-usermode-gateway.md)):** each service's single NIC
is backed by `VZFileHandleNetworkDeviceAttachment` over a datagram socket
connected to a per-project gateway daemon (`network-gateway`, embedding
gvisor-tap-vsock's netstack). The gateway provides service↔service switching,
NAT egress, DNS forwarding, and host-side `ports:` forwarding in one
userspace process — no entitlements, no root. The guest is configured by the
kernel with upstream urunc's exact static line
(`ip=<addr>::<gw>:<mask>:urunc:eth0:off:<dns>`), so guest-visible topology
stays 1:1 with urunc on Linux and all unikernel flavors work (no shell or
iproute2 needed in the image). Compose allocates static 10.87.0.0/24 IPs
before boot → complete name→IP `/etc/hosts` map for every service. This
merges the former Phase 2a/2b networking items and the Phase 3 compose-DNS
item. Verified under SIP: mesh ping 0% loss, DNS + ICMP egress 0% loss,
clean gateway teardown on `down`.

An earlier dual-NIC interim (Apple NAT + hand-rolled L2 switch + `urunc.net2=`
init-wrapper config) was replaced by this design — see the ADR for why.

**Phase 3 — lifecycle and storage (implemented):** multiple `--shared-dir`
entries, each with its own virtiofs tag, mounted at the declared guest path
by the init wrapper (virtiofs rootfs mode; compose passes all `volumes:`);
TCP healthchecks (`x-healthcheck-tcp: <port>`, probed by the gateway via
`DialContextTCP` — exec-based checks are not wired into compose, though
instance-level exec now exists via the guest agent) with
`depends_on` conditions `service_started`/`service_healthy` gating startup;
graceful stop: SIGTERM → vz-runner `requestStop`, force-stop after a
configurable grace (`run --stop-grace`, default 10 s) for guests without
ACPI handling, SIGKILL as the last resort.

Note on healthcheck semantics: the probe is a plain TCP connect — it proves
a listener exists, **not** that the application is ready (a database can
accept TCP during crash recovery long before it accepts queries). It is not
equivalent to `pg_isready`-style exec checks, which compose does not run
yet (instance-level exec exists via the guest agent, but the compose layer
does not use it for probes); use `start_period`/`retries`/`interval` tuning on
`x-healthcheck-tcp` for services that need warm-up headroom.

**Phase 4 — exit status, one-shot services, restart policies (implemented,
ADR [0007](adr/0007-compose-exit-status-oneshot-restart.md)):** `InstanceState`
carries `ExitCode`/`ExitedAt` plus a `StoppedByUser` marker; a service marked
`x-oneshot: true` — or targeted by a dependent's
`service_completed_successfully` — runs as a **job**, booting a benign init and
executing its `command` through the guest agent, whose `Exit` frame is the
exact code. Code 0 releases the dependents, a non-zero code fails `up` with the
code and the job's output. `restart:` policies are enforced by a supervision
loop in the per-project gateway daemon: it polls instance liveness and re-runs
what disappeared with capped exponential backoff, refusing to touch anything
carrying `StoppedByUser` or belonging to a project whose `Supervise` flag
teardown has cleared. Documented divergences: detection is within the poll
interval, `on-failure` degrades to `always` (with the `:N` cap honored),
`always` and `unless-stopped` are indistinguishable, jobs are never restarted,
and a job needs an agent-bearing image.

**Phase B — later (blocked on upstream urunc-dev/urunc#336):** urunit reports
the app's exit status and stops become graceful power events. That removes the
agent-image requirement for jobs, gives plain services real exit codes (making
`on-failure` exact and `ps`'s EXIT column meaningful for every instance), and
avoids block-device corruption on stop. Also still later: events/wait
primitives, block-mode guest-path mounts, fuller compose-spec fidelity
(remaining CLI verbs).

## Open questions

1. Separate `urunc-compose` binary vs `urunc-macos compose` subcommand?
   (Subcommand keeps the vz-runner-beside-binary discovery trivial.)
2. Should the compose supervisor run foreground-per-service (owns PIDs,
   reliable exit detection) or reuse detached mode + polling?
3. How much compose-spec fidelity is the target — the compose-go reference
   parser (github.com/compose-spec/compose-go) gets interpolation and
   schema for free at the cost of a heavyweight dependency.
