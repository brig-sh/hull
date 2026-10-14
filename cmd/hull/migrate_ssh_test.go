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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The remote command line survives the remote shell: every argument
// arrives as the one it was, whatever it holds.
func TestSSHRemoteCommandQuoting(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--name", "it's $HOME `id` \"q\" *", "--txid", "", "a\nb", "--x=$(touch " + dir + "/pwned)"}
	// "hull" is a shell function that prints each argument it gets: a
	// new script would cost a first-run scan on macOS.
	p := &sshPeer{target: "pm@h", remote: migrateRemote{Hull: "printargs", Store: dir + "/my store"}}
	cmdline := p.remoteCommand("migrate-status", args)
	out, err := exec.Command("/bin/sh", "-c",
		`printargs() { for a; do printf '%s\0' "$a"; done; }; `+cmdline).Output()
	if err != nil {
		t.Fatalf("%s: %v", cmdline, err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	want := append([]string{"--store-dir", dir + "/my store", "migrate-status"}, args...)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the remote shell saw %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("an argument ran a command")
	}
	argv := p.sshArgv("migrate-status", nil)
	if argv[0] != "ssh" || !strings.Contains(strings.Join(argv, " "), "BatchMode=yes") ||
		argv[len(argv)-3] != "--" || argv[len(argv)-2] != "pm@h" {
		t.Errorf("ssh argv %q", argv)
	}
	t.Setenv(migrateSSHEnv, "/x/fake-ssh")
	if argv := p.sshArgv("migrate-status", nil); argv[0] != "/x/fake-ssh" {
		t.Errorf("%s is not used: %q", migrateSSHEnv, argv)
	}
}

func TestSSHTarget(t *testing.T) {
	for _, ok := range []string{"pm@rp2", "rp2", "pm@10.0.0.2", "pm@rp2.tail-net.ts.net", "pm@[fe80::1]"} {
		if err := checkSSHTarget(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=sh", "pm@-oProxyCommand=sh", "pm@host;rm -rf /", "pm @host",
		"pm@host\n", "pm@host'"} {
		if err := checkSSHTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Anything that is not exactly one reply is no answer.
func TestParseMigrateReply(t *testing.T) {
	for _, bad := range []string{"", "Instance vm1 resumed\n{}", `{"protocol":1,"ok":true}{"protocol":1}`,
		`{"protocol":2,"ok":true}`, "bash: hull: command not found"} {
		if _, err := parseMigrateReply([]byte(bad)); err == nil {
			t.Errorf("%q parsed as a reply", bad)
		}
	}
	r, err := parseMigrateReply([]byte(`{"protocol":1,"ok":false,"code":"running","error":"x"}` + "\n"))
	if err != nil || r.OK || r.Code != codeRunning {
		t.Errorf("%+v %v", r, err)
	}
}
