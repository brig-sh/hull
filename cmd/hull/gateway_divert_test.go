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

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/netgw"
	"github.com/urfave/cli/v3"
)

// A divert the gateway cannot honor has to stop it. Started anyway, the
// gateway would dial the destination directly.
func TestGatewayRefusesBadDiverts(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"relative socket", []string{"--divert", "api.example.com:443=divert.sock"}, "must be absolute"},
		{"port out of range", []string{"--divert", "api.example.com:70000=/tmp/d.sock"}, "not in 1-65535"},
		{"no port", []string{"--divert", "api.example.com=/tmp/d.sock"}, "expected host:port=socket"},
		{"duplicate", []string{"--divert", "api.example.com:443=/tmp/a.sock", "--divert", "api.example.com:443=/tmp/b.sock"}, "diverted twice"},
		{"socket path too long", []string{"--divert", "api.example.com:443=/" + strings.Repeat("d", 200)}, "pass a shorter --divert socket path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runGatewayCommand(t, tc.args...)
			if err == nil {
				t.Fatal("the gateway started with a divert it cannot honor")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the problem (want %q)", err, tc.want)
			}
		})
	}
}

// The diverts a gateway runs with are listed on its API, with their counters.
func TestGatewayListsItsDiverts(t *testing.T) {
	dir := shortSocketDir(t)
	sock := socketPath(t, dir, "gw.sock")
	api := socketPath(t, dir, "gw.api")
	target := socketPath(t, dir, "divert.sock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		root := &cli.Command{Name: "hull", Commands: []*cli.Command{networkGatewayCommand()}}
		done <- root.Run(ctx, []string{"hull", "network-gateway", "--socket", sock, "--api", api,
			"--divert", "93.184.216.34:443=" + target})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", api)
		},
	}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://gateway/diverts")
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			var got []netgw.DivertStatus
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			want := netgw.Divert{Host: "93.184.216.34", Port: 443, Socket: target}
			if len(got) != 1 || got[0].Divert != want || got[0].Addresses != 1 || got[0].Diverted != 0 {
				t.Fatalf("/diverts = %+v, want %+v with one address and no connections", got, want)
			}
			return
		}
		select {
		case err := <-done:
			t.Fatalf("gateway exited before serving its API: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway never served its API: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
