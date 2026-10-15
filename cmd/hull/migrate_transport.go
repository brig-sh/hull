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

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/brig-sh/hull/pkg/store"
)

// streamCheckpointFormat is the first hvi checkpoint format whose migrate
// stream carries the disk rounds (hvi docs/checkpoint.md, "The stream
// format"). Format 2, RAM rounds only, left the disk to hull.
const streamCheckpointFormat = 3

// transportFacts are what decides how a move carries the guest.
type transportFacts struct {
	// WholeBundle is the operator asking for the whole-bundle move.
	WholeBundle bool
	// SourceFormat is the checkpoint_format of the source's hvi, 0 when
	// unknown.
	SourceFormat int
	// HasBase says the instance records a base and this store still
	// holds it; HviBase that the running hvi was given that base, so its
	// migrate stream carries the blocks that differ from it.
	HasBase    bool
	HviBase    bool
	BaseReason string
	// DestStream is the destination hull taking a streamed move, and
	// DestHviStream its hvi restoring from a stream with its disk rounds
	// onto a copy of the base.
	DestStream    bool
	DestHviStream bool
	DestHull      string
	DestHvi       string
}

// decideTransport is how a move carries the guest, and why when it is the
// whole bundle. The streamed move needs every piece: an hvi here whose
// migrate stream carries the disk rounds against the base it was booted
// with, a hull and an hvi there that take such a stream, and the base
// itself (the destination need not hold it yet: the move sends it before
// the guest stops).
func decideTransport(f transportFacts) (string, string) {
	switch {
	case f.WholeBundle:
		return transportBundle, "--whole-bundle asked for it"
	case !f.DestStream:
		return transportBundle, fmt.Sprintf("the hull there (%s) does not take a %s streamed move", f.DestHull,
			moveCapStream)
	case !f.DestHviStream:
		return transportBundle, fmt.Sprintf("the hvi there (%s) cannot restore a stream's disk rounds onto a base",
			f.DestHvi)
	case f.SourceFormat < streamCheckpointFormat:
		return transportBundle, fmt.Sprintf("the hvi here writes checkpoint format %d, and a streamed "+
			"move needs %d, whose stream carries the disk", f.SourceFormat, streamCheckpointFormat)
	case !f.HasBase:
		return transportBundle, f.BaseReason
	case !f.HviBase:
		return transportBundle, "the hvi running the instance was not given its base (it was started by " +
			"an older hull), so its stream would not carry the disk"
	}
	return transportStream, ""
}

// hviProfileFacts is what a move reads of the running hvi's machine
// profile.
type hviProfileFacts struct {
	CheckpointFormat int `json:"checkpoint_format"`
	// DiskBase is the base the disk was booted against
	// (--disk-base-sha256), which a restore carries on.
	DiskBase string `json:"disk_base_sha256"`
}

// runningHviProfile reads the machine profile the running hvi wrote, or
// asks hvi for it.
func runningHviProfile(state *store.InstanceState) (hviProfileFacts, bool) {
	var p hviProfileFacts
	if state.ProfilePath != "" {
		if data, err := os.ReadFile(state.ProfilePath); err == nil && json.Unmarshal(data, &p) == nil &&
			p.CheckpointFormat > 0 {
			return p, true
		}
	}
	if state.ControlSock != "" {
		if reply, err := hviOp(state.ControlSock, "profile"); err == nil && json.Unmarshal(reply.Profile, &p) == nil {
			return p, true
		}
	}
	return p, false
}

// moveTransportFacts gathers the facts for a move of state to the probed
// destination.
func moveTransportFacts(s *store.Store, state *store.InstanceState, probe *migrateProbe, wholeBundle bool) transportFacts {
	f := transportFacts{WholeBundle: wholeBundle}
	prof, _ := runningHviProfile(state)
	f.SourceFormat = prof.CheckpointFormat
	if probe != nil {
		f.DestStream = probe.hasCapability(moveCapStream)
		f.DestHviStream = probe.HviStream && probe.HviStreamDisk
		f.DestHull, f.DestHvi = probe.HullVersion, probe.Hvi
	}
	switch b := state.Base; {
	case b == nil:
		f.BaseReason = "the instance keeps no base (it was started before bases, or not on a block rootfs)"
	case !baseHeld(s, b.SHA256):
		f.BaseReason = fmt.Sprintf("its base %s is gone from this store", digestHead(b.SHA256))
	default:
		f.HasBase = true
		f.HviBase = prof.DiskBase == b.SHA256
	}
	return f
}
