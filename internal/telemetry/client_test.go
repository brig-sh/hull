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
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlushWaitsForSlowDelivery(t *testing.T) {
	// A cold TLS POST to the CDN-fronted prod endpoint was measured at
	// ~730ms; model that with a slow server. The old 500ms exit grace
	// abandons it (dropping a detached run's start/command), while
	// FlushTimeout waits long enough to deliver.
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(700 * time.Millisecond)
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)

	c := Init(Config{StoreDir: t.TempDir()})
	c.Send("command", map[string]string{"command": "run"})

	if c.Flush(500 * time.Millisecond) {
		t.Fatal("the old 500ms grace must not outlast a 700ms delivery")
	}
	if !c.Flush(FlushTimeout) {
		t.Fatal("FlushTimeout must be long enough to deliver a slow send")
	}
	if received.Load() != 1 {
		t.Fatalf("endpoint should have received the event, got %d", received.Load())
	}
}

func TestUUIDShapeAndRotation(t *testing.T) {
	dir := t.TempDir()
	st := loadState(dir)
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(st.InstallID) {
		t.Fatalf("install ID %q is not a v4 UUID", st.InstallID)
	}
	if err := saveState(dir, st); err != nil {
		t.Fatal(err)
	}
	if got := loadState(dir).InstallID; got != st.InstallID {
		t.Fatalf("install ID not stable across load: %q != %q", got, st.InstallID)
	}
	if err := os.Remove(statePath(dir)); err != nil {
		t.Fatal(err)
	}
	if got := loadState(dir).InstallID; got == st.InstallID {
		t.Fatal("deleting the state file must rotate the install ID")
	}
}

func TestEnvOptOutsDisableWithoutPersisting(t *testing.T) {
	for _, env := range []string{EnvDisabled, EnvDoNotTrack} {
		t.Run(env, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(env, "1")
			c := Init(Config{StoreDir: dir, Interactive: true})
			if c.Enabled() {
				t.Fatalf("%s=1 must disable telemetry", env)
			}
			if _, err := os.Stat(statePath(dir)); !os.IsNotExist(err) {
				t.Fatal("env opt-out must not persist state")
			}
		})
	}
}

func TestSuppressedChildStaysSilent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSuppress, "1")
	c := Init(Config{StoreDir: dir, Interactive: true})
	if c.Enabled() {
		t.Fatal("suppressed child invocations must not send")
	}
	if _, err := os.Stat(statePath(dir)); !os.IsNotExist(err) {
		t.Fatal("suppressed child invocations must not touch state")
	}
}

func TestDNTFlagPersistsOptOut(t *testing.T) {
	dir := t.TempDir()
	if c := Init(Config{StoreDir: dir, DNT: true}); c.Enabled() {
		t.Fatal("--dnt must disable telemetry")
	}
	// A later invocation without any flag stays opted out.
	if c := Init(Config{StoreDir: dir}); c.Enabled() {
		t.Fatal("--dnt opt-out must persist across invocations")
	}
}

func TestInteractivePrompt(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"enter approves", "\n", true},
		{"y approves", "y\n", true},
		{"yes approves", "Yes\n", true},
		{"n declines", "n\n", false},
		{"no declines", "NO\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var prompt bytes.Buffer
			c := Init(Config{
				StoreDir:    dir,
				Interactive: true,
				Stdin:       strings.NewReader(tc.input),
				Stderr:      &prompt,
			})
			if c.Enabled() != tc.want {
				t.Fatalf("input %q: enabled = %v, want %v", tc.input, c.Enabled(), tc.want)
			}
			if !strings.Contains(prompt.String(), "[Y/n]") {
				t.Fatal("prompt not shown")
			}
			st := loadState(dir)
			if st.Consent == nil || *st.Consent != tc.want || st.ConsentVersion != ConsentVersion {
				t.Fatalf("answer not persisted correctly: %+v", st)
			}
			// The recorded answer must silence the prompt next time.
			var second bytes.Buffer
			c2 := Init(Config{StoreDir: dir, Interactive: true, Stderr: &second})
			if c2.Enabled() != tc.want || second.Len() != 0 {
				t.Fatal("recorded consent must be reused without re-prompting")
			}
		})
	}
}

func TestEOFIsNotConsent(t *testing.T) {
	dir := t.TempDir()
	var prompt bytes.Buffer
	c := Init(Config{StoreDir: dir, Interactive: true, Stdin: strings.NewReader(""), Stderr: &prompt})
	if c.Enabled() {
		t.Fatal("EOF must not enable telemetry")
	}
	if !strings.Contains(prompt.String(), "[Y/n]") {
		t.Fatal("prompt should have been shown")
	}
	if st := loadState(dir); st.Consent != nil {
		t.Fatal("EOF must not record an answer")
	}
	// The question stays open: a later interactive run asks again.
	var second bytes.Buffer
	c2 := Init(Config{StoreDir: dir, Interactive: true, Stdin: strings.NewReader("n\n"), Stderr: &second})
	if c2.Enabled() || second.Len() == 0 {
		t.Fatal("a later interactive run must re-ask")
	}
}

func TestUnwritableStateDisablesTelemetry(t *testing.T) {
	// Fresh install, unwritable store: the ID would differ on every
	// invocation, so telemetry must stay off.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if c := Init(Config{StoreDir: dir}); c.Enabled() {
		t.Fatal("a non-durable install ID must not enable telemetry")
	}

	// Existing consented state stays operational even read-only.
	dir2 := t.TempDir()
	yes := true
	if err := saveState(dir2, &state{InstallID: newUUID(), Consent: &yes, ConsentVersion: ConsentVersion}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir2, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir2, 0o700) }()
	c2 := Init(Config{StoreDir: dir2})
	if !c2.Enabled() || c2.InstallID() == "" {
		t.Fatal("existing readable consent must keep operating")
	}
}

func TestConcurrentFirstRunsShareInstallID(t *testing.T) {
	dir := t.TempDir()
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, _ := loadOrCreateState(dir)
			ids[i] = st.InstallID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("concurrent first runs must agree on one install ID: %v", ids)
		}
	}
}

func TestUnattendedDefaultsOnWithoutPersisting(t *testing.T) {
	for _, cfg := range []Config{
		{Interactive: false},                  // no TTY
		{Interactive: true, Unattended: true}, // --unattended on a TTY
	} {
		dir := t.TempDir()
		cfg.StoreDir = dir
		c := Init(cfg)
		if !c.Enabled() {
			t.Fatalf("config %+v: unattended must default to on", cfg)
		}
		if st := loadState(dir); st.Consent != nil {
			t.Fatal("unattended default must not persist an answer")
		}
		// The install ID must survive, or every unattended invocation
		// would count as a fresh install.
		if c2 := Init(cfg); c2.InstallID() != c.InstallID() {
			t.Fatal("unattended invocations must keep a stable install ID")
		}
	}
}

func TestOlderYesReAsksAfterConsentVersionBump(t *testing.T) {
	dir := t.TempDir()
	yes := true
	if err := saveState(dir, &state{InstallID: newUUID(), Consent: &yes, ConsentVersion: ConsentVersion - 1}); err != nil {
		t.Fatal(err)
	}
	// Non-interactive: an old "yes" must not enable the new schema.
	if c := Init(Config{StoreDir: dir}); c.Enabled() {
		t.Fatal("a yes to an older ask must not enable a newer schema")
	}
	// Interactive: the user is re-asked.
	var prompt bytes.Buffer
	c := Init(Config{StoreDir: dir, Interactive: true, Stdin: strings.NewReader("\n"), Stderr: &prompt})
	if !c.Enabled() || prompt.Len() == 0 {
		t.Fatal("consent version bump must re-ask interactively")
	}
	if st := loadState(dir); st.ConsentVersion != ConsentVersion {
		t.Fatal("re-consent must record the current consent version")
	}
}

func TestSendEnvelope(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		got = buf.Bytes()
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)
	t.Setenv(EnvProduct, "urunc-claude")

	c := Init(Config{StoreDir: t.TempDir(), Version: "0.1.0-test", OSVersion: "26.0", Uname: "Darwin 25.3.0 test arm64"})
	c.Send("command", map[string]string{"command": "run", "outcome": "ok"})
	if !c.Flush(2 * time.Second) {
		t.Fatal("send did not complete in time")
	}

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("endpoint received no valid JSON: %v", err)
	}
	for k, want := range map[string]any{
		"schema_version": float64(SchemaVersion),
		"event":          "command",
		"product":        "urunc-claude",
		"version":        "0.1.0-test",
		"os":             "26.0",
		"uname":          "Darwin 25.3.0 test arm64",
		"command":        "run",
		"outcome":        "ok",
		"install_id":     c.InstallID(),
	} {
		if payload[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, payload[k], want)
		}
	}
	capturedAt, _ := payload["captured_at"].(string)
	if capturedAt == "" {
		t.Fatal("payload must carry captured_at")
	}
	if payload["checksum"] != Checksum("command", "urunc-claude", "0.1.0-test", c.InstallID(), capturedAt) {
		t.Fatal("payload checksum must match the documented formula")
	}
}

func TestOTLPEndpointGetsWrappedLogRecord(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		got = buf.Bytes()
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL+"/v1/logs")

	c := Init(Config{StoreDir: t.TempDir(), Version: "0.1.0-test"})
	c.Send("command", map[string]string{"command": "ps", "outcome": "ok"})
	if !c.Flush(2 * time.Second) {
		t.Fatal("send did not complete in time")
	}

	var otlp struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Body struct {
						StringValue string `json:"stringValue"`
					} `json:"body"`
					Attributes []struct {
						Key string `json:"key"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(got, &otlp); err != nil {
		t.Fatalf("endpoint received no valid OTLP JSON: %v", err)
	}
	recs := otlp.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(recs))
	}
	var flat map[string]any
	if err := json.Unmarshal([]byte(recs[0].Body.StringValue), &flat); err != nil {
		t.Fatalf("log record body is not the flat payload: %v", err)
	}
	if flat["command"] != "ps" || flat["event"] != "command" {
		t.Fatalf("flat payload wrong: %v", flat)
	}
	keys := map[string]bool{}
	for _, a := range recs[0].Attributes {
		keys[a.Key] = true
	}
	for _, want := range []string{"event", "product", "version", "install_id", "captured_at", "checksum"} {
		if !keys[want] {
			t.Errorf("missing routing attribute %q", want)
		}
	}
}

func TestDebugPrintsInsteadOfSending(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)
	t.Setenv(EnvDebug, "1")

	var stderr bytes.Buffer
	c := Init(Config{StoreDir: t.TempDir(), Stderr: &stderr})
	c.Send("command", map[string]string{"command": "ps"})
	if !c.Flush(2 * time.Second) {
		t.Fatal("send did not complete in time")
	}

	if hits.Load() != 0 {
		t.Fatal("debug mode must not send")
	}
	if !strings.Contains(stderr.String(), `"command":"ps"`) {
		t.Fatalf("debug mode must print the payload, got: %s", stderr.String())
	}
}

func TestDisabledSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	t.Setenv(EnvEndpoint, srv.URL)

	c := Init(Config{StoreDir: t.TempDir(), DNT: true})
	c.Send("command", nil)
	c.Flush(time.Second)
	var nilClient *Client
	nilClient.Send("command", nil) // must not panic
	if !nilClient.Flush(time.Second) {
		t.Fatal("nil client Flush must succeed immediately")
	}
	if hits.Load() != 0 {
		t.Fatal("disabled client must not send")
	}
}

func TestDeliverCountsOnly2xxAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{200, true}, {204, true}, {400, false}, {429, false}, {500, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		t.Setenv(EnvEndpoint, srv.URL)
		c := Init(Config{StoreDir: t.TempDir()})
		if got := c.deliver([]byte(`{}`)); got != tc.want {
			t.Errorf("status %d: deliver = %v, want %v", tc.status, got, tc.want)
		}
		srv.Close()
	}
}

func TestEmptyEndpointSendsNothing(t *testing.T) {
	// No EnvEndpoint, Endpoint var empty: Send must be a silent no-op.
	c := Init(Config{StoreDir: t.TempDir()})
	if !c.Enabled() {
		t.Fatal("expected enabled (unattended default)")
	}
	c.Send("command", nil) // nothing to assert beyond "does not hang or panic"
}
