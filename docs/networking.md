# Networking

hull has two network models. Which one you get changes what works, and the
difference between backends is larger here than anywhere else in hull.

- **Backend-native networking**, with `--net shared`. Each backend supplies its
  own. They are not equivalent, and on `hvi` this path carries no traffic.
- **The user-mode gateway**, with `--gateway-sock`. One process serves every
  backend the same way. This is the portable option.

For restricting what a guest may reach, see
[network-egress.md](network-egress.md). This page is about getting a guest
connected at all.

## Quick guidance

| You want | Do this |
|---|---|
| a guest that can reach the internet, least setup | `--hypervisor vz --net shared` |
| the same behavior on every backend | `--gateway-sock` |
| a guest on `hvi` that can reach anything | `--gateway-sock`. `--net shared` alone is refused |
| host ports forwarded into a guest | the gateway, with `--forward` |
| several services that talk to each other by name | `hull compose`, which sets up a gateway for you |
| an egress policy | the gateway. A policy is a gateway flag |
| no network at all | `--net none`, the default |

## Backend-native networking

`--net shared` is the flag. What serves it differs:

| | `vz` | `qemu` | `hvi` |
|---|---|---|---|
| Served by | `VZNATNetworkDeviceAttachment`, Apple NAT | `-netdev vmnet-shared` with a `virtio-net-pci` device | a responder inside the VMM |
| Real TCP egress | yes | yes | **no**, and the combination is refused |
| Guest address | DHCP on `192.168.64.0/24` | DHCP on `192.168.64.0/24` | fixed `10.0.2.15` |
| Gateway | `192.168.64.1` | `192.168.64.1` | `10.0.2.2` |
| DNS | from the DHCP answer, via `/proc/net/pnp` | from the DHCP answer, via `/proc/net/pnp` | `10.0.2.3`, and see below |
| Privilege needed | the virtualization entitlement, which a release already has | **root, or a managed entitlement** | none |

On `vz` and `qemu` the init wrapper copies the kernel's DHCP answer from
`/proc/net/pnp` into `/etc/resolv.conf`, so guest DNS is configured for you.

With `--net none`, which is the default, the guest gets no network device at
all. On `qemu` that is `-nic none`.

`--wait-ip` is useful with `--detach` and NAT: hull waits for the DHCP lease
and records the address before returning. An address is not readiness.

hull generates the guest MAC. On `qemu` it falls in the `52:54:00` range.

### `--net` is not validated

Only the literal string `none` is special-cased. Every other value, including a
typo such as `--net shred`, is treated as "networking on" and silently gets a
network device. Compare `--pull`, which rejects an unknown value.

### `qemu` and vmnet need privilege

`vmnet` requires the caller to be **root** or to hold
`com.apple.vm.networking`. That entitlement is managed: Apple must grant it to
your team and it needs a provisioning profile, so a plain Apple Development
certificate cannot sign for it. hull never signs QEMU, and CI runs the QEMU
tests against a stock Homebrew build through the gateway.

In practice, on `qemu`, use the gateway. See
[troubleshooting.md](troubleshooting.md#cannot-create-vmnet-interface-general-failure)
for the full list of options.

### `hvi --net shared` is refused

`hull run --hypervisor hvi --net shared IMAGE`, with no `--gateway-sock`, fails
with an error naming the gateway. It used to boot a guest that took an address
and reached nothing.

Two independent reasons, and neither is a missing feature:

**The built-in stack forwards nothing.** It is a user-space responder inside
the VMM. It answers ARP, ICMP echo, DHCP and DNS for a fixed address set, guest
`10.0.2.15`, gateway `10.0.2.2`, resolver `10.0.2.3`, and its TCP path ends at
the literal source line `None // no egress yet`. Any UDP that is not DHCP or a
query to its own resolver is dropped the same way. `hvi`'s own documentation
lists "no egress from the built-in network stack" among its known limits.

**The VMM confines itself before the guest runs, and could not forward even if
the code existed.** `hvi` installs a `(deny default)` Seatbelt profile as the
last thing before starting the vCPUs. Under that profile `socket(2)` still
succeeds, but `connect(2)` and `sendto(2)` both fail with `EPERM`. So the
confined VMM cannot originate traffic at all.

Its own resolver is caught by the same rule. `handle_dns` resolves through the
host's `getaddrinfo`, which needs to reach `mDNSResponder`; nothing earlier in
the boot warms that connection, so the query fails and the guest gets an answer
with no records.

That is also why the gateway path works. `hvi` opens the gateway socket
**before** confinement, and the sandbox comment names it explicitly among the
host authority acquired above that line. Everything after only services guest
I/O with what is already open.

So the gateway is not a workaround on `hvi`. It is the design.

## The user-mode gateway

`hull network-gateway` runs one process holding a gvisor netstack, a layer 2
switch, DHCP, DNS and the host port forwards. Guests reach it over a Unix
socket, which needs no entitlement and no root.

It gives the same addressing and DNS on all three backends, which no other
option does.

### Starting one by hand

```bash
hull network-gateway --socket /tmp/gw.sock &

hull run --hypervisor hvi \
  --net shared \
  --gateway-sock /tmp/gw.sock \
  --gateway-cidr 10.87.0.10/24 \
  ubuntu:latest /bin/sh -c 'getent hosts example.com'
```

Defaults: subnet `10.87.0.0/24`, with the gateway itself and the resolver both
at `10.87.0.1`.

`--gateway-sock` and `--gateway-cidr` must be given together, and
`--gateway-sock` with `--net none` is rejected.

`hull compose` starts one gateway per project, so compose users get this
without touching the flag. See [compose.md](compose.md).

### Two join mechanisms

The backends join differently, and the difference shows up in how a dead member
is noticed:

| Backends | How | Membership lifetime |
|---|---|---|
| `vz` | hull connects to the control socket, makes a datagram socketpair, hands one end to the gateway, and passes the local end to `vz-runner` as `--net-fd 3` with the control connection as fd 4 | closing the control connection removes the member, so the lifecycle is explicit |
| `qemu`, `hvi` | connect to `<control-socket>.qemu`, a stream socket speaking length-prefixed Ethernet frames | tracked only by whether the connection is open, so a wedged process keeps a dead member in the switch |

The `.qemu` socket is derived from `--socket` by appending the suffix. Despite
the name it carries **both** QEMU and `hvi` members.

### hull pre-flights the socket

For `qemu` and `hvi`, hull dials the gateway socket before starting the VM and
fails the run with a clear error if nothing is listening.

That check earns its place. On its own, `hvi` only logs a warning when it
cannot reach the gateway and falls back to the built-in no-egress stack. Without
hull's pre-flight, a run would look networked and carry no traffic.

### Port forwards, DNS records, service discovery

```bash
hull network-gateway --socket /tmp/gw.sock \
  --forward 127.0.0.1:8080=10.87.0.10:80 \
  --host db=10.87.0.11 &
```

`--forward` exposes a guest port on the host, as
`hostaddr:port=guestip:port`. It is ingress, and no egress rule applies to it.

`--host name=ip` adds a static A record the gateway's resolver serves. That is
how services find each other by name. `hull compose` writes these for you from
the service names.

### IPv6 is not forwarded

The netstack registers no IPv6 protocol, so a guest's IPv6 frame is counted as
an unsupported protocol and discarded. The gateway also hands out no `AAAA`
records, on the grounds that an address the guest cannot use is one worth not
giving it.

### Guest-to-guest traffic does not reach the netstack

Guests behind one gateway share a layer 2 switch, and traffic between them is
forwarded without reaching the netstack. That is what makes service-to-service
communication work. It also means no egress rule applies to it, which matters
if you were counting on one. See
[network-egress.md](network-egress.md#what-this-does-not-cover).

## Egress policy, in one paragraph

Filtering is off unless `--egress-default` is given. A policy belongs to the
**gateway**, not to a guest: the rules are flags on the gateway process, and
every guest behind it answers to all of them. A sandbox needing its own policy
needs its own gateway.

A `host` rule is enforced on the addresses the gateway resolved for that name,
so it is only as narrow as the address behind the name. It is not application
identity and it is not TLS authorization: a shared front end serving many names
on one address admits all of them once the guest holds it.

[network-egress.md](network-egress.md) has the whole picture, including
precedence, address rotation, direct-IP behavior, and the four things the filter
does not cover.

## What hull does not do

- No bridged networking. `vz-runner` has no bridged code path, and the
  `com.apple.vm.networking` entitlement in its second plist is not exercised by
  any behavior the runner currently has.
- No IPv6 forwarding through the gateway, as above.
- No `--net-tap`. That is an `hvi` flag and it is Linux-only; on macOS `hvi`
  refuses it and names the gateway flag instead. `--net-mac` likewise has no
  effect on macOS.
