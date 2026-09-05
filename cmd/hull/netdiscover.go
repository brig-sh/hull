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
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/hull/pkg/store"
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
//	 name=...
//	 ip_address=192.168.64.2
//	 hw_address=1,52:5f:7b:ab:cd:ef
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
			// format: hw_address=1,<mac> (leading "1," is the hw type)
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

// hostsEntryLines validates --add-host entries and renders them as the
// /etc/hosts lines they become. Both writers below share it so that what a
// block image gets composed and what a directory rootfs gets appended cannot
// drift apart, and so that an invalid entry is caught before either of them
// has touched a file.
func hostsEntryLines(entries []string) (string, error) {
	var out strings.Builder
	for _, e := range entries {
		// Docker splits at the FIRST separator so the address side can
		// contain colons (IPv6). Both host:ip and host=ip are accepted.
		sep := ":"
		if idx := strings.IndexAny(e, ":="); idx >= 0 {
			sep = string(e[idx])
		}
		parts := strings.SplitN(e, sep, 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("invalid --add-host entry %q, expected host:ip", e)
		}
		host, ip := parts[0], parts[1]
		if net.ParseIP(ip) == nil {
			return "", fmt.Errorf("invalid --add-host entry %q: %q is not an IP address", e, ip)
		}
		fmt.Fprintf(&out, "%s\t%s\n", ip, host)
	}
	return out.String(), nil
}

// injectHosts appends host:ip entries (docker --add-host format) to the
// rootfs's /etc/hosts, creating it if needed.
//
// Every step goes through an os.Root opened on the rootfs rather than through
// filepath.Join. This directory is an unpacked image and both "etc" and
// "etc/hosts" are entries the image chose: either can be a symlink to an
// absolute host path, which pkg/ociclient/unpack.go preserves on purpose
// because real images need absolute symlinks. os.MkdirAll, os.ReadFile,
// os.WriteFile and os.OpenFile all follow such a link, so this function used to
// create, read and append to a file of the image's choosing anywhere the caller
// could write. See errImagePlantedPath in run.go.
func injectHosts(rootfsDir string, entries []string) error {
	if len(entries) == 0 {
		return nil
	}
	lines, err := hostsEntryLines(entries)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return fmt.Errorf("failed to open image rootfs %s: %w", rootfsDir, err)
	}
	defer func() { _ = root.Close() }()

	if err := root.MkdirAll("etc", 0755); err != nil {
		// An "etc" that is already there as a symlink makes MkdirAll fail with
		// EEXIST, which says nothing about where the link points, so resolve it
		// through the root before reporting: an image that shipped "etc" as a
		// link out of the rootfs must be named as having done so, not filed
		// under "could not create a directory".
		if _, serr := root.Stat("etc"); serr != nil {
			return rootfsRefusal("resolve", "etc", serr)
		}
		return rootfsRefusal("create", "etc", err)
	}
	// An image whose /etc/hosts has no trailing newline is ordinary, and
	// appending straight onto it welded the first entry to the last line --
	// "127.0.0.1\tlocalhost10.87.0.10\tweb". Both lines are then lost.
	existing, rerr := root.ReadFile("etc/hosts")
	if rerr != nil {
		// A file that cannot be read is not fatal -- the O_CREATE append below
		// covers a missing one, and there is nothing to terminate either way --
		// but a path that leads out of the rootfs must never be appended to.
		if refusal := rootfsRefusal("read", "etc/hosts", rerr); errors.Is(refusal, errImagePlantedPath) {
			return refusal
		}
	} else if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		if werr := root.WriteFile("etc/hosts", append(existing, '\n'), 0644); werr != nil {
			return rootfsRefusal("write", "etc/hosts", werr)
		}
	}
	f, err := root.OpenFile("etc/hosts", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return rootfsRefusal("open", "etc/hosts", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(lines); err != nil {
		return fmt.Errorf("failed to write /etc/hosts entry: %w", err)
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

// hostsWithEntries composes what /etc/hosts should be: whatever the image
// ships, plus the --add-host entries appended.
//
// injectHosts edits a rootfs in place, which a block image no longer has -- its
// rootfs is the store's own copy and must not be written to. This returns the
// bytes so the caller can put them wherever the rootfs actually lives. nil
// means there is nothing to add and the image's own file should stand.
func hostsWithEntries(rootfsDir string, entries []string) ([]byte, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	lines, err := hostsEntryLines(entries)
	if err != nil {
		return nil, err
	}
	// Read through the rootfs, not joined onto it. Nothing is written here, but
	// the bytes go straight into the guest's /etc/hosts, so following an
	// image-planted symlink would hand the guest whichever host file it named.
	//
	// A missing /etc/hosts is not an error: the entries become the whole file,
	// which is what injectHosts's O_CREATE did.
	existing, err := readFromRootfs(rootfsDir, "etc/hosts")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read /etc/hosts: %w", err)
	}
	var out bytes.Buffer
	out.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString(lines)
	return out.Bytes(), nil
}
