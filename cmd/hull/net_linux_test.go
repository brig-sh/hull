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

package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// fakeBundles stands in for the bundle directories of a host, so the
// test does not see the bundles installed on the machine it runs on.
func fakeBundles(t *testing.T, dirs ...string) {
	t.Helper()
	old := bundleDirs
	bundleDirs = func() []string { return dirs }
	t.Cleanup(func() { bundleDirs = old })
	t.Setenv("CNI_PATH", "")
	t.Setenv("NETCONFPATH", "")
	// No rootless brig configuration either.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestCNIConfig(t *testing.T) {
	user, system := t.TempDir(), t.TempDir()
	for _, dir := range []string{user, system} {
		err := os.MkdirAll(filepath.Join(dir, "etc", "cni", "net.d"), 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(t.TempDir(), "none")
	for _, tc := range []struct {
		name        string
		bundles     []string
		cniPath     string
		netConfPath string
		wantPlugins []string
		wantConf    string
	}{
		{"no bundle", []string{missing}, "", "", []string{"/opt/cni/bin"}, "/etc/cni/net.d"},
		{"first bundle installed", []string{missing, user, system}, "", "",
			[]string{filepath.Join(user, "libexec", "cni")}, filepath.Join(user, "etc", "cni", "net.d")},
		{"environment wins", []string{user}, "/a/bin:/b/bin", "/c/net.d",
			[]string{"/a/bin", "/b/bin"}, "/c/net.d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBundles(t, tc.bundles...)
			t.Setenv("CNI_PATH", tc.cniPath)
			t.Setenv("NETCONFPATH", tc.netConfPath)
			cfg, err := cniConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.PluginDirs, tc.wantPlugins) || cfg.ConfDir != tc.wantConf || cfg.Network != "bridge" {
				t.Errorf("cniConfig = %+v, want plugins %q, configuration %q, network bridge",
					cfg, tc.wantPlugins, tc.wantConf)
			}
		})
	}

	t.Run("rootless brig's copy of the configuration", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root has no rootless brig configuration")
		}
		fakeBundles(t, user)
		config := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", config)
		copied := filepath.Join(config, "brig", "cni", "net.d")
		err := os.MkdirAll(copied, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := cniConfig()
		if err != nil {
			t.Fatal(err)
		}
		// The configuration from the copy nerdctl reads, the plugins still
		// from the bundle.
		if cfg.ConfDir != copied || !slices.Equal(cfg.PluginDirs, []string{filepath.Join(user, "libexec", "cni")}) {
			t.Errorf("cniConfig = %+v, want configuration %s and the bundle's plugins", cfg, copied)
		}
		t.Setenv("NETCONFPATH", "/c/net.d")
		cfg, err = cniConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConfDir != "/c/net.d" {
			t.Errorf("cniConfig with NETCONFPATH = %+v, want /c/net.d", cfg)
		}
	})

	t.Run("relative paths made absolute", func(t *testing.T) {
		fakeBundles(t, missing)
		t.Chdir(t.TempDir())
		t.Setenv("CNI_PATH", "bin")
		t.Setenv("NETCONFPATH", "net.d")
		cfg, err := cniConfig()
		if err != nil {
			t.Fatal(err)
		}
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.PluginDirs, []string{filepath.Join(wd, "bin")}) || cfg.ConfDir != filepath.Join(wd, "net.d") {
			t.Errorf("cniConfig = %+v, want both under %s", cfg, wd)
		}
	})
}

func TestHookPath(t *testing.T) {
	got := hookPath("/home/u/bin:/usr/bin")
	if got != "/home/u/bin:/usr/bin:/usr/sbin:/sbin" {
		t.Errorf("hookPath = %q", got)
	}
	got = hookPath("")
	if got != "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("hookPath of an empty PATH = %q", got)
	}
}

// fakeCNI is a CNI plugin that answers ADD with one address on eth0, or
// fails it when $FAKE_CNI_FAIL is set, fails DEL when $FAKE_CNI_FAIL_DEL
// is set, and logs each call to $FAKE_CNI_LOG.
const fakeCNI = `#!/bin/sh
echo "$CNI_COMMAND $CNI_CONTAINERID $CNI_NETNS" >> "$FAKE_CNI_LOG"
case "$CNI_COMMAND" in
VERSION) echo '{"cniVersion":"1.0.0","supportedVersions":["1.0.0"]}' ;;
DEL)
	if [ -n "$FAKE_CNI_FAIL_DEL" ]; then
		echo '{"cniVersion":"1.0.0","code":11,"msg":"lease store unreadable"}'
		exit 1
	fi ;;
ADD)
	if [ -n "$FAKE_CNI_FAIL" ]; then
		echo '{"cniVersion":"1.0.0","code":11,"msg":"no address left"}'
		exit 1
	fi
	echo '{"cniVersion":"1.0.0","interfaces":[{"name":"eth0","mac":"aa:bb:cc:dd:ee:ff","sandbox":"/x"}],"ips":[{"interface":0,"address":"10.4.0.7/24","gateway":"10.4.0.1"}]}' ;;
esac
`

// fakeBridgeNetwork installs a network named bridge whose plugin is fakeCNI
// in a bundle of the test's own, and returns the bundle and the plugin's
// log.
func fakeBridgeNetwork(t *testing.T) (bundle, log string) {
	t.Helper()
	bundle = t.TempDir()
	plugins := filepath.Join(bundle, "libexec", "cni")
	conf := filepath.Join(bundle, "etc", "cni", "net.d")
	for _, dir := range []string{plugins, conf} {
		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}
	err := os.WriteFile(filepath.Join(plugins, "fake"), []byte(fakeCNI), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(conf, "10-bridge.conflist"),
		[]byte(`{"cniVersion":"1.0.0","name":"bridge","plugins":[{"type":"fake"}]}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fakeBundles(t, bundle)
	log = filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_CNI_LOG", log)
	t.Setenv("FAKE_CNI_FAIL", "")
	t.Setenv("FAKE_CNI_FAIL_DEL", "")
	old := hookCacheDir
	hookCacheDir = t.TempDir()
	t.Cleanup(func() { hookCacheDir = old })
	// A pin is a mount, which a test cannot make as a user; an empty file
	// at its place stands in for it.
	oldPin, oldPinned := pinNetns, netnsPinned
	pinNetns = func(_, target string) error { return os.WriteFile(target, nil, 0o444) }
	netnsPinned = func(target string) bool {
		_, err := os.Stat(target)
		return err == nil
	}
	t.Cleanup(func() { pinNetns, netnsPinned = oldPin, oldPinned })
	return bundle, log
}

func TestRunNetwork(t *testing.T) {
	privateLogDirs(t)
	bundle, _ := fakeBridgeNetwork(t)
	// No rootlesskit, so no rootlesskit DNS to ask for.
	t.Setenv("ROOTLESSKIT_STATE_DIR", filepath.Join(t.TempDir(), "none"))

	n, err := runNetwork(context.Background(), "none", "hull", "vm")
	if err != nil || n != nil {
		t.Errorf("runNetwork(none) = %v, %v; want no network", n, err)
	}
	_, err = runNetwork(context.Background(), "shred", "hull", "vm")
	if err == nil || !strings.Contains(err.Error(), `invalid --net "shred"`) {
		t.Errorf("runNetwork(shred) = %v", err)
	}

	n, err = runNetwork(context.Background(), "shared", "hull", "vm")
	if err != nil {
		t.Fatal(err)
	}
	if n.Mode != "shared" {
		t.Errorf("mode = %q, want shared, for the container's network label", n.Mode)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantDir, err := instanceNetDir("hull", "vm")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{exe, "internal", "oci-hook",
		"--namespace", "hull",
		"--network", "bridge",
		"--cni-netconfpath", filepath.Join(bundle, "etc", "cni", "net.d"),
		"--dir", wantDir,
		"--cni-path", filepath.Join(bundle, "libexec", "cni"),
	}
	if n.Hook.Path != exe || !reflect.DeepEqual(n.Hook.Args, wantArgs) {
		t.Errorf("hook = %s %q, want %s %q", n.Hook.Path, n.Hook.Args, exe, wantArgs)
	}
	if len(n.Hook.Env) != 1 || !strings.HasPrefix(n.Hook.Env[0], "PATH=") || !strings.HasSuffix(n.Hook.Env[0], ":/usr/sbin:/sbin") {
		t.Errorf("hook environment = %q, want a PATH that ends with the sbin directories", n.Hook.Env)
	}
	if n.Dir != wantDir || !bytes.Contains(n.ResolvConf, []byte("nameserver ")) {
		t.Errorf("network = %+v, want its directory %s and a resolv.conf with a nameserver", n, wantDir)
	}

	// A state directory a stopped rootlesskit left is no rootless daemon:
	// the run goes on with the host's nameservers and no namespace for the
	// hook to enter.
	stale := t.TempDir()
	t.Setenv("ROOTLESSKIT_STATE_DIR", stale)
	n, err = runNetwork(context.Background(), "shared", "hull", "vm")
	if err != nil {
		t.Fatalf("runNetwork with a stopped rootlesskit's state directory: %v", err)
	}
	if !reflect.DeepEqual(n.Hook.Args, wantArgs) {
		t.Errorf("hook = %q, want %q, with no rootlesskit state directory", n.Hook.Args, wantArgs)
	}

	t.Setenv("NETCONFPATH", t.TempDir())
	_, err = runNetwork(context.Background(), "shared", "hull", "vm")
	if err == nil || !strings.Contains(err.Error(), "load CNI network bridge") {
		t.Errorf("runNetwork with no network configured = %v", err)
	}
}

// runHook runs `hull internal oci-hook` as the runtime would, with state on
// stdin and the arguments runNetwork gives it.
func runHook(t *testing.T, bundle, dir, event, state string) error {
	t.Helper()
	app := newApp()
	app.Reader = strings.NewReader(state)
	app.Writer = &bytes.Buffer{}
	return app.Run(context.Background(), []string{"hull", "internal", "oci-hook",
		"--namespace", "hull",
		"--network", "bridge",
		"--cni-path", filepath.Join(bundle, "libexec", "cni"),
		"--cni-netconfpath", filepath.Join(bundle, "etc", "cni", "net.d"),
		"--dir", dir,
		event,
	})
}

// hookError returns the error the hook left in dir, and removes it.
func hookError(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, containerd.HookErrorFile)
	reason, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	err = os.Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(reason)
}

func TestOCIHook(t *testing.T) {
	bundle, log := fakeBridgeNetwork(t)
	dir := t.TempDir()
	state := `{"ociVersion":"1.2.0","id":"vm","status":"creating","pid":4242,"bundle":"/b"}`

	err := runHook(t, bundle, dir, "createRuntime", state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, networkResultFile))
	if err != nil || string(got) != `{"ip":"10.4.0.7","mac":"aa:bb:cc:dd:ee:ff","gateway":"10.4.0.1"}` {
		t.Errorf("%s = %s, %v", networkResultFile, got, err)
	}
	pin := filepath.Join(dir, containerd.NetnsPinFile)
	calls, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(calls), "ADD hull--vm "+pin) {
		t.Errorf("plugin calls = %q, %v; want an ADD of the pinned namespace under the namespaced ID", calls, err)
	}

	rec := &store.InstanceState{ID: "vm"}
	privateLogDirs(t)
	netDir, err := instanceNetDir("hull", "vm")
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(netDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(netDir, networkResultFile), got, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	withNetworkAddress(rec, "hull")
	if rec.IP != "10.4.0.7" || rec.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("record = %+v, want the hook's address", rec)
	}

	err = os.WriteFile(log, nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = runHook(t, bundle, dir, "postStop", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	if err != nil {
		t.Fatal(err)
	}
	calls, err = os.ReadFile(log)
	if err != nil || string(calls) != "DEL hull--vm "+pin+"\n" {
		t.Errorf("plugin calls at postStop = %q, %v; want a DEL of the pinned namespace", calls, err)
	}
	for _, name := range []string{networkResultFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after postStop: error = %v, want it removed", name, err)
		}
	}

	// A failed detach is left for hull to report, since a postStop that
	// fails makes urunc's delete fail for good, and for hull rm to retry:
	// the pin stays, and the address record gives way to detachPendingFile,
	// so hull ps no longer shows the address.
	err = runHook(t, bundle, dir, "createRuntime", state)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "1")
	err = runHook(t, bundle, dir, "postStop", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	failure, rerr := os.ReadFile(filepath.Join(dir, containerd.HookErrorFile))
	if err != nil || rerr != nil || !strings.Contains(string(failure), "lease store unreadable") {
		t.Errorf("postStop with a failing plugin = %v, %s %q, %v; want success, with the plugin's error in the file", err, containerd.HookErrorFile, failure, rerr)
	}
	for _, name := range []string{detachPendingFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s after a failed postStop detach: %v, want it there", name, err)
		}
	}
	_, err = os.Stat(filepath.Join(dir, networkResultFile))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s after a failed postStop detach: error = %v, want it removed", networkResultFile, err)
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "")

	// hull rm's retry detaches from the pin, and clears what postStop left.
	err = os.WriteFile(log, nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = runHook(t, bundle, dir, "detach", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	if err != nil {
		t.Fatal(err)
	}
	calls, err = os.ReadFile(log)
	if err != nil || string(calls) != "DEL hull--vm "+pin+"\n" {
		t.Errorf("plugin calls at the retry = %q, %v; want a DEL of the pinned namespace", calls, err)
	}
	for _, name := range []string{detachPendingFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the retry: error = %v, want it removed", name, err)
		}
	}
	err = os.Remove(filepath.Join(dir, containerd.HookErrorFile))
	if err != nil {
		t.Fatal(err)
	}

	// detach, which hull rm runs for a postStop the runtime never ran, fails
	// when the detach does, and keeps the pin and the address for the next
	// try, which detaches from the pin.
	err = runHook(t, bundle, dir, "createRuntime", state)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "1")
	err = runHook(t, bundle, dir, "detach", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	if err == nil || !strings.Contains(err.Error(), "lease store unreadable") {
		t.Errorf("detach with a failing plugin = %v, want the plugin's error", err)
	}
	if hookError(t, dir) != "" {
		t.Errorf("%s after a failed detach: want none, hull rm reads the hook's own error", containerd.HookErrorFile)
	}
	for _, name := range []string{networkResultFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s after a failed detach: %v, want it kept", name, err)
		}
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "")
	err = os.WriteFile(log, nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = runHook(t, bundle, dir, "detach", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	if err != nil {
		t.Fatal(err)
	}
	calls, err = os.ReadFile(log)
	if err != nil || string(calls) != "DEL hull--vm "+pin+"\n" {
		t.Errorf("plugin calls at detach = %q, %v; want a DEL of the pinned namespace", calls, err)
	}
	for _, name := range []string{networkResultFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after detach: error = %v, want it removed", name, err)
		}
	}

	// An attach whose address cannot be recorded is undone from the pin,
	// before the pin goes.
	err = os.Mkdir(filepath.Join(dir, networkResultFile+".tmp"), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(log, nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = runHook(t, bundle, dir, "createRuntime", state)
	if err == nil || !strings.Contains(err.Error(), "record the instance's address") {
		t.Fatalf("createRuntime that cannot record the address = %v", err)
	}
	calls, err = os.ReadFile(log)
	if err != nil || !strings.HasSuffix(string(calls), "ADD hull--vm "+pin+"\nDEL hull--vm "+pin+"\n") {
		t.Errorf("plugin calls = %q, %v; want the ADD undone by a DEL of the pinned namespace", calls, err)
	}
	_, err = os.Stat(pin)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pin after an unrecorded attach: error = %v, want it released", err)
	}
	err = os.Remove(filepath.Join(dir, networkResultFile+".tmp"))
	if err != nil {
		t.Fatal(err)
	}

	// Every createRuntime error reaches the file, not only the attach's.
	err = runHook(t, bundle, dir, "createRuntime", "not json")
	if err == nil {
		t.Fatal("createRuntime with no state succeeded")
	}
	reason := hookError(t, dir)
	if !strings.Contains(reason, "read the container state") {
		t.Errorf("%s = %q; want the state's error for hull run to report", containerd.HookErrorFile, reason)
	}

	t.Setenv("FAKE_CNI_FAIL", "1")
	err = runHook(t, bundle, dir, "createRuntime", state)
	if err == nil || !strings.Contains(err.Error(), "no address left") {
		t.Fatalf("createRuntime with a failing plugin = %v", err)
	}
	reason = hookError(t, dir)
	if !strings.Contains(reason, "no address left") {
		t.Errorf("%s = %q; want the plugin's error for hull run to report", containerd.HookErrorFile, reason)
	}
	_, err = os.Stat(pin)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pin after a failed attach: error = %v, want it released", err)
	}

	err = runHook(t, bundle, dir, "createRuntime", `{"ociVersion":"1.2.0","id":"vm","status":"creating","bundle":"/b"}`)
	if err == nil || !strings.Contains(err.Error(), "names no process") {
		t.Errorf("createRuntime with no pid = %v", err)
	}
	err = runHook(t, bundle, dir, "poststart", state)
	if err == nil || !strings.Contains(err.Error(), `unknown hook "poststart"`) {
		t.Errorf("an unknown hook = %v", err)
	}
	err = runHook(t, bundle, dir, "postStop", "not json")
	reason = hookError(t, dir)
	if err != nil || !strings.Contains(reason, "read the container state") {
		t.Errorf("postStop with no state = %v, %s %q; want success, with the error in the file", err, containerd.HookErrorFile, reason)
	}
}

func TestCNIID(t *testing.T) {
	// One '-' between the two would give both a-b-c.
	a, b := cniID("a-b", "c"), cniID("a", "b-c")
	if a == b {
		t.Errorf("namespace a-b's c and namespace a's b-c share the CNI ID %q", a)
	}
}

func TestWarnDetachFailure(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	warnDetachFailure(&out, "vm", dir, "")
	if out.Len() != 0 {
		t.Errorf("with no hook error: printed %q, want nothing", out.String())
	}
	path := filepath.Join(dir, containerd.HookErrorFile)
	err := os.WriteFile(path, []byte("detach from CNI network bridge: lease store unreadable\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	warnDetachFailure(&out, "vm", dir, "hull rm tries the detach again")
	want := "warning: the network hook could not detach instance vm: detach from CNI network bridge: lease store unreadable; " +
		"hull rm tries the detach again\n"
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
	_, err = os.Stat(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s after the warning: error = %v, want it removed, so it is reported once", containerd.HookErrorFile, err)
	}
}

// The hook runs where the runtime ran it: as it is under a rootful daemon,
// and under a rootless one through nsenter into rootlesskit's user and
// mount namespaces, and its network namespace unless rootlesskit detached
// one.
func TestDetachCommand(t *testing.T) {
	old := inUserNS
	t.Cleanup(func() { inUserNS = old })
	inUserNS = func() bool { return false }
	stateDir := t.TempDir()
	err := os.WriteFile(filepath.Join(stateDir, "child_pid"), []byte("4242\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rootful := []string{"internal", "oci-hook", "--dir", "/d", "postStop"}
	rootless := []string{"internal", "oci-hook", "--rootlesskit-state-dir", stateDir, "postStop"}

	name, args, err := detachCommand("/hull", rootful)
	if err != nil || name != "/hull" || !slices.Equal(args, rootful) {
		t.Errorf("rootful: %s %q, %v; want the hook as it is", name, args, err)
	}
	name, args, err = detachCommand("/hull", rootless)
	want := append([]string{"-U", "--preserve-credentials", "-m", "-n", "-t", "4242", "--", "/hull"}, rootless...)
	if err != nil || name != "nsenter" || !slices.Equal(args, want) {
		t.Errorf("rootless: %s %q, %v; want nsenter %q", name, args, err, want)
	}
	err = os.WriteFile(filepath.Join(stateDir, "netns"), nil, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	name, args, err = detachCommand("/hull", rootless)
	want = append([]string{"-U", "--preserve-credentials", "-m", "-t", "4242", "--", "/hull"}, rootless...)
	if err != nil || name != "nsenter" || !slices.Equal(args, want) {
		t.Errorf("rootless with a detached namespace: %s %q, %v; want nsenter %q", name, args, err, want)
	}

	inUserNS = func() bool { return true }
	name, args, err = detachCommand("/hull", rootless)
	if err != nil || name != "/hull" || !slices.Equal(args, rootless) {
		t.Errorf("already in rootlesskit's namespaces: %s %q, %v; want the hook as it is", name, args, err)
	}
	inUserNS = func() bool { return false }
	_, _, err = detachCommand("/hull", []string{"internal", "oci-hook", "--rootlesskit-state-dir", t.TempDir(), "postStop"})
	if err == nil {
		t.Error("detachCommand found the namespaces of a rootlesskit with no child_pid")
	}
}

// listenGateway stands in for a gateway hvi joins: a socket listening at
// qemuGatewaySock of the control socket it returns.
func listenGateway(t *testing.T) string {
	t.Helper()
	control := filepath.Join(t.TempDir(), "gw.sock")
	l, err := net.Listen("unix", qemuGatewaySock(control))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return control
}

func TestGatewayNetwork(t *testing.T) {
	privateLogDirs(t)
	control := listenGateway(t)

	annotations := map[string]string{}
	n, err := gatewayNetwork("shared", control, "10.87.0.10/24", "hull", "vm", annotations)
	if err != nil {
		t.Fatal(err)
	}
	wantDir, err := instanceNetDir("hull", "vm")
	if err != nil {
		t.Fatal(err)
	}
	// No hook: urunc has hvi join the gateway itself, and the guest asks
	// the gateway's resolver.
	want := &containerd.Network{Mode: "gateway", Dir: wantDir, ResolvConf: []byte("nameserver 10.87.0.1\n")}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("network = %+v, want %+v", n, want)
	}
	wantAnnotations := map[string]string{
		containerd.NetSocketAnnotation:  control + ".qemu",
		containerd.NetAddressAnnotation: "10.87.0.10/24",
		containerd.NetGatewayAnnotation: "10.87.0.1",
	}
	if !reflect.DeepEqual(annotations, wantAnnotations) {
		t.Errorf("annotations = %v, want %v", annotations, wantAnnotations)
	}

	// urunc takes only an absolute socket.
	t.Chdir(filepath.Dir(control))
	annotations = map[string]string{}
	_, err = gatewayNetwork("shared", "gw.sock", "10.87.0.10/24", "hull", "vm", annotations)
	if err != nil || annotations[containerd.NetSocketAnnotation] != control+".qemu" {
		t.Errorf("a relative socket: %q, %v; want %s", annotations[containerd.NetSocketAnnotation], err, control+".qemu")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	err = os.WriteFile(file+".qemu", nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A killed gateway leaves its socket behind, with nothing listening.
	stale := filepath.Join(dir, "stale")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale + ".qemu", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	err = l.Close()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	err = os.Symlink(control+".qemu", link+".qemu")
	if err != nil {
		t.Fatal(err)
	}
	odd := filepath.Join(dir, "a b")
	for _, tc := range []struct{ net, sock, cidr, want string }{
		{"shared", control, "", "--gateway-sock and --gateway-cidr must be used together"},
		{"shared", "", "10.87.0.10/24", "--gateway-sock and --gateway-cidr must be used together"},
		{"none", control, "10.87.0.10/24", "--net none cannot be combined with --gateway-sock"},
		{"shred", control, "10.87.0.10/24", `invalid --net "shred"`},
		{"shared", control, "10.87.0.10", `invalid --gateway-cidr "10.87.0.10"`},
		{"shared", control, "10.87.0.1/24", "is the gateway's own address"},
		{"shared", filepath.Join(dir, "gone.sock"), "10.87.0.10/24", "no gateway listens for hvi at"},
		{"shared", file, "10.87.0.10/24", "it is not a socket"},
		{"shared", stale, "10.87.0.10/24", "no gateway listens for hvi at"},
		{"shared", link, "10.87.0.10/24", "a symlink to one included"},
		{"shared", odd, "10.87.0.10/24", "urunc takes only letters"},
	} {
		annotations := map[string]string{}
		_, err := gatewayNetwork(tc.net, tc.sock, tc.cidr, "hull", "vm", annotations)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--net %s --gateway-sock %q --gateway-cidr %q: error = %v, want %q", tc.net, tc.sock, tc.cidr, err, tc.want)
		}
		if len(annotations) != 0 {
			t.Errorf("--gateway-cidr %q: annotations = %v, want none on a refusal", tc.cidr, annotations)
		}
	}
}

// gatewayAddress refuses what urunc's validateNetGateway refuses, before
// the image is pulled.
func TestGatewayAddress(t *testing.T) {
	for _, tc := range []struct{ cidr, gateway, want string }{
		{"10.87.0.10/24", "10.87.0.1", ""},
		{"10.87.0.254/24", "10.87.0.1", ""},
		{"10.0.3.7/16", "10.0.0.1", ""},
		{"192.168.1.2/30", "192.168.1.1", ""},
		{"192.168.1.10/31", "192.168.1.11", ""},
		{"10.87.0.10", "", "expected a unicast IPv4 address"},
		{"fd00::10/64", "", "expected a unicast IPv4 address"},
		{"127.0.0.10/8", "", "expected a unicast IPv4 address"},
		{"169.254.0.10/16", "", "expected a unicast IPv4 address"},
		{"224.0.0.10/24", "", "expected a unicast IPv4 address"},
		{"10.87.0.10/0", "", "expected a unicast IPv4 address"},
		{"10.87.0.255/32", "", "a /32 has no room for the gateway"},
		{"10.87.0.1/24", "", "is the gateway's own address"},
		{"192.168.1.11/31", "", "is the gateway's own address"},
		{"10.87.0.0/24", "", "is the network or broadcast address of 10.87.0.0/24"},
		{"10.87.0.255/24", "", "is the network or broadcast address of 10.87.0.0/24"},
		{"10.87.255.255/16", "", "is the network or broadcast address of 10.87.0.0/16"},
	} {
		gateway, err := gatewayAddress(tc.cidr)
		if tc.want == "" {
			if err != nil || gateway != tc.gateway {
				t.Errorf("gatewayAddress(%q) = %q, %v; want %s", tc.cidr, gateway, err, tc.gateway)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("gatewayAddress(%q): error = %v, want %q", tc.cidr, err, tc.want)
		}
	}
}

func TestCheckGatewayAnnotations(t *testing.T) {
	err := checkGatewayAnnotations(map[string]string{ociclient.HypervisorAnnotation: "hvi"})
	if err != nil {
		t.Errorf("no gateway annotation: %v", err)
	}
	for _, k := range gatewayAnnotations {
		// An empty value is refused too: urunc reads it as unset, and hull
		// writes the keys itself.
		err := checkGatewayAnnotations(map[string]string{k: ""})
		if err == nil || !strings.Contains(err.Error(), "--annotation "+k+" is refused") {
			t.Errorf("--annotation %s: error = %v, want it refused", k, err)
		}
	}
}

// A gateway run boots on hvi unless the run or the image names a monitor,
// and a monitor other than hvi is refused.
func TestGatewayMonitor(t *testing.T) {
	annotations := map[string]string{}
	defaultGatewayMonitor(annotations, nil)
	if annotations[ociclient.HypervisorAnnotation] != "hvi" {
		t.Errorf("no monitor named: %v, want hvi", annotations)
	}
	annotations = map[string]string{ociclient.HypervisorAnnotation: "cloud-hypervisor"}
	defaultGatewayMonitor(annotations, nil)
	if annotations[ociclient.HypervisorAnnotation] != "cloud-hypervisor" {
		t.Errorf("the run's monitor: %v, want it kept", annotations)
	}
	annotations = map[string]string{}
	defaultGatewayMonitor(annotations, map[string]string{ociclient.HypervisorAnnotation: "qemu"})
	if annotations[ociclient.HypervisorAnnotation] != "" {
		t.Errorf("the image's monitor: %v, want the image's left to it", annotations)
	}
	err := checkGatewayMonitor("hvi")
	if err != nil {
		t.Errorf("hvi: %v", err)
	}
	err = checkGatewayMonitor("cloud-hypervisor")
	if err == nil || !strings.Contains(err.Error(), `--gateway-sock needs the hvi monitor on Linux, and the run's is "cloud-hypervisor"`) {
		t.Errorf("cloud-hypervisor: error = %v, want it refused", err)
	}
}

func TestMayWrite(t *testing.T) {
	st := func(uid, gid, mode uint32) *syscall.Stat_t {
		return &syscall.Stat_t{Uid: uid, Gid: gid, Mode: syscall.S_IFSOCK | mode}
	}
	for _, tc := range []struct {
		name     string
		st       *syscall.Stat_t
		uid, gid uint32
		want     bool
	}{
		{"root", st(1000, 1000, 0o700), 0, 0, true},
		{"the owner", st(1000, 1000, 0o755), 1000, 1000, true},
		{"the owner without its write bit", st(1000, 1000, 0o577), 1000, 1000, false},
		{"the group", st(1000, 100, 0o775), 2000, 100, true},
		{"the group without its write bit", st(1000, 100, 0o757), 2000, 100, false},
		{"another user", st(1000, 1000, 0o775), 2000, 2000, false},
		{"another user, others may write", st(1000, 1000, 0o777), 2000, 2000, true},
		{"a subordinate ID", st(1000, 1000, 0o775), noID, noID, false},
	} {
		got := mayWrite(tc.st, tc.uid, tc.gid)
		if got != tc.want {
			t.Errorf("%s: mayWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGatewayHostIDs(t *testing.T) {
	uid, gid := gatewayHostIDs(1000, 100, false)
	if uid != 1000 || gid != 100 {
		t.Errorf("rootful: %d:%d, want the instance's own 1000:100", uid, gid)
	}
	euid, egid := uint32(os.Geteuid()), uint32(os.Getegid())
	uid, gid = gatewayHostIDs(0, 0, true)
	if uid != euid || gid != egid {
		t.Errorf("rootless root: %d:%d, want hull's own %d:%d", uid, gid, euid, egid)
	}
	uid, gid = gatewayHostIDs(1000, 0, true)
	if uid != noID || gid != egid {
		t.Errorf("rootless 1000:0: %d:%d, want a subordinate uid and hull's gid %d", uid, gid, egid)
	}
}

// A run whose image's user may not write to the socket warns that hvi,
// which joins the gateway as that user, may not reach it.
func TestWarnGatewayUser(t *testing.T) {
	control := listenGateway(t)
	sock := control + ".qemu"
	warn := func(user string, rootless bool) string {
		var out bytes.Buffer
		warnGatewayUser(&out, ocispec.ImageConfig{User: user}, sock, rootless)
		return out.String()
	}
	err := os.Chmod(sock, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"", "0", "root", "0:0"} {
		got := warn(user, false)
		if got != "" {
			t.Errorf("rootful, user %q: warned %q, want nothing for root", user, got)
		}
		// rootlesskit maps root to hull's own user, the socket's owner.
		got = warn(user, true)
		if got != "" {
			t.Errorf("rootless, user %q: warned %q, want nothing for the socket's owner", user, got)
		}
	}
	for _, rootless := range []bool{false, true} {
		got := warn("65532", rootless)
		if !strings.Contains(got, "warning: the image runs as uid 65532") || !strings.Contains(got, sock) {
			t.Errorf("rootless %v, user 65532 and a socket others cannot write to: warned %q", rootless, got)
		}
	}
	// Under a rootful daemon the image's uid is the host's, and owns the
	// socket.
	owner := strconv.Itoa(os.Geteuid())
	got := warn(owner+":65532", false)
	if os.Geteuid() != 0 && got != "" {
		t.Errorf("rootful, the socket's owner: warned %q, want nothing", got)
	}
	got = warn(owner, true)
	if os.Geteuid() != 0 && got == "" {
		t.Errorf("rootless, uid %s, a subordinate ID: no warning", owner)
	}
	err = os.Chmod(sock, 0o777)
	if err != nil {
		t.Fatal(err)
	}
	got = warn("65532", true)
	if got != "" {
		t.Errorf("a socket anyone may write to: warned %q, want nothing", got)
	}
}

// A gateway run fails when urunc started hvi without --net-gateway, as a
// urunc that ignores the gateway's annotations does, and waits for urunc to
// exec hvi first.
func TestCheckJoinedGateway(t *testing.T) {
	old, oldTimeout := procCmdline, gatewayJoinTimeout
	t.Cleanup(func() { procCmdline, gatewayJoinTimeout = old, oldTimeout })
	gatewayJoinTimeout = 200 * time.Millisecond
	urunc := []string{"urunc", "--root", "/run/urunc", "reexec"}
	joined := []string{"/opt/brig/bin/hvi", "boot", "--net-gateway", "/urunc-net/gateway.sock", "--net-mac", "16:1f:43:50:64:d3"}
	tap := []string{"/opt/brig/bin/hvi", "boot", "--net-tap", "tap0_urunc"}
	for _, tc := range []struct {
		name    string
		seen    [][]string
		err     error
		want    string
		warning string
	}{
		{name: "hvi joined", seen: [][]string{urunc, urunc, joined}},
		{name: "hvi on a tap", seen: [][]string{urunc, tap}, want: "urunc started hvi without --net-gateway"},
		// A zombie's cmdline is empty.
		{name: "the task ended", seen: [][]string{urunc, {""}}, err: fs.ErrNotExist},
		// hidepid=invisible, or a containerd in another PID namespace.
		{name: "the task never seen", err: fs.ErrNotExist, warning: "hull cannot see the task's process 4242"},
		{name: "no hvi in time", seen: [][]string{urunc}, warning: "urunc did not start hvi within"},
		{name: "no /proc access", seen: [][]string{urunc}, err: fs.ErrPermission, warning: "cannot tell whether hvi joined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			procCmdline = func(pid uint32) ([]string, error) {
				if pid != 4242 {
					t.Errorf("read the arguments of %d, want the task's, 4242", pid)
				}
				calls++
				if calls > len(tc.seen) {
					if tc.err != nil {
						return nil, tc.err
					}
					return tc.seen[len(tc.seen)-1], nil
				}
				return tc.seen[calls-1], nil
			}
			var out bytes.Buffer
			err := checkJoinedGateway(context.Background(), &out, 4242)
			if tc.want == "" && err != nil {
				t.Errorf("error = %v, want none", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
			if !strings.Contains(out.String(), tc.warning) || (tc.warning == "" && out.Len() != 0) {
				t.Errorf("warned %q, want %q", out.String(), tc.warning)
			}
		})
	}
}

// procCmdline reads a process's arguments as it was given them, the first
// included, which names hvi for urunc's monitor.
func TestProcCmdline(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(sleep, "30")
	cmd.Args = []string{"/opt/brig/bin/hvi", "30"}
	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Start returns once the child has begun to exec, which can be before
	// the kernel has set up the new program's arguments: the command line
	// reads empty until then. checkJoinedGateway polls past it the same way.
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err = procCmdline(uint32(cmd.Process.Pid))
		if err == nil && slices.Equal(got, cmd.Args) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("procCmdline = %q, %v; want %q", got, err, cmd.Args)
		}
		time.Sleep(time.Millisecond)
	}
	_, err = procCmdline(1 << 30)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a process that does not exist: error = %v, want ErrNotExist", err)
	}
}
