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
	"maps"
	"slices"
	"sync"
	"time"
)

// Member is a guest the gateway's switch is serving.
//
// The gateway is the one place that knows which guests are on it. A caller
// cannot read it off the host's process table: a vz-runner holds its
// connection as an inherited descriptor, and urunc on Linux binds the QEMU
// socket into hvi's root, so hvi's arguments name the same path inside it
// for every gateway.
type Member struct {
	// Transport is how the member attached: "vfkit" for a datagram socket
	// handed over the control socket, as vz-runner's is, and "qemu" for a
	// stream connection to the QEMU socket, as hvi's and qemu's are.
	Transport string `json:"transport"`
	// JoinedAt is when the switch started serving the member.
	JoinedAt time.Time `json:"joinedAt"`
}

// memberSet holds the members being served, each under an ID of its own,
// since nothing a member brings is unique to it before its first frame.
type memberSet struct {
	mu      sync.Mutex
	next    uint64
	members map[uint64]Member
}

func (s *memberSet) add(transport string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members == nil {
		s.members = make(map[uint64]Member)
	}
	s.next++
	s.members[s.next] = Member{Transport: transport, JoinedAt: time.Now()}
	return s.next
}

func (s *memberSet) remove(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.members, id)
}

// list returns the members, longest served first. It is never nil, so an
// empty gateway marshals as [] rather than null.
//
// The order comes from the IDs, which add hands out in the order members
// join, and not from JoinedAt: two members can join within one tick of the
// clock, and the map gives them in a random order, which a sort on equal
// times would keep.
func (s *memberSet) list() []Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := slices.Sorted(maps.Keys(s.members))
	out := make([]Member, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.members[id])
	}
	return out
}

// Members returns the guests the switch is serving now. A member is listed
// from the moment the switch takes its connection until the switch lets it
// go: a QEMU stream member until the connection closes, and a vfkit member
// until its control connection does, which its VMM holds for its lifetime.
// A VMM that has been started but has not connected yet is not listed.
func (n *Network) Members() []Member {
	return n.members.list()
}
