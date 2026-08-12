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
	"os"
	"strings"
	"testing"
)

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"52:0A:00:1B:CD:EF": "52:a:0:1b:cd:ef",
		"52:a:0:1b:cd:ef":   "52:a:0:1b:cd:ef",
		"DE:AD:BE:EF:00:01": "de:ad:be:ef:0:1",
	}
	for in, want := range cases {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLeases(t *testing.T) {
	data := `{
	name=web
	ip_address=192.168.64.7
	hw_address=1,5e:a:bc:0:12:34
	identifier=1,5e:a:bc:0:12:34
	lease=0x69a1
}
{
	name=db
	ip_address=192.168.64.8
	hw_address=1,52:11:22:33:44:55
}`
	leases := parseLeases(data)
	if len(leases) != 2 {
		t.Fatalf("expected 2 leases, got %d", len(leases))
	}
	// query with the zero-padded form — must normalize to match
	if ip := leases[normalizeMAC("5E:0A:BC:00:12:34")]; ip != "192.168.64.7" {
		t.Errorf("lease lookup by padded MAC = %q, want 192.168.64.7", ip)
	}
	if ip := leases[normalizeMAC("52:11:22:33:44:55")]; ip != "192.168.64.8" {
		t.Errorf("second lease = %q, want 192.168.64.8", ip)
	}
}

func TestGatewayNetConfig(t *testing.T) {
	addr, gw, mask, err := gatewayNetConfig("10.87.0.10/24")
	if err != nil || addr != "10.87.0.10" || gw != "10.87.0.1" || mask != "255.255.255.0" {
		t.Errorf("gatewayNetConfig = %s,%s,%s,%v", addr, gw, mask, err)
	}
	if _, _, _, err := gatewayNetConfig("not-a-cidr"); err == nil {
		t.Error("expected error for invalid CIDR")
	}
}

func TestInjectHostsParsing(t *testing.T) {
	dir := t.TempDir()
	if err := injectHosts(dir, []string{"web:10.87.0.10", "db=2001:db8::1"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dir + "/etc/hosts")
	got := string(data)
	if !strings.Contains(got, "10.87.0.10\tweb") || !strings.Contains(got, "2001:db8::1\tdb") {
		t.Errorf("hosts content = %q", got)
	}
	for _, bad := range []string{"web", "web:", ":10.0.0.1", "web:notanip"} {
		if err := injectHosts(dir, []string{bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
