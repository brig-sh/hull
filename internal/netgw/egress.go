// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package netgw

import (
	"fmt"
	"net/netip"
	"path"
	"strings"
	"sync"
	"time"
)

// Egress policy: what a guest behind this gateway is allowed to reach.
//
// A rule is either a CIDR, matched against the address a connection is
// opened to, or a host glob, matched against the name a guest asks the
// gateway's resolver for. Names are pinned: an answer the resolver gives out
// puts its addresses in a per-guest set for as long as that answer is good,
// and the connection check consults that set. So a host glob is enforced on
// addresses this gateway itself handed out, and nothing else.
//
// Precedence is deny, then allow, then the default, evaluated per connection.
// A CIDR rule and a pinned host rule sit at the same level: any deny beats
// any allow.

// pinTTL is how long the gateway advertises an answer for, and pinGrace is
// the extra time a pinned address stays usable after that.
//
// The gateway rewrites the TTL of every answer it forwards to pinTTL, so the
// lifetime it tells the guest and the lifetime it enforces are the same
// number rather than two numbers that have to be reconciled. The grace covers
// a guest that connects on an answer that expired between the lookup and the
// SYN.
const (
	pinTTL   = 60 * time.Second
	pinGrace = 60 * time.Second
)

// EgressDefault is the verdict for a connection no rule matches.
type EgressDefault string

const (
	// EgressUnset means no filtering: the gateway forwards everything.
	EgressUnset EgressDefault = ""
	// EgressAllow forwards what no deny rule matches.
	EgressAllow EgressDefault = "allow"
	// EgressDeny forwards only what an allow rule matches.
	EgressDeny EgressDefault = "deny"
)

// ruleSet is one side of the policy: the CIDRs and host globs of either the
// allow rules or the deny rules.
type ruleSet struct {
	cidrs []netip.Prefix
	hosts []string
}

func (r ruleSet) matchesAddr(addr netip.Addr) bool {
	for _, c := range r.cidrs {
		if c.Contains(addr) {
			return true
		}
	}
	return false
}

func (r ruleSet) matchesHost(name string) bool {
	for _, glob := range r.hosts {
		if ok, err := path.Match(glob, name); err == nil && ok {
			return true
		}
	}
	return false
}

// Policy is a parsed egress policy plus the DNS answers pinned under it.
type Policy struct {
	def   EgressDefault
	allow ruleSet
	deny  ruleSet

	mu   sync.Mutex
	pins map[netip.Addr]*guestPins
}

// guestPins holds one guest's pinned addresses and when each stops counting.
type guestPins struct {
	allow map[netip.Addr]time.Time
	deny  map[netip.Addr]time.Time
}

// ParseEgressPolicy turns the --egress-* flags into a policy. It returns nil
// when filtering is off, and an error naming the offending rule when a rule
// does not parse: the gateway refuses to start rather than enforce part of
// what the operator asked for.
func ParseEgressPolicy(def string, allowRules, denyRules []string) (*Policy, error) {
	d := EgressDefault(def)
	switch d {
	case EgressUnset:
		if len(allowRules)+len(denyRules) > 0 {
			return nil, fmt.Errorf("--egress-allow and --egress-deny need --egress-default allow|deny; without it nothing is enforced")
		}
		return nil, nil
	case EgressAllow, EgressDeny:
	default:
		return nil, fmt.Errorf("invalid --egress-default %q, expected allow or deny", def)
	}

	p := &Policy{def: d, pins: map[netip.Addr]*guestPins{}}
	var err error
	if p.allow, err = parseRules("--egress-allow", allowRules); err != nil {
		return nil, err
	}
	if p.deny, err = parseRules("--egress-deny", denyRules); err != nil {
		return nil, err
	}
	// Deny-default with no allow rules is not an error: a sandbox that should
	// reach nothing is something an operator asks for on purpose.
	return p, nil
}

func parseRules(flag string, rules []string) (ruleSet, error) {
	var set ruleSet
	for _, rule := range rules {
		kind, value, ok := strings.Cut(rule, "=")
		if !ok {
			return ruleSet{}, fmt.Errorf("invalid %s %q, expected host=<glob> or cidr=<cidr>", flag, rule)
		}
		switch kind {
		case "cidr":
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return ruleSet{}, fmt.Errorf("invalid %s %q: %q is not a CIDR", flag, rule, value)
			}
			set.cidrs = append(set.cidrs, prefix.Masked())
		case "host":
			glob := normalizeName(value)
			if glob == "" {
				return ruleSet{}, fmt.Errorf("invalid %s %q: the host glob is empty", flag, rule)
			}
			if _, err := path.Match(glob, "example.com"); err != nil {
				return ruleSet{}, fmt.Errorf("invalid %s %q: %v", flag, rule, err)
			}
			set.hosts = append(set.hosts, glob)
		default:
			return ruleSet{}, fmt.Errorf("invalid %s %q, expected host=<glob> or cidr=<cidr>", flag, rule)
		}
	}
	return set, nil
}

// normalizeName puts a name or a glob in the one form everything else
// compares against: lower case, no trailing dot.
func normalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// Default reports the verdict for a connection no rule matches.
func (p *Policy) Default() EgressDefault {
	if p == nil {
		return EgressUnset
	}
	return p.def
}

// HasHostRules reports whether any rule is written against a name, which is
// what makes the resolver part of the enforcement path.
func (p *Policy) HasHostRules() bool {
	return p != nil && (len(p.allow.hosts) > 0 || len(p.deny.hosts) > 0)
}

// AllowsQuery reports whether the resolver should answer a query for name at
// all. Under deny-default a name no allow glob matches is refused: an address
// the guest never learns is an address it cannot reach, and that is what
// makes direct-to-IP, DoH and DoT dead ends rather than special cases.
//
// Under allow-default every query is answered. A deny glob is still applied
// to the answer, by pinning what came back into the guest's deny set.
func (p *Policy) AllowsQuery(name string) bool {
	if p == nil || p.def == EgressAllow {
		return true
	}
	name = normalizeName(name)
	if p.deny.matchesHost(name) {
		return false
	}
	return p.allow.matchesHost(name)
}

// DeniesQuery reports whether name matches a deny glob, so its answers are
// pinned as unreachable.
func (p *Policy) DeniesQuery(name string) bool {
	return p != nil && p.deny.matchesHost(normalizeName(name))
}

// Pin records the addresses an answer carried, for the guest that asked.
// deny says which set they land in.
func (p *Policy) Pin(guest netip.Addr, addrs []netip.Addr, deny bool) {
	if p == nil || len(addrs) == 0 {
		return
	}
	expiry := time.Now().Add(pinTTL + pinGrace)
	guest = guest.Unmap()

	p.mu.Lock()
	defer p.mu.Unlock()
	pins := p.pins[guest]
	if pins == nil {
		pins = &guestPins{allow: map[netip.Addr]time.Time{}, deny: map[netip.Addr]time.Time{}}
		p.pins[guest] = pins
	}
	set := pins.allow
	if deny {
		set = pins.deny
	}
	for _, addr := range addrs {
		set[addr.Unmap()] = expiry
	}
	p.sweepLocked()
}

// sweepLocked drops expired pins. A guest address comes back into use when
// its lease is recycled, so a pin that outlived its answer would hand the
// next guest on that address the previous guest's allowances.
func (p *Policy) sweepLocked() {
	now := time.Now()
	for guest, pins := range p.pins {
		for addr, expiry := range pins.allow {
			if now.After(expiry) {
				delete(pins.allow, addr)
			}
		}
		for addr, expiry := range pins.deny {
			if now.After(expiry) {
				delete(pins.deny, addr)
			}
		}
		if len(pins.allow) == 0 && len(pins.deny) == 0 {
			delete(p.pins, guest)
		}
	}
}

func (p *Policy) pinned(guest, dst netip.Addr, deny bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pins := p.pins[guest.Unmap()]
	if pins == nil {
		return false
	}
	set := pins.allow
	if deny {
		set = pins.deny
	}
	expiry, ok := set[dst.Unmap()]
	return ok && time.Now().Before(expiry)
}

// AllowsConnection is the verdict for one connection from guest to dst. Deny
// beats allow beats the default, and a pinned name counts the same as a CIDR
// on its side.
func (p *Policy) AllowsConnection(guest, dst netip.Addr) bool {
	if p == nil {
		return true
	}
	guest, dst = guest.Unmap(), dst.Unmap()
	if p.deny.matchesAddr(dst) || p.pinned(guest, dst, true) {
		return false
	}
	if p.allow.matchesAddr(dst) || p.pinned(guest, dst, false) {
		return true
	}
	return p.def == EgressAllow
}

// Summary describes the policy for the gateway's startup line.
func (p *Policy) Summary() string {
	if p == nil {
		return "egress unfiltered"
	}
	return fmt.Sprintf("egress default %s (%d allow, %d deny)",
		p.def, len(p.allow.cidrs)+len(p.allow.hosts), len(p.deny.cidrs)+len(p.deny.hosts))
}
