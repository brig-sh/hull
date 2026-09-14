// Copyright 2026 The hull Authors
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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayAPISockNamesTheComposePair(t *testing.T) {
	if got := gatewayAPISock("/tmp/hull/compose/web.gateway.sock"); got != "/tmp/hull/compose/web.api.sock" {
		t.Fatalf("compose pair: got %q", got)
	}
	if got := gatewayAPISock("/tmp/gw.sock"); got != "/tmp/gw.sock.api" {
		t.Fatalf("other name: got %q", got)
	}
}

// serveLeases answers /leases with the given table, the way the gateway does.
func serveLeases(t *testing.T, leases map[string]string) string {
	t.Helper()
	// A unix socket path is capped at 104 bytes on macOS, which t.TempDir's
	// test-named directories exceed.
	dir, err := os.MkdirTemp("/tmp", "hull-lease-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/leases", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(leases)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestLeaseMismatchWarnsWhenTheGuestIgnoredItsAddress(t *testing.T) {
	const mac = "52:54:00:12:34:56"
	sock := serveLeases(t, map[string]string{"10.87.0.2": mac, "10.87.0.1": "5a:94:ef:e4:0c:dd"})
	out := captureWarnings(t, func() {
		warnOnLeaseMismatch(sock, mac, "10.87.0.10", 0)
	})
	for _, want := range []string{"10.87.0.2", "10.87.0.10", "CONFIG_LIBUKNETDEV_EINFO_LIBPARAM"} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning %q does not mention %q", out, want)
		}
	}
}

func TestLeaseMismatchIsSilentWhenTheGuestTookWhatItWasTold(t *testing.T) {
	const mac = "52:54:00:12:34:56"
	// No lease for this MAC at all: a guest given an address never asks.
	sock := serveLeases(t, map[string]string{"10.87.0.1": "5a:94:ef:e4:0c:dd"})
	out := captureWarnings(t, func() {
		warnOnLeaseMismatch(sock, mac, "10.87.0.10", 0)
	})
	if strings.Contains(out, "took") {
		t.Fatalf("expected silence, got %q", out)
	}
}

// A lease matching the configured address is the guest agreeing, not a defect.
func TestLeaseMismatchIsSilentWhenTheLeaseMatches(t *testing.T) {
	const mac = "52:54:00:12:34:56"
	sock := serveLeases(t, map[string]string{"10.87.0.10": mac})
	out := captureWarnings(t, func() {
		warnOnLeaseMismatch(sock, mac, "10.87.0.10", 0)
	})
	if strings.Contains(out, "took") {
		t.Fatalf("expected silence, got %q", out)
	}
}

func TestLeaseMismatchIsSilentWithoutAGateway(t *testing.T) {
	out := captureWarnings(t, func() {
		warnOnLeaseMismatch(filepath.Join(t.TempDir(), "absent.sock"), "52:54:00:12:34:56", "10.87.0.10", 0)
	})
	if strings.Contains(out, "took") {
		t.Fatalf("expected silence, got %q", out)
	}
}
