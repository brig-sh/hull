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
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// Per-member rules.

func TestParseMemberScopedRules(t *testing.T) {
	p := mustPolicy(t, "deny",
		[]string{"host=shared.example.com", "web:host=api.example.com", "db:cidr=10.1.0.0/16"},
		[]string{"web:cidr=169.254.0.0/16"})

	if got := p.MemberNames(); strings.Join(got, ",") != "db,web" {
		t.Fatalf("MemberNames() = %v, want [db web]", got)
	}
	if !p.HasMemberRules() {
		t.Fatal("HasMemberRules() is false with member rules present")
	}
}

// A rule that names no member covers every member. A rule that names one
// covers that member alone.
func TestMemberRulesApplyToTheirMemberOnly(t *testing.T) {
	p := mustPolicy(t, "deny",
		[]string{"cidr=203.0.113.0/24", "web:cidr=198.51.100.0/24"}, nil)
	guest := addr(t, testGuestIP)
	shared, webOnly := addr(t, "203.0.113.5"), addr(t, "198.51.100.5")

	if !p.AllowsConnection("web", guest, shared) || !p.AllowsConnection("db", guest, shared) {
		t.Fatal("an unscoped rule must cover every member")
	}
	if !p.AllowsConnection("web", guest, webOnly) {
		t.Fatal("web cannot reach what its own rule allows")
	}
	if p.AllowsConnection("db", guest, webOnly) {
		t.Fatal("db reached an address only web's rule allows")
	}
	if p.AllowsConnection("", guest, webOnly) {
		t.Fatal("an unidentified member reached an address only web's rule allows")
	}
}

// A deny naming one member must not silence another.
func TestMemberDenyIsScoped(t *testing.T) {
	p := mustPolicy(t, "allow", nil, []string{"db:cidr=203.0.113.0/24"})
	guest, dst := addr(t, testGuestIP), addr(t, "203.0.113.5")

	if p.AllowsConnection("db", guest, dst) {
		t.Fatal("db reached an address its own deny rule blocks")
	}
	if !p.AllowsConnection("web", guest, dst) {
		t.Fatal("a deny naming db blocked web as well")
	}
}

// A member's deny beats the shared allow, so a general permission can be
// narrowed for one member.
func TestMemberDenyBeatsSharedAllow(t *testing.T) {
	p := mustPolicy(t, "deny",
		[]string{"cidr=203.0.113.0/24"},
		[]string{"db:cidr=203.0.113.5/32"})
	guest, dst := addr(t, testGuestIP), addr(t, "203.0.113.5")

	if p.AllowsConnection("db", guest, dst) {
		t.Fatal("a member deny did not beat the shared allow")
	}
	if !p.AllowsConnection("web", guest, dst) {
		t.Fatal("the shared allow stopped applying to another member")
	}
}

// The resolver answers per member too, or a name one member may resolve
// becomes reachable for all of them.
func TestMemberScopedQueries(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"web:host=api.example.com"}, nil)

	if !p.AllowsQuery("web", "api.example.com") {
		t.Fatal("web cannot resolve the name its own rule allows")
	}
	if p.AllowsQuery("db", "api.example.com") {
		t.Fatal("db resolved a name only web's rule allows")
	}
}

// A refreshed address belongs to the scope of the rule that resolved it.
func TestRefreshedAddressesAreScopedToTheirMember(t *testing.T) {
	p := mustPolicy(t, "deny", []string{"web:host=api.example.com"}, nil)
	res := newRotatingResolver("203.0.113.5")
	guest, dst := addr(t, testGuestIP), addr(t, "203.0.113.5")

	p.RefreshHosts(context.Background(), res, time.Minute)
	if !p.AllowsConnection("web", guest, dst) {
		t.Fatal("web cannot reach the address its own rule resolved")
	}
	if p.AllowsConnection("db", guest, dst) {
		t.Fatal("a refreshed address leaked from web's rule to db")
	}
}

// The wire format has to survive a round trip, and an older hull's join has to
// keep working.
func TestMemberIdentityRoundTrip(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	got, err := DecodeMember(EncodeMember(m))
	if err != nil {
		t.Fatalf("DecodeMember: %v", err)
	}
	if got.Name != m.Name || got.IP != m.IP || got.MAC != m.MAC {
		t.Fatalf("round trip gave %+v, want %+v", got, m)
	}
	if !got.Identified() {
		t.Fatal("a decoded member is not identified")
	}
}

func TestDecodeMemberAcceptsAnOlderJoin(t *testing.T) {
	for _, b := range [][]byte{nil, {0}} {
		got, err := DecodeMember(b)
		if err != nil {
			t.Fatalf("DecodeMember(%v): %v", b, err)
		}
		if got.Identified() {
			t.Fatal("an empty join produced an identified member")
		}
	}
}

// Two members cannot hold one address, or the gateway cannot tell them apart.
func TestMemberTableRefusesADuplicateAddress(t *testing.T) {
	table := newMemberTable()
	first := testMember(t, "web", testGuestIP, testGuestMA)
	second := Member{Name: "db", IP: netip.MustParseAddr(testGuestIP)}

	if err := table.claim(first); err != nil {
		t.Fatalf("claim: %v", err)
	}
	err := table.claim(second)
	if err == nil {
		t.Fatal("two members were allowed to hold one address")
	}
	if !strings.Contains(err.Error(), "web") {
		t.Fatalf("the error does not name the holder: %v", err)
	}

	table.release(first)
	if err := table.claim(second); err != nil {
		t.Fatalf("the address was not released: %v", err)
	}
}

// End to end: two members on one gateway, different rules, real bytes. This is
// the property the whole change exists for.
func TestPerMemberRulesEndToEnd(t *testing.T) {
	host := hostIPv4(t)
	server := newEchoServer(t, host)
	policy := mustPolicy(t, "deny", []string{"web:cidr=" + host + "/32"}, nil)

	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	web := joinMember(t, n, Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)})
	db := joinMember(t, n, Member{Name: "db", IP: netip.MustParseAddr("10.87.0.3"), MAC: mustMAC(t, "5a:94:ef:e4:0c:ef")})

	// web's own rule admits it.
	client := newTCPClient(web, host, server.port(), 41001)
	if !client.connect(t) {
		t.Fatal("web could not reach what its own rule allows")
	}
	if got := client.say(t, "web"); got != "web" {
		t.Fatalf("echo returned %q", got)
	}

	// db has no rule of its own and the default is deny.
	blocked := newTCPClient(db, host, server.port(), 41002)
	if blocked.connect(t) {
		t.Fatal("db reached an address only web's rule allows")
	}

	// db claims web's address to borrow its rule. The frame goes out of db's
	// socket with web's source address, which is exactly what the guard is
	// for, so nothing reaches the forwarder at all.
	spoofer := &member{conn: db.conn, frames: db.frames, mac: db.mac, ip: net.ParseIP(testGuestIP).To4()}
	spoof := newTCPClient(spoofer, host, server.port(), 41003)
	spoof.m.send(t, spoof.segment(t, header.TCPFlagSyn, nil))
	select {
	case <-time.After(500 * time.Millisecond):
	case frame := <-db.frames:
		t.Fatalf("the gateway answered a spoofed frame: %x", frame)
	}

	if got := server.accepted.Load(); got != 1 {
		t.Fatalf("the server accepted %d connections, want 1 (web only)", got)
	}
}

// The resolver end to end: each member resolves only what its own rules cover.
func TestPerMemberDNSEndToEnd(t *testing.T) {
	policy := mustPolicy(t, "deny", []string{"web:host=api.example.com"}, nil)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		resolve:           newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs("203.0.113.5")}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	web := joinMember(t, n, Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)})
	db := joinMember(t, n, Member{Name: "db", IP: netip.MustParseAddr("10.87.0.3"), MAC: mustMAC(t, "5a:94:ef:e4:0c:ef")})

	web.send(t, dnsQueryFrame(t, web, "api.example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, web)); len(got) != 1 || got[0] != "203.0.113.5" {
		t.Fatalf("web resolved api.example.com to %v", got)
	}

	db.send(t, dnsQueryFrame(t, db, "api.example.com", dns.TypeA))
	if reply := dnsReply(t, db); reply.Rcode != dns.RcodeRefused {
		t.Fatalf("db got %s for a name only web may resolve, want REFUSED", dns.RcodeToString[reply.Rcode])
	}
}
