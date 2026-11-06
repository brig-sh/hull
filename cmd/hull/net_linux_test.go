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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/containerd"
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

	// A failed detach still releases the pin and the address, and is left
	// for hull to report: a postStop that fails makes urunc's delete fail
	// for good.
	err = runHook(t, bundle, dir, "createRuntime", state)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "1")
	err = runHook(t, bundle, dir, "postStop", `{"ociVersion":"1.2.0","id":"vm","status":"stopped","bundle":"/b"}`)
	reason := hookError(t, dir)
	if err != nil || !strings.Contains(reason, "lease store unreadable") {
		t.Errorf("postStop with a failing plugin = %v, %s %q; want success, with the plugin's error in the file", err, containerd.HookErrorFile, reason)
	}
	for _, name := range []string{networkResultFile, containerd.NetnsPinFile} {
		_, err = os.Stat(filepath.Join(dir, name))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after a failed detach: error = %v, want it removed", name, err)
		}
	}
	t.Setenv("FAKE_CNI_FAIL_DEL", "")

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
	reason = hookError(t, dir)
	if reason != "" {
		t.Errorf("%s after a failed detach = %q, want none: hull rm reads the hook's own error", containerd.HookErrorFile, reason)
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
	reason = hookError(t, dir)
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
	warnDetachFailure(&out, "vm", dir)
	if out.Len() != 0 {
		t.Errorf("with no hook error: printed %q, want nothing", out.String())
	}
	path := filepath.Join(dir, containerd.HookErrorFile)
	err := os.WriteFile(path, []byte("detach from CNI network bridge: lease store unreadable\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	warnDetachFailure(&out, "vm", dir)
	want := "warning: the network hook could not detach instance vm: detach from CNI network bridge: lease store unreadable\n"
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
