// Copyright (c) 2026, NOFire AI
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

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nofireai/urunc-macos/pkg/store"
)

// dhcpdLeasesPath is where macOS's bootpd records vmnet DHCP leases.
const dhcpdLeasesPath = "/var/db/dhcpd_leases"

// waitForLeaseIP polls the vmnet DHCP lease database until an entry matching
// the given MAC address appears, and returns its IP.
func waitForLeaseIP(mac string, timeout time.Duration) (string, error) {
	want := normalizeMAC(mac)
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(dhcpdLeasesPath); err == nil {
			if ip, ok := parseLeases(string(data))[want]; ok {
				return ip, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no DHCP lease for MAC %s after %s", mac, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// parseLeases extracts MAC → IP pairs from the dhcpd_leases file. Entries look
// like:
//
//	{
//	        name=...
//	        ip_address=192.168.64.2
//	        hw_address=1,52:5f:7b:ab:cd:ef
//	}
//
// bootpd strips leading zeros from each octet of hw_address, so MACs are
// normalized before use as map keys.
func parseLeases(data string) map[string]string {
	leases := make(map[string]string)
	var ip, mac string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "{":
			ip, mac = "", ""
		case strings.HasPrefix(line, "ip_address="):
			ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "hw_address="):
			// format: hw_address=1,<mac>  (leading "1," is the hw type)
			v := strings.TrimPrefix(line, "hw_address=")
			if idx := strings.Index(v, ","); idx >= 0 {
				v = v[idx+1:]
			}
			mac = normalizeMAC(v)
		case line == "}":
			if ip != "" && mac != "" {
				leases[mac] = ip
			}
		}
	}
	return leases
}

// normalizeMAC lowercases a colon-separated MAC and strips per-octet leading
// zeros so "52:0A:00:..." and "52:a:0:..." compare equal.
func normalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	for i, p := range parts {
		if n, err := strconv.ParseUint(p, 16, 8); err == nil {
			parts[i] = strconv.FormatUint(n, 16)
		}
	}
	return strings.Join(parts, ":")
}

// recordInstanceIP re-reads the instance state and stores the discovered IP.
// Re-reading (instead of writing a captured copy) avoids clobbering status
// changes made concurrently, e.g. a foreground run marking the instance
// stopped while the discovery goroutine completes.
func recordInstanceIP(s *store.Store, instanceName, ip string) {
	state, err := s.GetInstance(instanceName)
	if err != nil {
		return
	}
	state.IP = ip
	if err := s.SaveInstance(state); err != nil {
		log.WithError(err).Debug("failed to record instance IP")
		return
	}
	log.Debugf("Instance %s has IP %s", instanceName, ip)
}

// injectHosts appends host:ip entries (docker --add-host format) to the
// rootfs's /etc/hosts, creating it if needed.
func injectHosts(rootfsDir string, entries []string) error {
	if len(entries) == 0 {
		return nil
	}
	etcDir := filepath.Join(rootfsDir, "etc")
	if err := os.MkdirAll(etcDir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", etcDir, err)
	}
	f, err := os.OpenFile(filepath.Join(etcDir, "hosts"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open /etc/hosts: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, e := range entries {
		// Docker splits at the FIRST separator so the address side can
		// contain colons (IPv6). Both host:ip and host=ip are accepted.
		sep := ":"
		if idx := strings.IndexAny(e, ":="); idx >= 0 {
			sep = string(e[idx])
		}
		parts := strings.SplitN(e, sep, 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid --add-host entry %q, expected host:ip", e)
		}
		host, ip := parts[0], parts[1]
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("invalid --add-host entry %q: %q is not an IP address", e, ip)
		}
		if _, err := fmt.Fprintf(f, "%s\t%s\n", ip, host); err != nil {
			return fmt.Errorf("failed to write /etc/hosts entry: %w", err)
		}
	}
	return nil
}

// gatewayNetConfig derives the guest address, gateway address (.1 on the
// subnet) and dotted netmask from a CIDR like 10.87.0.10/24 — the pieces of
// the kernel ip= parameter, in the same format urunc uses on Linux.
func gatewayNetConfig(cidr string) (addr, gw, mask string, err error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid gateway CIDR %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", "", "", fmt.Errorf("gateway CIDR %q is not IPv4", cidr)
	}
	gwIP := ipnet.IP.To4()
	gwIP = net.IPv4(gwIP[0], gwIP[1], gwIP[2], gwIP[3]+1)
	m := ipnet.Mask
	return ip4.String(), gwIP.String(), net.IPv4(m[0], m[1], m[2], m[3]).String(), nil
}
