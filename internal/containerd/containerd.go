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
)

// AddressEnv names the environment variable that overrides the socket
// address when no address is passed explicitly.
const AddressEnv = "HULL_CONTAINERD_ADDRESS"

// NamespaceEnv names the environment variable that overrides the namespace
// when no namespace is passed explicitly.
const NamespaceEnv = "HULL_NAMESPACE"

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
// one was given (a command-line flag), then $HULL_CONTAINERD_ADDRESS, then
// the default for the current user.
//
// The default for root is the system socket, /run/containerd/containerd.sock.
// For any other user it is the socket a rootless containerd started by
// containerd-rootless.sh exposes on the host,
// $XDG_RUNTIME_DIR/containerd/containerd.sock.
func ResolveAddress(explicit string) string {
	if explicit != "" {
		return explicit
	}
	env := os.Getenv(AddressEnv)
	if env != "" {
		return env
	}
	if geteuid() == 0 {
		return rootfulAddress
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(getuid()))
	}
	return filepath.Join(runtimeDir, "containerd", "containerd.sock")
}

// ResolveNamespace picks the containerd namespace: the explicit namespace if
// one was given (a command-line flag), then $HULL_NAMESPACE, then "hull".
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
