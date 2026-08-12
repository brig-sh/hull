# ADR 0001: Single-NIC user-mode network gateway for compose networking

**Status**: Accepted
**Date**: 2026-07-08
**Context**: How compose services on urunc-macos get service-to-service, egress, DNS, and port-forwarding networking without breaking guest parity with urunc on Linux

---

## Context

Compose on urunc-macos maps each service to one lightweight VM. Services must
reach each other, reach the internet, and be reachable from the host — and the
guest-visible environment should stay 1:1 with generic urunc deployments on
Linux, where a guest sees **one interface, statically configured by the kernel**
from an `ip=<addr>::<gw>:<mask>:urunc:eth0:off` command-line parameter
(upstream `unikernels/linux.go`), with zero guest-userland dependencies —
which is what makes every unikernel flavor work, not just full Linux images.

Field findings that force a decision:

- Apple's `VZNATNetworkDeviceAttachment` marks its bridge ports with the
  kernel `PRIVATE` flag: guests reach the host and the internet but **cannot
  reach each other** (ARP never resolves), and macOS exposes no toggle.
- `VZBridgedNetworkDeviceAttachment` requires the Apple-managed
  `com.apple.vm.networking` entitlement (application to Apple, provisioning
  profile) — unavailable in practice and heavier semantics (guests join the
  physical LAN).
- An interim dual-NIC design (NAT + second NIC on a hand-rolled L2 switch,
  configured by an init-wrapper running `ip` from a private `urunc.net2=`
  kernel parameter) worked, but broke parity three ways: guests saw a
  two-NIC topology that Linux urunc guests never see, eth1 configuration
  depended on iproute2 existing in the image, and shell-less unikernels
  (unikraft, mirage, rumprun, mewz) were excluded entirely.

The kernel's in-kernel `ipconfig` can only configure one interface, so any
multi-NIC design necessarily leaves the upstream contract.

Alternatives considered: (a) dual-NIC with the guest config moved into urunit
— fixes the userland dependency but keeps the foreign topology and only
covers urunit images; (b) upstreaming the `urunc.net2=` convention — documents
the divergence without removing it; (c) single NIC on a user-mode gateway —
this decision.

## Decision

We will give each compose service **exactly one NIC**, backed by
`VZFileHandleNetworkDeviceAttachment` over a datagram socket connected to a
per-project **user-mode gateway** process (hidden `network-gateway`
subcommand) that embeds gvisor-tap-vsock's netstack. The gateway provides L2
switching between services, NAT egress, DNS forwarding through the host
resolver, and host-side port forwarding (compose `ports:`).

Key principles:

- **The guest contract is upstream's contract.** The kernel command line
  carries the same static line urunc uses on Linux —
  `ip=<addr>::<gw>:<mask>:urunc:eth0:off:<dns>` (the trailing `dns0` field is
  the only addition; it lands in `/proc/net/pnp` and the existing init
  wrappers already copy it to `resolv.conf`). No guest userland tooling, no
  private kernel parameters, no extra interfaces.
- **Membership is fd-passing.** VMs join by SCM_RIGHTS-passing one end of a
  `socketpair(AF_UNIX, SOCK_DGRAM)`; the control connection is inherited by
  the VMM process so a member's port lives exactly as long as its VM.
- **Static addressing.** The compose layer allocates all service IPs before
  boot, so the complete name→IP `/etc/hosts` map is injected into every
  service regardless of start order, and port forwards are configured at
  gateway start.
- Plain `run` invocations without `--gateway-sock` keep the existing Apple
  NAT path unchanged.

## Consequences

### Positive

- Guest-visible topology, configuration mechanism, and `ip=` format are
  identical to urunc on Linux; images and unikernel flavors need nothing
  extra (no shell, no iproute2).
- No entitlements beyond `com.apple.security.virtualization`, no root, works
  with SIP enabled; verified: guest↔guest ping 0% loss, DNS resolution and
  ICMP egress 0% loss, all concurrently.
- One mechanism replaces three planned ones: service mesh, compose DNS, and
  `ports:` forwarding all come from the same gateway (former Phase 2/3 items
  merged).
- The vmnet lease-scraping discovery becomes unnecessary for compose
  (addresses are assigned, not discovered).

### Negative

- gvisor-tap-vsock (and gvisor's netstack) is a heavyweight dependency, and
  all service traffic now flows through a userspace TCP/IP stack — per-flow
  throughput will trail Apple's in-kernel NAT path; acceptable for compose
  workloads, unmeasured at scale.
- A per-project daemon is new operational surface: if the gateway dies, the
  project loses all networking (VM ports die with their control fds, but a
  crashed gateway needs `compose down && up`).
- ICMP egress semantics are gateway-emulated (gvisor answers/proxies pings);
  fine for reachability checks, not a transparent ICMP path.

### Neutral

- **Convention: the gateway always takes network+1** on the project subnet.
  Both sides derive from it — compose passes the gateway address explicitly
  to the daemon, and `run` derives the guest's default route (and `dns0`)
  as network+1 from its `--gateway-cidr`. Changing one side without the
  other breaks routing.
- The gateway subnet (default `10.87.0.0/24`, overridable with
  `compose up --subnet`) is private per project;
  cross-project traffic is intentionally impossible.
- Host→service access is exclusively via declared `ports:` forwards
  (127.0.0.1), matching Docker Desktop semantics rather than Linux-bridge
  semantics where the host can reach service IPs directly.
