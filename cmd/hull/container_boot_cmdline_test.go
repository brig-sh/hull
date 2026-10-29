//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

func hasParam(cmdline, p string) bool {
	return strings.Contains(" "+cmdline+" ", " "+p+" ")
}

// The single-binary initrd's /init takes the root the command line names and
// nothing else, so a block container boot has to name /dev/vda. Every
// container boot powers off at the end, since a reset reboots a Vz VM, and
// only hvi, where a reset ends the VM, turns a panic into one.
func TestContainerBootCmdline(t *testing.T) {
	for _, vmm := range []hypervisors.VmmType{hypervisors.HviVmm, hypervisors.VzVmm} {
		block := containerBootCmdline(true, vmm, "hvc0")
		for _, want := range []string{"rdinit=/init", "root=/dev/vda", "rootfstype=ext4", "URUNIT_POWEROFF=1", "console=hvc0"} {
			if !hasParam(block, want) {
				t.Errorf("%s block cmdline %q lacks %q", vmm, block, want)
			}
		}
		shared := containerBootCmdline(false, vmm, "hvc0")
		for _, want := range []string{"rdinit=/vz-init", "URUNIT_POWEROFF=1", "console=hvc0"} {
			if !hasParam(shared, want) {
				t.Errorf("%s virtiofs cmdline %q lacks %q", vmm, shared, want)
			}
		}
		// vz-init takes the virtiofs tag rootfs; a root= would make it refuse.
		if strings.Contains(shared, "root=") {
			t.Errorf("%s virtiofs cmdline %q names a root", vmm, shared)
		}
		panics := hasParam(block, "panic=-1") && hasParam(shared, "panic=-1")
		if (vmm == hypervisors.HviVmm) != panics {
			t.Errorf("%s: panic=-1 in %q and %q", vmm, block, shared)
		}
	}
}
