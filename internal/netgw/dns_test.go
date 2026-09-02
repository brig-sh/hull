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
	"net"
	"net/netip"
	"testing"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// fakeResolver stands in for the host's resolver. Names it does not know are
// NXDOMAIN, so a test can tell "the gateway asked and got nothing" from "the
// gateway never asked".
type fakeResolver struct {
	hosts map[string][]net.IPAddr
	asked chan string
}

func newFakeResolver(hosts map[string][]net.IPAddr) *fakeResolver {
	return &fakeResolver{hosts: hosts, asked: make(chan string, 16)}
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	select {
	case r.asked <- host:
	default:
	}
	addrs, ok := r.hosts[normalizeName(host)]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	return addrs, nil
}

func (r *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	return "", fmt.Errorf("no CNAME for %q", host)
}
func (r *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, fmt.Errorf("no MX for %q", name)
}
func (r *fakeResolver) LookupNS(_ context.Context, name string) ([]*net.NS, error) {
	return nil, fmt.Errorf("no NS for %q", name)
}
func (r *fakeResolver) LookupSRV(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
	return "", nil, fmt.Errorf("no SRV for %q", name)
}
func (r *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	return nil, fmt.Errorf("no TXT for %q", name)
}

func v4addrs(ips ...string) []net.IPAddr {
	var out []net.IPAddr
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out
}

func ask(h *dnsHandler, name string, qtype uint16) *dns.Msg {
	return askAs(h, netip.MustParseAddr(testGuestIP), name, qtype)
}

func askAs(h *dnsHandler, guest netip.Addr, name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	reply := new(dns.Msg)
	reply.SetReply(m)
	h.addAnswers(reply, guest)
	return reply
}

func answerIPs(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out
}

// The gateway is started with the project's service names. Serving them is
// the whole reason they are passed in.
func TestDNSServesTheRecordsItWasGiven(t *testing.T) {
	h := &dnsHandler{
		zones:    []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.87.0.5")}}}},
		upstream: newFakeResolver(nil),
	}
	reply := ask(h, "web", dns.TypeA)
	if got := answerIPs(reply); len(got) != 1 || got[0] != "10.87.0.5" {
		t.Fatalf("A web = %v, rcode %s", got, dns.RcodeToString[reply.Rcode])
	}
}

// A zone named "." holds names with no domain of their own. It must not turn
// into an authority for the whole namespace.
func TestDNSRootZoneFallsThrough(t *testing.T) {
	resolver := newFakeResolver(map[string][]net.IPAddr{"example.com": v4addrs("93.184.216.34")})
	h := &dnsHandler{
		zones:    []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.87.0.5")}}}},
		upstream: resolver,
	}
	reply := ask(h, "example.com", dns.TypeA)
	if got := answerIPs(reply); len(got) != 1 || got[0] != "93.184.216.34" {
		t.Fatalf("A example.com = %v, rcode %s", got, dns.RcodeToString[reply.Rcode])
	}
}

// A name the gateway holds is answered by the gateway whatever was asked
// about it, rather than leaking to the host's resolver.
func TestDNSLocalNameIsNotForwarded(t *testing.T) {
	resolver := newFakeResolver(nil)
	h := &dnsHandler{
		zones:    []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.87.0.5")}}}},
		upstream: resolver,
	}
	reply := ask(h, "web", dns.TypeAAAA)
	if len(reply.Answer) != 0 || reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("AAAA web = %v, rcode %s", reply.Answer, dns.RcodeToString[reply.Rcode])
	}
	select {
	case name := <-resolver.asked:
		t.Fatalf("the host resolver was asked for %q", name)
	default:
	}
}

// IPv6 forwarding is not wired up, so an address the guest cannot use is one
// the gateway does not hand out.
func TestDNSAnswersNoAAAA(t *testing.T) {
	resolver := newFakeResolver(map[string][]net.IPAddr{
		"example.com": {{IP: net.ParseIP("2606:2800:220::1")}, {IP: net.ParseIP("93.184.216.34")}},
	})
	h := &dnsHandler{upstream: resolver}

	if reply := ask(h, "example.com", dns.TypeAAAA); len(reply.Answer) != 0 {
		t.Fatalf("AAAA example.com = %v", reply.Answer)
	}
	// The A query filters the IPv6 address out of the same answer set.
	if got := answerIPs(ask(h, "example.com", dns.TypeA)); len(got) != 1 || got[0] != "93.184.216.34" {
		t.Fatalf("A example.com = %v", got)
	}
}

func TestDNSUnknownNameIsNXDOMAIN(t *testing.T) {
	h := &dnsHandler{upstream: newFakeResolver(nil)}
	if reply := ask(h, "nowhere.test", dns.TypeA); reply.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %s, want NXDOMAIN", dns.RcodeToString[reply.Rcode])
	}
}

// End to end: a guest's DNS query as an Ethernet frame, and the gateway's
// answer as another one.

func dnsQueryFrame(t *testing.T, m *member, name string, qtype uint16) []byte {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	q.Id = 0x1234
	payload, err := q.Pack()
	if err != nil {
		t.Fatalf("pack query: %v", err)
	}

	frame := ethernet(t, m, header.IPv4MinimumSize+header.UDPMinimumSize+len(payload), header.IPv4ProtocolNumber)
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(m.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(testGatewayIP).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	udpStart := header.EthernetMinimumSize + header.IPv4MinimumSize
	header.UDP(frame[udpStart:]).Encode(&header.UDPFields{
		SrcPort: 45678,
		DstPort: 53,
		Length:  uint16(header.UDPMinimumSize + len(payload)),
	})
	copy(frame[udpStart+header.UDPMinimumSize:], payload)
	return frame
}

// dnsReply pulls the gateway's answer out of the frames the member received.
func dnsReply(t *testing.T, m *member) *dns.Msg {
	t.Helper()
	var reply *dns.Msg
	m.await(t, "a DNS reply", func(frame []byte) bool {
		if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize {
			return false
		}
		if header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
			return false
		}
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.Protocol() != uint8(header.UDPProtocolNumber) {
			return false
		}
		udpStart := header.EthernetMinimumSize + int(ip.HeaderLength())
		udp := header.UDP(frame[udpStart:])
		if udp.SourcePort() != 53 {
			return false
		}
		m := new(dns.Msg)
		if err := m.Unpack(frame[udpStart+header.UDPMinimumSize:]); err != nil {
			return false
		}
		reply = m
		return true
	})
	return reply
}

func TestDNSOverTheVirtualNetwork(t *testing.T) {
	resolver := newFakeResolver(map[string][]net.IPAddr{"example.com": v4addrs("93.184.216.34")})
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		DNSZones:          []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.87.0.5")}}}},
		resolve:           resolver,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := join(t, n)

	m.send(t, dnsQueryFrame(t, m, "web", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != "10.87.0.5" {
		t.Fatalf("A web over the network = %v", got)
	}

	m.send(t, dnsQueryFrame(t, m, "example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != "93.184.216.34" {
		t.Fatalf("A example.com over the network = %v", got)
	}
}

// Egress filtering at the resolver.

func TestDNSRefusesUnlistedNamesUnderDenyDefault(t *testing.T) {
	h := &dnsHandler{
		upstream: newFakeResolver(map[string][]net.IPAddr{"evil.test": v4addrs("93.184.216.34")}),
		policy:   mustPolicy(t, "deny", []string{"host=*.example.com"}, nil),
	}
	reply := ask(h, "evil.test", dns.TypeA)
	if reply.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode = %s, want REFUSED", dns.RcodeToString[reply.Rcode])
	}
	if len(reply.Answer) != 0 {
		t.Fatalf("a refused query still carried answers: %v", reply.Answer)
	}
}

// The names a project's own services answer to are the gateway's own records
// and stay resolvable, whatever the egress rules say about the outside world.
func TestDNSServesLocalRecordsUnderDenyDefault(t *testing.T) {
	h := &dnsHandler{
		zones:    []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.87.0.5")}}}},
		upstream: newFakeResolver(nil),
		policy:   mustPolicy(t, "deny", nil, nil),
	}
	if got := answerIPs(ask(h, "web", dns.TypeA)); len(got) != 1 || got[0] != "10.87.0.5" {
		t.Fatalf("A web = %v", got)
	}
}

func TestDNSPinsTheAnswerForTheGuestThatAsked(t *testing.T) {
	policy := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	h := &dnsHandler{
		upstream: newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs("93.184.216.34")}),
		policy:   policy,
	}
	asker, other := netip.MustParseAddr(testGuestIP), netip.MustParseAddr("10.87.0.3")
	dst := netip.MustParseAddr("93.184.216.34")

	if got := answerIPs(askAs(h, asker, "api.example.com", dns.TypeA)); len(got) != 1 {
		t.Fatalf("A api.example.com = %v", got)
	}
	if !policy.AllowsConnection("", asker, dst) {
		t.Fatal("the guest cannot reach the answer it was given")
	}
	if policy.AllowsConnection("", other, dst) {
		t.Fatal("the answer was pinned for a guest that never asked")
	}
}

// Under allow-default a deny glob is best effort: the name still resolves,
// and what came back is pinned as unreachable.
func TestDNSPinsDeniedNamesUnderAllowDefault(t *testing.T) {
	policy := mustPolicy(t, "allow", nil, []string{"host=*.tracker.test"})
	h := &dnsHandler{
		upstream: newFakeResolver(map[string][]net.IPAddr{"a.tracker.test": v4addrs("93.184.216.34")}),
		policy:   policy,
	}
	guest, dst := netip.MustParseAddr(testGuestIP), netip.MustParseAddr("93.184.216.34")

	if got := answerIPs(ask(h, "a.tracker.test", dns.TypeA)); len(got) != 1 {
		t.Fatalf("A a.tracker.test = %v", got)
	}
	if policy.AllowsConnection("", guest, dst) {
		t.Fatal("an address a deny glob resolved to was still reachable")
	}
}

// What the gateway tells the guest and what it honours are the same number.
func TestDNSAnswerTTLMatchesThePin(t *testing.T) {
	withHostRules := &dnsHandler{
		upstream: newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs("93.184.216.34")}),
		policy:   mustPolicy(t, "deny", []string{"host=*.example.com"}, nil),
	}
	reply := ask(withHostRules, "api.example.com", dns.TypeA)
	if len(reply.Answer) != 1 {
		t.Fatalf("answers = %v", reply.Answer)
	}
	if got := reply.Answer[0].Header().Ttl; got != uint32(pinTTL.Seconds()) {
		t.Fatalf("TTL = %d, want %d", got, uint32(pinTTL.Seconds()))
	}

	// With no rule written against a name there is nothing to pin, so the
	// answer keeps the TTL the gateway has always given out.
	unfiltered := &dnsHandler{upstream: newFakeResolver(map[string][]net.IPAddr{"example.com": v4addrs("93.184.216.34")})}
	reply = ask(unfiltered, "example.com", dns.TypeA)
	if got := reply.Answer[0].Header().Ttl; got != 0 {
		t.Fatalf("unfiltered TTL = %d, want 0", got)
	}
}

// The whole path, over the wire: the guest resolves an allowed name, then
// connects to the address it was given, and the connection leaves. A name
// that was never resolved does not.
func TestEgressHostGlobEndToEnd(t *testing.T) {
	policy := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	dials := make(chan dialed, 16)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		resolve:           newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(testDst4)}),
		dial: func(network, address string) (net.Conn, error) {
			select {
			case dials <- dialed{network, address}:
			default:
			}
			return nil, fmt.Errorf("test dialer never connects")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := join(t, n)

	// Before the lookup the address means nothing to the policy.
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	expectNoDial(t, dials)

	// The gateway's own resolver stays reachable under deny-default.
	m.send(t, dnsQueryFrame(t, m, "api.example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != testDst4 {
		t.Fatalf("A api.example.com = %v", got)
	}

	m.send(t, tcpSYN4(t, m, testDst4, 443))
	if got := awaitDial(t, dials); got.address != testDst4+":443" {
		t.Fatalf("dialed %v", got)
	}

	// A name outside the allow globs is refused, so its address is never
	// learned from here.
	m.send(t, dnsQueryFrame(t, m, "evil.test", dns.TypeA))
	if reply := dnsReply(t, m); reply.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode for evil.test = %s, want REFUSED", dns.RcodeToString[reply.Rcode])
	}
}
