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

// streamCheckpointFormat is the first hvi checkpoint format with the
// streamed migrate op.
const streamCheckpointFormat = 2

// transportFacts are what decides how a move carries the guest.
type transportFacts struct {
	// WholeBundle is the operator asking for the whole-bundle move.
	WholeBundle bool
	// SourceFormat is the checkpoint_format of the source's hvi, 0 when
	// unknown.
	SourceFormat int
	// HasBase says the instance records a base and this store still
	// holds it.
	HasBase    bool
	BaseReason string
	// DestStream is the destination hull taking a streamed move, and
	// DestHviStream its hvi restoring from a stream.
	DestStream    bool
	DestHviStream bool
	DestHull      string
	DestHvi       string
}

// decideTransport is how a move carries the guest, and why when it is the
// whole bundle. The streamed move needs every piece: an hvi here that
// streams, a hull and an hvi there that take a stream, and a base the disk
// blocks apply to (the destination need not hold it yet: the move sends it
// before the guest stops).
func decideTransport(f transportFacts) (string, string) {
	switch {
	case f.WholeBundle:
		return transportBundle, "--whole-bundle asked for it"
	case !f.DestStream:
		return transportBundle, fmt.Sprintf("the hull there (%s) takes no streamed move", f.DestHull)
	case !f.DestHviStream:
		return transportBundle, fmt.Sprintf("the hvi there (%s) cannot restore from a stream", f.DestHvi)
	case f.SourceFormat < streamCheckpointFormat:
		return transportBundle, fmt.Sprintf("the hvi here writes checkpoint format %d, and a streamed "+
			"move needs %d", f.SourceFormat, streamCheckpointFormat)
	case !f.HasBase:
		return transportBundle, f.BaseReason
	}
	return transportStream, ""
}

// hviCheckpointFormat reads the checkpoint format of the running hvi from
// the machine profile it wrote, or asks it; 0 when neither answers.
func hviCheckpointFormat(state *store.InstanceState) int {
	var p struct {
		CheckpointFormat int `json:"checkpoint_format"`
	}
	if state.ProfilePath != "" {
		if data, err := os.ReadFile(state.ProfilePath); err == nil && json.Unmarshal(data, &p) == nil &&
			p.CheckpointFormat > 0 {
			return p.CheckpointFormat
		}
	}
	if state.ControlSock != "" {
		if reply, err := hviOp(state.ControlSock, "profile"); err == nil && json.Unmarshal(reply.Profile, &p) == nil {
			return p.CheckpointFormat
		}
	}
	return 0
}

// moveTransportFacts gathers the facts for a move of state to the probed
// destination.
func moveTransportFacts(s *store.Store, state *store.InstanceState, probe *migrateProbe, wholeBundle bool) transportFacts {
	f := transportFacts{WholeBundle: wholeBundle, SourceFormat: hviCheckpointFormat(state)}
	if probe != nil {
		f.DestStream = probe.hasCapability(moveCapStream)
		f.DestHviStream = probe.HviStream
		f.DestHull, f.DestHvi = probe.HullVersion, probe.Hvi
	}
	switch b := state.Base; {
	case b == nil:
		f.BaseReason = "the instance keeps no base (it was started before bases, or not on a block rootfs)"
	case !baseHeld(s, b.SHA256):
		f.BaseReason = fmt.Sprintf("its base %s is gone from this store", digestHead(b.SHA256))
	default:
		f.HasBase = true
	}
	return f
}
