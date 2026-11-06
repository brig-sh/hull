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

package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/brig-sh/hull/internal/cni"
)

// Labels hull sets on the containers it creates, for inspect to report.
const (
	LabelBackend     = "sh.brig.hull/backend"
	LabelImageDigest = "sh.brig.hull/image-digest"
	LabelLogFile     = "sh.brig.hull/log-file"
)

// LabelName is the label that holds the name of the instance a container
// hull created is. The container's ID is one of its own; see newContainerID.
const LabelName = "sh.brig.hull/name"

// LabelNetwork holds the network a container hull created was run with:
// none, or the --net mode of its RunConfig.Network. The attachment's own
// record goes when the task does, and a label is still there for a stopped
// instance, as nerdctl keeps a container's networks in its nerdctl/networks
// label.
const LabelNetwork = "sh.brig.hull/network"

// ImageForRun returns the image ref names, for platform, pulling it as
// policy says: "missing" pulls only an image the namespace does not have,
// "always" pulls it whatever is there, and "never" fails when it is not
// there. A pull unpacks into snapshotter, the one the run's instance will
// use, so that the namespace's default snapshotter need not work for it.
// Progress, when not nil, receives the report of a pull.
func ImageForRun(ctx context.Context, c *client.Client, ref, policy string, platform ocispec.Platform, snapshotter string, progress io.Writer) (client.Image, error) {
	name, err := NormalizeRef(ref)
	if err != nil {
		return nil, err
	}
	matcher := platforms.OnlyStrict(platform)
	if policy != "always" {
		img, err := localImage(ctx, c, name, matcher)
		if err == nil {
			return img, nil
		}
		if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("look up image %s: %w", ref, err)
		}
		if policy == "never" {
			return nil, fmt.Errorf("image %s has not been pulled for %s, and --pull never does not fetch it",
				ref, platforms.Format(platform))
		}
	}
	_, err = Pull(ctx, c, ref, platform, snapshotter, progress)
	if err != nil {
		return nil, err
	}
	meta, err := c.ImageService().Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("look up image %s after pulling it: %w", ref, err)
	}
	return client.NewImageWithPlatform(c, meta, matcher), nil
}

// localImage returns the image the namespace keeps under name, for the
// platform matcher accepts, when the content store holds that platform's
// manifest and config. The image's record is not enough: a pull for another
// platform records the image too, with an index that lists this platform's
// manifest but none of its content.
func localImage(ctx context.Context, c *client.Client, name string, matcher platforms.MatchComparer) (client.Image, error) {
	meta, err := findImage(ctx, c, name)
	if err != nil {
		return nil, err
	}
	img := client.NewImageWithPlatform(c, meta, matcher)
	_, err = img.Spec(ctx)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// findImage returns the image the namespace keeps under name. A name pinned
// to a digest, which NormalizeRef makes of repo:tag@digest too, also finds
// an image of the same repository pulled by its tag whose target is that
// digest: containerd keeps an image under the name it was pulled by, so the
// tag's record holds the content the digest names.
func findImage(ctx context.Context, c *client.Client, name string) (images.Image, error) {
	meta, err := c.ImageService().Get(ctx, name)
	if !errdefs.IsNotFound(err) {
		return meta, err
	}
	ref, perr := reference.ParseNormalizedNamed(name)
	if perr != nil {
		return images.Image{}, err
	}
	digested, ok := ref.(reference.Digested)
	if !ok {
		return images.Image{}, err
	}
	list, lerr := c.ImageService().List(ctx, "target.digest=="+digested.Digest().String())
	if lerr != nil {
		return images.Image{}, lerr
	}
	for _, img := range list {
		other, perr := reference.ParseNormalizedNamed(img.Name)
		if perr == nil && other.Name() == ref.Name() {
			return img, nil
		}
	}
	return images.Image{}, err
}

// RunImage is what a run takes from an image.
type RunImage struct {
	Image  client.Image
	Config ocispec.ImageConfig
	// Urunc holds the urunc annotations the image declares, without the
	// keys that name host paths.
	Urunc map[string]string
}

// LoadRunImage reads the config and the urunc annotations of img, for the
// platform the image was loaded with, from the content store.
//
// It reads nothing from a snapshot. oci.WithImageConfig would: it mounts
// the image's snapshot in this process to look up the user and groups in
// /etc/passwd and /etc/group. That fails for a client outside a rootless
// daemon's user and mount namespaces, and for an ordinary user against a
// daemon run as root, whose snapshots only root can open.
func LoadRunImage(ctx context.Context, img client.Image) (RunImage, error) {
	spec, err := img.Spec(ctx)
	if err != nil {
		return RunImage{}, fmt.Errorf("read the config of %s: %w", img.Name(), err)
	}
	manifest, err := images.Manifest(ctx, img.ContentStore(), img.Target(), img.Platform())
	if err != nil {
		return RunImage{}, fmt.Errorf("read the manifest of %s: %w", img.Name(), err)
	}
	return RunImage{
		Image:  img,
		Config: spec.Config,
		Urunc:  imageUrunc(manifest.Annotations, spec.Config.Labels),
	}, nil
}

// RunConfig describes an instance to create.
type RunConfig struct {
	// Name is the instance's name, which hull's commands take.
	Name string
	// Args replaces the image's Cmd when it is not empty. The image's
	// Entrypoint is kept, as docker run keeps it.
	Args []string
	// Env is added to the image's environment, and wins on a variable
	// both set.
	Env      []string
	MemoryMB int
	CPUs     int
	// Annotations are set on the spec over the image's urunc annotations.
	Annotations map[string]string
	Backend     string
	// LogFile is where the shim writes the guest's console. Its directory
	// must exist.
	LogFile string
	// Snapshotter holds the instance's root filesystem. Empty means the
	// namespace's default, the one a pull unpacks into.
	Snapshotter string
	// Network, when not nil, attaches the instance to a network. Without
	// it, the instance has none.
	Network *Network
	// Mounts are added to the spec's, for urunc to take into the guest.
	Mounts []specs.Mount
}

// afterCreate runs between CreateInstance's create of the container and its
// second check of the name. A variable, so a test can create a container of
// the name there, as a run under another lock could.
var afterCreate = func() {}

// containerIDLen is the length of the IDs newContainerID makes.
const containerIDLen = 16

// hostnameLen is how much of the container ID is the spec's host name: 12
// characters, as in nerdctl and docker.
const hostnameLen = 12

// maxNamespaceLen is the longest namespace an instance can start in. urunc
// listens on two sockets in the runc shim's default root,
// /run/containerd/runc/<namespace>/<id>/urunc.sock and reexec.sock, the
// longer, and Go's listener takes a path of at most 107 bytes: a socket
// address holds 108, the last a NUL. A namespace one character longer
// fails the start with no word of why.
const maxNamespaceLen = 107 - len("/run/containerd/runc/"+"/"+"/reexec.sock") - containerIDLen

// ValidateRunNamespace checks that an instance can start in namespace; see
// maxNamespaceLen.
func ValidateRunNamespace(namespace string) error {
	if len(namespace) > maxNamespaceLen {
		return fmt.Errorf("namespace %s is %d characters, and an instance can start only in one of at most %d: "+
			"urunc's sockets under /run/containerd/runc/<namespace>/<id> would not fit in a socket address",
			namespace, len(namespace), maxNamespaceLen)
	}
	return nil
}

// Network attaches an instance to a network through OCI hooks: the runtime
// runs Hook at createRuntime, once the instance's network namespace exists
// and before urunc's start reads the interface from it, and at postStop,
// when the task is deleted.
type Network struct {
	// Mode is the --net the run attaches the instance with, such as
	// shared, which the container's LabelNetwork keeps.
	Mode string
	// Hook is the command the runtime runs, with "createRuntime" or
	// "postStop" appended to its arguments.
	Hook specs.Hook
	// Dir holds the instance's network files: ResolvConf, which
	// CreateInstance writes there as ResolvConfFile, and whatever the hook
	// keeps. Its parent must exist. CreateInstance replaces a Dir that is
	// left from an old instance, and removes it when the run fails.
	//
	// A hook that fails writes why to HookErrorFile there. At createRuntime,
	// urunc puts a failed hook's output in its own error, but the shim
	// reports only that urunc's child process went away, so CreateInstance
	// adds the file to the error it returns. At postStop the hook succeeds
	// anyway, and the file is all that is left of the error.
	Dir        string
	ResolvConf []byte
}

// ResolvConfFile is the file Network.Dir holds for the guest's
// /etc/resolv.conf.
const ResolvConfFile = "resolv.conf"

// HookErrorFile is the file in Network.Dir the hook writes its error to,
// when the attach at createRuntime or the detach at postStop fails.
const HookErrorFile = "hook-error"

// NetnsPinFile is the file in Network.Dir where the hook pins the
// instance's network namespace while the instance is attached.
const NetnsPinFile = "netns"

// cpuPeriod is the CFS period a CPU count is expressed in. urunc sizes the
// guest's vCPUs from the quota over the period, rounded up.
const cpuPeriod = 100000

// CreateInstance creates the instance cfg.Name from image: a container of
// its own ID, with urunc as its runtime and a new snapshot of the image as
// its root filesystem, and starts its task with the console written to
// cfg.LogFile. An error that errdefs.IsAlreadyExists accepts means an
// instance already has that name.
//
// containerd keeps container IDs unique, but not labels, so the name is
// checked twice. Before the container is created, so a name in use costs
// nothing; hull run holds the namespace's log directory lock meanwhile, so
// no other run of the name under the same lock gets past the check. And
// after, since two runs under different locks, as root's and a user's are,
// can both pass the first check: a run that then finds another container
// holding its name removes its own and fails. The run whose second check
// comes after the other's create fails, and when both creates come before
// either second check, both fail; they never both succeed. A run killed
// between its create and its removal leaves both containers, and every
// command that acts on the name then fails until one is removed with ctr;
// see LoadNamed. Instances lists both under the name.
//
// Whatever fails after the container exists, and after the second check,
// removes it again, with its snapshot and its log, so a failed run does not
// hold the name.
func CreateInstance(ctx context.Context, c *client.Client, image RunImage, cfg RunConfig) (err error) {
	err = ValidateName(cfg.Name)
	if err != nil {
		return err
	}
	err = ValidateRunNamespace(c.DefaultNamespace())
	if err != nil {
		return err
	}
	_, err = LoadNamed(ctx, c, cfg.Name)
	if err == nil {
		return fmt.Errorf("instance %s: %w", cfg.Name, errdefs.ErrAlreadyExists)
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	ctr, err := newContainer(ctx, c, image, cfg)
	if err != nil {
		return err
	}
	afterCreate()
	holders, err := nameHolders(ctx, c, cfg.Name)
	if err == nil && len(holders) > 1 {
		err = fmt.Errorf("instance %s: %w", cfg.Name, errdefs.ErrAlreadyExists)
	}
	if err != nil {
		// Not the log: nothing of this run is at its path yet, and an old
		// log there goes only once the name is this container's alone.
		derr := ctr.Delete(context.WithoutCancel(ctx), client.WithSnapshotCleanup)
		if derr != nil {
			err = errors.Join(err, fmt.Errorf("remove the container %s of %s: %w", ctr.ID(), cfg.Name, derr))
		}
		return err
	}
	defer func() {
		if err != nil {
			// The run's own context may be what ended it.
			cleanup := context.WithoutCancel(ctx)
			derr := ctr.Delete(cleanup, client.WithSnapshotCleanup)
			if derr != nil {
				err = errors.Join(err, fmt.Errorf("remove the container %s of %s: %w", ctr.ID(), cfg.Name, derr))
			}
			derr = os.Remove(cfg.LogFile)
			if derr != nil && !errors.Is(derr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove the log of %s: %w", cfg.Name, derr))
			}
			if cfg.Network != nil {
				derr = RemoveNetworkDir(cfg.Network.Dir)
				if derr != nil {
					err = errors.Join(err, fmt.Errorf("remove the network files of %s: %w", cfg.Name, derr))
				}
			}
		}
	}()

	// The shim appends to the log, and a name can be used again once its
	// instance is removed. Only now, with the name held by this container
	// alone, is a log left at the path not another live instance's.
	err = os.Remove(cfg.LogFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the old log of %s: %w", cfg.Name, err)
	}
	// The network files are another live instance's until the name is
	// taken, as the log is, and the resolver configuration must be in place
	// before the task is created: on a block device root filesystem, urunc
	// copies it into the guest's disk then.
	if cfg.Network != nil {
		err = writeNetworkFiles(cfg.Network)
		if err != nil {
			return fmt.Errorf("write the network files of %s: %w", cfg.Name, err)
		}
	}
	task, err := ctr.NewTask(ctx, cio.LogFile(cfg.LogFile))
	if err != nil {
		err = fmt.Errorf("create the task of %s: %w", cfg.Name, err)
		if cfg.Network != nil {
			err = withHookError(err, cfg.Network.Dir)
		}
		return err
	}
	defer func() {
		if err != nil {
			_, derr := task.Delete(context.WithoutCancel(ctx), client.WithProcessKill)
			if derr != nil {
				err = errors.Join(err, fmt.Errorf("remove the task of %s: %w", cfg.Name, derr))
			}
		}
	}()
	err = task.Start(ctx)
	if err != nil {
		return fmt.Errorf("start %s: %w", cfg.Name, err)
	}
	return nil
}

// newContainer creates the container CreateInstance starts.
func newContainer(ctx context.Context, c *client.Client, image RunImage, cfg RunConfig) (client.Container, error) {
	annotations := UruncAnnotations(cfg.Annotations, image.Urunc)
	err := checkCRIKeys(annotations)
	if err != nil {
		return nil, err
	}
	err = checkBootsVM(annotations)
	if err != nil {
		return nil, err
	}
	process, err := processFromImage(image.Config, cfg.Args)
	if err != nil {
		return nil, err
	}
	img := image.Image
	err = unpack(ctx, img, cfg.Snapshotter)
	if err != nil {
		return nil, err
	}

	id, err := newContainerID()
	if err != nil {
		return nil, err
	}
	specOpts := []oci.SpecOpts{
		// The container ID's first characters, as nerdctl and docker name
		// a container's host, and not the instance's name, which can be
		// longer than the 64 bytes a Linux host name can be.
		oci.WithHostname(id[:hostnameLen]),
		oci.WithProcessArgs(process.Args...),
		// The default PATH first, as docker gives an image that sets none,
		// so the image's own replaces it.
		oci.WithDefaultPathEnv,
		oci.WithEnv(image.Config.Env),
		// A second option rather than one appended list, so a variable the
		// run sets replaces the image's instead of following it.
		oci.WithEnv(cfg.Env),
		oci.WithProcessCwd(process.Cwd),
		oci.WithUIDGID(process.User.UID, process.User.GID),
		oci.WithAnnotations(annotations),
	}
	if cfg.MemoryMB > 0 {
		specOpts = append(specOpts, oci.WithMemoryLimit(uint64(cfg.MemoryMB)*1024*1024))
	}
	if cfg.CPUs > 0 {
		specOpts = append(specOpts, oci.WithCPUCFS(int64(cfg.CPUs)*cpuPeriod, cpuPeriod))
	}
	if len(cfg.Mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(cfg.Mounts))
	}
	network := "none"
	if cfg.Network != nil {
		specOpts = append(specOpts, withNetwork(cfg.Network))
		network = cfg.Network.Mode
	}

	ctr, err := c.NewContainer(ctx, id,
		client.WithImage(img),
		// Before WithNewSnapshot, which prepares the snapshot in the
		// container's snapshotter.
		client.WithSnapshotter(cfg.Snapshotter),
		client.WithNewSnapshot(id, img),
		client.WithRuntime(RuntimeName, nil),
		client.WithNewSpec(specOpts...),
		// Not client.WithImageConfigLabels: urunc's shim copies a
		// container's com.urunc.unikernel.* labels into the spec, and the
		// image's labels are the image's to set.
		client.WithContainerLabels(map[string]string{
			LabelName:        cfg.Name,
			LabelBackend:     cfg.Backend,
			LabelImageDigest: img.Target().Digest.String(),
			LabelLogFile:     cfg.LogFile,
			LabelNetwork:     network,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create container for %s: %w", cfg.Name, err)
	}
	return ctr, nil
}

// newContainerID returns a new container ID: containerIDLen random hex
// digits. A name that is run again gets a new ID, so nothing kept under an
// earlier instance's ID, such as its snapshot, the runtime's state or a CNI
// plugin's, is mistaken for the new instance's.
//
// nerdctl's IDs have 64 digits, but the ID is part of urunc's socket paths
// (see maxNamespaceLen): with 64 digits, an instance could not start in a
// namespace longer than 9 characters. 16 leave it 57.
func newContainerID() (string, error) {
	b := make([]byte, containerIDLen/2)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("make a container ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// withNetwork adds n's hooks to the spec, and binds its resolver
// configuration over the guest's /etc/resolv.conf. urunc binds it into a
// shared root filesystem, and copies it into a block device one.
//
// The guest keeps the image's own /etc/hosts, as on macOS. hull cannot
// read the image's root filesystem to add to that file, and a file of its
// own would drop the image's entries for no gain: it could not name the
// instance's own address either, since on a block device root filesystem
// urunc copies the file before the network has given the instance one.
func withNetwork(n *Network) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Hooks == nil {
			s.Hooks = &specs.Hooks{}
		}
		hook := func(event string) specs.Hook {
			h := n.Hook
			h.Args = append(slices.Clone(n.Hook.Args), event)
			return h
		}
		s.Hooks.CreateRuntime = append(s.Hooks.CreateRuntime, hook("createRuntime"))
		s.Hooks.Poststop = append(s.Hooks.Poststop, hook("postStop"))
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      filepath.Join(n.Dir, ResolvConfFile),
			Options:     []string{"bind", "rprivate"},
		})
		return nil
	}
}

// withHookError adds to err, a failed task create's, the error a failed
// createRuntime hook wrote to HookErrorFile in dir, if it wrote one.
func withHookError(err error, dir string) error {
	reason, rerr := os.ReadFile(filepath.Join(dir, HookErrorFile))
	if rerr != nil {
		return err
	}
	return fmt.Errorf("%w: the network hook failed: %s", err, strings.TrimSpace(string(reason)))
}

// RemoveNetworkDir removes an instance's network directory, dir. When the
// instance's postStop hook never ran, as when the hull binary it names is
// gone, the namespace is still pinned in dir. A pin is a mount, which
// unlink refuses with EBUSY in the mount namespace that holds it, as a
// rootful daemon's and hull's own is, so the pin is released first.
func RemoveNetworkDir(dir string) error {
	err := cni.UnpinNetns(filepath.Join(dir, NetnsPinFile))
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// writeNetworkFiles replaces n.Dir with one holding n's resolver
// configuration. The file is readable by anyone, as the guest's
// /etc/resolv.conf is, whatever the umask: urunc copies its mode into the
// guest, where a process that is not root has to read it.
func writeNetworkFiles(n *Network) error {
	err := RemoveNetworkDir(n.Dir)
	if err != nil {
		return err
	}
	err = os.Mkdir(n.Dir, 0o755)
	if err != nil {
		return err
	}
	path := filepath.Join(n.Dir, ResolvConfFile)
	err = os.WriteFile(path, n.ResolvConf, 0o644)
	if err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// ResolveSnapshotter returns the snapshotter a run given snapshotter, "" for
// the namespace's default, creates its container in: the namespace's
// containerd.io/defaults/snapshotter label, else containerd's default, as
// the client resolves it when it unpacks and creates.
func ResolveSnapshotter(ctx context.Context, c *client.Client, snapshotter string) (string, error) {
	if snapshotter != "" {
		return snapshotter, nil
	}
	label, err := c.GetLabel(ctx, defaults.DefaultSnapshotterNSLabel)
	if err != nil {
		return "", fmt.Errorf("read the namespace's default snapshotter: %w", err)
	}
	if label != "" {
		return label, nil
	}
	return defaults.DefaultSnapshotter, nil
}

// unpack unpacks img into snapshotter, "" for the namespace's default, when
// a pull has not already. A run's own pull unpacks into the run's
// snapshotter, but hull pull unpacks into the namespace's default, and a
// pull through ctr may not have unpacked at all.
func unpack(ctx context.Context, img client.Image, snapshotter string) error {
	unpacked, err := img.IsUnpacked(ctx, snapshotter)
	if err != nil {
		return fmt.Errorf("check whether %s is unpacked: %w", img.Name(), err)
	}
	if unpacked {
		return nil
	}
	err = img.Unpack(ctx, snapshotter)
	if err != nil {
		return fmt.Errorf("unpack %s: %w", img.Name(), err)
	}
	return nil
}

// process is the guest's process as the image and the run define it.
type process struct {
	Args []string
	Cwd  string
	User struct{ UID, GID uint32 }
}

// processFromImage applies a run's command to an image's config, with
// docker's rules: args replace the image's Cmd and keep its Entrypoint, an
// image with neither a command nor args is refused, and an image with no
// working directory runs in /.
//
// macOS hull differs on two of these (createOCISpec in pkg/ociclient, and
// runInstance in cmd/hull): its args replace the Entrypoint too, and an
// image with no command runs /bin/sh. Linux follows docker, as containerd's
// own clients do.
func processFromImage(config ocispec.ImageConfig, args []string) (process, error) {
	var p process
	cmd := config.Cmd
	if len(args) > 0 {
		cmd = args
	}
	p.Args = append(append([]string{}, config.Entrypoint...), cmd...)
	if len(p.Args) == 0 {
		return process{}, errors.New("the image has no command to run, and none was given")
	}
	p.Cwd = config.WorkingDir
	if p.Cwd == "" {
		p.Cwd = "/"
	}
	uid, gid, err := numericUser(config.User)
	if err != nil {
		return process{}, fmt.Errorf("the image runs as user %q: %w", config.User, err)
	}
	p.User.UID, p.User.GID = uid, gid
	return p, nil
}

// numericUser parses a user, an image's USER or the one hull exec is asked
// to run as, as uid[:gid]. A name needs the image's /etc/passwd or
// /etc/group, which are in its snapshot, and hull reads no snapshot (see
// LoadRunImage), so a name other than root is refused rather than run as
// root. A uid with no gid runs with gid 0, as docker runs a uid that
// /etc/passwd does not list; docker would take a listed user's primary
// group from there. macOS hull reads /etc/passwd from the unpacked root
// filesystem, so it takes a listed uid's group from there too, and gives
// an unlisted uid its own value as its gid (resolveImageUser in
// pkg/ociclient).
func numericUser(user string) (uid, gid uint32, err error) {
	if user == "" {
		return 0, 0, nil
	}
	name, group, _ := strings.Cut(user, ":")
	uid, err = userID(name, "root")
	if err != nil {
		return 0, 0, err
	}
	if group == "" {
		return uid, 0, nil
	}
	gid, err = userID(group, "root")
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func userID(s, rootName string) (uint32, error) {
	if s == rootName {
		return 0, nil
	}
	id, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("hull on Linux takes only a numeric uid and gid, or root, "+
			"since resolving %q would mean reading the image's files", s)
	}
	return uint32(id), nil
}
