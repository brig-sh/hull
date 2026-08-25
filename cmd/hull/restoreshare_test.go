//go:build darwin

package main

import (
	"strings"
	"testing"
)

// Restore replays the recorded command line. A share named by a descriptor
// leaves an identity path in it that nothing holds open any more, so restoring
// it would hand the guest a directory nobody vouched for.
func TestRestoreRefusesADescriptorShare(t *testing.T) {
	cmdLine := []string{
		"/usr/local/bin/vz-runner", "--kernel", "/store/kernel", "--rootfs", "/store/disk.img",
		"--share", "/.vol/16777232/267104285", "share0",
	}
	err := requireNoDescriptorShare(cmdLine)
	if err == nil {
		t.Fatal("restoring an instance whose share was named by a descriptor must be refused")
	}
	if !strings.Contains(err.Error(), "shared-dir-fd") {
		t.Errorf("error = %v, want it to name the flag that started the instance", err)
	}
}

func TestRestoreAllowsAPathShare(t *testing.T) {
	cmdLine := []string{
		"/usr/local/bin/vz-runner", "--kernel", "/store/kernel", "--rootfs", "/store/disk.img",
		"--share", "/Users/someone/workspace", "share0",
	}
	if err := requireNoDescriptorShare(cmdLine); err != nil {
		t.Errorf("a path share must still restore: %v", err)
	}
}
