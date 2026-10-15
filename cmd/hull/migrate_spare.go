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
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/brig-sh/hull/pkg/telemetry"
)

// After a streamed move commits, its source is the likeliest destination
// of the next move of the instance: the way back. A receive with no spare
// of the base copies it first, and on a Pi that took 22 s; and a base no
// receive has checked here is hashed whole while the stream arrives, which
// the move waits for before it is verified, with the guest paused. So the
// source makes the spare and checks the base itself once the move commits,
// in a process of its own that outlives the `hull migrate` (and the ssh
// session or brig that ran it), writing what it did to the instance's
// log. It keeps the base as a prepare's received one is kept, so the `hull
// rm` of the stale copy that a move back needs first leaves the base and
// the spare in place, until a `hull prune --bases`.

// startSpareBase starts the making of the spare of base sum for the move
// back of instance name, without waiting for it. A variable so tests can
// run it their way: the test binary is not hull.
var startSpareBase = spawnSpareBase

// spawnSpareBase runs `hull migrate-spare-base --for NAME` in a session of
// its own, its output appended to the instance's log, and returns once it
// has started.
func spawnSpareBase(s *store.Store, name, sum string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(s.InstanceLogFile(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	c := exec.Command(self, "--store-dir", s.RootDir(), "migrate-spare-base", "--sha256", sum, "--for", name)
	// Not the move's stdout or stderr, which an ssh session or brig reads
	// to its end: they would wait for the copy. Its reply goes to the log
	// after its lines, so a failure to open the store is there too.
	c.Stdout, c.Stderr = logFile, logFile
	c.Env = append(os.Environ(), telemetry.EnvSuppress+"=1")
	// Out of the move's session and process group, so the hangup of an
	// ssh session or a ^C to the brig that ran the move does not reach it.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

// spareForTheWayBack is what the source of a committed streamed move does
// for a move back: the base is kept, and its spare made in the background
// unless there is one. It says what it did on the move's output; the
// background process says how it went in the instance's log.
func (m *moveSource) spareForTheWayBack() {
	sum := m.rec.BaseSHA256
	if m.rec.Transport != transportStream || sum == "" {
		return
	}
	switch {
	case !baseHeld(m.s, sum):
		m.logf("move: no spare copy of base %s here for a move back: the base is gone from this store",
			digestHead(sum))
		return
	case spareHeld(m.s, sum) && baseCheckStands(m.s.BasePath(sum), sum):
		m.logf("move: base %s has a spare copy here for a move back", digestHead(sum))
	}
	if err := markBaseReceived(m.s, sum); err != nil {
		m.logf("warning: base %s is not kept for a move back: %v", digestHead(sum), err)
	}
	if spareHeld(m.s, sum) && baseCheckStands(m.s.BasePath(sum), sum) {
		return
	}
	if err := startSpareBase(m.s, m.name, sum); err != nil {
		m.logf("warning: no spare copy of base %s for a move back: %v; a move back copies the base",
			digestHead(sum), err)
		return
	}
	m.logf("move: making a spare copy of base %s here in the background, for a move back (see %s)",
		digestHead(sum), m.s.InstanceLogFile(m.name))
}

// spareHeld reports whether the spare of base sum is there.
func spareHeld(s *store.Store, sum string) bool {
	_, err := os.Stat(spareBasePath(s, sum))
	return checkDiskDigest(sum) == nil && err == nil
}

// spareAfterMove is `hull migrate-spare-base --for NAME`, the background
// half of spareForTheWayBack: it makes the spare unless there is one or
// the base is gone, and writes a dated line of each to w, the log.
func spareAfterMove(s *store.Store, name, sum string, w io.Writer) *migrateReply {
	if err := checkDiskDigest(sum); err != nil {
		return refuse(codeFailed, err)
	}
	line := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, "%s hull: "+format+"\n",
			append([]any{time.Now().UTC().Format(time.RFC3339)}, args...)...)
	}
	if !baseHeld(s, sum) {
		line("no spare copy of base %s for a move back of %s: the base is gone from this store", digestHead(sum), name)
		return &migrateReply{OK: true, Detail: "gone"}
	}
	how := "held"
	if spareHeld(s, sum) {
		line("base %s already has a spare copy for a move back of %s", digestHead(sum), name)
	} else {
		started := time.Now()
		line("making a spare copy of base %s for a move back of %s (pid %d)", digestHead(sum), name, os.Getpid())
		var err error
		if how, err = ensureSpareBase(s, sum); err != nil {
			line("no spare copy of base %s for a move back of %s: %v; a move back copies the base",
				digestHead(sum), name, err)
			return refuse(codeFailed, err)
		}
		if how == "held" {
			line("base %s already has a spare copy for a move back of %s", digestHead(sum), name)
		} else {
			line("made a spare copy of base %s for a move back of %s (%s, %.1f s)", digestHead(sum), name, how,
				time.Since(started).Seconds())
		}
	}
	if path := s.BasePath(sum); !baseCheckStands(path, sum) {
		started := time.Now()
		if err := checkBaseDigest(path, sum); err != nil {
			// The receive of a move back finds the same and has the base
			// sent again.
			line("base %s does not check (%v); a move back has it sent again", digestHead(sum), err)
			return refuse(codeFailed, err)
		}
		line("checked base %s for a move back of %s (%.1f s)", digestHead(sum), name, time.Since(started).Seconds())
	}
	return &migrateReply{OK: true, Detail: how}
}
