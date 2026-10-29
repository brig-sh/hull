//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"
)

func hasParam(cmdline, p string) bool {
	return strings.Contains(" "+cmdline+" ", " "+p+" ")
}

// urunc builds a container boot's command line; these are the parts hull
// relies on. The single-binary initrd's /init takes the root the line names,
// /vz-init its virtiofs tag, so only a block boot names one. Vz reboots on a
// reset, so its guests power off at the end and only stop on a panic.
func TestContainerBootCmdline(t *testing.T) {
	cmd := []string{"/bin/sh", "-c", "echo hi"}
	for _, vmm := range []hypervisors.VmmType{hypervisors.HviVmm, hypervisors.VzVmm} {
		block, err := containerBootCmdline(vmm, true, unikernels.LinuxNet{DHCP: true}, cmd)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"rdinit=/init", "root=/dev/vda", "rootfstype=ext4", "ip=dhcp"} {
			if !hasParam(block, want) {
				t.Errorf("%s block cmdline %q lacks %q", vmm, block, want)
			}
		}
		shared, err := containerBootCmdline(vmm, false, unikernels.LinuxNet{}, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if !hasParam(shared, "rdinit=/vz-init") || strings.Contains(shared, "root=") {
			t.Errorf("%s virtiofs cmdline %q: want rdinit=/vz-init and no root=", vmm, shared)
		}
		for _, c := range []string{block, shared} {
			if strings.Contains(c, " -- ") || strings.Contains(c, "echo hi") {
				t.Errorf("%s cmdline %q carries the command, which travels in /urunc-cmd", vmm, c)
			}
			vz := vmm == hypervisors.VzVmm
			if vz != hasParam(c, "URUNIT_POWEROFF=1") || vz == hasParam(c, "panic=-1") {
				t.Errorf("%s cmdline %q: want URUNIT_POWEROFF=1 and no panic=-1 on vz only", vmm, c)
			}
		}
	}
}
