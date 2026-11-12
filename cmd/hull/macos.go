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

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

// macosBundleFiles are what a macOS guest directory must hold. This is Tart's
// layout, so a VM that `tart clone` or `tart create` made boots as it is:
// config.json carries the hardware model and machine identifier the system was
// installed under, nvram.bin the auxiliary storage that install wrote, and
// disk.img the system itself.
var macosBundleFiles = []string{"config.json", "disk.img", "nvram.bin"}

// macosBundleFlag is the vz-runner flag that names a macOS guest directory.
const macosBundleFlag = "--macos-bundle"

// macosRunFlagsRefused are the `hull run` flags that describe an OCI image or
// a Linux guest. They are refused for a macOS guest, so that none of them is
// dropped without a word.
var macosRunFlagsRefused = []string{
	"shared-dir-fd", "image-disk", "rootfs-type", "no-boot-assets",
	"add-host", "gateway-sock", "gateway-cidr", "rosetta", "platform", "pull",
	"env", "annotation", "qemu-path",
}

// macOS takes far longer to shut down than a Linux microVM, and a guest that
// is killed halfway through can lose data. These replace the defaults of
// `hull run --stop-grace` and `hull stop --timeout` for a macOS guest. The
// stop timeout is the longer one, so the runner's own force-stop comes first.
const (
	macosStopGrace   = 60
	macosStopTimeout = 90
)

// stopTimeoutFor returns the timeout `hull stop` uses for an instance when
// --timeout was not given.
func stopTimeoutFor(s *store.Store, instanceID string, fallback int) int {
	state, err := s.GetInstance(instanceID)
	if err == nil && bundleOfCmdLine(state.CmdLine) != "" {
		return macosStopTimeout
	}
	return fallback
}

// macosRunOptions is what `hull run` passes on to vz-runner for a macOS
// guest. A zero CPUs or MemoryMB leaves the bundle's own sizing in place.
type macosRunOptions struct {
	AgentSock string
	StateDir  string
	StopGrace int
	GUI       bool
	GUITitle  string
	GUIRecord string
	CPUs      int
	MemoryMB  int
	NoNet     bool
	// ConfigDir is shared read-only under macosConfigTag. The guest agent
	// reads the mounts it should make from mounts.json in it.
	ConfigDir string
	Shares    []hostShare
}

// macosConfigTag is the virtiofs tag of the config share. hull-guest-agent
// looks for it by this name.
const macosConfigTag = "hull-config"

// macosMount is one entry of the mount table the guest agent applies at boot.
// The agent defines the same fields.
type macosMount struct {
	Kind     string `json:"kind"`
	Tag      string `json:"tag,omitempty"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	SizeMB   int    `json:"sizeMB,omitempty"`
}

// parseMacOSTmpfs parses --tmpfs /guest/path[:SIZE], where SIZE is a number of
// megabytes with an optional m or g suffix. The default is 64m.
func parseMacOSTmpfs(spec string) (macosMount, error) {
	path, size, hasSize := strings.Cut(spec, ":")
	if !filepath.IsAbs(path) {
		return macosMount{}, fmt.Errorf("--tmpfs %q: the guest path must be absolute", spec)
	}
	m := macosMount{Kind: "ram", Path: filepath.Clean(path), SizeMB: 64}
	if !hasSize {
		return m, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(size, "g"):
		mult, size = 1024, strings.TrimSuffix(size, "g")
	case strings.HasSuffix(size, "m"):
		size = strings.TrimSuffix(size, "m")
	}
	n, err := strconv.Atoi(size)
	if err != nil || n <= 0 {
		return macosMount{}, fmt.Errorf("--tmpfs %q: size must be a number of megabytes, eg. 256m or 1g", spec)
	}
	m.SizeMB = n * mult
	return m, nil
}

// writeMacOSMounts writes the mount table for the shares and RAM volumes into
// dir, which becomes the config share.
func writeMacOSMounts(dir string, shares []hostShare, ram []macosMount) error {
	mounts := []macosMount{}
	for _, sh := range shares {
		mounts = append(mounts, macosMount{Kind: "virtiofs", Tag: sh.tag, Path: sh.guest, ReadOnly: sh.readOnly})
	}
	mounts = append(mounts, ram...)
	data, err := json.MarshalIndent(mounts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "mounts.json"), data, 0o600)
}

// macosBundleConfig is the part of a bundle's config.json that hull reads.
// vz-runner reads the rest.
type macosBundleConfig struct {
	OS         string `json:"os"`
	MACAddress string `json:"macAddress"`
}

// checkMacOSBundle reports the first file dir is missing, so a wrong path is
// named here and not by a framework error once the runner is up.
func checkMacOSBundle(dir string) error {
	for _, name := range macosBundleFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s is not a macOS guest directory: %w", dir, err)
		}
	}
	return nil
}

// readMacOSBundleConfig reads dir's config.json, and refuses one that does
// not describe a macOS guest. Tart keeps Linux VMs in the same layout.
func readMacOSBundleConfig(dir string) (*macosBundleConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg macosBundleConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s/config.json: %w", dir, err)
	}
	if cfg.OS != "darwin" {
		return nil, fmt.Errorf("%s/config.json describes a %q guest, not a macOS one", dir, cfg.OS)
	}
	return &cfg, nil
}

// tartHome is where Tart keeps its VMs.
func tartHome() string {
	if dir := os.Getenv("TART_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tart")
}

// resolveMacOSGuest reports whether ref names a macOS guest, and returns its
// directory. ref is either a path to a guest directory or the name of a local
// Tart VM. A name is only looked up when it cannot be a path or carry a tag,
// so an image reference such as ubuntu:latest never reaches Tart's directory.
func resolveMacOSGuest(ref string) (string, bool) {
	candidates := []string{ref}
	if home := tartHome(); home != "" && !strings.ContainsAny(ref, "/:@") {
		candidates = append(candidates, filepath.Join(home, "vms", ref))
	}
	for _, candidate := range candidates {
		dir, err := filepath.Abs(candidate)
		if err != nil || checkMacOSBundle(dir) != nil {
			continue
		}
		if _, err := readMacOSBundleConfig(dir); err != nil {
			continue
		}
		return dir, true
	}
	return "", false
}

// macosRunnerArgs builds the vz-runner arguments for a macOS guest.
func macosRunnerArgs(dir string, opts macosRunOptions) []string {
	args := []string{macosBundleFlag, dir}
	if opts.AgentSock != "" {
		args = append(args, "--agent-sock", opts.AgentSock)
	}
	if opts.StateDir != "" {
		args = append(args, "--state-dir", opts.StateDir)
	}
	if opts.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(opts.CPUs))
	}
	if opts.MemoryMB > 0 {
		args = append(args, "--mem", strconv.Itoa(opts.MemoryMB))
	}
	if opts.NoNet {
		args = append(args, "--no-net")
	}
	if opts.ConfigDir != "" {
		args = append(args, "--share-ro", opts.ConfigDir, macosConfigTag)
	}
	for _, sh := range opts.Shares {
		flag := "--share"
		if sh.readOnly {
			flag = "--share-ro"
		}
		args = append(args, flag, sh.host, sh.tag)
	}
	if opts.StopGrace > 0 {
		args = append(args, "--stop-grace", strconv.Itoa(opts.StopGrace))
	}
	if opts.GUI {
		args = append(args, "--gui")
		if opts.GUITitle != "" {
			args = append(args, "--gui-title", opts.GUITitle)
		}
		if opts.GUIRecord != "" {
			args = append(args, "--gui-record", opts.GUIRecord)
		}
	}
	return args
}

// vzRunnerPath finds vz-runner next to this executable, the same place the
// Vz backend looks for it.
func vzRunnerPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	runner := filepath.Join(filepath.Dir(exe), "vz-runner")
	if _, err := os.Stat(runner); err != nil {
		return "", fmt.Errorf("vz-runner not found next to hull: %w", err)
	}
	return runner, nil
}

// bundleOfCmdLine returns the macOS guest directory a VMM command line boots,
// or "" for any other instance.
func bundleOfCmdLine(cmdLine []string) string {
	for i := 0; i+1 < len(cmdLine); i++ {
		if cmdLine[i] == macosBundleFlag {
			return cmdLine[i+1]
		}
	}
	return ""
}

// macosBundleInUse returns the instance whose VMM has dir's disk open. The
// guest boots from the bundle's disk in place, so a second VM on it would
// corrupt the system.
func macosBundleInUse(s *store.Store, dir string) (string, error) {
	instances, err := s.ListInstances()
	if err != nil {
		return "", err
	}
	for _, st := range instances {
		if bundleOfCmdLine(st.CmdLine) != dir {
			continue
		}
		if st.Status == store.StatusStarting || processIsAVMM(st.PID, st) {
			return st.ID, nil
		}
	}
	return "", nil
}

// dropStoppedMacOSInstance removes the record a previous run of dir left under
// name, so the guest can be run again under the same name without `hull rm`.
// The system lives in the guest directory, and the record holds only the log.
// A record with a checkpoint is kept, since `hull restore` needs it.
func dropStoppedMacOSInstance(s *store.Store, name, dir string) error {
	st, err := s.GetInstance(name)
	if err != nil {
		return nil
	}
	if st.Status != "stopped" || bundleOfCmdLine(st.CmdLine) != dir {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.InstanceDir(name), "checkpoint", "latest.json")); err == nil {
		return nil
	}
	return s.DeleteInstance(name)
}

// runMacOSGuest is `hull run` for a macOS guest directory.
//
// A macOS guest is a disk with an identity, so nothing is pulled or unpacked
// and the instance holds no bundle of its own: the guest boots from dir in
// place. The instance is recorded like any other, and reaches the guest's
// agent on the usual socket, so ps, exec, logs, stop and rm work on it.
func runMacOSGuest(ctx context.Context, cmd *cli.Command, dir string) error {
	if cmd.Args().Len() > 1 {
		return errors.New("a macOS guest takes no command: run it, then use `hull exec`")
	}
	for _, name := range macosRunFlagsRefused {
		if cmd.IsSet(name) {
			return fmt.Errorf("--%s does not apply to a macOS guest", name)
		}
	}
	if hv := cmd.String("hypervisor"); hv != "" && normalizeVmmName(hv) != string(hypervisors.VzVmm) {
		return fmt.Errorf("a macOS guest runs on the Vz hypervisor only (got %q)", hv)
	}
	netMode := cmd.String("net")
	if netMode != "none" && netMode != "shared" {
		return fmt.Errorf("--net %s: a macOS guest takes none or shared", netMode)
	}
	gui := cmd.Bool("gui")
	guiTitle := cmd.String("gui-title")
	if guiTitle != "" && !gui {
		return errors.New("--gui-title requires --gui")
	}
	guiRecord := cmd.String("gui-record")
	if guiRecord != "" {
		if !gui {
			return errors.New("--gui-record requires --gui")
		}
		abs, err := filepath.Abs(guiRecord)
		if err != nil {
			return err
		}
		guiRecord = abs
	}
	shares, _, err := parseShares(cmd.StringSlice("shared-dir"), nil, string(hypervisors.VzVmm))
	if err != nil {
		return err
	}
	var ram []macosMount
	for _, spec := range cmd.StringSlice("tmpfs") {
		m, err := parseMacOSTmpfs(spec)
		if err != nil {
			return err
		}
		ram = append(ram, m)
	}
	cfg, err := readMacOSBundleConfig(dir)
	if err != nil {
		return err
	}

	// The guest directory names the instance by default. One directory backs
	// one guest, so the name is as unique as the directory.
	instanceName := cmd.String("name")
	if instanceName == "" {
		instanceName = filepath.Base(dir)
	}
	if err := store.ValidateInstanceID(instanceName); err != nil {
		return fmt.Errorf("--name: %w", err)
	}
	if gui && guiTitle == "" {
		guiTitle = instanceName
	}

	ctx, stopSignals := notifyRunSignals(ctx)
	defer stopSignals()

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	if other, err := macosBundleInUse(s, dir); err != nil {
		return err
	} else if other != "" {
		return fmt.Errorf("%s is already running as instance %s", dir, other)
	}
	runner, err := vzRunnerPath()
	if err != nil {
		return err
	}

	if err := dropStoppedMacOSInstance(s, instanceName, dir); err != nil {
		return err
	}
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
	if err := s.SaveInstance(&store.InstanceState{
		ID:        instanceName,
		Status:    store.StatusCreating,
		StartTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to record instance: %w", err)
	}
	// A checkpoint clones the guest's disk, and only an APFS clone makes that
	// cheap. A clone cannot cross volumes, and hull's store is a volume of its
	// own, so the checkpoint lives in the guest directory and the instance's
	// checkpoint directory links to it. A fresh run starts with an empty one.
	ckpt := filepath.Join(dir, "hull-checkpoint")
	if err := os.RemoveAll(ckpt); err != nil {
		return err
	}
	if err := os.MkdirAll(ckpt, 0o700); err != nil {
		return err
	}
	if err := os.Symlink(ckpt, instanceCheckpointDir(s, instanceName)); err != nil {
		return err
	}
	configDir := filepath.Join(instanceDir, "guest-config")
	if err := writeMacOSMounts(configDir, shares, ram); err != nil {
		return fmt.Errorf("failed to write the guest's mount table: %w", err)
	}
	agentSock := s.InstanceAgentSocket(instanceName)
	if err := checkUnixSocketPath("agent", agentSock); err != nil {
		return err
	}

	opts := macosRunOptions{
		AgentSock: agentSock,
		StateDir:  ckpt,
		StopGrace: macosStopGrace,
		GUI:       gui,
		GUITitle:  guiTitle,
		GUIRecord: guiRecord,
		ConfigDir: configDir,
		Shares:    shares,
		NoNet:     netMode == "none",
	}
	// --mem and --cpus default to a Linux microVM's sizing. Only a value the
	// caller gave overrides the bundle's own.
	if cmd.IsSet("stop-grace") {
		opts.StopGrace = int(cmd.Int("stop-grace"))
	}
	if cmd.IsSet("cpus") {
		opts.CPUs = int(cmd.Int("cpus"))
	}
	if cmd.IsSet("mem") {
		opts.MemoryMB = int(cmd.Int("mem"))
	}

	// BundleDir stays empty. It names an OCI bundle inside the instance
	// directory, and the guest directory is neither.
	state := &store.InstanceState{
		ID:      instanceName,
		LogFile: s.InstanceLogFile(instanceName),
		MAC:     cfg.MACAddress,
		Backend: string(hypervisors.VzVmm),
	}
	cmdArgs := append([]string{runner}, macosRunnerArgs(dir, opts)...)
	instanceStarted, err = launchVMM(ctx, cmd, s, state, cmdArgs, nil, hypervisors.VzVmm, cmd.Bool("detach"), netMode, "")
	return err
}
