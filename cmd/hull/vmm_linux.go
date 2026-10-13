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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

// vzVmm is the value urunc gives Virtualization.framework on macOS. urunc does
// not declare it on Linux, but run.go compares against it on both, so the
// Linux build carries the same string, from vzVmmName.
const vzVmm hypervisors.VmmType = vzVmmName

// newVMM builds the monitor for a Linux run. Only hvi is offered: it is the
// backend hull's Linux support is built around, and the one a sandbox moved
// from another KVM host was running (see checkLinuxRun).
//
// urunc's Linux factory is not used for it. It finds hvi only on PATH, so the
// hvi installed beside hull -- the layout make and the release archive
// produce, and the one macOS resolves first -- is not found; and its argv is
// the containerd shim's, not hull's. See hviLinuxArgv.
func newVMM(vmmType hypervisors.VmmType, monitors map[string]types.MonitorConfig) (types.VMM, error) {
	if vmmType != hypervisors.HviVmm {
		return nil, fmt.Errorf("hull on Linux runs only the hvi backend (got %q)", vmmType)
	}
	path, err := linuxHviPath(monitors)
	if err != nil {
		return nil, err
	}
	return &hviLinux{binaryPath: path}, nil
}

// setVzQMPSocket has nothing to do on Linux: there is no vz-runner.
func setVzQMPSocket(types.VMM, string) {}

// linuxHviPath resolves hvi the way urunc's darwin factory does: an explicit
// monitor config first, then the binary installed beside hull, then PATH.
// Beside hull comes before PATH so that a hull and hvi built together are run
// together, rather than this hull driving whatever older hvi is on PATH.
func linuxHviPath(monitors map[string]types.MonitorConfig) (string, error) {
	if p := monitors[string(hypervisors.HviVmm)].BinaryPath; p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), hypervisors.HviBinary)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath(hypervisors.HviBinary); err == nil {
		return p, nil
	}
	return "", errors.New("hvi not found next to hull or in PATH")
}

// hviLinux is hull's hvi monitor on Linux. Only BuildExecCmd carries any
// logic; the rest answers the urunc VMM interface the way urunc's own hvi
// monitors do.
type hviLinux struct {
	binaryPath string
}

func (h *hviLinux) BuildExecCmd(args types.ExecArgs, _ types.Unikernel) ([]string, error) {
	return hviLinuxArgv(h.binaryPath, args)
}

func (h *hviLinux) PreExec(types.ExecArgs) error { return nil }

func (h *hviLinux) Stop(pid int) error {
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

func (h *hviLinux) Signal(pid int, sig unix.Signal) error { return unix.Kill(pid, sig) }

func (h *hviLinux) Path() string { return h.binaryPath }

func (h *hviLinux) UsesKVM() bool { return true }

func (h *hviLinux) SupportsSharedfs(string) bool { return false }

func (h *hviLinux) Ok() error {
	info, err := os.Stat(h.binaryPath)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("hvi not found or not executable at %s", h.binaryPath)
	}
	return nil
}
