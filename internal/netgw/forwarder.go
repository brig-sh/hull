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
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/inetaf/tcpproxy"
	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The forwarders are where a guest's traffic leaves the virtual network for
// the host's, so they are where the egress policy is enforced. Everything
// else a guest can reach stops short of this point: the gateway's own DNS and
// DHCP are endpoints registered on the stack, and guest-to-guest traffic is
// switched at layer 2 and never reaches the stack at all.

// dialFunc opens the host-side connection. Tests replace it; in the gateway
// it is net.Dial.
type dialFunc func(network, address string) (net.Conn, error)

// linkLocalSubnet is the EC2 metadata service. Upstream blocks it because a
// CoreOS guest dials it on boot and the gateway is not that service.
const linkLocalSubnet = "169.254.0.0/16"

func linkLocal() tcpip.Subnet {
	_, parsed, _ := net.ParseCIDR(linkLocalSubnet)
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFromSlice(parsed.IP), tcpip.MaskFromBytes(parsed.Mask))
	return subnet
}

// tcpipAddr converts a netip address for the stack, which speaks tcpip.
func tcpipAddr(a netip.Addr) tcpip.Address {
	if a.Is4() {
		return tcpip.AddrFrom4(a.As4())
	}
	return tcpip.AddrFrom16(a.As16())
}

// toNetip converts a stack address for the policy, which speaks netip.
func toNetip(a tcpip.Address) netip.Addr {
	addr, _ := netip.AddrFromSlice(a.AsSlice())
	return addr.Unmap()
}

// rejectLog keeps a rejected destination out of the log for a while after it
// is first reported. A blocked TCP connection is retried by the guest for as
// long as its stack keeps retransmitting the SYN, and one line per
// retransmission buries everything else.
type rejectLog struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const rejectLogInterval = 30 * time.Second

func newRejectLog() *rejectLog { return &rejectLog{seen: map[string]time.Time{}} }

func (l *rejectLog) report(protocol, member string, guest netip.Addr, dst netip.Addr, port uint16) {
	key := fmt.Sprintf("%s|%s|%s|%d", protocol, guest, dst, port)
	now := time.Now()

	l.mu.Lock()
	last, ok := l.seen[key]
	if ok && now.Sub(last) < rejectLogInterval {
		l.mu.Unlock()
		return
	}
	l.seen[key] = now
	for k, t := range l.seen {
		if now.Sub(t) > rejectLogInterval {
			delete(l.seen, k)
		}
	}
	l.mu.Unlock()

	who := guest.String()
	if member != "" {
		who = fmt.Sprintf("%s (%s)", member, guest)
	}
	log.Warnf("egress: rejected %s from %s to %s", protocol, who, net.JoinHostPort(dst.String(), strconv.Itoa(int(port))))
}

// tcpForwarder handles every TCP segment that matches no endpoint on the
// stack, which is every connection a guest opens to the outside world.
func tcpForwarder(s *stack.Stack, policy *Policy, members *memberTable, dial dialFunc, rejects *rejectLog) *tcp.Forwarder {
	linkLocal := linkLocal()
	return tcp.NewForwarder(s, 0, 10, func(r *tcp.ForwarderRequest) {
		dst, port := r.ID().LocalAddress, r.ID().LocalPort
		guest := r.ID().RemoteAddress

		if linkLocal.Contains(dst) {
			r.Complete(true)
			return
		}
		// The guard has already dropped any frame whose source address is not
		// the sender's own, so the address identifies the member.
		src := toNetip(guest)
		if !policy.AllowsConnection(members.nameOf(src), src, toNetip(dst)) {
			rejects.report("tcp", members.nameOf(src), src, toNetip(dst), port)
			r.Complete(true)
			return
		}

		outbound, err := dial("tcp", net.JoinHostPort(dst.String(), strconv.Itoa(int(port))))
		if err != nil {
			log.Tracef("dial() = %v", err)
			r.Complete(true)
			return
		}

		var wq waiter.Queue
		ep, tcpErr := r.CreateEndpoint(&wq)
		r.Complete(false)
		if tcpErr != nil {
			_ = outbound.Close()
			if _, ok := tcpErr.(*tcpip.ErrConnectionRefused); ok {
				log.Debugf("r.CreateEndpoint() = %v", tcpErr)
			} else {
				log.Errorf("r.CreateEndpoint() = %v", tcpErr)
			}
			return
		}

		remote := tcpproxy.DialProxy{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return outbound, nil },
		}
		remote.HandleConn(gonet.NewTCPConn(&wq, ep))
	})
}

// udpForwarder does for datagrams what tcpForwarder does for connections. The
// policy is applied per forwarder request, which is once per flow rather than
// once per datagram.
func udpForwarder(s *stack.Stack, policy *Policy, members *memberTable, dial dialFunc, rejects *rejectLog) *udp.Forwarder {
	linkLocal := linkLocal()
	return udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		dst, port := r.ID().LocalAddress, r.ID().LocalPort
		guest := r.ID().RemoteAddress

		if linkLocal.Contains(dst) || dst == header.IPv4Broadcast {
			return
		}
		src := toNetip(guest)
		if !policy.AllowsConnection(members.nameOf(src), src, toNetip(dst)) {
			rejects.report("udp", members.nameOf(src), src, toNetip(dst), port)
			return
		}

		var wq waiter.Queue
		ep, udpErr := r.CreateEndpoint(&wq)
		if udpErr != nil {
			if _, ok := udpErr.(*tcpip.ErrConnectionRefused); ok {
				log.Debugf("r.CreateEndpoint() = %v", udpErr)
			} else {
				log.Errorf("r.CreateEndpoint() = %v", udpErr)
			}
			return
		}

		p, _ := forwarder.NewUDPProxy(&autoStoppingListener{underlying: gonet.NewUDPConn(&wq, ep)}, func() (net.Conn, error) {
			return dial("udp", net.JoinHostPort(dst.String(), strconv.Itoa(int(port))))
		})
		go func() {
			p.Run()
			// Datagrams sent to this flow are dropped from here until a new
			// forwarder request arrives.
			ep.Close()
		}()
	})
}

// autoStoppingListener bounds an idle UDP flow: every read and write pushes
// the deadline out, so a flow nobody uses stops on its own. Upstream has the
// same type, unexported.
type autoStoppingListener struct {
	underlying *gonet.UDPConn
}

func (l *autoStoppingListener) ReadFrom(b []byte) (int, net.Addr, error) {
	_ = l.underlying.SetReadDeadline(time.Now().Add(forwarder.UDPConnTrackTimeout))
	return l.underlying.ReadFrom(b)
}

func (l *autoStoppingListener) WriteTo(b []byte, addr net.Addr) (int, error) {
	_ = l.underlying.SetReadDeadline(time.Now().Add(forwarder.UDPConnTrackTimeout))
	return l.underlying.WriteTo(b, addr)
}

func (l *autoStoppingListener) SetReadDeadline(t time.Time) error {
	return l.underlying.SetReadDeadline(t)
}

func (l *autoStoppingListener) Close() error { return l.underlying.Close() }
