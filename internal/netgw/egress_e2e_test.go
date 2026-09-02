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
	"sync/atomic"
	"testing"
	"time"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// These tests carry real bytes. The gateway dials with net.Dial, the other end
// is a real listener on a real host socket, and the guest speaks TCP over the
// virtual network a frame at a time. What they demonstrate is not that a dial
// was attempted but that a conversation either happened or did not.

// echoServer is the outside world: a real listener that echoes what it reads
// and counts the connections it accepted.
type echoServer struct {
	ln       net.Listener
	accepted atomic.Int64
}

// hostIPv4 is a non-loopback address of this machine. The netstack drops a
// packet addressed to 127.0.0.0/8 arriving on a NIC that is not the loopback,
// the same martian check a kernel makes, so the far end of these tests cannot
// live on localhost.
func hostIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot list the host's addresses: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	t.Skip("this host has no non-loopback IPv4 address to put the far end of the test on")
	return ""
}

func newEchoServer(t *testing.T, host string) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	s := &echoServer{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 512)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *echoServer) port() uint16 { return uint16(s.ln.Addr().(*net.TCPAddr).Port) }

// tcpClient is the guest's TCP: enough of it to open a connection, say
// something and hear the answer.
type tcpClient struct {
	m       *member
	dst     string
	dstPort uint16
	srcPort uint16
	seq     uint32
	ack     uint32
}

func newTCPClient(m *member, dst string, dstPort, srcPort uint16) *tcpClient {
	return &tcpClient{m: m, dst: dst, dstPort: dstPort, srcPort: srcPort, seq: 1000}
}

// segment builds one Ethernet frame carrying a TCP segment. The link endpoint
// declares RX checksum offload, so the stack does not verify the transport
// checksum and there is none to compute here.
func (c *tcpClient) segment(t *testing.T, flags header.TCPFlags, payload []byte) []byte {
	t.Helper()
	frame := ethernet(t, c.m, header.IPv4MinimumSize+header.TCPMinimumSize+len(payload), header.IPv4ProtocolNumber)
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + header.TCPMinimumSize + len(payload)),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4Slice(c.m.ip),
		DstAddr:     tcpip.AddrFrom4Slice(net.ParseIP(c.dst).To4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	tcpStart := header.EthernetMinimumSize + header.IPv4MinimumSize
	header.TCP(frame[tcpStart:]).Encode(&header.TCPFields{
		SrcPort:    c.srcPort,
		DstPort:    c.dstPort,
		SeqNum:     c.seq,
		AckNum:     c.ack,
		DataOffset: header.TCPMinimumSize,
		Flags:      flags,
		WindowSize: 65535,
	})
	copy(frame[tcpStart+header.TCPMinimumSize:], payload)
	return frame
}

// receive returns the next TCP segment of this connection, and its payload.
func (c *tcpClient) receive(t *testing.T, what string) (header.TCP, []byte) {
	t.Helper()
	var seg header.TCP
	var payload []byte
	c.m.await(t, what, func(frame []byte) bool {
		if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize {
			return false
		}
		if header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
			return false
		}
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.Protocol() != uint8(header.TCPProtocolNumber) || ip.SourceAddress().String() != c.dst {
			return false
		}
		tcpStart := header.EthernetMinimumSize + int(ip.HeaderLength())
		t2 := header.TCP(frame[tcpStart:])
		if t2.SourcePort() != c.dstPort || t2.DestinationPort() != c.srcPort {
			return false
		}
		seg = t2
		payload = frame[tcpStart+int(t2.DataOffset()):]
		return true
	})
	return seg, payload
}

// connect performs the handshake and reports whether it completed. A refused
// connection comes back as a reset, which is what the filter produces.
func (c *tcpClient) connect(t *testing.T) bool {
	t.Helper()
	c.m.send(t, c.segment(t, header.TCPFlagSyn, nil))
	seg, _ := c.receive(t, "a SYN-ACK or a reset")
	if seg.Flags()&header.TCPFlagRst != 0 {
		return false
	}
	if seg.Flags()&(header.TCPFlagSyn|header.TCPFlagAck) != (header.TCPFlagSyn | header.TCPFlagAck) {
		t.Fatalf("expected a SYN-ACK, got flags %v", seg.Flags())
	}
	c.seq++
	c.ack = seg.SequenceNumber() + 1
	c.m.send(t, c.segment(t, header.TCPFlagAck, nil))
	return true
}

// say sends payload and returns what came back.
func (c *tcpClient) say(t *testing.T, payload string) string {
	t.Helper()
	c.m.send(t, c.segment(t, header.TCPFlagPsh|header.TCPFlagAck, []byte(payload)))
	c.seq += uint32(len(payload))
	for {
		seg, data := c.receive(t, "the echo")
		if len(data) > 0 {
			c.ack = seg.SequenceNumber() + uint32(len(data))
			return string(data)
		}
	}
}

// A CIDR rule, end to end: the guest holds a real conversation with a real
// listener when the rule admits it, and gets nothing at all when it does not.
func TestEgressCIDRCarriesRealBytes(t *testing.T) {
	host := hostIPv4(t)
	server := newEchoServer(t, host)
	policy := mustPolicy(t, "deny", []string{"cidr=" + host + "/32"}, nil)
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
	m := join(t, n)

	client := newTCPClient(m, host, server.port(), 40001)
	if !client.connect(t) {
		t.Fatal("the allowed destination refused the connection")
	}
	if got := client.say(t, "ping"); got != "ping" {
		t.Fatalf("echo returned %q, want %q", got, "ping")
	}
	if got := server.accepted.Load(); got != 1 {
		t.Fatalf("the server accepted %d connections, want 1", got)
	}

	// 203.0.113.0/24 is TEST-NET-3 and is not routable, so a filter that let
	// this through would be reaching for a network that does not answer
	// rather than quietly succeeding somewhere.
	blocked := newTCPClient(m, "203.0.113.1", 80, 40002)
	if blocked.connect(t) {
		t.Fatal("a destination outside the allow rule completed a handshake")
	}
}

// A host glob, end to end: the name has to be resolved through this gateway
// before its address means anything, and then real bytes flow.
func TestEgressHostGlobCarriesRealBytesAfterResolving(t *testing.T) {
	host := hostIPv4(t)
	server := newEchoServer(t, host)
	policy := mustPolicy(t, "deny", []string{"host=api.example.com"}, nil)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		DNSZones:          []gvntypes.Zone{},
		Egress:            policy,
		resolve:           newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(host)}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := join(t, n)

	// The address is known to the test but not to the policy: nothing has
	// resolved the name through this gateway yet.
	before := newTCPClient(m, host, server.port(), 40003)
	if before.connect(t) {
		t.Fatal("the address was reachable before the name was resolved")
	}
	if got := server.accepted.Load(); got != 0 {
		t.Fatalf("the server accepted %d connections before the lookup, want 0", got)
	}

	m.send(t, dnsQueryFrame(t, m, "api.example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != host {
		t.Fatalf("A api.example.com = %v", got)
	}

	after := newTCPClient(m, host, server.port(), 40004)
	if !after.connect(t) {
		t.Fatal("the resolved address is still not reachable")
	}
	if got := after.say(t, "hello"); got != "hello" {
		t.Fatalf("echo returned %q, want %q", got, "hello")
	}

	// A name the rules do not cover is refused, so its address is never
	// learned from here.
	m.send(t, dnsQueryFrame(t, m, "evil.test", dns.TypeA))
	if reply := dnsReply(t, m); reply.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode for evil.test = %s, want REFUSED", dns.RcodeToString[reply.Rcode])
	}
}

// The refresher, end to end: a name whose addresses rotate keeps working
// without the guest asking again, which is the case the timer exists for.
func TestEgressRefreshedHostCarriesRealBytes(t *testing.T) {
	host := hostIPv4(t)
	server := newEchoServer(t, host)
	policy := mustPolicy(t, "deny", []string{"host=api.example.com"}, nil)
	// The name answers with an address nothing is listening on, then rotates
	// to the one that is.
	res := newRotatingResolver("203.0.113.9")
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		resolve:           res,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := join(t, n)

	policy.RefreshHosts(t.Context(), res, time.Minute)
	unrotated := newTCPClient(m, host, server.port(), 40005)
	if unrotated.connect(t) {
		t.Fatal("an address the name never answered with was reachable")
	}

	res.set(host)
	policy.RefreshHosts(t.Context(), res, time.Minute)

	rotated := newTCPClient(m, host, server.port(), 40006)
	if !rotated.connect(t) {
		t.Fatal("the address the name rotated to is not reachable")
	}
	if got := rotated.say(t, "rotated"); got != "rotated" {
		t.Fatalf("echo returned %q, want %q", got, "rotated")
	}
}
