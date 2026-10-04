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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// crashDirName is the queue directory inside the store dir. The files
// are the whole queue: users can inspect or delete them at any time.
const crashDirName = "crashes"

// maxCrashFiles caps the queue; oldest files are dropped first. A crash
// loop must not fill the disk.
const maxCrashFiles = 5

// outboxDirName is the queue of events an invocation wrote instead of
// sending, because it could not wait for the network. Uploaded like the
// crash queue.
const outboxDirName = "outbox"

// maxOutboxFiles caps the outbox the way maxCrashFiles caps the crash queue.
const maxOutboxFiles = 20

// maxUploadsPerRun bounds how many queued files one invocation uploads, over
// both queues, and so the time it spends on them (each upload is capped by
// sendTimeout).
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

// queueTempPrefix names a queue file still being written.
const queueTempPrefix = ".queue-"

// CapturePanic writes a ready-to-send crash payload to the queue. It is
// called from the recover handler in main, so it swallows every error:
// the original panic output and exit code matter more than the report.
// Nothing is written when telemetry is off, and the payload carries the
// panic type and scrubbed stack only -- never the panic message, which
// can embed paths or image references.
func (c *Client) CapturePanic(recovered any, stack []byte, command, backend string) {
	if !c.Sends("crash") {
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
	c.enqueue(crashDirName, maxCrashFiles, body)
}

// Queue writes one event to the outbox instead of sending it, for the next
// invocation with telemetry on to upload, and returns the file it wrote, or
// "" when it wrote none. In debug mode it prints the event and writes nothing,
// so a later run cannot send what debug mode showed. It is for an exit that cannot wait for the network:
// brig queues its command event right before it hands its process to hull,
// and hull uploads it while the session runs. Removing the file before an
// upload claims it takes the event back.
func (c *Client) Queue(event string, fields map[string]string) string {
	if !c.Sends(event) {
		return ""
	}
	body, err := json.Marshal(c.payload(event, fields))
	if err != nil {
		return ""
	}
	if c.debug {
		c.deliver(body)
		return ""
	}
	return c.enqueue(outboxDirName, maxOutboxFiles, body)
}

// enqueue adds one marshaled payload to a queue directory and enforces its
// cap, and returns the file it wrote, or "" on a failure. The file appears
// under its final name only once it is complete, so an uploader never claims
// half of one. A build with no endpoint writes nothing, so that no other build
// sends what it collected.
func (c *Client) enqueue(dirName string, max int, body []byte) string {
	if endpoint() == "" {
		return ""
	}
	dir := filepath.Join(c.cfg.StoreDir, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	tmp, err := os.CreateTemp(dir, queueTempPrefix+"*")
	if err != nil {
		return ""
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	name := filepath.Join(dir, fmt.Sprintf("%d-%s.json", queueStamp(), newUUID()[:8]))
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), name) != nil {
		_ = os.Remove(tmp.Name())
		return ""
	}
	pruneQueue(dir, max)
	return name
}

// lastStamp is the last stamp queueStamp returned.
var lastStamp atomic.Int64

// queueStamp returns the time in nanoseconds for a queue file's name, greater
// than any it returned before. Names then sort in the order this process wrote
// them, even within one tick of the clock, and the cap drops the oldest.
func queueStamp() int64 {
	now := time.Now().UnixNano()
	for {
		last := lastStamp.Load()
		next := max(now, last+1)
		if lastStamp.CompareAndSwap(last, next) {
			return next
		}
	}
}

// UploadPendingAsync uploads the queues from a background goroutine,
// tracked like any delivery, so initialization never blocks on it. Exit
// truncation is safe: a file leaves a queue only on a 2xx response, so an
// interrupted upload retries on a later run.
func (c *Client) UploadPendingAsync() {
	if !c.Enabled() {
		return
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.UploadPending()
	}()
}

// UploadPending uploads the crash queue, then the outbox. A queue holds
// complete payloads, so whichever invocation runs next uploads them, whatever
// its own product or suppress list. It stops at the first failed delivery.
func (c *Client) UploadPending() {
	if !c.Enabled() {
		return
	}
	budget := maxUploadsPerRun
	for _, name := range []string{crashDirName, outboxDirName} {
		if !c.uploadQueue(filepath.Join(c.cfg.StoreDir, name), &budget) {
			return
		}
	}
}

// uploadQueue uploads one queue's files, oldest first, while budget lasts,
// takes each upload off budget, and reports whether every attempt was
// delivered. Each file is claimed first by an atomic rename, so two
// concurrent invocations never upload the same file twice; a claim is
// removed on delivery and renamed back on failure, and claims orphaned by a
// dead process are requeued once they go stale.
func (c *Client) uploadQueue(dir string, budget *int) bool {
	recoverStaleClaims(dir)
	for _, f := range queuedFiles(dir) {
		if *budget <= 0 {
			return true
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
			return false
		}
		_ = os.Remove(claimed)
		*budget--
	}
	return true
}

// clearQueues removes every queued file of dir that no upload has claimed. A
// no covers what was queued under an earlier yes.
func clearQueues(dir string) {
	for _, name := range []string{crashDirName, outboxDirName} {
		for _, f := range queuedFiles(filepath.Join(dir, name)) {
			_ = os.Remove(f)
		}
	}
}

// recoverStaleClaims requeues claims whose owner died mid-upload, and removes
// the temporary file of a write that died before its rename. The mtime of a
// claim was refreshed at claim time, so age here is real.
func recoverStaleClaims(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		claim, temp := strings.HasSuffix(name, claimSuffix), strings.HasPrefix(name, queueTempPrefix)
		if e.IsDir() || !claim && !temp {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleClaimAge {
			continue
		}
		full := filepath.Join(dir, name)
		if temp {
			_ = os.Remove(full)
			continue
		}
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

// pruneQueue enforces a queue's cap counting claimed files too --
// concurrent claims must not let the queue grow past the documented
// limit. Stale claims are recovered first; fresh claims are counted
// but never deleted, so the oldest unclaimed files go.
func pruneQueue(dir string, max int) {
	recoverStaleClaims(dir)
	queued := queuedFiles(dir)
	excess := len(queued) + countClaims(dir) - max
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

// queuedFiles lists a queue oldest first (names sort by their
// unix-timestamp prefix).
func queuedFiles(dir string) []string {
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
