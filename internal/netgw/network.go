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

// Package netgw builds the user-mode network the gateway serves: one gvisor
// netstack, one L2 switch, and the services a guest needs (DHCP, DNS, NAT
// egress, host port forwarding).
//
// It composes the same building blocks as gvisor-tap-vsock's
// virtualnetwork.New rather than calling it. Egress filtering has to sit
// between the guest's SYN and the host's net.Dial, and between a guest's DNS
// query and the answer; upstream exposes no hook at either point and keeps
// the stack unexported, so hull owns the composition instead of forking the
// library.
package netgw

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// nicID is the single NIC every guest and every service shares.
const nicID = 1

// Config is what the gateway hands the netstack. It carries only the fields
// hull sets: an upstream Configuration has more, and a field accepted but
// ignored is worse than one that does not exist.
type Config struct {
	// Debug prints every packet on stderr.
	Debug bool
	// MTU of the virtual link.
	MTU int
	// Subnet the guests live on, in CIDR form.
	Subnet string
	// GatewayIP is the gateway's address on Subnet.
	GatewayIP string
	// GatewayMacAddress is the gateway's MAC on the virtual switch.
	GatewayMacAddress string
	// Forwards maps hostaddr:port to guestip:port for host port forwarding.
	Forwards map[string]string
	// DNSZones are records the gateway's own resolver answers from.
	DNSZones []gvntypes.Zone
}

// Network is a running user-mode network. Members join it by handing over a
// socket that carries their Ethernet frames.
type Network struct {
	stack         *stack.Stack
	networkSwitch *tap.Switch
}

func New(cfg Config) (*Network, error) {
	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("cannot parse subnet cidr: %w", err)
	}
	if cfg.MTU < 0 || cfg.MTU > math.MaxInt32 {
		return nil, errors.New("mtu is out of range")
	}

	ipPool := tap.NewIPPool(subnet)
	ipPool.Reserve(net.ParseIP(cfg.GatewayIP), cfg.GatewayMacAddress)

	endpoint, err := tap.NewLinkEndpoint(cfg.Debug, uint32(cfg.MTU), cfg.GatewayMacAddress, cfg.GatewayIP, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create tap endpoint: %w", err)
	}
	networkSwitch := tap.NewSwitch(cfg.Debug)
	endpoint.Connect(networkSwitch)
	networkSwitch.Connect(endpoint)

	s, err := createStack(cfg, endpoint)
	if err != nil {
		return nil, fmt.Errorf("cannot create network stack: %w", err)
	}
	if err := addServices(cfg, s, ipPool); err != nil {
		return nil, fmt.Errorf("cannot add network services: %w", err)
	}

	return &Network{stack: s, networkSwitch: networkSwitch}, nil
}

// AcceptVfkit serves a member that speaks the vfkit protocol: one Ethernet
// frame per datagram.
func (n *Network) AcceptVfkit(ctx context.Context, conn net.Conn) error {
	return n.networkSwitch.Accept(ctx, conn, gvntypes.VfkitProtocol)
}

// AcceptQemu serves a member that speaks the QEMU stream protocol:
// length-prefixed Ethernet frames.
func (n *Network) AcceptQemu(ctx context.Context, conn net.Conn) error {
	return n.networkSwitch.Accept(ctx, conn, gvntypes.QemuProtocol)
}

// DialContextTCP opens a TCP connection from inside the virtual network. The
// gateway is the only process that can reach a guest's listening port, so
// this is how healthchecks are run.
func (n *Network) DialContextTCP(ctx context.Context, addr string) (net.Conn, error) {
	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return nil, errors.New("invalid address, must be an IPv4 address")
	}
	return gonet.DialContextTCP(ctx, n.stack, tcpip.FullAddress{
		NIC:  nicID,
		Addr: tcpip.AddrFrom4Slice(ip.To4()),
		Port: uint16(port),
	}, ipv4.ProtocolNumber)
}

// createStack registers IPv4 and ARP only. IPv6 is deliberately absent: with
// no IPv6 network protocol the stack discards every IPv6 frame a guest sends,
// which is the behaviour we want until IPv6 forwarding is wired up.
func createStack(cfg Config, endpoint stack.LinkEndpoint) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
		},
	})

	if err := s.CreateNIC(nicID, endpoint); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return nil, errors.New(err.String())
	}

	// The stack answers for addresses it does not own: that is how a guest's
	// packet for the outside world reaches the forwarder at all.
	s.SetSpoofing(nicID, true)
	s.SetPromiscuousMode(nicID, true)

	_, parsedSubnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("cannot parse cidr: %w", err)
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(parsedSubnet.IP), tcpip.MaskFromBytes(parsedSubnet.Mask))
	if err != nil {
		return nil, fmt.Errorf("cannot parse subnet: %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: nicID}})

	return s, nil
}

func addServices(cfg Config, s *stack.Stack, ipPool *tap.IPPool) error {
	var natLock sync.Mutex
	tcpForwarder := forwarder.TCP(s, nil, &natLock, false)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	udpForwarder := forwarder.UDP(s, nil, &natLock, false)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	if err := dnsServer(cfg, s); err != nil {
		return err
	}
	if err := dhcpServer(cfg, s, ipPool); err != nil {
		return err
	}
	return forwardHostVM(cfg, s)
}

func dnsServer(cfg Config, s *stack.Stack) error {
	gatewayAddr := tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4())
	udpConn, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: nicID, Addr: gatewayAddr, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return err
	}
	tcpLn, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: nicID, Addr: gatewayAddr, Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return err
	}
	server, err := dns.New(udpConn, tcpLn, cfg.DNSZones)
	if err != nil {
		return err
	}
	go func() {
		if err := server.Serve(); err != nil {
			log.Error(err)
		}
	}()
	go func() {
		if err := server.ServeTCP(); err != nil {
			log.Error(err)
		}
	}()
	return nil
}

func dhcpServer(cfg Config, s *stack.Stack, ipPool *tap.IPPool) error {
	// dhcp.New reads the subnet, the gateway address and the MTU off an
	// upstream Configuration, so build one for it rather than widen Config.
	server, err := dhcp.New(&gvntypes.Configuration{
		Subnet:    cfg.Subnet,
		GatewayIP: cfg.GatewayIP,
		MTU:       cfg.MTU,
	}, s, ipPool)
	if err != nil {
		return err
	}
	go func() {
		log.Error(server.Serve())
	}()
	return nil
}

func forwardHostVM(cfg Config, s *stack.Stack) error {
	fw := forwarder.NewPortsForwarder(s)
	for local, remote := range cfg.Forwards {
		if strings.HasPrefix(local, "udp:") {
			if err := fw.Expose(gvntypes.UDP, strings.TrimPrefix(local, "udp:"), remote); err != nil {
				return err
			}
		} else if err := fw.Expose(gvntypes.TCP, local, remote); err != nil {
			return err
		}
	}
	return nil
}
