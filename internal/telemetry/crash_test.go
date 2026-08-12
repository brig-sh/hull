// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeStack = `goroutine 1 [running]:
main.runInstance(0x14000..., 0x1)
	/Users/somebody/develop/urunc-macos/cmd/urunc-macos/run.go:412 +0x1a4
github.com/sirupsen/logrus.(*Entry).Debugf(...)
	/Users/somebody/go/pkg/mod/github.com/sirupsen/logrus@v1.9.4/entry.go:314 +0x88
runtime.main()
	/opt/homebrew/Cellar/go/1.26.4/libexec/src/runtime/proc.go:283 +0x2f0
`

func TestScrubStackRemovesLocalPaths(t *testing.T) {
	scrubbed := scrubStack(fakeStack)
	for _, leak := range []string{"/Users/", "somebody", "/opt/homebrew"} {
		if strings.Contains(scrubbed, leak) {
			t.Errorf("scrubbed stack still contains %q:\n%s", leak, scrubbed)
		}
	}
	for _, keep := range []string{
		"urunc-macos/cmd/urunc-macos/run.go:412",
		"pkg/mod/github.com/sirupsen/logrus@v1.9.4/entry.go:314",
		"src/runtime/proc.go:283",
	} {
		if !strings.Contains(scrubbed, keep) {
			t.Errorf("scrubbed stack lost the frame %q:\n%s", keep, scrubbed)
		}
	}
}

func TestCapturePanicQueuesScrubbedReport(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir, Version: "0.1.0-test"})
	c.CapturePanic(fmt.Errorf("boom: /Users/somebody/secret"), []byte(fakeStack), "run", "vz")

	files := sortedCrashFiles(filepath.Join(dir, crashDirName))
	if len(files) != 1 {
		t.Fatalf("expected 1 queued crash file, got %d", len(files))
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "somebody") || strings.Contains(string(body), "secret") {
		t.Fatalf("crash payload leaks the panic message or paths: %s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"event":      "crash",
		"command":    "run",
		"backend":    "vz",
		"panic_type": "*errors.errorString",
	} {
		if payload[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, payload[k], want)
		}
	}
}

func TestCapturePanicDisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir, DNT: true})
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) != 0 {
		t.Fatal("disabled telemetry must not queue crash files")
	}
	var nilClient *Client
	nilClient.CapturePanic("boom", []byte(fakeStack), "run", "") // must not panic
	nilClient.UploadPendingCrashes()
}

func TestCrashQueueIsCapped(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	for i := 0; i < maxCrashFiles+3; i++ {
		c.CapturePanic("boom", []byte(fakeStack), "run", "")
	}
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) > maxCrashFiles {
		t.Fatalf("queue not pruned: %d files, cap %d", len(files), maxCrashFiles)
	}
}

func TestCrashStackIsCapped(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	huge := strings.Repeat("goroutine 1 [running]:\nmain.recurse(...)\n\t/tmp/x/main.go:10 +0x1a4\n", 4000)
	c.CapturePanic("stack overflow", []byte(huge), "run", "")
	files := sortedCrashFiles(filepath.Join(dir, crashDirName))
	if len(files) != 1 {
		t.Fatal("expected one queued crash")
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > maxStackBytes+4096 {
		t.Fatalf("crash payload not capped: %d bytes", len(body))
	}
	if !strings.Contains(string(body), "[stack truncated]") {
		t.Fatal("truncated stack must be marked")
	}
}

func TestUploadPendingCrashes(t *testing.T) {
	var hits atomic.Int32
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		lastBody = buf.Bytes()
		hits.Add(1)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)

	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	c.CapturePanic("boom", []byte(fakeStack), "run", "qemu")
	c.CapturePanic("boom", []byte(fakeStack), "ps", "")

	c.UploadPendingCrashes()
	if hits.Load() != 2 {
		t.Fatalf("expected 2 uploads, got %d", hits.Load())
	}
	if !strings.Contains(string(lastBody), `"event":"crash"`) {
		t.Fatalf("uploaded payload is not a crash event: %s", lastBody)
	}
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) != 0 {
		t.Fatal("delivered crash files must be removed from the queue")
	}
}

func TestUploadKeepsQueueOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // dead endpoint: every POST fails
	t.Setenv(EnvEndpoint, srv.URL)

	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	c.UploadPendingCrashes()
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) != 1 {
		t.Fatal("undelivered crash files must stay queued")
	}
}

func TestScrubStackHandlesSpacesInPaths(t *testing.T) {
	stack := "goroutine 1 [running]:\nmain.runInstance(...)\n\t/Users/John Doe/develop/urunc-macos/cmd/urunc-macos/run.go:412 +0x1a4\n"
	scrubbed := scrubStack(stack)
	for _, leak := range []string{"John", "/Users/"} {
		if strings.Contains(scrubbed, leak) {
			t.Errorf("scrubbed stack leaks %q:\n%s", leak, scrubbed)
		}
	}
	if !strings.Contains(scrubbed, "urunc-macos/cmd/urunc-macos/run.go:412") {
		t.Errorf("scrubbed stack lost the frame:\n%s", scrubbed)
	}
}

func TestUploadKeepsQueueOnRejectedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)

	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	c.UploadPendingCrashes()
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) != 1 {
		t.Fatal("a rejected upload must keep the crash file queued")
	}
}

func TestQueueCapCountsClaimedFiles(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	for i := 0; i < maxCrashFiles; i++ {
		c.CapturePanic("boom", []byte(fakeStack), "run", "")
	}
	crashDir := filepath.Join(dir, crashDirName)
	// Two concurrent invocations hold fresh claims.
	files := sortedCrashFiles(crashDir)
	for _, f := range files[:2] {
		if err := os.Rename(f, f+claimSuffix); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		_ = os.Chtimes(f+claimSuffix, now, now)
	}
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	queued := len(sortedCrashFiles(crashDir))
	claimed := countClaims(crashDir)
	if queued+claimed > maxCrashFiles {
		t.Fatalf("cap exceeded: %d queued + %d claimed > %d", queued, claimed, maxCrashFiles)
	}
	if claimed != 2 {
		t.Fatalf("fresh claims must never be deleted, got %d", claimed)
	}
}

func TestClaimedCrashIsNotDoubleUploaded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)

	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	files := sortedCrashFiles(filepath.Join(dir, crashDirName))
	if len(files) != 1 {
		t.Fatal("expected one queued crash")
	}
	// Another invocation holds a fresh claim on the file.
	claimed := files[0] + claimSuffix
	if err := os.Rename(files[0], claimed); err != nil {
		t.Fatal(err)
	}
	c.UploadPendingCrashes()
	if hits.Load() != 0 {
		t.Fatal("a freshly claimed crash must not be uploaded by another invocation")
	}
	// The claimant died: once the claim goes stale it is requeued and
	// uploaded exactly once.
	old := time.Now().Add(-2 * staleClaimAge)
	if err := os.Chtimes(claimed, old, old); err != nil {
		t.Fatal(err)
	}
	c.UploadPendingCrashes()
	if hits.Load() != 1 {
		t.Fatalf("stale claim must be recovered and uploaded once, got %d uploads", hits.Load())
	}
	if remaining := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(remaining) != 0 {
		t.Fatal("recovered crash must leave the queue after delivery")
	}
}

func TestUploadWithoutEndpointKeepsQueue(t *testing.T) {
	dir := t.TempDir()
	c := Init(Config{StoreDir: dir})
	c.CapturePanic("boom", []byte(fakeStack), "run", "")
	c.UploadPendingCrashes()
	if files := sortedCrashFiles(filepath.Join(dir, crashDirName)); len(files) != 1 {
		t.Fatal("with no endpoint configured, crash files must stay queued")
	}
}
