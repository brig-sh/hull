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

// Package cni attaches a Linux instance to a CNI network, from the OCI
// hooks hull run puts in the instance's spec, as nerdctl attaches a
// container. urunc gives the guest the address of the interface it finds in
// the instance's network namespace, so the attachment is the guest's
// network.
package cni

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// DefaultNetwork is the network an instance joins. It is the name of
// nerdctl's default network, which the brig-standalone-linux bundle's
// network configuration keeps, so hull's instances and nerdctl's containers
// share one bridge and one address pool.
const DefaultNetwork = "bridge"

// ifName is the interface the network gets in the instance's namespace.
// urunc takes whichever interface has an address and a default route, so
// the name only has to be the one a later Detach removes.
const ifName = "eth0"

// lockName is the file nerdctl's hook locks in the configuration directory
// around every attach and detach. Taking the same lock keeps hull and
// nerdctl from running the plugins of one network at once, which the bridge
// and firewall plugins do not expect.
const lockName = ".cni-concurrency.lock"

// Config names a network and where its plugins and configuration are.
type Config struct {
	// PluginDirs are where the plugins are looked for, in order.
	PluginDirs []string
	// ConfDir holds the network configuration files.
	ConfDir string
	// Network is the name of the network to join.
	Network string
	// CacheDir is where libcni keeps the result of each attach, which a
	// detach hands back to the plugins. Empty means libcni's own,
	// /var/lib/cni, which nerdctl's attachments use too.
	CacheDir string
}

// Result is what the network gave an instance.
type Result struct {
	IP      string `json:"ip"`
	MAC     string `json:"mac,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// Check loads the network's configuration, finds each of its plugins and
// opens the lock, so a run fails before its instance exists, and not in the
// hook. The lock is opened, creating it as the hook would, but not taken,
// so Check does not wait behind a nerdctl that holds it. hull, run as
// docs/build.md says, opens it with the hook's permissions: both are root
// on a rootful daemon, and on a rootless one hull is the user whom
// rootlesskit's user namespace maps to the hook's root.
func (c Config) Check() error {
	conf, err := c.load()
	if err != nil {
		return err
	}
	for _, p := range conf.Plugins {
		_, err := invoke.FindInPath(p.Network.Type, c.PluginDirs)
		if err != nil {
			return fmt.Errorf("CNI network %s needs the %s plugin: %w", c.Network, p.Network.Type, err)
		}
	}
	f, err := c.openLock()
	if err != nil {
		return err
	}
	return f.Close()
}

func (c Config) load() (*libcni.NetworkConfigList, error) {
	conf, err := libcni.LoadNetworkConf(c.ConfDir, c.Network)
	if err != nil {
		return nil, fmt.Errorf("load CNI network %s from %s: %w", c.Network, c.ConfDir, err)
	}
	return conf, nil
}

func (c Config) runtimeConf(id, netns string) *libcni.RuntimeConf {
	return &libcni.RuntimeConf{ContainerID: id, NetNS: netns, IfName: ifName}
}

// Attach adds the network namespace netns to the network, under the
// container ID id, and returns the address it was given.
func (c Config) Attach(ctx context.Context, id, netns string) (res Result, err error) {
	unlock, err := c.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	conf, err := c.load()
	if err != nil {
		return Result{}, err
	}
	cfg := libcni.NewCNIConfigWithCacheDir(c.PluginDirs, c.CacheDir, nil)
	defer func() {
		if err != nil {
			// Undo here, while the namespace is pinned: the hook releases
			// the pin when the attach fails, so a postStop that runs after
			// the failed create detaches without the namespace and leaves
			// the bridge plugin's masquerade rule behind.
			derr := cfg.DelNetworkList(context.WithoutCancel(ctx), conf, c.runtimeConf(id, netns))
			if derr != nil {
				err = errors.Join(err, fmt.Errorf("undo the attach: %w", derr))
			}
		}
	}()
	out, err := cfg.AddNetworkList(ctx, conf, c.runtimeConf(id, netns))
	if err != nil {
		return Result{}, fmt.Errorf("attach to CNI network %s: %w", c.Network, err)
	}
	result, err := types100.NewResultFromResult(out)
	if err != nil {
		return Result{}, fmt.Errorf("read the result of CNI network %s: %w", c.Network, err)
	}
	res, err = sandboxAddress(result)
	if err != nil {
		return Result{}, fmt.Errorf("CNI network %s: %w", c.Network, err)
	}
	return res, nil
}

// sandboxAddress returns the first IPv4 address result gives the interface
// in the instance's namespace, which is the one urunc hands the guest, with
// that interface's MAC.
func sandboxAddress(result *types100.Result) (Result, error) {
	for _, ip := range result.IPs {
		if ip.Interface == nil || *ip.Interface < 0 || *ip.Interface >= len(result.Interfaces) {
			continue
		}
		iface := result.Interfaces[*ip.Interface]
		if iface.Name != ifName || iface.Sandbox == "" || ip.Address.IP.To4() == nil {
			continue
		}
		res := Result{IP: ip.Address.IP.String(), MAC: iface.Mac}
		if ip.Gateway != nil {
			res.Gateway = ip.Gateway.String()
		}
		return res, nil
	}
	return Result{}, fmt.Errorf("%s has no IPv4 address, and urunc boots the guest only with one", ifName)
}

// Detach removes the attachment under the container ID id, from the
// network namespace netns: the instance's, pinned by PinNetns. With netns
// empty, for an instance whose namespace is gone, the plugins still
// release its address, but the bridge plugin leaves its masquerade rule.
func (c Config) Detach(ctx context.Context, id, netns string) error {
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer unlock()
	conf, err := c.load()
	if err != nil {
		return err
	}
	cfg := libcni.NewCNIConfigWithCacheDir(c.PluginDirs, c.CacheDir, nil)
	err = cfg.DelNetworkList(ctx, conf, c.runtimeConf(id, netns))
	if err != nil {
		return fmt.Errorf("detach from CNI network %s: %w", c.Network, err)
	}
	return nil
}

// openLock opens nerdctl's lock file in the configuration directory,
// creating it when there is none.
func (c Config) openLock() (*os.File, error) {
	path := filepath.Join(c.ConfDir, lockName)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the CNI lock: %w", err)
	}
	return f, nil
}

// lock takes nerdctl's lock on the configuration directory.
func (c Config) lock() (unlock func(), err error) {
	f, err := c.openLock()
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { _ = f.Close() }, nil
}
