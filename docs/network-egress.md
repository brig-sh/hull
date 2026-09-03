# Egress filtering in the network gateway

`hull network-gateway` gives every guest in a project its NIC: one userspace
process holding a gvisor netstack, a layer 2 switch, DHCP, DNS and the host
port forwards. It is also the one place every packet a guest sends to the
outside world already passes through, which is where egress filtering sits.

Filtering is off unless `--egress-default` is given. Without it the gateway
behaves as it always has and forwards everything.

## Give each sandbox its own gateway

**A policy belongs to a gateway, not to a guest.** The rules are flags on the
gateway process, and every microVM behind that gateway answers to all of them.
No rule can be written to apply to one member and not another.

So a sandbox that needs an egress policy of its own needs a network of its
own. Run one gateway per sandbox and the boundary lands where you want it: one
gateway, one guest, one policy. That is the intended shape, and every claim on
this page about what a guest can reach assumes it.

Sharing a gateway shares the policy, in both directions. `hull compose up`
starts one gateway per project, so every service in a project answers to the
same rules, and `hull run --gateway-sock` joins an existing gateway and takes
its policy. That suits a project whose services are one unit of trust. It does
not suit two agents that must not reach the same things: put those on separate
gateways.

Per-sandbox networks are brig-sh/brig#15.

## The flags

```
--egress-default allow|deny        the verdict for a connection no rule matches
--egress-allow host=<glob>         repeatable
--egress-allow cidr=<cidr>         repeatable
--egress-deny  host=<glob>         repeatable
--egress-deny  cidr=<cidr>         repeatable
```

A sandbox that should reach two APIs:

```
hull network-gateway --socket ... \
  --egress-default deny \
  --egress-allow host=api.example.com \
  --egress-allow 'host=*.githubusercontent.com'
```

A sandbox that may reach the internet but not the host's own networks:

```
hull network-gateway --socket ... \
  --egress-default allow \
  --egress-deny cidr=10.0.0.0/8 \
  --egress-deny cidr=192.168.0.0/16 \
  --egress-deny cidr=169.254.0.0/16
```

## How a rule is matched

A `cidr` rule is matched against the address a connection is opened to. It
takes IPv4 and IPv6 prefixes, and a rule of one family never covers the other:
`cidr=0.0.0.0/0` does not admit an IPv6 address.

A `host` rule is a glob matched against the name a guest asks the gateway's
resolver for. `*` spans dots, so `*.example.com` covers `a.example.com` and
`a.b.example.com`, but not `example.com` itself; list the apex separately if
you want it. Names are compared case-folded and without the trailing dot.

Precedence is deny, then allow, then the default, decided per connection. A
`cidr` rule and a `host` rule sit at the same level: any deny beats any allow.

## Why host globs work

A packet carries an address, not a name, so a name has to be turned into
addresses before it can be enforced. The gateway's resolver is the only one a
guest can reach, so it does that itself: an answer it gives out puts its
addresses in that guest's allow set, and the connection check consults the
set. Pins are per guest, cover both address families, and expire; a guest that
never asked never inherits another's.

The gateway tells the guest the same TTL it enforces, so what the guest caches
and what the gateway honours cannot drift apart, and adds a short grace period
for a guest that connects on an answer that has just expired.

## A host rule authorizes an address

The filter sees addresses. A `host` rule is enforced on the addresses the
gateway resolved for that name, so it admits whatever else answers on them.
Two names behind one address are one destination here.

That matters for shared front ends. If `api.example.com` sits behind a CDN or
a reverse proxy, its address serves other names too, and a guest reaches them
with its own `Host` header or SNI. The resolver refusing those names does not
stop it, because the guest already holds an address that serves them.

So a `host` rule is as narrow as the address behind the name. For an API on a
dedicated address it is exact. For one on shared infrastructure it is not, and
a `cidr` deny is what narrows it.

## Names whose addresses move

Pinning covers a guest that asks again. Plenty of runtimes do not: a resolver
that caches past the TTL, or an application that holds an address for the life
of the process, will keep using an address the gateway has since forgotten,
and a name behind a rotating record set will hand out an address that was
never pinned. The name is allowed and the connection is refused anyway.

So the gateway also resolves the named hosts in its rules on a timer,
`--egress-refresh` (30s by default, `0` disables), and keeps whatever they
currently answer with. An address that stops appearing loses its place after
three rounds, so a resolver that fails once does not cut a sandbox's egress,
and one that stays broken does not keep an address alive forever.

These addresses are not held per guest, and a pin is. The operator wrote the
host into the policy, so it is reachable for every guest behind that gateway,
whether or not the guest ever asked for the name. That grants nothing a guest
could not already have had by resolving the name itself, because one gateway
carries one policy for everything on it.

This works for a rule that names one host, `host=api.example.com`. A glob
cannot be resolved ahead of a query, because there is no way to enumerate what
`*.example.com` stands for, so globs stay driven by what guests ask for.

Resolving a `host` deny on the same timer is what makes it more than best
effort under `--egress-default allow`: the addresses the name answers with are
blocked whether or not the guest asked this resolver for them.

Under `--egress-default deny` this has a second effect. A query for a name no
allow glob covers is answered `REFUSED`, so a guest cannot learn an address
here that it is not allowed to reach. Traffic sent straight to an IP,
DNS-over-HTTPS and DNS-over-TLS then have nothing to reach: they are dead ends
because of that rule, not because they are named anywhere.

The flip side is worth stating plainly. Under `--egress-default deny`, a guest
resolves only what an allow glob covers, so a policy written entirely in
`cidr` rules leaves guests with no name resolution at all. Write the names you
want resolvable as host globs.

Under `--egress-default allow`, a `host` deny is best effort. The guest is
free to reach an address it learned somewhere else, and traffic sent straight
to an IP never asks for a name. Resolving the name on a timer closes most of
that gap for a rule naming one host, but not for a glob. A `cidr` deny has no
such gap at all.

## What the gateway refuses to start with

A rule it cannot parse stops it, naming the rule. So does `--egress-allow` or
`--egress-deny` without `--egress-default`, which would enforce nothing.
Unknown flags are refused as they always have been, so an older gateway handed
these flags fails rather than running wide open while its operator believes it
is filtering.

## What this does not cover

**Guest to guest.** Guests behind one gateway share a switch, and traffic
between them is forwarded at layer 2 without reaching the netstack. No egress
rule applies to it, and `--egress-default deny` does not separate one guest
from another. Separation is a network per sandbox, as above.

**Ingress.** `--forward` exposes a guest's port on the host. It is a host
exposure, it is not egress, and no egress rule applies to it.

**ICMP.** A guest's ping is answered by the gateway itself: the netstack
treats the destination as one of its own addresses and replies, so an echo
request never leaves the host. A ping to a blocked address still succeeds and
still tells the guest nothing about the outside world.

**Source addresses.** The policy reads the source address out of the packet,
and a guest picks its own. One guest can therefore borrow another's resolved
answers by claiming its address, which the shared switch makes possible in the
first place. It widens nothing: a gateway carries one policy for every guest
on it, so those addresses are the ones the borrower's own queries would have
been answered with anyway. Binding an address to a member belongs with a
network per sandbox, not with a rule here.

**IPv6.** The netstack does not forward IPv6 yet: it registers no IPv6
protocol, so a guest's IPv6 frame is counted as an unsupported protocol and
discarded, with or without a policy. The gateway also hands out no `AAAA`
records, since an address the guest cannot use is one worth not giving it. The
filter itself is written for both families already: `cidr` rules take IPv6
prefixes and pins hold IPv6 addresses. What is missing is the forwarding, not
the filtering.
