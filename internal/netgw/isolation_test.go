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
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// Peer isolation is what makes a per-member rule mean something. Without it a
// member denied all egress reaches a member that has it and uses it, so the
// denial only holds as long as the neighbours cooperate.

const peerMAC = "5a:94:ef:e4:0c:ef"
const peerIP = "10.87.0.3"

func isolatedNetwork(t *testing.T, policy *Policy) (*Network, chan dialed) {
	t.Helper()
	dials := make(chan dialed, 16)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		IsolatePeers:      true,
		dial: func(network, address string) (net.Conn, error) {
			select {
			case dials <- dialed{network, address}:
			default:
			}
			return nil, net.ErrClosed
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n, dials
}

// peerFrame is a frame one guest sends straight to another.
func peerFrame(t *testing.T, from *member, dstMAC tcpip.LinkAddress, dstIP string) []byte {
	t.Helper()
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: from.mac,
		DstAddr: dstMAC,
		Type:    header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(from.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(dstIP).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 40000, DstPort: 8080, Length: header.UDPMinimumSize,
	})
	return frame
}

// The attack the whole thing exists to stop: a member with no egress asks a
// neighbour that has some to fetch for it.
func TestIsolatedMemberCannotReachAPeer(t *testing.T) {
	n, _ := isolatedNetwork(t, mustPolicy(t, "deny", nil, nil))
	web := joinMember(t, n, Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)})
	peer := joinMember(t, n, Member{Name: "db", IP: netip.MustParseAddr(peerIP), MAC: mustMAC(t, peerMAC)})

	// Let the peer be learned by the switch, so a failure to deliver is
	// isolation rather than an unlearned address.
	peer.send(t, arpRequest(t, peer, net.ParseIP(testGatewayIP)))
	peer.await(t, "the peer's own ARP reply", func(f []byte) bool {
		return len(f) >= header.EthernetMinimumSize &&
			header.Ethernet(f).Type() == header.ARPProtocolNumber &&
			header.ARP(f[header.EthernetMinimumSize:]).Op() == header.ARPReply
	})

	web.send(t, peerFrame(t, web, mustMAC(t, peerMAC), peerIP))
	select {
	case frame := <-peer.frames:
		t.Fatalf("an isolated member reached its peer: %x", frame)
	case <-time.After(500 * time.Millisecond):
	}
}

// A member must not be able to find out who its neighbours are either.
func TestIsolatedMemberCannotARPAPeer(t *testing.T) {
	n, _ := isolatedNetwork(t, mustPolicy(t, "deny", nil, nil))
	web := joinMember(t, n, Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)})
	peer := joinMember(t, n, Member{Name: "db", IP: netip.MustParseAddr(peerIP), MAC: mustMAC(t, peerMAC)})

	web.send(t, arpRequest(t, web, net.ParseIP(peerIP)))
	select {
	case frame := <-peer.frames:
		t.Fatalf("an ARP for a peer was delivered: %x", frame)
	case <-time.After(500 * time.Millisecond):
	}
}

// Isolation must not break the things a guest cannot start without.
func TestIsolatedMemberStillReachesTheGateway(t *testing.T) {
	n, dials := isolatedNetwork(t, mustPolicy(t, "deny", []string{"cidr=203.0.113.0/24"}, nil))
	web := joinMember(t, n, Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)})

	// ARP for the gateway is how it finds its next hop.
	web.send(t, arpRequest(t, web, net.ParseIP(testGatewayIP)))
	web.await(t, "the gateway's ARP reply", func(f []byte) bool {
		return len(f) >= header.EthernetMinimumSize &&
			header.Ethernet(f).Type() == header.ARPProtocolNumber &&
			header.ARP(f[header.EthernetMinimumSize:]).Op() == header.ARPReply
	})

	// DNS, which is a service on the gateway itself.
	web.send(t, dnsQueryFrame(t, web, "web", dns.TypeA))

	// And egress, which is the whole point of the gateway.
	web.send(t, tcpSYN4(t, web, "203.0.113.5", 443))
	if got := awaitDial(t, dials); got.address != "203.0.113.5:443" {
		t.Fatalf("an isolated member could not egress: %v", got)
	}
}

// A guest with no address has to be able to get one, and DHCP is broadcast.
func TestIsolatedMemberCanStillDHCP(t *testing.T) {
	m := Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)}
	iso := isolation{on: true, gatewayMAC: []byte(mustMAC(t, testGatewayMA)), gatewayIP: tcpipAddr(netip.MustParseAddr(testGatewayIP))}

	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: m.MAC,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpipAddr(netip.MustParseAddr("0.0.0.0")),
		DstAddr:     tcpipAddr(netip.MustParseAddr("255.255.255.255")),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 68, DstPort: dhcpServerPort, Length: header.UDPMinimumSize,
	})

	g := newGuard(nil, m, false, iso)
	g.macBytes = []byte(m.MAC)
	if !g.allow(frame) {
		t.Fatal("an isolated member cannot ask for an address")
	}
}

// A broadcast that is not DHCP or an ARP for the gateway is how a member
// would otherwise reach or enumerate its neighbours.
func TestIsolatedMemberCannotBroadcastToPeers(t *testing.T) {
	m := Member{Name: "web", IP: netip.MustParseAddr(testGuestIP), MAC: mustMAC(t, testGuestMA)}
	iso := isolation{on: true, gatewayMAC: []byte(mustMAC(t, testGatewayMA)), gatewayIP: tcpipAddr(netip.MustParseAddr(testGatewayIP))}
	g := newGuard(nil, m, false, iso)
	g.macBytes = []byte(m.MAC)

	// An ARP asking for a peer rather than the gateway.
	peerARP := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(peerARP).Encode(&header.EthernetFields{
		SrcAddr: m.MAC, DstAddr: header.EthernetBroadcastAddress, Type: header.ARPProtocolNumber,
	})
	a := header.ARP(peerARP[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPRequest)
	copy(a.HardwareAddressSender(), m.MAC)
	copy(a.ProtocolAddressSender(), net.ParseIP(testGuestIP).To4())
	copy(a.ProtocolAddressTarget(), net.ParseIP(peerIP).To4())
	if g.allow(peerARP) {
		t.Fatal("an isolated member could ARP for a peer")
	}

	// A broadcast UDP that is not DHCP.
	broadcast := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize)
	header.Ethernet(broadcast).Encode(&header.EthernetFields{
		SrcAddr: m.MAC, DstAddr: header.EthernetBroadcastAddress, Type: header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(broadcast[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpipAddr(netip.MustParseAddr(testGuestIP)),
		DstAddr:     tcpipAddr(netip.MustParseAddr("255.255.255.255")),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(broadcast[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 5353, DstPort: 5353, Length: header.UDPMinimumSize,
	})
	if g.allow(broadcast) {
		t.Fatal("an isolated member could broadcast to its peers")
	}
}

// Without isolation the switch behaves as it always has, so a project whose
// services must talk to each other is unaffected.
func TestPeersStillReachEachOtherWithoutIsolation(t *testing.T) {
	n, _ := filteredNetwork(t, mustPolicy(t, "deny", nil, nil))
	web := joinAs(t, n, testGuestMA, testGuestIP)
	peer := joinAs(t, n, peerMAC, peerIP)

	peer.send(t, arpRequest(t, peer, net.ParseIP(testGatewayIP)))
	peer.await(t, "the peer's own ARP reply", func(f []byte) bool {
		return len(f) >= header.EthernetMinimumSize &&
			header.Ethernet(f).Type() == header.ARPProtocolNumber &&
			header.ARP(f[header.EthernetMinimumSize:]).Op() == header.ARPReply
	})

	frame := peerFrame(t, web, mustMAC(t, peerMAC), peerIP)
	web.send(t, frame)
	peer.await(t, "the frame from the other guest", func(got []byte) bool {
		return string(got) == string(frame)
	})
}
