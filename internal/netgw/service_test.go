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
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const testServiceCIDR = "10.96.0.0/12"

// testServiceWait bounds every wait on a connection through the test
// network. A handshake there crosses two netstacks and two switches, which
// under the race detector on a loaded host takes seconds rather than
// milliseconds.
const testServiceWait = 30 * time.Second

// peer is a second netstack on the gateway's switch. It stands in for a guest
// with a real TCP/IP stack, where a test needs a connection through a service
// to complete rather than only start.
func peer(t *testing.T, gw *Network, ip, mac string, zones []gvntypes.Zone) *Network {
	t.Helper()
	p, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         ip,
		GatewayMacAddress: mac,
		DNSZones:          zones,
	})
	if err != nil {
		t.Fatalf("New peer: %v", err)
	}
	// A guest resets a connection to a port nothing listens on. The peer's
	// own forwarders would dial it out from the host instead.
	p.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, nil)
	p.stack.SetTransportProtocolHandler(udp.ProtocolNumber, nil)
	// A datagram socketpair as a VMM member uses, not net.Pipe. Two switches
	// joined by an unbuffered pipe deadlock when both write at once.
	ours, theirs := socketpair(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = gw.AcceptVfkit(ctx, theirs) }()
	go func() { _ = p.AcceptVfkit(ctx, ours) }()
	// A guest sends service traffic to its default gateway. The peer's own
	// table routes only the subnet, so give it the service range via the
	// gateway under test.
	_, svc, _ := net.ParseCIDR(testServiceCIDR)
	dst, _ := tcpip.NewSubnet(tcpip.AddrFromSlice(svc.IP.To4()), tcpip.MaskFromBytes(svc.Mask))
	p.stack.AddRoute(tcpip.Route{Destination: dst, Gateway: tcpip.AddrFrom4Slice(net.ParseIP(testGatewayIP).To4()), NIC: nicID})
	t.Cleanup(func() {
		cancel()
		_ = ours.Close()
		_ = theirs.Close()
	})
	// The switch takes a port when its Accept goroutine runs, and drops what
	// is sent to a port it does not have yet. Wait until the peer reaches the
	// gateway's own resolver, so a test starts on a joined network.
	deadline := time.Now().Add(testServiceWait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), testServiceWait/4)
		conn, err := gonet.DialContextTCP(ctx, p.stack, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4Slice(net.ParseIP(testGatewayIP).To4()), Port: 53}, ipv4.ProtocolNumber)
		cancel()
		if err == nil {
			_ = conn.Close()
			return p
		}
		t.Logf("peer %s cannot reach the gateway yet: %v", ip, err)
		if time.Now().After(deadline) {
			t.Fatalf("peer %s never reached the gateway: %v", ip, err)
		}
		// A refusal from the neighbor cache comes back at once. Retrying
		// without a pause would only keep the CPU from the stacks.
		time.Sleep(200 * time.Millisecond)
	}
}

func socketpair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4<<20)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
		f := os.NewFile(uintptr(fd), "peer")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}
		conns[i] = c
	}
	return conns[0], conns[1]
}

func serviceNetwork(t *testing.T, policy *Policy) (*Network, chan dialed) {
	t.Helper()
	dials := make(chan dialed, 16)
	n, err := New(Config{
		MTU:                1500,
		Subnet:             testSubnet,
		GatewayIP:          testGatewayIP,
		GatewayMacAddress:  testGatewayMA,
		Egress:             policy,
		ServiceCIDR:        netip.MustParsePrefix(testServiceCIDR),
		serviceDialTimeout: testServiceWait / 2,
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

// serve answers one connection on ip:port of p with "hello", and reports the
// address the connection came from.
func serve(t *testing.T, p *Network, ip string, port uint16) chan string {
	t.Helper()
	ln, err := gonet.ListenTCP(p.stack, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4Slice(net.ParseIP(ip).To4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	from := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		from <- c.RemoteAddr().String()
		_, _ = c.Write([]byte("hello"))
		_ = c.Close()
	}()
	return from
}

// readService connects from p to vip:port and returns what the far end sent.
func readService(t *testing.T, p *Network, vip string, port uint16) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testServiceWait)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, p.stack, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4Slice(net.ParseIP(vip).To4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial %s:%d: %v", vip, port, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(testServiceWait))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read from %s:%d: %v", vip, port, err)
	}
	return string(got)
}

// A connection to a service address reaches a guest endpoint through the
// stack, and never leaves the host. The endpoint sees the gateway as the
// source: the gateway terminates the client's connection and opens its own.
func TestServiceTCPToAGuestEndpoint(t *testing.T) {
	n, dials := serviceNetwork(t, nil)
	server := peer(t, n, "10.87.0.3", "5a:94:ef:e4:0c:03", nil)
	client := peer(t, n, "10.87.0.4", "5a:94:ef:e4:0c:04", nil)
	from := serve(t, server, "10.87.0.3", 8080)

	if err := n.SetServices([]Service{{
		VIP: netip.MustParseAddr("10.96.0.20"), Port: 80,
		Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("10.87.0.3"), Port: 8080}},
	}}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}

	if got := readService(t, client, "10.96.0.20", 80); got != "hello" {
		t.Fatalf("read %q", got)
	}
	if src := <-from; hostOf(src) != testGatewayIP {
		t.Fatalf("the endpoint saw %s, want the gateway %s", src, testGatewayIP)
	}
	expectNoDial(t, dials)
}

// An endpoint that refuses is passed over for the next one, so a connection
// lands while the caller's table still lists a backend that went away.
func TestServiceTCPFailsOverToTheNextEndpoint(t *testing.T) {
	n, _ := serviceNetwork(t, nil)
	server := peer(t, n, "10.87.0.3", "5a:94:ef:e4:0c:03", nil)
	client := peer(t, n, "10.87.0.4", "5a:94:ef:e4:0c:04", nil)
	serve(t, server, "10.87.0.3", 8080)

	if err := n.SetServices([]Service{{
		VIP: netip.MustParseAddr("10.96.0.20"), Port: 80,
		Endpoints: []ServiceEndpoint{
			{IP: netip.MustParseAddr("10.87.0.3"), Port: 9},
			{IP: netip.MustParseAddr("10.87.0.3"), Port: 8080},
		},
	}}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	if got := readService(t, client, "10.96.0.20", 80); got != "hello" {
		t.Fatalf("read %q", got)
	}
}

// A host endpoint is dialed from the host. That is egress in form only: the
// destination is the table's, not the guest's, so deny-default does not stop
// it.
func TestServiceHostEndpointBypassesEgress(t *testing.T) {
	n, dials := serviceNetwork(t, mustPolicy(t, "deny", nil, nil))
	m := join(t, n)
	if err := n.SetServices([]Service{{
		VIP: netip.MustParseAddr("10.96.0.1"), Port: 443,
		Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("127.0.0.1"), Port: 6443, Host: true}},
	}}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	m.send(t, tcpSYN4(t, m, "10.96.0.1", 443))
	if got := awaitDial(t, dials); got.network != "tcp" || got.address != "127.0.0.1:6443" {
		t.Fatalf("dialed %v", got)
	}
}

// awaitFrame delivers the first frame m receives that matches pred. It is
// member.await with a bound of testServiceWait rather than three seconds.
func awaitFrame(m *member, pred func([]byte) bool) <-chan []byte {
	out := make(chan []byte, 1)
	go func() {
		for frame := range m.frames {
			if pred(frame) {
				out <- frame
				return
			}
		}
	}()
	return out
}

func isRST(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.TCPMinimumSize {
		return false
	}
	if header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		return false
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if ip.Protocol() != uint8(header.TCPProtocolNumber) {
		return false
	}
	return header.TCP(frame[header.EthernetMinimumSize+int(ip.HeaderLength()):]).Flags()&header.TCPFlagRst != 0
}

// An address in the service range is never egress. One the table does not
// list, or lists with no endpoint, is refused with a reset and not dialed
// from the host, whatever the egress policy says.
func TestServiceWithoutAnEndpointIsReset(t *testing.T) {
	for _, tc := range []struct {
		what     string
		services []Service
	}{
		{"no service at the address", nil},
		{"a service with no endpoints", []Service{{VIP: netip.MustParseAddr("10.96.0.5"), Port: 80, Endpoints: nil}}},
		{"a service on another port", []Service{{VIP: netip.MustParseAddr("10.96.0.5"), Port: 81,
			Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("127.0.0.1"), Port: 1, Host: true}}}}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			n, dials := serviceNetwork(t, nil)
			m := join(t, n)
			if err := n.SetServices(tc.services); err != nil {
				t.Fatalf("SetServices: %v", err)
			}
			m.send(t, tcpSYN4(t, m, "10.96.0.5", 80))
			select {
			case <-awaitFrame(m, isRST):
			case <-time.After(testServiceWait):
				t.Fatal("no reset")
			}
			expectNoDial(t, dials)
		})
	}
}

func TestServiceUDPWithoutAnEndpointIsDropped(t *testing.T) {
	n, dials := serviceNetwork(t, nil)
	m := join(t, n)
	m.send(t, udpDatagram4(t, m, "10.96.0.10", 53))
	expectNoDial(t, dials)
}

// Without a service range the table is inert: the range's addresses are
// egress like any other, and a table cannot be installed.
func TestServiceTableIsInertWithoutARange(t *testing.T) {
	n, dials := filteredNetwork(t, nil)
	m := join(t, n)
	err := n.SetServices([]Service{{VIP: netip.MustParseAddr("10.96.0.1"), Port: 443}})
	if !errors.Is(err, ErrServiceInvalid) {
		t.Fatalf("SetServices without a range = %v, want ErrServiceInvalid", err)
	}
	m.send(t, tcpSYN4(t, m, "10.96.0.1", 443))
	if got := awaitDial(t, dials); got.address != "10.96.0.1:443" {
		t.Fatalf("dialed %v", got)
	}
}

func TestServiceRangeMustNotOverlapTheSubnet(t *testing.T) {
	for _, cidr := range []string{"10.87.0.0/24", "10.87.0.128/25", "10.0.0.0/8", "fd00::/64"} {
		_, err := New(Config{
			MTU: 1500, Subnet: testSubnet, GatewayIP: testGatewayIP, GatewayMacAddress: testGatewayMA,
			ServiceCIDR: netip.MustParsePrefix(cidr),
		})
		if err == nil {
			t.Fatalf("service range %s was accepted beside subnet %s", cidr, testSubnet)
		}
	}
}

func testTable() *ServiceTable {
	return NewServiceTable(netip.MustParsePrefix(testServiceCIDR), netip.MustParsePrefix(testSubnet))
}

func TestServiceTableValidation(t *testing.T) {
	guest := []ServiceEndpoint{{IP: netip.MustParseAddr("10.87.0.12"), Port: 53}}
	vip := netip.MustParseAddr("10.96.0.10")
	for _, tc := range []struct {
		what string
		in   []Service
		ok   bool
	}{
		{"a guest endpoint", []Service{{VIP: vip, Port: 53, Protocol: "udp", Endpoints: guest}}, true},
		{"a host endpoint off the subnet", []Service{{VIP: vip, Port: 443,
			Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("127.0.0.1"), Port: 6443, Host: true}}}}, true},
		{"no endpoints", []Service{{VIP: vip, Port: 80}}, true},
		{"an empty table", nil, true},
		{"the protocol in upper case", []Service{{VIP: vip, Port: 53, Protocol: "UDP", Endpoints: guest}}, true},
		{"the same address and port on both protocols", []Service{
			{VIP: vip, Port: 53, Protocol: "udp", Endpoints: guest},
			{VIP: vip, Port: 53, Protocol: "tcp", Endpoints: guest}}, true},
		{"a virtual address outside the range", []Service{{VIP: netip.MustParseAddr("10.112.0.1"), Port: 80}}, false},
		{"a virtual address on the subnet", []Service{{VIP: netip.MustParseAddr("10.87.0.5"), Port: 80}}, false},
		{"an IPv6 virtual address", []Service{{VIP: netip.MustParseAddr("fd00::1"), Port: 80}}, false},
		{"no virtual address", []Service{{Port: 80}}, false},
		{"no port", []Service{{VIP: vip, Endpoints: guest}}, false},
		{"an unknown protocol", []Service{{VIP: vip, Port: 80, Protocol: "sctp"}}, false},
		{"a guest endpoint off the subnet", []Service{{VIP: vip, Port: 443,
			Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("127.0.0.1"), Port: 6443}}}}, false},
		{"an endpoint with no port", []Service{{VIP: vip, Port: 53,
			Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("10.87.0.12")}}}}, false},
		{"an IPv6 endpoint", []Service{{VIP: vip, Port: 53,
			Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("fd00::2"), Port: 53, Host: true}}}}, false},
		{"one service twice", []Service{
			{VIP: vip, Port: 53, Protocol: "udp", Endpoints: guest},
			{VIP: vip, Port: 53, Protocol: "UDP"}}, false},
		{"one service twice, the protocol left out once", []Service{
			{VIP: vip, Port: 80, Protocol: "tcp"},
			{VIP: vip, Port: 80}}, false},
	} {
		err := testTable().SetServices(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%s: refused: %v", tc.what, err)
		}
		if !tc.ok && !errors.Is(err, ErrServiceInvalid) {
			t.Errorf("%s: got %v, want ErrServiceInvalid", tc.what, err)
		}
	}
}

// A table with one bad entry is refused whole, and the table already
// installed stays as it was.
func TestServiceTableReplacesAtomically(t *testing.T) {
	table := testTable()
	good := []Service{{VIP: netip.MustParseAddr("10.96.0.1"), Port: 443,
		Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("127.0.0.1"), Port: 6443, Host: true}}}}
	if err := table.SetServices(good); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	bad := []Service{
		{VIP: netip.MustParseAddr("10.96.0.2"), Port: 80},
		{VIP: netip.MustParseAddr("192.0.2.1"), Port: 80},
	}
	if err := table.SetServices(bad); err == nil {
		t.Fatal("a table with an address outside the range was installed")
	}
	got := table.Services()
	if len(got) != 1 || got[0].VIP != good[0].VIP {
		t.Fatalf("after a refused replace the table is %+v", got)
	}
	if _, ok := table.route(netip.MustParseAddr("10.96.0.2"), 80, "tcp"); ok {
		t.Fatal("part of the refused table is routed")
	}
	if err := table.SetServices(nil); err != nil {
		t.Fatalf("SetServices(nil): %v", err)
	}
	if got := table.Services(); got == nil || len(got) != 0 {
		t.Fatalf("an emptied table reads %#v, want an empty list", got)
	}
}

// The table reads back as installed: defaults filled in, and in a fixed
// order so an unchanged table renders the same twice.
func TestServiceTableReadsBackAsInstalled(t *testing.T) {
	table := testTable()
	if err := table.SetServices([]Service{
		{VIP: netip.MustParseAddr("10.96.0.10"), Port: 53, Protocol: "UDP"},
		{VIP: netip.MustParseAddr("10.96.0.1"), Port: 443},
		{VIP: netip.MustParseAddr("10.96.0.10"), Port: 53, Protocol: "tcp"},
	}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	got := table.Services()
	want := []struct {
		vip      string
		port     uint16
		protocol string
	}{{"10.96.0.1", 443, "tcp"}, {"10.96.0.10", 53, "tcp"}, {"10.96.0.10", 53, "udp"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].VIP.String() != w.vip || got[i].Port != w.port || got[i].Protocol != w.protocol {
			t.Fatalf("entry %d is %+v, want %+v", i, got[i], w)
		}
		if got[i].Endpoints == nil {
			t.Fatalf("entry %d has null endpoints, want an empty list", i)
		}
	}
}

func TestServiceRoundRobin(t *testing.T) {
	table := testTable()
	vip := netip.MustParseAddr("10.96.0.20")
	eps := []ServiceEndpoint{
		{IP: netip.MustParseAddr("10.87.0.11"), Port: 80},
		{IP: netip.MustParseAddr("10.87.0.12"), Port: 80},
		{IP: netip.MustParseAddr("10.87.0.13"), Port: 80},
	}
	if err := table.SetServices([]Service{{VIP: vip, Port: 80, Endpoints: eps}}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	for i := range 7 {
		order, ok := table.route(vip, 80, "tcp")
		if !ok || len(order) != len(eps) {
			t.Fatalf("route %d: %v %v", i, order, ok)
		}
		// Each call leads with the next endpoint, and still offers the rest
		// in turn behind it for failover.
		for j := range order {
			if want := eps[(i+j)%len(eps)]; order[j] != want {
				t.Fatalf("route %d position %d is %s, want %s", i, j, order[j], want)
			}
		}
	}
	if _, ok := table.route(vip, 80, "udp"); ok {
		t.Fatal("a tcp service answered for udp")
	}
}

// DNS to a resolver in a guest, which is how cluster DNS is reached: a UDP
// flow to the service address is carried to the endpoint and the answer comes
// back.
func TestServiceUDPToAGuestEndpoint(t *testing.T) {
	n, dials := serviceNetwork(t, nil)
	_ = peer(t, n, "10.87.0.3", "5a:94:ef:e4:0c:03", []gvntypes.Zone{{
		Name:    "cluster.local.",
		Records: []gvntypes.Record{{Name: "web", IP: net.ParseIP("10.96.0.20")}},
	}})
	client := peer(t, n, "10.87.0.4", "5a:94:ef:e4:0c:04", nil)
	if err := n.SetServices([]Service{{
		VIP: netip.MustParseAddr("10.96.0.10"), Port: 53, Protocol: "udp",
		Endpoints: []ServiceEndpoint{{IP: netip.MustParseAddr("10.87.0.3"), Port: 53}},
	}}); err != nil {
		t.Fatalf("SetServices: %v", err)
	}
	conn, err := gonet.DialUDP(client.stack, nil, &tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4([4]byte{10, 96, 0, 10}), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	q := new(dns.Msg)
	q.SetQuestion("web.cluster.local.", dns.TypeA)
	c := &dns.Conn{Conn: conn}
	// Asked again until answered, as a resolver does. The first datagram can
	// be dropped while the gateway is still resolving the endpoint's address.
	var r *dns.Msg
	deadline := time.Now().Add(testServiceWait)
	for r == nil {
		if time.Now().After(deadline) {
			t.Fatal("no answer through the service")
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := c.WriteMsg(q); err != nil {
			t.Fatalf("write: %v", err)
		}
		r, _ = c.ReadMsg()
	}
	if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "10.96.0.20" {
		t.Fatalf("answer %v", r.Answer)
	}
	expectNoDial(t, dials)
}
