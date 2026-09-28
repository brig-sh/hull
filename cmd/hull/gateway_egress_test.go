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
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

// runGatewayCommand runs the hidden command far enough to see whether it
// accepted its flags. Every case here fails before the gateway opens
// anything, so nothing is left behind.
func runGatewayCommand(t *testing.T, args ...string) error {
	t.Helper()
	root := &cli.Command{Name: "hull", Commands: []*cli.Command{networkGatewayCommand()}}
	full := append([]string{"hull", "network-gateway", "--socket", filepath.Join(t.TempDir(), "gw.sock")}, args...)
	return root.Run(context.Background(), full)
}

// A gateway that is handed egress flags it does not know must not start. An
// older binary that ignored them would run wide open while its operator
// believed it was filtering, so this is the property the whole fail-closed
// argument rests on.
func TestGatewayRefusesUnknownFlags(t *testing.T) {
	err := runGatewayCommand(t, "--egress-allowed", "host=*.example.com")
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if !strings.Contains(err.Error(), "egress-allowed") {
		t.Fatalf("error %q does not name the flag", err)
	}
}

func TestGatewayRefusesBadEgressRules(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unparseable cidr",
			args: []string{"--egress-default", "deny", "--egress-allow", "cidr=10.0.0.0/33"},
			want: "10.0.0.0/33",
		},
		{
			name: "empty glob",
			args: []string{"--egress-default", "deny", "--egress-allow", "host="},
			want: "the host glob is empty",
		},
		{
			name: "unknown default",
			args: []string{"--egress-default", "maybe"},
			want: "--egress-default",
		},
		{
			name: "rules without a default",
			args: []string{"--egress-allow", "host=*.example.com"},
			want: "--egress-default",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runGatewayCommand(t, tc.args...)
			if err == nil {
				t.Fatal("the gateway started with a rule it cannot enforce")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the problem (want %q)", err, tc.want)
			}
		})
	}
}

// A gateway started with only --socket must still be joinable by qemu and hvi.
// hull run dials qemuGatewaySock(--gateway-sock) for those backends, so a
// gateway that listens there only when told to would turn the hand-started
// example in the docs, and the one hvi's --net shared refusal prints, into a
// second error. The explicit flag still wins.
func TestGatewayListensOnTheDerivedQemuSocketByDefault(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{name: "derived", want: "gw.sock.qemu"},
		{name: "explicit", extra: []string{"--qemu-socket"}, want: "q.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortSocketDir(t)
			sock := socketPath(t, dir, "gw.sock")
			want := socketPath(t, dir, tc.want)
			args := []string{"hull", "network-gateway", "--socket", sock}
			if tc.extra != nil {
				args = append(args, append(tc.extra, want)...)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				root := &cli.Command{Name: "hull", Commands: []*cli.Command{networkGatewayCommand()}}
				done <- root.Run(ctx, args)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})

			deadline := time.Now().Add(5 * time.Second)
			for {
				conn, err := net.Dial("unix", want)
				if err == nil {
					_ = conn.Close()
					break
				}
				select {
				case err := <-done:
					t.Fatalf("gateway exited before listening on %s: %v", want, err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("gateway never listened on %s: %v", want, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
