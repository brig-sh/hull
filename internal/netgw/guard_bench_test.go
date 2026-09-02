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
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The guard inspects every frame a member sends, so its cost is paid per
// packet. These measure the check itself, away from the socket.

func benchGuard(b *testing.B) *guard {
	m := Member{Name: "web", IP: netip.MustParseAddr(testGuestIP)}
	hw, _ := netip.ParseAddr(testGuestIP)
	_ = hw
	g := newGuard(nil, m, false, isolation{})
	return g
}

func BenchmarkGuardAllowIPv4(b *testing.B) {
	g := benchGuard(b)
	frame := benchFrame(b, testGuestIP)
	g.mac = header.Ethernet(frame).SourceAddress()

	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if !g.allow(frame) {
			b.Fatal("the guard rejected the member's own frame")
		}
	}
}

func BenchmarkGuardRejectSpoofed(b *testing.B) {
	g := benchGuard(b)
	frame := benchFrame(b, "10.87.0.3")
	g.mac = header.Ethernet(frame).SourceAddress()

	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if g.allow(frame) {
			b.Fatal("the guard passed a spoofed frame")
		}
	}
}

// The policy decision runs once per connection, not per packet, but a
// deny-default gateway makes it on every rejected SYN a guest retries.
func BenchmarkPolicyAllowsConnection(b *testing.B) {
	p, err := ParseEgressPolicy("deny", []string{
		"cidr=203.0.113.0/24", "web:cidr=198.51.100.0/24", "web:host=api.example.com",
	}, []string{"cidr=169.254.0.0/16"})
	if err != nil {
		b.Fatal(err)
	}
	guest, dst := netip.MustParseAddr(testGuestIP), netip.MustParseAddr("198.51.100.5")

	b.ReportAllocs()
	for b.Loop() {
		if !p.AllowsConnection("web", guest, dst) {
			b.Fatal("the policy rejected an allowed destination")
		}
	}
}

func BenchmarkMemberTableLookup(b *testing.B) {
	table := newMemberTable()
	for i := 2; i < 60; i++ {
		addr := netip.AddrFrom4([4]byte{10, 87, 0, byte(i)})
		_ = table.claim(Member{Name: "member", IP: addr})
	}
	dst := netip.MustParseAddr("10.87.0.30")

	b.ReportAllocs()
	for b.Loop() {
		if table.nameOf(dst) == "" {
			b.Fatal("the member was not found")
		}
	}
}

func benchFrame(b *testing.B, srcIP string) []byte {
	b.Helper()
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: "\x5a\x94\xef\xe4\x0c\xee",
		DstAddr: "\x5a\x94\xef\xe4\x0c\xdd",
		Type:    header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	addr, _ := netip.ParseAddr(srcIP)
	dst, _ := netip.ParseAddr("93.184.216.34")
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpipAddr(addr),
		DstAddr:     tcpipAddr(dst),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:]).Encode(&header.UDPFields{
		SrcPort: 40000, DstPort: 53, Length: header.UDPMinimumSize,
	})
	return frame
}
