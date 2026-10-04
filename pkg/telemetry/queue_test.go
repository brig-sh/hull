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

package telemetry

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Queue is how an exit that cannot wait for the network still counts: the
// event goes to disk, and the next invocation with telemetry on uploads it.
func TestQueuedEventIsUploadedByTheNextRun(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		got = append(got, b.String())
		mu.Unlock()
	}))
	defer srv.Close()
	dir := t.TempDir()
	consentOn(t, dir)

	t.Setenv(EnvEndpoint, deadEndpoint)
	path := Init(Config{StoreDir: dir, Product: "brig"}).Queue("command", map[string]string{"command": "run"})
	if files := queuedFiles(filepath.Join(dir, outboxDirName)); len(files) != 1 || files[0] != path {
		t.Fatalf("Queue returned %q, and the outbox holds %v", path, files)
	}
	mu.Lock()
	sent := len(got)
	mu.Unlock()
	if sent != 0 {
		t.Fatal("Queue sent the event")
	}

	t.Setenv(EnvEndpoint, srv.URL)
	Init(Config{StoreDir: dir}).UploadPending()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], `"product":"brig"`) || !strings.Contains(got[0], `"command":"run"`) {
		t.Fatalf("the next run uploaded %q, want brig's queued event", got)
	}
	if files := queuedFiles(filepath.Join(dir, outboxDirName)); len(files) != 0 {
		t.Fatalf("a delivered event stayed queued: %v", files)
	}
}

func TestQueueHonorsSuppressionAndTheCap(t *testing.T) {
	dir := t.TempDir()
	if c := Init(Config{StoreDir: dir, AskFirst: true}); c.Enabled() {
		t.Fatal("test setup: an unanswered AskFirst client is enabled")
	} else if path := c.Queue("command", nil); path != "" {
		t.Fatalf("a client that sends nothing returned %q", path)
	}
	if files := queuedFiles(filepath.Join(dir, outboxDirName)); len(files) != 0 {
		t.Fatalf("a client that sends nothing queued %v", files)
	}

	consentOn(t, dir)
	c := Init(Config{StoreDir: dir})
	for i := 0; i < maxOutboxFiles+5; i++ {
		c.Queue("command", map[string]string{"command": "run"})
	}
	if files := queuedFiles(filepath.Join(dir, outboxDirName)); len(files) > maxOutboxFiles {
		t.Fatalf("outbox holds %d files, cap is %d", len(files), maxOutboxFiles)
	}
}

// One invocation uploads maxUploadsPerRun files over both queues, so the time
// it spends on them does not grow with the number of queues.
func TestOneRunUploadsAtMostItsBudget(t *testing.T) {
	var mu sync.Mutex
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent++
		mu.Unlock()
	}))
	defer srv.Close()
	dir := t.TempDir()
	consentOn(t, dir)
	c := Init(Config{StoreDir: dir})
	for i := 0; i < maxUploadsPerRun; i++ {
		c.enqueue(crashDirName, maxCrashFiles, []byte("{}"))
		c.enqueue(outboxDirName, maxOutboxFiles, []byte("{}"))
	}

	t.Setenv(EnvEndpoint, srv.URL)
	Init(Config{StoreDir: dir}).UploadPending()

	mu.Lock()
	defer mu.Unlock()
	if sent != maxUploadsPerRun {
		t.Fatalf("one run uploaded %d files, want %d", sent, maxUploadsPerRun)
	}
}

// A crash loop writes many files within one second. The cap keeps the newest
// of them, which needs names that sort in the order they were written.
func TestTheCapKeepsTheNewestWrittenInOneSecond(t *testing.T) {
	dir := t.TempDir()
	consentOn(t, dir)
	c := Init(Config{StoreDir: dir})
	for i := 0; i < maxCrashFiles+2; i++ {
		c.enqueue(crashDirName, maxCrashFiles, []byte{byte('0' + i)})
	}
	var kept []byte
	for _, f := range queuedFiles(filepath.Join(dir, crashDirName)) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, b...)
	}
	if string(kept) != "23456" {
		t.Fatalf("the cap kept %q, want the newest five, 23456", kept)
	}
}

// A process killed between the write and the rename leaves a temporary file
// that no upload lists. Once it is stale, the next write removes it.
func TestAStaleQueueTempFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	consentOn(t, dir)
	queue := filepath.Join(dir, crashDirName)
	if err := os.MkdirAll(queue, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(queue, queueTempPrefix+"1"), filepath.Join(queue, queueTempPrefix+"2")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleClaimAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	Init(Config{StoreDir: dir}).enqueue(crashDirName, maxCrashFiles, []byte("{}"))

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stale temporary file stayed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a temporary file still being written was removed: %v", err)
	}
}

// A build with no endpoint sends nothing. It queues nothing either, or the
// next build that has one would send what it collected.
func TestABuildWithNoEndpointQueuesNothing(t *testing.T) {
	dir := t.TempDir()
	consentOn(t, dir)
	t.Setenv(EnvEndpoint, "")
	c := Init(Config{StoreDir: dir})
	if path := c.Queue("command", nil); path != "" {
		t.Errorf("Queue wrote %s", path)
	}
	c.CapturePanic("boom", []byte("goroutine 1 [running]:\n"), "run", "")
	for _, q := range []string{crashDirName, outboxDirName} {
		if files := queuedFiles(filepath.Join(dir, q)); len(files) != 0 {
			t.Errorf("%s holds %v", q, files)
		}
	}
}

// Debug mode shows an event instead of sending it. Queued, it would be sent by
// the next run without debug mode.
func TestDebugModePrintsAQueuedEvent(t *testing.T) {
	dir := t.TempDir()
	consentOn(t, dir)
	t.Setenv(EnvDebug, "1")
	var out bytes.Buffer
	if path := Init(Config{StoreDir: dir, Stderr: &out}).Queue("command", map[string]string{"command": "ls"}); path != "" {
		t.Errorf("debug mode queued %s", path)
	}
	if !strings.Contains(out.String(), `"command":"ls"`) {
		t.Errorf("debug mode did not print the event:\n%s", out.String())
	}
}

// A no covers what was queued before it.
func TestANoClearsTheQueues(t *testing.T) {
	dir := t.TempDir()
	consentOn(t, dir)
	c := Init(Config{StoreDir: dir})
	c.Queue("command", nil)
	c.CapturePanic("boom", []byte("goroutine 1 [running]:\n"), "run", "")

	if err := SetConsent(dir, false); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{crashDirName, outboxDirName} {
		if files := queuedFiles(filepath.Join(dir, q)); len(files) != 0 {
			t.Errorf("%s still holds %v after a no", q, files)
		}
	}
}
