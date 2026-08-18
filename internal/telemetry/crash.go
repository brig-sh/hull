// Copyright (c) 2023-2026, Nubificus LTD
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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// crashDirName is the queue directory inside the store dir. The files
// are the whole queue: users can inspect or delete them at any time.
const crashDirName = "crashes"

// maxCrashFiles caps the queue; oldest files are dropped first. A crash
// loop must not fill the disk.
const maxCrashFiles = 5

// maxUploadsPerRun bounds how much time a post-crash invocation can
// spend flushing the queue (each upload is capped by sendTimeout).
const maxUploadsPerRun = 3

// maxStackBytes caps the scrubbed stack in a crash payload. A
// stack-overflow panic can produce hundreds of KB of trace, and the
// collector rejects bodies over its size limit -- a permanently
// rejected file at the head of the queue would wedge every upload
// behind it. 16KB keeps far more frames than debugging needs.
const maxStackBytes = 16 * 1024

// claimSuffix marks a queue file some invocation is currently
// uploading; staleClaimAge is when a claim from a dead process gets
// requeued (claims refresh their mtime when taken).
const claimSuffix = ".uploading"

const staleClaimAge = 10 * time.Minute

// CapturePanic writes a ready-to-send crash payload to the queue. It is
// called from the recover handler in main, so it swallows every error:
// the original panic output and exit code matter more than the report.
// Nothing is written when telemetry is off, and the payload carries the
// panic type and scrubbed stack only -- never the panic message, which
// can embed paths or image references.
func (c *Client) CapturePanic(recovered any, stack []byte, command, backend string) {
	if !c.Enabled() {
		return
	}
	scrubbed := scrubStack(string(stack))
	if len(scrubbed) > maxStackBytes {
		scrubbed = scrubbed[:maxStackBytes] + "\n[stack truncated]"
	}
	fields := map[string]string{
		"command":    command,
		"panic_type": fmt.Sprintf("%T", recovered),
		"stack":      scrubbed,
	}
	if backend != "" {
		fields["backend"] = backend
	}
	body, err := json.Marshal(c.payload("crash", fields))
	if err != nil {
		return
	}
	dir := filepath.Join(c.cfg.StoreDir, crashDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	name := fmt.Sprintf("%d-%s.json", time.Now().UTC().Unix(), newUUID()[:8])
	_ = os.WriteFile(filepath.Join(dir, name), body, 0o600)
	pruneCrashDir(dir)
}

// UploadPendingCrashesAsync flushes the queue from a background
// goroutine, tracked like any delivery, so initialization never blocks
// on it. Exit truncation is safe: a file leaves the queue only on a
// 2xx response, so an interrupted upload retries on a later run.
func (c *Client) UploadPendingCrashesAsync() {
	if !c.Enabled() {
		return
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.UploadPendingCrashes()
	}()
}

// UploadPendingCrashes flushes queued crash files, oldest first, at
// most maxUploadsPerRun per invocation. Each file is claimed first by
// an atomic rename, so two concurrent invocations never upload the
// same report twice; a claim is removed on delivery and renamed back
// on failure, and claims orphaned by a dead process are requeued once
// they go stale.
func (c *Client) UploadPendingCrashes() {
	if !c.Enabled() {
		return
	}
	dir := filepath.Join(c.cfg.StoreDir, crashDirName)
	recoverStaleClaims(dir)
	uploaded := 0
	for _, f := range sortedCrashFiles(dir) {
		if uploaded >= maxUploadsPerRun {
			return
		}
		claimed := f + claimSuffix
		if err := os.Rename(f, claimed); err != nil {
			// Another invocation claimed it between listing and now.
			continue
		}
		now := time.Now()
		_ = os.Chtimes(claimed, now, now)
		body, err := os.ReadFile(claimed)
		if err != nil || !c.deliver(body) {
			_ = os.Rename(claimed, f)
			return
		}
		_ = os.Remove(claimed)
		uploaded++
	}
}

// recoverStaleClaims requeues claims whose owner died mid-upload. The
// mtime was refreshed at claim time, so age here is real.
func recoverStaleClaims(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, claimSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleClaimAge {
			continue
		}
		full := filepath.Join(dir, name)
		_ = os.Rename(full, strings.TrimSuffix(full, claimSuffix))
	}
}

// stackPathRe matches the source-file lines of a Go stack trace
// ("\t/abs/path/file.go:123 +0x.."). The path part must admit spaces
// ("/Users/John Doe/...") or those frames would escape scrubbing.
var stackPathRe = regexp.MustCompile(`(?m)^(\t)(.+\.go)`)

// scrubStack trims stack-trace file paths to module-relative form so no
// home directory, username or machine layout leaves the machine.
func scrubStack(stack string) string {
	return stackPathRe.ReplaceAllStringFunc(stack, func(m string) string {
		sub := stackPathRe.FindStringSubmatch(m)
		return sub[1] + trimSourcePath(sub[2])
	})
}

// trimSourcePath cuts a source path down to something that identifies
// the frame without identifying the machine: module-relative for our
// code, module-cache-relative for dependencies, GOROOT-relative for the
// standard library, and file name plus one parent for anything else.
func trimSourcePath(p string) string {
	// First occurrence: a repo checked out under a directory that itself
	// matches a marker (eg. .../hull/cmd/hull/...) must
	// keep the full module-relative path.
	for _, marker := range []string{"/hull/", "/pkg/mod/", "/go/src/", "/src/runtime/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			return p[idx+1:]
		}
	}
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return p
}

// pruneCrashDir enforces the queue cap counting claimed files too --
// concurrent claims must not let the queue grow past the documented
// limit. Stale claims are recovered first; fresh claims are counted
// but never deleted, so the oldest unclaimed reports go.
func pruneCrashDir(dir string) {
	recoverStaleClaims(dir)
	queued := sortedCrashFiles(dir)
	excess := len(queued) + countClaims(dir) - maxCrashFiles
	for i := 0; i < excess && i < len(queued); i++ {
		_ = os.Remove(queued[i])
	}
}

// countClaims counts in-flight claim files (post-recovery, all fresh).
func countClaims(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), claimSuffix) {
			n++
		}
	}
	return n
}

// sortedCrashFiles lists the queue oldest first (names sort by their
// unix-timestamp prefix).
func sortedCrashFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files
}
