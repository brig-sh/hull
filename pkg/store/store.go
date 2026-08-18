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

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Store manages persistent state for hull images and instances
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
	Status      string    `json:"status"` // StatusStarting, "running", "stopped", "error"
	StartTime   time.Time `json:"startTime"`
	LogFile     string    `json:"logFile"`
	CmdLine     []string  `json:"cmdLine"`
	BundleDir   string    `json:"bundleDir"`
	MAC         string    `json:"mac,omitempty"`
	IP          string    `json:"ip,omitempty"`
	// ExitCode is the guest process's exit status when one could be
	// observed, nil otherwise. A stopped instance with a nil code ended
	// without a reportable status: today only one-shot jobs, which run
	// their command through the guest agent, can report one.
	ExitCode *int `json:"exitCode,omitempty"`
	// ExitedAt is when the instance was observed to have ended; zero while
	// running or when the end was never observed.
	ExitedAt time.Time `json:"exitedAt,omitempty"`
	// StoppedByUser records that this instance was stopped deliberately
	// (`hull stop`), as opposed to having died. A supervisor must not
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

// StatusStarting marks the one moment an instance has no pid to record: the
// VMM is about to be spawned, or has just been spawned and its pid has not
// been written yet.
//
// It exists so that no VMM is ever running without a record of it. hull used to
// write the record only after exec had succeeded, and a SIGKILL of hull in
// between left a live VMM that `ps` could not see, `stop` could not reach and
// `rm` answered "instance not found" for -- while the directory kept the name
// permanently squatted, so every later run failed ErrInstanceExists. A record
// written before the spawn turns that into an instance an operator can see and
// remove.
//
// Anything asking "is this usable?" must read it as not-running: it carries no
// pid, so there is nothing to signal, attach to or sample.
const StatusStarting = "starting"

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
	// Beyond path safety: the id is printed, matched and parsed, so it has to be
	// something those operations can survive.
	//
	// The rule above admitted newlines, and `hull ps` prints the id into a
	// table that brig parses line by line -- so one instance could forge a row
	// for another, and brig's Running("victim") returned true for a VM that did
	// not exist. It also admitted ESC (escapes straight to the terminal, on a
	// path the guest-output filter does not cover), spaces and colons (which
	// urunc's qemu backend splits its argv on), and names of any length.
	//
	// This is the same shape compose already enforces for container_name, so
	// nothing that works through compose is affected.
	if len(id) > maxInstanceIDLen {
		return fmt.Errorf("%w %q: a name is at most %d characters",
			ErrInvalidInstanceID, id, maxInstanceIDLen)
	}
	if !instanceIDPattern.MatchString(id) {
		return fmt.Errorf("%w %q: a name starts with a letter or digit and contains only "+
			"letters, digits, '.', '_' and '-'", ErrInvalidInstanceID, id)
	}
	return nil
}

// maxInstanceIDLen bounds a name at the filesystem's own limit for one path
// component. Anything longer cannot become a directory anyway, so this reports
// it as a bad name rather than as a mkdir failure later.
//
// Not tightened further on purpose: an existing test pins a 200-character name
// as valid, and there is no reason beyond neatness to break that.
const maxInstanceIDLen = 255

// instanceIDPattern is what a name may contain. Deliberately narrow: everything
// outside it has bitten somebody once already.
var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

	if err := ValidateInstanceID(state.ID); err != nil {
		return err
	}

	instanceDir := filepath.Join(s.rootDir, "instances", state.ID)
	stateFile := filepath.Join(instanceDir, "state.json")

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal instance state: %w", err)
	}

	// Temp file, then rename. os.WriteFile opens O_TRUNC and writes in place,
	// so the record spent a moment empty on every save -- and a crash or a
	// concurrent reader in that moment produced a truncated file that
	// ListInstances silently skips. The instance then vanishes from ps, stop
	// and inspect say "not found", and rm deletes the directory out from under
	// a VM that is still running. Measured on the real binary: an instance
	// dropped from ps 86 times in 62,053 probes at an ordinary save rate, and
	// brig polls hull ps.
	//
	// saveProject in the compose package has done this since it was written,
	// with a comment naming the same hazard. The instance record just never got
	// the same treatment, and round 5 doubled the number of saves per run.
	//
	// The temp file is created in the instance directory so the rename is on
	// one filesystem, and it is cleaned up on every failure path.
	tmp, err := os.CreateTemp(instanceDir, "state.json.tmp-*")
	if err != nil {
		return fmt.Errorf("failed to write instance state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write instance state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write instance state: %w", err)
	}
	// Durability before visibility: a rename that lands before the bytes do
	// leaves a record that is present and empty, which is the state this whole
	// change exists to make impossible.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write instance state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write instance state: %w", err)
	}
	if err := os.Rename(tmpName, stateFile); err != nil {
		return fmt.Errorf("failed to write instance state: %w", err)
	}

	return nil
}

// DeleteInstanceRecord removes an instance's state file and nothing else.
//
// This is the undo for a record published before a VMM was spawned: when the
// spawn fails there is no process to describe, and leaving a StatusStarting
// record behind would report a VMM that never existed. The directory is left
// alone -- its lifetime belongs to the caller that created it, which removes
// the whole thing on any pre-start failure.
func (s *Store) DeleteInstanceRecord(id string) error {
	if err := ValidateInstanceID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	err = os.Remove(filepath.Join(s.rootDir, "instances", id, "state.json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove instance state: %w", err)
	}
	return nil
}

// GetInstance retrieves instance state by ID
func (s *Store) GetInstance(id string) (*InstanceState, error) {
	// Validated here, not just on the write paths.
	//
	// Without this, GetInstance would read a state.json from anywhere on the
	// filesystem: `hull restore '../../../../tmp/outside/evil'` found an
	// attacker-placed record and hull then executed its cmdLine[0], the only
	// check being that argv[0] contained "vz-runner" -- satisfied by naming the
	// payload payload-vz-runner. Caller-controlled today, and one wrapper or
	// project file away from not being.
	if err := ValidateInstanceID(id); err != nil {
		return nil, err
	}

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

// instancePath joins a validated id onto the instance root.
//
// These accessors return a path with no error, so an id that should never have
// reached them cannot be reported -- it can only be prevented from escaping.
// filepath.Base collapses any traversal to its last element, so the worst an
// invalid id can now do is name a sibling directory inside the store instead of
// a file anywhere on the disk. Every path that can report an error validates
// properly first; this is the backstop for the ones that cannot.
func (s *Store) instancePath(id string, rest ...string) string {
	if ValidateInstanceID(id) != nil {
		id = filepath.Base(filepath.Clean(id))
		if id == "." || id == ".." || id == string(filepath.Separator) {
			id = "invalid-instance-id"
		}
	}
	return filepath.Join(append([]string{s.rootDir, "instances", id}, rest...)...)
}

// InstanceDir returns the base directory for an instance
func (s *Store) InstanceDir(id string) string {
	return s.instancePath(id)
}

// InstanceBundleDir returns the OCI bundle directory for an instance
func (s *Store) InstanceBundleDir(id string) string {
	return s.instancePath(id, "bundle")
}

// InstanceLogFile returns the log file path for an instance
func (s *Store) InstanceLogFile(id string) string {
	return s.instancePath(id, "log")
}

// InstanceQMPSocket returns the QMP socket path for an instance
func (s *Store) InstanceQMPSocket(id string) string {
	return s.instancePath(id, "qmp.sock")
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
