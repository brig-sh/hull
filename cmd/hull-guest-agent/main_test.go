// Copyright (c) 2023-2026, Nubificus LTD
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
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urunc-dev/urunc/pkg/agentproto"
)

// runSession opens one session on a fresh connection to serveConn and returns
// what it wrote and how it ended.
func runSession(t *testing.T, req agentproto.OpenRequest, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	host, guest := net.Pipe()
	defer host.Close()
	go func() {
		serveConn(guest)
		guest.Close()
	}()
	_ = host.SetDeadline(time.Now().Add(20 * time.Second))

	if err := agentproto.WriteJSON(host, agentproto.TypeOpen, 1, req); err != nil {
		t.Fatal(err)
	}
	if stdin != "" {
		if err := agentproto.WriteFrame(host, agentproto.TypeStdin, 1, []byte(stdin)); err != nil {
			t.Fatal(err)
		}
	}
	if err := agentproto.WriteFrame(host, agentproto.TypeCloseStdin, 1, nil); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	for {
		f, err := agentproto.ReadFrame(host)
		if err != nil {
			t.Fatalf("connection ended before the exit frame: %v", err)
		}
		switch f.Type {
		case agentproto.TypeStdout:
			out.Write(f.Payload)
		case agentproto.TypeStderr:
			errOut.Write(f.Payload)
		case agentproto.TypeExit:
			var ex agentproto.Exit
			if err := json.Unmarshal(f.Payload, &ex); err != nil {
				t.Fatal(err)
			}
			return out.String(), errOut.String(), ex.Code
		case agentproto.TypeError:
			t.Fatalf("agent error: %s", f.Payload)
		}
	}
}

// asSelf makes an empty user resolve to the test's own uid, as it does on a
// guest where that user is logged in at the console.
func asSelf(t *testing.T) {
	t.Helper()
	prev := consoleUser
	consoleUser = func() uint32 { return uint32(os.Getuid()) }
	t.Cleanup(func() { consoleUser = prev })
}

func TestSessionPipes(t *testing.T) {
	asSelf(t)
	out, errOut, code := runSession(t, agentproto.OpenRequest{
		Argv: []string{"/bin/sh", "-c", "cat; echo to-stderr >&2; exit 7"},
	}, "from-stdin\n")
	if out != "from-stdin\n" || errOut != "to-stderr\n" || code != 7 {
		t.Fatalf("got stdout %q, stderr %q, code %d", out, errOut, code)
	}
}

func TestSessionTTY(t *testing.T) {
	asSelf(t)
	out, _, code := runSession(t, agentproto.OpenRequest{
		Argv: []string{"/bin/sh", "-c", "test -t 0 && stty size"},
		TTY:  true, Rows: 31, Cols: 97,
	}, "")
	if code != 0 || !strings.Contains(out, "31 97") {
		t.Fatalf("got output %q, code %d", out, code)
	}
}

func TestSessionEnvDefaults(t *testing.T) {
	asSelf(t)
	out, _, code := runSession(t, agentproto.OpenRequest{
		Argv: []string{"/bin/sh", "-c", `echo "$PATH|$HOME|$USER|$SHELL|$PWD|$EXTRA"`},
		Env:  []string{"EXTRA=1"},
	}, "")
	fields := strings.Split(strings.TrimSpace(out), "|")
	if code != 0 || len(fields) != 6 {
		t.Fatalf("got output %q, code %d", out, code)
	}
	if fields[0] != defaultPath {
		t.Errorf("PATH is %q", fields[0])
	}
	home, _ := os.UserHomeDir()
	if fields[1] != home {
		t.Errorf("HOME is %q, want %q", fields[1], home)
	}
	if fields[2] == "" || fields[3] == "" {
		t.Errorf("USER and SHELL must be set, got %q and %q", fields[2], fields[3])
	}
	if fields[5] != "1" {
		t.Errorf("the request's own variable was lost: %q", fields[5])
	}
}

func TestResolveUser(t *testing.T) {
	prev := consoleUser
	t.Cleanup(func() { consoleUser = prev })

	consoleUser = func() uint32 { return 0 }
	if gu, err := resolveUser(""); err != nil || gu != nil {
		t.Fatalf("no console user must mean root, got %+v, %v", gu, err)
	}
	if gu, err := resolveUser("root"); err != nil || gu != nil {
		t.Fatalf("root must run as the agent, got %+v, %v", gu, err)
	}
	if _, err := resolveUser("no-such-user-here"); err == nil {
		t.Fatal("an unknown name must be refused")
	}

	consoleUser = func() uint32 { return uint32(os.Getuid()) }
	gu, err := resolveUser("")
	if err != nil || gu == nil {
		t.Fatalf("got %+v, %v", gu, err)
	}
	if gu.uid != uint32(os.Getuid()) || gu.groups[0] != gu.gid {
		t.Fatalf("got uid %d, groups %v, gid %d", gu.uid, gu.groups, gu.gid)
	}
	if !strings.HasPrefix(gu.shell, "/") {
		t.Fatalf("shell is %q", gu.shell)
	}
}

func TestExecAsSpec(t *testing.T) {
	groups := make([]uint32, maxGroups+4)
	for i := range groups {
		groups[i] = uint32(100 + i)
	}
	gu := &guestUser{uid: 501, gid: 20, groups: groups}
	uid, gid, got, err := parseExecAs(gu.execAsSpec())
	if err != nil {
		t.Fatal(err)
	}
	if uid != 501 || gid != 20 || len(got) != maxGroups || got[0] != 100 {
		t.Fatalf("got uid %d, gid %d, groups %v", uid, gid, got)
	}
	for _, bad := range []string{"", "501", "501:20", "x:20:1", "501:y:1", "501:20:z"} {
		if _, _, _, err := parseExecAs(bad); err == nil {
			t.Errorf("parseExecAs(%q) must fail", bad)
		}
	}
}

func TestDesktopSessionArgv(t *testing.T) {
	gu := &guestUser{uid: 501, gid: 20, groups: []uint32{20, 80}}
	argv, err := desktopSessionArgv(gu, []string{"open", "-a", "Claude"})
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	want := []string{"/bin/launchctl", "asuser", "501", self, execAsFlag, "501:20:20,80", "--", "open", "-a", "Claude"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", argv, want)
	}
}

func TestParseUserShell(t *testing.T) {
	if got := parseUserShell("UserShell: /bin/zsh\n"); got != "/bin/zsh" {
		t.Fatalf("got %q", got)
	}
	if got := parseUserShell("No such key: UserShell\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestIsMountPointAndParentOwner(t *testing.T) {
	if !isMountPoint("/dev") {
		t.Fatal("/dev is a mount point")
	}
	dir := t.TempDir()
	if isMountPoint(dir) {
		t.Fatalf("%s is not a mount point", dir)
	}
	uid, _, err := parentOwner(dir + "/a/b/c")
	if err != nil || uid != os.Getuid() {
		t.Fatalf("got uid %d, %v; want the owner of %s", uid, err, dir)
	}
}
