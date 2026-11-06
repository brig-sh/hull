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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePlugin is a CNI plugin that logs each call to $FAKE_CNI_LOG, answers
// ADD with the result in $FAKE_CNI_RESULT, and fails ADD when
// $FAKE_CNI_FAIL is set.
const fakePlugin = `#!/bin/sh
echo "$CNI_COMMAND $CNI_CONTAINERID $CNI_NETNS $CNI_IFNAME" >> "$FAKE_CNI_LOG"
case "$CNI_COMMAND" in
VERSION) echo '{"cniVersion":"1.0.0","supportedVersions":["0.4.0","1.0.0"]}' ;;
ADD)
	if [ -n "$FAKE_CNI_FAIL" ]; then
		echo '{"cniVersion":"1.0.0","code":11,"msg":"no address left"}'
		exit 1
	fi
	printf '%s' "$FAKE_CNI_RESULT" ;;
esac
`

// bridgeResult is what the bridge plugin answers: the bridge, the host end
// of the veth and the instance's eth0, an IPv6 and an IPv4 address on eth0
// and one on the bridge.
const bridgeResult = `{"cniVersion":"1.0.0",
"interfaces":[{"name":"br0","mac":"aa:aa:aa:aa:aa:01"},{"name":"veth1","mac":"aa:aa:aa:aa:aa:02"},
	{"name":"eth0","mac":"aa:aa:aa:aa:aa:03","sandbox":"/proc/1/ns/net"}],
"ips":[{"interface":0,"address":"10.9.0.1/24"},
	{"interface":2,"address":"fd00::5/64","gateway":"fd00::1"},
	{"interface":2,"address":"10.9.0.5/24","gateway":"10.9.0.1"}]}`

// fakeNetwork writes a network named name whose one plugin is the fake, and
// returns its Config and the log the plugin writes.
func fakeNetwork(t *testing.T, name string) (Config, string) {
	t.Helper()
	plugins := t.TempDir()
	err := os.WriteFile(filepath.Join(plugins, "fake"), []byte(fakePlugin), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	conf := t.TempDir()
	list := `{"cniVersion":"1.0.0","name":"` + name + `","plugins":[{"type":"fake"}]}`
	err = os.WriteFile(filepath.Join(conf, "10-test.conflist"), []byte(list), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_CNI_LOG", log)
	t.Setenv("FAKE_CNI_RESULT", bridgeResult)
	t.Setenv("FAKE_CNI_FAIL", "")
	return Config{PluginDirs: []string{plugins}, ConfDir: conf, Network: name, CacheDir: t.TempDir()}, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestCheck(t *testing.T) {
	cfg, _ := fakeNetwork(t, "bridge")
	err := cfg.Check()
	if err != nil {
		t.Errorf("Check of a network whose plugin is there = %v", err)
	}
	_, err = os.Stat(filepath.Join(cfg.ConfDir, lockName))
	if err != nil {
		t.Errorf("nerdctl's lock file after Check: %v", err)
	}

	other := cfg
	other.Network = "other"
	err = other.Check()
	if err == nil || !strings.Contains(err.Error(), "load CNI network other") {
		t.Errorf("Check of a network not configured = %v", err)
	}

	noPlugin := cfg
	noPlugin.PluginDirs = []string{t.TempDir()}
	err = noPlugin.Check()
	if err == nil || !strings.Contains(err.Error(), "needs the fake plugin") {
		t.Errorf("Check with the plugin missing = %v", err)
	}

	// The hook creates the lock file, so a directory where it cannot
	// fails the run here, before anything is pulled.
	err = os.Remove(filepath.Join(cfg.ConfDir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory whatever its mode")
	}
	err = os.Chmod(cfg.ConfDir, 0o555)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.ConfDir, 0o755) })
	err = cfg.Check()
	if err == nil || !strings.Contains(err.Error(), "open the CNI lock") {
		t.Errorf("Check with a configuration directory it cannot write = %v", err)
	}
}

func TestAttach(t *testing.T) {
	cfg, log := fakeNetwork(t, "bridge")
	res, err := cfg.Attach(context.Background(), "ns-vm", "/proc/1/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	want := Result{IP: "10.9.0.5", MAC: "aa:aa:aa:aa:aa:03", Gateway: "10.9.0.1"}
	if res != want {
		t.Errorf("Attach = %+v, want eth0's IPv4 address %+v", res, want)
	}
	// Only the ADD: a container ID is never used twice, so no attachment
	// of an earlier container can be in the way.
	got := calls(t, log)
	if len(got) != 1 || got[0] != "ADD ns-vm /proc/1/ns/net eth0" {
		t.Errorf("plugin calls = %q, want the ADD alone", got)
	}
	_, err = os.Stat(filepath.Join(cfg.ConfDir, lockName))
	if err != nil {
		t.Errorf("nerdctl's lock file: %v", err)
	}
	_, err = os.Stat(filepath.Join(cfg.CacheDir, "results", "bridge-ns-vm-eth0"))
	if err != nil {
		t.Errorf("libcni's cached result in CacheDir: %v", err)
	}

	err = cfg.Detach(context.Background(), "ns-vm", "/pinned/netns")
	if err != nil {
		t.Fatal(err)
	}
	got = calls(t, log)
	if got[len(got)-1] != "DEL ns-vm /pinned/netns eth0" {
		t.Errorf("Detach's plugin call = %q, want a DEL of the pinned namespace", got[len(got)-1])
	}
}

func TestAttachUndoesAFailedAttach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   string
		result string
		want   string
	}{
		{"plugin fails", "1", bridgeResult, "no address left"},
		{"no IPv4 address", "", strings.Replace(bridgeResult, `"10.9.0.5/24"`, `"fd00::6/64"`, 1),
			"eth0 has no IPv4 address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, log := fakeNetwork(t, "bridge")
			t.Setenv("FAKE_CNI_FAIL", tc.fail)
			t.Setenv("FAKE_CNI_RESULT", tc.result)
			_, err := cfg.Attach(context.Background(), "ns-vm", "/proc/1/ns/net")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Attach = %v, want %q", err, tc.want)
			}
			// Nothing else detaches an instance whose hook failed.
			got := calls(t, log)
			if got[len(got)-1] != "DEL ns-vm /proc/1/ns/net eth0" {
				t.Errorf("last plugin call = %q, want the DEL that undoes the ADD", got[len(got)-1])
			}
		})
	}
}
