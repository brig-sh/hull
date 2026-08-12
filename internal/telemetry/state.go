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

package telemetry

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// stateFile is the name of the state file inside the store directory.
const stateFile = "telemetry.json"

// state is what we persist. InstallID is a random, locally-generated
// UUID: not derived from hardware or the user, and deleting the file
// rotates it. Consent is nil until the user has answered (or a flag
// recorded an answer for them); ConsentVersion records which version of
// the ask they answered.
type state struct {
	InstallID      string `json:"install_id"`
	Consent        *bool  `json:"consent,omitempty"`
	ConsentVersion int    `json:"consent_version,omitempty"`
}

func statePath(storeDir string) string {
	return filepath.Join(storeDir, stateFile)
}

// withStateLock serializes state access across processes with a flock
// on a sidecar lock file, so two concurrent first invocations cannot
// mint different install IDs or interleave partial writes. Lock
// trouble degrades to running fn unlocked: telemetry state must never
// break the CLI.
func withStateLock(storeDir string, fn func()) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		fn()
		return
	}
	f, err := os.OpenFile(filepath.Join(storeDir, "telemetry.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fn()
		return
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		fn()
		return
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	fn()
}

// loadState reads the state file, returning a fresh in-memory state
// (with a new install ID) when the file is missing or unreadable.
func loadState(storeDir string) *state {
	st := &state{}
	data, err := os.ReadFile(statePath(storeDir))
	if err == nil {
		_ = json.Unmarshal(data, st)
	}
	if st.InstallID == "" {
		st.InstallID = newUUID()
	}
	return st
}

// loadOrCreateState is loadState under the interprocess lock, and it
// persists a freshly minted install ID immediately -- concurrent first
// invocations then agree on one ID instead of each emitting their own.
// durable reports whether the returned ID is backed by disk: an ID
// read from an existing file always is, a fresh one only if the write
// succeeded. A non-durable ID must not be sent -- every invocation
// would mint a different one, exactly what the lock exists to prevent.
func loadOrCreateState(storeDir string) (st *state, durable bool) {
	withStateLock(storeDir, func() {
		data, err := os.ReadFile(statePath(storeDir))
		st = &state{}
		if err == nil {
			_ = json.Unmarshal(data, st)
		}
		if st.InstallID != "" {
			durable = true
			return
		}
		st.InstallID = newUUID()
		durable = writeState(storeDir, st) == nil
	})
	return st, durable
}

// saveState persists the state file under the lock. Errors are
// returned for the telemetry subcommand to report; event-path callers
// ignore them.
func saveState(storeDir string, st *state) error {
	var err error
	withStateLock(storeDir, func() {
		err = writeState(storeDir, st)
	})
	return err
}

// writeState writes atomically (temp file + rename) with user-only
// permissions, so a concurrent reader never observes a partial file.
func writeState(storeDir string, st *state) error {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(storeDir, ".telemetry-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), statePath(storeDir))
}

// newUUID returns a random (version 4) UUID string without pulling in a
// dependency for it.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
