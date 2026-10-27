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

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/containerd/errdefs"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/imageplatform"
	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// defaultHypervisor is the monitor a plain image boots on. The
// brig-standalone-linux bundle ships cloud-hypervisor and firecracker, and
// no qemu, which is the monitor urunc picks when none is named.
const defaultHypervisor = "cloud-hypervisor"

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "create and run an instance",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "run in background (required on Linux)",
			},
			&cli.StringFlag{
				Name:  "net",
				Value: "none",
				Usage: "network mode: only 'none' on Linux so far",
			},
			pullFlag(),
			memFlag(),
			cpusFlag(),
			nameFlag(),
			&cli.StringFlag{
				Name:  "hypervisor",
				Usage: "urunc monitor to boot with, such as 'cloud-hypervisor' (default: from the image, else cloud-hypervisor for a plain image)",
			},
			annotationFlag(),
			&cli.BoolFlag{
				Name:  "no-boot-assets",
				Usage: "do not boot a generic kernel and initrd: the image or --annotation must name a whole unikernel",
			},
			envFlag(),
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
	// No attach yet: the console goes to the log file, and the run returns
	// once the instance has started.
	if !cmd.Bool("detach") {
		return errors.New("hull run on Linux needs --detach: it cannot attach to the console yet")
	}
	if cmd.String("net") != "none" {
		return fmt.Errorf("--net %s is not supported on Linux yet: an instance has no network, so pass --net none",
			cmd.String("net"))
	}
	pullPolicy := cmd.String("pull")
	if !validPullPolicy(pullPolicy) {
		return fmt.Errorf("invalid --pull %q, expected missing, always or never", pullPolicy)
	}
	annotations, err := parseAnnotationEntries(cmd.StringSlice("annotation"))
	if err != nil {
		return err
	}
	hypervisor := cmd.String("hypervisor")
	if hypervisor != "" {
		err = checkMonitor(hypervisor)
		if err != nil {
			return fmt.Errorf("--hypervisor %s: %w", hypervisor, err)
		}
	}
	env, err := resolveEnvEntries(cmd.StringSlice("env"), os.LookupEnv)
	if err != nil {
		return err
	}
	name := cmd.String("name")
	if name == "" {
		name = generateID(8)
	}
	err = store.ValidateInstanceID(name)
	if err != nil {
		return fmt.Errorf("--name: %w", err)
	}
	platform, err := imageplatform.Parse(defaultPlatform)
	if err != nil {
		return err
	}

	c, err := connectContainerd(ctx, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	img, err := containerd.ImageForRun(ctx, c, imageRef, pullPolicy, platform, cmd.ErrWriter)
	if err != nil {
		return err
	}
	image, err := containerd.LoadRunImage(ctx, img)
	if err != nil {
		return err
	}

	if hypervisor != "" {
		annotations[ociclient.HypervisorAnnotation] = hypervisor
	}
	backend, err := planBoot(annotations, image.Urunc, cmd.Bool("no-boot-assets"), genericBootAssets)
	if err != nil {
		return err
	}
	// The image or --annotation may name the monitor too.
	if backend != "" {
		err = checkMonitor(backend)
		if err != nil {
			return fmt.Errorf("%s=%s: %w", ociclient.HypervisorAnnotation, backend, err)
		}
	}
	err = absBootFiles(annotations)
	if err != nil {
		return err
	}
	err = verifyBootFiles(annotations[ociclient.BootKernelAnnotation], annotations[ociclient.BootInitrdAnnotation])
	if err != nil {
		return err
	}

	logFile, err := instanceLogFile(containerd.ResolveNamespace(cmd.String("namespace")), name)
	if err != nil {
		return err
	}
	unlock, err := lockLogDir(filepath.Dir(logFile))
	if err != nil {
		return err
	}
	defer unlock()
	err = containerd.CreateInstance(ctx, c, image, containerd.RunConfig{
		ID:          name,
		Args:        args.Tail(),
		Env:         env,
		MemoryMB:    int(cmd.Int("mem")),
		CPUs:        int(cmd.Int("cpus")),
		Annotations: annotations,
		Backend:     backend,
		LogFile:     logFile,
	})
	if errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("%w: %s", store.ErrInstanceExists, name)
	}
	if errors.Is(err, containerd.ErrNotVM) {
		return fmt.Errorf("%w. Give the unikernel type, monitor and binary with --annotation. "+
			"An image that is not a unikernel boots a generic kernel when neither --no-boot-assets "+
			"nor a %s annotation is given", err, ociclient.UnikernelBinaryAnnotation)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.Writer, name)
	return err
}

// planBoot decides how a run boots and records it in annotations, the run's
// own urunc annotations, given the image's. It returns the monitor the run
// boots on, or "" when neither names one.
//
// An image that names no unikernel binary, in its annotations or the
// run's, boots the kernel and initrd generic returns, the way brig boots
// one, unless noBootAssets says not to or the run names its own.
// urunc boots a host kernel on qemu when no monitor is named, and the
// bundle ships no qemu, so such a boot defaults to cloud-hypervisor.
//
// A boot with a kernel and initrd is a Linux guest, so its unikernel type
// defaults to linux, as macOS's unikernelFamily has it. The bundle's urunc
// would fill the type in itself, but a urunc that does not leaves the
// spec invalid and runs the container with runc, so the spec names it.
func planBoot(annotations, image map[string]string, noBootAssets bool,
	generic func() (kernel, initrd string, err error)) (backend string, err error) {
	spec := containerd.UruncAnnotations(annotations, image)
	containerBoot := spec[ociclient.BootKernelAnnotation] != "" || spec[ociclient.BootInitrdAnnotation] != ""
	if !containerBoot && spec[ociclient.UnikernelBinaryAnnotation] == "" && !noBootAssets {
		kernel, initrd, err := generic()
		if err != nil {
			return "", fmt.Errorf("this image carries no kernel, so it needs a generic kernel and initrd: %w", err)
		}
		annotations[ociclient.BootKernelAnnotation] = kernel
		annotations[ociclient.BootInitrdAnnotation] = initrd
		containerBoot = true
	}
	if containerBoot && spec[ociclient.UnikernelTypeAnnotation] == "" {
		annotations[ociclient.UnikernelTypeAnnotation] = "linux"
	}
	backend = spec[ociclient.HypervisorAnnotation]
	if backend == "" && containerBoot {
		backend = defaultHypervisor
		annotations[ociclient.HypervisorAnnotation] = backend
	}
	return backend, nil
}

// checkMonitor refuses a monitor urunc would refuse only when the task is
// created, after the container and its snapshot exist.
func checkMonitor(name string) error {
	if name == "vz" || name == "hvi" {
		return errors.New("macOS only. On Linux urunc boots the instance, with a monitor such as cloud-hypervisor")
	}
	if !slices.Contains(containerd.Monitors, name) {
		return fmt.Errorf("urunc has no such monitor, only %s", strings.Join(containerd.Monitors, ", "))
	}
	return nil
}

// absBootFiles makes the run's boot kernel and initrd absolute. urunc takes
// only absolute paths, and refuses others when the task is created, after
// the container exists; the shim would otherwise open a relative one from
// its own working directory.
func absBootFiles(annotations map[string]string) error {
	for _, k := range []string{ociclient.BootKernelAnnotation, ociclient.BootInitrdAnnotation} {
		if annotations[k] == "" {
			continue
		}
		abs, err := filepath.Abs(annotations[k])
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		annotations[k] = abs
	}
	return nil
}

// verifyBootFiles checks the kernel and initrd a run boots, however the run
// arrived at them, and refuses half a pair, both as macOS does. A file in a
// runtime bundle's directory is checked against the bundle's SHA256SUMS.
// Any other is checked as macOS checks it, against the provenance record
// `hull assets pull` writes beside it, with a warning when there is none.
// Each file is checked on its own, so a bundle's kernel is checked against
// the bundle even beside an initrd from elsewhere.
func verifyBootFiles(kernel, initrd string) error {
	err := checkBootPair(kernel, initrd)
	if err != nil {
		return err
	}
	if kernel == "" {
		return nil
	}
	ok, err := bootassets.VerifyBundleFile(kernel)
	if err != nil {
		return err
	}
	if ok {
		kernel = ""
	}
	ok, err = bootassets.VerifyBundleFile(initrd)
	if err != nil {
		return err
	}
	if ok {
		initrd = ""
	}
	// verifyContainerBootAssets skips an empty path, so a file the bundle's
	// SHA256SUMS vouched for is not looked for in a provenance record too.
	return verifyContainerBootAssets(kernel, initrd)
}

// bundleGuest finds the runtime bundle's guest files. A variable, so tests
// can stand in for a host with no bundle installed.
var bundleGuest = bootassets.BundleGuest

// genericBootAssets returns the kernel and initrd a plain image boots: the
// runtime bundle's, or the local build HULL_BOOT_ASSETS names.
//
// It never falls back to the boot assets `hull assets pull` fetches from
// ghcr.io/nofireai/hull-assets, nor fetches them. The linux-amd64 initrd
// published there does not boot under the bundle's urunc: the guest reports
// that it cannot find the container's command. Should a published bundle
// boot under urunc on Linux, this is where the fallback would go.
func genericBootAssets() (kernel, initrd string, err error) {
	kernel, initrd, ok, err := bundleGuest()
	if err != nil || ok {
		return kernel, initrd, err
	}
	if os.Getenv("HULL_BOOT_ASSETS") != "" {
		return bootassets.Check("")
	}
	return "", "", fmt.Errorf("no %s and %s are installed, and hull on Linux does not fetch them. "+
		"Install the brig-standalone-linux bundle, which ships them, point HULL_BOOT_ASSETS at a "+
		"directory that holds them, or pass their paths with --annotation %s=<path> and "+
		"--annotation %s=<path>",
		bootassets.KernelName(), bootassets.InitrdName,
		ociclient.BootKernelAnnotation, ociclient.BootInitrdAnnotation)
}

// instanceLogFile returns where the console of instance id in namespace is
// written, and creates its directory.
func instanceLogFile(namespace, id string) (string, error) {
	path, err := instanceLogPath(namespace, id)
	if err != nil {
		return "", err
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return "", fmt.Errorf("create the log directory: %w", err)
	}
	return path, nil
}

// rootLogDir is where root's instance logs go. A variable, so tests run as
// root can point it at a directory of their own.
var rootLogDir = "/var/log/hull"

// lockLogDir takes an exclusive lock on dir, a namespace's log directory,
// and returns what releases it.
//
// hull run holds it from creating an instance until the instance's shim
// has opened its log, and hull rm from removing an instance until its log
// is gone. A run of a name whose old instance rm has removed, but whose old
// log it has not, would otherwise have its new log deleted by that rm. The
// lock is flock(2)'s, so it goes when the process does.
func lockLogDir(dir string) (unlock func(), err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() { _ = f.Close() }, nil
}

// instanceLogPath returns where the console of instance id in namespace is
// written. Root's logs go under rootLogDir; anyone else's under
// $XDG_STATE_HOME/hull, or ~/.local/state/hull, so a rootless daemon's shim,
// which runs as the same user, can write them.
func instanceLogPath(namespace, id string) (string, error) {
	base := rootLogDir
	if os.Geteuid() != 0 {
		state := os.Getenv("XDG_STATE_HOME")
		if !filepath.IsAbs(state) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("find the log directory: %w", err)
			}
			state = filepath.Join(home, ".local", "state")
		}
		base = filepath.Join(state, "hull")
	}
	return filepath.Join(base, namespace, id+".log"), nil
}
