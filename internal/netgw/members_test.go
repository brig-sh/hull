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
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"
)

// joinQemu attaches a member over the QEMU stream protocol, as hvi does, and
// keeps reading what the switch sends it, since a member that stops reading
// wedges the switch.
func joinQemu(t *testing.T, n *Network) net.Conn {
	t.Helper()
	ours, theirs := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = n.AcceptQemu(ctx, theirs) }()
	go func() {
		buf := make([]byte, 65536)
		for {
			_, err := ours.Read(buf)
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ours.Close()
	})
	return ours
}

// awaitTransports waits until the gateway lists members of exactly these
// transports, in this order.
func awaitTransports(t *testing.T, n *Network, want ...string) []Member {
	t.Helper()
	deadline := time.Now().Add(replyWait)
	for {
		members := n.Members()
		got := make([]string, 0, len(members))
		for _, m := range members {
			got = append(got, m.Transport)
		}
		if slices.Equal(got, want) {
			return members
		}
		if time.Now().After(deadline) {
			t.Fatalf("members = %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A guest is a member while the switch serves it, of either transport, and
// stops being one when its connection closes.
func TestMembersFollowTheGuestsServed(t *testing.T) {
	n := testNetwork(t)
	got, err := json.Marshal(n.Members())
	if err != nil || string(got) != "[]" {
		t.Fatalf("no members marshal as %s (%v), want []", got, err)
	}

	before := time.Now()
	vfkit := join(t, n)
	awaitTransports(t, n, "vfkit")
	qemu := joinQemu(t, n)
	members := awaitTransports(t, n, "vfkit", "qemu")
	for _, m := range members {
		if m.JoinedAt.Before(before) || m.JoinedAt.After(time.Now()) {
			t.Errorf("member %+v joined outside the test", m)
		}
	}

	_ = vfkit.conn.Close()
	awaitTransports(t, n, "qemu")
	_ = qemu.Close()
	awaitTransports(t, n)
}

// Members that join within one tick of the clock still list in the order
// they joined.
func TestMembersListInJoinOrder(t *testing.T) {
	var s memberSet
	var want []string
	for i := range 50 {
		transport := fmt.Sprintf("m%d", i)
		s.add(transport)
		want = append(want, transport)
	}
	same := time.Now()
	for id, m := range s.members {
		m.JoinedAt = same
		s.members[id] = m
	}
	for range 5 {
		var got []string
		for _, m := range s.list() {
			got = append(got, m.Transport)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("members = %q, want them in join order %q", got, want)
		}
	}
}
