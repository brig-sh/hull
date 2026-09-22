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
| a guest on `hvi` that can reach anything | `--gateway-sock`. `--net shared` will not work |
| host ports forwarded into a guest | the gateway, with `--forward` or the `/forwards` API |
| several services that talk to each other by name | `hull compose`, which sets up a gateway for you |
| an egress policy | the gateway. A policy is a gateway flag |
| no network at all | `--net none`, the default |

## Backend-native networking

`--net shared` is the flag. What serves it differs:

| | `vz` | `qemu` | `hvi` |
|---|---|---|---|
| Served by | `VZNATNetworkDeviceAttachment`, Apple NAT | `-netdev vmnet-shared` with a `virtio-net-pci` device | a responder inside the VMM |
| Real TCP egress | yes | yes | **no** |
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

### `hvi --net shared` gives no egress

This is the most important thing on this page.

`hvi`'s built-in stack is a user-space responder inside the VMM. It answers
ARP, ICMP echo, DHCP and DNS for a fixed address set, and it **drops guest TCP
after logging it**. ARP is answered only for the gateway and DNS addresses.
`hvi`'s own documentation lists "no egress from the built-in network stack"
among its known limits.

A guest there gets an address, and cannot open a connection to anything.

Its DNS has a second problem. The handler resolves names by calling the host's
`getaddrinfo`, and it runs after `hvi` installs its Seatbelt sandbox. That
profile is deny-by-default with no network grant, and `hvi`'s own selftest
asserts that opening an outbound socket fails once the sandbox is entered. So
built-in DNS is expected to answer with an empty record set. hull never passes
`--no-sandbox`, so every `hvi` instance hull starts runs confined.

hull still writes `nameserver 10.0.2.3` into the boot initrd for an `hvi`
generic container boot with any net mode other than `none`, so the guest is
pointed at that resolver whether or not it can answer.

Use the gateway on `hvi`.

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
something else did -- and 400 for a forward the gateway cannot read. A
`DELETE` answers 200 with the forward that went, 404 when nothing holds that
address, and 400 for a protocol or an address it cannot read, which is what
`POST` answers for the same input.

`local` takes the spellings the flag takes, so `:3000` means every interface
and reads back as `0.0.0.0:3000`, and a host named rather than addressed is a
400.

The guest side must be an IPv4 address. The gateway forwards no IPv6 and the
guest network is IPv4-only, so an IPv6 remote is refused with a 400 rather than
reaching the forwarder and failing there.

The `--api` socket binds host ports, so treat it as a control socket rather
than a probe. Anything that can open it can publish a port, `0.0.0.0`
included. The gateway gives it mode `0600` before it answers under that name,
so the gate is the owner rather than whatever the umask happened to leave.
Keep it that way, and do not hand it to a guest. The control and QEMU sockets
are not narrowed: a member connects to those, and what may reach them is a
separate question from who may publish a port.

This is the one thing about a running gateway that can change. The subnet and
the egress rules are read once at startup, so changing either means restarting
the gateway, which drops every member of its network.

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
