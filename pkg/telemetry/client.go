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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// sendTimeout bounds every delivery attempt. One POST, no retries,
// silent failure.
const sendTimeout = 2 * time.Second

// FlushTimeout is how long an exit path should wait for in-flight
// sends. It matches sendTimeout: a shorter grace abandons a send that
// would have succeeded, which is how short-lived invocations (a
// detached `run`, a quick command) dropped their events when a cold
// TLS POST to the CDN-fronted endpoint took longer than the grace.
// Flush returns the instant delivery completes, so this only adds
// latency when the endpoint is genuinely slow, and never more than a
// send could already take.
const FlushTimeout = sendTimeout

// Config carries everything the client needs from the caller. The
// caller decides interactivity and passes version and OS info in, which
// HostOS and HostUname read for it.
type Config struct {
	StoreDir  string
	Version   string
	OSVersion string // HostOS, eg. "26.3.1" or "ubuntu 24.04"
	Uname     string // HostUname: kernel name, release and machine

	// Interactive is true when stdin and stderr are both terminals.
	Interactive bool
	// Unattended skips the consent prompt even when Interactive.
	Unattended bool
	// DNT records an opt-out (the --dnt flag).
	DNT bool

	// Product is the product the events count against. Empty means the
	// one a wrapper names in HULL_TELEMETRY_PRODUCT, or hull. A caller
	// that sets it (brig, linking this package) ignores the variable.
	Product string
	// DocsURL is the schema page the consent prompt links to. Empty means
	// hull's.
	DocsURL string
	// AskFirst keeps an install off until someone has answered the
	// consent question. Without it, an unattended run on an install
	// nobody has asked yet defaults to on.
	AskFirst bool

	// Stdin/Stderr default to os.Stdin/os.Stderr; injectable for tests.
	Stdin  io.Reader
	Stderr io.Writer
}

// Client answers "may I send?" and delivers events. All its methods are
// safe on a nil receiver (no-ops), so callers never need nil checks.
type Client struct {
	cfg     Config
	product string
	// version is what the events report. runtimeVersion is hull's own
	// version when a wrapper's replaced it, and empty otherwise.
	version        string
	runtimeVersion string
	// suppressed are the events a wrapper sends itself, and this
	// invocation must not.
	suppressed map[string]bool
	st         *state
	enabled    bool
	debug      bool
	// inflight tracks background deliveries so exit paths can grant a
	// bounded grace via Flush without ever blocking indefinitely.
	inflight sync.WaitGroup
}

// Init loads state and runs the consent flow. It never
// returns an error and never panics: whatever goes wrong, the user's
// command must proceed and telemetry just stays off.
func Init(cfg Config) *Client {
	if cfg.Stdin == nil {
		cfg.Stdin = os.Stdin
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	if cfg.DocsURL == "" {
		cfg.DocsURL = DocsURL
	}
	// Child invocations of our own binary stay silent: the parent
	// command already counts, and a daemon must never prompt.
	suppressAll, suppressed := parseSuppress(os.Getenv(EnvSuppress))
	if len(suppressed) > 0 {
		// A wrapper names the events it sends itself. It also owns the
		// consent question: this invocation never asks, and sends
		// nothing until someone has answered.
		cfg.Unattended, cfg.AskFirst = true, true
	}
	c := &Client{
		cfg:        cfg,
		product:    cfg.Product,
		version:    cfg.Version,
		suppressed: suppressed,
		debug:      os.Getenv(EnvDebug) == "1",
	}
	// A wrapper driving hull names its product, and its version when it
	// knows it. hull's own version then goes out as runtime_version, so
	// the events say which hull ran under the wrapper.
	if c.product == "" {
		c.product = DefaultProduct
		if p := os.Getenv(EnvProduct); p != "" {
			c.product, c.runtimeVersion = p, cfg.Version
			if v := cleanVersion(os.Getenv(EnvVersion)); v != "" {
				c.version = v
			}
		}
	}
	if suppressAll {
		return c
	}

	// Per-invocation offs next, before any state is read or created:
	// an env-opted-out run leaves no trace on disk. --dnt still
	// persists below.
	if envOptedOut() && !cfg.DNT {
		return c
	}
	st, durable := loadOrCreateState(cfg.StoreDir)
	c.st = st
	// An install ID that cannot be persisted (read-only store, full
	// disk) would change on every invocation and corrupt per-install
	// analysis, so telemetry stays off. Existing readable state keeps
	// operating even if a later rewrite were to fail.
	if !durable {
		return c
	}

	// --dnt records the opt-out before anything else is considered.
	if cfg.DNT {
		c.st.Consent = boolPtr(false)
		c.st.ConsentVersion = ConsentVersion
		_ = saveState(cfg.StoreDir, c.st)
		return c
	}

	// A recorded "no" wins regardless of version; a recorded "yes" only
	// covers the consent version it answered.
	if c.st.Consent != nil {
		if !*c.st.Consent {
			return c
		}
		if c.st.ConsentVersion >= ConsentVersion {
			c.enabled = true
			return c
		}
	}

	// Either nothing on file, or a yes to an older ask. Interactive
	// runs get the (re-)ask. EOF or a read failure is NOT an answer:
	// nothing is persisted, telemetry stays off this invocation, and
	// the question remains open for a later run.
	if cfg.Interactive && !cfg.Unattended {
		answer, answered := c.ask()
		if answered {
			c.enabled = answer
			c.st.Consent = boolPtr(answer)
			c.st.ConsentVersion = ConsentVersion
			_ = saveState(cfg.StoreDir, c.st)
		}
		return c
	}

	// No prompt possible. A fresh install defaults to on (the consent
	// answer stays unrecorded, so a later interactive run still gets
	// asked; the install ID was already persisted under the state lock
	// by loadOrCreateState, so concurrent invocations agree on one
	// ID); a yes to an older ask does NOT cover the expanded schema --
	// expanding collection silently is the one thing we never do, so
	// stay off until an interactive run re-asks.
	if c.st.Consent == nil && !cfg.AskFirst {
		c.enabled = true
	}
	return c
}

// ask prints the consent prompt and reads one line. Empty input (a
// single enter) or anything starting with y/Y approves; an explicit
// n/N declines. EOF or a read failure is no answer at all (answered
// false): consent must never be recorded from a Ctrl-D.
func (c *Client) ask() (answer, answered bool) {
	_, _ = fmt.Fprintf(c.cfg.Stderr, `%s collects anonymous usage events and crash reports to help us
improve it: command names, which agent and backend you run, OS and tool
versions, and stack traces. An agent of your own goes out as a salted
hash of its name. File paths, arguments and image names are never sent.
Docs: %s
Enable telemetry? [Y/n] `, c.product, c.cfg.DocsURL)
	line, err := bufio.NewReader(c.cfg.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false, false
	}
	text := strings.ToLower(strings.TrimSpace(line))
	return text == "" || strings.HasPrefix(text, "y"), true
}

// Enabled reports whether events may be sent this invocation.
func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

// Sends reports whether an event of this kind goes out this invocation:
// telemetry is enabled, and no wrapper sends the event itself.
func (c *Client) Sends(event string) bool {
	return c.Enabled() && !c.suppressed[event]
}

// InstallID exposes the anonymous install UUID (for `telemetry status`).
func (c *Client) InstallID() string {
	if c == nil || c.st == nil {
		return ""
	}
	return c.st.InstallID
}

// Send delivers one event, merging fields into the common envelope
// documented in docs/telemetry.md. Delivery happens on a background
// goroutine so an unreachable endpoint never stalls the user command;
// exit paths call Flush for a bounded grace. Failure is never
// reported. With HULL_TELEMETRY_DEBUG=1 the payload is printed to
// stderr instead of being sent.
func (c *Client) Send(event string, fields map[string]string) {
	if !c.Sends(event) {
		return
	}
	body, err := json.Marshal(c.payload(event, fields))
	if err != nil {
		return
	}
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.deliver(body)
	}()
}

// Flush waits up to timeout for in-flight deliveries, reporting
// whether they all completed. Exit paths use a short grace; anything
// still in flight after it is simply dropped with the process.
func (c *Client) Flush(timeout time.Duration) bool {
	if c == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// payload assembles the common envelope around an event's fields,
// including the integrity checksum the ingestion side validates.
func (c *Client) payload(event string, fields map[string]string) map[string]any {
	capturedAt := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"schema_version": SchemaVersion,
		"event":          event,
		"product":        c.product,
		"version":        c.version,
		"platform":       Platform(),
		"os":             c.cfg.OSVersion,
		"arch":           runtime.GOARCH,
		"install_id":     c.st.InstallID,
		"captured_at":    capturedAt,
		"checksum":       Checksum(event, c.product, c.version, c.st.InstallID, capturedAt),
	}
	if c.runtimeVersion != "" {
		payload["runtime_version"] = c.runtimeVersion
	}
	if c.cfg.Uname != "" {
		payload["uname"] = c.cfg.Uname
	}
	for k, v := range fields {
		payload[k] = v
	}
	return payload
}

// Checksum is the soft integrity control: a salted
// SHA-256 over a few envelope fields, validated (and dropped on
// mismatch) at ingestion. The salt ships in the binary, so this guards
// against naive forgery only -- it is not a security boundary.
func Checksum(event, product, version, installID, capturedAt string) string {
	sum := sha256.Sum256([]byte(checksumSalt + "|" + event + "|" + product + "|" + version + "|" + installID + "|" + capturedAt))
	return hex.EncodeToString(sum[:])
}

// deliver hands one marshaled payload off: printed in debug mode, or
// POSTed with the send timeout. OpenTelemetry collector endpoints
// (path /v1/logs) get the payload wrapped as an OTLP/HTTP JSON log
// record; anything else receives the flat JSON. It reports whether the
// payload was handled -- with no endpoint configured nothing is, so
// queued crash files stay queued.
func (c *Client) deliver(body []byte) bool {
	if c.debug {
		_, _ = fmt.Fprintf(c.cfg.Stderr, "telemetry (not sent): %s\n", body)
		return true
	}
	endpoint := Endpoint
	if env := os.Getenv(EnvEndpoint); env != "" {
		endpoint = env
	}
	if endpoint == "" {
		return false
	}
	if strings.HasSuffix(endpoint, "/v1/logs") {
		body = otlpWrap(body)
	}
	client := &http.Client{Timeout: sendTimeout}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	// Only a 2xx counts as delivered: a rejected payload must not be
	// treated as handled (queued crash reports would be lost).
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// SetConsent records an explicit answer (the `telemetry on|off`
// subcommand) and returns the save error for the subcommand to report.
func SetConsent(storeDir string, enabled bool) error {
	st, _ := loadOrCreateState(storeDir)
	st.Consent = boolPtr(enabled)
	st.ConsentVersion = ConsentVersion
	return saveState(storeDir, st)
}

// Answer is the consent state on file, as `telemetry status` reports it.
type Answer int

const (
	// Unanswered means nobody has answered yet. An interactive run asks,
	// and an unattended one defaults to on unless the caller asks first.
	Unanswered Answer = iota
	// On means a yes to the current consent version.
	On
	// Off means a recorded no, or an opt-out variable in the environment.
	Off
	// Outdated means a yes to an older consent version. Nothing is sent
	// until an interactive run asks again.
	Outdated
)

// Effective returns the answer in force for storeDir. When a variable in the
// environment decided it, the second value names it, as NAME=1. It only reads:
// asking for the answer creates no state file and no install id.
func Effective(storeDir string) (Answer, string) {
	if envOptedOut() {
		return Off, optOutEnvName()
	}
	st := peekState(storeDir)
	switch {
	case st.Consent == nil:
		return Unanswered, ""
	case !*st.Consent:
		return Off, ""
	case st.ConsentVersion < ConsentVersion:
		return Outdated, ""
	default:
		return On, ""
	}
}

// Status describes the effective state for `telemetry status`.
func Status(storeDir string) string {
	answer, setting := Effective(storeDir)
	switch answer {
	case Unanswered:
		return "not configured (on by default; interactive runs will be asked)"
	case Off:
		if setting != "" {
			return fmt.Sprintf("disabled (%s)", setting)
		}
		return "disabled"
	case Outdated:
		return "enabled for an older schema (interactive runs will be re-asked)"
	default:
		return "enabled"
	}
}

// events are the event names a suppress list may hold.
var events = map[string]bool{"command": true, "start": true, "end": true, "metrics": true, "crash": true}

// parseSuppress reads HULL_TELEMETRY_SUPPRESS. "1" suppresses everything, and
// so does any value that is not a list of event names: someone who sets it to
// "true" or "yes" means off, and must not get the prompt-free sending a list
// asks for. A list of event names suppresses only those.
func parseSuppress(v string) (all bool, list map[string]bool) {
	if strings.TrimSpace(v) == "" {
		return false, nil
	}
	list = map[string]bool{}
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if !events[e] {
			return true, nil
		}
		list[e] = true
	}
	return false, list
}

// maxVersion is the longest wrapper version kept.
const maxVersion = 64

// cleanVersion returns a wrapper's version when it is spelled only with the
// characters of a version and is at most maxVersion long, and "" otherwise.
// A value that fails is dropped whole: stripping characters out of it would
// still send what was left, such as a branch name. The value goes into every
// event, the checksum and a queued crash report.
func cleanVersion(v string) string {
	if len(v) > maxVersion {
		return ""
	}
	for _, r := range v {
		if !versionRune(r) {
			return ""
		}
	}
	return v
}

// versionRune reports whether r is one of the characters a version is
// spelled with.
func versionRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)
}

func envOptedOut() bool {
	return os.Getenv(EnvDisabled) == "1" || os.Getenv(EnvDoNotTrack) == "1"
}

func optOutEnvName() string {
	if os.Getenv(EnvDisabled) == "1" {
		return EnvDisabled + "=1"
	}
	return EnvDoNotTrack + "=1"
}

func boolPtr(b bool) *bool { return &b }
