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
	// Availability check only: unikernelCommandLine builds its own instance.
	_, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine("unikraft",
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
	// Availability check only: unikernelCommandLine builds its own instance.
	_, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine("unikraft",
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
	// Availability check only: unikernelCommandLine builds its own instance.
	_, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine("unikraft",
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

// A unikraft guest cannot be told a prefix other than /24 on the current
// argument spelling: urunc renders the address with /24 hardcoded and
// Unikraft >= 0.16.1 has no mask parameter to correct it. The pre-0.16.1
// compatibility path does render the real mask, and urunc picks between them
// on the image's version annotation, so the refusal follows that dispatch
// rather than refusing every unikraft image.
func TestUnikernelGatewayCIDRRefusesANonSlash24(t *testing.T) {
	for _, tc := range []struct {
		name, ukType, version, cidr string
		wantErr                     bool
	}{
		{"unikraft /24", "unikraft", "0.18.0", "10.87.0.10/24", false},
		{"unikraft /16", "unikraft", "0.18.0", "10.87.0.10/16", true},
		{"unikraft /25", "unikraft", "0.18.0", "10.87.0.10/25", true},
		{"unikraft, no gateway", "unikraft", "0.18.0", "", false},
		{"another family is not urunc-rendered the same way", "rumprun", "", "10.87.0.10/16", false},
		{"malformed", "unikraft", "0.18.0", "not-a-cidr", true},
		// urunc's compat path renders netdev.ipv4_subnet_mask from the real
		// mask, so a non-/24 reaches these guests correctly.
		{"pre-0.16.1 takes the compat path", "unikraft", "0.15.0", "10.87.0.10/16", false},
		{"0.16.1 itself is the current path", "unikraft", "0.16.1", "10.87.0.10/16", true},
		// An absent or unparsable version falls to the current spelling in
		// urunc, so it is refused with it rather than assumed old.
		{"no version is the current path", "unikraft", "", "10.87.0.10/16", true},
		{"unparsable version is the current path", "unikraft", "banana", "10.87.0.10/16", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unikernelGatewayCIDRSupported(tc.ukType, tc.version, tc.cidr)
			if (err != nil) != tc.wantErr {
				t.Errorf("unikernelGatewayCIDRSupported(%q, %q, %q) = %v, wantErr %v",
					tc.ukType, tc.version, tc.cidr, err, tc.wantErr)
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
	// Availability check only: unikernelCommandLine builds its own instance.
	_, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	line, err := unikernelCommandLine("unikraft",
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

// An image that carries a hypervisor or kernel label but no unikernelType is
// usually a Linux kernel: ociclient injects the linux defaults only when an
// image carries none of the three annotations, so such an image keeps the
// rumprun default. hull's own test/Dockerfile.ubuntu-simple and
// ubuntu-bootable have exactly that shape. Diverting them to a unikernel
// command line would hand a Linux kernel Rumprun's Solo5 JSON and drop its
// root=, init= and console=.
func TestUnikernelFamilyDivertsOnlyOnADeclaredFamily(t *testing.T) {
	for _, tc := range []struct {
		name          string
		annotations   map[string]string
		containerBoot bool
		wantFamily    string
		wantDivert    bool
	}{
		{
			name: "ubuntu-simple label shape is not diverted",
			annotations: map[string]string{
				"com.urunc.unikernel.kernel":     "/.boot/kernel",
				"com.urunc.unikernel.hypervisor": "qemu",
				"com.urunc.unikernel.cmdline":    "root=/dev/ram0 init=/init console=ttyS0",
			},
			wantFamily: "rumprun",
			wantDivert: false,
		},
		{
			name: "ubuntu-bootable label shape is not diverted",
			annotations: map[string]string{
				"com.urunc.unikernel.kernel":     "/.boot/kernel",
				"com.urunc.unikernel.hypervisor": "qemu",
				"com.urunc.unikernel.cmdline":    "root=/dev/nfs nfsroot=/ init=/init console=ttyS0 rw",
			},
			wantFamily: "rumprun",
			wantDivert: false,
		},
		{
			name:        "a declared unikraft image is diverted",
			annotations: map[string]string{"com.urunc.unikernel.unikernelType": "unikraft"},
			wantFamily:  "unikraft",
			wantDivert:  true,
		},
		{
			name:        "a declared linux image is not diverted",
			annotations: map[string]string{"com.urunc.unikernel.unikernelType": "linux"},
			wantFamily:  "linux",
			wantDivert:  false,
		},
		{
			name:        "an image with no urunc annotations is not diverted",
			annotations: map[string]string{},
			wantFamily:  "rumprun",
			wantDivert:  false,
		},
		{
			name:          "a host-supplied kernel is linux",
			annotations:   map[string]string{},
			containerBoot: true,
			wantFamily:    "linux",
			wantDivert:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			family, declared := unikernelFamily(tc.annotations, tc.containerBoot)
			if family != tc.wantFamily {
				t.Errorf("family = %q, want %q", family, tc.wantFamily)
			}
			if diverts := declared && family != "linux"; diverts != tc.wantDivert {
				t.Errorf("diverts = %v, want %v", diverts, tc.wantDivert)
			}
		})
	}
}

// A guest whose family will not mount what hull prepared gets no rootfs
// argument. hull cannot tell whether that guest needs one -- httpreply is a
// single binary and boots without any -- so this warns and does not refuse.
// Unikraft boots from an initrd and shares over 9pfs; it claims neither block
// nor virtiofs.
func TestUnikernelRootfsWarnsOnlyForWhatTheFamilyWillNotMount(t *testing.T) {
	uk, err := unikernels.New("unikraft")
	if err != nil {
		t.Fatalf("unikraft is not supported on this platform: %v", err)
	}
	for _, tc := range []struct {
		rootfsType string
		wantWarn   bool
	}{
		{"initrd", false},
		{"9pfs", false},
		{"", false},
		{"block", true},
		{"virtiofs", true},
	} {
		t.Run(tc.rootfsType, func(t *testing.T) {
			out := captureWarnings(t, func() {
				warnUnikernelRootfsUnusable(uk, "unikraft", tc.rootfsType)
			})
			if got := strings.Contains(out, "no rootfs argument"); got != tc.wantWarn {
				t.Errorf("warned = %v, want %v (output %q)", got, tc.wantWarn, out)
			}
		})
	}
}

// Unikraft's renderer joins the entries inside env.vars=[ ... ] unquoted, so
// a value with a space reaches the guest truncated with a stray element after
// it. No other family shares the defect: rumprun marshals each entry as JSON,
// and hermit_rs, mewz and mirage carry no environment at all, so the refusal
// is Unikraft's alone.
func TestUnikernelEnvRefusesWhitespaceForUnikraftOnly(t *testing.T) {
	if err := unikernelEnvSupported("unikraft", []string{"PATH=/usr/bin", "TZ=UTC"}); err != nil {
		t.Fatalf("plain entries: %v", err)
	}
	err := unikernelEnvSupported("unikraft", []string{"NGINX_ARGS=-c /etc/nginx.conf"})
	if err == nil {
		t.Fatal("a value with a space was accepted")
	}
	if !strings.Contains(err.Error(), "NGINX_ARGS") {
		t.Errorf("error does not name the variable: %v", err)
	}
	if !strings.Contains(err.Error(), "unikraft") {
		t.Errorf("error does not name the family it applies to: %v", err)
	}
	if err := unikernelEnvSupported("unikraft", []string{"A=b\tc"}); err == nil {
		t.Fatal("a value with a tab was accepted")
	}
	// rumprun quotes and escapes each entry, so whitespace survives.
	for _, family := range []string{"rumprun", "hermit_rs", "mewz", "mirage"} {
		if err := unikernelEnvSupported(family, []string{"NGINX_ARGS=-c /etc/nginx.conf"}); err != nil {
			t.Errorf("%s: whitespace refused although its renderer does not space-join: %v", family, err)
		}
	}
}
