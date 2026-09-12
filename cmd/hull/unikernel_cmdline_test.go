package main

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"
)

// A Unikraft guest is told its address through a netdev.* library parameter,
// and its own arguments belong after the "--" separator. hull used to hand it
// a Linux command line instead, so it came up with no address, no root
// filesystem, and its arguments parsed as kernel parameters.
func TestUnikraftCommandLineCarriesTheAddressAndTheAppArguments(t *testing.T) {
	uk, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine(uk, "unikraft",
		map[string]string{
			"com.urunc.unikernel.cmdline": "-c /nginx/conf/nginx.conf",
			"com.urunc.unikernel.version": "0.18.0",
		},
		nil, "hvi", "/instance/image-initrd", "initrd",
		types.NetDevParams{IP: "10.87.0.10", Gateway: "10.87.0.1", Mask: "255.255.255.0"})
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}

	if !strings.Contains(line, "netdev.ip=10.87.0.10") {
		t.Errorf("no address in %q", line)
	}
	if !strings.Contains(line, "vfs.fstab=") {
		t.Errorf("no rootfs in %q", line)
	}
	sep := strings.Index(line, " -- ")
	if sep < 0 {
		t.Fatalf("no argument separator in %q", line)
	}
	// The guest's own arguments go after the separator, the parameters before
	// it. Getting this the wrong way round is how nginx came to be handed
	// "nginx" as an option to itself.
	if strings.Contains(line[:sep], "/nginx/conf/nginx.conf") {
		t.Errorf("app arguments landed among the kernel parameters: %q", line)
	}
	if !strings.Contains(line[sep:], "-c /nginx/conf/nginx.conf") {
		t.Errorf("app arguments missing after the separator: %q", line)
	}
}

// Without a gateway there is no address to hand over, and an empty one must
// not render as "netdev.ip=" -- that tells the guest to configure an
// interface it has not been given.
func TestUnikraftCommandLineOmitsAnEmptyAddress(t *testing.T) {
	uk, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine(uk, "unikraft",
		map[string]string{"com.urunc.unikernel.version": "0.18.0"},
		nil, "hvi", "", "", types.NetDevParams{})
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}
	if strings.Contains(line, "netdev.") {
		t.Errorf("address arguments with no address: %q", line)
	}
}
