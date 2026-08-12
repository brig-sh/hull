# Compose stack review — July 2026

Review notes for the phase 1–4a compose series (issues #1 #3 #5 #7, PRs #2 #4
#6 #8), written against the stated goal: **a near-1:1 docker-compose
experience on macOS compared to urunc on Linux.** Detailed line-level findings
live as inline comments on the PRs; the UX gap list is tracked in issue #9.
This document records the overall assessment and how the stack compares with
the existing macOS container/VM solutions.

## Verdict

The architecture is right, and it is right in a way the review can anchor to
external evidence: every design decision the stack converged on independently
matches where the mature ecosystem ended up (user-mode gvisor netstack,
virtiofs volumes, host-side proxy for port publishing, static addressing over
discovery). The differentiating decision — preserving upstream urunc's
kernel-configured single-NIC `ip=` guest contract (ADR 0001) — is what none
of the comparables have, and it is what makes every unikernel flavor work
without guest userland cooperation.

What separates "the architecture works" from "docker muscle memory works" is
now a long tail of parsing, defaults, and feedback behaviors, collected in
issue #9. The single highest-leverage item there: **warn when compose keys are
ignored** — today `restart:`, `healthcheck:`, `entrypoint:`, `networks:` etc.
drop silently, so a stock compose file *appears* to work while behaving
differently.

## Per-PR summary

### PR #2 — phase 1: discovery, run overrides, compose up/down

Solid MVP with honest field findings (the `VZNATNetworkDeviceAttachment`
`PRIVATE`-flag discovery is documented in compose-support.md and directly
motivated the phase-2 pivot). Must-fix found: **block-mode `--add-host`
appends to the shared image rootfs in the store** (the virtiofs path clones
before mutating; block mode resolves the symlink and writes through it), so
hosts entries accumulate across runs and leak into unrelated instances.
Secondary: failed runs leave the instance dir behind and now squat the name
(duplicate `--name` is an error); `command:`/`cpus` parsing rejects common
compose forms; `run -d` blocks up to 30 s on lease discovery; the 24-bit
name-hash MAC can collide now that discovery is keyed by MAC.

### PR #4 — phase 2: single-NIC user-mode gateway

The heart of the stack, and the strongest engineering in it. gvisor-tap-vsock
membership via SCM_RIGHTS-passed datagram socketpairs, port lifetime tied to
the VMM process, static IPs allocated before boot so every service gets the
complete hosts map. Found: non-vz services still get phantom mesh IPs/hosts
entries/port forwards (fixed by phase 4a, but live if the stack merges
incrementally); gateway readiness is checked by `os.Stat` on a socket path a
crashed daemon leaves behind (dial instead); an orphan window between gateway
start and the first project-state save; `ports:` grammar covers only
`HOST:GUEST` TCP; `down` kills the recorded gateway PID without an identity
check (pid reuse).

### PR #6 — phase 3: graceful stop, multi-volumes, healthchecks

Right primitives, one real bug: the `service_healthy`-timeout failure path
tears down the gateway and project state but **orphans already-started VMs**
(the run-failure path a few lines away does it correctly). Data-safety
concern: vz-runner force-stops the guest 5 s after `requestStop`
unconditionally — a database mid-shutdown gets hard-stopped; docker's
contract here is a per-service `stop_grace_period` (default 10 s). The
issue-#5 criteria `interval`/`retries`/`start_period` are not implemented.
Volume syntax misses `:ro` suffixes, named volumes, file binds, and resolves
relative paths against the CWD rather than the compose file's directory.

### PR #8 — phase 4a: QEMU joins the gateway

Small and lands cleanly. A mixed Vz + stock-Homebrew-QEMU mesh under SIP with
no entitlements, no root, and no re-signed binaries is a capability **none of
the existing macOS solutions offer**. Remaining polish: README still carries
the re-sign-QEMU guidance the PR obsoletes, 9p mounts need `msize` (the
classic Lima/QEMU I/O complaint), and the QEMU gateway-connect failure
deserves the same friendly pre-flight error the vz path gets.

## Comparison with the macOS landscape

The structural point first: **Docker Desktop, Lima/Colima, and Podman machine
achieve compose parity by running one shared Linux VM** — real bridge
networks, docker's embedded DNS at 127.0.0.11, dockerd itself. Their compose
UX is Linux's because it *is* Linux. None of them ever solved multi-VM
networking, discovery, or lifecycle, which is the problem this stack actually
solves. The only architectural comparable is Apple's `container`
(VM-per-container, Containerization.framework, 1.0 in June 2026).

| | urunc-macos (this stack) | Docker Desktop | Lima / Colima | Apple `container` 1.x |
|---|---|---|---|---|
| Isolation model | **VM per service** (Vz or QEMU+HVF) | one shared VM | one shared VM | VM per container (Vz) |
| Guest↔guest | gvisor-tap-vsock user-mode gateway; works on macOS 13+ class APIs, SIP on, no root | Linux bridges in-VM | Linux bridges in-VM | vmnet — **requires macOS 26**; isolated before that |
| Service DNS / names | `/etc/hosts` injection from pre-allocated static IPs (gateway DNS zone suggested, PR #4) | embedded DNS 127.0.0.11 | in-VM (Linux-normal) | opt-in domains, **sudo-gated** (`container system dns create`), recurring bug reports |
| `ports:` | gateway forwards on 127.0.0.1 (TCP only today) | host proxy, 0.0.0.0 default | ssh/gRPC forwarders, auto-detect | "use the container IP"; `--publish` broke on macOS 26.1 |
| Volumes | virtiofs (Vz) / 9p (QEMU), per-share tags, guest-path automount | VirtioFS (perf is the #1 complaint) | virtiofs (vz) / 9p / sshfs | virtiofs |
| Healthchecks / `depends_on` | TCP probe through the gateway netstack + `service_healthy` | full (real dockerd) | full (real dockerd) | **none** |
| Graceful stop | QMP → SIGTERM/`requestStop` → SIGKILL chain | SIGTERM + `stop_grace_period` | in-VM | vminitd RPC, docker-like |
| compose | native subset, one VM per service | full | full (via engine in VM) | **none** (community fills in) |
| Host entitlements | virtualization only; no root, SIP on | — | vmnet modes need root helper | vmnet; networking gated on OS version |
| Mixed hypervisors on one network | **yes (Vz + QEMU)** | no | no (one VM type per instance) | no |
| Unikernel guests | **yes — kernel-configured single NIC, no guest userland needed** | no | no | no (Linux + vminitd required) |

Points of comparison worth internalizing:

- **The gateway choice is the ecosystem's converged answer.** Docker Desktop
  replaced vpnkit with the gVisor netstack in 4.19 (~5× throughput claim);
  Podman ships gvproxy (gvisor-tap-vsock) for ports and
  `host.containers.internal`. Known costs to track: userspace-netstack
  per-flow throughput, and no external ICMP forwarding in the general case.
- **Apple's `container` is the cautionary tale for per-VM UX.** Its
  guest↔guest networking needed macOS-26-only vmnet APIs (this stack needs
  none of that), DNS is sudo-gated opt-in with recurring breakage
  (#856, #1794), and port publishing regressed on a macOS point release
  (#919). Every one of those pain points is something this stack already
  avoids or can avoid cheaply — that is the UX bar, and it is beatable.
- **virtiofs performance complaints dominate every solution's issue
  tracker.** Nothing to fix today, but worth a benchmark and an eventual
  fast-path story (Docker's answer is Mutagen-based Synchronized File
  Shares).
- **`host.docker.internal` is table stakes** everywhere except Apple's tool —
  a `host.urunc.internal` alias in the gateway DNS is cheap and
  differentiating (issue #9).

## What "1:1 with docker-compose" needs next

Priority-ordered, from the review (full checklist in issue #9):

1. Warn on ignored compose keys (silent divergence → informed use).
2. Parsing parity for the forms stock files actually use: quoted `command:`
   strings, string-or-number `cpus`, `ports:` grammar (host-IP prefix, UDP,
   single-port), volume mode suffixes / named-volume detection / compose-dir
   relative paths, `${VAR}` interpolation + `.env`.
3. Lifecycle muscle memory: foreground `up` with aggregated logs and Ctrl-C
   teardown, idempotent re-`up`, `logs` for all services, `stop`/`start`/
   `restart` verbs, `stop_grace_period`.
4. `restart:` policies + exit-code capture (needs the phase-4 supervisor) —
   also unlocks `service_completed_successfully`.
5. Gateway DNS zone + `host.urunc.internal`.

None of these are architectural. The hard problems — the ones that made
docker-compose-on-macOS-without-a-big-Linux-VM impossible for everyone else —
are solved in this stack.
