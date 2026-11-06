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

//go:build linux

package cni

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// The host's resolver configuration, and the one systemd-resolved writes
// with its upstream servers. Variables, so tests can point them at files of
// their own.
var (
	hostResolvConf     = "/etc/resolv.conf"
	resolvedResolvConf = "/run/systemd/resolve/resolv.conf"
)

// resolvedStub is the nameserver systemd-resolved puts in /etc/resolv.conf:
// a stub listener on the host's loopback.
const resolvedStub = "127.0.0.53"

// fallbackNameservers are what an instance gets when the host names no
// nameserver it can reach, the ones docker and nerdctl fall back to.
var fallbackNameservers = []string{"8.8.8.8", "8.8.4.4"}

// HostResolvConf returns the host's resolver configuration. When its only
// nameserver is systemd-resolved's stub, it returns the file
// systemd-resolved keeps with the upstream servers instead, as docker
// does: the stub listens on the host's loopback, which an instance cannot
// reach.
func HostResolvConf() ([]byte, error) {
	conf, err := os.ReadFile(hostResolvConf)
	if err != nil {
		return nil, fmt.Errorf("read the host's resolver configuration: %w", err)
	}
	servers := nameservers(conf)
	if len(servers) != 1 || servers[0] != resolvedStub {
		return conf, nil
	}
	upstream, err := os.ReadFile(resolvedResolvConf)
	if errors.Is(err, os.ErrNotExist) {
		return conf, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read systemd-resolved's resolver configuration: %w", err)
	}
	return upstream, nil
}

func nameservers(conf []byte) []string {
	var out []string
	for _, line := range strings.Split(string(conf), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}

// ResolvConf returns the /etc/resolv.conf an instance gets: first's
// nameservers, then host's that first does not already name, then host's
// other lines as they are.
//
// host's loopback nameservers are left out, since they answer on the host
// only, and so are its IPv6 ones: urunc gives the guest an IPv4 address
// only, so it has no route to them. With no nameserver left, the instance
// gets fallbackNameservers.
func ResolvConf(host []byte, first []string) []byte {
	servers := append([]string(nil), first...)
	var rest []string
	for _, line := range strings.Split(string(host), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], ";") {
			continue
		}
		if fields[0] != "nameserver" {
			rest = append(rest, strings.TrimSpace(line))
			continue
		}
		if len(fields) < 2 {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil || addr.IsLoopback() || !addr.Is4() || slices.Contains(servers, addr.String()) {
			continue
		}
		servers = append(servers, addr.String())
	}
	if len(servers) == 0 {
		servers = fallbackNameservers
	}
	var b bytes.Buffer
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	for _, line := range rest {
		fmt.Fprintln(&b, line)
	}
	return b.Bytes()
}
