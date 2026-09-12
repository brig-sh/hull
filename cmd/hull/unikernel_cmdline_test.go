package main

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
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
		types.NetDevParams{IP: "10.87.0.10", Gateway: "10.87.0.1", Mask: "255.255.255.0"}, nil)
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
		nil, "hvi", "", "", types.NetDevParams{}, nil)
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}
	if strings.Contains(line, "netdev.") {
		t.Errorf("address arguments with no address: %q", line)
	}
}

// What hull prepared decides what the guest is told to mount. An initrd wins
// because that is what hull writes out for an image that carries one; a share
// is 9pfs on QEMU and virtio-fs everywhere else.
func TestUnikernelRootfsTypeNamesWhatWasPrepared(t *testing.T) {
	cases := []struct {
		name              string
		initrd, disk, dir string
		vmm               hypervisors.VmmType
		want              string
	}{
		{"initrd wins", "/i", "/d", "/r", hypervisors.HviVmm, "initrd"},
		{"block disk", "", "/d", "", hypervisors.HviVmm, "block"},
		{"share on qemu is 9p", "", "", "/r", hypervisors.QemuVmm, "9pfs"},
		{"share on hvi is virtio-fs", "", "", "/r", hypervisors.HviVmm, "virtiofs"},
		{"nothing prepared", "", "", "", hypervisors.HviVmm, ""},
	}
	for _, c := range cases {
		if got := unikernelRootfsType(c.initrd, c.disk, c.dir, c.vmm); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Positional arguments after the image ref are what urunc's own path uses
// (Spec.Process.Args), so hull must agree with it or the same image behaves
// differently depending on which runtime started it. They also survive
// whitespace that the annotation cannot: the annotation is one string, and
// splitting and rejoining it collapses runs of spaces inside an argument.
func TestUnikraftCommandLinePrefersPositionalArguments(t *testing.T) {
	uk, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine(uk, "unikraft",
		map[string]string{
			"com.urunc.unikernel.cmdline": "from-the-annotation",
			"com.urunc.unikernel.version": "0.18.0",
		},
		nil, "hvi", "", "", types.NetDevParams{},
		[]string{"from-the-command-line", "-c", "/etc/x.conf"})
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}
	if strings.Contains(line, "from-the-annotation") {
		t.Errorf("the annotation beat the positional arguments: %q", line)
	}
	sep := strings.Index(line, " -- ")
	if sep < 0 {
		t.Fatalf("no argument separator in %q", line)
	}
	if got, want := line[sep+4:], "from-the-command-line -c /etc/x.conf"; got != want {
		t.Errorf("application arguments = %q, want %q", got, want)
	}
}

// A unikraft guest cannot be told a prefix other than /24: urunc renders the
// address with /24 hardcoded and Unikraft has no mask parameter to correct
// it, so anything else would boot with a silently wrong on-link range.
func TestUnikernelGatewayCIDRRefusesANonSlash24(t *testing.T) {
	for _, tc := range []struct {
		name, ukType, cidr string
		wantErr            bool
	}{
		{"unikraft /24", "unikraft", "10.87.0.10/24", false},
		{"unikraft /16", "unikraft", "10.87.0.10/16", true},
		{"unikraft /25", "unikraft", "10.87.0.10/25", true},
		{"unikraft, no gateway", "unikraft", "", false},
		{"another family is not urunc-rendered the same way", "rumprun", "10.87.0.10/16", false},
		{"malformed", "unikraft", "not-a-cidr", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unikernelGatewayCIDRSupported(tc.ukType, tc.cidr)
			if (err != nil) != tc.wantErr {
				t.Errorf("unikernelGatewayCIDRSupported(%q, %q) = %v, wantErr %v",
					tc.ukType, tc.cidr, err, tc.wantErr)
			}
		})
	}
}

// hull defaults Process.Args to /bin/sh for an image with no Cmd, so "the
// user gave arguments" cannot be inferred from it. Reading it as an override
// hands a unikernel /bin/sh as its argv: measured through `hull run` against
// the published nginx image, the guest printed `nginx: invalid option:
// "/bin/sh"` and powered off. Only a real override may displace the image's
// cmdline annotation.
func TestUnikraftCommandLineIgnoresADefaultedShell(t *testing.T) {
	uk, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine(uk, "unikraft",
		map[string]string{
			"com.urunc.unikernel.cmdline": "-c /nginx/conf/nginx.conf",
			"com.urunc.unikernel.version": "0.18.0",
		},
		nil, "hvi", "/instance/image-initrd", "initrd", types.NetDevParams{},
		nil) // no user override: Process.Args's /bin/sh must not reach here
	if err != nil {
		t.Fatalf("building the command line: %v", err)
	}
	if strings.Contains(line, "/bin/sh") {
		t.Errorf("a defaulted shell reached the guest argv: %q", line)
	}
	sep := strings.Index(line, " -- ")
	if sep < 0 {
		t.Fatalf("no argument separator in %q", line)
	}
	if got, want := line[sep+4:], "-c /nginx/conf/nginx.conf"; got != want {
		t.Errorf("application arguments = %q, want %q", got, want)
	}
}
