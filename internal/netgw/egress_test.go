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
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustPolicy(t *testing.T, def string, allow, deny []string) *Policy {
	t.Helper()
	p, err := ParseEgressPolicy(def, allow, deny)
	if err != nil {
		t.Fatalf("ParseEgressPolicy(%q, %v, %v): %v", def, allow, deny, err)
	}
	return p
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse addr %q: %v", s, err)
	}
	return a
}

func TestParseEgressPolicyOffByDefault(t *testing.T) {
	p, err := ParseEgressPolicy("", nil, nil)
	if err != nil {
		t.Fatalf("no flags at all: %v", err)
	}
	if p != nil {
		t.Fatalf("no flags should mean no policy, got %v", p)
	}
	guest, dst := addr(t, "10.87.0.2"), addr(t, "93.184.216.34")
	if !p.AllowsConnection(guest, dst) {
		t.Fatal("a nil policy must forward everything")
	}
	if !p.AllowsQuery("anything.example.com") {
		t.Fatal("a nil policy must answer every query")
	}
}

// Rules without a default would enforce nothing, which is the one outcome an
// operator writing rules cannot have meant.
func TestParseEgressPolicyRulesWithoutDefault(t *testing.T) {
	for _, tc := range []struct{ allow, deny []string }{
		{allow: []string{"host=*.example.com"}},
		{deny: []string{"cidr=10.0.0.0/8"}},
	} {
		if _, err := ParseEgressPolicy("", tc.allow, tc.deny); err == nil {
			t.Fatalf("rules %v/%v without --egress-default were accepted", tc.allow, tc.deny)
		}
	}
}

func TestParseEgressPolicyRejectsBadRules(t *testing.T) {
	tests := []struct {
		name  string
		def   string
		allow []string
		deny  []string
		// want is a fragment the error must contain, so a rejection names
		// the rule the operator has to fix.
		want string
	}{
		{name: "unknown default", def: "maybe", want: `--egress-default "maybe"`},
		{name: "no separator", def: "deny", allow: []string{"example.com"}, want: `"example.com"`},
		{name: "unknown kind", def: "deny", allow: []string{"dns=example.com"}, want: `"dns=example.com"`},
		{name: "bad cidr", def: "deny", allow: []string{"cidr=10.0.0.0/33"}, want: `"10.0.0.0/33" is not a CIDR`},
		{name: "cidr without mask", def: "deny", allow: []string{"cidr=10.0.0.1"}, want: "is not a CIDR"},
		{name: "empty glob", def: "deny", allow: []string{"host="}, want: "the host glob is empty"},
		{name: "empty deny glob", def: "allow", deny: []string{"host=  "}, want: "the host glob is empty"},
		{name: "bad glob", def: "deny", allow: []string{"host=[a-"}, want: `"host=[a-"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseEgressPolicy(tc.def, tc.allow, tc.deny)
			if err == nil {
				t.Fatal("rule was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the rule (want %q)", err, tc.want)
			}
		})
	}
}

func TestPolicyCIDRPrecedence(t *testing.T) {
	guest := addr(t, "10.87.0.2")
	tests := []struct {
		name  string
		def   string
		allow []string
		deny  []string
		dst   string
		want  bool
	}{
		{name: "deny default rejects", def: "deny", dst: "93.184.216.34", want: false},
		{name: "allow default admits", def: "allow", dst: "93.184.216.34", want: true},
		{name: "allow cidr under deny default", def: "deny", allow: []string{"cidr=93.184.216.0/24"}, dst: "93.184.216.34", want: true},
		{name: "outside the allow cidr", def: "deny", allow: []string{"cidr=93.184.216.0/24"}, dst: "1.1.1.1", want: false},
		{name: "deny cidr under allow default", def: "allow", deny: []string{"cidr=169.254.0.0/16"}, dst: "169.254.169.254", want: false},
		{name: "deny beats allow", def: "deny", allow: []string{"cidr=93.184.216.0/24"}, deny: []string{"cidr=93.184.216.34/32"}, dst: "93.184.216.34", want: false},
		{name: "v6 allow cidr", def: "deny", allow: []string{"cidr=2606:2800:220::/48"}, dst: "2606:2800:220::1", want: true},
		{name: "v6 outside the allow cidr", def: "deny", allow: []string{"cidr=2606:2800:220::/48"}, dst: "2606:2800:221::1", want: false},
		{name: "v6 deny beats v6 allow", def: "allow", deny: []string{"cidr=2606:2800:220::/48"}, dst: "2606:2800:220::1", want: false},
		{name: "a v4 rule does not cover v6", def: "deny", allow: []string{"cidr=0.0.0.0/0"}, dst: "2606:2800:220::1", want: false},
		{name: "a v6 rule does not cover v4", def: "deny", allow: []string{"cidr=::/0"}, dst: "93.184.216.34", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPolicy(t, tc.def, tc.allow, tc.deny)
			if got := p.AllowsConnection(guest, addr(t, tc.dst)); got != tc.want {
				t.Fatalf("AllowsConnection(%s) = %v, want %v", tc.dst, got, tc.want)
			}
		})
	}
}

func TestPolicyQueryGating(t *testing.T) {
	tests := []struct {
		name    string
		def     string
		allow   []string
		deny    []string
		query   string
		allowed bool
		denied  bool
	}{
		{name: "deny default refuses an unlisted name", def: "deny", allow: []string{"host=*.example.com"}, query: "evil.test", allowed: false},
		{name: "deny default answers a listed name", def: "deny", allow: []string{"host=*.example.com"}, query: "api.example.com", allowed: true},
		{name: "a glob spans labels", def: "deny", allow: []string{"host=*.example.com"}, query: "a.b.example.com", allowed: true},
		{name: "a glob is not a suffix match", def: "deny", allow: []string{"host=*.example.com"}, query: "example.com", allowed: false},
		{name: "the apex can be listed too", def: "deny", allow: []string{"host=example.com"}, query: "example.com", allowed: true},
		{name: "names are compared case-folded and dot-stripped", def: "deny", allow: []string{"host=API.Example.COM."}, query: "api.example.com.", allowed: true},
		{name: "deny beats allow at the resolver", def: "deny", allow: []string{"host=*.example.com"}, deny: []string{"host=secret.example.com"}, query: "secret.example.com", allowed: false, denied: true},
		{name: "allow default answers everything", def: "allow", deny: []string{"host=*.tracker.test"}, query: "a.tracker.test", allowed: true, denied: true},
		{name: "allow default leaves other names alone", def: "allow", deny: []string{"host=*.tracker.test"}, query: "example.com", allowed: true, denied: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPolicy(t, tc.def, tc.allow, tc.deny)
			if got := p.AllowsQuery(tc.query); got != tc.allowed {
				t.Fatalf("AllowsQuery(%q) = %v, want %v", tc.query, got, tc.allowed)
			}
			if got := p.DeniesQuery(tc.query); got != tc.denied {
				t.Fatalf("DeniesQuery(%q) = %v, want %v", tc.query, got, tc.denied)
			}
		})
	}
}

func TestPolicyPinsAreForOneGuest(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	first, second := addr(t, "10.87.0.2"), addr(t, "10.87.0.3")
	dst := addr(t, "93.184.216.34")

	p.Pin(first, []netip.Addr{dst}, false)
	if !p.AllowsConnection(first, dst) {
		t.Fatal("the guest that asked cannot reach the answer it was given")
	}
	if p.AllowsConnection(second, dst) {
		t.Fatal("a pin leaked to a guest that never asked")
	}
}

func TestPolicyPinsBothFamilies(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	guest := addr(t, "10.87.0.2")
	v4, v6 := addr(t, "93.184.216.34"), addr(t, "2606:2800:220::1")

	p.Pin(guest, []netip.Addr{v4, v6}, false)
	for _, dst := range []netip.Addr{v4, v6} {
		if !p.AllowsConnection(guest, dst) {
			t.Fatalf("pinned %s is not reachable", dst)
		}
	}
}

func TestPolicyDenyPinBeatsAllowPin(t *testing.T) {
	p := mustPolicy(t, "allow", []string{"host=*.example.com"}, []string{"host=bad.example.com"})
	guest, dst := addr(t, "10.87.0.2"), addr(t, "93.184.216.34")

	p.Pin(guest, []netip.Addr{dst}, false)
	p.Pin(guest, []netip.Addr{dst}, true)
	if p.AllowsConnection(guest, dst) {
		t.Fatal("an address pinned by both a deny and an allow glob was admitted")
	}
}

func TestPolicyPinsExpire(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	guest, dst := addr(t, "10.87.0.2"), addr(t, "93.184.216.34")

	p.Pin(guest, []netip.Addr{dst}, false)
	// Reach into the pin table rather than wait out the real lifetime.
	p.mu.Lock()
	p.pins[guest].allow[dst] = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if p.AllowsConnection(guest, dst) {
		t.Fatal("an expired pin still admitted the connection")
	}
}

func TestPolicySweepDropsExpiredGuests(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	stale, fresh := addr(t, "10.87.0.2"), addr(t, "10.87.0.3")
	dst := addr(t, "93.184.216.34")

	p.Pin(stale, []netip.Addr{dst}, false)
	p.mu.Lock()
	p.pins[stale].allow[dst] = time.Now().Add(-time.Second)
	p.mu.Unlock()

	// Any later pin sweeps the table, so a recycled lease does not inherit
	// what the previous holder of that address was allowed to reach.
	p.Pin(fresh, []netip.Addr{dst}, false)
	p.mu.Lock()
	_, kept := p.pins[stale]
	p.mu.Unlock()
	if kept {
		t.Fatal("expired pins for a departed guest were kept")
	}
}

func TestPolicySummary(t *testing.T) {
	var off *Policy
	if got := off.Summary(); got != "egress unfiltered" {
		t.Fatalf("Summary() = %q", got)
	}
	p := mustPolicy(t, "deny", []string{"host=*.example.com", "cidr=10.0.0.0/8"}, []string{"cidr=169.254.0.0/16"})
	if got := p.Summary(); got != "egress default deny (2 allow, 1 deny)" {
		t.Fatalf("Summary() = %q", got)
	}
}
