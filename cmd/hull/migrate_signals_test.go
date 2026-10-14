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
	"bytes"
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the signal goroutine may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// A signal that would end hull cancels the move at once, except while it
// is held: from hvi's migrate op until the fence is recorded. One that
// comes then waits for the release. SIGPIPE never ends the move.
func TestMoveSignalsWaitForTheFenceRecord(t *testing.T) {
	var out syncBuffer
	ctx, ms, stop := watchMoveSignals(context.Background(), &out)
	defer stop()
	ms.hold()
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE} {
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ctx.Done():
		t.Fatal("a held signal cancelled the move")
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(out.String(), "stops once it is") {
		t.Errorf("no word of the held signal: %q", out.String())
	}
	ms.release()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the held signal was lost at the release")
	}

	ctx, _, stop2 := watchMoveSignals(context.Background(), &out)
	defer stop2()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGPIPE); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("SIGPIPE cancelled the move")
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP outside a hold did not cancel the move")
	}
	var none *moveSignals
	none.hold()
	none.release()
}
