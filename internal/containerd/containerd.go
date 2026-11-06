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

// Package containerd connects hull to a containerd daemon.
//
// On Linux, hull is a containerd client: images, containers and tasks live in
// containerd, not in a store directory as on macOS. This package finds the
// daemon's socket, checks that the daemon answers, and scopes the client to
// one containerd namespace.
//
// The namespace is hull's isolation boundary on Linux, the role --store-dir
// plays on macOS. Two namespaces on one daemon see none of each other's
// images and instances, and `ctr -n <namespace>` shows what hull sees.
package containerd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/identifiers"
	securejoin "github.com/cyphar/filepath-securejoin"
)

// AddressEnv names the environment variable that overrides the socket
// address when no address is passed explicitly. It is the one nerdctl and
// ctr read, so an environment set up for them points hull at the same
// daemon.
const AddressEnv = "CONTAINERD_ADDRESS"

// NamespaceEnv names the environment variable that overrides the namespace
// when no namespace is passed explicitly, the one nerdctl and ctr read.
const NamespaceEnv = "CONTAINERD_NAMESPACE"

// DefaultNamespace is the namespace hull uses when nothing names another.
const DefaultNamespace = "hull"

// rootfulAddress is where a system containerd listens. It is also the right
// path for a rootless containerd when hull runs inside rootlesskit's
// namespaces: containerd-rootless.sh bind-mounts
// $XDG_RUNTIME_DIR/containerd over /run/containerd there.
const rootfulAddress = "/run/containerd/containerd.sock"

// probeTimeout bounds the check that the daemon answers. A local daemon
// replies in milliseconds; the bound only matters when it is wedged.
const probeTimeout = 5 * time.Second

// Overridable in tests.
var (
	geteuid = os.Geteuid
	getuid  = os.Getuid
)

// ResolveAddress picks the containerd socket address: the explicit address if
// one was given (a command-line flag), then $CONTAINERD_ADDRESS, then the
// default for the current user.
//
// The default for root is the system socket, /run/containerd/containerd.sock.
// For any other user it is the socket a rootless containerd started by
// containerd-rootless.sh exposes on the host,
// $XDG_RUNTIME_DIR/containerd/containerd.sock.
//
// A given address is read as nerdctl reads it; see inRootlessKit.
func ResolveAddress(explicit string) (string, error) {
	if explicit != "" {
		return inRootlessKit(explicit)
	}
	env := os.Getenv(AddressEnv)
	if env != "" {
		return inRootlessKit(env)
	}
	if geteuid() == 0 {
		return rootfulAddress, nil
	}
	return filepath.Join(runtimeDir(), "containerd", "containerd.sock"), nil
}

// childRoot is where hull finds the root directory of rootlesskit's child,
// the process pid. Overridable in tests.
var childRoot = func(pid int) string {
	return filepath.Join("/proc", strconv.Itoa(pid), "root")
}

// inRootlessKit returns address as a path in the mount namespace of the
// rootlesskit a rootless containerd runs under, when hull runs as a user
// with a rootlesskit state directory, and address unchanged otherwise.
//
// nerdctl, run as a user, first re-executes itself in rootlesskit's
// namespaces (rootlessutil.ParentMain), and only then reads its address.
// There, containerd-rootless.sh has mounted $XDG_RUNTIME_DIR/containerd over
// /run/containerd, so the address an environment set up for nerdctl
// carries, unix:///run/containerd/containerd.sock, is the rootless daemon's
// socket. On the host it is the system daemon's. hull stays on the host and
// opens the path through /proc/<pid>/root of rootlesskit's child. The
// kernel resolves an absolute symlink met on that path against hull's root,
// the host's, so /var/run, a link to /run, would lead back to the system
// daemon: the path is resolved inside the child's root first, as a process
// in the namespace would. A path rootlesskit's namespace does not replace,
// such as one under $XDG_RUNTIME_DIR, is the same file either way.
//
// A state directory whose child is gone is a stopped rootlesskit's, and
// nerdctl fails then. So does hull, rather than dial the address on the
// host, which names the system daemon: a user who can reach it would drive
// it in place of the rootless one.
func inRootlessKit(address string) (string, error) {
	if geteuid() == 0 {
		return address, nil
	}
	stateDir := RootlessKitStateDir()
	if stateDir == "" {
		return address, nil
	}
	path := strings.TrimPrefix(address, "unix://")
	if !filepath.IsAbs(path) {
		return address, nil
	}
	pidFile := filepath.Join(stateDir, "child_pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return "", fmt.Errorf("rootless containerd not running? %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return "", fmt.Errorf("rootless containerd not running? %s holds no process ID: %q", pidFile, data)
	}
	root := childRoot(pid)
	_, err = os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("rootless containerd not running? rootlesskit's child, process %d in %s, "+
			"is gone: %w", pid, pidFile, err)
	}
	resolved, err := securejoin.SecureJoin(root, path)
	if err != nil {
		return "", fmt.Errorf("resolve %s in rootlesskit's mount namespace: %w", path, err)
	}
	return resolved, nil
}

// runtimeDir returns the user's runtime directory: $XDG_RUNTIME_DIR, or
// /run/user/<uid> when that is unset or relative. The XDG Base Directory spec
// says to ignore a relative one. A path under it would depend on the working
// directory: the socket would be looked for wherever hull runs, and a shim,
// which does not run in hull's working directory, could not find a FIFO.
func runtimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join("/run/user", strconv.Itoa(getuid()))
	}
	return dir
}

// RootlessKitStateDir returns the state directory of the rootlesskit a
// rootless containerd runs under, for a user other than root:
// $ROOTLESSKIT_STATE_DIR, which nerdctl reads too, else the one
// containerd-rootless.sh uses by default, $XDG_RUNTIME_DIR/containerd-rootless.
// It returns "" for root, and when the directory does not exist: the daemon
// is not rootless then. A relative $ROOTLESSKIT_STATE_DIR is made absolute:
// the hook that reads the directory runs in the runtime's working
// directory, not hull's.
func RootlessKitStateDir() string {
	if geteuid() == 0 {
		return ""
	}
	dir := os.Getenv("ROOTLESSKIT_STATE_DIR")
	if dir == "" {
		dir = filepath.Join(runtimeDir(), "containerd-rootless")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	_, err = os.Stat(dir)
	if err != nil {
		return ""
	}
	return dir
}

// ResolveNamespace picks the containerd namespace: the explicit namespace if
// one was given (a command-line flag), then $CONTAINERD_NAMESPACE, then
// "hull".
func ResolveNamespace(explicit string) string {
	if explicit != "" {
		return explicit
	}
	env := os.Getenv(NamespaceEnv)
	if env != "" {
		return env
	}
	return DefaultNamespace
}

// Connect opens a client to the containerd daemon at address, scoped to
// namespace, and checks that the daemon answers. address is the socket's
// path, with or without a unix:// scheme. The caller closes the client.
//
// The client sends the namespace on every request whose context does not
// carry one already; DefaultNamespace reports it.
func Connect(ctx context.Context, address, namespace string) (*client.Client, error) {
	// Refuse a name containerd would refuse, before dialing, so a typo in
	// --namespace fails the same way whether or not the daemon is up.
	err := identifiers.Validate(namespace)
	if err != nil {
		return nil, fmt.Errorf("invalid containerd namespace: %w", err)
	}
	// containerd's dialer puts unix:// in front of the address itself, and an
	// address that already has the scheme then fails to parse.
	address = strings.TrimPrefix(address, "unix://")
	// containerd's dialer retries a missing socket until the timeout, in case
	// the daemon is still starting, and then reports only the timeout. Name
	// the missing socket at once instead. Any other stat error, such as
	// permission denied, is left for the dial to report.
	_, err = os.Stat(address)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot reach containerd: no socket at %s\n"+
			"Start containerd, or point hull at its socket with --containerd-address or %s", address, AddressEnv)
	}
	c, err := client.New(address, client.WithDefaultNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", address, err)
	}
	// client.New does not dial; the first request does. Make one now, so an
	// absent or wedged daemon fails here with the address in the message,
	// rather than in the middle of whatever the caller does next.
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, err = c.Version(probeCtx)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cannot reach containerd at %s: %w\n"+
			"Start containerd, or point hull at its socket with --containerd-address or %s", address, err, AddressEnv)
	}
	return c, nil
}
