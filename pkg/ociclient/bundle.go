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

package ociclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Annotation keys for urunc unikernel configuration (from pkg/unikontainers/config.go)
const (
	UnikernelTypeAnnotation    = "com.urunc.unikernel.unikernelType"
	HypervisorAnnotation       = "com.urunc.unikernel.hypervisor"
	CmdlineAnnotation          = "com.urunc.unikernel.cmdline"
	UnikernelBinaryAnnotation  = "com.urunc.unikernel.binary"
	InitrdAnnotation           = "com.urunc.unikernel.initrd"
	UnikernelVersionAnnotation = "com.urunc.unikernel.version"
)

// GenerateBundle creates an OCI runtime bundle (config.json + rootfs symlink) in the bundle directory
func (c *Client) GenerateBundle(bundleDir, imageDigest string, imgConfig *v1.ConfigFile) error {
	// Load the saved image metadata to get the exact labels stored during pull
	imgMetadata, err := c.store.GetImage(imageDigest)
	if err == nil && imgMetadata.Labels != nil {
		// Use the stored labels which may have more complete annotations
		// than what's in the OCI ConfigFile
		if imgConfig == nil {
			imgConfig = &v1.ConfigFile{}
		}
		if imgConfig.Config.Labels == nil {
			imgConfig.Config.Labels = make(map[string]string)
		}
		// Merge stored labels into config, with stored labels taking precedence
		for k, v := range imgMetadata.Labels {
			imgConfig.Config.Labels[k] = v
		}
	}
	// Create bundle directory structure
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		return fmt.Errorf("failed to create bundle directory: %w", err)
	}

	// Create rootfs symlink to the image rootfs
	rootfsLink := filepath.Join(bundleDir, "rootfs")
	if err := os.RemoveAll(rootfsLink); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing rootfs symlink: %w", err)
	}

	// Use absolute symlink to the image rootfs directory
	// Use digest as-is since it's stored exactly as returned (with "sha256:" prefix if present)
	storeRoot := c.store.RootDir()
	absTarget := filepath.Join(storeRoot, "images", imageDigest, "rootfs")

	if err := os.Symlink(absTarget, rootfsLink); err != nil {
		return fmt.Errorf("failed to create rootfs symlink: %w", err)
	}

	// Create OCI runtime spec
	spec := createOCISpec(imgConfig, filepath.Join(bundleDir, "rootfs"))

	// Write config.json
	configPath := filepath.Join(bundleDir, "config.json")
	specData, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal OCI spec: %w", err)
	}

	if err := os.WriteFile(configPath, specData, 0644); err != nil {
		return fmt.Errorf("failed to write config.json: %w", err)
	}

	log.Debugf("Bundle created at %s", bundleDir)
	return nil
}

// createOCISpec creates an OCI runtime spec from an OCI image config
func createOCISpec(imgConfig *v1.ConfigFile, rootfsDir string) *specs.Spec {
	if imgConfig == nil {
		imgConfig = &v1.ConfigFile{}
	}

	// Start with a minimal spec
	spec := &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path: "rootfs",
		},
		Process: &specs.Process{
			Terminal: false,
			User: specs.User{
				UID: 0,
				GID: 0,
			},
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    []string{},
				Effective:   []string{},
				Inheritable: []string{},
				Permitted:   []string{},
				Ambient:     []string{},
			},
		},
		Hostname:    "unikernel",
		Mounts:      []specs.Mount{},
		Annotations: make(map[string]string),
	}

	// Copy image labels as annotations
	for k, v := range imgConfig.Config.Labels {
		spec.Annotations[k] = v
	}

	// Set command from image config (Docker semantics: Args = Entrypoint + Cmd)
	if len(imgConfig.Config.Entrypoint) > 0 {
		spec.Process.Args = append(imgConfig.Config.Entrypoint, imgConfig.Config.Cmd...)
	} else if len(imgConfig.Config.Cmd) > 0 {
		spec.Process.Args = imgConfig.Config.Cmd
	} else {
		spec.Process.Args = []string{"/bin/sh"}
	}

	// Set environment variables
	spec.Process.Env = append(spec.Process.Env, imgConfig.Config.Env...)

	// Set working directory
	if imgConfig.Config.WorkingDir != "" {
		spec.Process.Cwd = imgConfig.Config.WorkingDir
	}

	// Resolve the image-configured user (name, uid, user:group ...) into
	// numeric ids the way container runtimes do: names resolve against
	// the image's own /etc/passwd and /etc/group.
	if imgConfig.Config.User != "" {
		uid, gid, home, err := resolveImageUser(rootfsDir, imgConfig.Config.User)
		if err != nil {
			log.Warnf("cannot resolve image user %q, keeping uid 0: %v", imgConfig.Config.User, err)
		} else {
			spec.Process.User.UID = uid
			spec.Process.User.GID = gid
			// Docker semantics: HOME follows the user unless the image
			// set it explicitly.
			hasHome := false
			for _, e := range spec.Process.Env {
				if strings.HasPrefix(e, "HOME=") {
					hasHome = true
					break
				}
			}
			if !hasHome && home != "" {
				spec.Process.Env = append(spec.Process.Env, "HOME="+home)
			}
		}
	}

	// Only set default annotations if no urunc annotations came from image labels.
	// Images built with bunny store annotations in urunc.json (read later by run.go),
	// so we should not override with defaults here.
	hasUruncAnnotations := false
	for k := range spec.Annotations {
		if k == UnikernelTypeAnnotation || k == HypervisorAnnotation || k == UnikernelBinaryAnnotation {
			hasUruncAnnotations = true
			break
		}
	}

	if !hasUruncAnnotations {
		spec.Annotations[UnikernelTypeAnnotation] = "linux"
		spec.Annotations[HypervisorAnnotation] = "qemu-hvf"
		spec.Annotations[UnikernelBinaryAnnotation] = "unikernel"
		spec.Annotations["com.urunc.unikernel.kernel"] = "unikernel"
	}

	return spec
}

// LoadImageConfig loads the OCI image config from a stored image
func (c *Client) LoadImageConfig(imageDigest string) (*v1.ConfigFile, error) {
	configPath := filepath.Join(c.store.RootDir(), "images", imageDigest, "oci-config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Return empty config if not found - it's not critical
		return &v1.ConfigFile{}, nil
	}

	var config v1.ConfigFile
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal image config: %w", err)
	}

	return &config, nil
}
