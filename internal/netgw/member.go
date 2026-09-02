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
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// A member is one guest on the gateway. Everything the policy says about a
// guest is said about its member, so the gateway has to know which member sent
// a frame before it can enforce anything per guest.
//
// The frame itself cannot answer that. The switch strips the Ethernet header
// before the netstack sees a packet, so the only identity that reaches the
// forwarder is the source address in the IP header, which the guest writes.
// What the gateway does have is one socket per member. Identity is therefore
// bound to the socket: the caller says who is joining, and every frame that
// arrives on that socket has to match.

// Member is the identity of one guest, as its caller declared it.
type Member struct {
	// Name is what egress rules refer to. For compose it is the service
	// name, and for a single instance it is the instance name.
	Name string
	// IP is the address the guest was given on the gateway subnet.
	IP netip.Addr
	// MAC is the guest's hardware address. It can be empty, in which case
	// the gateway pins whichever address the member first uses.
	MAC tcpip.LinkAddress
}

// Identified reports whether the caller said who is joining.
func (m Member) Identified() bool { return m.Name != "" && m.IP.IsValid() }

func (m Member) String() string {
	if !m.Identified() {
		return "unidentified member"
	}
	return fmt.Sprintf("%s (%s)", m.Name, m.IP)
}

// memberWire is how an identity crosses the control socket. It is JSON so a
// field can be added later without a flag day between a running gateway and a
// newer hull.
//
// The MAC travels in its textual form. A tcpip.LinkAddress holds six raw
// bytes, which are not valid UTF-8, and JSON replaces what it cannot encode:
// marshalling the raw bytes and reading them back gives a different address,
// silently.
type memberWire struct {
	Name string `json:"name,omitempty"`
	IP   string `json:"ip,omitempty"`
	MAC  string `json:"mac,omitempty"`
}

// EncodeMember renders an identity for the join message.
func EncodeMember(m Member) []byte {
	w := memberWire{Name: m.Name, MAC: m.MAC.String()}
	if m.IP.IsValid() {
		w.IP = m.IP.String()
	}
	if m.MAC == "" {
		w.MAC = ""
	}
	b, err := json.Marshal(w)
	if err != nil {
		// The struct is three strings; marshalling cannot fail.
		return nil
	}
	return b
}

// DecodeMember reads an identity from a join message. An older hull sends a
// single zero byte and no identity, which is not an error: the gateway treats
// that member as unidentified and says so.
func DecodeMember(b []byte) (Member, error) {
	if len(b) == 0 || (len(b) == 1 && b[0] == 0) {
		return Member{}, nil
	}
	var w memberWire
	if err := json.Unmarshal(b, &w); err != nil {
		return Member{}, fmt.Errorf("cannot read the member identity: %w", err)
	}
	m := Member{Name: w.Name}
	if w.MAC != "" {
		hw, err := net.ParseMAC(w.MAC)
		if err != nil {
			return Member{}, fmt.Errorf("member %q has an unparseable hardware address %q: %w", w.Name, w.MAC, err)
		}
		m.MAC = tcpip.LinkAddress(hw)
	}
	if w.IP != "" {
		addr, err := netip.ParseAddr(w.IP)
		if err != nil {
			return Member{}, fmt.Errorf("member %q has an unparseable address %q: %w", w.Name, w.IP, err)
		}
		m.IP = addr.Unmap()
	}
	return m, nil
}

// memberTable maps a guest address to the member that holds it. The egress
// policy reads it to decide whose rules apply to a connection.
//
// An address is held by one member at a time. A second member claiming an
// address already in use is refused rather than allowed to share it, because
// two guests behind one address cannot be told apart afterwards.
type memberTable struct {
	mu     sync.RWMutex
	byAddr map[netip.Addr]Member
}

func newMemberTable() *memberTable {
	return &memberTable{byAddr: map[netip.Addr]Member{}}
}

// claim registers a member's address. It fails when another member holds it.
func (t *memberTable) claim(m Member) error {
	if !m.IP.IsValid() {
		return nil
	}
	addr := m.IP.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	if held, ok := t.byAddr[addr]; ok {
		return fmt.Errorf("address %s is already held by member %q", addr, held.Name)
	}
	t.byAddr[addr] = m
	return nil
}

// release gives up a member's address when it leaves.
func (t *memberTable) release(m Member) {
	if !m.IP.IsValid() {
		return
	}
	addr := m.IP.Unmap()

	t.mu.Lock()
	defer t.mu.Unlock()
	if held, ok := t.byAddr[addr]; ok && held.Name == m.Name {
		delete(t.byAddr, addr)
	}
}

// nameOf returns the member holding addr, or "" when no member does.
func (t *memberTable) nameOf(addr netip.Addr) string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byAddr[addr.Unmap()].Name
}
