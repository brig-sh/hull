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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
		"[::ffff:1.2.3.4]:443=/tmp/mapped.sock",
		"169.254.170.2:80=/tmp/link-local.sock",
	})
	if err != nil {
		t.Fatalf("ParseDiverts: %v", err)
	}
	want := []Divert{
		{Host: "api.example.com", Port: 443, Socket: "/run/brig/s1/divert.sock"},
		{Host: "93.184.216.34", Port: 8443, Socket: "/tmp/b.sock"},
		{Host: "1.2.3.4", Port: 443, Socket: "/tmp/mapped.sock"},
		{Host: "169.254.170.2", Port: 80, Socket: "/tmp/link-local.sock"},
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
		{"IPv6", []string{"[2606:2800:220::1]:443=/tmp/s"}, "does not forward IPv6"},
		{"IPv6 link-local", []string{"[fe80::1]:443=/tmp/s"}, "does not forward IPv6"},
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

// socketOwner is the far end of a divert socket. It reads the preamble of every
// connection and hands the connection to the test.
type socketOwner struct {
	path  string
	conns chan *ownedConn
}

type ownedConn struct {
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

// newSocketOwner listens on a fresh socket.
func newSocketOwner(t *testing.T) *socketOwner {
	t.Helper()
	path := filepath.Join(shortDir(t), "i.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	owner := &socketOwner{path: path, conns: make(chan *ownedConn, 8)}
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
			owner.conns <- &ownedConn{conn: conn, r: r, preamble: line}
		}
	}()
	return owner
}

func (o *socketOwner) next(t *testing.T) *ownedConn {
	t.Helper()
	select {
	case c := <-o.conns:
		t.Cleanup(func() { _ = c.conn.Close() })
		return c
	case <-time.After(replyWait):
		t.Fatal("nothing arrived on the divert socket")
		return nil
	}
}

func (o *socketOwner) expectNone(t *testing.T) {
	t.Helper()
	select {
	case c := <-o.conns:
		t.Fatalf("a connection was diverted: %q", c.preamble)
	case <-time.After(500 * time.Millisecond):
	}
}

// readN reads exactly n bytes of what the guest sent.
func (c *ownedConn) readN(t *testing.T, n int) string {
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
	owner := newSocketOwner(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+owner.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40101)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	got := owner.next(t)
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
	owner := newSocketOwner(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+owner.path)
	m := join(t, n)

	m.send(t, tcpSYN4(t, m, testDst4, 80))
	if got := awaitDial(t, dials); got.address != testDst4+":80" {
		t.Fatalf("dialed %v", got)
	}
	m.send(t, tcpSYN4(t, m, "1.1.1.1", 443))
	if got := awaitDial(t, dials); got.address != "1.1.1.1:443" {
		t.Fatalf("dialed %v", got)
	}
	owner.expectNone(t)
}

// UDP to a diverted destination is dropped, so QUIC cannot go around the
// divert. Other UDP is forwarded as before.
func TestDivertDropsUDPToTheDivertedDestination(t *testing.T) {
	owner := newSocketOwner(t)
	n, dials := divertNetwork(t, nil, nil, testDst4+":443="+owner.path)
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
			owner := newSocketOwner(t)
			n, dials := divertNetwork(t, policy, nil, testDst4+":443="+owner.path)
			m := join(t, n)

			client := newTCPClient(m, testDst4, 443, 40102)
			if !client.connect(t) {
				t.Fatal("the diverted connection was refused")
			}
			if got := owner.next(t); got.preamble != preambleFor(testDst4, 443, 40102) {
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
	owner := newSocketOwner(t)
	res := newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(addr)})
	n, dials := divertNetwork(t, mustPolicy(t, "deny", nil, nil), res, "api.example.com:443="+owner.path)
	m := join(t, n)

	before := newTCPClient(m, addr, 443, 40103)
	if before.connect(t) {
		t.Fatal("the address was reachable before the name was resolved")
	}
	owner.expectNone(t)

	// Deny-default allows no name, and the divert is answered all the same.
	m.send(t, dnsQueryFrame(t, m, "api.example.com", dns.TypeA))
	if got := answerIPs(dnsReply(t, m)); len(got) != 1 || got[0] != addr {
		t.Fatalf("A api.example.com = %v", got)
	}

	after := newTCPClient(m, addr, 443, 40104)
	if !after.connect(t) {
		t.Fatal("the resolved address was not diverted")
	}
	if got := owner.next(t); got.preamble != preambleFor(addr, 443, 40104) {
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
	owner := newSocketOwner(t)
	res := newFakeResolver(map[string][]net.IPAddr{"api.example.com": v4addrs(addr)})
	n, dials := divertNetwork(t, nil, res, "api.example.com:443="+owner.path)
	n.Start(t.Context())
	m := join(t, n)

	client := newTCPClient(m, addr, 443, 40106)
	if !client.connect(t) {
		t.Fatal("the resolved address was not diverted")
	}
	if got := owner.next(t); got.preamble != preambleFor(addr, 443, 40106) {
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
	owner := newSocketOwner(t)
	n, _ := divertNetwork(t, nil, nil, testDst4+":443="+owner.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40108)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	_ = owner.next(t).conn.Close()
	if _, flags := client.receiveUntil(t, header.TCPFlagRst|header.TCPFlagFin); flags&header.TCPFlagRst == 0 {
		t.Fatalf("the guest got flags %v, want a reset", flags)
	}
}

// cutRequest and cutReply are what a guest sends and what the socket owner
// writes before it stops partway through the reply.
const (
	cutRequest = "GET / HTTP/1.1\r\nHost: brig.internal\r\n\r\n"
	cutReply   = "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nthe first part"
)

// ownerSocketEnv names the socket TestDivertOwnerProcess listens on.
const ownerSocketEnv = "HULL_TEST_DIVERT_OWNER_SOCKET"

// TestDivertOwnerProcess is the socket owner that TestDivertCutReplyEndsInAFIN
// kills. It runs in a child of the test binary, and skips anywhere else.
func TestDivertOwnerProcess(t *testing.T) {
	path := os.Getenv(ownerSocketEnv)
	if path == "" {
		t.Skip("run by TestDivertCutReplyEndsInAFIN")
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	fmt.Println("ready")
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatalf("read the preamble: %v", err)
	}
	if _, err := io.ReadFull(r, make([]byte, len(cutRequest))); err != nil {
		t.Fatalf("read the request: %v", err)
	}
	if _, err := io.WriteString(conn, cutReply); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// startOwnerProcess runs TestDivertOwnerProcess on a fresh socket. It returns
// the socket's path once the owner listens, and a function that checks the
// owner was killed.
func startOwnerProcess(t *testing.T) (string, func(t *testing.T)) {
	t.Helper()
	path := filepath.Join(shortDir(t), "p.sock")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDivertOwnerProcess$")
	cmd.Env = append(os.Environ(), ownerSocketEnv+"="+path)
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the owner: %v", err)
	}
	_ = w.Close()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		_ = r.Close()
	})

	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("the owner said %q, want ready", line)
		}
	case <-time.After(replyWait):
		t.Fatal("the owner did not start")
	}

	return path, func(t *testing.T) {
		t.Helper()
		select {
		case <-exited:
		case <-time.After(replyWait):
			t.Fatal("the owner is still running")
		}
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			t.Fatalf("the owner ended with %v, want SIGKILL", waitErr)
		}
		if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the owner ended with %v, want SIGKILL", waitErr)
		}
	}
}

// The gateway cannot tell a reply the owner stopped partway through from a
// finished one, so the guest gets a FIN either way. See relay.
func TestDivertCutReplyEndsInAFIN(t *testing.T) {
	tests := []struct {
		name  string
		owner func(t *testing.T) (path string, cut func(t *testing.T))
	}{
		{"owner closes", func(t *testing.T) (string, func(*testing.T)) {
			owner := newSocketOwner(t)
			return owner.path, func(t *testing.T) {
				got := owner.next(t)
				if read := got.readN(t, len(cutRequest)); read != cutRequest {
					t.Fatalf("the socket read %q, want %q", read, cutRequest)
				}
				if _, err := io.WriteString(got.conn, cutReply); err != nil {
					t.Fatalf("write the reply: %v", err)
				}
				_ = got.conn.Close()
			}
		}},
		{"owner is killed", func(t *testing.T) (string, func(*testing.T)) {
			return startOwnerProcess(t)
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, cut := tc.owner(t)
			n, _ := divertNetwork(t, nil, nil, testDst4+":443="+path)
			m := join(t, n)

			client := newTCPClient(m, testDst4, 443, uint16(40110+i))
			if !client.connect(t) {
				t.Fatal("the diverted connection was refused")
			}
			client.m.send(t, client.segment(t, header.TCPFlagPsh|header.TCPFlagAck, []byte(cutRequest)))
			client.seq += uint32(len(cutRequest))
			cut(t)

			data, flags := client.receiveUntil(t, header.TCPFlagRst|header.TCPFlagFin)
			if flags&header.TCPFlagRst != 0 {
				t.Fatalf("the guest got a reset, want a FIN")
			}
			if data != cutReply {
				t.Fatalf("the guest received %q, want %q", data, cutReply)
			}
		})
	}
}

// waitUnread waits until conn has at least n bytes queued that nobody has
// read.
func waitUnread(t *testing.T, conn *net.UnixConn, n int) {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	buf := make([]byte, n)
	for deadline := time.Now().Add(replyWait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		queued := 0
		_ = raw.Control(func(fd uintptr) {
			queued, _, _ = syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK)
		})
		if queued >= n {
			return
		}
	}
	t.Fatalf("%d bytes never reached the socket", n)
}

// Linux reports ECONNRESET to the gateway when the owner closes with some of
// the guest's bytes unread, and the guest gets a reset. macOS reports a clean
// EOF, and the guest gets a FIN.
func TestDivertOwnerClosingWithUnreadBytes(t *testing.T) {
	path := filepath.Join(shortDir(t), "u.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan *net.UnixConn, 1)
	go func() {
		if conn, err := ln.AcceptUnix(); err == nil {
			conns <- conn
		}
	}()
	n, _ := divertNetwork(t, nil, nil, testDst4+":443="+path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40112)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	var conn *net.UnixConn
	select {
	case conn = <-conns:
	case <-time.After(replyWait):
		t.Fatal("nothing arrived on the divert socket")
	}
	client.m.send(t, client.segment(t, header.TCPFlagPsh|header.TCPFlagAck, []byte(cutRequest)))
	client.seq += uint32(len(cutRequest))
	waitUnread(t, conn, len(preambleFor(testDst4, 443, 40112))+len(cutRequest))
	if _, err := io.WriteString(conn, cutReply); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	_ = conn.Close()

	data, flags := client.receiveUntil(t, header.TCPFlagRst|header.TCPFlagFin)
	if runtime.GOOS == "linux" {
		if flags&header.TCPFlagRst == 0 {
			t.Fatalf("the guest got flags %v, want a reset", flags)
		}
		return
	}
	if flags&header.TCPFlagRst != 0 {
		t.Fatalf("the guest got a reset, want a FIN")
	}
	if data != cutReply {
		t.Fatalf("the guest received %q, want %q", data, cutReply)
	}
}

// Each side's EOF reaches the other as a half-close, and the other direction
// keeps working after it.
func TestDivertPropagatesHalfClose(t *testing.T) {
	owner := newSocketOwner(t)
	n, _ := divertNetwork(t, nil, nil, testDst4+":443="+owner.path)
	m := join(t, n)

	client := newTCPClient(m, testDst4, 443, 40109)
	if !client.connect(t) {
		t.Fatal("the diverted connection was refused")
	}
	got := owner.next(t)

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
			owner := newSocketOwner(t)
			parsed, err := ParseDiverts([]string{testGatewayIP + ":443=" + owner.path})
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
			if got := owner.next(t); got.preamble != preambleFor(testGatewayIP, 443, 40301) {
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
			owner.expectNone(t)
		})
	}
}

// A divert of a link-local address is served, so a guest can reach a
// credential endpoint at the address its SDK accepts plain HTTP from. Every
// other link-local destination, the metadata service included, is still
// reset and never dialed.
func TestDivertServesALinkLocalDestination(t *testing.T) {
	owner := newSocketOwner(t)
	n, dials := divertNetwork(t, nil, nil, "169.254.170.2:80="+owner.path)
	m := join(t, n)

	client := newTCPClient(m, "169.254.170.2", 80, 40111)
	if !client.connect(t) {
		t.Fatal("the diverted link-local connection was refused")
	}
	if got := owner.next(t); got.preamble != preambleFor("169.254.170.2", 80, 40111) {
		t.Fatalf("preamble %q", got.preamble)
	}

	for _, dst := range []struct {
		ip   string
		port uint16
	}{{"169.254.169.254", 80}, {"169.254.170.2", 443}} {
		m.send(t, tcpSYN4(t, m, dst.ip, dst.port))
	}
	expectNoDial(t, dials)
	owner.expectNone(t)
}
