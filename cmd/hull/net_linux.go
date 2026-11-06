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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/moby/sys/userns"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/internal/cni"
	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/store"
)

// networkResultFile is the file the hook keeps in an instance's network
// directory while the instance is attached: the address, in cni.Result's
// JSON.
const networkResultFile = "network.json"

// The places CNI looks for plugins and network configuration when neither
// the environment nor an installed bundle names others. Variables, so tests
// can point them at directories of their own.
var (
	defaultCNIPath    = "/opt/cni/bin"
	defaultNetConfDir = "/etc/cni/net.d"
)

// bundleDirs lists where a bundle may be installed. A variable, so tests
// can stand in for a host whose system bundle they cannot control.
var bundleDirs = bootassets.BundleDirs

// runNetwork returns how a run of instance id in namespace is attached to
// the network --net mode names: nil for none. shared attaches it to
// cni.DefaultNetwork, through the hook that `hull internal oci-hook` runs.
func runNetwork(ctx context.Context, mode, namespace, id string) (*containerd.Network, error) {
	switch mode {
	case "none":
		return nil, nil
	case "shared":
	default:
		return nil, fmt.Errorf("invalid --net %q, expected none or shared", mode)
	}
	cfg, err := cniConfig()
	if err != nil {
		return nil, err
	}
	err = cfg.Check()
	if err != nil {
		return nil, err
	}
	stateDir := containerd.RootlessKitStateDir()
	var rootlessDNS []string
	if stateDir != "" {
		rootlessDNS, err = cni.RootlessKitDNS(ctx, stateDir)
		if errors.Is(err, cni.ErrRootlessKitNotRunning) {
			// The state directory is a stopped rootlesskit's, so no
			// rootless daemon runs under it: the hook stays in the
			// daemon's network namespace, and the guest gets the host's
			// nameservers. With hull's default address the run then
			// fails when it dials that rootless daemon, an error about
			// the daemon rather than about rootlesskit's DNS.
			stateDir = ""
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	resolv, err := guestResolvConf(rootlessDNS)
	if err != nil {
		return nil, err
	}
	dir, err := instanceNetDir(namespace, id)
	if err != nil {
		return nil, err
	}
	// The runtime runs the hook from its own working directory and inside
	// a rootless daemon's mount namespace, where the binary is found by
	// its absolute path, not hull's PATH.
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find hull's executable for the network hook: %w", err)
	}
	args := []string{exe, "internal", "oci-hook",
		"--namespace", namespace,
		"--network", cfg.Network,
		"--cni-netconfpath", cfg.ConfDir,
		"--dir", dir,
	}
	for _, p := range cfg.PluginDirs {
		args = append(args, "--cni-path", p)
	}
	if stateDir != "" {
		args = append(args, "--rootlesskit-state-dir", stateDir)
	}
	return &containerd.Network{
		Hook: specs.Hook{
			Path: exe,
			Args: args,
			// The runtime gives a hook only the environment the spec names.
			// The bridge and firewall plugins run iptables, which a PATH
			// without the sbin directories does not find.
			Env: []string{"PATH=" + hookPath(os.Getenv("PATH"))},
		},
		Dir:        dir,
		ResolvConf: resolv,
	}, nil
}

// hookPath is the PATH the network hook runs with: path, or a default when
// it is empty, with /usr/sbin and /sbin after it, as nerdctl gives its hook.
func hookPath(path string) string {
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return path + ":/usr/sbin:/sbin"
}

// cniConfig finds the plugins and the configuration of the network --net
// shared joins: $CNI_PATH and $NETCONFPATH, the variables nerdctl and the
// CNI tools read, then the installed brig-standalone-linux bundle's, then
// CNI's own /opt/cni/bin and /etc/cni/net.d. The bundle's plugins are in its
// libexec/cni. Its configuration is the copy rootlessBrigNetConfDir names,
// for a user who has one, else the bundle's etc/cni/net.d. A relative path
// is made absolute: the hook runs in the runtime's working directory, not
// hull's.
func cniConfig() (cni.Config, error) {
	pluginDirs := []string{defaultCNIPath}
	confDir := defaultNetConfDir
	for _, dir := range bundleDirs() {
		conf := filepath.Join(dir, "etc", "cni", "net.d")
		_, err := os.Stat(conf)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cni.Config{}, err
		}
		pluginDirs = []string{filepath.Join(dir, "libexec", "cni")}
		confDir = conf
		break
	}
	userConf := rootlessBrigNetConfDir()
	if userConf != "" {
		_, err := os.Stat(userConf)
		if err == nil {
			confDir = userConf
		} else if !errors.Is(err, os.ErrNotExist) {
			return cni.Config{}, err
		}
	}
	env := os.Getenv("CNI_PATH")
	if env != "" {
		pluginDirs = filepath.SplitList(env)
	}
	env = os.Getenv("NETCONFPATH")
	if env != "" {
		confDir = env
	}
	for i, dir := range pluginDirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return cni.Config{}, fmt.Errorf("CNI_PATH: %w", err)
		}
		pluginDirs[i] = abs
	}
	abs, err := filepath.Abs(confDir)
	if err != nil {
		return cni.Config{}, fmt.Errorf("NETCONFPATH: %w", err)
	}
	return cni.Config{PluginDirs: pluginDirs, ConfDir: abs, Network: cni.DefaultNetwork}, nil
}

// rootlessBrigNetConfDir returns where brig-rootless-setup.sh puts the
// network configuration of a user's rootless brig:
// $XDG_CONFIG_HOME/brig/cni/net.d, or ~/.config/brig/cni/net.d. It is a
// copy of the bundle's etc/cni/net.d, and the nerdctl that script sets up
// reads it. hull reads it too, so the two take the same lock, the one
// cni.Config takes in that directory. It returns "" for root, whose system
// install's nerdctl reads the bundle's own directory.
func rootlessBrigNetConfDir() string {
	if os.Geteuid() == 0 {
		return ""
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "brig", "cni", "net.d")
}

// guestResolvConf returns the /etc/resolv.conf an attached instance gets.
// Under a rootless daemon, rootlesskit's own nameservers, rootlessDNS, come
// first: they reach the host's resolver even when that listens on the
// host's loopback only.
func guestResolvConf(rootlessDNS []string) ([]byte, error) {
	host, err := cni.HostResolvConf()
	if err != nil {
		return nil, err
	}
	return cni.ResolvConf(host, rootlessDNS), nil
}

// instanceNetDir returns the directory that holds the network files of
// instance id in namespace.
func instanceNetDir(namespace, id string) (string, error) {
	logFile, err := instanceLogPath(namespace, id)
	if err != nil {
		return "", err
	}
	return netDirOf(logFile), nil
}

// netDirOf returns the network directory of the instance whose log is
// logFile: beside the log, named after it, so rm, which locks the log's
// directory, removes both under one lock.
func netDirOf(logFile string) string {
	return strings.TrimSuffix(logFile, ".log") + ".net"
}

// warnDetachFailure prints to w the error the network hook left in dir,
// instance id's network directory, when it could not detach the instance at
// postStop, and removes it, so it is reported once. The hook does not fail
// then, and urunc drops the output of a hook that succeeds, so the file is
// the only place the error is kept.
func warnDetachFailure(w io.Writer, id, dir string) {
	path := filepath.Join(dir, containerd.HookErrorFile)
	reason, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "warning: the network hook could not detach instance %s: %s\n", id, strings.TrimSpace(string(reason)))
	_ = os.Remove(path)
}

// withNetworkAddress fills in rec's address from the result the network
// hook keeps for the instance, when there is one. An instance with no
// network, or one detached, keeps none.
func withNetworkAddress(rec *store.InstanceState, namespace string) {
	dir, err := instanceNetDir(namespace, rec.ID)
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, networkResultFile))
	if err != nil {
		return
	}
	var res cni.Result
	err = json.Unmarshal(data, &res)
	if err != nil {
		return
	}
	rec.IP = res.IP
	rec.MAC = res.MAC
}

// leftAttachment is the detach an instance's postStop hook may not have
// run: the hook as the container's spec names it, and the state the
// runtime would have given it.
type leftAttachment struct {
	hook  specs.Hook
	state specs.State
}

// attachmentOf returns what detachLeft needs to detach ctr, whose network
// directory is dir, when its hook still records an attachment there, and
// nil when it records none or the spec names no network hook. Once ctr's
// task is gone, a record left means the hook never ran at postStop, which
// removes it whether or not its detach works.
func attachmentOf(ctx context.Context, ctr client.Container, dir string) (*leftAttachment, error) {
	_, err := os.Stat(filepath.Join(dir, networkResultFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the spec of container %s: %w", ctr.ID(), err)
	}
	if spec.Hooks == nil {
		return nil, nil
	}
	for _, h := range spec.Hooks.Poststop {
		if len(h.Args) >= 4 && h.Args[1] == "internal" && h.Args[2] == "oci-hook" && h.Args[len(h.Args)-1] == "postStop" {
			return &leftAttachment{
				hook:  h,
				state: specs.State{Version: spec.Version, ID: ctr.ID(), Status: specs.StateStopped},
			}, nil
		}
	}
	return nil, nil
}

// hookExecutable is the hull binary detachLeft runs the hook with. A
// variable, so tests can stand in for the hook.
var hookExecutable = os.Executable

// inUserNS reports whether hull runs in a user namespace, as the hook does
// under a rootless daemon. A variable, so tests can take either side.
var inUserNS = userns.RunningInUserNS

// detachLeft runs the postStop hook of a, which the runtime never ran, as
// detach: as postStop, except that a detach that fails keeps the hook's
// record and the pin and fails the hook, so hull rm can keep the instance
// and run it again. A task that is gone without being deleted, as after a
// host reboot, or after its shim was killed because urunc could not delete
// it, never ran its hook. A rootful daemon's host-local plugin keeps its
// leases on disk, so the address would stay leased for good, and a
// container ID is never used again for a later attach to free it.
//
// The hook runs from this hull binary, with the arguments the spec gives
// it: the binary that ran the instance may be the one that is gone. Under a
// rootless daemon it runs, as the runtime would have run it, in
// rootlesskit's user and mount namespaces, where the plugins keep their
// state, and in rootlesskit's network namespace too unless rootlesskit
// detached one, which the hook enters itself. hull enters them with
// nsenter, as containerd-rootless-setuptool.sh does: a process that runs
// more than one thread, as every Go program does, cannot join a user
// namespace.
func detachLeft(ctx context.Context, a *leftAttachment) error {
	exe, err := hookExecutable()
	if err != nil {
		return fmt.Errorf("find hull's executable: %w", err)
	}
	args := slices.Clone(a.hook.Args[1:])
	args[len(args)-1] = "detach"
	name, args, err := detachCommand(exe, args)
	if err != nil {
		return err
	}
	state, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = a.hook.Env
	cmd.Stdin = bytes.NewReader(state)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil && output != "" {
		return fmt.Errorf("run the network hook: %w: %s", err, output)
	}
	if err != nil {
		return fmt.Errorf("run the network hook: %w", err)
	}
	return nil
}

// detachCommand returns the command line that runs exe with the hook
// arguments args where the runtime would have run the hook; see detachLeft.
func detachCommand(exe string, args []string) (string, []string, error) {
	i := slices.Index(args, "--rootlesskit-state-dir")
	if i < 0 || i+1 >= len(args) || inUserNS() {
		return exe, args, nil
	}
	stateDir := args[i+1]
	pid, err := os.ReadFile(filepath.Join(stateDir, "child_pid"))
	if err != nil {
		return "", nil, fmt.Errorf("find rootlesskit's namespaces: %w", err)
	}
	enter := []string{"-U", "--preserve-credentials", "-m"}
	_, err = os.Stat(filepath.Join(stateDir, "netns"))
	if errors.Is(err, os.ErrNotExist) {
		enter = append(enter, "-n")
	}
	enter = append(enter, "-t", strings.TrimSpace(string(pid)), "--", exe)
	return "nsenter", append(enter, args...), nil
}
