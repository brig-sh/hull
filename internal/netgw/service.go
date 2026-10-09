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
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// A service is a virtual address and port with a set of endpoints behind it,
// the way a Kubernetes ClusterIP Service is. A guest connects to the virtual
// address, the packet leaves the switch for the gateway because the address
// is off the subnet, and the forwarder opens a connection to one of the
// endpoints instead of dialing the virtual address from the host.
//
// There is no NAT here. The gateway terminates the guest's connection and
// opens a second one, so the endpoint sees the gateway's address as the
// source, never the client guest's.
//
// The table is replaced whole. The caller computes it from its own source of
// truth and sends all of it, so the gateway keeps no history and a caller
// that restarts loses nothing by sending it again.

// ErrServiceInvalid is returned for a service table the gateway cannot
// install: a virtual address outside the service range, an unknown protocol,
// an endpoint it cannot reach, or one service named twice.
var ErrServiceInvalid = errors.New("invalid service")

// defaultServiceDialTimeout bounds one attempt to reach a guest endpoint. A guest
// that is gone does not answer ARP, and the stack gives up on resolving it
// after about this long, so a longer bound would only hold the forwarder's
// in-flight slot for nothing.
const defaultServiceDialTimeout = 3 * time.Second

// ServiceEndpoint is one backend of a service.
type ServiceEndpoint struct {
	// IP is the endpoint's address. A guest endpoint must be on the
	// gateway's subnet.
	IP netip.Addr `json:"ip"`
	// Port is the endpoint's port.
	Port uint16 `json:"port"`
	// Host marks an endpoint on the host rather than in the virtual network.
	// It is dialed from the host, the way egress is, so 127.0.0.1 names the
	// host itself and not the gateway.
	Host bool `json:"host,omitempty"`
}

func (e ServiceEndpoint) String() string {
	addr := netip.AddrPortFrom(e.IP, e.Port).String()
	if e.Host {
		return "host:" + addr
	}
	return addr
}

// Service is one virtual address, port and protocol, and the endpoints a
// connection to it is carried to.
type Service struct {
	// VIP is the virtual address. It must be inside the gateway's service
	// range.
	VIP netip.Addr `json:"vip"`
	// Port is the virtual port.
	Port uint16 `json:"port"`
	// Protocol is tcp or udp. Empty means tcp.
	Protocol string `json:"protocol"`
	// Endpoints are tried in turn, one connection or UDP flow at a time. A
	// service with none rejects every connection to it.
	Endpoints []ServiceEndpoint `json:"endpoints"`
}

type serviceKey struct {
	vip      netip.Addr
	port     uint16
	protocol string
}

func (s Service) key() serviceKey { return serviceKey{s.VIP, s.Port, s.Protocol} }

// serviceEntry is one installed service and its round-robin position.
type serviceEntry struct {
	endpoints []ServiceEndpoint
	next      atomic.Uint32
}

// ServiceTable holds the services a gateway routes. It is usable on its own,
// without a netstack, which is what lets the API be tested without one.
type ServiceTable struct {
	cidr        netip.Prefix
	subnet      netip.Prefix
	dialTimeout time.Duration

	mu      sync.RWMutex
	entries map[serviceKey]*serviceEntry
	list    []Service
}

// NewServiceTable returns an empty table for the service range cidr, with
// guest endpoints confined to subnet. A zero cidr gives a table that holds
// nothing and routes nothing.
func NewServiceTable(cidr, subnet netip.Prefix) *ServiceTable {
	return &ServiceTable{
		cidr:        cidr,
		subnet:      subnet,
		dialTimeout: defaultServiceDialTimeout,
		entries:     map[serviceKey]*serviceEntry{},
		list:        []Service{},
	}
}

// contains reports whether addr is a service address, which decides whether
// the forwarder routes it through the table or treats it as egress.
func (t *ServiceTable) contains(addr netip.Addr) bool {
	return t != nil && t.cidr.IsValid() && t.cidr.Contains(addr)
}

// SetServices replaces the whole table. Either all of in is installed or none
// of it is: a table with one bad entry is refused and the old one stays.
//
// # Errors
//
// Returns ErrServiceInvalid for a non-empty table when the gateway has no
// service range, or when any entry fails validation. An empty table is
// accepted either way, so a caller can clear the table unconditionally.
func (t *ServiceTable) SetServices(in []Service) error {
	if !t.cidr.IsValid() && len(in) > 0 {
		return fmt.Errorf("%w: this gateway was started without a service range", ErrServiceInvalid)
	}
	list := make([]Service, 0, len(in))
	entries := make(map[serviceKey]*serviceEntry, len(in))
	for _, s := range in {
		s, err := t.validate(s)
		if err != nil {
			return err
		}
		if _, ok := entries[s.key()]; ok {
			return fmt.Errorf("%w: %s/%s is listed more than once", ErrServiceInvalid,
				netip.AddrPortFrom(s.VIP, s.Port), s.Protocol)
		}
		entries[s.key()] = &serviceEntry{endpoints: s.Endpoints}
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.VIP != b.VIP {
			return a.VIP.Less(b.VIP)
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Protocol < b.Protocol
	})

	t.mu.Lock()
	t.entries, t.list = entries, list
	t.mu.Unlock()
	return nil
}

// Services returns the table as installed, ordered by address, port and
// protocol so two reads of an unchanged table render the same.
func (t *ServiceTable) Services() []Service {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// The endpoints too: the routing entries share them, and a caller
	// editing what it was handed must not change where traffic goes.
	out := make([]Service, len(t.list))
	for i, s := range t.list {
		s.Endpoints = append([]ServiceEndpoint{}, s.Endpoints...)
		out[i] = s
	}
	return out
}

// validate checks one service and returns it with the defaults filled in, so
// what a later read returns is what the forwarder acts on.
func (t *ServiceTable) validate(s Service) (Service, error) {
	protocol := strings.ToLower(s.Protocol)
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return Service{}, fmt.Errorf("%w: protocol %q, use tcp or udp", ErrServiceInvalid, s.Protocol)
	}
	s.Protocol = protocol
	s.VIP = s.VIP.Unmap()
	if !s.VIP.Is4() {
		return Service{}, fmt.Errorf("%w: virtual address %q is not IPv4", ErrServiceInvalid, s.VIP)
	}
	if !t.cidr.Contains(s.VIP) {
		return Service{}, fmt.Errorf("%w: virtual address %s is outside the service range %s",
			ErrServiceInvalid, s.VIP, t.cidr)
	}
	if s.Port == 0 {
		return Service{}, fmt.Errorf("%w: %s has no port", ErrServiceInvalid, s.VIP)
	}
	endpoints := make([]ServiceEndpoint, 0, len(s.Endpoints))
	for _, e := range s.Endpoints {
		e.IP = e.IP.Unmap()
		if !e.IP.Is4() {
			return Service{}, fmt.Errorf("%w: endpoint %q of %s is not IPv4", ErrServiceInvalid, e.IP, s.VIP)
		}
		if e.Port == 0 {
			return Service{}, fmt.Errorf("%w: endpoint %s of %s has no port", ErrServiceInvalid, e.IP, s.VIP)
		}
		// A guest endpoint is dialed through the stack, whose only route is
		// the subnet, so any other address would fail on every connection.
		if !e.Host && !t.subnet.Contains(e.IP) {
			return Service{}, fmt.Errorf("%w: endpoint %s of %s is not on the guest subnet %s; mark it host to dial it from the host",
				ErrServiceInvalid, e.IP, s.VIP, t.subnet)
		}
		endpoints = append(endpoints, e)
	}
	s.Endpoints = endpoints
	return s, nil
}

// route returns the endpoints of the service at vip:port, in the order to try
// them, and whether there is such a service. Each call starts one further
// along, which is the round robin.
func (t *ServiceTable) route(vip netip.Addr, port uint16, protocol string) ([]ServiceEndpoint, bool) {
	t.mu.RLock()
	e, ok := t.entries[serviceKey{vip, port, protocol}]
	t.mu.RUnlock()
	if !ok || len(e.endpoints) == 0 {
		return nil, ok
	}
	n := len(e.endpoints)
	start := int((e.next.Add(1) - 1) % uint32(n))
	out := make([]ServiceEndpoint, 0, n)
	out = append(out, e.endpoints[start:]...)
	out = append(out, e.endpoints[:start]...)
	return out, true
}

// hasEndpoints reports whether the service at vip:port exists and has an
// endpoint, without moving its round robin on.
func (t *ServiceTable) hasEndpoints(vip netip.Addr, port uint16, protocol string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[serviceKey{vip, port, protocol}]
	return ok && len(e.endpoints) > 0
}

// dialEndpoint opens a connection to one service endpoint: from the host for
// a host endpoint, and from inside the virtual network for a guest one, where
// the gateway is the only process that can reach it.
func dialEndpoint(ctx context.Context, s *stack.Stack, hostDial dialFunc, network string, e ServiceEndpoint, timeout time.Duration) (net.Conn, error) {
	if e.Host {
		return hostDial(network, netip.AddrPortFrom(e.IP, e.Port).String())
	}
	addr := tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(e.IP.As4()), Port: e.Port}
	if network == "udp" {
		// Checked here rather than returned directly: a nil *gonet.UDPConn
		// in a net.Conn is not a nil net.Conn.
		conn, err := gonet.DialUDP(s, nil, &addr, ipv4.ProtocolNumber)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(dialCtx, s, addr, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// SetServices replaces the gateway's service table. See ServiceTable.
func (n *Network) SetServices(in []Service) error { return n.services.SetServices(in) }

// Services returns the gateway's service table as installed.
func (n *Network) Services() []Service { return n.services.Services() }
