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
| host ports forwarded into a guest | the gateway, with `--forward` or the `/forwards` API |
| a virtual address that spreads connections over several guests | the gateway, with `--service-cidr` and the `/services` API |
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
Prefix the host address with `udp:` for a UDP forward.

Leaving the host address out, as `:8080=10.87.0.10:80`, means every interface.
It is recorded as `0.0.0.0:8080`, so a read of the set names the address the
gateway bound. The host must be an address and not a name: a forward published
as `localhost:8080` binds `127.0.0.1:8080`, and could then not be withdrawn
under either spelling.

Two forwards cannot share a local address, and the gateway refuses the second
rather than starting. Whether two *different* addresses can share a port is
the host kernel's answer, not the gateway's: macOS binds `0.0.0.0:8080` beside
`127.0.0.1:8080` for TCP, because Go asks for `SO_REUSEADDR` on every TCP
listener, and refuses the same pair for UDP. Linux refuses both. So the
gateway offers the pair to the kernel and reports what it says.

`--host name=ip` adds a static A record the gateway's resolver serves. That is
how services find each other by name. `hull compose` writes these for you from
the service names.

### Publishing a port on a running gateway

The forwards a gateway starts with are not the only ones it can have. With
`--api` it serves `/forwards` on that socket, and the set can be changed while
guests are attached:

```bash
hull network-gateway --socket /tmp/gw.sock --api /tmp/gw.api &

# what is published
curl --unix-socket /tmp/gw.api http://gw/forwards

# publish 10.87.0.10:3000 on the host's 127.0.0.1:3000
curl --unix-socket /tmp/gw.api -X POST http://gw/forwards \
  -d '{"protocol":"tcp","local":"127.0.0.1:3000","remote":"10.87.0.10:3000"}'

# take it away again
curl --unix-socket /tmp/gw.api -X DELETE \
  'http://gw/forwards?protocol=tcp&local=127.0.0.1:3000'
```

`protocol` may be left out and means `tcp`. A `POST` answers 201 with the
forward **as installed** -- the defaults filled in, so a request that omitted
the protocol is answered `tcp` and a later `GET` agrees with it. It answers 409
when something is already published on that local address -- including a
a port the host already holds, whether this gateway published it or
something else did -- 400 for a forward the gateway cannot read, and 422 for
a remote it cannot reach. A
`DELETE` answers 200 with the forward that went, 404 when nothing holds that
address, and 400 for a protocol or an address it cannot read, which is what
`POST` answers for the same input.

`local` takes the spellings the flag takes, so `:3000` means every interface
and reads back as `0.0.0.0:3000`, and a host named rather than addressed is a
400.

The guest side must be an IPv4 address. The gateway forwards no IPv6 and the
guest network is IPv4-only, so an IPv6 remote is refused with a 400 rather than
reaching the forwarder and failing there.

The remote must also be one the gateway can reach: a guest on the subnet, or a
service address when the gateway has `--service-cidr` (see
[Services](#services)). Any other address is refused with a 422. The netstack
routes only the subnet, so such a forward would listen and then fail every
connection. The same rule applies to `--forward`, and a gateway given an
unreachable one refuses to start.

The `--api` socket binds host ports and, with `--service-cidr`, names the host
addresses service traffic is carried to, so treat it as a control socket
rather than a probe. Anything that can open it can publish a port, `0.0.0.0`
included. The gateway gives it mode `0600` before it answers under that name,
so the gate is the owner rather than whatever the umask happened to leave.
Keep it that way, and do not hand it to a guest. The control and QEMU sockets
are not narrowed: a member connects to those, and what may reach them is a
separate question from who may publish a port.

The forwards and the service table below are the parts of a running gateway
that can change. The subnet and the egress rules are read once at startup, so
changing either means restarting the gateway, which drops every member of its
network.

### Services

With `--service-cidr` the gateway routes a range of virtual addresses the way
a Kubernetes ClusterIP Service does. A guest connects to a virtual address and
port, and the gateway carries the connection to one of the endpoints its
service table lists for that address, port and protocol.

```bash
hull network-gateway --socket /tmp/gw.sock --api /tmp/gw.sock.api \
  --service-cidr 10.96.0.0/12 &

# replace the whole table
curl --unix-socket /tmp/gw.sock.api -X PUT http://gw/services -d '[
  {"vip":"10.96.0.10","port":53,"protocol":"udp",
   "endpoints":[{"ip":"10.87.0.12","port":53}]},
  {"vip":"10.96.0.1","port":443,"protocol":"tcp",
   "endpoints":[{"ip":"127.0.0.1","port":6443,"host":true}]}
]'

# read it back
curl --unix-socket /tmp/gw.sock.api http://gw/services
```

How it works:

- A guest's default route is the gateway, so a packet for a virtual address
  leaves the switch and reaches the netstack, as egress does. The forwarder
  checks the service range before anything else.
- A guest endpoint must be on the subnet. The gateway dials it from inside the
  virtual network, the way `/probe/tcp` does. An endpoint with `"host":true` is
  dialed from the host instead, so `127.0.0.1` there is the host itself. That
  is how a guest reaches a server on the host under a virtual address.
- Endpoints take turns. Each TCP connection starts at the next one, and a TCP
  endpoint that refuses or does not answer within 3 seconds is skipped for the
  one after it. A UDP endpoint is chosen once per flow.
- An address in the range with no service, or a service with no endpoints, is
  refused: TCP gets a reset and UDP is dropped. Neither is dialed from the
  host.

A forward may name a service address as its remote. That is how a service is
published on the host, the way a Kubernetes NodePort is:

```bash
curl --unix-socket /tmp/gw.sock.api -X POST http://gw/forwards \
  -d '{"protocol":"tcp","local":"0.0.0.0:30080","remote":"10.96.0.20:80"}'
```

Such a forward is not handed to the netstack, which has no route to the
service range. Each host connection, or each UDP flow, is resolved through the
service table when it arrives, with the same turns, the same failover and the
same guest or host dialing as a guest's own connection. So the forward can be
published before the service has endpoints, or before the service exists at
all: until the table names an endpoint, a TCP connection is reset and a
datagram is dropped. A forward to a service address on a gateway without
`--service-cidr` is refused with a 422.

The API:

- `GET /services` answers 200 with the table as a JSON array, ordered by
  address, port and protocol.
- `PUT /services` takes a JSON array and replaces the whole table. The caller
  keeps the source of truth and sends all of it every time, so the gateway
  keeps no history and a restarted caller only has to send it again. The
  response is 200 with the table as installed.
- `protocol` is `tcp` or `udp` in either case, and may be left out to mean
  `tcp`. `host` may be left out to mean a guest endpoint.
- A body that is not a JSON array of services, or that carries a field the
  gateway does not know, is 400. A table the gateway cannot route is 422: a
  virtual address outside `--service-cidr`, an unknown protocol, a port of 0,
  a guest endpoint off the subnet, or one address, port and protocol listed
  twice. A gateway started without `--service-cidr` answers 422 to every
  `PUT` with a service in it.
- A refused table changes nothing. The table already installed stays.

`--service-cidr` must be IPv4 and must not overlap `--subnet`. A guest reaches
an address on the subnet over the switch, so a virtual address there would
never reach the forwarder.

Two things to know before relying on it:

- **The endpoint sees the gateway as the source.** There is no NAT. The
  gateway accepts the guest's connection and opens a second one to the
  endpoint, so a guest endpoint sees the gateway's address (`10.87.0.1` by
  default) and a host endpoint sees a loopback or host address. Nothing that
  identifies the client guest by source address works through a service.
- **Service traffic is not egress.** No egress rule applies to it, under any
  `--egress-default`. The destination is the table's choice and not the
  guest's: a guest endpoint is on this network, and a host endpoint is one the
  holder of the API socket named. That makes a host endpoint a hole in an
  egress policy that only the API socket's owner can open, which is one more
  reason to keep that socket out of a guest's reach.

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
precedence, address rotation, direct-IP behavior, and what the filter does not
cover.

## What hull does not do

- No bridged networking. `vz-runner` has no bridged code path, and the
  `com.apple.vm.networking` entitlement in its second plist is not exercised by
  any behavior the runner currently has.
- No IPv6 forwarding through the gateway, as above.
- No `--net-tap`. That is an `hvi` flag and it is Linux-only; on macOS `hvi`
  refuses it and names the gateway flag instead. `--net-mac` likewise has no
  effect on macOS.
