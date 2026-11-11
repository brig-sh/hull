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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/moby/sys/userns"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/internal/cni"
	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/pkg/ociclient"
	"github.com/brig-sh/hull/pkg/store"
)

// networkResultFile is the file the hook keeps in an instance's network
// directory while the instance is attached: the address, in cni.Result's
// JSON.
const networkResultFile = "network.json"

// detachPendingFile is the file the hook leaves in an instance's network
// directory when its detach at postStop fails, so that hull rm runs the
// detach again.
const detachPendingFile = "detach-pending"

// The places CNI looks for plugins and network configuration when neither
// the environment nor an installed bundle names others. Variables, so tests
// can point them at directories of their own.
var (
	defaultCNIPath    = "/opt/cni/bin"
	defaultNetConfDir = "/etc/cni/net.d"
)

// bundleDirs lists where a bundle may be installed. A variable, so tests
// can stand in for a host whose system bundle they cannot control.
var bundleDirs = bootassets.BundleDirs

// runNetwork returns how a run of instance id in namespace is attached to
// the network --net mode names: nil for none. shared attaches it to
// cni.DefaultNetwork, through the hook that `hull internal oci-hook` runs.
func runNetwork(ctx context.Context, mode, namespace, id string) (*containerd.Network, error) {
	switch mode {
	case "none":
		return nil, nil
	case "shared":
	default:
		return nil, fmt.Errorf("invalid --net %q, expected none or shared", mode)
	}
	cfg, err := cniConfig()
	if err != nil {
		return nil, err
	}
	err = cfg.Check()
	if err != nil {
		return nil, err
	}
	stateDir := containerd.RootlessKitStateDir()
	var rootlessDNS []string
	if stateDir != "" {
		rootlessDNS, err = cni.RootlessKitDNS(ctx, stateDir)
		if errors.Is(err, cni.ErrRootlessKitNotRunning) {
			// The state directory is a stopped rootlesskit's, so no
			// rootless daemon runs under it: the hook stays in the
			// daemon's network namespace, and the guest gets the host's
			// nameservers. With hull's default address the run then
			// fails when it dials that rootless daemon, an error about
			// the daemon rather than about rootlesskit's DNS.
			stateDir = ""
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	resolv, err := guestResolvConf(rootlessDNS)
	if err != nil {
		return nil, err
	}
	dir, err := instanceNetDir(namespace, id)
	if err != nil {
		return nil, err
	}
	// The runtime runs the hook from its own working directory and inside
	// a rootless daemon's mount namespace, where the binary is found by
	// its absolute path, not hull's PATH.
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find hull's executable for the network hook: %w", err)
	}
	args := []string{exe, "internal", "oci-hook",
		"--namespace", namespace,
		"--network", cfg.Network,
		"--cni-netconfpath", cfg.ConfDir,
		"--dir", dir,
	}
	for _, p := range cfg.PluginDirs {
		args = append(args, "--cni-path", p)
	}
	if stateDir != "" {
		args = append(args, "--rootlesskit-state-dir", stateDir)
	}
	return &containerd.Network{
		Mode: mode,
		Hook: specs.Hook{
			Path: exe,
			Args: args,
			// The runtime gives a hook only the environment the spec names.
			// The bridge and firewall plugins run iptables, which a PATH
			// without the sbin directories does not find.
			Env: []string{"PATH=" + hookPath(os.Getenv("PATH"))},
		},
		Dir:        dir,
		ResolvConf: resolv,
	}, nil
}

// gatewayNetwork returns how a run joins the user-mode network gateway whose
// control socket is controlSock, at the address cidr names, and records it
// in annotations, the run's own urunc annotations. The flags are macOS's,
// and so is the rest: hvi joins the gateway through the QEMU stream socket
// beside the control socket, which qemuGatewaySock names, and the guest
// routes through the subnet's first address, gatewayNetConfig's, which is
// also its nameserver, the gateway's resolver.
//
// urunc has hvi join the gateway itself, so no hook runs and no CNI network
// takes part, and the guest's traffic never passes through the container's
// network namespace, rootlesskit's included. The network's directory holds
// only the guest's resolv.conf.
//
// urunc refuses the socket and the address it refuses here too (see
// validateNetGateway and gatewayNet in urunc's pkg/unikontainers), but only
// when the task is created, after the image is pulled and the container
// exists.
func gatewayNetwork(netMode, controlSock, cidr, namespace, id string, annotations map[string]string) (*containerd.Network, error) {
	if controlSock == "" || cidr == "" {
		return nil, errors.New("--gateway-sock and --gateway-cidr must be used together")
	}
	switch netMode {
	case "shared":
	case "none":
		return nil, errors.New("--net none cannot be combined with --gateway-sock: one asks for no " +
			"network device at all and the other attaches the guest to the gateway")
	default:
		return nil, fmt.Errorf("invalid --net %q, expected none or shared", netMode)
	}
	gateway, err := gatewayAddress(cidr)
	if err != nil {
		return nil, err
	}
	// urunc takes only an absolute path. The flag takes a relative one, as
	// on macOS, relative to hull's working directory.
	control, err := filepath.Abs(controlSock)
	if err != nil {
		return nil, err
	}
	sock := qemuGatewaySock(control)
	err = checkGatewaySock(sock)
	if err != nil {
		return nil, err
	}
	dir, err := instanceNetDir(namespace, id)
	if err != nil {
		return nil, err
	}
	annotations[containerd.NetSocketAnnotation] = sock
	annotations[containerd.NetAddressAnnotation] = cidr
	annotations[containerd.NetGatewayAnnotation] = gateway
	return &containerd.Network{
		Mode:          gatewayMode,
		GatewaySocket: control,
		Dir:           dir,
		ResolvConf:    []byte("nameserver " + gateway + "\n"),
	}, nil
}

// gatewayMode is the network a gateway run records in its container's
// LabelNetwork, which hull ps and hull inspect print. The --net mode,
// shared, would name the CNI network the run does not join.
const gatewayMode = "gateway"

// gatewayAddress returns the gateway's address for a guest at cidr, the
// subnet's first, as gatewayNetConfig derives it, and refuses an address
// urunc refuses: one that is not unicast IPv4, a /32, whose subnet has no
// room for the gateway, the gateway's own address, and the subnet's network
// or broadcast address.
func gatewayAddress(cidr string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsGlobalUnicast() || prefix.Bits() == 0 {
		return "", fmt.Errorf("invalid --gateway-cidr %q: expected a unicast IPv4 address with its prefix, "+
			"such as 10.87.0.10/24", cidr)
	}
	// gatewayNetConfig would add one to the last byte of the address
	// itself, and wrap at 255.
	if prefix.Bits() == 32 {
		return "", fmt.Errorf("invalid --gateway-cidr %q: a /32 has no room for the gateway", cidr)
	}
	_, gateway, _, err := gatewayNetConfig(cidr)
	if err != nil {
		return "", err
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return "", err
	}
	if !gw.IsGlobalUnicast() {
		return "", fmt.Errorf("invalid --gateway-cidr %q: the gateway's address, %s, is not unicast", cidr, gw)
	}
	if gw == prefix.Addr() {
		return "", fmt.Errorf("invalid --gateway-cidr %q: %s is the gateway's own address, the subnet's first",
			cidr, gw)
	}
	if isNetworkOrBroadcast(prefix, prefix.Addr()) {
		return "", fmt.Errorf("invalid --gateway-cidr %q: %s is the network or broadcast address of %s",
			cidr, prefix.Addr(), prefix.Masked())
	}
	return gateway, nil
}

// isNetworkOrBroadcast reports whether addr is the first or the last address
// of prefix, which name the network and its broadcast on any prefix shorter
// than /31. A /31 has no broadcast: its two addresses are its two hosts.
func isNetworkOrBroadcast(prefix netip.Prefix, addr netip.Addr) bool {
	if prefix.Bits() >= 31 {
		return false
	}
	network := prefix.Masked().Addr().As4()
	broadcast := network
	hostBits := 32 - prefix.Bits()
	for i := 3; i >= 0 && hostBits > 0; i-- {
		n := min(hostBits, 8)
		broadcast[i] |= byte(1<<n - 1)
		hostBits -= n
	}
	a := addr.As4()
	return a == network || a == broadcast
}

// hostPathRe is the set of characters urunc takes in a host path
// annotation (validateAnnotationHostPath).
var hostPathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// checkGatewaySock refuses a stream socket, an absolute path, that urunc
// would refuse or hvi could not join. hvi does not fail on a gateway it
// cannot reach: it boots the guest on a stub network with no way out.
// So the gateway must answer, as macOS's run checks too. A gateway that
// was killed leaves its socket behind, and only a connection tells the
// two apart. urunc refuses a symlink in the socket's place, to keep a
// path its user controls from leading root's bind mount elsewhere.
func checkGatewaySock(sock string) error {
	if !hostPathRe.MatchString(sock) {
		return fmt.Errorf("--gateway-sock: urunc takes only letters, digits, '.', '_', '-' and '/' in the "+
			"socket's path, %s", sock)
	}
	info, err := os.Lstat(sock)
	if err != nil {
		return fmt.Errorf("no gateway listens for hvi at %s: %w", sock, err)
	}
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("no gateway listens for hvi at %s: it is not a socket, and urunc takes nothing "+
			"else in its place, a symlink to one included", sock)
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return fmt.Errorf("no gateway listens for hvi at %s: %w", sock, err)
	}
	_ = conn.Close()
	return nil
}

// gatewayAnnotations are the urunc annotations a gateway run sets from
// --gateway-sock and --gateway-cidr.
var gatewayAnnotations = []string{
	containerd.NetSocketAnnotation,
	containerd.NetAddressAnnotation,
	containerd.NetGatewayAnnotation,
}

// checkGatewayAnnotations refuses the gateway's annotations in --annotation.
// Set there, they would skip every check gatewayNetwork makes, and the
// guest's resolv.conf, and with --net shared the CNI network would attach
// an address the guest never uses.
func checkGatewayAnnotations(annotations map[string]string) error {
	for _, k := range gatewayAnnotations {
		_, ok := annotations[k]
		if ok {
			return fmt.Errorf("--annotation %s is refused: --gateway-sock and --gateway-cidr set it", k)
		}
	}
	return nil
}

// gatewayMonitor is the one monitor urunc joins to a gateway. It gives
// every other monitor a tap in the container's network namespace.
const gatewayMonitor = "hvi"

// defaultGatewayMonitor has a gateway run boot on hvi when neither the run
// nor the image, whose urunc annotations are image, names a monitor.
// planBoot would otherwise give a plain image cloud-hypervisor, which
// cannot join a gateway.
func defaultGatewayMonitor(annotations, image map[string]string) {
	if containerd.UruncAnnotations(annotations, image)[ociclient.HypervisorAnnotation] == "" {
		annotations[ociclient.HypervisorAnnotation] = gatewayMonitor
	}
}

// checkGatewayMonitor refuses a gateway run on a monitor other than hvi,
// which urunc refuses only when the task is created.
func checkGatewayMonitor(backend string) error {
	if backend == gatewayMonitor {
		return nil
	}
	return fmt.Errorf("--gateway-sock needs the hvi monitor on Linux, and the run's is %q: urunc joins "+
		"only hvi to a gateway, and the others to the network of the container's namespace", backend)
}

// gatewayJoinTimeout bounds how long checkJoinedGateway waits for urunc to
// start hvi.
var gatewayJoinTimeout = 10 * time.Second

// procCmdline returns the arguments of the process pid.
var procCmdline = func(pid uint32) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// checkJoinedGateway fails a gateway run whose hvi did not join the
// gateway. A urunc that does not read the gateway's annotations, such as
// the brig-standalone-linux bundle's 3c00b69, ignores them and gives hvi a
// tap in the container's network namespace instead, where no network is
// attached, so the guest boots with none and nothing reports it. hull
// cannot tell beforehand which urunc the shim runs, so it reads what urunc
// started: the task's process, pid, execs hvi once urunc has set the
// instance up, and hvi joins a gateway only with --net-gateway.
//
// The process starts as urunc, so the check waits for hvi, for
// gatewayJoinTimeout at most. A task seen to end before then is not the
// gateway's to report. When the check cannot see hvi at all, it warns on
// w, and leaves the instance running.
//
// A process missing at the first read is not taken as a task that ended.
// It may have, but /proc can also hide it from hull: mounted with
// hidepid=invisible, it hides another user's processes, and a containerd in
// another PID namespace, as rootlesskit's --pidns puts it, reports a pid
// that hull's /proc does not have. hull cannot tell an ended task from a
// hidden one, and taking a hidden one for ended would let a guest with no
// network pass unreported, so the check warns.
func checkJoinedGateway(ctx context.Context, w io.Writer, pid uint32) error {
	deadline := time.Now().Add(gatewayJoinTimeout)
	seen := false
	for {
		args, err := procCmdline(pid)
		if errors.Is(err, fs.ErrNotExist) {
			if !seen {
				_, _ = fmt.Fprintf(w, "warning: hull cannot see the task's process %d, so it cannot tell whether hvi "+
					"joined the network gateway: the task ended at once, or /proc hides the process from hull\n", pid)
			}
			return nil
		}
		if err != nil {
			_, _ = fmt.Fprintf(w, "warning: cannot tell whether hvi joined the network gateway: %v\n", err)
			return nil
		}
		seen = true
		if filepath.Base(args[0]) == gatewayMonitor {
			if slices.Contains(args[1:], "--net-gateway") {
				return nil
			}
			return errors.New("urunc started hvi without --net-gateway, so the guest has no network: " +
				"the shim's urunc does not read the gateway's annotations, which takes " +
				"urunc-dev/urunc@034929e. The instance is removed")
		}
		if time.Now().After(deadline) {
			_, _ = fmt.Fprintf(w, "warning: urunc did not start hvi within %s, so hull cannot tell whether "+
				"it joined the network gateway\n", gatewayJoinTimeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// noID stands for an ID that owns no file: (uid_t)-1, which the kernel
// gives no file.
const noID = math.MaxUint32

// gatewayHostIDs returns the host uid and gid hvi runs as for an instance
// whose image runs as uid and gid. Under a rootful daemon they are the
// same IDs. Under a rootless one, rootlesskit maps root to the user it runs
// as, which is hull's own, since hull finds its state directory under its
// own runtime directory, and every other ID to a subordinate ID, which owns
// no file a gateway creates; noID stands for that.
func gatewayHostIDs(uid, gid uint32, rootless bool) (hostUID, hostGID uint32) {
	if !rootless {
		return uid, gid
	}
	hostUID, hostGID = noID, noID
	if uid == 0 {
		hostUID = uint32(os.Geteuid())
	}
	if gid == 0 {
		hostGID = uint32(os.Getegid())
	}
	return hostUID, hostGID
}

// mayWrite reports whether a process with the host uid and gid may write to
// a file with st's owner and mode, by the kernel's rule: root may, the
// owner's bits apply to the owner alone, the group's to the rest of its
// group, and the others' bits to everyone else.
func mayWrite(st *syscall.Stat_t, uid, gid uint32) bool {
	switch {
	case uid == 0:
		return true
	case st.Uid == uid:
		return st.Mode&0o200 != 0
	case st.Gid == gid:
		return st.Mode&0o020 != 0
	default:
		return st.Mode&0o002 != 0
	}
}

// warnGatewayUser warns when an instance of an image with config is
// unlikely to reach the gateway at socket. hvi connects to the socket as the
// instance's user, which takes write permission on it, and boots the guest
// on a stub network with no way out when it has none. rootless says whether
// the daemon is rootless, which maps the instance's IDs to other host IDs
// (gatewayHostIDs). The check leaves out supplementary groups.
func warnGatewayUser(w io.Writer, config ocispec.ImageConfig, socket string, rootless bool) {
	uid, gid, err := containerd.ImageUser(config)
	// A user hull cannot resolve fails the run anyway, with the reason.
	if err != nil {
		return
	}
	var st syscall.Stat_t
	err = syscall.Stat(socket, &st)
	// checkGatewaySock found the socket. One gone since fails the create.
	if err != nil {
		return
	}
	hostUID, hostGID := gatewayHostIDs(uid, gid, rootless)
	if mayWrite(&st, hostUID, hostGID) {
		return
	}
	_, _ = fmt.Fprintf(w, "warning: the image runs as uid %d, and hvi joins the network gateway as that user. "+
		"It may not write to %s, so the guest is likely to boot with no network\n", uid, socket)
}

// hookPath is the PATH the network hook runs with: path, or a default when
// it is empty, with /usr/sbin and /sbin after it, as nerdctl gives its hook.
func hookPath(path string) string {
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return path + ":/usr/sbin:/sbin"
}

// cniConfig finds the plugins and the configuration of the network --net
// shared joins: $CNI_PATH and $NETCONFPATH, the variables nerdctl and the
// CNI tools read, then the installed brig-standalone-linux bundle's, then
// CNI's own /opt/cni/bin and /etc/cni/net.d. The bundle's plugins are in its
// libexec/cni. Its configuration is the copy rootlessBrigNetConfDir names,
// for a user who has one, else the bundle's etc/cni/net.d. A relative path
// is made absolute: the hook runs in the runtime's working directory, not
// hull's.
func cniConfig() (cni.Config, error) {
	pluginDirs := []string{defaultCNIPath}
	confDir := defaultNetConfDir
	for _, dir := range bundleDirs() {
		conf := filepath.Join(dir, "etc", "cni", "net.d")
		_, err := os.Stat(conf)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cni.Config{}, err
		}
		pluginDirs = []string{filepath.Join(dir, "libexec", "cni")}
		confDir = conf
		break
	}
	userConf := rootlessBrigNetConfDir()
	if userConf != "" {
		_, err := os.Stat(userConf)
		if err == nil {
			confDir = userConf
		} else if !errors.Is(err, os.ErrNotExist) {
			return cni.Config{}, err
		}
	}
	env := os.Getenv("CNI_PATH")
	if env != "" {
		pluginDirs = filepath.SplitList(env)
	}
	env = os.Getenv("NETCONFPATH")
	if env != "" {
		confDir = env
	}
	for i, dir := range pluginDirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return cni.Config{}, fmt.Errorf("CNI_PATH: %w", err)
		}
		pluginDirs[i] = abs
	}
	abs, err := filepath.Abs(confDir)
	if err != nil {
		return cni.Config{}, fmt.Errorf("NETCONFPATH: %w", err)
	}
	return cni.Config{PluginDirs: pluginDirs, ConfDir: abs, Network: cni.DefaultNetwork}, nil
}

// rootlessBrigNetConfDir returns where brig-rootless-setup.sh puts the
// network configuration of a user's rootless brig:
// $XDG_CONFIG_HOME/brig/cni/net.d, or ~/.config/brig/cni/net.d. It is a
// copy of the bundle's etc/cni/net.d, and the nerdctl that script sets up
// reads it. hull reads it too, so the two take the same lock, the one
// cni.Config takes in that directory. It returns "" for root, whose system
// install's nerdctl reads the bundle's own directory.
func rootlessBrigNetConfDir() string {
	if os.Geteuid() == 0 {
		return ""
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "brig", "cni", "net.d")
}

// guestResolvConf returns the /etc/resolv.conf an attached instance gets.
// Under a rootless daemon, rootlesskit's own nameservers, rootlessDNS, come
// first: they reach the host's resolver even when that listens on the
// host's loopback only.
func guestResolvConf(rootlessDNS []string) ([]byte, error) {
	host, err := cni.HostResolvConf()
	if err != nil {
		return nil, err
	}
	return cni.ResolvConf(host, rootlessDNS), nil
}

// instanceNetDir returns the directory that holds the network files of
// instance id in namespace.
func instanceNetDir(namespace, id string) (string, error) {
	logFile, err := instanceLogPath(namespace, id)
	if err != nil {
		return "", err
	}
	return netDirOf(logFile), nil
}

// netDirOf returns the network directory of the instance whose log is
// logFile: beside the log, named after it, so rm, which locks the log's
// directory, removes both under one lock.
func netDirOf(logFile string) string {
	return strings.TrimSuffix(logFile, ".log") + ".net"
}

// warnDetachFailure prints to w the error the network hook left in dir,
// instance id's network directory, when it could not detach the instance at
// postStop, and removes it, so it is reported once. The hook does not fail
// then, and urunc drops the output of a hook that succeeds, so the file is
// the only place the error is kept. next, when not empty, follows the error
// and says what happens to the detach now.
func warnDetachFailure(w io.Writer, id, dir, next string) {
	path := filepath.Join(dir, containerd.HookErrorFile)
	reason, err := os.ReadFile(path)
	if err != nil {
		return
	}
	msg := fmt.Sprintf("warning: the network hook could not detach instance %s: %s", id, strings.TrimSpace(string(reason)))
	if next != "" {
		msg += "; " + next
	}
	_, _ = fmt.Fprintln(w, msg)
	_ = os.Remove(path)
}

// withNetworkAddress fills in rec's address from the result the network
// hook keeps for the instance, when there is one. An instance with no
// network, or one detached, keeps none.
func withNetworkAddress(rec *store.InstanceState, namespace string) {
	dir, err := instanceNetDir(namespace, rec.ID)
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, networkResultFile))
	if err != nil {
		return
	}
	var res cni.Result
	err = json.Unmarshal(data, &res)
	if err != nil {
		return
	}
	rec.IP = res.IP
	rec.MAC = res.MAC
}

// leftAttachment is the detach an instance's postStop hook may not have
// run: the hook as the container's spec names it, and the state the
// runtime would have given it.
type leftAttachment struct {
	hook  specs.Hook
	state specs.State
}

// attachmentOf returns what detachLeft needs to detach ctr, whose network
// directory is dir, when its hook still records an attachment there, and
// nil when it records none or the spec names no network hook. Once ctr's
// task is gone, the address record left means the hook never ran at
// postStop, and detachPendingFile means it ran and its detach failed.
func attachmentOf(ctx context.Context, ctr client.Container, dir string) (*leftAttachment, error) {
	left := false
	for _, name := range []string{networkResultFile, detachPendingFile} {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			left = true
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if !left {
		return nil, nil
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the spec of container %s: %w", ctr.ID(), err)
	}
	if spec.Hooks == nil {
		return nil, nil
	}
	for _, h := range spec.Hooks.Poststop {
		if len(h.Args) >= 4 && h.Args[1] == "internal" && h.Args[2] == "oci-hook" && h.Args[len(h.Args)-1] == "postStop" {
			return &leftAttachment{
				hook:  h,
				state: specs.State{Version: spec.Version, ID: ctr.ID(), Status: specs.StateStopped},
			}, nil
		}
	}
	return nil, nil
}

// hookExecutable is the hull binary detachLeft runs the hook with. A
// variable, so tests can stand in for the hook.
var hookExecutable = os.Executable

// inUserNS reports whether hull runs in a user namespace, as the hook does
// under a rootless daemon. A variable, so tests can take either side.
var inUserNS = userns.RunningInUserNS

// detachLeft runs the postStop hook of a, which the runtime never ran or
// ran without detaching the instance, as detach: as postStop, except that a
// detach that fails keeps the hook's record and the pin and fails the hook,
// so hull rm can keep the instance and run it again. The runtime never runs
// the hook of a task that is gone without being deleted, as after a host
// reboot, or after its shim was killed because urunc could not delete it.
// A rootful daemon's host-local plugin keeps its leases on disk, so the
// address would stay leased for good, and a container ID is never used
// again for a later attach to free it.
//
// The hook runs from this hull binary, with the arguments the spec gives
// it: the binary that ran the instance may be the one that is gone. Under a
// rootless daemon it runs, as the runtime would have run it, in
// rootlesskit's user and mount namespaces, where the plugins keep their
// state, and in rootlesskit's network namespace too unless rootlesskit
// detached one, which the hook enters itself. hull enters them with
// nsenter, as containerd-rootless-setuptool.sh does: a process that runs
// more than one thread, as every Go program does, cannot join a user
// namespace.
func detachLeft(ctx context.Context, a *leftAttachment) error {
	exe, err := hookExecutable()
	if err != nil {
		return fmt.Errorf("find hull's executable: %w", err)
	}
	args := slices.Clone(a.hook.Args[1:])
	args[len(args)-1] = "detach"
	name, args, err := detachCommand(exe, args)
	if err != nil {
		return err
	}
	state, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = a.hook.Env
	cmd.Stdin = bytes.NewReader(state)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil && output != "" {
		return fmt.Errorf("run the network hook: %w: %s", err, output)
	}
	if err != nil {
		return fmt.Errorf("run the network hook: %w", err)
	}
	return nil
}

// detachCommand returns the command line that runs exe with the hook
// arguments args where the runtime would have run the hook; see detachLeft.
func detachCommand(exe string, args []string) (string, []string, error) {
	i := slices.Index(args, "--rootlesskit-state-dir")
	if i < 0 || i+1 >= len(args) || inUserNS() {
		return exe, args, nil
	}
	stateDir := args[i+1]
	pid, err := os.ReadFile(filepath.Join(stateDir, "child_pid"))
	if err != nil {
		return "", nil, fmt.Errorf("find rootlesskit's namespaces: %w", err)
	}
	enter := []string{"-U", "--preserve-credentials", "-m"}
	_, err = os.Stat(filepath.Join(stateDir, "netns"))
	if errors.Is(err, os.ErrNotExist) {
		enter = append(enter, "-n")
	}
	enter = append(enter, "-t", strings.TrimSpace(string(pid)), "--", exe)
	return "nsenter", append(enter, args...), nil
}
