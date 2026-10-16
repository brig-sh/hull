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
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestParseDivertsReadsTheFlagGrammar(t *testing.T) {
	got, err := ParseDiverts([]string{
		"API.Example.com.:443=/run/brig/s1/divert.sock",
		"93.184.216.34:8443=/tmp/a/../b.sock",
		"[2606:2800:220::1]:443=/tmp/v6.sock",
		"[::ffff:1.2.3.4]:443=/tmp/mapped.sock",
	})
	if err != nil {
		t.Fatalf("ParseDiverts: %v", err)
	}
	want := []Divert{
		{Host: "api.example.com", Port: 443, Socket: "/run/brig/s1/divert.sock"},
		{Host: "93.184.216.34", Port: 8443, Socket: "/tmp/b.sock"},
		{Host: "2606:2800:220::1", Port: 443, Socket: "/tmp/v6.sock"},
		{Host: "1.2.3.4", Port: 443, Socket: "/tmp/mapped.sock"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d diverts, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("divert %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDivertsRefusesWhatCannotBeMatched(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"no socket", []string{"api.example.com:443"}, "expected host:port=socket"},
		{"no port", []string{"api.example.com=/tmp/s"}, "expected host:port=socket"},
		{"port zero", []string{"api.example.com:0=/tmp/s"}, "not in 1-65535"},
		{"port too large", []string{"api.example.com:65536=/tmp/s"}, "not in 1-65535"},
		{"named port", []string{"api.example.com:https=/tmp/s"}, "not in 1-65535"},
		{"relative socket", []string{"api.example.com:443=divert.sock"}, "must be absolute"},
		{"empty socket", []string{"api.example.com:443="}, "must be absolute"},
		{"glob", []string{"*.example.com:443=/tmp/s"}, "neither a host name nor an IP"},
		{"empty host", []string{":443=/tmp/s"}, "neither a host name nor an IP"},
		{"zone", []string{"[fe80::1%en0]:443=/tmp/s"}, "zone"},
		{"duplicate", []string{"api.example.com:443=/tmp/a", "API.example.com.:443=/tmp/b"}, "diverted twice"},
		{"duplicate literal", []string{"1.2.3.4:443=/tmp/a", "[::ffff:1.2.3.4]:443=/tmp/b"}, "diverted twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDiverts(tc.values)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

// The same host on two ports is two diverts.
func TestParseDivertsAcceptsOneHostOnTwoPorts(t *testing.T) {
	if _, err := ParseDiverts([]string{"api.example.com:443=/tmp/a", "api.example.com:8443=/tmp/a"}); err != nil {
		t.Fatalf("ParseDiverts: %v", err)
	}
}

// injector is the far end of a divert socket. It reads the preamble of every
// connection and hands the connection to the test.
type injector struct {
	path  string
	conns chan *injected
}

type injected struct {
	conn     *net.UnixConn
	r        *bufio.Reader
	preamble string
}

// shortDir returns a fresh directory with a short path. macOS caps a socket
// path at 104 bytes, and t.TempDir can exceed that.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hdv")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newInjector listens on a fresh socket.
func newInjector(t *testing.T) *injector {
	t.Helper()
	path := filepath.Join(shortDir(t), "i.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	inj := &injector{path: path, conns: make(chan *injected, 8)}
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			_ = conn.SetReadDeadline(time.Now().Add(replyWait))
			line, err := r.ReadString('\n')
			_ = conn.SetReadDeadline(time.Time{})
			if err != nil {
				_ = conn.Close()
				continue
			}
			inj.conns <- &injected{conn: conn, r: r, preamble: line}
		}
	}()
	return inj
}

func (i *injector) next(t *testing.T) *injected {
	t.Helper()
	select {
	case c := <-i.conns:
		t.Cleanup(func() { _ = c.conn.Close() })
		return c
	case <-time.After(replyWait):
		t.Fatal("nothing arrived on the divert socket")
		return nil
	}
}

func (i *injector) expectNone(t *testing.T) {
	t.Helper()
	select {
	case c := <-i.conns:
		t.Fatalf("a connection was diverted: %q", c.preamble)
	case <-time.After(500 * time.Millisecond):
	}
}

// readN reads exactly n bytes of what the guest sent.
func (c *injected) readN(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_ = c.conn.SetReadDeadline(time.Now().Add(replyWait))
	if _, err := io.ReadFull(c.r, buf); err != nil {
		t.Fatalf("read %d bytes from the guest: %v", n, err)
	}
	return string(buf)
}

// divertNetwork builds a network whose direct dials are recorded and never
// connect, so a test can tell a diverted connection from a dialed one.
func divertNetwork(t *testing.T, policy *Policy, res resolver, divs ...string) (*Network, chan dialed) {
	t.Helper()
	parsed, err := ParseDiverts(divs)
	if err != nil {
		t.Fatalf("ParseDiverts: %v", err)
	}
	dials := make(chan dialed, 16)
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Egress:            policy,
		Diverts:           parsed,
		resolve:           res,
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

func preambleFor(dst string, dstPort uint16, srcPort uint16) string {
	return fmt.Sprintf("BRIG-DIVERT/1 %s %d %s %d\n", dst, dstPort, testGuestIP, srcPort)
}

func TestDivertHandsTheConnectionToTheSocket(t *testing.T) {
	inj := newInjector(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+inj.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40101)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	got := inj.next(t)
	if want := preambleFor(testDst4, 443, 40101); got.preamble != want {
		t.Fatalf("preamble %q, want %q", got.preamble, want)
	}

	// Bytes outside printable ASCII, so a relay that treated the stream as
	// text would show.
	hello := "\x16\x03\x01\x00\xffclient\x00hello\r\n"
	reply := "\x16\x03\x03\x00\x7fserver\x00hello\n"
	client.m.send(t, client.segment(t, header.TCPFlagPsh|header.TCPFlagAck, []byte(hello)))
	client.seq += uint32(len(hello))
	if read := got.readN(t, len(hello)); read != hello {
		t.Fatalf("the socket read %q, want %q", read, hello)
	}
	if _, err := io.WriteString(got.conn, reply); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	if received := client.receiveData(t); received != reply {
		t.Fatalf("the guest received %q, want %q", received, reply)
	}
	expectNoDial(t, dials)

	if st := n.Diverts(); len(st) != 1 || st[0].Diverted != 1 || st[0].Failed != 0 {
		t.Fatalf("status %+v, want one diverted connection", st)
	}
}

func TestDivertLeavesOtherDestinationsAlone(t *testing.T) {
	inj := newInjector(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+inj.path)
	m := join(t, n)

	m.send(t, tcpSYN4(t, m, testDst4, 80))
	if got := awaitDial(t, dials); got.address != testDst4+":80" {
		t.Fatalf("dialed %v", got)
	}
	m.send(t, tcpSYN4(t, m, "1.1.1.1", 443))
	if got := awaitDial(t, dials); got.address != "1.1.1.1:443" {
		t.Fatalf("dialed %v", got)
	}
	inj.expectNone(t)
}

// UDP to a diverted destination is dropped, so QUIC cannot go around the
// divert. Other UDP is forwarded as before.
func TestDivertDropsUDPToTheDivertedDestination(t *testing.T) {
	inj := newInjector(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+inj.path)
	m := join(t, n)

	m.send(t, udpDatagram4(t, m, testDst4, 443))
	expectNoDial(t, dials)

	m.send(t, udpDatagram4(t, m, testDst4, 53))
	if got := awaitDial(t, dials); got.network != "udp" || got.address != testDst4+":53" {
		t.Fatalf("dialed %v", got)
	}
}

// A diverted connection is never dialed, whatever the policy says about its
// destination.
func TestDivertTakesPrecedenceOverTheEgressPolicy(t *testing.T) {
	policies := map[string]*Policy{
		"allowed": mustPolicy(t, "deny", []string{"cidr=" + testDst4 + "/32"}, nil),
		"denied":  mustPolicy(t, "deny", nil, nil),
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			inj := newInjector(t)
			n, dials := divertNetwork(t, policy, nil, testDst4+":443="+inj.path)
			m := join(t, n)

			client := newTCPClient(m, testDst4, 443, 40102)
			if !client.connect(t) {
				t.Fatal("the diverted connection was refused")
			}
			if got := inj.next(t); got.preamble != preambleFor(testDst4, 443, 40102) {
				t.Fatalf("preamble %q", got.preamble)
			}
			expectNoDial(t, dials)
		})
	}
}

// A named divert, end to end. The address means nothing until the guest
// resolves the name through the gateway. Then the diverted port goes to the
// socket and every other port of that address stays under the policy.
func TestDivertFollowsTheGatewaysDNSAnswer(t *testing.T) {
	const addr = "203.0.113.7"
	inj := newInjector(t)
	res := newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(addr)})
	n, dials := divertNetwork(t, mustPolicy(t, "deny", nil, nil), res, "api.example.com:443="+inj.path)
	m := join(t, n)

	before := newTCPClient(m, addr, 443, 40103)
	if before.connect(t) {
		t.Fatal("the address was reachable before the name was resolved")
	}
	inj.expectNone(t)

	// Deny-default allows no name, and the divert is answered all the same.
	m.send(t, dnsQueryFrame(t, m, "api.example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != addr {
		t.Fatalf("A api.example.com = %v", got)
	}

	after := newTCPClient(m, addr, 443, 40104)
	if !after.connect(t) {
		t.Fatal("the resolved address was not diverted")
	}
	if got := inj.next(t); got.preamble != preambleFor(addr, 443, 40104) {
		t.Fatalf("preamble %q", got.preamble)
	}

	// The answer was not pinned, so the policy still refuses another port.
	other := newTCPClient(m, addr, 80, 40105)
	if other.connect(t) {
		t.Fatal("answering the diverted name opened the rest of its address")
	}
	expectNoDial(t, dials)
}

// The gateway resolves the host itself at start, so a guest that never asks
// is diverted all the same.
func TestDivertResolvesTheHostAtStart(t *testing.T) {
	const addr = "203.0.113.8"
	inj := newInjector(t)
	res := newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(addr)})
	n, dials := divertNetwork(t, nil, res, "api.example.com:443="+inj.path)
	n.Start(t.Context())
	m := join(t, n)

	client := newTCPClient(m, addr, 443, 40106)
	if !client.connect(t) {
		t.Fatal("the resolved address was not diverted")
	}
	if got := inj.next(t); got.preamble != preambleFor(addr, 443, 40106) {
		t.Fatalf("preamble %q", got.preamble)
	}
	expectNoDial(t, dials)
}

// With no socket to hand the connection to, the guest is reset. The policy is
// off here, so a fallback to the direct path would show as a dial.
func TestDivertResetsWhenTheSocketIsUnavailable(t *testing.T) {
	missing := filepath.Join(shortDir(t), "absent.sock")
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+missing)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40107)
	if client.connect(t) {
		t.Fatal("the guest completed a handshake with no socket behind it")
	}
	expectNoDial(t, dials)
	if st := n.Diverts(); st[0].Failed != 1 || st[0].Diverted != 0 {
		t.Fatalf("status %+v, want one failed connection", st)
	}
}

// receiveUntil collects the payload of this connection's segments until one
// carries any of flags, and returns it with that segment's flags. Data is
// acknowledged, and a retransmitted segment is counted once.
func (c *tcpClient) receiveUntil(t *testing.T, flags header.TCPFlags) (string, header.TCPFlags) {
	t.Helper()
	var data []byte
	for {
		seg, payload := c.receive(t, "the end of the stream")
		if len(payload) > 0 && seg.SequenceNumber() == c.ack {
			data = append(data, payload...)
			c.ack += uint32(len(payload))
			c.m.send(t, c.segment(t, header.TCPFlagAck, nil))
		}
		if seg.Flags()&flags != 0 {
			return string(data), seg.Flags()
		}
	}
}

// receiveData returns the payload of the next segment that carries one.
func (c *tcpClient) receiveData(t *testing.T) string {
	t.Helper()
	for {
		if _, payload := c.receive(t, "data"); len(payload) > 0 {
			return string(payload)
		}
	}
}

// The socket owner refuses a connection by closing it unanswered, and the
// guest sees a reset.
func TestDivertRefusalResetsTheGuest(t *testing.T) {
	inj := newInjector(t)
	n, _ := divertNetwork(t, nil, nil, testDst4+":443="+inj.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40108)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	_ = inj.next(t).conn.Close()
	if _, flags := client.receiveUntil(t, header.TCPFlagRst|header.TCPFlagFin); flags&header.TCPFlagRst == 0 {
		t.Fatalf("the guest got flags %v, want a reset", flags)
	}
}

// Each side's EOF reaches the other as a half-close, and the other direction
// keeps working after it.
func TestDivertPropagatesHalfClose(t *testing.T) {
	inj := newInjector(t)
	n, _ := divertNetwork(t, nil, nil, testDst4+":443="+inj.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40109)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	got := inj.next(t)

	const request, response = "request", "response after EOF"
	client.m.send(t, client.segment(t, header.TCPFlagPsh|header.TCPFlagAck, []byte(request)))
	client.seq += uint32(len(request))
	client.m.send(t, client.segment(t, header.TCPFlagFin|header.TCPFlagAck, nil))
	client.seq++

	if read := got.readN(t, len(request)); read != request {
		t.Fatalf("the socket read %q, want %q", read, request)
	}
	if rest, err := io.ReadAll(got.r); err != nil || len(rest) != 0 {
		t.Fatalf("after the guest's FIN the socket read %q, %v; want EOF", rest, err)
	}
	if _, err := io.WriteString(got.conn, response); err != nil {
		t.Fatalf("write after the guest's FIN: %v", err)
	}
	if err := got.conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	data, flags := client.receiveUntil(t, header.TCPFlagRst|header.TCPFlagFin)
	if flags&header.TCPFlagRst != 0 {
		t.Fatalf("the guest got a reset, want a FIN")
	}
	if data != response {
		t.Fatalf("the guest received %q, want %q", data, response)
	}
}

// brig.internal, as brig asks for it: a --host record of the gateway's own
// address, and a divert of its port 443. The name is answered under
// deny-default, port 443 of the address goes to the socket, and every other
// port of it is reset and never dialed, with or without a policy. UDP to it
// is dropped.
func TestTheGatewayAddressAnswersOnlyWhatItServes(t *testing.T) {
	for name, policy := range map[string]*Policy{
		"no policy":    nil,
		"allow":        mustPolicy(t, "allow", nil, nil),
		"deny default": mustPolicy(t, "deny", nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			inj := newInjector(t)
			parsed, err := ParseDiverts([]string{testGatewayIP + ":443=" + inj.path})
			if err != nil {
				t.Fatal(err)
			}
			dials := make(chan dialed, 16)
			n, err := New(Config{
				MTU:               1500,
				Subnet:            testSubnet,
				GatewayIP:         testGatewayIP,
				GatewayMacAddress: testGatewayMA,
				Egress:            policy,
				Diverts:           parsed,
				DNSZones: []gvntypes.Zone{{Name: ".", Records: []gvntypes.Record{
					{Name: "brig.internal", IP: net.ParseIP(testGatewayIP)}}}},
				resolve: newFakeResolver(nil),
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
			m := join(t, n)

			m.send(t, dnsQueryFrame(t, m, "brig.internal", dns.TypeA))
			if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != testGatewayIP {
				t.Fatalf("A brig.internal = %v", got)
			}

			client := newTCPClient(m, testGatewayIP, 443, 40301)
			if !client.connect(t) {
				t.Fatal("port 443 of the gateway was not diverted")
			}
			if got := inj.next(t); got.preamble != preambleFor(testGatewayIP, 443, 40301) {
				t.Fatalf("preamble %q", got.preamble)
			}

			for i, port := range []uint16{80, 8443, 22} {
				if newTCPClient(m, testGatewayIP, port, 40302+uint16(i)).connect(t) {
					t.Errorf("port %d of the gateway completed a handshake", port)
				}
			}
			m.send(t, udpDatagram4(t, m, testGatewayIP, 443))
			m.send(t, udpDatagram4(t, m, testGatewayIP, 123))
			expectNoDial(t, dials)
			inj.expectNone(t)
		})
	}
}
