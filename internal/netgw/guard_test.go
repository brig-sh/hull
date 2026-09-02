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
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The guard is what makes a member's identity worth anything. These tests are
// written from the attacker's side: each one is a frame a guest could send to
// take another member's rules or another member's traffic.

func testMember(t *testing.T, name, ip, mac string) Member {
	t.Helper()
	return Member{Name: name, IP: netip.MustParseAddr(ip), MAC: mustMAC(t, mac)}
}

// guardPipe feeds frames to a guard and returns what came out the other side.
func guardPipe(t *testing.T, m Member, stream bool, frames ...[]byte) [][]byte {
	t.Helper()
	ours, theirs := net.Pipe()
	g := newGuard(theirs, m, stream)

	go func() {
		defer func() { _ = ours.Close() }()
		for _, f := range frames {
			if stream {
				prefix := make([]byte, 4)
				binary.BigEndian.PutUint32(prefix, uint32(len(f)))
				if _, err := ours.Write(append(prefix, f...)); err != nil {
					return
				}
				continue
			}
			if _, err := ours.Write(f); err != nil {
				return
			}
		}
	}()

	var out [][]byte
	buf := make([]byte, 65536)
	for {
		_ = g.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := g.Read(buf)
		if n > 0 {
			frame := make([]byte, n)
			copy(frame, buf[:n])
			out = append(out, frame)
		}
		if err != nil {
			return out
		}
	}
}

// ipv4Frame builds an Ethernet frame with the given source addresses, so a
// test can lie about either one.
func ipv4Frame(t *testing.T, srcMAC tcpip.LinkAddress, srcIP, dstIP string, proto tcpip.TransportProtocolNumber, dstPort uint16) []byte {
	t.Helper()
	const payload = header.UDPMinimumSize
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+payload)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: srcMAC,
		DstAddr: mustMAC(t, testGatewayMA),
		Type:    header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + payload),
		TTL:         64,
		Protocol:    uint8(proto),
		SrcAddr:     tcpip.AddrFrom4Slice(net.ParseIP(srcIP).To4()),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(dstIP).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 40000,
		DstPort: dstPort,
		Length:  header.UDPMinimumSize,
	})
	return frame
}

func arpFrame(t *testing.T, ethSrc, arpSrcMAC tcpip.LinkAddress, arpSrcIP string) []byte {
	t.Helper()
	frame := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: ethSrc,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.ARPProtocolNumber,
	})
	a := header.ARP(frame[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPRequest)
	copy(a.HardwareAddressSender(), arpSrcMAC)
	copy(a.ProtocolAddressSender(), net.ParseIP(arpSrcIP).To4())
	copy(a.ProtocolAddressTarget(), net.ParseIP(testGatewayIP).To4())
	return frame
}

func TestGuardPassesTheMemberOwnTraffic(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	frame := ipv4Frame(t, m.MAC, testGuestIP, "93.184.216.34", header.UDPProtocolNumber, 53)

	if got := guardPipe(t, m, false, frame); len(got) != 1 {
		t.Fatalf("the guard dropped the member's own frame (%d passed)", len(got))
	}
}

// The attack the whole feature rests on: claim another member's IP and inherit
// its rules.
func TestGuardDropsASpoofedSourceAddress(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	frame := ipv4Frame(t, m.MAC, "10.87.0.3", "93.184.216.34", header.UDPProtocolNumber, 53)

	if got := guardPipe(t, m, false, frame); len(got) != 0 {
		t.Fatal("a frame carrying another member's address was passed on")
	}
}

// The other half: claim another member's MAC, take over its port on the
// switch, and receive its traffic.
func TestGuardDropsASpoofedHardwareAddress(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	frame := ipv4Frame(t, mustMAC(t, "5a:94:ef:e4:0c:ef"), testGuestIP, "93.184.216.34", header.UDPProtocolNumber, 53)

	if got := guardPipe(t, m, false, frame); len(got) != 0 {
		t.Fatal("a frame carrying another member's hardware address was passed on")
	}
}

// A guest with no address yet has to be able to ask for one.
func TestGuardPassesDHCPBeforeAnAddress(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	dhcp := ipv4Frame(t, m.MAC, "0.0.0.0", "255.255.255.255", header.UDPProtocolNumber, dhcpServerPort)

	if got := guardPipe(t, m, false, dhcp); len(got) != 1 {
		t.Fatal("the guard dropped a DHCP request from a member with no address yet")
	}
}

// The DHCP exemption must not become a hole: 0.0.0.0 is allowed for DHCP and
// nothing else.
func TestGuardDropsNonDHCPFromTheAnyAddress(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	frame := ipv4Frame(t, m.MAC, "0.0.0.0", "93.184.216.34", header.UDPProtocolNumber, 53)

	if got := guardPipe(t, m, false, frame); len(got) != 0 {
		t.Fatal("a non-DHCP frame from 0.0.0.0 was passed on")
	}
}

// ARP carries its own sender fields. A guest that answers for an address it
// does not hold poisons the other guests' tables, and the switch's too.
func TestGuardDropsASpoofedARPSender(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)

	own := arpFrame(t, m.MAC, m.MAC, testGuestIP)
	if got := guardPipe(t, m, false, own); len(got) != 1 {
		t.Fatal("the guard dropped the member's own ARP")
	}

	spoofedIP := arpFrame(t, m.MAC, m.MAC, "10.87.0.3")
	if got := guardPipe(t, m, false, spoofedIP); len(got) != 0 {
		t.Fatal("an ARP claiming another member's address was passed on")
	}

	spoofedMAC := arpFrame(t, m.MAC, mustMAC(t, "5a:94:ef:e4:0c:ef"), testGuestIP)
	if got := guardPipe(t, m, false, spoofedMAC); len(got) != 0 {
		t.Fatal("an ARP whose body disagrees with its Ethernet header was passed on")
	}
}

// A stream member's frames are length-prefixed. The guard has to reassemble
// them to inspect them, and re-emit the ones it passes with framing intact.
func TestGuardFiltersStreamMembers(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	good := ipv4Frame(t, m.MAC, testGuestIP, "93.184.216.34", header.UDPProtocolNumber, 53)
	bad := ipv4Frame(t, m.MAC, "10.87.0.3", "93.184.216.34", header.UDPProtocolNumber, 53)

	got := guardPipe(t, m, true, bad, good, bad)
	if len(got) == 0 {
		t.Fatal("the guard dropped every frame from a stream member")
	}
	// The output is a stream, so join it and check the framing.
	var joined []byte
	for _, chunk := range got {
		joined = append(joined, chunk...)
	}
	if len(joined) != 4+len(good) {
		t.Fatalf("the guard emitted %d bytes, want %d (one framed frame)", len(joined), 4+len(good))
	}
	if size := binary.BigEndian.Uint32(joined[:4]); int(size) != len(good) {
		t.Fatalf("the length prefix says %d, want %d", size, len(good))
	}
	if string(joined[4:]) != string(good) {
		t.Fatal("the frame that passed was altered")
	}
}

// A member whose caller named no MAC is pinned to the first one it uses, so it
// still cannot become another member later.
func TestGuardPinsTheFirstHardwareAddressWhenNoneWasDeclared(t *testing.T) {
	m := Member{Name: "web", IP: netip.MustParseAddr(testGuestIP)}
	first := ipv4Frame(t, mustMAC(t, testGuestMA), testGuestIP, "93.184.216.34", header.UDPProtocolNumber, 53)
	second := ipv4Frame(t, mustMAC(t, "5a:94:ef:e4:0c:ef"), testGuestIP, "93.184.216.34", header.UDPProtocolNumber, 53)

	got := guardPipe(t, m, false, first, second)
	if len(got) != 1 {
		t.Fatalf("%d frames passed, want 1: the first address should be pinned", len(got))
	}
}

// A short or malformed frame must not reach the switch, and must not panic
// the guard.
func TestGuardDropsMalformedFrames(t *testing.T) {
	m := testMember(t, "web", testGuestIP, testGuestMA)
	short := []byte{1, 2, 3}
	truncatedIP := make([]byte, header.EthernetMinimumSize+4)
	header.Ethernet(truncatedIP).Encode(&header.EthernetFields{
		SrcAddr: m.MAC,
		DstAddr: mustMAC(t, testGatewayMA),
		Type:    header.IPv4ProtocolNumber,
	})

	if got := guardPipe(t, m, false, short, truncatedIP); len(got) != 0 {
		t.Fatal("a malformed frame was passed on")
	}
}
