# Egress filtering in the network gateway

`hull network-gateway` gives every guest in a project its NIC: one userspace
process holding a gvisor netstack, a layer 2 switch, DHCP, DNS and the host
port forwards. It is also the one place every packet a guest sends to the
outside world already passes through, which is where egress filtering sits.

Filtering is off unless `--egress-default` is given. Without it the gateway
behaves as it always has and forwards everything.

## Which guests a rule covers

A rule with no prefix covers every microVM behind the gateway. A rule written
`<member>:host=...` or `<member>:cidr=...` covers that member alone:

```
hull network-gateway --socket ... \
  --egress-default deny \
  --egress-allow 'host=*.githubusercontent.com' \
  --egress-allow api:host=api.example.com \
  --egress-deny  db:cidr=0.0.0.0/0
```

Every guest may reach `*.githubusercontent.com`. Only `api` may reach
`api.example.com`. `db` may reach nothing outside the subnet at all. A member
with rules of its own answers to those and to the unprefixed ones, and deny
still beats allow, so a member deny narrows a shared allow.

The member is the compose service name, or the instance name for a `hull run`.
Pass `--gateway-member` to a `run` to choose the name it answers to. A member
that joins without a name, which is what an older hull does, is covered by the
unprefixed rules only.

## What makes a member name trustworthy

A packet's source address is written by the guest, so a rule keyed on it would
be a rule a guest could opt into by lying. The gateway does not read identity
from the frame. Each member declares who it is when it joins, in the same
message that hands over its socket, and the gateway then holds it to that:
every frame arriving on that socket must carry that member's own IP and MAC,
and any frame that does not is dropped.

Both addresses matter. The IP source is what the policy reads, so a guest
writing another member's address would inherit its rules. The MAC source is
what the switch builds its forwarding table from, so a guest writing another
member's MAC would take over its port and receive its traffic. ARP carries its
own copy of both, so those are checked against the same member too.

A guest that has no address yet may send DHCP from `0.0.0.0` and may send an
ARP probe. Nothing else is allowed from an address the member does not hold.

## Still give each sandbox its own gateway

Per-member rules decide what a guest may reach outside. They do not separate
guests from each other. Traffic between two guests on one gateway is switched
at layer 2 and never reaches the filter, so a member denied all egress can
still reach a member that has it, and use it. Nothing in a rule changes that.

So an agent that must not reach what another agent reaches still needs a
gateway of its own. Per-member rules are for guests that already trust each
other and need different reach: the services of one project, where the
database talks to nothing and the API to one vendor.

Per-sandbox networks are brig-sh/brig#15.

## The flags

```
--egress-default allow|deny             the verdict for a connection no rule matches
--egress-allow [<member>:]host=<glob>   repeatable
--egress-allow [<member>:]cidr=<cidr>   repeatable
--egress-deny  [<member>:]host=<glob>   repeatable
--egress-deny  [<member>:]cidr=<cidr>   repeatable
--egress-refresh <duration>             how often to re-resolve named hosts
```

A sandbox that should reach two APIs and nothing else:

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

These addresses are not held per guest, and a pin is. They are held under the
scope of the rule that resolved them: an unprefixed rule makes them reachable
for every member, and a `<member>:` rule for that member alone. A guest that
never asked for the name still reaches what an unprefixed rule resolved, which
grants nothing it could not have had by resolving the name itself.

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
from another, whether the rule names a member or not. Separation is a network
per sandbox, as above.

**Ingress.** `--forward` exposes a guest's port on the host. It is a host
exposure, it is not egress, and no egress rule applies to it.

**ICMP.** A guest's ping is answered by the gateway itself: the netstack
treats the destination as one of its own addresses and replies, so an echo
request never leaves the host. A ping to a blocked address still succeeds and
still tells the guest nothing about the outside world.

**IPv6.** The netstack does not forward IPv6 yet: it registers no IPv6
protocol, so a guest's IPv6 frame is counted as an unsupported protocol and
discarded, with or without a policy. The gateway also hands out no `AAAA`
records, since an address the guest cannot use is one worth not giving it. The
filter itself is written for both families already: `cidr` rules take IPv6
prefixes and pins hold IPv6 addresses. What is missing is the forwarding, not
the filtering.
