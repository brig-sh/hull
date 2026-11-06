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
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/brig-sh/hull/pkg/ociclient"
)

// RuntimeName is the containerd shim urunc ships, which boots a container as
// a VM.
const RuntimeName = "io.containerd.urunc.v2"

const uruncPrefix = "com.urunc.unikernel."

// uruncKeys is every annotation the urunc of the brig-standalone-linux bundle
// reads, at urunc-dev/urunc@0b1ef07 (pkg/unikontainers/annotations.go).
//
// urunc's containerd shim copies each com.urunc.unikernel.* key of the
// image's manifest annotations and of the container's labels into the
// runtime spec, unless the spec already has the key
// (pkg/containerd-shim/containerd/annotations.go, patchConfigJSON). Without
// a key in the spec, an image could therefore set bootKernel or bootInitrd:
// host paths urunc hands to the monitor, so any host file the shim can read
// would become the guest's kernel or initrd. UruncAnnotations writes every
// key, an empty value standing for an unset one, as urunc reads it, so
// nothing is left for the shim to fill in.
var uruncKeys = []string{
	ociclient.UnikernelTypeAnnotation,
	"com.urunc.unikernel.unikernelVersion",
	ociclient.UnikernelBinaryAnnotation,
	ociclient.HypervisorAnnotation,
	ociclient.InitrdAnnotation,
	"com.urunc.unikernel.block",
	"com.urunc.unikernel.blkMntPoint",
	"com.urunc.unikernel.mountRootfs",
	ociclient.BootKernelAnnotation,
	ociclient.BootInitrdAnnotation,
	"com.urunc.unikernel.solo5NetDev",
	"com.urunc.unikernel.solo5BlkDev",
	"com.urunc.unikernel.vAccel",
	"com.urunc.unikernel.RPCAddress",
}

// hostKeys name things on the host rather than in the image: the boot
// kernel and initrd are host paths, and vAccel and RPCAddress configure a
// host service. Only the invocation sets them, never the image. urunc
// itself keeps them out of an image's urunc.json for the same reason.
var hostKeys = map[string]bool{
	ociclient.BootKernelAnnotation:   true,
	ociclient.BootInitrdAnnotation:   true,
	"com.urunc.unikernel.vAccel":     true,
	"com.urunc.unikernel.RPCAddress": true,
}

// imageUrunc returns the urunc annotations an image declares, in its
// manifest annotations and its config labels. The shim reads the manifest
// annotations itself when the image's target is a manifest rather than an
// index, and the config labels when the client copies them onto the
// container, as nerdctl and ctr do. hull reads the platform's manifest
// either way, so an image that is an index declares the same keys as one
// that is a manifest. The manifest wins on a key both set, as in the shim.
// Keys in hostKeys are dropped.
func imageUrunc(manifest, labels map[string]string) map[string]string {
	out := make(map[string]string)
	for _, source := range []map[string]string{labels, manifest} {
		for k, v := range source {
			if strings.HasPrefix(k, uruncPrefix) && !hostKeys[k] && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

// UruncAnnotations returns the annotations for a run's spec: every key in
// uruncKeys, from explicit, then from image, and empty if neither sets it,
// and any other explicit annotation as given.
func UruncAnnotations(explicit, image map[string]string) map[string]string {
	out := make(map[string]string, len(uruncKeys)+len(explicit))
	for _, k := range uruncKeys {
		out[k] = image[k]
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}

// Monitors are the monitors urunc boots a guest on, at the commit uruncKeys
// names (supportedGuestMonitorPairs in pkg/unikontainers/annotations.go).
// urunc refuses any other only when the task is created.
var Monitors = []string{"cloud-hypervisor", "firecracker", "qemu", "hvt", "spt", "hyperlight-unikraft"}

// criContainerName is the annotation the CRI plugin names a pod's container
// with. urunc reads it before any annotation of its own: it hands a
// container named queue-proxy to runc, as an ordinary container on the
// host's kernel, and gives one named user-container a static network
// (unikontainers.New and getNetworkType in pkg/unikontainers). A hull
// instance is no pod's container, so a run has no reason to set it.
const criContainerName = "io.kubernetes.cri.container-name"

// checkCRIKeys refuses spec annotations urunc reads as a pod's.
func checkCRIKeys(annotations map[string]string) error {
	_, ok := annotations[criContainerName]
	if ok {
		return fmt.Errorf("annotation %s is refused: urunc takes it from Kubernetes, and runs a "+
			"container named queue-proxy with runc, on the host's kernel", criContainerName)
	}
	return nil
}

// sharedFSMonitors are the monitors urunc shares a host directory with a
// guest on, over virtio-fs or 9p (SupportsSharedfs in
// pkg/unikontainers/hypervisors at urunc-dev/urunc@3c00b69).
var sharedFSMonitors = map[string]bool{"cloud-hypervisor": true, "hvi": true, "qemu": true}

// CheckShares refuses bind mounts of directories, a run's shares, that urunc
// would leave out of the guest with no error, given the run's spec
// annotations and the snapshotter its root comes from. urunc takes such a
// mount into the guest only on a root it shares from the host, over
// virtio-fs or 9p, and on the overlay-block root (newRootfsBuilder in
// pkg/unikontainers/unikontainers.go, ChooseRootfs in rootfs.go). Into a
// root it boots from as an initrd or a disk it copies only files, and a
// guest with no root from the host gets none of them.
//
// A container boot, of an image with no kernel of its own, has its root
// shared from the host whenever the monitor can share one, on any
// snapshotter. Only on a monitor that cannot, such as firecracker, does
// urunc boot it from the snapshot as a disk (tryContainerRootfs). urunc
// fails such a boot itself when it is given an initrd, a block image or
// mountRootfs false (validateContainerBoot in annotations.go). The
// overlay-block root is a container boot's too, on hvi, cloud-hypervisor or
// qemu (validateRootfsMode), monitors that can share, so it passes the same
// way and needs no check of its own.
//
// An image that boots its own kernel has a root from the host only with
// mountRootfs true and neither an initrd nor a disk named for /. urunc then
// boots it from the snapshot as a disk whenever the snapshot is a block
// device, as devmapper's is, and shares it only otherwise. hull can tell
// that the snapshot is no block device ahead of the run only for overlayfs.
//
// Both take the virtiofsd urunc is configured with to be present, as the
// bundle ships it: without it urunc falls back to the snapshot as a disk on
// cloud-hypervisor and hvi.
func CheckShares(spec map[string]string, snapshotter string) error {
	monitor := spec[ociclient.HypervisorAnnotation]
	// checkBootsVM refuses a run that names no monitor, and says why.
	if monitor == "" {
		return nil
	}
	if !sharedFSMonitors[monitor] {
		return fmt.Errorf("--shared-dir needs a monitor that shares a directory with the guest, cloud-hypervisor, "+
			"hvi or qemu, and the run's is %s: urunc boots the guest on it from a disk or an initrd, and "+
			"copies only files into it", monitor)
	}
	if spec[ociclient.BootKernelAnnotation] != "" || spec[ociclient.BootInitrdAnnotation] != "" {
		return nil
	}
	if spec[ociclient.InitrdAnnotation] != "" {
		return fmt.Errorf("--shared-dir cannot be used with %s: urunc then boots the guest from that initrd, "+
			"and copies only files into it", ociclient.InitrdAnnotation)
	}
	if spec["com.urunc.unikernel.block"] != "" && spec["com.urunc.unikernel.blkMntPoint"] == "/" {
		return errors.New("--shared-dir cannot be used with a block device mounted at /: urunc then boots the guest " +
			"from that device, and copies only files into it")
	}
	// urunc takes a value it cannot parse as false.
	mountRootfs := spec["com.urunc.unikernel.mountRootfs"]
	mount, err := strconv.ParseBool(mountRootfs)
	if err != nil || !mount {
		return fmt.Errorf("--shared-dir needs urunc to mount the container's root for an image that boots its "+
			"own kernel, and com.urunc.unikernel.mountRootfs is %q: urunc gives such a guest no root from "+
			"the host, and so no share", mountRootfs)
	}
	if snapshotter != "overlayfs" {
		return fmt.Errorf("--shared-dir for an image that boots its own kernel needs the overlayfs snapshotter, "+
			"and the run's is %s: urunc boots such a guest from a snapshot that is a block device, as "+
			"devmapper's is, and copies only files into it, and hull cannot tell ahead of the run what "+
			"another snapshotter gives", snapshotter)
	}
	return nil
}

// ErrNotVM marks annotations urunc would not boot as a VM.
var ErrNotVM = errors.New("urunc would not boot this as a VM")

// checkBootsVM refuses spec annotations urunc would not boot as a VM, by
// the rules of urunc's validate (pkg/unikontainers/annotations.go): a
// unikernel type and a monitor, and a binary unless bootKernel or
// bootInitrd is set. When one is missing urunc looks for a urunc.json in
// the image's root filesystem, and when that is missing or invalid too it
// runs the container with runc, as an ordinary container on the host's
// kernel (cmd/urunc/create.go). hull reads no file from the image, so it
// cannot tell whether a urunc.json would save the run, and refuses it.
//
// The bundle's urunc fills in a missing type and monitor for a boot with a
// kernel and initrd, before validate, but hull requires them all the same,
// so the check does not depend on that. It cannot make up for a urunc that
// predates such boots altogether (urunc-dev/urunc f149eb7 and the commits
// after it): that one requires a binary in every case, so it runs a plain
// image with runc whatever the spec says. A half-given boot pair urunc
// rejects with an error of its own.
func checkBootsVM(annotations map[string]string) error {
	required := []string{ociclient.UnikernelTypeAnnotation, ociclient.HypervisorAnnotation}
	if annotations[ociclient.BootKernelAnnotation] == "" && annotations[ociclient.BootInitrdAnnotation] == "" {
		required = append(required, ociclient.UnikernelBinaryAnnotation)
	}
	var missing []string
	for _, k := range required {
		if annotations[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: urunc needs a unikernel type, a monitor, and a binary unless a boot "+
			"kernel and initrd are given, and the spec has no %s. It would look for a urunc.json in the "+
			"image instead, and without a valid one run the image with runc, as an ordinary "+
			"container on the host's kernel", ErrNotVM, strings.Join(missing, ", "))
	}
	return nil
}
