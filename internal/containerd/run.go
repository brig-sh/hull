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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Labels hull sets on the containers it creates, for inspect to report.
const (
	LabelBackend     = "sh.brig.hull/backend"
	LabelImageDigest = "sh.brig.hull/image-digest"
	LabelLogFile     = "sh.brig.hull/log-file"
)

// ImageForRun returns the image ref names, for platform, pulling it as
// policy says: "missing" pulls only an image the namespace does not have,
// "always" pulls it whatever is there, and "never" fails when it is not
// there. Progress, when not nil, receives the report of a pull.
func ImageForRun(ctx context.Context, c *client.Client, ref, policy string, platform ocispec.Platform, progress io.Writer) (client.Image, error) {
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
	_, err = Pull(ctx, c, ref, platform, progress)
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
	ID string
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
}

// cpuPeriod is the CFS period a CPU count is expressed in. urunc sizes the
// guest's vCPUs from the quota over the period, rounded up.
const cpuPeriod = 100000

// CreateInstance creates the container cfg.ID from image, with urunc as its
// runtime and a new snapshot of the image as its root filesystem, and
// starts its task with the console written to cfg.LogFile. An error that
// errdefs.IsAlreadyExists accepts means a container already has that ID.
//
// Whatever fails after the container exists removes it again, with its
// snapshot and its log, so a failed run does not hold the name.
func CreateInstance(ctx context.Context, c *client.Client, image RunImage, cfg RunConfig) (err error) {
	ctr, err := newContainer(ctx, c, image, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// The run's own context may be what ended it.
			cleanup := context.WithoutCancel(ctx)
			derr := ctr.Delete(cleanup, client.WithSnapshotCleanup)
			if derr != nil {
				err = errors.Join(err, fmt.Errorf("remove the container %s: %w", cfg.ID, derr))
			}
			derr = os.Remove(cfg.LogFile)
			if derr != nil && !errors.Is(derr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove the log of %s: %w", cfg.ID, derr))
			}
		}
	}()

	// The shim appends to the log, and a name can be used again once its
	// instance is removed. Only now, with the name taken by this container,
	// is a log left at the path not another live instance's.
	err = os.Remove(cfg.LogFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the old log of %s: %w", cfg.ID, err)
	}
	task, err := ctr.NewTask(ctx, cio.LogFile(cfg.LogFile))
	if err != nil {
		return fmt.Errorf("create the task of %s: %w", cfg.ID, err)
	}
	defer func() {
		if err != nil {
			_, derr := task.Delete(context.WithoutCancel(ctx), client.WithProcessKill)
			if derr != nil {
				err = errors.Join(err, fmt.Errorf("remove the task of %s: %w", cfg.ID, derr))
			}
		}
	}()
	err = task.Start(ctx)
	if err != nil {
		return fmt.Errorf("start %s: %w", cfg.ID, err)
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
	err = unpack(ctx, img)
	if err != nil {
		return nil, err
	}

	specOpts := []oci.SpecOpts{
		oci.WithHostname(cfg.ID),
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

	ctr, err := c.NewContainer(ctx, cfg.ID,
		client.WithImage(img),
		client.WithNewSnapshot(cfg.ID, img),
		client.WithRuntime(RuntimeName, nil),
		client.WithNewSpec(specOpts...),
		// Not client.WithImageConfigLabels: urunc's shim copies a
		// container's com.urunc.unikernel.* labels into the spec, and the
		// image's labels are the image's to set.
		client.WithContainerLabels(map[string]string{
			LabelBackend:     cfg.Backend,
			LabelImageDigest: img.Target().Digest.String(),
			LabelLogFile:     cfg.LogFile,
		}),
	)
	if errdefs.IsAlreadyExists(err) {
		// The snapshot takes the container's ID as its key, and is prepared
		// before the container is created, so a snapshot left behind under
		// that key fails the run as an instance of that name would. hull rm
		// finds no instance to remove then, so name the snapshot.
		_, lerr := c.LoadContainer(ctx, cfg.ID)
		if errdefs.IsNotFound(lerr) {
			return nil, fmt.Errorf("create container %s: no instance has that name, but a snapshot "+
				"with no container does; remove it with ctr snapshots rm %s: %v", cfg.ID, cfg.ID, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create container %s: %w", cfg.ID, err)
	}
	return ctr, nil
}

// unpack unpacks img into the snapshotter a new snapshot will use, when a
// pull has not already. A pull through hull unpacks, and one through ctr
// may not have.
func unpack(ctx context.Context, img client.Image) error {
	unpacked, err := img.IsUnpacked(ctx, "")
	if err != nil {
		return fmt.Errorf("check whether %s is unpacked: %w", img.Name(), err)
	}
	if unpacked {
		return nil
	}
	err = img.Unpack(ctx, "")
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
		return process{}, err
	}
	p.User.UID, p.User.GID = uid, gid
	return p, nil
}

// numericUser parses an image's USER as uid[:gid]. A name needs the image's
// /etc/passwd or /etc/group, which are in its snapshot, and LoadRunImage
// reads no snapshot, so a name other than root is refused rather than run
// as root. A uid with no gid runs with gid 0, as docker runs a uid that
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
		return 0, 0, fmt.Errorf("the image runs as user %q: %w", user, err)
	}
	if group == "" {
		return uid, 0, nil
	}
	gid, err = userID(group, "root")
	if err != nil {
		return 0, 0, fmt.Errorf("the image runs as user %q: %w", user, err)
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
