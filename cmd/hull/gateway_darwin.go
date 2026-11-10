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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// gatewayAPISock names the HTTP API socket beside a gateway's control socket.
// compose builds both from the project name, so the mapping is that pair's;
// a control socket named otherwise gets the suffix appended.
func gatewayAPISock(controlSock string) string {
	if rest, ok := strings.CutSuffix(controlSock, ".gateway.sock"); ok {
		return rest + ".api.sock"
	}
	return controlSock + ".api"
}

// gatewayLeases reads the gateway's DHCP lease table: the address a guest was
// given, keyed by address. Returns nothing when the gateway serves no API
// socket, which is the common case for a gateway hull did not start.
func gatewayLeases(apiSock string) (map[string]string, error) {
	return gatewayLeasesContext(context.Background(), apiSock)
}

func gatewayLeasesContext(ctx context.Context, apiSock string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if apiSock == "" {
		return nil, errors.New("no gateway API socket")
	}
	// Stat first. Without it a gateway started without --api costs the caller
	// its whole polling budget on connections that can only fail.
	if _, err := os.Stat(apiSock); err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 3 * time.Second,
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://gateway/leases", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var leases map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&leases); err != nil {
		return nil, err
	}
	return leases, nil
}

// leaseFor returns the address the gateway gave mac, if it gave one.
func leaseFor(leases map[string]string, mac string) (string, bool) {
	for ip, holder := range leases {
		if strings.EqualFold(holder, mac) {
			return ip, true
		}
	}
	return "", false
}

// leaseMismatchMessage describes a guest that took a DHCP address after the
// runtime configured a static one. A configured guest never asks, so a lease
// for its MAC is the guest overriding what it was told, and every host-side
// record then names an address the guest does not answer on. Unikraft images
// built without CONFIG_LIBUKNETDEV_EINFO_LIBPARAM fail exactly this way.
func leaseMismatchMessage(mac, leased, configured string) string {
	return fmt.Sprintf("guest %s took %s from the gateway although it was configured with %s; "+
		"host-side records name %s, which the guest does not answer on "+
		"(a Unikraft image built without CONFIG_LIBUKNETDEV_EINFO_LIBPARAM ignores netdev.ip)",
		mac, leased, configured, configured)
}

// warnOnLeaseMismatch polls the gateway until the guest has asked for an
// address or budget runs out, and warns if it asked for a different one.
//
// A healthy guest never asks, so the whole budget is spent on the good path.
// Callers that cannot afford that pass a zero budget, which makes it a single
// probe, or run it where the wait is already being paid.
func warnOnLeaseMismatch(apiSock, mac, configured string, budget time.Duration) {
	warnOnLeaseMismatchContext(context.Background(), apiSock, mac, configured, budget)
}

func warnOnLeaseMismatchContext(ctx context.Context, apiSock, mac, configured string, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for {
		if ctx.Err() != nil {
			return
		}
		if leases, err := gatewayLeasesContext(ctx, apiSock); err == nil {
			if ip, ok := leaseFor(leases, mac); ok && ip != configured {
				log.Warn(leaseMismatchMessage(mac, ip, configured))
				return
			}
		} else if os.IsNotExist(err) {
			return // no API socket: nothing to read, now or later
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// joinGateway connects to a gateway's control socket, creates the datagram
// socketpair, sends one end to the gateway, and returns the local end plus
// the control connection (which must stay open for the member's lifetime).
func joinGateway(sockPath string) (*os.File, *net.UnixConn, error) {
	raddr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.DialUnix("unix", nil, raddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reach network gateway at %s: %w", sockPath, err)
	}

	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	// Generous buffers: each datagram is a full Ethernet frame. CLOEXEC so
	// nothing leaks into unrelated children — the VMM inherits its end
	// explicitly via ExtraFiles, which re-dups without the flag.
	for _, fd := range pair {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4<<20)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
		syscall.CloseOnExec(fd)
	}

	rights := unix.UnixRights(pair[1])
	if _, _, err := conn.WriteMsgUnix([]byte{0}, rights, nil); err != nil {
		_ = conn.Close()
		_ = syscall.Close(pair[0])
		_ = syscall.Close(pair[1])
		return nil, nil, fmt.Errorf("failed to send fd to gateway: %w", err)
	}
	// The gateway dup'ed the fd on receive; close our copy of its end.
	_ = syscall.Close(pair[1])

	return os.NewFile(uintptr(pair[0]), "vm-net"), conn, nil
}
