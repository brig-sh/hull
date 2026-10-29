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
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// A divert hands guest connections to a local socket instead of dialing them.
//
// A divert names a host, a port and a Unix socket. A guest TCP connection to
// one of the host's addresses on that port is never dialed. The gateway
// connects to the socket, writes one preamble line that names the connection,
// and relays the guest's bytes both ways unchanged. brig uses this to hand a
// sandbox's connections to brig.internal to its credential broker.
//
// The addresses of a named host are every address the gateway has resolved
// that name to: once at start, on the egress refresh timer, and on each guest
// query for it. The set only grows. A connection that was not meant for the
// host is the socket owner's to handle. Dialing one that was meant for it
// would bypass the socket, so an address is never dropped from the set.
//
// A diverted connection is not checked against the egress policy. The socket
// owner applies its own binding to what it receives, and the connection does
// not leave the host through the gateway.

// divertPreamble is the first line written to the socket, in the form
// "BRIG-DIVERT/1 <dst-ip> <dst-port> <src-ip> <src-port>\n".
const divertPreamble = "BRIG-DIVERT/1"

// divertDialTimeout bounds the connect to the socket. The socket is local, so
// a connect that takes longer means its owner is wedged.
const divertDialTimeout = 2 * time.Second

// divertResolveTimeout bounds the resolution of the named hosts at start.
const divertResolveTimeout = 5 * time.Second

// Divert hands guest TCP connections to Host:Port to the Unix socket at
// Socket.
type Divert struct {
	// Host is a DNS name, lower case with no trailing dot, or an IPv4
	// literal.
	Host string `json:"host"`
	// Port is the destination port, 1-65535.
	Port uint16 `json:"port"`
	// Socket is the absolute path of the Unix stream socket.
	Socket string `json:"socket"`
}

func (d Divert) target() string { return net.JoinHostPort(d.Host, strconv.Itoa(int(d.Port))) }

func (d Divert) String() string { return d.target() + "=" + d.Socket }

// DivertStatus is one divert with what it has done since the gateway started.
type DivertStatus struct {
	Divert
	// Addresses is the number of addresses connections are diverted from.
	Addresses int `json:"addresses"`
	// Diverted counts the guest connections handed to the socket.
	Diverted uint64 `json:"diverted"`
	// Failed counts the guest connections reset because the socket could
	// not be connected.
	Failed uint64 `json:"failed"`
}

// ParseDiverts reads --divert values of the form host:port=socket. It refuses
// the whole list on one bad value, or on a host:port given twice.
func ParseDiverts(values []string) ([]Divert, error) {
	var out []Divert
	seen := map[string]bool{}
	for _, v := range values {
		d, err := parseDivert(v)
		if err != nil {
			return nil, err
		}
		if seen[d.target()] {
			return nil, fmt.Errorf("invalid --divert %q: %s is diverted twice", v, d.target())
		}
		seen[d.target()] = true
		out = append(out, d)
	}
	return out, nil
}

func parseDivert(v string) (Divert, error) {
	target, socket, ok := strings.Cut(v, "=")
	if !ok {
		return Divert{}, fmt.Errorf("invalid --divert %q, expected host:port=socket", v)
	}
	host, portString, err := net.SplitHostPort(target)
	if err != nil {
		return Divert{}, fmt.Errorf("invalid --divert %q, expected host:port=socket: %v", v, err)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return Divert{}, fmt.Errorf("invalid --divert %q: port %q is not in 1-65535", v, portString)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return Divert{}, fmt.Errorf("invalid --divert %q: an address with a zone cannot be matched", v)
		}
		addr = addr.Unmap()
		// The stack has no IPv6, so a divert of one would start and then
		// never match.
		if !addr.Is4() {
			return Divert{}, fmt.Errorf("invalid --divert %q: the gateway does not forward IPv6, so %s cannot be matched", v, addr)
		}
		host = addr.String()
	} else {
		host = normalizeName(host)
		if !validHostName(host) {
			return Divert{}, fmt.Errorf("invalid --divert %q: %q is neither a host name nor an IP address", v, host)
		}
	}
	if !filepath.IsAbs(socket) {
		return Divert{}, fmt.Errorf("invalid --divert %q: the socket path must be absolute", v)
	}
	return Divert{Host: host, Port: uint16(port), Socket: filepath.Clean(socket)}, nil
}

// validHostName reports whether name is a plain DNS name. A glob is refused:
// there is no way to resolve one ahead of a query.
func validHostName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// divertRule is one divert and the addresses it applies to.
type divertRule struct {
	Divert
	named bool

	mu    sync.Mutex
	addrs map[netip.Addr]struct{}

	diverted atomic.Uint64
	failed   atomic.Uint64
}

// diverts is the gateway's set of divert rules. A nil set diverts nothing.
type diverts struct {
	rules []*divertRule
}

func newDiverts(list []Divert) *diverts {
	if len(list) == 0 {
		return nil
	}
	d := &diverts{}
	for _, div := range list {
		rule := &divertRule{Divert: div, addrs: map[netip.Addr]struct{}{}}
		if addr, err := netip.ParseAddr(div.Host); err == nil {
			rule.addrs[addr.Unmap()] = struct{}{}
		} else {
			rule.named = true
		}
		d.rules = append(d.rules, rule)
	}
	return d
}

// match returns the rule a connection to dst:port is diverted by, or nil. When
// two named hosts share an address and a port, the first rule given wins.
func (d *diverts) match(dst netip.Addr, port uint16) *divertRule {
	if d == nil {
		return nil
	}
	dst = dst.Unmap()
	for _, rule := range d.rules {
		if rule.Port != port {
			continue
		}
		rule.mu.Lock()
		_, ok := rule.addrs[dst]
		rule.mu.Unlock()
		if ok {
			return rule
		}
	}
	return nil
}

// coversName reports whether name is the host of a named divert.
func (d *diverts) coversName(name string) bool {
	if d == nil {
		return false
	}
	name = normalizeName(name)
	for _, rule := range d.rules {
		if rule.named && rule.Host == name {
			return true
		}
	}
	return false
}

// learn adds addrs to every named divert whose host is name.
//
// A link-local address is never learned. The forwarder matches a divert
// before it resets link-local, and a name's addresses come from DNS and are
// kept, so a name that once resolved to the metadata service would hand
// every connection to it to the socket owner. Only a literal names a
// link-local address on purpose.
func (d *diverts) learn(name string, addrs []netip.Addr) {
	if d == nil || len(addrs) == 0 {
		return
	}
	name = normalizeName(name)
	ll := netip.MustParsePrefix(linkLocalSubnet)
	for _, rule := range d.rules {
		if !rule.named || rule.Host != name {
			continue
		}
		rule.mu.Lock()
		for _, addr := range addrs {
			if addr = addr.Unmap(); ll.Contains(addr) {
				continue
			}
			rule.addrs[addr] = struct{}{}
		}
		rule.mu.Unlock()
	}
}

// resolve resolves every named divert once. A name that fails keeps the
// addresses it has.
func (d *diverts) resolve(ctx context.Context, res resolver) {
	if d == nil {
		return
	}
	for _, rule := range d.rules {
		if !rule.named {
			continue
		}
		ips, err := res.LookupIPAddr(ctx, rule.Host)
		if err != nil {
			log.Warnf("divert: cannot resolve %s, keeping its current addresses: %v", rule.Host, err)
			continue
		}
		addrs := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			if addr, ok := netip.AddrFromSlice(ip.IP); ok {
				addrs = append(addrs, addr)
			}
		}
		d.learn(rule.Host, addrs)
	}
}

// watch resolves the named diverts every interval until ctx is done.
func (d *diverts) watch(ctx context.Context, res resolver, interval time.Duration) {
	if d == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.resolve(ctx, res)
		}
	}
}

func (d *diverts) status() []DivertStatus {
	if d == nil {
		return []DivertStatus{}
	}
	out := make([]DivertStatus, 0, len(d.rules))
	for _, rule := range d.rules {
		rule.mu.Lock()
		n := len(rule.addrs)
		rule.mu.Unlock()
		out = append(out, DivertStatus{
			Divert:    rule.Divert,
			Addresses: n,
			Diverted:  rule.diverted.Load(),
			Failed:    rule.failed.Load(),
		})
	}
	return out
}

// serve hands the guest connection behind r to the rule's socket. A socket
// that cannot be connected resets the guest connection.
func (rule *divertRule) serve(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst, src := toNetip(id.LocalAddress), toNetip(id.RemoteAddress)
	conn, err := dialDivert(rule.Socket, dst, id.LocalPort, src, id.RemotePort)
	if err != nil {
		rule.failed.Add(1)
		log.Warnf("divert: reset tcp from %s to %s: %v", src,
			net.JoinHostPort(dst.String(), strconv.Itoa(int(id.LocalPort))), err)
		r.Complete(true)
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if tcpErr != nil {
		_ = conn.Close()
		log.Debugf("divert: r.CreateEndpoint() = %v", tcpErr)
		return
	}
	rule.diverted.Add(1)
	relay(gonet.NewTCPConn(&wq, ep), ep, conn)
}

// dialDivert connects to the socket and writes the preamble.
func dialDivert(socket string, dst netip.Addr, dstPort uint16, src netip.Addr, srcPort uint16) (*net.UnixConn, error) {
	c, err := net.DialTimeout("unix", socket, divertDialTimeout)
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UnixConn)
	line := fmt.Sprintf("%s %s %d %s %d\n", divertPreamble, dst.WithZone(""), dstPort, src.WithZone(""), srcPort)
	_ = conn.SetWriteDeadline(time.Now().Add(divertDialTimeout))
	if _, err := io.WriteString(conn, line); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write the preamble to %s: %w", socket, err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return conn, nil
}

// relay relays bytes between the guest and the socket until both directions
// end. EOF on one side is passed on as a half-close of the other.
//
// The socket owner refuses a connection by closing it without writing. The
// guest then gets a reset, so a refusal does not look like an empty reply. A
// read error from the socket resets the guest too. On Linux, an owner that
// closes with some of the guest's bytes unread causes one.
//
// A reply cut partway through reaches the guest as a complete one, with a
// FIN. On a Unix stream socket, an owner that closes, exits or is killed gives
// the same EOF. Its pid does not tell them apart either: kill(pid, 0) still
// succeeds at that EOF, until the owner's parent reaps it. A protocol that
// must catch a cut reply has to frame its replies.
func relay(guest *gonet.TCPConn, ep tcpip.Endpoint, sock *net.UnixConn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(sock, guest)
		_ = sock.CloseWrite()
	}()

	n, err := io.Copy(guest, sock)
	if n == 0 || err != nil {
		ep.Abort()
	} else {
		_ = guest.CloseWrite()
	}
	<-done
	_ = guest.Close()
	_ = sock.Close()
}
