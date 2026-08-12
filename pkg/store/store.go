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

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Store manages persistent state for urunc-macos images and instances
type Store struct {
	rootDir string
	mu      sync.RWMutex
}

// ImageMetadata holds metadata about a pulled OCI image
type ImageMetadata struct {
	Ref      string            `json:"ref"`
	Digest   string            `json:"digest"`
	PulledAt time.Time         `json:"pulledAt"`
	Labels   map[string]string `json:"labels,omitempty"`
	Size     int64             `json:"size"`
	// Platform is the os/arch[/variant] the image was pulled for. Empty on
	// records from before the field existed, which were all default pulls.
	Platform string `json:"platform,omitempty"`
}

// InstanceState holds the state of a running or stopped instance
type InstanceState struct {
	ID          string    `json:"id"`
	ImageDigest string    `json:"imageDigest"`
	PID         int       `json:"pid"`
	QMPSocket   string    `json:"qmpSocket"`
	Status      string    `json:"status"` // "running", "stopped", "error"
	StartTime   time.Time `json:"startTime"`
	LogFile     string    `json:"logFile"`
	CmdLine     []string  `json:"cmdLine"`
	BundleDir   string    `json:"bundleDir"`
	MAC         string    `json:"mac,omitempty"`
	IP          string    `json:"ip,omitempty"`
	// ExitCode is the guest process's exit status when one could be
	// observed, nil otherwise. A stopped instance with a nil code ended
	// without a reportable status: today only one-shot jobs, which run
	// their command through the guest agent, can report one (ADR-0007).
	ExitCode *int `json:"exitCode,omitempty"`
	// ExitedAt is when the instance was observed to have ended; zero while
	// running or when the end was never observed.
	ExitedAt time.Time `json:"exitedAt,omitempty"`
	// StoppedByUser records that this instance was stopped deliberately
	// (`urunc-macos stop`), as opposed to having died. A supervisor must not
	// restart it whatever its policy says: docker's restart manager makes the
	// same distinction, and without it "stop" is undone within a poll.
	StoppedByUser bool `json:"stoppedByUser,omitempty"`
	// Backend is the VMM type (qemu | vz) this instance ran under,
	// persisted so telemetry can tag the `metrics` and `end` events a
	// later `exec` attach or `stop` emits. Empty on instances created
	// before this field existed.
	Backend string `json:"backend,omitempty"`
	// TelemetryEndSent guards the telemetry `end` event to exactly once
	// across the foreground exit path and `stop`.
	TelemetryEndSent bool `json:"telemetryEndSent,omitempty"`
}

var ErrInstanceNotFound = errors.New("instance not found")
var ErrInstanceExists = errors.New("instance already exists")
var ErrImageNotFound = errors.New("image not found")
var ErrInvalidInstanceID = errors.New("invalid instance name")

// ValidateInstanceID rejects any id that is not a single safe path element.
//
// The store lays every instance out as <root>/instances/<id>, so the id is a
// directory name, not a path: one carrying a separator or a ".." component
// places the bundle, the state file and the log outside the store, where the
// instance is invisible to ps and where DeleteInstance would later recurse.
// The compose front-end already validates container_name this way; the store
// owns the layout, so the check belongs here too and covers every caller.
func ValidateInstanceID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: the name is empty", ErrInvalidInstanceID)
	}
	if id == "." || id == ".." || id != filepath.Base(id) ||
		strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("%w %q: a name is a single path element, with no '/', '\\' or '..'",
			ErrInvalidInstanceID, id)
	}
	return nil
}

// New creates a new Store at the given root directory
func New(rootDir string) (*Store, error) {
	// Expand home directory if needed
	if rootDir == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		rootDir = home
	} else if len(rootDir) > 0 && rootDir[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		rootDir = filepath.Join(home, rootDir[1:])
	}

	if err := os.MkdirAll(rootDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create store root: %w", err)
	}

	store := &Store{rootDir: rootDir}

	// Create subdirectories
	for _, subdir := range []string{"images", "instances"} {
		if err := os.MkdirAll(filepath.Join(rootDir, subdir), 0700); err != nil {
			return nil, fmt.Errorf("failed to create store subdirectory %s: %w", subdir, err)
		}
	}

	return store, nil
}

// SaveImage stores image metadata and returns the image directory path
func (s *Store) SaveImage(digest string, metadata *ImageMetadata) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	imageDir := filepath.Join(s.rootDir, "images", digest)
	if err := os.MkdirAll(imageDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create image directory: %w", err)
	}

	if err := WriteImageMetadata(imageDir, metadata); err != nil {
		return "", err
	}

	return imageDir, nil
}

// WriteImageMetadata writes image.json into an arbitrary directory. The pull
// path uses this to place the metadata inside the staging directory, so the
// rename that publishes the rootfs publishes the metadata with it: there is
// never a moment where one exists without the other.
func WriteImageMetadata(dir string, metadata *ImageMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal image metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "image.json"), data, 0600); err != nil {
		return fmt.Errorf("failed to write image metadata: %w", err)
	}

	return nil
}

// isStagingDir reports whether an images/ entry is pull scaffolding rather than
// a published image. The pull path stages under <digest>.tmp-<pid> and displaces
// the previous image to <digest>.old-<pid>; both now carry an image.json, so
// name is the only thing that distinguishes them from the real entry.
func isStagingDir(name string) bool {
	return strings.Contains(name, ".tmp-") || strings.Contains(name, ".old-")
}

// ImageComplete reports whether a cached image is actually usable, i.e. both
// its metadata and its unpacked rootfs are present.
//
// Metadata alone is not enough. An interrupted pull can leave image.json
// behind with no rootfs (os.RemoveAll of a large rootfs is not instant, and
// deletes in readdir order), and callers that trusted the metadata would then
// resolve the image as cached and fail on every subsequent run, with no way
// out but deleting the whole store by hand.
func (s *Store) ImageComplete(digest string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	imageDir := filepath.Join(s.rootDir, "images", digest)
	if _, err := os.Stat(filepath.Join(imageDir, "image.json")); err != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(imageDir, "rootfs"))
	return err == nil && fi.IsDir()
}

// GetImage retrieves image metadata by digest
func (s *Store) GetImage(digest string) (*ImageMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaPath := filepath.Join(s.rootDir, "images", digest, "image.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrImageNotFound
		}
		return nil, fmt.Errorf("failed to read image metadata: %w", err)
	}

	var metadata ImageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal image metadata: %w", err)
	}

	return &metadata, nil
}

// ListImages returns all stored images
func (s *Store) ListImages() ([]*ImageMetadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	imagesDir := filepath.Join(s.rootDir, "images")
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list images directory: %w", err)
	}

	var images []*ImageMetadata
	for _, entry := range entries {
		if !entry.IsDir() || isStagingDir(entry.Name()) {
			continue
		}

		metaPath := filepath.Join(imagesDir, entry.Name(), "image.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue // Skip invalid entries
		}

		var metadata ImageMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			continue
		}

		images = append(images, &metadata)
	}

	return images, nil
}

// ImageRootfsDir returns the rootfs directory for an image
func (s *Store) ImageRootfsDir(digest string) string {
	return filepath.Join(s.rootDir, "images", digest, "rootfs")
}

// CreateInstance initializes a new instance directory
func (s *Store) CreateInstance(id string) (string, error) {
	if err := ValidateInstanceID(id); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockFile()
	if err != nil {
		return "", err
	}
	defer unlock()

	instanceDir := filepath.Join(s.rootDir, "instances", id)
	if err := os.Mkdir(instanceDir, 0700); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("%w: %s", ErrInstanceExists, id)
		}
		return "", fmt.Errorf("failed to create instance directory: %w", err)
	}

	for _, subdir := range []string{"bundle"} {
		if err := os.MkdirAll(filepath.Join(instanceDir, subdir), 0700); err != nil {
			return "", fmt.Errorf("failed to create instance subdirectory: %w", err)
		}
	}

	return instanceDir, nil
}

// SaveInstance persists instance state
func (s *Store) SaveInstance(state *InstanceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	instanceDir := filepath.Join(s.rootDir, "instances", state.ID)
	stateFile := filepath.Join(instanceDir, "state.json")

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal instance state: %w", err)
	}

	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write instance state: %w", err)
	}

	return nil
}

// GetInstance retrieves instance state by ID
func (s *Store) GetInstance(id string) (*InstanceState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stateFile := filepath.Join(s.rootDir, "instances", id, "state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrInstanceNotFound
		}
		return nil, fmt.Errorf("failed to read instance state: %w", err)
	}

	var state InstanceState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal instance state: %w", err)
	}

	return &state, nil
}

// ListInstances returns all stored instances
func (s *Store) ListInstances() ([]*InstanceState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	instancesDir := filepath.Join(s.rootDir, "instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances directory: %w", err)
	}

	var instances []*InstanceState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		stateFile := filepath.Join(instancesDir, entry.Name(), "state.json")
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue
		}

		var state InstanceState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		instances = append(instances, &state)
	}

	return instances, nil
}

// InstanceDir returns the base directory for an instance
func (s *Store) InstanceDir(id string) string {
	return filepath.Join(s.rootDir, "instances", id)
}

// InstanceBundleDir returns the OCI bundle directory for an instance
func (s *Store) InstanceBundleDir(id string) string {
	return filepath.Join(s.rootDir, "instances", id, "bundle")
}

// InstanceLogFile returns the log file path for an instance
func (s *Store) InstanceLogFile(id string) string {
	return filepath.Join(s.rootDir, "instances", id, "log")
}

// InstanceQMPSocket returns the QMP socket path for an instance
func (s *Store) InstanceQMPSocket(id string) string {
	return filepath.Join(s.rootDir, "instances", id, "qmp.sock")
}

// InstanceAgentSocket returns the urunit-agent socket path for an instance.
// The VMM bridges this unix socket to the in-guest agent transport
// (virtio-serial on QEMU, vsock on Vz).
func (s *Store) InstanceAgentSocket(id string) string {
	return filepath.Join(s.rootDir, "instances", id, "agent.sock")
}

// DeleteInstance removes an instance directory
func (s *Store) DeleteInstance(id string) error {
	// This one recurses, so it validates even though callers resolve the
	// record first: a RemoveAll must never be handed a traversing path.
	if err := ValidateInstanceID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	instanceDir := filepath.Join(s.rootDir, "instances", id)
	return os.RemoveAll(instanceDir)
}

// RootDir returns the root directory of the store
func (s *Store) RootDir() string {
	return s.rootDir
}

// lockFile takes an exclusive advisory lock on the store, serializing
// mutations across processes (the in-process mutex only covers one process).
// It returns an unlock function.
func (s *Store) lockFile() (func(), error) {
	lockPath := filepath.Join(s.rootDir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open store lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock store: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
