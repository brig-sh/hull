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
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	testSubnet    = "10.87.0.0/24"
	testGatewayIP = "10.87.0.1"
	testGatewayMA = "5a:94:ef:e4:0c:dd"
	testGuestIP   = "10.87.0.2"
	testGuestMA   = "5a:94:ef:e4:0c:ee"
)

// member is a fake guest on the switch: it writes Ethernet frames in and
// collects the ones the gateway sends back.
type member struct {
	conn   net.Conn
	frames chan []byte
	mac    tcpip.LinkAddress
	ip     net.IP
}

func mustMAC(t *testing.T, s string) tcpip.LinkAddress {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parse mac %q: %v", s, err)
	}
	return tcpip.LinkAddress(hw)
}

// join attaches a member to n over a synchronous pipe. Reading has to be
// continuous: the switch holds its write lock while sending, so a member that
// stops reading wedges the whole network.
func join(t *testing.T, n *Network) *member {
	return joinAs(t, n, testGuestMA, testGuestIP)
}

func joinAs(t *testing.T, n *Network, mac, ip string) *member {
	t.Helper()
	ours, theirs := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = n.AcceptVfkit(ctx, theirs) }()

	m := &member{
		conn:   ours,
		frames: make(chan []byte, 64),
		mac:    mustMAC(t, mac),
		ip:     net.ParseIP(ip).To4(),
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := ours.Read(buf)
			if err != nil {
				return
			}
			frame := make([]byte, n)
			copy(frame, buf[:n])
			// A real guest answers ARP, and the stack will not send a reply
			// to a member whose MAC it cannot resolve.
			if reply := m.arpReplyTo(frame); reply != nil {
				go func() { _, _ = ours.Write(reply) }()
				continue
			}
			select {
			case m.frames <- frame:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ours.Close()
	})
	return m
}

// arpReplyTo answers an ARP request for this member's address, and returns
// nil for anything else.
func (m *member) arpReplyTo(frame []byte) []byte {
	if len(frame) < header.EthernetMinimumSize+header.ARPSize {
		return nil
	}
	if header.Ethernet(frame).Type() != header.ARPProtocolNumber {
		return nil
	}
	request := header.ARP(frame[header.EthernetMinimumSize:])
	if request.Op() != header.ARPRequest || !net.IP(request.ProtocolAddressTarget()).Equal(m.ip) {
		return nil
	}

	reply := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(reply).Encode(&header.EthernetFields{
		SrcAddr: m.mac,
		DstAddr: header.Ethernet(frame).SourceAddress(),
		Type:    header.ARPProtocolNumber,
	})
	a := header.ARP(reply[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPReply)
	copy(a.HardwareAddressSender(), m.mac)
	copy(a.ProtocolAddressSender(), m.ip)
	copy(a.HardwareAddressTarget(), request.HardwareAddressSender())
	copy(a.ProtocolAddressTarget(), request.ProtocolAddressSender())
	return reply
}

func (m *member) send(t *testing.T, frame []byte) {
	t.Helper()
	_ = m.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := m.conn.Write(frame); err != nil {
		t.Fatalf("send frame: %v", err)
	}
}

// await returns the first frame matching pred, or fails after the deadline.
func (m *member) await(t *testing.T, what string, pred func([]byte) bool) []byte {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case frame := <-m.frames:
			if pred(frame) {
				return frame
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func testNetwork(t *testing.T) *Network {
	t.Helper()
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

func arpRequest(t *testing.T, m *member, target net.IP) []byte {
	t.Helper()
	frame := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: m.mac,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.ARPProtocolNumber,
	})
	a := header.ARP(frame[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPRequest)
	copy(a.HardwareAddressSender(), m.mac)
	copy(a.ProtocolAddressSender(), m.ip)
	copy(a.ProtocolAddressTarget(), target.To4())
	return frame
}

// TestGatewayAnswersARP is the smoke test for the whole composition: a frame
// from a member reaches the stack, and the stack answers for the address it
// was given.
func TestGatewayAnswersARP(t *testing.T) {
	n := testNetwork(t)
	m := join(t, n)
	m.send(t, arpRequest(t, m, net.ParseIP(testGatewayIP)))

	reply := m.await(t, "ARP reply", func(frame []byte) bool {
		if len(frame) < header.EthernetMinimumSize+header.ARPSize {
			return false
		}
		if header.Ethernet(frame).Type() != header.ARPProtocolNumber {
			return false
		}
		return header.ARP(frame[header.EthernetMinimumSize:]).Op() == header.ARPReply
	})

	a := header.ARP(reply[header.EthernetMinimumSize:])
	if got := net.IP(a.ProtocolAddressSender()).String(); got != testGatewayIP {
		t.Fatalf("ARP reply from %s, want %s", got, testGatewayIP)
	}
	if got := tcpip.LinkAddress(a.HardwareAddressSender()); got != mustMAC(t, testGatewayMA) {
		t.Fatalf("ARP reply carries MAC %s, want %s", got, testGatewayMA)
	}
}
