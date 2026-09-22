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
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
)

// A forward is a listener on the host that carries connections to one guest
// address. The gateway is the only process that can open a connection into
// the virtual network, so every published port is one of these.
//
// They were fixed at startup, from --forward. A caller that wanted one after
// boot had to restart the gateway, which drops every member of the network
// with it. The set is mutable here so a running gateway can be given one and
// have it taken away again.

// ErrForwardExists is returned when a forward is already listening on the
// local address asked for.
var ErrForwardExists = errors.New("a forward already listens there")

// ErrForwardNotFound is returned when no forward holds the local address
// asked for.
var ErrForwardNotFound = errors.New("no forward listens there")

// ErrForwardInvalid is returned for a forward the gateway cannot install at
// all: an unknown protocol, or an address it cannot read.
var ErrForwardInvalid = errors.New("invalid forward")

// Forward is one host listener and the guest address behind it.
type Forward struct {
	// Protocol is tcp or udp.
	Protocol string `json:"protocol"`
	// Local is the host address the gateway listens on, as host:port. An
	// address of 127.0.0.1 keeps the port on this machine; 0.0.0.0 offers it
	// to the network the host is on.
	Local string `json:"local"`
	// Remote is the guest address connections are carried to, as ip:port.
	Remote string `json:"remote"`
}

// key is what a forward occupies: one protocol on one local address. Two
// forwards cannot share that, whatever guest they point at.
func (f Forward) key() string { return f.Protocol + "/" + f.Local }

// String renders a forward the way the --forward flag spells one.
func (f Forward) String() string {
	if f.Protocol == "udp" {
		return "udp:" + f.Local + "=" + f.Remote
	}
	return f.Local + "=" + f.Remote
}

// validate rejects a forward the gateway cannot install, and fills in the
// defaults a caller may leave out.
//
// The protocol and the local address are parseProtocol's and ParseLocal's
// rules. The remote is checked here, because only a forward has one: it must
// carry a port, and it must be IPv4.
func (f *Forward) validate() error {
	protocol, err := parseProtocol(f.Protocol)
	if err != nil {
		return err
	}
	f.Protocol = protocol
	local, err := ParseLocal(f.Local)
	if err != nil {
		return err
	}
	f.Local = local
	host, port, err := net.SplitHostPort(f.Remote)
	if err != nil {
		return fmt.Errorf("%w: remote address %q is not host:port: %w",
			ErrForwardInvalid, f.Remote, err)
	}
	if host == "" {
		return fmt.Errorf("%w: remote address %q names no host", ErrForwardInvalid, f.Remote)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%w: remote address %q does not carry a port between 1 and 65535",
			ErrForwardInvalid, f.Remote)
	}
	// IPv4, and not merely an address. The forwarder takes the remote apart on
	// its colons and calls To4 on what it finds, so an IPv6 guest reaches it as
	// a nil address and comes back as the gateway's own failure. The guest
	// network is IPv4-only and the gateway forwards no IPv6, so this is a
	// refusal the caller can act on rather than a 500 they cannot.
	if ip := net.ParseIP(hostOf(f.Remote)); ip == nil || ip.To4() == nil {
		return fmt.Errorf("%w: remote address %q must name a guest by IPv4 address",
			ErrForwardInvalid, f.Remote)
	}
	return nil
}

// ParseLocal reads a forward's local address and returns it as the set
// records one: the host as net.IP renders it, and the port unchanged.
//
// Exported because a caller computing forwards ahead of the gateway reads
// them by the same grammar, and one rule is what keeps the two answers the
// same.
//
// An empty host is 0.0.0.0, which is how net.Listen reads it. The host must
// be an address and not a name: the record is what a later lookup is matched
// against, and a name is not what the listener binds, so a forward published
// as localhost:80 could not be withdrawn as 127.0.0.1:80.
//
// # Errors
//
// Returns ErrForwardInvalid for an address that is not host:port, whose host
// is not an IP address, or whose port is outside 1-65535.
func ParseLocal(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%w: local address %q is not host:port: %w",
			ErrForwardInvalid, addr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("%w: local address %q must name the host by address, not by name",
			ErrForwardInvalid, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%w: local address %q does not carry a port between 1 and 65535",
			ErrForwardInvalid, addr)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// parseProtocol reads a forward's protocol, defaulting an empty one to tcp.
//
// # Errors
//
// Returns ErrForwardInvalid for a protocol this gateway does not forward.
func parseProtocol(p string) (string, error) {
	if p == "" {
		return "tcp", nil
	}
	p = strings.ToLower(p)
	if p != "tcp" && p != "udp" {
		return "", fmt.Errorf("%w: protocol %q is not one this gateway forwards, use tcp or udp",
			ErrForwardInvalid, p)
	}
	return p, nil
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

// ParseTarget reads the protocol and local address a withdrawal names, and
// returns them as the set records them.
//
// The same grammar a publication is read by, so an argument this gateway
// cannot read is invalid on both methods. Reported as an absent forward it
// would read as "that one is already gone", and a caller retries a typo.
//
// # Errors
//
// Returns ErrForwardInvalid for a protocol this gateway does not forward, or
// a local address ParseLocal refuses.
func ParseTarget(protocol, local string) (string, string, error) {
	p, err := parseProtocol(protocol)
	if err != nil {
		return "", "", err
	}
	l, err := ParseLocal(local)
	if err != nil {
		return "", "", err
	}
	return p, l, nil
}

// ParseForward reads one --forward value, hostaddr:port=guestip:port, with an
// optional udp: prefix on the local address.
func ParseForward(s string) (Forward, error) {
	local, remote, ok := strings.Cut(s, "=")
	if !ok {
		return Forward{}, fmt.Errorf("%w %q, expected hostaddr:port=guestip:port", ErrForwardInvalid, s)
	}
	f := Forward{Protocol: "tcp", Local: local, Remote: remote}
	if rest, cut := strings.CutPrefix(local, "udp:"); cut {
		f.Protocol, f.Local = "udp", rest
	}
	if err := f.validate(); err != nil {
		return Forward{}, fmt.Errorf("%q: %w", s, err)
	}
	return f, nil
}

// forwards is the set of host listeners the gateway is running, and the
// gvisor forwarder that owns them.
//
// The set is kept here as well as in the forwarder because the forwarder does
// not read its own back: upstream keeps the map unexported and serves it only
// through an HTTP handler of its own. Listing is what a caller asks for first,
// so the record lives beside the thing it describes.
type forwards struct {
	fw *forwarder.PortsForwarder

	mu  sync.Mutex
	set map[string]Forward
}

func newForwards(fw *forwarder.PortsForwarder) *forwards {
	return &forwards{fw: fw, set: map[string]Forward{}}
}

// expose installs one forward.
//
// The record is written only once the listener is up. A record written first
// would name a port nothing is listening on, and the caller reading the list
// back would be told the publication succeeded.
func (s *forwards) expose(f Forward) (Forward, error) {
	if err := f.validate(); err != nil {
		return Forward{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if have, ok := s.set[f.key()]; ok {
		return Forward{}, fmt.Errorf("%w: %s/%s carries %s",
			ErrForwardExists, have.Protocol, have.Local, have.Remote)
	}
	// Whether two addresses can share a port is the host kernel's answer, not
	// one to reimplement: darwin binds 0.0.0.0:80 beside 127.0.0.1:80 for TCP
	// and refuses the pair for UDP, and Linux refuses both. So the bind
	// decides, and the refusal it gives for a port already held is the same
	// answer the set gives for one this gateway holds.
	if err := s.fw.Expose(protocolOf(f.Protocol), f.Local, f.Remote); err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			// Which holder, read from the set rather than assumed. The keys
			// differ here by definition -- an identical address was refused
			// above -- so the port can still be one of ours on another
			// address, as udp/0.0.0.0:80 is for udp/127.0.0.1:80.
			if have, ok := s.holderOfPort(f); ok {
				return Forward{}, fmt.Errorf("%w: %s cannot be bound beside %s/%s, which carries %s",
					ErrForwardExists, f.Local, have.Protocol, have.Local, have.Remote)
			}
			return Forward{}, fmt.Errorf("%w: %s is held outside this gateway: %w",
				ErrForwardExists, f.Local, err)
		}
		return Forward{}, fmt.Errorf("cannot listen on %s: %w", f.Local, err)
	}
	s.set[f.key()] = f
	return f, nil
}

// holderOfPort returns a forward this gateway serves on f's protocol and
// port at another address, and whether there is one.
//
// Read only to say who holds a port the bind refused. It rules on nothing:
// whether two addresses can share a port is the kernel's answer, and this
// runs after the kernel has already given it.
//
// The caller holds the lock.
func (s *forwards) holderOfPort(f Forward) (Forward, bool) {
	_, port, err := net.SplitHostPort(f.Local)
	if err != nil {
		return Forward{}, false
	}
	for _, have := range s.set {
		if have.Protocol != f.Protocol {
			continue
		}
		if _, p, err := net.SplitHostPort(have.Local); err == nil && p == port {
			return have, true
		}
	}
	return Forward{}, false
}

// unexpose removes the forward on this protocol and local address, and
// reports the one that went.
func (s *forwards) unexpose(protocol, local string) (Forward, error) {
	// The same grammar expose reads, so a protocol or an address this gateway
	// cannot read is the invalid it is on the other method rather than an
	// absent forward, which a caller would retry.
	p, l, err := ParseTarget(protocol, local)
	if err != nil {
		return Forward{}, err
	}
	f := Forward{Protocol: p, Local: l}
	s.mu.Lock()
	defer s.mu.Unlock()
	have, ok := s.set[f.key()]
	if !ok {
		return Forward{}, fmt.Errorf("%w: %s/%s", ErrForwardNotFound, f.Protocol, f.Local)
	}
	// Dropped whatever upstream returned. It removes its own record before it
	// closes the listener, so keeping ours on an error would leave a forward
	// listed that nothing serves, refused as published, and undeletable.
	err = s.fw.Unexpose(protocolOf(have.Protocol), have.Local)
	delete(s.set, have.key())
	if err != nil {
		return Forward{}, fmt.Errorf("cannot stop listening on %s: %w", have.Local, err)
	}
	return have, nil
}

// list returns every forward installed, ordered by local address so two reads
// of an unchanged set render the same.
func (s *forwards) list() []Forward {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Forward, 0, len(s.set))
	for _, f := range s.set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local == out[j].Local {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].Local < out[j].Local
	})
	return out
}

func protocolOf(p string) gvntypes.TransportProtocol {
	if p == "udp" {
		return gvntypes.UDP
	}
	return gvntypes.TCP
}

// Expose installs a forward on a running gateway and returns it as installed,
// with the defaults validate filled in.
//
// The installed forward rather than the one asked for. A caller that left the
// protocol out would otherwise be answered with the blank it sent, while a
// later read of the same forward says "tcp" -- two answers about one thing.
func (n *Network) Expose(f Forward) (Forward, error) { return n.forwards.expose(f) }

// Unexpose stops the forward on this protocol and local address, and returns
// the one that went.
func (n *Network) Unexpose(protocol, local string) (Forward, error) {
	return n.forwards.unexpose(protocol, local)
}

// Forwards returns every forward this gateway is running.
func (n *Network) Forwards() []Forward { return n.forwards.list() }
