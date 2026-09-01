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
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	testGuestIP6 = "fd87::2"
	testDst4     = "93.184.216.34"
	testDst6     = "2606:2800:220::1"
)

// dialed is one host-side connection the forwarder tried to open. Whether it
// was attempted at all is the whole question the filter answers, so the test
// dialer records the attempt and then fails.
type dialed struct {
	network string
	address string
}

// filteredNetwork builds a network whose egress never leaves the process.
func filteredNetwork(t *testing.T, policy *Policy) (*Network, chan dialed) {
	t.Helper()
	dials := make(chan dialed, 16)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		dial: func(network, address string) (net.Conn, error) {
			select {
			case dials <- dialed{network, address}:
			default:
			}
			return nil, errors.New("test dialer never connects")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n, dials
}

func awaitDial(t *testing.T, dials chan dialed) dialed {
	t.Helper()
	select {
	case d := <-dials:
		return d
	case <-time.After(3 * time.Second):
		t.Fatal("the forwarder never dialed out")
		return dialed{}
	}
}

func expectNoDial(t *testing.T, dials chan dialed) {
	t.Helper()
	select {
	case d := <-dials:
		t.Fatalf("the forwarder dialed %s %s", d.network, d.address)
	case <-time.After(500 * time.Millisecond):
	}
}

func ethernet(t *testing.T, m *member, payloadLen int, ethertype tcpip.NetworkProtocolNumber) []byte {
	t.Helper()
	frame := make([]byte, header.EthernetMinimumSize+payloadLen)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: m.mac,
		DstAddr: mustMAC(t, testGatewayMA),
		Type:    ethertype,
	})
	return frame
}

func tcpSYN4(t *testing.T, m *member, dst string, port uint16) []byte {
	t.Helper()
	frame := ethernet(t, m, header.IPv4MinimumSize+header.TCPMinimumSize, header.IPv4ProtocolNumber)
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.TCPMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(m.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(dst).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	encodeSYN(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:], port)
	return frame
}

func tcpSYN6(t *testing.T, m *member, dst string, port uint16) []byte {
	t.Helper()
	frame := ethernet(t, m, header.IPv6MinimumSize+header.TCPMinimumSize, header.IPv6ProtocolNumber)
	ip := header.IPv6(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv6Fields{
		PayloadLength:     header.TCPMinimumSize,
		TransportProtocol: header.TCPProtocolNumber,
		HopLimit:          64,
		SrcAddr:           tcpip.AddrFrom16Slice(net.ParseIP(testGuestIP6).To16()),
		DstAddr:           tcpip.AddrFrom16Slice(net.ParseIP(dst).To16()),
	})
	encodeSYN(frame[header.EthernetMinimumSize+header.IPv6MinimumSize:], port)
	return frame
}

func encodeSYN(b []byte, port uint16) {
	header.TCP(b).Encode(&header.TCPFields{
		SrcPort:    45678,
		DstPort:    port,
		SeqNum:     1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagSyn,
		WindowSize: 65535,
	})
}

func udpDatagram4(t *testing.T, m *member, dst string, port uint16) []byte {
	t.Helper()
	const payload = 4
	frame := ethernet(t, m, header.IPv4MinimumSize+header.UDPMinimumSize+payload, header.IPv4ProtocolNumber)
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize + payload),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(m.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(dst).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 45678,
		DstPort: port,
		Length:  header.UDPMinimumSize + payload,
	})
	return frame
}

func TestEgressDenyDefaultBlocksTCP(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "deny", nil, nil))
	m := join(t, n)
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	expectNoDial(t, dials)
}

func TestEgressAllowCIDRAdmitsTCP(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "deny", []string{"cidr=93.184.216.0/24"}, nil))
	m := join(t, n)
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	if got := awaitDial(t, dials); got.network != "tcp" || got.address != "93.184.216.34:443" {
		t.Fatalf("dialed %v", got)
	}
}

// A pinned name is what a host glob turns into once the resolver has answered.
func TestEgressPinnedNameAdmitsTCP(t *testing.T) {
	policy := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	n, dials := filteredNetwork(t, policy)
	m := join(t, n)

	m.send(t, tcpSYN4(t, m, testDst4, 443))
	expectNoDial(t, dials)

	policy.Pin(netip.MustParseAddr(testGuestIP), []netip.Addr{netip.MustParseAddr(testDst4)}, false)
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	if got := awaitDial(t, dials); got.address != "93.184.216.34:443" {
		t.Fatalf("dialed %v", got)
	}
}

func TestEgressDenyCIDRUnderAllowDefault(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "allow", nil, []string{"cidr=93.184.216.0/24"}))
	m := join(t, n)
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	expectNoDial(t, dials)

	m.send(t, tcpSYN4(t, m, "1.1.1.1", 443))
	if got := awaitDial(t, dials); got.address != "1.1.1.1:443" {
		t.Fatalf("dialed %v", got)
	}
}

func TestEgressUnfilteredForwardsTCP(t *testing.T) {
	n, dials := filteredNetwork(t, nil)
	m := join(t, n)
	m.send(t, tcpSYN4(t, m, testDst4, 443))
	if got := awaitDial(t, dials); got.address != "93.184.216.34:443" {
		t.Fatalf("dialed %v", got)
	}
}

func TestEgressFiltersUDP(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "deny", []string{"cidr=1.1.1.0/24"}, nil))
	m := join(t, n)

	m.send(t, udpDatagram4(t, m, testDst4, 53))
	expectNoDial(t, dials)

	m.send(t, udpDatagram4(t, m, "1.1.1.1", 53))
	if got := awaitDial(t, dials); got.network != "udp" || got.address != "1.1.1.1:53" {
		t.Fatalf("dialed %v", got)
	}
}

// IPv6 forwarding is not wired up. The stack registers no IPv6 network
// protocol, so an IPv6 frame from a guest is discarded rather than forwarded,
// and that has to hold under allow-default too: a family that leaks past the
// policy would be a hole in it.
func TestIPv6NeverEgresses(t *testing.T) {
	for _, def := range []string{"", "allow", "deny"} {
		t.Run("default "+def, func(t *testing.T) {
			n, dials := filteredNetwork(t, mustPolicy(t, def, nil, nil))
			m := join(t, n)
			m.send(t, tcpSYN6(t, m, testDst6, 443))
			expectNoDial(t, dials)
			// A frame dropped for being malformed would pass the check above
			// for the wrong reason, so confirm the stack saw a well-formed
			// IPv6 frame and turned it away for the stated reason.
			awaitUnknownL3(t, n, header.IPv6ProtocolNumber)
		})
	}
}

func awaitUnknownL3(t *testing.T, n *Network, protocol tcpip.NetworkProtocolNumber) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		counter, ok := n.stack.Stats().NICs.UnknownL3ProtocolRcvdPacketCounts.Get(uint64(protocol))
		if ok && counter.Value() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stack never counted an unsupported %d frame", protocol)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Guest-to-guest traffic is switched at layer 2 and never reaches the stack,
// so the egress policy does not apply to it and deny-default does not
// separate one guest from another. That is a property of where the filter
// sits, so pin it down rather than leave it to be discovered.
func TestGuestToGuestIsNotEgress(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "deny", nil, nil))
	sender := joinAs(t, n, testGuestMA, testGuestIP)
	peerMAC := "5a:94:ef:e4:0c:ef"
	peer := joinAs(t, n, peerMAC, "10.87.0.3")
	// The switch drops a unicast frame for a MAC it has not learned, so let
	// the peer speak first.
	peer.send(t, arpRequest(t, peer, net.ParseIP(testGatewayIP)))
	peer.await(t, "the peer's own ARP reply", func(frame []byte) bool {
		return len(frame) >= header.EthernetMinimumSize && header.Ethernet(frame).Type() == header.ARPProtocolNumber &&
			header.ARP(frame[header.EthernetMinimumSize:]).Op() == header.ARPReply
	})

	frame := tcpSYN4(t, sender, "10.87.0.3", 8080)
	copy(frame[0:6], mustMAC(t, peerMAC))
	sender.send(t, frame)

	peer.await(t, "the frame the other guest sent", func(got []byte) bool {
		return len(got) == len(frame) && string(got) == string(frame)
	})
	expectNoDial(t, dials)
}

func icmpEcho4(t *testing.T, m *member, dst string) []byte {
	t.Helper()
	const payload = 8
	frame := ethernet(t, m, header.IPv4MinimumSize+header.ICMPv4MinimumSize+payload, header.IPv4ProtocolNumber)
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.ICMPv4MinimumSize + payload),
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(m.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(dst).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	echo := header.ICMPv4(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:])
	echo.SetType(header.ICMPv4Echo)
	echo.SetCode(header.ICMPv4UnusedCode)
	echo.SetIdent(1)
	echo.SetSequence(1)
	echo.SetChecksum(0)
	echo.SetChecksum(^checksum.Checksum(echo, 0))
	return frame
}

// A guest's ping is answered by the gateway itself: the netstack takes the
// destination as one of its own addresses and replies, so nothing leaves the
// host. Under deny-default that means a ping to a blocked address still
// succeeds, and it still tells the guest nothing about the outside world.
func TestICMPEchoIsAnsweredNotForwarded(t *testing.T) {
	n, dials := filteredNetwork(t, mustPolicy(t, "deny", nil, nil))
	m := join(t, n)
	m.send(t, icmpEcho4(t, m, testDst4))

	m.await(t, "an ICMP echo reply", func(frame []byte) bool {
		if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.ICMPv4MinimumSize {
			return false
		}
		if header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
			return false
		}
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) || ip.SourceAddress().String() != testDst4 {
			return false
		}
		return header.ICMPv4(frame[header.EthernetMinimumSize+int(ip.HeaderLength()):]).Type() == header.ICMPv4EchoReply
	})
	expectNoDial(t, dials)
}

// The policy sees the source address in the packet, and a guest picks its own.
// One guest can therefore use another's pinned answers by claiming its
// address, which the shared switch makes possible in the first place.
//
// It widens nothing: a gateway carries one policy for every guest on it, so
// the addresses a guest reaches by borrowing another's pins are the ones its
// own queries would have been answered with. Separating guests is a network
// per sandbox, not a rule here.
func TestPinsFollowTheSourceAddressNotTheMember(t *testing.T) {
	policy := mustPolicy(t, "deny", []string{"host=*.example.com"}, nil)
	n, dials := filteredNetwork(t, policy)
	second := joinAs(t, n, "5a:94:ef:e4:0c:ef", "10.87.0.3")
	// Another guest resolved the name, so the pin is held against its
	// address.
	policy.Pin(netip.MustParseAddr(testGuestIP), []netip.Addr{netip.MustParseAddr(testDst4)}, false)

	// The second guest has no pins of its own.
	second.send(t, tcpSYN4(t, second, testDst4, 443))
	expectNoDial(t, dials)

	// Claiming the first guest's address is enough to use its pins.
	spoofed := tcpSYN4(t, second, testDst4, 443)
	ip := header.IPv4(spoofed[header.EthernetMinimumSize:])
	ip.SetSourceAddress(tcpip.AddrFrom4Slice(net.ParseIP(testGuestIP).To4()))
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())
	second.send(t, spoofed)
	if got := awaitDial(t, dials); got.address != testDst4+":443" {
		t.Fatalf("dialed %v", got)
	}
}
