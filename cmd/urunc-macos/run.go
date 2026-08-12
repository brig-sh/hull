// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/nofireai/urunc-macos/pkg/ociclient"
	"github.com/nofireai/urunc-macos/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/qmp"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"

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
				Usage: "share a host directory with the guest: /host/path:/guest/path (virtiofs, repeatable on Vz)",
			},
			&cli.StringFlag{
				Name:  "hypervisor",
				Value: "",
				Usage: "override hypervisor backend: 'qemu' or 'vz' (default: from image annotation)",
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
				Usage: "join the user-mode network gateway at this control socket (replaces NAT, Vz only)",
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
	uruncJSON := filepath.Join(bundleDir, "rootfs", "urunc.json")
	if data, err := os.ReadFile(uruncJSON); err == nil {
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

	unikernelType := "rumprun"
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
	if gatewaySock != "" && vmmType == hypervisors.QemuVmm {
		// QEMU connects to the gateway's stream socket itself (-netdev
		// stream, requires QEMU >= 7.2); no fd passing needed. Never fall
		// back to vmnet, which needs the restricted com.apple.vm.networking
		// entitlement. Pre-flight the socket so a down gateway surfaces as
		// the same friendly error the Vz path gets, instead of a -netdev
		// connect failure buried in the VMM log.
		qsock := qemuGatewaySock(gatewaySock)
		if conn, err := net.DialTimeout("unix", qsock, 2*time.Second); err != nil {
			return fmt.Errorf("failed to reach network gateway at %s: %w", qsock, err)
		} else {
			_ = conn.Close()
		}
		netParams.UnixSocket = qsock
		netParams.TapDev = ""
	}

	// Extract kernel and rootfs paths from annotations (for Linux kernels on darwin)
	kernelPath := ""
	if binary, ok := ociSpec.Annotations["com.urunc.unikernel.binary"]; ok && binary != "" && binary != "unikernel" {
		// Use binary annotation (set by bunny via urunc.json)
		kernelPath = filepath.Join(bundleDir, "rootfs", binary)
	} else if kernel, ok := ociSpec.Annotations["com.urunc.unikernel.kernel"]; ok && kernel != "" && kernel != "unikernel" {
		kernelPath = filepath.Join(bundleDir, "rootfs", kernel)
	} else if vmmType == hypervisors.VzVmm {
		// For Vz backend, look for common kernel locations in rootfs
		rootfsDir := filepath.Join(bundleDir, "rootfs")
		commonPaths := []string{
			filepath.Join(rootfsDir, "vmlinuz"),
			filepath.Join(rootfsDir, "kernel"),
			filepath.Join(rootfsDir, ".boot", "kernel"),
			filepath.Join(rootfsDir, "boot", "vmlinuz"),
		}
		for _, path := range commonPaths {
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				kernelPath = path
				log.Debugf("Found kernel at: %s", kernelPath)
				break
			}
		}
	}

	// Determine rootfs mode: initrd file vs directory (virtiofs/9pfs/block)
	initrdPath := ""
	rootfsDir := ""
	rootfsDiskImage := "" // path to ext4 disk image (block mode)
	mountRootfs := ociSpec.Annotations["com.urunc.unikernel.mountRootfs"] == "true"

	if initrd, ok := ociSpec.Annotations["com.urunc.unikernel.initrd"]; ok && initrd != "" {
		candidate := filepath.Join(bundleDir, "rootfs", initrd)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			initrdPath = candidate
		}
	}

	// When mountRootfs=true (or no initrd), use the container rootfs directory.
	if initrdPath == "" {
		rootfsLink := filepath.Join(bundleDir, "rootfs")

		// Determine rootfs sharing mode
		rootfsType := rootfsTypeOverride
		if rootfsType == "" {
			if vmmType == hypervisors.VzVmm {
				rootfsType = "virtiofs"
			} else {
				rootfsType = "9pfs"
			}
		}

		if rootfsType == "block" {
			// Block mode: create ext4 disk image from rootfs directory.
			// Clone the image rootfs into the instance dir first: the
			// wrapper/config/hosts writes below must never land in the
			// shared store image (appends like /etc/hosts would leak into
			// every future instance of this image).
			srcDir := filepath.Join(instanceDir, "rootfs-staging")
			cloneSrc := rootfsLink
			if target, err := os.Readlink(rootfsLink); err == nil {
				cloneSrc = target
			}
			cpCmd := exec.Command("/bin/cp", "-c", "-a", cloneSrc, srcDir)
			if _, err := cpCmd.CombinedOutput(); err != nil {
				log.Debugf("APFS clone failed (%v), falling back to regular copy", err)
				cpCmd = exec.Command("/bin/cp", "-a", cloneSrc, srcDir)
				if out, err := cpCmd.CombinedOutput(); err != nil {
					return fmt.Errorf("failed to stage rootfs: %s: %w", string(out), err)
				}
			}

			// Write urunit config and init wrapper into the source before imaging
			initBin := ""
			if len(cmdLine) > 0 {
				initBin = cmdLine[0]
			}
			if strings.Contains(initBin, "urunit") {
				urunitConf := unikernels.BuildUrunitConfig(ociSpec.Process.Env, procConfig(ociSpec.Process.User.UID, ociSpec.Process.User.GID, ociSpec.Process.Cwd), nil, "")
				confPath := filepath.Join(srcDir, "urunit.conf")
				if err := os.WriteFile(confPath, []byte(urunitConf), 0600); err != nil {
					log.WithError(err).Warn("failed to write urunit.conf to rootfs for block image")
				}

				// Simple init wrapper: mount devpts, resolv.conf, then exec urunit.
				// No overlay needed — ext4 block device supports all POSIX operations natively.
				wrapperPath := filepath.Join(srcDir, ".block-init")
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
				if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err != nil {
					log.WithError(err).Warn("failed to write block init wrapper")
				}
			}

			diskPath := filepath.Join(bundleDir, "rootfs.ext4")
			log.Debugf("Creating ext4 disk image from %s", srcDir)

			if err := injectHosts(srcDir, addHosts); err != nil {
				return fmt.Errorf("failed to inject /etc/hosts entries: %w", err)
			}

			// Calculate directory size for image sizing
			dirSize, _ := dirSizeBytes(srcDir)
			// Add 50% headroom, minimum 512MB
			imageSizeMB := int(dirSize/(1024*1024)) * 3 / 2
			if imageSizeMB < 512 {
				imageSizeMB = 512
			}

			mke2fs := "/opt/homebrew/opt/e2fsprogs/sbin/mke2fs"
			mkCmd := exec.Command(mke2fs, "-t", "ext4", "-d", srcDir, "-L", "rootfs", "-m", "0", "-q",
				diskPath, fmt.Sprintf("%dM", imageSizeMB))
			if out, err := mkCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("failed to create ext4 image: %s: %w", string(out), err)
			}
			log.Debugf("Created ext4 image: %s (%d MB)", diskPath, imageSizeMB)
			rootfsDiskImage = diskPath
		} else {
			// virtiofs or 9pfs: clone the rootfs directory
			if target, err := os.Readlink(rootfsLink); err == nil {
				log.Debugf("Cloning rootfs from %s (APFS copy-on-write)", target)
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
			}
			rootfsDir = rootfsLink
			if err := injectHosts(rootfsDir, addHosts); err != nil {
				return fmt.Errorf("failed to inject /etc/hosts entries: %w", err)
			}
		}
	}

	// Parse shared directories: on Vz each gets its own virtiofs tag and is
	// mounted at its guest path by the init wrapper; QEMU keeps the legacy
	// single 9pfs share (tag fs0, guest mounts it manually).
	type hostShare struct{ host, guest, tag string }
	var shares []hostShare
	for i, sd := range sharedDirs {
		parts := strings.Split(sd, ":")
		if len(parts) == 3 {
			// Bind mode suffix (host:guest:ro|rw). Read-only is not
			// enforceable yet; accept it so common compose files parse.
			switch parts[2] {
			case "ro":
				log.Warnf("shared-dir %q: read-only mode is not enforced yet, mounting read-write", sd)
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
		shares = append(shares, hostShare{hostPath, guestPath, fmt.Sprintf("share%d", i)})
	}
	shareMounts := ""
	for _, sh := range shares {
		if vmmType == hypervisors.VzVmm {
			shareMounts += fmt.Sprintf("mkdir -p %s\nmount -t virtiofs %s %s\n", sh.guest, sh.tag, sh.guest)
		} else {
			shareMounts += fmt.Sprintf("mkdir -p %s\nmount -t 9p -o trans=virtio,version=9p2000.L,msize=512000 %s %s\n", sh.guest, sh.tag, sh.guest)
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

	if rootfsDir != "" && vmmType == hypervisors.VzVmm {
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
			confPath := filepath.Join(rootfsDir, "urunit.conf")
			if err := os.WriteFile(confPath, []byte(urunitConf), 0600); err != nil {
				log.WithError(err).Warn("failed to write urunit.conf")
			} else {
				log.Debugf("Wrote urunit.conf to %s", confPath)
				kernelCmdline += " URUNIT_CONFIG=/urunit.conf"
			}

			// Create init wrapper that:
			// 1. Mounts an overlayfs with tmpfs upper layer over the virtiofs root.
			//    This works around macOS virtiofs not allowing writes to mode-000 files
			//    (dpkg creates temp files with mode 0, which fails on macOS virtiofs).
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
			wrapperPath := filepath.Join(rootfsDir, ".vz-init")
			var wrapperContent string
			if rosetta {
				bbPath := filepath.Join(rootfsDir, ".rosetta", "busybox")
				if info, err := os.Stat(bbPath); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
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
# Start the exec agent when the image ships one (see urunc-macos exec).
# Its log goes to the console: the overlay upper layer is a tmpfs, so a
# file would not be reachable from the host.
[ -x /urunit-agent ] && /urunit-agent >/dev/hvc0 2>&1 &
exec %s "$@"
`, shareMounts, initBin)
			}
			if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err != nil {
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
			confPath := filepath.Join(rootfsDir, "urunit.conf")
			if err := os.WriteFile(confPath, []byte(urunitConf), 0600); err != nil {
				log.WithError(err).Warn("failed to write urunit.conf")
			} else {
				log.Debugf("Wrote urunit.conf to %s", confPath)
				kernelCmdline += " URUNIT_CONFIG=/urunit.conf"
			}

			// Init wrapper for QEMU: mount devpts, populate resolv.conf, then exec urunit.
			// No overlay needed — QEMU's 9pfs with security_model=none handles writes directly.
			wrapperPath := filepath.Join(rootfsDir, ".qemu-init")
			wrapperContent := fmt.Sprintf(`#!/bin/sh
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /tmp
%s# Populate /etc/resolv.conf from kernel DHCP response
mount -t proc proc /proc 2>/dev/null
if [ -f /proc/net/pnp ]; then cp /proc/net/pnp /etc/resolv.conf; fi
umount /proc 2>/dev/null
# Start the exec agent when the image ships one (see urunc-macos exec)
[ -x /urunit-agent ] && /urunit-agent >/urunit-agent.log 2>&1 &
exec %s "$@"
`, shareMounts, initBin)
			if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err != nil {
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
	var sharedDirParams []types.SharedDirParams
	for _, sh := range shares {
		sharedDirParams = append(sharedDirParams, types.SharedDirParams{Path: sh.host, Tag: sh.tag})
		log.Debugf("Shared directory: host=%s guest=%s tag=%s", sh.host, sh.guest, sh.tag)
	}

	execArgs := types.ExecArgs{
		ContainerID:   instanceName,
		UnikernelPath: filepath.Join(bundleDir, "rootfs", "unikernel"),
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
		cmdArgs = append(cmdArgs, "--stop-grace", strconv.Itoa(int(cmd.Int("stop-grace"))))
		// Every Vz instance is checkpoint-ready: the state dir persists the
		// machine identifier (restore demands the identical identity) and
		// receives `urunc-macos checkpoint` snapshots. vz-runner drops the
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
		vmmCmd.SysProcAttr = &syscall.SysProcAttr{
			Foreground: true,
			Ctty:       int(os.Stdin.Fd()),
		}
		vmmCmd.Stdout = os.Stdout
		vmmCmd.Stderr = os.Stderr
		vmmCmd.Stdin = os.Stdin
	} else {
		// Detached mode: own process group, no terminal access needed
		vmmCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		logFd, err := os.OpenFile(state.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return false, fmt.Errorf("failed to open log file: %w", err)
		}
		vmmCmd.Stdout = logFd
		vmmCmd.Stderr = logFd
	}

	fmt.Fprintf(os.Stderr, "Starting VMM: %s\n", strings.Join(cmdArgs, " "))
	log.Debugf("Starting VMM: %v", cmdArgs)
	if err := vmmCmd.Start(); err != nil {
		sendStartEvent(string(vmmType), false)
		return false, fmt.Errorf("failed to start VMM: %w", err)
	}
	fmt.Fprintf(os.Stderr, "VMM started (PID %d)\n", vmmCmd.Process.Pid)
	sendStartEvent(string(vmmType), true)

	// Save instance state
	state.PID = vmmCmd.Process.Pid
	state.Status = "running"
	state.StartTime = time.Now()
	state.CmdLine = cmdArgs

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
			log.Warnf("cached image %s is incomplete (no rootfs); re-pulling", img.Digest)
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
