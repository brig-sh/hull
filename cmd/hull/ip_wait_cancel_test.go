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
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIPWaitContextCancellation(t *testing.T) {
	t.Run("missing DHCP lease", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			// An invalid MAC cannot match a real host lease. No system files
			// need to be changed to exercise the polling wait.
			_, err := waitForLeaseIPContext(ctx, "cancellation-test", 30*time.Second)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("missing lease: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("DHCP polling ignored cancellation")
		}
	})

	for _, tc := range []struct {
		name      string
		wait      func(context.Context, string) error
		wantError bool
	}{
		{
			name: "stalled gateway request",
			wait: func(ctx context.Context, sock string) error {
				_, err := gatewayLeasesContext(ctx, sock)
				return err
			},
			wantError: true,
		},
		{
			name: "stalled gateway mismatch wait",
			wait: func(ctx context.Context, sock string) error {
				warnOnLeaseMismatchContext(ctx, sock, "52:54:00:12:34:56", "10.87.0.10", 30*time.Second)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Darwin's Unix socket path limit is shorter than t.TempDir's
			// test-named directories.
			dir, err := os.MkdirTemp("/tmp", "hull-ip-wait-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			sock := filepath.Join(dir, "api.sock")
			listener, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{}, 1)
			server := &http.Server{
				ReadHeaderTimeout: time.Second,
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					select {
					case entered <- struct{}{}:
					default:
					}
					// Keep the request blocked before sending headers. Its
					// cancellation must reach the HTTP transport itself.
					<-r.Context().Done()
				}),
			}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.wait(ctx, sock) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("wait ended before the gateway request: %v", err)
			case <-time.After(time.Second):
				t.Fatal("wait did not reach the gateway")
			}
			cancel()
			select {
			case err := <-done:
				if tc.wantError && !errors.Is(err, context.Canceled) {
					t.Fatalf("gateway request: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("gateway wait ignored cancellation")
			}
		})
	}
}
