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
	"context"
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
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

// literalHosts returns the host rules that name one host rather than a set of
// them. A glob cannot be resolved ahead of a query, because there is no way to
// enumerate what "*.example.com" stands for, so only these can be kept warm.
func (r ruleSet) literalHosts() []string {
	var out []string
	for _, glob := range r.hosts {
		if !strings.ContainsAny(glob, "*?[") {
			out = append(out, glob)
		}
	}
	return out
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
	// Rules written against one member. A member with rules of its own is
	// judged by those and by the rules that name no member; a member with
	// none is judged by the unscoped rules alone.
	perMember map[string]*memberRules

	mu   sync.Mutex
	pins map[netip.Addr]*guestPins
	// Addresses learned by resolving the literal host rules on a timer,
	// rather than from an answer a guest was given. They are keyed by the
	// scope of the rule that produced them: the empty scope for a rule that
	// names no member, and the member's name for one that does. A member's
	// resolved address must not widen what another member can reach.
	resolvedAllow map[scopedAddr]time.Time
	resolvedDeny  map[scopedAddr]time.Time
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

	p := &Policy{
		def:           d,
		perMember:     map[string]*memberRules{},
		pins:          map[netip.Addr]*guestPins{},
		resolvedAllow: map[scopedAddr]time.Time{},
		resolvedDeny:  map[scopedAddr]time.Time{},
	}
	if err := p.addRules("--egress-allow", allowRules, false); err != nil {
		return nil, err
	}
	if err := p.addRules("--egress-deny", denyRules, true); err != nil {
		return nil, err
	}
	// Deny-default with no allow rules is not an error: a sandbox that should
	// reach nothing is something an operator asks for on purpose.
	return p, nil
}

// addRules parses each rule and files it under the member it names, or under
// the policy when it names none.
func (p *Policy) addRules(flag string, rules []string, deny bool) error {
	for _, rule := range rules {
		member, rest := splitMemberRule(rule)
		set, err := parseRules(flag, []string{rest})
		if err != nil {
			return err
		}
		if member == "" {
			if deny {
				p.deny.merge(set)
			} else {
				p.allow.merge(set)
			}
			continue
		}
		// A name that could never belong to a member is a typo, and a typo
		// here is a rule that silently never applies.
		if !validMemberName(member) {
			return fmt.Errorf("invalid %s %q: %q is not a usable member name", flag, rule, member)
		}
		own, ok := p.perMember[member]
		if !ok {
			own = &memberRules{}
			p.perMember[member] = own
		}
		if deny {
			own.deny.merge(set)
		} else {
			own.allow.merge(set)
		}
	}
	return nil
}

func (r *ruleSet) merge(other ruleSet) {
	r.cidrs = append(r.cidrs, other.cidrs...)
	r.hosts = append(r.hosts, other.hosts...)
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
func (p *Policy) AllowsQuery(member, name string) bool {
	if p == nil || p.def == EgressAllow {
		return true
	}
	name = normalizeName(name)
	if p.deniesHost(member, name) {
		return false
	}
	return p.allowsHost(member, name)
}

// DeniesQuery reports whether name matches a deny glob, so its answers are
// pinned as unreachable.
func (p *Policy) DeniesQuery(member, name string) bool {
	if p == nil {
		return false
	}
	return p.deniesHost(member, normalizeName(name))
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
// beats allow beats the default. A CIDR rule, an address resolved from a
// literal host rule, and an address pinned from an answer this guest was given
// all count the same on their side.
func (p *Policy) AllowsConnection(member string, guest, dst netip.Addr) bool {
	if p == nil {
		return true
	}
	guest, dst = guest.Unmap(), dst.Unmap()
	if p.deniesAddr(member, dst) || p.resolved(member, dst, true) || p.pinned(guest, dst, true) {
		return false
	}
	if p.allowsAddr(member, dst) || p.resolved(member, dst, false) || p.pinned(guest, dst, false) {
		return true
	}
	return p.def == EgressAllow
}

// scopedAddr is an address together with the rule scope that resolved it.
type scopedAddr struct {
	scope string
	addr  netip.Addr
}

func (p *Policy) resolved(member string, dst netip.Addr, deny bool) bool {
	dst = dst.Unmap()
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.resolvedAllow
	if deny {
		set = p.resolvedDeny
	}
	now := time.Now()
	// A rule naming no member covers every member, so the shared scope is
	// always consulted. A member's own scope covers only itself.
	if expiry, ok := set[scopedAddr{addr: dst}]; ok && now.Before(expiry) {
		return true
	}
	if member == "" {
		return false
	}
	expiry, ok := set[scopedAddr{scope: member, addr: dst}]
	return ok && now.Before(expiry)
}

// Summary describes the policy for the gateway's startup line.
func (p *Policy) Summary() string {
	if p == nil {
		return "egress unfiltered"
	}
	shared := fmt.Sprintf("egress default %s (%d allow, %d deny",
		p.def, len(p.allow.cidrs)+len(p.allow.hosts), len(p.deny.cidrs)+len(p.deny.hosts))
	if names := p.MemberNames(); len(names) > 0 {
		return fmt.Sprintf("%s, plus rules for %s)", shared, strings.Join(names, ", "))
	}
	return shared + ")"
}

// Keeping literal host rules resolved.
//
// A host glob is enforced through the answers the gateway itself gave out, so
// a name is only reachable once a guest has asked for it. That is enough while
// the guest re-asks: the gateway advertises pinTTL, so a well-behaved resolver
// comes back before the pin lapses and picks up whatever the name resolves to
// now.
//
// It is not enough when the guest caches past the TTL, which plenty of runtimes
// do, or holds an address across a rotation. The name is still allowed, the
// address it now answers with was never pinned, and the connection is refused.
// So the gateway also resolves the literal host rules on a timer and keeps the
// addresses they currently answer with in a set of its own.
//
// Globs stay query-driven. There is no way to enumerate what "*.example.com"
// stands for, so nothing can be resolved ahead of a guest asking.

// hostRefreshRetentionFactor sets how long a resolved address outlives the
// last refresh that saw it, as a multiple of the refresh interval. An address
// has to go missing from several rounds before it loses its allowance, so a
// resolver that fails once does not cut the guest's egress.
const hostRefreshRetentionFactor = 3

// WatchHosts keeps the addresses of the literal host rules current until ctx
// is done. It returns immediately when there is nothing to resolve.
func (p *Policy) WatchHosts(ctx context.Context, res resolver, interval time.Duration) {
	if p == nil || interval <= 0 {
		return
	}
	if p.literalHostCount() == 0 {
		return
	}
	retention := time.Duration(hostRefreshRetentionFactor) * interval

	// Resolve once before the first tick so a guest that connects straight
	// away is not held up for a whole interval.
	p.RefreshHosts(ctx, res, retention)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RefreshHosts(ctx, res, retention)
		}
	}
}

// literalHostCount counts the host rules that name one host, across every
// scope. Nothing needs a timer when there are none.
func (p *Policy) literalHostCount() int {
	n := len(p.allow.literalHosts()) + len(p.deny.literalHosts())
	for _, own := range p.perMember {
		n += len(own.allow.literalHosts()) + len(own.deny.literalHosts())
	}
	return n
}

// RefreshHosts resolves every literal host rule once and records what came
// back, under the scope of the rule that named it. A name that fails to
// resolve keeps the addresses it had: dropping them on a transient failure
// would take a sandbox's egress down for as long as the resolver is unwell.
func (p *Policy) RefreshHosts(ctx context.Context, res resolver, retention time.Duration) {
	if p == nil {
		return
	}
	type resolved struct {
		scope string
		addrs []netip.Addr
		deny  bool
	}
	var found []resolved
	found = append(found,
		resolved{addrs: resolveHosts(ctx, res, p.allow.literalHosts())},
		resolved{addrs: resolveHosts(ctx, res, p.deny.literalHosts()), deny: true})
	for _, name := range p.MemberNames() {
		own := p.perMember[name]
		found = append(found,
			resolved{scope: name, addrs: resolveHosts(ctx, res, own.allow.literalHosts())},
			resolved{scope: name, addrs: resolveHosts(ctx, res, own.deny.literalHosts()), deny: true})
	}
	expiry := time.Now().Add(retention)

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range found {
		set := p.resolvedAllow
		if r.deny {
			set = p.resolvedDeny
		}
		for _, addr := range r.addrs {
			set[scopedAddr{scope: r.scope, addr: addr}] = expiry
		}
	}
	now := time.Now()
	for _, set := range []map[scopedAddr]time.Time{p.resolvedAllow, p.resolvedDeny} {
		for key, at := range set {
			if now.After(at) {
				delete(set, key)
			}
		}
	}
}

func resolveHosts(ctx context.Context, res resolver, hosts []string) []netip.Addr {
	var out []netip.Addr
	for _, host := range hosts {
		ips, err := res.LookupIPAddr(ctx, host)
		if err != nil {
			log.Warnf("egress: cannot resolve the allowed host %s, keeping its current addresses: %v", host, err)
			continue
		}
		for _, ip := range ips {
			if addr, ok := netip.AddrFromSlice(ip.IP); ok {
				out = append(out, addr.Unmap())
			}
		}
	}
	return out
}

// Rules that name one member.
//
// A rule may be written as `<member>:host=<glob>` or `<member>:cidr=<cidr>`,
// and then applies to that member alone. A rule with no member applies to
// every member, which is what every rule did before this existed.
//
// The member name is trusted because the gateway pins it to a socket and drops
// frames that do not carry that member's addresses. Without that guard a guest
// could take another member's rules by writing its address, so the two belong
// together.

// memberRules holds one member's own allow and deny sets.
type memberRules struct {
	allow ruleSet
	deny  ruleSet
}

// splitMemberRule separates an optional `<member>:` prefix from a rule.
//
// The kinds are known, so a rule that starts with one has no member prefix.
// That keeps `cidr=` unambiguous without escaping, and a member named "host"
// or "cidr" is refused at parse time rather than silently misread.
func splitMemberRule(rule string) (member, rest string) {
	if strings.HasPrefix(rule, "host=") || strings.HasPrefix(rule, "cidr=") {
		return "", rule
	}
	name, after, ok := strings.Cut(rule, ":")
	if !ok {
		return "", rule
	}
	return name, after
}

// validMemberName is what a rule may name. It is the instance-name shape hull
// already enforces, so a rule cannot name something that could never join.
func validMemberName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// HasMemberRules reports whether any rule names a member, which is what makes
// a member's identity worth having.
func (p *Policy) HasMemberRules() bool {
	return p != nil && len(p.perMember) > 0
}

// MemberNames lists the members the policy names, in order, for the startup
// line and for error messages.
func (p *Policy) MemberNames() []string {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.perMember))
	for name := range p.perMember {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ownRules returns the rules written against one member, or nil when it has
// none. The shared rules are consulted separately, so nothing is copied to
// make a decision: this runs once per connection on a busy gateway.
func (p *Policy) ownRules(member string) *memberRules {
	if member == "" || len(p.perMember) == 0 {
		return nil
	}
	return p.perMember[member]
}

// deniesAddr reports whether any deny rule covers addr, shared or the
// member's own.
func (p *Policy) deniesAddr(member string, addr netip.Addr) bool {
	if p.deny.matchesAddr(addr) {
		return true
	}
	own := p.ownRules(member)
	return own != nil && own.deny.matchesAddr(addr)
}

// allowsAddr reports whether any allow rule covers addr.
func (p *Policy) allowsAddr(member string, addr netip.Addr) bool {
	if p.allow.matchesAddr(addr) {
		return true
	}
	own := p.ownRules(member)
	return own != nil && own.allow.matchesAddr(addr)
}

func (p *Policy) deniesHost(member, name string) bool {
	if p.deny.matchesHost(name) {
		return true
	}
	own := p.ownRules(member)
	return own != nil && own.deny.matchesHost(name)
}

func (p *Policy) allowsHost(member, name string) bool {
	if p.allow.matchesHost(name) {
		return true
	}
	own := p.ownRules(member)
	return own != nil && own.allow.matchesHost(name)
}
