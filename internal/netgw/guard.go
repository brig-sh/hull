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
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The guard sits between a member's socket and the switch, and drops any frame
// that does not carry that member's own addresses.
//
// Two addresses have to be pinned, not one. The source address in the IP header
// is what the egress policy reads, so a guest that writes another member's
// address there inherits that member's rules. The source address in the
// Ethernet header is what the switch learns its forwarding table from, so a
// guest that writes another member's MAC takes over its port and receives its
// traffic. Pinning either one alone leaves the other open.
//
// This is where the gateway earns the right to say "this connection came from
// that guest" at all. Without it, every per-member rule is a suggestion.

// isolation is what a guard needs to keep a member away from its peers: the
// gateway's own addresses, which are the only ones it may exchange frames
// with. The zero value leaves the member on the shared switch as before.
type isolation struct {
	on         bool
	gatewayMAC []byte
	gatewayIP  tcpip.Address
}

// maxFrameSize bounds one frame read from a stream member. It is well above
// the MTU the gateway hands out and exists so a corrupt length prefix cannot
// ask for an unbounded allocation.
const maxFrameSize = 128 * 1024

// dhcpServerPort is the one port a guest may reach before it has an address.
const dhcpServerPort = 67

// guard is a net.Conn that yields only the frames a member is entitled to
// send. It wraps the member's socket and is handed to the switch in its place.
type guard struct {
	conn   net.Conn
	member Member
	// mac is the hardware address this member is pinned to. It is the
	// declared one when the caller gave it, and otherwise the first one seen.
	mac tcpip.LinkAddress
	// macBytes is mac in wire form, compared against directly so the check
	// allocates nothing per frame.
	macBytes []byte
	addr     tcpip.Address
	hasAddr  bool

	// isolate stops this member exchanging frames with any other member, so
	// the only thing it can talk to is the gateway.
	isolate    bool
	gatewayMAC []byte
	gatewayIP  tcpip.Address

	dropped atomic.Uint64
	lastLog atomic.Int64

	// Stream members carry length-prefixed frames, so the guard has to
	// reassemble a frame to inspect it and re-emit the ones it passes.
	stream  bool
	reader  *bufio.Reader
	sizeBuf []byte
	frame   []byte
	out     []byte
	pending []byte
}

func newGuard(conn net.Conn, m Member, stream bool, iso isolation) *guard {
	g := &guard{
		conn: conn, member: m, mac: m.MAC, macBytes: []byte(m.MAC), stream: stream,
		isolate: iso.on, gatewayMAC: iso.gatewayMAC, gatewayIP: iso.gatewayIP,
	}
	if m.IP.IsValid() && m.IP.Is4() {
		g.addr = tcpip.AddrFrom4(m.IP.As4())
		g.hasAddr = true
	}
	if stream {
		g.reader = bufio.NewReaderSize(conn, maxFrameSize)
		g.sizeBuf = make([]byte, 4)
		g.frame = make([]byte, 0, maxFrameSize)
		g.out = make([]byte, 0, maxFrameSize+4)
	}
	return g
}

func (g *guard) Read(p []byte) (int, error) {
	if g.stream {
		return g.readStream(p)
	}
	// A datagram member gives one frame per read, so a rejected frame is
	// simply not returned and the next one is read in its place.
	for {
		n, err := g.conn.Read(p)
		if err != nil {
			return n, err
		}
		if g.allow(p[:n]) {
			return n, nil
		}
		g.reject(p[:n])
	}
}

func (g *guard) readStream(p []byte) (int, error) {
	for {
		if len(g.pending) > 0 {
			n := copy(p, g.pending)
			g.pending = g.pending[n:]
			return n, nil
		}
		if _, err := io.ReadFull(g.reader, g.sizeBuf); err != nil {
			return 0, err
		}
		size := binary.BigEndian.Uint32(g.sizeBuf)
		if size > maxFrameSize {
			return 0, io.ErrUnexpectedEOF
		}
		g.frame = g.frame[:0]
		if cap(g.frame) < int(size) {
			g.frame = make([]byte, size)
		}
		g.frame = g.frame[:size]
		if _, err := io.ReadFull(g.reader, g.frame); err != nil {
			return 0, err
		}
		if !g.allow(g.frame) {
			g.reject(g.frame)
			continue
		}
		// Hand the frame on with its prefix intact: the switch reads the
		// same framing the member wrote.
		g.out = append(g.out[:0], g.sizeBuf...)
		g.out = append(g.out, g.frame...)
		g.pending = g.out
	}
}

// Write delivers a frame to the member. An isolated member hears only the
// gateway: the switch floods a broadcast to every member, so without this an
// isolated member would still see its neighbours' DHCP and ARP and learn that
// they exist. Frames the gateway itself sends carry its address, which is what
// separates them.
func (g *guard) Write(p []byte) (int, error) {
	if g.isolate && len(p) >= header.EthernetMinimumSize {
		if !bytes.Equal(g.frameSource(p), g.gatewayMAC) {
			// Report the frame as written. It was not delivered, which is the
			// point, and a short write would tear the member's connection
			// down instead.
			return len(p), nil
		}
	}
	return g.conn.Write(p)
}

// frameSource is the source address of a frame, allowing for the length
// prefix a stream member's frames carry.
func (g *guard) frameSource(p []byte) []byte {
	if g.stream {
		if len(p) < 4+header.EthernetMinimumSize {
			return nil
		}
		return p[4+6 : 4+12]
	}
	return p[6:12]
}
func (g *guard) Close() error                       { return g.conn.Close() }
func (g *guard) LocalAddr() net.Addr                { return g.conn.LocalAddr() }
func (g *guard) RemoteAddr() net.Addr               { return g.conn.RemoteAddr() }
func (g *guard) SetDeadline(t time.Time) error      { return g.conn.SetDeadline(t) }
func (g *guard) SetReadDeadline(t time.Time) error  { return g.conn.SetReadDeadline(t) }
func (g *guard) SetWriteDeadline(t time.Time) error { return g.conn.SetWriteDeadline(t) }

// allow reports whether this frame carries the member's own addresses.
func (g *guard) allow(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize {
		return false
	}
	// Compare the source MAC as bytes. header.Ethernet.SourceAddress converts
	// it to a string, which allocates, and this runs on every frame a member
	// sends.
	src := frame[6:12]
	if len(g.macBytes) == 0 {
		// The caller named no hardware address, so the first one this member
		// uses becomes the one it is held to.
		g.macBytes = append(g.macBytes[:0], src...)
		g.mac = tcpip.LinkAddress(g.macBytes)
	}
	if !bytes.Equal(src, g.macBytes) {
		return false
	}

	if g.isolate && !g.allowIsolatedDestination(frame) {
		return false
	}

	payload := frame[header.EthernetMinimumSize:]
	switch header.Ethernet(frame).Type() {
	case header.IPv4ProtocolNumber:
		return g.allowIPv4(payload)
	case header.ARPProtocolNumber:
		return g.allowARP(payload, src)
	default:
		// Other families are pinned by the hardware address above. The
		// netstack registers no protocol for them and discards them, and the
		// switch only ever forwards them to other members.
		return true
	}
}

// allowIsolatedDestination decides whether an isolated member may send this
// frame at all, by where it is addressed.
//
// The gateway is the only thing an isolated member talks to. A frame to the
// gateway's own address is therefore fine, and a frame to another member's is
// not. Broadcast is the case that needs care: a guest legitimately broadcasts
// to find the gateway and to ask for an address, and the switch floods a
// broadcast to every member, so allowing it wholesale would hand an isolated
// member both a way to reach its neighbours and a way to enumerate them.
// Only the two broadcasts a guest cannot start without are allowed, and only
// when they are directed at the gateway.
func (g *guard) allowIsolatedDestination(frame []byte) bool {
	dst := frame[0:6]
	if bytes.Equal(dst, g.gatewayMAC) {
		return true
	}
	if !bytes.Equal(dst, []byte(header.EthernetBroadcastAddress)) {
		// Unicast to somebody who is not the gateway, or multicast. Either
		// way it is not addressed to the one peer this member may talk to.
		return false
	}

	payload := frame[header.EthernetMinimumSize:]
	switch header.Ethernet(frame).Type() {
	case header.ARPProtocolNumber:
		// An ARP that asks for the gateway is how a guest finds its next hop.
		// One that asks for anything else is how it finds its neighbours.
		if len(payload) < header.ARPSize {
			return false
		}
		arp := header.ARP(payload)
		return arp.IsValid() && tcpip.AddrFromSlice(arp.ProtocolAddressTarget()) == g.gatewayIP
	case header.IPv4ProtocolNumber:
		// A DHCP request has to be broadcast: the guest has no address and
		// does not know the server's.
		if len(payload) < header.IPv4MinimumSize {
			return false
		}
		ip := header.IPv4(payload)
		return ip.IsValid(len(payload)) && isDHCPRequest(ip)
	default:
		return false
	}
}

func (g *guard) allowIPv4(payload []byte) bool {
	if len(payload) < header.IPv4MinimumSize {
		return false
	}
	ip := header.IPv4(payload)
	if !ip.IsValid(len(payload)) {
		return false
	}
	src := ip.SourceAddress()
	if g.hasAddr && src == g.addr {
		return true
	}
	// A guest with no address yet has one thing it can legitimately say, and
	// that is a DHCP request. Anything else from an address the member does
	// not hold is another member's traffic, or nobody's.
	if src == header.IPv4Any {
		return isDHCPRequest(ip)
	}
	return false
}

// isDHCPRequest reports whether an address-less packet is a DHCP client
// message, which is the only traffic a guest may send before it has an address.
func isDHCPRequest(ip header.IPv4) bool {
	if ip.TransportProtocol() != header.UDPProtocolNumber {
		return false
	}
	if ip.FragmentOffset() != 0 || ip.Flags()&header.IPv4FlagMoreFragments != 0 {
		return false
	}
	payload := ip.Payload()
	if len(payload) < header.UDPMinimumSize {
		return false
	}
	return header.UDP(payload).DestinationPort() == dhcpServerPort
}

func (g *guard) allowARP(payload []byte, ethSrc []byte) bool {
	if len(payload) < header.ARPSize {
		return false
	}
	arp := header.ARP(payload)
	if !arp.IsValid() {
		return false
	}
	// An ARP body carries its own idea of who is speaking. Both halves have
	// to agree with the member, or a guest answers for an address it does not
	// hold and the other guests believe it.
	if !bytes.Equal(arp.HardwareAddressSender(), ethSrc) {
		return false
	}
	sender := tcpip.AddrFromSlice(arp.ProtocolAddressSender())
	if g.hasAddr && sender == g.addr {
		return true
	}
	// A probe from 0.0.0.0 asks whether an address is taken and claims
	// nothing, so it is safe before the member has one.
	return sender == header.IPv4Any
}

// reject counts a dropped frame and reports it, at most once a minute per
// member. A guest that is spoofing does it continuously.
func (g *guard) reject(frame []byte) {
	n := g.dropped.Add(1)
	now := time.Now().UnixNano()
	last := g.lastLog.Load()
	if now-last < int64(time.Minute) || !g.lastLog.CompareAndSwap(last, now) {
		return
	}
	log.Warnf("gateway: dropped a frame from %s carrying an address it does not hold (%d so far)", g.member, n)
	_ = frame
}
