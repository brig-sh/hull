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
// reset, so its guests power off at the end and only stop on a panic. hvi
// ends the run on a reset, so panic=-1 stops a panicked guest there too.
func TestContainerBootCmdline(t *testing.T) {
	cmd := []string{"/bin/sh", "-c", "echo hi"}
	// hull's own consoleDev picked these before urunc built the line. A
	// change here silences the guest console.
	consoles := map[hypervisors.VmmType][]string{
		hypervisors.HviVmm: {"console=ttyAMA0"},
		hypervisors.VzVmm:  {"console=hvc0", "8250.nr_uarts=0"},
	}
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
		// A static address on the gateway's default subnet, 10.87.0.0/24, as a
		// `hull run` with a network passes it.
		static, err := containerBootCmdline(vmm, false, unikernels.LinuxNet{
			Address: "10.87.0.2", Gateway: "10.87.0.1", Mask: "255.255.255.0", DNS: "10.87.0.1",
		}, cmd)
		if err != nil {
			t.Fatal(err)
		}
		const ip = "ip=10.87.0.2::10.87.0.1:255.255.255.0:urunc:eth0:off:10.87.0.1"
		if !hasParam(static, ip) || hasParam(static, "URUNIT_DEFROUTE=1") {
			t.Errorf("%s static cmdline %q: want %q and no URUNIT_DEFROUTE=1", vmm, static, ip)
		}
		for _, c := range []string{block, shared, static} {
			if strings.Contains(c, " -- ") || strings.Contains(c, "echo hi") {
				t.Errorf("%s cmdline %q carries the command, which travels in /urunc-cmd", vmm, c)
			}
			for _, want := range consoles[vmm] {
				if !hasParam(c, want) {
					t.Errorf("%s cmdline %q lacks %q", vmm, c, want)
				}
			}
			vz := vmm == hypervisors.VzVmm
			if vz != hasParam(c, "URUNIT_POWEROFF=1") || vz == hasParam(c, "panic=-1") {
				t.Errorf("%s cmdline %q: want URUNIT_POWEROFF=1 and no panic=-1 on vz only", vmm, c)
			}
		}
	}
}
