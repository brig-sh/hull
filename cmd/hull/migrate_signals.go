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
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// moveSignals is how `hull migrate` takes the signals that would end it.
// SIGINT, SIGTERM and SIGHUP cancel the move's context, which stops it at
// its next step with its record saying where, except while the move holds
// them: from hvi's migrate op until hull has recorded the fence that op
// ends in. hvi fences the guest as soon as its manifest is written, and
// until the fence is recorded the stream hull is reading and spooling is
// the guest, so a signal there waits, and acts once the fence is on
// record. SIGPIPE is taken too, so a write to a reader that went away
// (brig, a pipe to tee) fails as an error rather than killing hull.
type moveSignals struct {
	mu      sync.Mutex
	held    int
	pending os.Signal
	cancel  context.CancelFunc
	out     io.Writer
}

// watchMoveSignals returns ctx with the cancel the signals call, the
// signals to hold, and the function that stops watching.
func watchMoveSignals(ctx context.Context, out io.Writer) (context.Context, *moveSignals, func()) {
	ctx, cancel := context.WithCancel(ctx)
	ms := &moveSignals{cancel: cancel, out: out}
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				ms.got(sig)
			case <-done:
				return
			}
		}
	}()
	return ctx, ms, func() {
		signal.Stop(ch)
		close(done)
		cancel()
	}
}

func (ms *moveSignals) got(sig os.Signal) {
	if sig == syscall.SIGPIPE {
		return
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.held == 0 {
		ms.cancel()
		return
	}
	if ms.pending == nil {
		_, _ = fmt.Fprintf(ms.out, "move: %v: hvi may have fenced the guest, and its stream is not on record "+
			"yet; the move stops once it is\n", sig)
	}
	ms.pending = sig
}

// hold defers the signals until the matching release. A nil moveSignals
// (a move run by a test) holds nothing.
func (ms *moveSignals) hold() {
	if ms == nil {
		return
	}
	ms.mu.Lock()
	ms.held++
	ms.mu.Unlock()
}

// release ends a hold, and acts on a signal that came during it.
func (ms *moveSignals) release() {
	if ms == nil {
		return
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.held--
	if ms.held == 0 && ms.pending != nil {
		ms.cancel()
	}
}
