// Copyright (c) 2023-2026, Nubificus LTD
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

//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/qmp"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/initrd"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"
	"golang.org/x/sys/unix"

	"encoding/base64"
)

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "create and run a unikernel",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "detach,d",
				Usage: "run in background",
			},
			&cli.StringFlag{
				Name:  "net",
				Value: "none",
				Usage: "network mode: 'none' (default) or 'shared'",
			},
			&cli.StringFlag{
				Name:  "pull",
				Value: pullMissing,
				Usage: "pull policy: 'missing' (default), 'always' or 'never'. " +
					"A cached tag is not re-resolved, so use 'always' to pick up a republished tag",
			},
			&cli.IntFlag{
				Name:  "mem",
				Value: 512,
				Usage: "memory size in MB",
			},
			&cli.IntFlag{
				Name:  "cpus",
				Value: 1,
				Usage: "number of CPUs",
			},
			&cli.StringFlag{
				Name:  "name",
				Value: "",
				Usage: "instance name (default: auto-generated)",
			},
			&cli.StringSliceFlag{
				Name:  "shared-dir",
				Usage: "share a host directory with the guest: /host/path:/guest/path[:ro|rw] (repeatable)",
			},
			&cli.StringFlag{
				Name:  "hypervisor",
				Value: "",
				Usage: "override hypervisor backend: 'qemu', 'vz', or 'hvi' (default: from image annotation)",
			},
			&cli.StringFlag{
				Name:  "qemu-path",
				Value: "",
				Usage: "path to qemu-system-aarch64 binary (default: auto-detect, prefers signed copy)",
			},
			&cli.StringFlag{
				Name:  "rootfs-type",
				Value: "",
				Usage: "rootfs sharing mode: 'block' (ext4 disk image), 'virtiofs' (Vz default), '9pfs' (QEMU default)",
			},
			&cli.StringSliceFlag{
				Name:  "annotation",
				Usage: "set an OCI runtime annotation: KEY=VALUE (repeatable)",
			},
			&cli.BoolFlag{
				Name:  "no-boot-assets",
				Usage: "do not fall back to the published boot assets when the image carries no kernel",
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "set an environment variable in the guest: KEY=VALUE, or a bare KEY to inherit it from the host environment and keep the value out of argv (repeatable)",
			},
			&cli.StringSliceFlag{
				Name:  "add-host",
				Usage: "add an entry to the guest's /etc/hosts (host:ip, repeatable)",
			},
			&cli.IntFlag{
				Name:  "stop-grace",
				Value: 10,
				Usage: "seconds vz-runner waits for the guest to answer a stop request before forcing (docker stop_grace_period)",
			},
			&cli.BoolFlag{
				Name:  "wait-ip",
				Usage: "with --detach and NAT networking, wait for the DHCP lease and record the IP before returning",
			},
			&cli.StringFlag{
				Name:  "gateway-sock",
				Usage: "join the user-mode network gateway at this control socket (Vz, HVI, or QEMU)",
			},
			&cli.StringFlag{
				Name:  "gateway-cidr",
				Usage: "static guest CIDR on the gateway subnet, e.g. 10.87.0.10/24 (requires --gateway-sock)",
			},
			&cli.BoolFlag{
				Name:  "gui",
				Usage: "open a graphical window for the instance (Vz only)",
			},
			&cli.StringFlag{
				Name:  "gui-title",
				Usage: "title for the GUI window (requires --gui)",
			},
			&cli.BoolFlag{
				Name:  "rosetta",
				Usage: "run an amd64 rootfs under Rosetta translation (Vz only; the kernel stays arm64)",
			},
			&cli.StringFlag{
				Name:  "platform",
				Value: ociclient.DefaultPlatform,
				Usage: "image platform to pull, e.g. linux/amd64 for the Rosetta path",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runInstance(ctx, cmd)
		},
	}
}

func runInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("image reference required")
	}

	imageRef := args.First()
	detach := cmd.Bool("detach")
	netMode := cmd.String("net")
	pullPolicy := cmd.String("pull")
	memMB := cmd.Int("mem")
	cpus := cmd.Int("cpus")
	instanceName := cmd.String("name")
	sharedDirs := cmd.StringSlice("shared-dir")
	hypervisorOverride := cmd.String("hypervisor")
	qemuPath := cmd.String("qemu-path")
	rootfsTypeOverride := cmd.String("rootfs-type")
	annotationOverrides, err := parseAnnotationEntries(cmd.StringSlice("annotation"))
	if err != nil {
		return err
	}
	envOverrides, err := resolveEnvEntries(cmd.StringSlice("env"), os.LookupEnv)
	if err != nil {
		return err
	}
	addHosts := cmd.StringSlice("add-host")
	gatewaySock := cmd.String("gateway-sock")
	gatewayIP := cmd.String("gateway-cidr")
	if (gatewaySock == "") != (gatewayIP == "") {
		return errors.New("--gateway-sock and --gateway-cidr must be used together")
	}
	// Two contradictory instructions about the same NIC. Whichever one gets
	// silently dropped, somebody gets a sandbox with different connectivity
	// than they asked for, and `--net none` is the one it is dangerous to lose
	// -- so neither is guessed at.
	if netMode == "none" && gatewaySock != "" {
		return errors.New("--net none cannot be combined with --gateway-sock: one asks for no " +
			"network device at all and the other attaches the guest to the gateway")
	}
	if !validPullPolicy(pullPolicy) {
		return fmt.Errorf("invalid --pull %q, expected missing, always or never", pullPolicy)
	}
	gui := cmd.Bool("gui")
	guiTitle := cmd.String("gui-title")
	platform := cmd.String("platform")
	// --rosetta exists to run amd64 rootfses. When the flag is given and no
	// platform is asked for explicitly, pulling the native arm64 variant
	// would only fail later at the busybox check, with a message that never
	// mentions the real omission -- so default the pull to linux/amd64.
	// An explicit --platform still wins. Only the flag can imply this: the
	// annotation form lives inside the image, which is not pulled yet.
	if cmd.Bool("rosetta") && !cmd.IsSet("platform") {
		platform = "linux/amd64"
	}
	if guiTitle != "" && !gui {
		return errors.New("--gui-title requires --gui")
	}

	// Generate instance ID if not provided
	if instanceName == "" {
		instanceName = generateID(8)
	}
	// The name becomes a directory under the store. Reject a traversing one
	// here so the failure names the flag, rather than surfacing from the
	// store after the image has already been pulled and unpacked.
	if err := store.ValidateInstanceID(instanceName); err != nil {
		return fmt.Errorf("--name: %w", err)
	}

	// Load or create store
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	// Create OCI client
	client := ociclient.New(s)

	log.Debugf("Running instance %s from image %s", instanceName, imageRef)

	// Create instance directory. Until the VMM is started, any failure
	// removes it again — otherwise a failed run permanently squats the
	// instance name (duplicate names are an error).
	instanceDir, err := s.CreateInstance(instanceName)
	if err != nil {
		return err
	}
	instanceStarted := false
	defer func() {
		if !instanceStarted {
			if rmErr := os.RemoveAll(instanceDir); rmErr != nil {
				log.WithError(rmErr).Warnf("failed to clean up instance dir %s", instanceDir)
			}
		}
	}()

	bundleDir := s.InstanceBundleDir(instanceName)
	logFile := s.InstanceLogFile(instanceName)
	qmpSocket := s.InstanceQMPSocket(instanceName)

	// Track image digest (will be set based on whether we use local bundle or pull from registry)
	var imageDigest string

	// Check if imageRef is a local bundle path (for testing)
	if info, err := os.Stat(imageRef); err == nil && info.IsDir() {
		// It's a local directory - check if it has config.json (OCI bundle)
		configPath := filepath.Join(imageRef, "config.json")
		if _, err := os.Stat(configPath); err == nil {
			// It's a valid OCI bundle - copy it to the instance directory
			log.Debugf("Using local bundle: %s", imageRef)

			// Copy entire bundle to instance directory using tar for robustness
			srcPath := filepath.Clean(imageRef)
			tarCmd := exec.Command("tar", "-C", srcPath, "-cf", "-", ".")
			untarCmd := exec.Command("tar", "-C", bundleDir, "-xf", "-")

			pipe, err := tarCmd.StdoutPipe()
			if err != nil {
				return fmt.Errorf("failed to create tar pipe: %w", err)
			}

			untarCmd.Stdin = pipe
			if err := tarCmd.Start(); err != nil {
				return fmt.Errorf("failed to start tar: %w", err)
			}

			if err := untarCmd.Run(); err != nil {
				return fmt.Errorf("failed to untar bundle: %w", err)
			}

			if err := tarCmd.Wait(); err != nil {
				return fmt.Errorf("tar failed: %w", err)
			}

			log.Debugf("Bundle copied to: %s", bundleDir)

			// Use imageRef as digest for local bundles
			imageDigest = "local:" + imageRef
		} else {
			// Not a bundle, treat as image reference
			var err error
			imageDigest, err = resolveImageDigest(ctx, client, s, imageRef, pullPolicy, platform)
			if err != nil {
				return err
			}

			// Load image config and generate bundle
			imgConfig, err := client.LoadImageConfig(imageDigest)
			if err != nil {
				return err
			}

			if err := client.GenerateBundle(bundleDir, imageDigest, imgConfig); err != nil {
				return err
			}
		}
	} else {
		// Not a local path, treat as image reference
		var err error
		imageDigest, err = resolveImageDigest(ctx, client, s, imageRef, pullPolicy, platform)
		if err != nil {
			return err
		}

		// Load image config and generate bundle
		imgConfig, err := client.LoadImageConfig(imageDigest)
		if err != nil {
			return err
		}

		if err := client.GenerateBundle(bundleDir, imageDigest, imgConfig); err != nil {
			return err
		}
	}

	// Load OCI spec from bundle
	specPath := filepath.Join(bundleDir, "config.json")
	specData, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("failed to read OCI spec: %w", err)
	}

	var ociSpec struct {
		Annotations map[string]string `json:"annotations"`
		Hostname    string            `json:"hostname"`
		Process     struct {
			Args []string `json:"args"`
			Env  []string `json:"env"`
			Cwd  string   `json:"cwd"`
			User struct {
				UID uint32 `json:"uid"`
				GID uint32 `json:"gid"`
			} `json:"user"`
		} `json:"process"`
	}
	if err := json.Unmarshal(specData, &ociSpec); err != nil {
		return fmt.Errorf("failed to parse OCI spec: %w", err)
	}

	if ociSpec.Annotations == nil {
		ociSpec.Annotations = make(map[string]string)
	}
	// Always try loading urunc.json — bunny stores config there with base64 values.
	// urunc.json values override spec annotations (which may be defaults).
	// Read through the rootfs rather than joined onto it. This file is image
	// content by definition, and an image that ships it as a symlink to an
	// absolute host path would otherwise have hull parse a host file as its own
	// configuration. A missing or unparseable urunc.json stays what it always
	// was -- ordinary, and silently skipped.
	uruncJSON, uruncErr := readFromRootfs(filepath.Join(bundleDir, "rootfs"), "urunc.json")
	if errors.Is(uruncErr, errImagePlantedPath) {
		return uruncErr
	}
	if data := uruncJSON; uruncErr == nil {
		var jsonAnnot map[string]string
		if err := json.Unmarshal(data, &jsonAnnot); err == nil {
			for k, v := range jsonAnnot {
				decoded, err := base64.StdEncoding.DecodeString(v)
				if err == nil {
					ociSpec.Annotations[k] = string(decoded)
				} else {
					ociSpec.Annotations[k] = v
				}
			}
			log.Debugf("Loaded annotations from urunc.json: %v", ociSpec.Annotations)
		}
	}
	// Explicit runtime annotations win over image metadata. This is how a stock
	// image receives host boot artifacts without being rebuilt or relabelled.
	for k, v := range annotationOverrides {
		ociSpec.Annotations[k] = v
	}

	// CLI env overrides: appended after the image env so they win (urunit
	// applies entries in order).
	if len(envOverrides) > 0 {
		ociSpec.Process.Env = append(ociSpec.Process.Env, envOverrides...)
	}

	// Positional command override (docker semantics): everything after the
	// image ref replaces the image Cmd. Bunny images run urunit as the
	// entrypoint (args[0]); keep it and replace only the user command.
	if args.Len() > 1 {
		override := args.Slice()[1:]
		if len(ociSpec.Process.Args) > 0 && strings.Contains(ociSpec.Process.Args[0], "urunit") {
			ociSpec.Process.Args = append([]string{ociSpec.Process.Args[0]}, override...)
		} else {
			ociSpec.Process.Args = override
		}
		log.Debugf("Command override: %v", ociSpec.Process.Args)
	}

	// Load urunc config
	uruncCfg, _ := unikontainers.LoadUruncConfig(unikontainers.UruncConfigPath)
	if uruncCfg == nil {
		uruncCfg = &unikontainers.UruncConfig{
			Monitors: map[string]types.MonitorConfig{},
		}
	}

	// Determine VMM and unikernel types from annotations or CLI override
	// Default to QEMU for backward compatibility
	var vmmType hypervisors.VmmType
	vmmName := ""

	// CLI override takes precedence
	vmmSource := "default"
	if hypervisorOverride != "" {
		vmmName = hypervisorOverride
		vmmSource = "flag"
		log.Debugf("Using hypervisor from --hypervisor flag: %s", vmmName)
	} else if annotation, ok := ociSpec.Annotations["com.urunc.unikernel.hypervisor"]; ok {
		vmmName = annotation
		vmmSource = "annotation"
		log.Debugf("Using hypervisor from annotation: %s", vmmName)
	} else {
		vmmName = "qemu"
		log.Debugf("No hypervisor override or annotation, using default: qemu")
	}

	// Normalize hypervisor names
	switch vmmName {
	case "qemu-hvf":
		vmmName = "qemu"
	case "virtualization", "apple":
		vmmName = "vz"
	}

	vmmType = hypervisors.VmmType(vmmName)
	telemetryBackendSource = vmmSource
	telemetryBackend = vmmName
	// Graphics is only supported on the Vz backend; the QEMU darwin builder
	// never emits window flags.
	if gui && vmmType != hypervisors.VzVmm {
		return fmt.Errorf("--gui is only supported with the Vz hypervisor (got %q)", vmmName)
	}

	// Rosetta is a Vz facility: the translator arrives as a virtiofs device
	// from Virtualization.framework, which the QEMU backend cannot provide.
	// The annotation lets an image opt in; the flag covers everything else.
	rosetta := cmd.Bool("rosetta") || ociSpec.Annotations["com.urunc.darwin.rosetta"] == "true"
	if rosetta && vmmType != hypervisors.VzVmm {
		return fmt.Errorf("--rosetta is only supported with the Vz hypervisor (got %q)", vmmName)
	}

	// An image that carries no kernel of its own can still boot, on a backend
	// that supports it, if we bring one. Do that here rather than making every
	// caller pass the two annotations by hand: brig already does, and a person
	// running `hull run ubuntu` should not have to know the file names.
	//
	// The paths still come from this invocation and not from the image -- they
	// are resolved on the host, from the host's own asset directory, and only
	// when the image itself offered nothing.
	if !cmd.Bool("no-boot-assets") &&
		annotationOverrides["com.urunc.unikernel.bootKernel"] == "" &&
		annotationOverrides["com.urunc.unikernel.bootInitrd"] == "" &&
		(vmmType == hypervisors.VzVmm || vmmType == hypervisors.HviVmm) &&
		!imageCarriesKernel(ociSpec.Annotations) {
		kernel, initrdFile, assetErr := bootassets.Ensure(ctx, cmd.String("store-dir"))
		if assetErr != nil {
			return fmt.Errorf("this image carries no kernel, so it needs the generic boot assets: %w", assetErr)
		}
		log.Debugf("Generic container boot using %s and %s", kernel, initrdFile)
		// Both maps, so this run is indistinguishable from one where the
		// annotations were passed on the command line.
		for k, v := range map[string]string{
			"com.urunc.unikernel.bootKernel": kernel,
			"com.urunc.unikernel.bootInitrd": initrdFile,
		} {
			annotationOverrides[k] = v
			ociSpec.Annotations[k] = v
		}
	}

	// Host boot paths are accepted only from this invocation, never from image
	// metadata or urunc.json: an image must not be able to nominate host files.
	containerBoot := annotationOverrides["com.urunc.unikernel.bootKernel"] != "" ||
		annotationOverrides["com.urunc.unikernel.bootInitrd"] != ""
	unikernelType := "rumprun"
	if containerBoot {
		unikernelType = "linux"
	}
	if uk, ok := ociSpec.Annotations["com.urunc.unikernel.unikernelType"]; ok {
		unikernelType = uk
	}

	// Create VMM instance
	vmm, err := hypervisors.NewVMM(vmmType, uruncCfg.Monitors)
	if err != nil {
		return fmt.Errorf("failed to create VMM: %w", err)
	}

	// Override QEMU binary path if specified
	if qemuPath != "" {
		if qemuD, ok := vmm.(*hypervisors.Qemu); ok {
			qemuD.SetBinaryPath(qemuPath)
		}
	}

	// Set the per-instance QMP socket (QEMU only)
	if qemuD, ok := vmm.(*hypervisors.Qemu); ok {
		qemuD.SetQMPSocket(qmpSocket)
	}
	if vzD, ok := vmm.(*hypervisors.VzDarwin); ok {
		vzD.SetQMPSocket(qmpSocket)
	}

	// Create unikernel instance
	unikernel, err := unikernels.New(unikernelType)
	if err != nil {
		return fmt.Errorf("failed to create unikernel instance: %w", err)
	}

	// Container entrypoint. The unikernel object is used only to build monitor
	// CLI args in vmm.BuildExecCmd; the darwin runner constructs its own kernel
	// command line and urunit config below (with macOS-specific init wrappers),
	// so we deliberately do not call unikernel.Init here — its Init would run
	// the shared setupUrunitConfig, which the product handles itself.
	cmdLine := ociSpec.Process.Args
	if len(cmdLine) == 0 {
		cmdLine = []string{"/bin/sh"}
	}

	// Prepare execution arguments
	mac, err := generateMAC(s)
	if err != nil {
		return err
	}
	netParams := types.NetDevParams{
		MAC: mac,
	}

	// Add network device if not "none"
	if netMode != "none" {
		netParams.TapDev = "en0" // Will use vmnet-shared
	}
	if gatewaySock != "" && (vmmType == hypervisors.QemuVmm || vmmType == hypervisors.HviVmm) {
		// QEMU and HVI connect to the gateway's QEMU-protocol stream socket.
		// No fd passing or vmnet entitlement is needed. Pre-flight the socket
		// so a down gateway surfaces as the same friendly error the Vz path
		// gets, instead of a VMM connection failure buried in its log.
		qsock := qemuGatewaySock(gatewaySock)
		if conn, err := net.DialTimeout("unix", qsock, 2*time.Second); err != nil {
			return fmt.Errorf("failed to reach network gateway at %s: %w", qsock, err)
		} else {
			_ = conn.Close()
		}
		netParams.UnixSocket = qsock
		netParams.TapDev = ""
	}

	// Extract kernel and rootfs paths from annotations (for Linux kernels on darwin).
	// bootKernel is a host artifact; binary remains an image-relative path.
	kernelPath := ""
	bootKernel := annotationOverrides["com.urunc.unikernel.bootKernel"]
	bootInitrd := annotationOverrides["com.urunc.unikernel.bootInitrd"]
	if (bootKernel == "") != (bootInitrd == "") {
		return errors.New("com.urunc.unikernel.bootKernel and com.urunc.unikernel.bootInitrd must be set together")
	}
	// Whatever this run is about to boot gets checked here, and here is the
	// only place it happens: after the paths are settled and before anything
	// uses them, so every way of arriving at a kernel goes through it.
	//
	// See verifyContainerBootAssets for why this is not attached to the
	// resolver above.
	if err := verifyContainerBootAssets(bootKernel, bootInitrd); err != nil {
		return err
	}
	if containerBoot && vmmType != hypervisors.VzVmm && vmmType != hypervisors.HviVmm {
		return fmt.Errorf("generic virtiofs container boot requires the vz or hvi backend (got %q)", vmmName)
	}
	if bootKernel != "" {
		abs, absErr := filepath.Abs(bootKernel)
		if absErr != nil {
			return fmt.Errorf("resolve boot kernel: %w", absErr)
		}
		// Copy it, then check the copy, then boot the copy. Verifying the
		// original and booting the original meant verifying one resolution of a
		// name and executing another.
		kernelPath, err = stageHostBootFile(abs, instanceDir, stagedHostKernelName)
		if err != nil {
			return err
		}
		if vErr := verifyStagedBootAsset(cmd.String("store-dir"), abs, kernelPath); vErr != nil {
			return vErr
		}
	} else if binary, ok := ociSpec.Annotations["com.urunc.unikernel.binary"]; ok && binary != "" && binary != "unikernel" {
		// Use binary annotation (set by bunny via urunc.json). The value is
		// image content, so it is resolved through the rootfs rather than
		// joined onto it, and the VMM is given hull's own copy rather than a
		// path back into the image -- see stageImageBootFile.
		kernelPath, err = stageImageBootFile(filepath.Join(bundleDir, "rootfs"), binary, instanceDir, stagedKernelName)
		if err != nil {
			return fmt.Errorf("boot kernel from image annotation: %w", err)
		}
	} else if kernel, ok := ociSpec.Annotations["com.urunc.unikernel.kernel"]; ok && kernel != "" && kernel != "unikernel" {
		kernelPath, err = stageImageBootFile(filepath.Join(bundleDir, "rootfs"), kernel, instanceDir, stagedKernelName)
		if err != nil {
			return fmt.Errorf("boot kernel from image annotation: %w", err)
		}
	} else if vmmType == hypervisors.VzVmm || vmmType == hypervisors.HviVmm {
		// For native arm64 backends, look for common kernel locations in rootfs.
		//
		// The names are hull's, but the files are the image's, and an image
		// that ships "vmlinuz" as a symlink to a host file would otherwise have
		// hull boot that host file. Resolving through the rootfs is what stops
		// a probe hull started from becoming a path the image chose; a name
		// that simply is not there is the ordinary case and moves on to the
		// next candidate.
		rootfsDir := filepath.Join(bundleDir, "rootfs")
		commonPaths := []string{
			"vmlinuz",
			"kernel",
			".boot/kernel",
			"boot/vmlinuz",
		}
		for _, candidate := range commonPaths {
			found, ferr := stageImageBootFile(rootfsDir, candidate, instanceDir, stagedKernelName)
			if ferr != nil {
				if errors.Is(ferr, errImagePlantedPath) {
					return fmt.Errorf("boot kernel candidate %s: %w", candidate, ferr)
				}
				// A candidate that is simply not there is the ordinary case
				// and moves on; a copy that could not be made is not, and
				// falling through would boot with no kernel and blame the VMM.
				if errors.Is(ferr, errStageBootFile) {
					return fmt.Errorf("boot kernel candidate %s: %w", candidate, ferr)
				}
				continue
			}
			kernelPath = found
			log.Debugf("Found kernel at: %s", kernelPath)
			break
		}
	}

	// Determine rootfs mode: initrd file vs directory (virtiofs/9pfs/block)
	initrdPath := ""
	rootfsDir := ""
	rootfsDiskImage := "" // path to ext4 disk image (block mode)
	containerRootfsDirect := false
	containerMetadataRootfs := ""
	mountRootfs := ociSpec.Annotations["com.urunc.unikernel.mountRootfs"] == "true"

	if containerBoot {
		absInitrd, absErr := filepath.Abs(bootInitrd)
		if absErr != nil {
			return fmt.Errorf("resolve boot initrd: %w", absErr)
		}
		// Was os.ReadFile into memory then WriteFile: unbounded, with no
		// regular-file check, re-resolving the same name that was hashed
		// seventy lines earlier. Same staging as the kernel now, and the copy
		// is what gets checked.
		initrdPath, err = stageHostBootFile(absInitrd, instanceDir, stagedHostInitrdName)
		if err != nil {
			return err
		}
		if vErr := verifyStagedBootAsset(cmd.String("store-dir"), absInitrd, initrdPath); vErr != nil {
			return vErr
		}
	} else if initrd, ok := ociSpec.Annotations["com.urunc.unikernel.initrd"]; ok && initrd != "" {
		// This one ends up as VZLinuxBootLoader.initialRamdiskURL, i.e. the
		// whole file is copied into guest RAM, so an unvalidated join here
		// exfiltrates any host file the image names. Resolve it through the
		// rootfs instead, and boot hull's own copy of it: what the VMM loads
		// then cannot differ from what was checked. A file that is simply
		// absent still falls through to the directory-rootfs modes below, as
		// the bare Stat did; a path that leads out of the rootfs, or a copy
		// that could not be made, stops the run.
		candidate, resolveErr := stageImageBootFile(
			filepath.Join(bundleDir, "rootfs"), initrd, instanceDir, stagedInitrdName)
		if resolveErr != nil {
			if errors.Is(resolveErr, errImagePlantedPath) || errors.Is(resolveErr, errStageBootFile) {
				return fmt.Errorf("initrd from image annotation: %w", resolveErr)
			}
			log.Debugf("Image initrd annotation %q is unusable (%v); sharing the rootfs directory instead", initrd, resolveErr)
		} else {
			initrdPath = candidate
		}
	}

	// When mountRootfs=true (or no initrd), use the container rootfs directory.
	if initrdPath == "" || containerBoot {
		rootfsLink := filepath.Join(bundleDir, "rootfs")

		// Determine rootfs sharing mode
		rootfsType := rootfsTypeOverride
		if rootfsType == "" {
			if vmmType == hypervisors.VzVmm || vmmType == hypervisors.HviVmm {
				rootfsType = "virtiofs"
			} else {
				rootfsType = "9pfs"
			}
		}

		if containerBoot && rootfsType != "block" {
			if rootfsTypeOverride != "" && rootfsTypeOverride != "virtiofs" {
				return fmt.Errorf("generic container boot supports --rootfs-type virtiofs or block (got %q)", rootfsTypeOverride)
			}
			// The mode is virtiofs from here on: generic container boot is
			// already restricted to the vz and hvi backends above, and both
			// default to virtiofs when no override is given.
			resolved, resolveErr := filepath.EvalSymlinks(rootfsLink)
			if resolveErr != nil {
				return fmt.Errorf("resolve unpacked OCI rootfs: %w", resolveErr)
			}
			if vmmType == hypervisors.HviVmm {
				// HVI's writable virtio-fs backend can use an unpacked directory as
				// the real root. Replace only the instance bundle's cache symlink
				// with an APFS copy-on-write clone: guest changes then persist with
				// this instance without consuming guest RAM or changing the cached
				// OCI image used by later instances.
				if info, statErr := os.Lstat(rootfsLink); statErr != nil {
					return fmt.Errorf("inspect unpacked OCI rootfs: %w", statErr)
				} else if info.Mode()&os.ModeSymlink != 0 {
					if err := cloneRootfsAPFS(resolved, rootfsLink); err != nil {
						return err
					}
				}
				rootfsDir = rootfsLink
				containerMetadataRootfs = rootfsDir
				containerRootfsDirect = true
			} else {
				rootfsDir = resolved
				warnSetuidUnsupported(resolved)
			}
		} else if rootfsType == "block" {
			// Block mode: an ext4 image built straight from the store's rootfs.
			//
			// Nothing is copied. The per-instance files below used to be the
			// only reason for a full clone of the tree -- they must not land in
			// the shared image, where an /etc/hosts append would leak into
			// every future instance -- and they now go into the finished image
			// instead. mke2fs only reads its source, so the store stays
			// pristine, and dropping the clone also drops the reason sudo did
			// not work here: macOS `cp` clears setuid and setgid unless it runs
			// as root, so staging silently disarmed every setuid binary on the
			// way past. See buildBlockRootfs.
			srcDir := rootfsLink
			if target, err := os.Readlink(rootfsLink); err == nil {
				srcDir = target
			}

			initBin := ""
			if len(cmdLine) > 0 {
				initBin = cmdLine[0]
			}
			var injects []blockInject
			if strings.Contains(initBin, "urunit") {
				urunitConf := unikernels.BuildUrunitConfig(ociSpec.Process.Env, procConfig(ociSpec.Process.User.UID, ociSpec.Process.User.GID, ociSpec.Process.Cwd), nil, "")
				injects = append(injects, blockInject{
					guestPath: "/urunit.conf", content: []byte(urunitConf), mode: 0o600,
				})

				// Simple init wrapper: mount devpts, resolv.conf, then exec urunit.
				// No overlay needed — ext4 block device supports all POSIX operations natively.
				wrapperContent := fmt.Sprintf(`#!/bin/sh
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /tmp
mount -t proc proc /proc 2>/dev/null
if [ -f /proc/net/pnp ]; then cp /proc/net/pnp /etc/resolv.conf; fi
umount /proc 2>/dev/null
exec %s "$@"
`, initBin)
				injects = append(injects, blockInject{
					guestPath: "/.block-init", content: []byte(wrapperContent), mode: 0o755,
				})
			}

			// /etc/hosts is an append to whatever the image ships, so it is
			// composed here and injected whole rather than edited in place.
			hostsContent, err := hostsWithEntries(srcDir, addHosts)
			if err != nil {
				return fmt.Errorf("failed to inject /etc/hosts entries: %w", err)
			}
			if hostsContent != nil {
				injects = append(injects, blockInject{
					guestPath: "/etc/hosts", content: hostsContent, mode: 0o644,
				})
			}

			diskPath := filepath.Join(bundleDir, "rootfs.ext4")
			log.Debugf("Creating ext4 disk image from %s", srcDir)

			// Calculate directory size for image sizing. Keep 50% headroom for
			// package installs and guest writes, with a 15 GiB sparse floor so
			// development containers do not immediately run out of space.
			dirSize, _ := dirSizeBytes(srcDir)
			// Add 50% headroom, minimum 15 GiB.
			imageSizeMB := int(dirSize/(1024*1024)) * 3 / 2
			if imageSizeMB < 15*1024 {
				imageSizeMB = 15 * 1024
			}

			if err := buildBlockRootfs(diskPath, srcDir, injects, imageSizeMB); err != nil {
				return err
			}
			rootfsDiskImage = diskPath
			if containerBoot {
				containerMetadataRootfs = srcDir
			}
		} else {
			// virtiofs or 9pfs: clone the rootfs directory
			if target, err := os.Readlink(rootfsLink); err == nil {
				if err := os.Remove(rootfsLink); err != nil {
					return fmt.Errorf("failed to remove rootfs symlink: %w", err)
				}
				cpCmd := exec.Command("/bin/cp", "-c", "-a", target, rootfsLink)
				if _, err := cpCmd.CombinedOutput(); err != nil {
					log.Debugf("APFS clone failed (%v), falling back to regular copy", err)
					cpCmd = exec.Command("/bin/cp", "-a", target, rootfsLink)
					if out, err := cpCmd.CombinedOutput(); err != nil {
						return fmt.Errorf("failed to copy rootfs: %s: %w", string(out), err)
					}
				}
				// Either copy clears setuid and setgid on every file, and this
				// is the one rootfs the guest reads the mode of directly: vz
				// reports the host's mode straight through, so without this a
				// setuid binary arrives disarmed and sudo refuses to run.
				//
				// Except over 9pfs, where putting them back is what breaks the
				// guest. security_model=none reports the host's ownership too,
				// not just the mode, so a restored setuid bit means "become the
				// user who ran hull" -- and /usr/bin/mount is setuid in a stock
				// ubuntu image, so the guest's init dropped to uid 501 on its
				// first mount and could not mount anything for the rest of the
				// boot. See dropSetID.
				setIDPolicy := keepSetID
				if rootfsType == "9pfs" {
					setIDPolicy = dropSetID
				}
				if err := restoreClonedModes(rootfsLink, setIDPolicy); err != nil {
					return fmt.Errorf("failed to restore rootfs modes: %w", err)
				}
			}
			rootfsDir = rootfsLink
			if err := injectHosts(rootfsDir, addHosts); err != nil {
				return fmt.Errorf("failed to inject /etc/hosts entries: %w", err)
			}
		}
	}

	// Parse shared directories: on Vz/HVI each gets its own virtiofs tag and is
	// mounted at its guest path by the init wrapper; QEMU keeps the legacy 9pfs
	// path. Access mode is carried independently for every virtiofs export.
	type hostShare struct {
		host, guest, tag string
		readOnly         bool
	}
	var shares []hostShare
	for i, sd := range sharedDirs {
		readOnly := false
		parts := strings.Split(sd, ":")
		if len(parts) == 3 {
			// Bind mode suffix (host:guest:ro|rw).
			switch parts[2] {
			case "ro":
				// Vz and HVI can hold this read-only. QEMU shares over 9p
				// here with no equivalent, and mounting read-write while the
				// caller asked for read-only is the one outcome worth refusing
				// outright -- a share you believe is protected and is not is
				// worse than no share at all.
				if vmmName == "qemu" {
					return fmt.Errorf("shared-dir %q: read-only shares are not supported on the qemu backend; "+
						"use --hypervisor vz, or drop the :ro suffix to mount it read-write", sd)
				}
				readOnly = true
			case "rw":
			default:
				return fmt.Errorf("invalid shared-dir mode %q in %q (want ro or rw)", parts[2], sd)
			}
			parts = parts[:2]
		}
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid shared-dir %q, expected /host/path:/guest/path[:mode]", sd)
		}
		hostPath, guestPath := parts[0], parts[1]
		if !strings.HasPrefix(hostPath, "/") && !strings.HasPrefix(hostPath, ".") && !strings.HasPrefix(hostPath, "~") {
			return fmt.Errorf("named volumes are not supported (%q): only bind mounts of host paths", hostPath)
		}
		if strings.HasPrefix(hostPath, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to expand home directory: %w", err)
			}
			hostPath = filepath.Join(home, hostPath[1:])
		}
		info, err := os.Stat(hostPath)
		if err != nil {
			return fmt.Errorf("shared directory not found: %s", hostPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is a file: file bind mounts are not supported (virtiofs/9p share directories)", hostPath)
		}
		if !strings.HasPrefix(guestPath, "/") {
			return fmt.Errorf("guest path %q must be absolute", guestPath)
		}
		if strings.ContainsAny(guestPath, " \t'\"") {
			return fmt.Errorf("guest path %q must not contain spaces or quotes", guestPath)
		}
		shares = append(shares, hostShare{
			host: hostPath, guest: guestPath, tag: fmt.Sprintf("share%d", i), readOnly: readOnly,
		})
	}
	shareMounts := ""
	var containerMounts strings.Builder
	for _, sh := range shares {
		if vmmType == hypervisors.VzVmm || vmmType == hypervisors.HviVmm {
			mountOpts := ""
			if sh.readOnly {
				mountOpts = "-o ro "
			}
			shareMounts += fmt.Sprintf("mkdir -p %s\nmount -t virtiofs %s%s %s\n", sh.guest, mountOpts, sh.tag, sh.guest)
		} else {
			shareMounts += fmt.Sprintf("mkdir -p %s\nmount -t 9p -o trans=virtio,version=9p2000.L,msize=512000 %s %s\n", sh.guest, sh.tag, sh.guest)
		}
		mode := "rw"
		if sh.readOnly {
			mode = "ro"
		}
		fmt.Fprintf(&containerMounts, "%s\t%s\t%s\n", sh.tag, sh.guest, mode)
	}

	if containerBoot {
		if err := initrd.AddFileToInitrd(initrdPath, strings.Join(cmdLine, "\n")+"\n", "/urunc-cmd"); err != nil {
			return fmt.Errorf("add OCI command to boot initrd: %w", err)
		}
		if err := initrd.AddFileToInitrd(initrdPath, strings.Join(ociSpec.Process.Env, "\n")+"\n", "/urunc-env"); err != nil {
			return fmt.Errorf("add OCI environment to boot initrd: %w", err)
		}
		if containerMounts.Len() > 0 {
			if err := initrd.AddFileToInitrd(initrdPath, containerMounts.String(), "/urunc-mounts"); err != nil {
				return fmt.Errorf("add virtio-fs shares to boot initrd: %w", err)
			}
		}
		if containerRootfsDirect {
			if err := initrd.AddFileToInitrd(initrdPath, "true\n", "/urunc-rootfs-direct"); err != nil {
				return fmt.Errorf("select direct writable rootfs in boot initrd: %w", err)
			}
		}
		resolver, err := containerBootResolver(vmmType, netMode, gatewayIP)
		if err != nil {
			return err
		}
		if resolver != "" {
			if err := initrd.AddFileToInitrd(initrdPath, "nameserver "+resolver+"\n", "/urunc-resolv.conf"); err != nil {
				return fmt.Errorf("add resolver to boot initrd: %w", err)
			}
		}
		// Hypervisor.framework exposes the architectural counter but HVI does
		// not yet emulate a wall-clock RTC. Seed Linux once at boot so TLS and
		// build tools see a sane clock; it advances normally after this point.
		if vmmType == hypervisors.HviVmm {
			epoch := strconv.FormatInt(time.Now().Unix(), 10) + "\n"
			if err := initrd.AddFileToInitrd(initrdPath, epoch, "/urunc-epoch"); err != nil {
				return fmt.Errorf("add boot time to boot initrd: %w", err)
			}
		}
		hosts, err := containerHosts(containerMetadataRootfs, addHosts)
		if err != nil {
			return err
		}
		if hosts != "" {
			if err := initrd.AddFileToInitrd(initrdPath, hosts, "/urunc-hosts"); err != nil {
				return fmt.Errorf("add hosts file to boot initrd: %w", err)
			}
		}
		if ociSpec.Hostname != "" {
			if err := initrd.AddFileToInitrd(initrdPath, ociSpec.Hostname+"\n", "/urunc-hostname"); err != nil {
				return fmt.Errorf("add hostname to boot initrd: %w", err)
			}
		}
		if rosetta {
			if err := initrd.AddFileToInitrd(initrdPath, "true\n", "/urunc-rosetta"); err != nil {
				return fmt.Errorf("enable Rosetta in boot initrd: %w", err)
			}
		}
	}

	// Console device depends on VMM backend
	consoleDev := "ttyS0"
	if runtime.GOARCH == "arm64" {
		if vmmType == hypervisors.VzVmm {
			consoleDev = "hvc0"
		} else {
			consoleDev = "ttyAMA0"
		}
	}

	// Build kernel cmdline
	kernelCmdline := ""
	// Only request kernel DHCP when networking is enabled
	ipArg := ""
	if gatewayIP != "" {
		addr, gw, mask, err := gatewayNetConfig(gatewayIP)
		if err != nil {
			return err
		}
		ipArg = fmt.Sprintf(" ip=%s::%s:%s:urunc:eth0:off:%s", addr, gw, mask, gw)
	} else if netMode != "none" {
		ipArg = " ip=dhcp"
	}

	// Rosetta needs the generated .vz-init wrapper to register the x86_64
	// handler, and that wrapper only exists on the virtiofs rootfs path.
	if rosetta && rootfsDir == "" {
		return errors.New("rosetta requires the virtiofs rootfs mode (not block or initrd)")
	}

	if containerBoot {
		initrdEntry := "/vz-init"
		if rootfsDiskImage != "" {
			initrdEntry = "/init"
		}
		kernelCmdline = fmt.Sprintf("rdinit=%s console=%s", initrdEntry, consoleDev) + ipArg
	} else if rootfsDir != "" && vmmType == hypervisors.VzVmm {
		// Vz: virtiofs rootfs boot (no initramfs)
		kernelCmdline = fmt.Sprintf("root=rootfs rootfstype=virtiofs rw console=%s", consoleDev) + ipArg

		initBin := ""
		if len(cmdLine) > 0 {
			initBin = cmdLine[0]
		}
		if rosetta && !strings.Contains(initBin, "urunit") {
			return errors.New("rosetta requires a urunit init: the handler registration runs in the generated .vz-init wrapper")
		}

		// If init is urunit, generate urunit.conf and create an init wrapper
		// that mounts /dev/pts (for pty support) before execing urunit.
		if strings.Contains(initBin, "urunit") {
			urunitConf := unikernels.BuildUrunitConfig(ociSpec.Process.Env, procConfig(ociSpec.Process.User.UID, ociSpec.Process.User.GID, ociSpec.Process.Cwd), nil, "")
			// Through the rootfs, not joined onto it: an image is free to ship
			// "urunit.conf" as a symlink to a host file, and this content is
			// substantially the image's own (BuildUrunitConfig embeds the
			// image's environment). An ordinary write failure is still only a
			// warning -- the guest boots without the config -- but a planted
			// path is the image aiming hull at the host and stops the run.
			if err := writeIntoRootfs(rootfsDir, "urunit.conf", []byte(urunitConf), 0600); err != nil {
				if errors.Is(err, errImagePlantedPath) {
					return err
				}
				log.WithError(err).Warn("failed to write urunit.conf")
			} else {
				log.Debugf("Wrote urunit.conf to %s", filepath.Join(rootfsDir, "urunit.conf"))
				kernelCmdline += " URUNIT_CONFIG=/urunit.conf"
			}

			// Create init wrapper that:
			// 1. Mounts an overlayfs with tmpfs upper layer over the virtiofs root.
			// This works around macOS virtiofs not allowing writes to mode-000 files
			// (dpkg creates temp files with mode 0, which fails on macOS virtiofs).
			// 2. Mounts /dev/pts for pty support.
			// After pivot_root, urunit runs in the overlay and can write freely.
			//
			// On the Rosetta path nothing in the rootfs can execute until
			// the x86_64 handler is registered, so the wrapper runs native
			// end to end: the image ships a static arm64 busybox at
			// /.rosetta/busybox, the shebang runs the wrapper under it and
			// every command is a busybox applet. Registration happens
			// before pivot_root (flag F pins the translator open across
			// the root switch, where /mnt/rosetta becomes unreachable) and
			// /proc is left mounted for the translated workload.
			var wrapperContent string
			if rosetta {
				// Resolved through the rootfs so that a .rosetta/busybox
				// symlink pointing at a host binary cannot satisfy this check;
				// the path is only stat'ed for its exec bit afterwards, which
				// is safe because the resolve above already established that it
				// lands inside the image.
				bbPath, bbErr := imageFileHostPath(rootfsDir, ".rosetta/busybox")
				if errors.Is(bbErr, errImagePlantedPath) {
					return bbErr
				}
				if info, err := os.Stat(bbPath); bbErr != nil || err != nil || info.Mode()&0111 == 0 {
					return errors.New("rosetta rootfs must ship an executable static arm64 busybox at /.rosetta/busybox (see urunc-images images/rosetta-proof)")
				}
				bbShares := strings.ReplaceAll(shareMounts, "mkdir ", "$bb mkdir ")
				bbShares = strings.ReplaceAll(bbShares, "mount ", "$bb mount ")
				wrapperContent = fmt.Sprintf(`#!/.rosetta/busybox sh
bb=/.rosetta/busybox
$bb mkdir -p /mnt/rosetta
$bb mount -t virtiofs rosetta /mnt/rosetta
$bb mount -t proc proc /proc
$bb mount -t binfmt_misc none /proc/sys/fs/binfmt_misc
# Canonical x86_64 magic/mask (same bytes lima and Docker Desktop
# register): mask byte 16 is \xfe so both ET_EXEC (02) and ET_DYN (03,
# every PIE binary in a modern rootfs) match. A \xff there makes every
# PIE exec fall through with ENOEXEC.
$bb printf '%%s' ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/mnt/rosetta/rosetta:OCF' > /proc/sys/fs/binfmt_misc/register
$bb mount -t tmpfs tmpfs /tmp
$bb mkdir -p /tmp/.ovl/upper /tmp/.ovl/work /tmp/.ovl/merged
$bb mount -t overlay overlay -o lowerdir=/,upperdir=/tmp/.ovl/upper,workdir=/tmp/.ovl/work /tmp/.ovl/merged
cd /tmp/.ovl/merged
$bb mkdir -p .pivot_old
$bb pivot_root . .pivot_old
$bb umount -l /.pivot_old/tmp 2>/dev/null
$bb mount -t tmpfs tmpfs /tmp
$bb mount -t devtmpfs devtmpfs /dev
$bb mkdir -p /dev/pts
$bb mount -t devpts devpts /dev/pts
%s# Populate /etc/resolv.conf from kernel DHCP response (stored in /proc/net/pnp)
$bb mount -t proc proc /proc
if [ -f /proc/net/pnp ]; then $bb cp /proc/net/pnp /etc/resolv.conf; fi
# /proc stays mounted: Rosetta needs it at every translated exec.
[ -x /urunit-agent ] && /urunit-agent >/dev/hvc0 2>&1 &
exec %s "$@"
`, bbShares, initBin)
			} else {
				wrapperContent = fmt.Sprintf(`#!/bin/sh
mount -t tmpfs tmpfs /tmp
mkdir -p /tmp/.ovl/upper /tmp/.ovl/work /tmp/.ovl/merged
mount -t overlay overlay -o lowerdir=/,upperdir=/tmp/.ovl/upper,workdir=/tmp/.ovl/work /tmp/.ovl/merged
cd /tmp/.ovl/merged
mkdir -p .pivot_old
pivot_root . .pivot_old
umount -l /.pivot_old/tmp 2>/dev/null
mount -t tmpfs tmpfs /tmp
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
%s# Populate /etc/resolv.conf from kernel DHCP response (stored in /proc/net/pnp)
mount -t proc proc /proc 2>/dev/null
if [ -f /proc/net/pnp ]; then cp /proc/net/pnp /etc/resolv.conf; fi
umount /proc 2>/dev/null
# Start the exec agent when the image ships one (see hull exec).
# Its log goes to the console: the overlay upper layer is a tmpfs, so a
# file would not be reachable from the host.
[ -x /urunit-agent ] && /urunit-agent >/dev/hvc0 2>&1 &
exec %s "$@"
`, shareMounts, initBin)
			}
			// Same containment as urunit.conf above: ".vz-init" is hull's name
			// but the image owns the directory it lands in, and this file is
			// executed as the guest's init.
			if err := writeIntoRootfs(rootfsDir, ".vz-init", []byte(wrapperContent), 0755); err != nil {
				if errors.Is(err, errImagePlantedPath) {
					return err
				}
				log.WithError(err).Warn("failed to write init wrapper")
			} else {
				initBin = "/.vz-init"
			}
		}

		// init= and -- args must come last
		if initBin != "" {
			kernelCmdline += " init=" + initBin
			if len(cmdLine) > 1 {
				kernelCmdline += " -- " + strings.Join(cmdLine[1:], " ")
			}
		}
	} else if rootfsDir != "" && vmmType == hypervisors.QemuVmm {
		// QEMU: 9pfs rootfs boot (no initramfs)
		kernelCmdline = fmt.Sprintf("root=rootfs rootfstype=9p rootflags=trans=virtio,version=9p2000.L rw console=%s", consoleDev) + ipArg

		initBin := ""
		if len(cmdLine) > 0 {
			initBin = cmdLine[0]
		}

		if strings.Contains(initBin, "urunit") {
			urunitConf := unikernels.BuildUrunitConfig(ociSpec.Process.Env, procConfig(ociSpec.Process.User.UID, ociSpec.Process.User.GID, ociSpec.Process.Cwd), nil, "")
			// Contained exactly as on the Vz path above -- the 9pfs rootfs is
			// the same unpacked image, planted symlinks and all.
			if err := writeIntoRootfs(rootfsDir, "urunit.conf", []byte(urunitConf), 0600); err != nil {
				if errors.Is(err, errImagePlantedPath) {
					return err
				}
				log.WithError(err).Warn("failed to write urunit.conf")
			} else {
				log.Debugf("Wrote urunit.conf to %s", filepath.Join(rootfsDir, "urunit.conf"))
				kernelCmdline += " URUNIT_CONFIG=/urunit.conf"
			}

			// Init wrapper for QEMU: mount devpts, populate resolv.conf, then exec urunit.
			// No overlay needed — QEMU's 9pfs with security_model=none handles writes directly.
			wrapperContent := fmt.Sprintf(`#!/bin/sh
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /tmp
%s# Populate /etc/resolv.conf from kernel DHCP response
mount -t proc proc /proc 2>/dev/null
if [ -f /proc/net/pnp ]; then cp /proc/net/pnp /etc/resolv.conf; fi
umount /proc 2>/dev/null
# Start the exec agent when the image ships one (see hull exec)
[ -x /urunit-agent ] && /urunit-agent >/urunit-agent.log 2>&1 &
exec %s "$@"
`, shareMounts, initBin)
			if err := writeIntoRootfs(rootfsDir, ".qemu-init", []byte(wrapperContent), 0755); err != nil {
				if errors.Is(err, errImagePlantedPath) {
					return err
				}
				log.WithError(err).Warn("failed to write QEMU init wrapper")
			} else {
				initBin = "/.qemu-init"
			}
		}

		if initBin != "" {
			kernelCmdline += " init=" + initBin
			if len(cmdLine) > 1 {
				kernelCmdline += " -- " + strings.Join(cmdLine[1:], " ")
			}
		}
	} else if rootfsDiskImage != "" {
		// Block device rootfs (ext4 disk image)
		// Vz: first block device is /dev/vda. QEMU with -M virt: /dev/vda.
		kernelCmdline = fmt.Sprintf("root=/dev/vda rw console=%s", consoleDev) + ipArg

		initBin := ""
		if len(cmdLine) > 0 {
			initBin = cmdLine[0]
		}
		if strings.Contains(initBin, "urunit") {
			kernelCmdline += " URUNIT_CONFIG=/urunit.conf"
			initBin = "/.block-init"
		}
		if initBin != "" {
			kernelCmdline += " init=" + initBin
			if len(cmdLine) > 1 {
				kernelCmdline += " -- " + strings.Join(cmdLine[1:], " ")
			}
		}
	} else if initrdPath != "" {
		kernelCmdline = "rdinit=/init console=" + consoleDev
	}

	// Override with explicit cmdline annotation if present
	if cmdline, ok := ociSpec.Annotations["com.urunc.unikernel.cmdline"]; ok && cmdline != "" {
		if strings.Contains(cmdline, "=") || strings.Contains(cmdline, "root") ||
			strings.Contains(cmdline, "console") || strings.Contains(cmdline, "init") {
			kernelCmdline = cmdline
		} else if kernelCmdline != "" {
			kernelCmdline += " -- " + cmdline
		} else {
			kernelCmdline = cmdline
		}
	}
	_ = mountRootfs

	// Wire the parsed shares into the VMM arguments: each share is a tagged
	// export (virtiofs on Vz, 9p on QEMU) mounted by the init wrapper.
	sharedfsParams := types.SharedfsParams{}
	if containerBoot && rootfsDir != "" {
		sharedfsParams = types.SharedfsParams{
			Type: "virtiofs", Path: rootfsDir, Tag: "rootfs", ReadOnly: !containerRootfsDirect,
		}
	}
	var sharedDirParams []types.SharedDirParams
	for _, sh := range shares {
		sharedDirParams = append(sharedDirParams, types.SharedDirParams{
			Path: sh.host, Tag: sh.tag, ReadOnly: sh.readOnly,
		})
		log.Debugf("Shared directory: host=%s guest=%s tag=%s readOnly=%t",
			sh.host, sh.guest, sh.tag, sh.readOnly)
	}

	// "unikernel" is hull's own default name, but the file it names would come
	// out of the image and is handed to the monitor as a binary to load, so it
	// is resolved through the rootfs like every other image-relative path: a
	// symlink planted there must not turn into a host file on the monitor's
	// command line. The literal has no separators, so when there is simply no
	// such file -- every container image -- the joined path is still contained
	// and is kept, and monitors fail on it exactly as they did before.
	bundleRootfs := filepath.Join(bundleDir, "rootfs")
	unikernelPath, unikernelErr := imageFileHostPath(bundleRootfs, "unikernel")
	if errors.Is(unikernelErr, errImagePlantedPath) {
		return unikernelErr
	}
	if unikernelErr != nil {
		unikernelPath = filepath.Join(bundleRootfs, "unikernel")
	}

	execArgs := types.ExecArgs{
		ContainerID:   instanceName,
		UnikernelPath: unikernelPath,
		KernelPath:    kernelPath,
		InitrdPath:    initrdPath,
		RootfsPath:    rootfsDir,
		BlockDevPath:  rootfsDiskImage,
		LogFile:       logFile,
		AgentSockPath: s.InstanceAgentSocket(instanceName),
		GUI:           gui,
		GUITitle:      guiTitle,
		Command:       kernelCmdline,
		MemSizeB:      uint64(memMB) * 1024 * 1024,
		VCPUs:         uint(cpus),
		Net:           netParams,
		Sharedfs:      sharedfsParams,
		SharedDirs:    sharedDirParams,
		// Who the workload runs as, so a file server sharing a host directory
		// can present it as owned by them. Without this the share comes back
		// owned by root, and an image whose user is anyone else cannot write
		// its own home -- the guest kernel refuses before the request reaches
		// the host, so it surfaces as EACCES that names nothing.
		GuestUID: ociSpec.Process.User.UID,
		GuestGID: ociSpec.Process.User.GID,
	}

	// Build VMM command
	cmdArgs, err := vmm.BuildExecCmd(execArgs, unikernel)
	if err != nil {
		return fmt.Errorf("failed to build VMM command: %w", err)
	}

	// Pre-execution setup
	if err := vmm.PreExec(execArgs); err != nil {
		return fmt.Errorf("failed to perform pre-execution setup: %w", err)
	}

	// Start VMM process
	var gatewayFiles []*os.File

	// Join the user-mode gateway: the datagram end backs the guest's single
	// NIC (fd 3 in the child); the control connection (fd 4) stays open for
	// the VMM's lifetime and its closure removes the gateway member.
	if gatewaySock != "" && vmmType == hypervisors.VzVmm {
		dataF, ctlConn, err := joinGateway(gatewaySock)
		if err != nil {
			return err
		}
		defer func() { _ = dataF.Close() }()
		ctlF, err := ctlConn.File()
		if err != nil {
			return fmt.Errorf("failed to dup gateway control connection: %w", err)
		}
		_ = ctlConn.Close()
		defer func() { _ = ctlF.Close() }()
		gatewayFiles = []*os.File{dataF, ctlF}
		cmdArgs = append(cmdArgs, "--net-fd", "3")
	}
	if vmmType == hypervisors.VzVmm {
		cmdArgs = append(cmdArgs, vzNetArgs(netMode)...)
		cmdArgs = append(cmdArgs, "--stop-grace", strconv.Itoa(int(cmd.Int("stop-grace"))))
		// Every Vz instance is checkpoint-ready: the state dir persists the
		// machine identifier (restore demands the identical identity) and
		// receives `hull checkpoint` snapshots. vz-runner drops the
		// never-driven memory balloon when a state dir is set, because
		// balloon devices are not snapshottable.
		cmdArgs = append(cmdArgs, "--state-dir", filepath.Join(instanceDir, "checkpoint"))
		// Rosetta rides the same host-side append as --state-dir and
		// --net-fd: a vz-runner flag the shared exec builder does not know.
		if rosetta {
			cmdArgs = append(cmdArgs, "--rosetta")
		}
	}

	state := &store.InstanceState{
		ID:          instanceName,
		ImageDigest: imageDigest,
		QMPSocket:   qmpSocket,
		LogFile:     logFile,
		BundleDir:   bundleDir,
		MAC:         mac,
		Backend:     string(vmmType),
	}
	started, err := launchVMM(cmd, s, state, cmdArgs, gatewayFiles, vmmType, detach, netMode, gatewayIP)
	instanceStarted = started
	return err
}

// setuidProbePaths are the binaries whose whole purpose is to elevate. If none
// of these carries the setuid bit the image has nothing to lose from a share
// that cannot express one, so the warning below stays quiet.
var setuidProbePaths = []string{
	"usr/bin/sudo",
	"bin/su",
	"usr/bin/su",
	"usr/bin/passwd",
	"usr/bin/newgrp",
	"usr/bin/chsh",
}

// warnSetuidUnsupported says so when an image ships setuid binaries onto a
// share that cannot carry them.
//
// Apple's virtio-fs reports every file as owned by the uid of the guest
// process that asked, so a guest process never sees a file it does not already
// own and setuid-exec is a no-op. Worse, the answer is order-dependent: the
// same inode comes back root-owned to whichever caller looked it up first and
// caller-owned to the next, so `ls -la /usr/bin` and `ls -la /usr/bin/sudo`
// disagree about the same file. There is no attribute channel to fix this
// with -- the daemon is Apple's -- so the only honest thing is to name the
// two configurations that do work: hvi's virtio-fs, which reads the ownership
// hull records at unpack, and a block rootfs, whose ext4 carries real modes.
func warnSetuidUnsupported(rootfsDir string) {
	for _, rel := range setuidProbePaths {
		fi, err := os.Lstat(filepath.Join(rootfsDir, rel))
		if err != nil || fi.Mode()&os.ModeSetuid == 0 {
			continue
		}
		log.Warnf("%s is setuid, but the vz backend shares the root filesystem through "+
			"Apple's virtio-fs, which reports every file as owned by the caller: sudo and "+
			"friends will not elevate, and file ownership will look inconsistent between "+
			"a directory listing and a direct stat. Use `--rootfs-type block`, or the hvi "+
			"backend, if the guest needs to become root.", rel)
		return
	}
}

// cloneRootfsAPFS atomically replaces an instance bundle's rootfs symlink with
// a copy-on-write directory clone. A regular-copy fallback is intentionally not
// used: silently copying a large image would make the cost and disk behavior of
// HVI container boot depend on free space rather than APFS clone semantics.
func cloneRootfsAPFS(source, rootfsLink string) error {
	staging := rootfsLink + ".apfs-clone"
	if _, err := os.Lstat(staging); err == nil {
		return fmt.Errorf("stage APFS rootfs clone: destination already exists: %s", staging)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect APFS rootfs clone destination: %w", err)
	}
	if err := requireSameAPFSVolume(source, filepath.Dir(rootfsLink)); err != nil {
		return err
	}

	cpCmd := exec.Command("/bin/cp", "-c", "-a", source, staging)
	if out, err := cpCmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("APFS clone rootfs %s: %s: %w", source, strings.TrimSpace(string(out)), err)
	}
	// Darwin cp documents that recursive copies split hard links, including
	// with -c. Re-link cloned siblings according to the source inode graph so
	// the guest receives the original OCI filesystem semantics (and tools such
	// as du do not count one cloned extent once per hard-link name).
	if err := restoreClonedHardlinks(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("restore hard links in APFS rootfs clone: %w", err)
	}
	// cp also drops setuid and setgid on every file it copies. Nothing notices
	// on this path, because hvi serves the mode from the record beside the file
	// rather than from the clone -- but a clone that is wrong only where
	// something else happens to cover for it is a trap for the next reader.
	if err := restoreClonedModes(staging, keepSetID); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("restore modes in APFS rootfs clone: %w", err)
	}
	if err := os.Remove(rootfsLink); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("replace instance rootfs symlink: %w", err)
	}
	if err := os.Rename(staging, rootfsLink); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("install instance APFS rootfs clone: %w", err)
	}
	return nil
}

func requireSameAPFSVolume(source, destinationParent string) error {
	var sourceFS, destinationFS unix.Statfs_t
	if err := unix.Statfs(source, &sourceFS); err != nil {
		return fmt.Errorf("inspect source filesystem for APFS clone: %w", err)
	}
	if err := unix.Statfs(destinationParent, &destinationFS); err != nil {
		return fmt.Errorf("inspect destination filesystem for APFS clone: %w", err)
	}
	fsName := func(raw [16]byte) string {
		for i, value := range raw {
			if value == 0 {
				return string(raw[:i])
			}
		}
		return string(raw[:])
	}
	if fsName(sourceFS.Fstypename) != "apfs" || fsName(destinationFS.Fstypename) != "apfs" {
		return fmt.Errorf("HVI writable rootfs requires an APFS image store and instance directory")
	}
	if sourceFS.Fsid.Val != destinationFS.Fsid.Val {
		return fmt.Errorf("HVI writable rootfs requires the image store and instance directory on the same APFS volume")
	}
	return nil
}

func restoreClonedHardlinks(source, destination string) error {
	type inodeKey struct {
		device uint64
		inode  uint64
	}
	firstDestination := make(map[inodeKey]string)
	return filepath.Walk(source, func(sourcePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink < 2 {
			return nil
		}
		relative, err := filepath.Rel(source, sourcePath)
		if err != nil {
			return err
		}
		destinationPath := filepath.Join(destination, relative)
		key := inodeKey{device: uint64(stat.Dev), inode: stat.Ino}
		first, seen := firstDestination[key]
		if !seen {
			firstDestination[key] = destinationPath
			return nil
		}
		if err := os.Remove(destinationPath); err != nil {
			return err
		}
		return os.Link(first, destinationPath)
	})
}

// containerBootResolver returns the resolver that the generic boot initrd can
// install before switch_root. A static kernel ip= configuration does not fill
// /proc/net/pnp, while HVI's built-in stack has a fixed DNS endpoint.
func containerBootResolver(vmmType hypervisors.VmmType, netMode, gatewayCIDR string) (string, error) {
	if gatewayCIDR != "" {
		_, gateway, _, err := gatewayNetConfig(gatewayCIDR)
		return gateway, err
	}
	if vmmType == hypervisors.HviVmm && netMode != "none" {
		return "10.0.2.3", nil
	}
	return "", nil
}

// vzNetArgs returns the vz-runner flags that carry `--net none` to the backend.
//
// The other backends take "no network" by omission: QEMU and HVI are given no
// netdev and so have none. vz-runner cannot work that way, because its default
// when no --net-fd is passed is Apple's NAT -- a working route to the internet.
// Leaving netParams.TapDev empty therefore said nothing to it at all, and a
// sandbox asked to run with no network ran with full outbound connectivity and
// no message anywhere saying so. An agent isolated by `--net none` was not
// isolated, which is the sort of gap that is only found afterwards.
//
// So the request is stated positively and the runner acts on it, rather than
// being inferred from an absence that also means "use the default".
func vzNetArgs(netMode string) []string {
	if netMode == "none" {
		return []string{"--no-net"}
	}
	return nil
}

// verifyContainerBootAssets checks the kernel and initrd this run is about to
// boot against the provenance record beside them.
//
// It takes paths, not a decision about where they came from, and that is the
// point. The content check used to live inside bootassets.Ensure, which runs
// only when hull resolves the assets itself -- and the caller that boots most
// of these sandboxes does not let it. brig finds the kernel in the same shared
// asset directory with its own stat-and-non-zero-size check and passes hull the
// result in the bootKernel/bootInitrd annotations, which it does for six of its
// eight shipped profiles. So the verification was in the tree, was tested, and
// never executed for the path real users take: the annotations were set, the
// Ensure branch was skipped, and the kernel went to the VMM unhashed.
//
// Hanging it off the files instead means there is no branch to miss. Every
// container boot has a kernel path by the time it reaches here, whoever chose
// it, and a future resolver -- another runtime, a new flag, a cache -- is
// covered without anyone remembering this exists. When hull did resolve the
// assets itself the files are hashed twice; that costs a sha256 over a file
// already in the page cache, which is a price worth paying for a check that
// cannot be routed around.
//
// A kernel with no record beside it is not refused. HULL_BOOT_ASSETS pointing
// at somebody's own build is a supported way to work and there is nothing to
// compare it against, so it is reported and allowed. A record that disagrees
// with the bytes, or one that does not cover them, stops the run.

// verifyStagedBootAsset checks a staged copy against the record beside the
// original, and decides what an absent record means.
//
// The policy differs by where the original lives, which is the distinction the
// old warn-and-boot missed. A file in hull's OWN asset directory got there
// through Fetch, which writes a record; if the record is gone, something
// removed it, and `rm provenance.json` was a one-unlink downgrade that worked
// in every mode including HULL_VERIFY=require. A file the operator pointed at
// with --boot-kernel is theirs, and hull has never claimed to vouch for it.
func verifyStagedBootAsset(storeDir, original, staged string) error {
	err := bootassets.VerifyStagedCopy(original, staged)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bootassets.ErrNoProvenance):
		// An absent record is ambiguous, and the first version of this refused
		// outright for anything inside hull's own asset directory. That broke a
		// real boot within the hour: assets fetched before the record existed
		// -- or by brig's Linux fetcher, which still writes none -- have no
		// record through no fault of anyone, and ErrNoProvenance's own comment
		// says as much: "a cache written by an older hull is a migration, not
		// an attack". Refusing it was overriding that decision without doing
		// the migration.
		//
		// So the refusal is tied to the mode instead, which is what the finding
		// was actually about: HULL_VERIFY=require was referenced in exactly one
		// place, inside Fetch, and never covered a boot at all. Now it does.
		// Under the default it warns, and names the fix.
		inOurs, dirErr := underBootAssetDir(storeDir, original)
		if bootassets.VerifyModeFromEnv() == bootassets.VerifyRequire {
			return fmt.Errorf("refusing to boot %s: %s=require and it has no provenance "+
				"record beside it, so nothing vouches for its contents. Run "+
				"`hull assets pull --force` to fetch and record them again",
				original, bootassets.VerifyModeEnv)
		}
		if dirErr == nil && inOurs {
			log.Warnf("%s is in hull's own boot-asset directory but has no provenance "+
				"record beside it, so nothing vouches for its contents. Fetch writes one; "+
				"`hull assets pull --force` will record them. Booting as given "+
				"(%s=require refuses instead)", original, bootassets.VerifyModeEnv)
			return nil
		}
		log.Warnf("%s has no provenance record beside it, so nothing vouches for its "+
			"contents; it is being booted as given", original)
		return nil
	default:
		return fmt.Errorf("refusing to boot this kernel: %w", err)
	}
}

// underBootAssetDir reports whether a path is inside the directory hull fetches
// boot assets into.
func underBootAssetDir(storeDir, path string) (bool, error) {
	assetDir, err := bootassets.Dir(storeDir)
	if err != nil {
		return false, err
	}
	absAsset, err := filepath.Abs(assetDir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absAsset, path)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func verifyContainerBootAssets(bootKernel, bootInitrd string) error {
	paths := make([]string, 0, 2)
	for _, p := range []string{bootKernel, bootInitrd} {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve boot asset %s: %w", p, err)
		}
		paths = append(paths, abs)
	}
	if len(paths) == 0 {
		return nil
	}
	err := bootassets.VerifyFiles(paths...)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bootassets.ErrNoProvenance):
		log.Warnf("The kernel and initrd for this run (%s) have no provenance record beside "+
			"them, so nothing vouches for their contents; they are being booted as given",
			strings.Join(paths, ", "))
		return nil
	default:
		return fmt.Errorf("refusing to boot this kernel: %w", err)
	}
}

// imageCarriesKernel reports whether the image brought a kernel of its own, in
// which case there is nothing for the generic boot assets to do.
//
// "unikernel" is not a file name. pkg/ociclient writes it as the default for
// both keys when an image has no urunc labels at all, and the boot path below
// already treats it as "no kernel here" rather than as a path to look up in the
// rootfs. Anything else is a real, image-relative path put there by bunny.
func imageCarriesKernel(annotations map[string]string) bool {
	for _, key := range []string{
		"com.urunc.unikernel.binary",
		"com.urunc.unikernel.kernel",
	} {
		if v := annotations[key]; v != "" && v != "unikernel" {
			return true
		}
	}
	return false
}

func parseAnnotationEntries(entries []string) (map[string]string, error) {
	annotations := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --annotation %q, expected KEY=VALUE", entry)
		}
		annotations[key] = value
	}
	return annotations, nil
}

// errImagePlantedPath marks a containment refusal: a file hull was about to
// write into, read out of, or boot from an unpacked image turned out to lead
// somewhere else on the host.
//
// The entry being there is not a bug in the extractor. pkg/ociclient/unpack.go
// preserves absolute symlinks on purpose -- real images need them
// ("usr/bin/awk -> /etc/alternatives/awk") -- and its own os.Root refuses to
// traverse one, so nothing the extractor writes can leave the rootfs. What that
// argument does not cover is hull's own host-side writers here in cmd/hull,
// which used filepath.Join and plain os.WriteFile: an image shipping
// "urunit.conf" as a symlink to a file in the caller's home had hull truncate
// and rewrite that file, with content the image largely chose
// (BuildUrunitConfig embeds the image's own environment).
//
// This is a refusal, not an I/O error, and callers must not carry on past it:
// continuing means an image aimed hull at a host path and hull said nothing.
// They test for it with errors.Is so a planted path is distinguishable from a
// full disk or a read-only store.
var errImagePlantedPath = errors.New("the image planted a path that leads out of the rootfs")

// rootfsRefusal labels what an os.Root operation on an unpacked image returned,
// separating containment from ordinary failure.
//
// os.Root reports every genuine filesystem failure as a syscall errno (ENOENT,
// EACCES, ENOSPC). Containment is the one refusal it synthesises itself -- an
// unexported "path escapes from parent" with no errno behind it -- so a
// PathError with no errno inside is the kernel-backed walk telling us this name
// leaves the rootfs. The original error is kept wrapped either way, so
// errors.Is(err, os.ErrNotExist) still works for callers that treat a missing
// file as normal.
func rootfsRefusal(op, entry string, err error) error {
	var pathErr *os.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && !errors.As(pathErr.Err, &errno) {
		return fmt.Errorf("refusing to %s %q in the image rootfs: %w: %w",
			op, entry, errImagePlantedPath, err)
	}
	return fmt.Errorf("failed to %s %q in the image rootfs: %w", op, entry, err)
}

// writeIntoRootfs writes one of hull's own files into an unpacked image.
//
// The write goes through an os.Root opened on the rootfs rather than through
// filepath.Join, for the reason spelled out on errImagePlantedPath: the name is
// hull's, but the directory it lands in is the image's, and the image is free
// to have put a symlink there first. os.Root re-resolves every component with
// the rootfs as its ceiling and refuses to cross it -- the same guarantee
// pkg/ociclient/unpack.go relies on for extraction.
func writeIntoRootfs(rootfsDir, name string, data []byte, perm os.FileMode) error {
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return fmt.Errorf("failed to open image rootfs %s: %w", rootfsDir, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(name, data, perm); err != nil {
		return rootfsRefusal("write", name, err)
	}
	return nil
}

// readFromRootfs reads one of the image's own files, through the same os.Root
// containment as writeIntoRootfs. A read matters as much as a write here:
// hull copies /etc/hosts out of the rootfs and into guest-visible metadata, so
// an image that ships it as a symlink to a host file would have hull hand that
// file's contents to the guest.
//
// An empty rootfsDir means the caller has no metadata source for this instance
// and is reported as a missing file, which is how the plain os.ReadFile this
// replaces behaved for a path that did not exist.
func readFromRootfs(rootfsDir, name string) ([]byte, error) {
	if rootfsDir == "" {
		return nil, fmt.Errorf("no image rootfs to read %q from: %w", name, os.ErrNotExist)
	}
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open image rootfs %s: %w", rootfsDir, err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, rootfsRefusal("read", name, err)
	}
	return data, nil
}

// openImageFile resolves a file the image nominated -- a binary, kernel or
// initrd annotation, or one of the well-known kernel names hull probes for --
// and hands back the open descriptor together with the rootfs-relative name it
// resolved to.
//
// bundle/rootfs/urunc.json is image content: pkg/ociclient merges it into the
// spec's annotations with no allowlist, so every value read here is attacker
// controlled. filepath.Join is not a containment check for such a value. It
// cleans, so a fixed, over-long "../../../../../.." prefix walks out of any
// rootfs whatever its depth, and the attacker needs to know nothing about where
// the bundle lives. That is how an arbitrary host file reached
// VZLinuxBootLoader.initialRamdiskURL and was copied into guest RAM, against
// the invariant hull states in internal/bootassets/bootassets.go and above:
// an image must not be able to nominate a file on the host.
//
// The descriptor is what callers should prefer to the name: it is the one
// handle on the file that no later rename, unlink or symlink swap in the image
// rootfs can redirect. The caller closes it.
func openImageFile(rootfsDir, name string) (string, *os.File, error) {
	// Both conventions are in the wild: bunny writes "/unikernel/app.elf",
	// other tooling writes "boot/vmlinuz". A leading slash means the root of
	// the image, not the host's root, which is how filepath.Join has always
	// read these -- so strip it and let os.Root judge what is left. Unlike
	// Join, os.Root refuses a ".." that leaves the rootfs instead of quietly
	// cleaning it away, and refuses a symlink whose target is absolute.
	rel := strings.TrimLeft(filepath.ToSlash(name), "/")
	if rel == "" {
		return "", nil, fmt.Errorf("image nominated %q, which names no file in the rootfs", name)
	}
	root, err := os.OpenRoot(rootfsDir)
	if err != nil {
		return "", nil, fmt.Errorf("failed to open image rootfs %s: %w", rootfsDir, err)
	}
	defer func() { _ = root.Close() }()
	// O_NONBLOCK, because the mode check below cannot run until the open
	// returns and open(2) on a FIFO does not return: it blocks until somebody
	// opens the write end, which an image that ships a FIFO here never does.
	// mkfifo needs no privilege, so that is a one-entry layer that hangs
	// `hull run` with no timeout and no output. Containment does not help --
	// os.Root resolves the path perfectly well and then blocks in the syscall.
	//
	// Lstat would be the obvious guard and is the wrong one twice over: it
	// answers about the link rather than the target, so it refuses the ordinary
	// symlink-to-a-kernel an image is entitled to ship, and it still misses a
	// symlink pointing at a FIFO inside the rootfs. Opening without blocking
	// and then asking the descriptor what it got answers both, and leaves
	// os.Root owning the containment taxonomy so a planted path still reports
	// as one.
	//
	// O_NONBLOCK on the regular file this is meant to return is a no-op.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, rootfsRefusal("open", name, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", nil, fmt.Errorf("failed to stat %q in the image rootfs: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return "", nil, fmt.Errorf("%q in the image rootfs is not a regular file", name)
	}
	return rel, f, nil
}

// imageFileHostPath resolves an image-nominated file to a host path.
//
// A path is strictly weaker than the descriptor openImageFile returns: it is
// re-resolved by whoever opens it next, and between the two resolutions the
// image rootfs is an ordinary directory on disk that a concurrent `hull pull`
// of the same tag, or anything else with write access to the store, can change
// underneath. Anything hull hands to a VMM to boot goes through
// stageImageBootFile instead; this is for the two cases that cannot be staged,
// the `unikernel` name and the Rosetta busybox check, both spelled out in
// TestBootFilesAreStagedRatherThanPathed.
func imageFileHostPath(rootfsDir, name string) (string, error) {
	rel, f, err := openImageFile(rootfsDir, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	// The join happens last, and only because it has already been validated:
	// the successful open above is the kernel confirming that every component
	// of rel resolves inside rootfsDir, so joining it back on cannot name
	// anything else. Joining first and checking afterwards -- or not checking
	// at all, which is what this replaces -- is the bug.
	return filepath.Join(rootfsDir, rel), nil
}

// The names hull's own copies of the image's boot files take inside the
// instance directory. They are fixed, and deliberately not derived from what
// the image called them: a destination assembled from image content would put
// the attacker back on the other side of the path hull is trying to own.
// "container-boot-initrd" alongside them is the same idea for the --boot-initrd
// flag, which stages a host file for the same reason.
const (
	stagedKernelName = "image-kernel"
	stagedInitrdName = "image-initrd"
	// The host-supplied boot assets get their own names, so a run that stages
	// both an image kernel and a host kernel cannot collide on one file.
	stagedHostKernelName = "boot-kernel"
	stagedHostInitrdName = "boot-initrd"
)

// maxBootFileBytes bounds a staged kernel or initrd.
//
// 2 GiB is far past anything real -- the guest kernels hull ships are tens of
// megabytes -- and the number only has to be small enough that an image cannot
// use the staging copy to fill the host's disk once per run. The image's own
// layer cap does not help here: it is 16 GiB and it is per layer, while this
// copy happens again for every instance and every supervisor restart.
const maxBootFileBytes = 2 << 30

// errStageBootFile marks a failure to put the copy in place, as opposed to a
// failure to resolve the name. The callers that treat an absent or unusable
// image file as "no initrd, share the rootfs directory instead" must not
// silently take that branch because the disk filled up.
var errStageBootFile = errors.New("failed to stage the image's boot file")

// stageImageBootFile copies a file the image nominated into the instance
// directory and returns that copy's path, which is what the VMM is given.
//
// Validating a name and handing back a path inside the image rootfs leaves a
// window: the check and the VMM's open are two separate resolutions of the
// same name, minutes apart in a slow boot, and in between the rootfs is an
// ordinary directory on disk. A concurrent `hull pull` republishing the tag,
// another instance sharing the same store, or the image's own guest where the
// rootfs is shared writable, can all change what the name resolves to after
// hull has approved it -- and the file the VMM then loads into guest memory is
// not the file that was checked. Copying closes the window by removing the
// second resolution entirely: the bytes come off the descriptor openImageFile
// already validated, and the path the VMM opens is one hull owns.
//
// A hardlink or an APFS clone would be cheaper, and both were considered:
//
//   - A hardlink is the wrong tool. It re-resolves the source by name, which
//     is the very lookup being eliminated, and it shares the inode -- so an
//     image whose rootfs the guest mounts writable can still rewrite the
//     kernel's contents in place after the link is made. Sharing an inode
//     with a file the attacker may hold a handle on is not a boundary.
//   - An APFS clone (`cp -c`) has the right copy-on-write semantics, but the
//     only interface to it is by path, so it too re-resolves the source, and
//     it fails outright off APFS.
//
// So the read is from the descriptor. The cost is one sequential copy of a
// kernel or initrd -- tens of MB, once, at boot -- against a run that already
// clones or rebuilds the whole rootfs per instance; it is not measurable
// beside that. The copy lives in the instance directory, so `hull rm` removes
// it with everything else and nothing has to remember to clean it up.

// stageHostBootFile copies a host boot asset into the instance directory and
// returns the copy's path.
//
// The image-nominated path has done this since round 4, for a reason its own
// comment states: "a path is re-resolved by whoever opens it next". The HOST
// path -- the kernel and initrd hull fetched into its shared asset directory --
// did not, and that is the more exposed of the two. It was hashed against the
// provenance record here and then handed to the VMM as the same path string,
// with the whole of rootfs preparation in between; for a block rootfs that is
// an mke2fs over a multi-gigabyte tree. Anything that can write in the asset
// directory -- which is precisely the capability the record exists to defend
// against, and which brig's own fetcher creates 0755 -- could replace the file
// in that window and the VMM would boot the replacement.
//
// The copy is bounded and refuses anything that is not a regular file, for the
// same reasons as stageImageBootFile, and the destination is opened
// O_EXCL|O_NOFOLLOW so it cannot be redirected either.
func stageHostBootFile(hostPath, instanceDir, staged string) (string, error) {
	src, err := os.OpenFile(hostPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%w %q: %w", errStageBootFile, hostPath, err)
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("%w %q: %w", errStageBootFile, hostPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w %q: it is not a regular file (%s)",
			errStageBootFile, hostPath, info.Mode().Type())
	}

	dest := filepath.Join(instanceDir, staged)
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, hostPath, dest, err)
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxBootFileBytes+1))
	if err == nil && n > maxBootFileBytes {
		err = fmt.Errorf("it is larger than the %d byte limit for a boot file", maxBootFileBytes)
	}
	if err != nil {
		_ = dst.Close()
		_ = os.Remove(dest)
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, hostPath, dest, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, hostPath, dest, err)
	}
	return dest, nil
}

func stageImageBootFile(rootfsDir, name, instanceDir, staged string) (string, error) {
	_, src, err := openImageFile(rootfsDir, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()

	// A fixed destination name, never one derived from the image's: the whole
	// point is that no component of the path the VMM opens comes from the
	// image.
	dest := filepath.Join(instanceDir, staged)
	// O_NOFOLLOW and O_EXCL: this function exists so that no name the VMM
	// opens is re-resolved, and the destination is the one name it does
	// re-resolve. The instance directory is 0700 and freshly created, so
	// nothing image-controlled reaches it today -- which is an argument for
	// why this is cheap, not for leaving it open.
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, name, dest, err)
	}
	// Bounded, because the source is image content and the copy is per
	// instance. The image's own size is capped, but that cap is per layer:
	// nothing stopped a 16 GiB "kernel" being written again for every run and
	// every supervisor restart, where before this staging existed N instances
	// shared one file on disk. A kernel or initrd that exceeds this is not a
	// kernel or initrd.
	n, err := io.Copy(dst, io.LimitReader(src, maxBootFileBytes+1))
	if err == nil && n > maxBootFileBytes {
		err = fmt.Errorf("it is larger than the %d byte limit for a boot file", maxBootFileBytes)
	}
	if err != nil {
		_ = dst.Close()
		_ = os.Remove(dest)
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, name, dest, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("%w %q to %s: %w", errStageBootFile, name, dest, err)
	}
	log.Debugf("Staged image boot file %q as %s", name, dest)
	return dest, nil
}

// containerHosts builds per-instance metadata without changing its source.
// Vz reads it from the immutable virtiofs lower layer; HVI reads it from the
// instance's APFS clone before that clone is mounted as the writable root.
func containerHosts(rootfsDir string, entries []string) (string, error) {
	var out strings.Builder
	// Read through an os.Root: /etc/hosts is a file the image ships and may be
	// a symlink of its choosing, and following an absolute one would copy a
	// host file -- an ssh key, a shell profile -- into the guest's /etc/hosts.
	// A rootfs of "" means there is no metadata source for this instance.
	if data, err := readFromRootfs(rootfsDir, "etc/hosts"); err == nil {
		out.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			out.WriteByte('\n')
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read container /etc/hosts: %w", err)
	}
	for _, entry := range entries {
		idx := strings.IndexAny(entry, ":=")
		if idx <= 0 || idx == len(entry)-1 {
			return "", fmt.Errorf("invalid --add-host entry %q, expected host:ip", entry)
		}
		host, ip := entry[:idx], entry[idx+1:]
		if net.ParseIP(ip) == nil {
			return "", fmt.Errorf("invalid --add-host entry %q: %q is not an IP address", entry, ip)
		}
		fmt.Fprintf(&out, "%s\t%s\n", ip, host)
	}
	return out.String(), nil
}

// launchVMM starts the assembled VMM command line, records the instance
// state, discovers the guest IP, and in foreground mode holds the terminal
// until the VMM exits. Shared by run and restore. The returned bool reports
// whether the VMM process was started — once it was, the caller must keep
// the instance directory even on error.
func launchVMM(cmd *cli.Command, s *store.Store, state *store.InstanceState, cmdArgs []string, gatewayFiles []*os.File, vmmType hypervisors.VmmType, detach bool, netMode, gatewayIP string) (bool, error) {
	vmmCmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	vmmCmd.ExtraFiles = gatewayFiles

	// Save terminal state and restore it on every exit path. For the Vz
	// backend the host tty must be switched to raw ourselves: the guest's
	// line discipline does the echoing and editing, and leaving the host
	// tty canonical double-echoes every keystroke and garbles interactive
	// sessions. (QEMU's stdio chardev sets raw mode itself.) ISIG stays on
	// so Ctrl-C still stops the VM, matching the QEMU backend.
	var origTermios *syscall.Termios
	if !detach {
		// Once the VMM's process group owns the terminal, this process is a
		// background process: touching the tty (the termios restore below,
		// TIOCSPGRP) would raise SIGTTOU, whose default action SUSPENDS the
		// process — leaving a stopped job and a garbled terminal behind
		// under interactive shells. Ignore it, and reclaim the foreground
		// group before restoring so the shell gets back a sane terminal.
		signal.Ignore(syscall.SIGTTOU, syscall.SIGTTIN)
		origTermios, _ = getTermios(int(os.Stdin.Fd()))
		defer restoreTerminal(origTermios)
		defer claimForeground()
		if vmmType == hypervisors.VzVmm && origTermios != nil {
			raw := *origTermios
			raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
			raw.Iflag &^= syscall.ICRNL
			setTermios(&raw)
		}
	}

	if !detach {
		// Foreground mode: give VMM its own process group and make it the
		// foreground group so it can freely read/write the terminal.
		// QEMU's -serial stdio needs terminal access (raw mode) which is
		// blocked for background process groups (SIGTTIN/SIGTTOU).
		//
		// Only when there is a terminal to be in the foreground of. Asking for
		// it without one makes the exec itself fail -- Go performs the
		// tcsetpgrp before handing over, and that returns ENOTSUP on anything
		// that is not a tty, surfacing as
		//
		//	failed to start VMM: fork/exec ...: operation not supported by device
		//
		// which reads like the VMM binary is at fault and is not. Every
		// non-interactive caller hits it: a CI step, a script, a pipeline, any
		// `hull run` whose stdin is a pipe or /dev/null.
		if isTerminal(int(os.Stdin.Fd())) {
			vmmCmd.SysProcAttr = &syscall.SysProcAttr{
				Foreground: true,
				Ctty:       int(os.Stdin.Fd()),
			}
		} else {
			vmmCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		vmmCmd.Stdout = os.Stdout
		vmmCmd.Stderr = os.Stderr
		vmmCmd.Stdin = os.Stdin
	} else {
		// Detached mode: own process group, no terminal access needed
		vmmCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// 0600, not 0644. This is the guest's console: everything the VM
		// prints, which on an agent sandbox includes whatever the agent echoes
		// -- tokens it was given, contents of files it read. The rest of the
		// store is 0600/0700 and this was the one file that was not.
		logFd, err := os.OpenFile(state.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return false, fmt.Errorf("failed to open log file: %w", err)
		}
		vmmCmd.Stdout = logFd
		vmmCmd.Stderr = logFd
	}

	// Publish the record BEFORE the spawn, not after it.
	//
	// The pid is not knowable until the process exists, so the record cannot
	// simply be moved down here whole -- but the pid is not what makes an
	// instance findable, the record is. Writing it only after exec succeeded
	// left a window in which hull had a live VMM and the store had nothing:
	// a SIGKILL of hull in that window (the deferred cleanup does not run for
	// one either) left a VMM invisible to `ps`, unreachable by `stop`, and a
	// directory that squatted the name forever, because CreateInstance kept
	// failing ErrInstanceExists while `rm` answered "instance not found".
	// Publishing first turns the worst case into a StatusStarting record an
	// operator can see and remove; the pid and "running" follow one statement
	// later.
	//
	// A store that cannot be written is fatal here rather than a warning: the
	// whole point is that no VMM starts without a record, so starting one
	// anyway would be the defect with an extra log line.
	prevRecord, prevErr := s.GetInstance(state.ID)
	state.PID = 0
	state.Status = store.StatusStarting
	state.StartTime = time.Now()
	state.CmdLine = cmdArgs
	if err := s.SaveInstance(state); err != nil {
		return false, fmt.Errorf("failed to record instance before starting the VMM: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Starting VMM: %s\n", strings.Join(cmdArgs, " "))
	log.Debugf("Starting VMM: %v", cmdArgs)
	if err := vmmCmd.Start(); err != nil {
		// No process exists, so the record describes nothing and has to go --
		// otherwise every failed run leaves an instance stuck at "starting".
		// Restore comes before removal because restore is also a path: the
		// checkpoint/restore caller hands us an instance that already had a
		// record, and dropping it would lose a checkpointed VM's own state.
		if prevErr == nil {
			if rbErr := s.SaveInstance(prevRecord); rbErr != nil {
				log.WithError(rbErr).Warnf("failed to restore the previous state of instance %s", state.ID)
			}
		} else if rbErr := s.DeleteInstanceRecord(state.ID); rbErr != nil {
			log.WithError(rbErr).Warnf("failed to remove the starting record for instance %s", state.ID)
		}
		sendStartEvent(string(vmmType), false)
		return false, fmt.Errorf("failed to start VMM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "VMM started (PID %d)\n", vmmCmd.Process.Pid)
	sendStartEvent(string(vmmType), true)

	// Now that the process exists, name it. A crash between the two saves
	// leaves the StatusStarting record above, which is recoverable.
	state.PID = vmmCmd.Process.Pid
	state.Status = "running"

	if err := s.SaveInstance(state); err != nil {
		log.WithError(err).Warn("failed to save instance state")
	}

	// Gateway mode: the IP is static and known now.
	if gatewayIP != "" {
		if addr, _, _, err := gatewayNetConfig(gatewayIP); err == nil {
			recordInstanceIP(s, state.ID, addr)
		}
	}

	// Discover the guest IP from the vmnet DHCP leases by MAC and record it.
	// Detached mode waits (callers like compose need the IP); foreground mode
	// discovers in the background while the console is attached.
	if gatewayIP == "" && netMode != "none" {
		if detach && cmd.Bool("wait-ip") {
			if ip, err := waitForLeaseIP(state.MAC, 30*time.Second); err == nil {
				recordInstanceIP(s, state.ID, ip)
			} else {
				log.Debugf("guest IP not discovered: %v", err)
			}
		} else if !detach {
			go func() {
				if ip, err := waitForLeaseIP(state.MAC, 30*time.Second); err == nil {
					recordInstanceIP(s, state.ID, ip)
				}
			}()
		}
	}

	if detach {
		// Print instance ID and return
		fmt.Println(state.ID)
		return true, nil
	}

	// Wait for the VMM (foreground mode).
	//
	// Terminal signals do NOT reach this process: the VMM's process group
	// is the foreground group, so Ctrl-C delivers SIGINT to the VMM
	// directly (QEMU exits; vz-runner requests a graceful guest stop and
	// forces after --stop-grace). This handler only fires for signals sent
	// to this process explicitly (e.g. kill <pid>), where it forwards a
	// graceful stop to the VMM.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigChan)

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- vmmCmd.Wait()
	}()

	metricsDone := make(chan struct{})
	defer close(metricsDone)
	startVMMMetricsSampler(vmmCmd.Process.Pid, string(vmmType), state.StartTime,
		s.InstanceDir(state.ID), metricsDone)

	markStopped := func() {
		state.Status = "stopped"
		state.PID = 0
		if state.ExitedAt.IsZero() {
			state.ExitedAt = time.Now()
		}
		_ = s.SaveInstance(state)
		sendEndOnce(s, state)
	}

	select {
	case <-sigChan:
		log.Debug("Received signal, attempting graceful shutdown")
		// Try QMP powerdown (QEMU only)
		if state.QMPSocket != "" && vmmType == hypervisors.QemuVmm {
			qmpClient, err := qmp.Dial(state.QMPSocket, 5*time.Second)
			if err == nil {
				defer func() { _ = qmpClient.Close() }()
				_ = qmpClient.SystemPowerdown()
			}
		}
		// Send SIGTERM to vz-runner/QEMU
		_ = vmmCmd.Process.Signal(syscall.SIGTERM)

		select {
		case waitErr := <-waitDone:
			markStopped()
			return true, waitErr
		case <-time.After(time.Duration(cmd.Int("stop-grace")+5) * time.Second):
			log.Debug("VMM did not exit in time, force killing")
			_ = vmmCmd.Process.Kill()
			<-waitDone
			markStopped()
			return true, nil
		}
	case err := <-waitDone:
		markStopped()
		return true, err
	}
}

// cachedDigest looks up a locally cached image by ref or digest.
//
// A metadata hit only counts if the unpacked rootfs is there too. An
// interrupted pull can leave image.json without one, and trusting the record
// alone made every later run fail on a missing rootfs with no way out but
// deleting the store by hand. Reporting an incomplete image as a miss lets the
// caller re-pull and heal it.
// A tag can match several stored images: pulling a republished tag adds a new
// digest without retiring the old one. Directory order is meaningless there, so
// take the most recently pulled match, which is the one the user last asked for.
func cachedDigest(s *store.Store, ref, platform string) (string, bool) {
	images, err := s.ListImages()
	if err != nil {
		return "", false
	}
	var best *store.ImageMetadata
	for _, img := range images {
		if img.Ref != ref && img.Digest != ref {
			continue
		}
		// The store is digest-keyed so platform variants of a tag coexist;
		// the lookup must honor that or an arm64 pull satisfies a later
		// --platform linux/amd64 run. Entries from before the field existed
		// were all pulled as the default platform, so they keep matching it.
		entryPlatform := img.Platform
		if entryPlatform == "" {
			entryPlatform = ociclient.DefaultPlatform
		}
		if entryPlatform != platform {
			continue
		}
		if !s.ImageComplete(img.Digest) {
			imageDir := filepath.Join(s.RootDir(), "images", img.Digest)
			if schema := store.ReadUnpackSchema(imageDir); schema != store.UnpackSchema {
				log.Warnf("cached image %s was unpacked by an older hull (layout %d, current %d), "+
					"so it is missing the file ownership and setuid bits the guest needs; re-pulling",
					img.Digest, schema, store.UnpackSchema)
			} else {
				log.Warnf("cached image %s is incomplete (no rootfs); re-pulling", img.Digest)
			}
			continue
		}
		if best == nil || img.PulledAt.After(best.PulledAt) {
			best = img
		}
	}
	if best == nil {
		return "", false
	}
	return best.Digest, true
}

// Pull policies, matching docker's vocabulary.
const (
	pullMissing = "missing" // use the cached image if there is one (default)
	pullAlways  = "always"  // re-resolve the reference and pull it again
	pullNever   = "never"   // never reach the network; fail if not cached
)

func validPullPolicy(p string) bool {
	return p == pullMissing || p == pullAlways || p == pullNever
}

// resolveImageDigest gets the image digest, pulling according to policy.
//
// Note what "missing" means for a tag: a tag is a moving target, and a cached
// entry for it is whatever that tag pointed at when it was first pulled. We do
// not re-resolve it, so a republished tag is never picked up. That is docker's
// behaviour too, and --pull=always is the way out of it. Anything that must
// track a tag (CI, a wrapper that should follow releases) should say so.
func resolveImageDigest(ctx context.Context, client *ociclient.Client, s *store.Store, ref, policy, platform string) (string, error) {
	if policy != pullAlways {
		if digest, ok := cachedDigest(s, ref, platform); ok {
			return digest, nil
		}
		if policy == pullNever {
			return "", fmt.Errorf("image %s is not cached and --pull=never was given", ref)
		}
	}

	// Pull the image
	result, err := client.PullPlatform(ctx, ref, platform)
	if err != nil {
		return "", fmt.Errorf("failed to pull image %s: %w", ref, err)
	}

	return result.Digest, nil
}

// generateID generates a random instance ID
func generateID(length int) string {
	b := make([]byte, length/2)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID
		return fmt.Sprintf("%x", time.Now().UnixNano())[:length]
	}
	return hex.EncodeToString(b)[:length]
}

// generateMAC returns a random MAC (QEMU's traditional 52:54:00 prefix)
// that no other stored instance is using. Discovery is keyed by MAC, so a
// collision would make one instance silently record another's DHCP lease;
// random octets with a uniqueness check beat the previous name-hash scheme,
// whose 24-bit space made collisions consequential and undiagnosable.
func generateMAC(s *store.Store) (string, error) {
	inUse := map[string]bool{}
	if instances, err := s.ListInstances(); err == nil {
		for _, st := range instances {
			if st.MAC != "" {
				inUse[st.MAC] = true
			}
		}
	}
	for attempt := 0; attempt < 32; attempt++ {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("failed to generate MAC: %w", err)
		}
		mac := fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
		if !inUse[mac] {
			return mac, nil
		}
	}
	return "", errors.New("could not find an unused MAC address")
}

// dirSizeBytes returns the total size of all files in a directory tree.
func dirSizeBytes(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// procConfig adapts an OCI process spec to the shared unikernel process
// config, defaulting the working directory to "/". The urunit config blob
// itself is rendered by the shared unikernels.BuildUrunitConfig so the darwin
// product and the Linux engine emit an identical format.
func procConfig(uid, gid uint32, cwd string) types.ProcessConfig {
	if cwd == "" {
		cwd = "/"
	}
	return types.ProcessConfig{UID: uid, GID: gid, WorkDir: cwd}
}
