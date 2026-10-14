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

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

// migrateRecordVersion is the version of migration.json this hull writes.
const migrateRecordVersion = 1

// The two sides of a move.
const (
	roleSource      = "source"
	roleDestination = "destination"
)

// Source states, in the order a move passes them. docs/migration.md has the
// table. The fence comes before the transfer: in this version the bundle a
// move carries is the fenced checkpoint itself (there is no pre-copy), so
// there is nothing to send until the source has stopped for good.
const (
	srcPreflight    = "preflight"
	srcReserved     = "reserved"
	srcFenced       = "fenced"
	srcTransferring = "transferring"
	srcTransferred  = "transferred"
	srcCommitting   = "committing"
	srcCommitted    = "committed"
)

// Destination states, in order.
const (
	dstReserved       = "reserved"
	dstReceiving      = "receiving"
	dstReceived       = "received"
	dstVerified       = "verified"
	dstRestoredPaused = "restored-paused"
	dstRunning        = "running"
)

// migAborted ends a move on either side.
const migAborted = "aborted"

// migrateTransitions is every state change a record may make, by role. A
// step that is re-run finds its record already in the target state and
// moves nothing; anything not listed here is refused, so a bug or a stale
// process cannot walk a record backwards past the fence or the commit.
var migrateTransitions = map[string]map[string][]string{
	roleSource: {
		srcPreflight:    {srcReserved, migAborted},
		srcReserved:     {srcFenced, migAborted},
		srcFenced:       {srcTransferring, migAborted},
		srcTransferring: {srcTransferred, migAborted},
		srcTransferred:  {srcCommitting, migAborted},
		// committing may still abort: only once the destination has
		// confirmed it is not running and has recorded the abort, which
		// also makes it refuse a commit that arrives late.
		srcCommitting: {srcCommitted, migAborted},
	},
	roleDestination: {
		dstReserved:  {dstReceiving, migAborted},
		dstReceiving: {dstReceived, migAborted},
		// A received copy that failed verification is received again.
		dstReceived:       {dstVerified, dstReceiving, migAborted},
		dstVerified:       {dstRestoredPaused, migAborted},
		dstRestoredPaused: {dstRunning, migAborted},
	},
}

// migrateStateOrder ranks the states of role, for "has the move got this far
// yet". aborted ranks with none of them.
func migrateStateOrder(role, state string) int {
	order := []string{srcPreflight, srcReserved, srcFenced, srcTransferring, srcTransferred, srcCommitting, srcCommitted}
	if role == roleDestination {
		order = []string{dstReserved, dstReceiving, dstReceived, dstVerified, dstRestoredPaused, dstRunning}
	}
	for i, s := range order {
		if s == state {
			return i + 1
		}
	}
	return 0
}

// migrationRecord is <instance>/migration.json, on both sides of a move.
type migrationRecord struct {
	Version int    `json:"version"`
	TxID    string `json:"txid"`
	Role    string `json:"role"`
	Name    string `json:"name"`
	// Epoch is the owner epoch this move hands over: the source's
	// OwnerEpoch plus one. The commit carries it, and the destination
	// takes it as its own when it commits.
	Epoch uint64 `json:"epoch"`
	State string `json:"state"`
	// Peer is the other host: USER@HOST of the destination on the source,
	// the source's host name on the destination.
	Peer string `json:"peer"`

	// Source only: how to reach the destination's hull again on a retry.
	Remote *migrateRemote `json:"remote,omitempty"`

	// BundleDir and ManifestSHA256 name the fenced checkpoint the move
	// carries, on either side once it is there.
	BundleDir      string `json:"bundleDir,omitempty"`
	ManifestSHA256 string `json:"manifestSha256,omitempty"`
	// Disk is the guest's disk as fenced (source) or received (destination).
	Disk *migrateDisk `json:"disk,omitempty"`
	// Transfer is what the stream carried and how fast.
	Transfer *migrateTransfer `json:"transfer,omitempty"`
	// Spec is what the source asked the destination to build (destination).
	Spec *migrateSpec `json:"spec,omitempty"`
	// Verify is hvi checkpoint-verify's verdict on the received bundle.
	Verify string `json:"verify,omitempty"`
	// Timings are the milliseconds each step took, by step name.
	Timings map[string]int64 `json:"timingsMs,omitempty"`

	// Error is the last step's failure, kept for `hull migrate --status`.
	Error   string           `json:"error,omitempty"`
	History []migrateHistory `json:"history"`
	Created time.Time        `json:"createdAt"`
	Updated time.Time        `json:"updatedAt"`
}

// migrateRemote is how the source reaches the destination's hull.
type migrateRemote struct {
	Target      string `json:"target"`
	Store       string `json:"store,omitempty"`
	Hull        string `json:"hull,omitempty"`
	Hvi         string `json:"hvi,omitempty"`
	GatewaySock string `json:"gatewaySock,omitempty"`
	// AllowNoRestore lets the move go on although an hvi on either side
	// has no restore subcommand: for testing the transfer only. A move
	// with it can fence the source and then have no way to bring the VM
	// back on either host.
	AllowNoRestore bool `json:"allowNoRestore,omitempty"`
}

// migrateSpec is what the source asks the destination to build: the parts of
// its instance record and hvi command line that are not in the bundle.
type migrateSpec struct {
	SourceHost  string `json:"sourceHost"`
	ImageDigest string `json:"imageDigest,omitempty"`
	MAC         string `json:"mac,omitempty"`
	IP          string `json:"ip,omitempty"`
	SandboxID   string `json:"sandboxId,omitempty"`
	Agent       bool   `json:"agent,omitempty"`
	Events      bool   `json:"events,omitempty"`
	// Net is how the destination's guest NIC is backed: none, stub, or
	// gateway at GatewaySock, a socket on the destination.
	Net         string `json:"net"`
	GatewaySock string `json:"gatewaySock,omitempty"`
	// Hvi is the destination's hvi, when the operator named one.
	Hvi string `json:"hvi,omitempty"`
}

// migrateReserveRequest is migrate-receive --reserve's standard input.
type migrateReserveRequest struct {
	Epoch uint64      `json:"epoch"`
	Spec  migrateSpec `json:"spec"`
}

// migrateDisk describes the disk image a move carries.
type migrateDisk struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// ModNs is the source file's modification time at the fence, in
	// nanoseconds. A transfer that finds it changed refuses: something
	// wrote the disk after the guest was fenced.
	ModNs int64 `json:"modNs,omitempty"`
	// SHA256 is over the sparse stream (see docs/migration.md), the same
	// on both ends; DataBytes is how much of the image the stream carried.
	SHA256    string `json:"sha256,omitempty"`
	DataBytes int64  `json:"dataBytes,omitempty"`
}

// migrateTransfer is the stream's size and speed.
type migrateTransfer struct {
	Bytes       int64     `json:"bytes"`
	Seconds     float64   `json:"seconds"`
	BytesPerSec int64     `json:"bytesPerSec"`
	StartedAt   time.Time `json:"startedAt"`
}

// migrateHistory is one state change.
type migrateHistory struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

// txidPattern is a move's txid: 128 random bits in lower-case hex.
var txidPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// newMoveTxID draws a move's txid. 128 bits, not the 64 of a checkpoint's:
// a move's txid is matched across two hosts' stores and lives on in their
// ledgers, and a collision there would let one move's commit act on
// another's record.
func newMoveTxID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// checkTxID refuses anything that is not a move's txid. It goes into file
// names and remote command lines.
func checkTxID(txid string) error {
	if !txidPattern.MatchString(txid) {
		return fmt.Errorf("txid %q is not a move's txid (32 lower-case hex digits)", txid)
	}
	return nil
}

// advance moves rec to state to, if the transition table allows it, and
// writes it. Already being in to is not a move and succeeds.
func (rec *migrationRecord) advance(path, to string) error {
	if rec.State == to {
		return nil
	}
	allowed := false
	for _, next := range migrateTransitions[rec.Role][rec.State] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("move %s: %s cannot go from %s to %s", rec.TxID, rec.Role, rec.State, to)
	}
	rec.State = to
	rec.Error = ""
	rec.History = append(rec.History, migrateHistory{State: to, At: time.Now().UTC()})
	return writeMigrationRecord(path, rec)
}

// fail records why the current step stopped, without moving the state.
func (rec *migrationRecord) fail(path string, err error) error {
	rec.Error = err.Error()
	if werr := writeMigrationRecord(path, rec); werr != nil {
		return errors.Join(err, fmt.Errorf("record the failure: %w", werr))
	}
	return err
}

// writeMigrationRecord publishes rec at path: temp file, fsync, rename, then
// an fsync of the directory.
//
// Each state is a promise to the other host. "fenced" says the source will
// not run the guest again; "committing" says it has let the destination
// take over. A record that a crash could lose or tear would break the
// promise after it was made, so a state counts as recorded only once it is
// on stable storage, and only then does the step that relies on it go on.
func writeMigrationRecord(path string, rec *migrationRecord) error {
	rec.Version = migrateRecordVersion
	rec.Updated = time.Now().UTC()
	if rec.Created.IsZero() {
		rec.Created = rec.Updated
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory, so a rename or a create in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// readMigrationRecord reads the record at path.
func readMigrationRecord(path string) (*migrationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec migrationRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s does not read as a move record: %w", path, err)
	}
	if rec.Version != migrateRecordVersion {
		return nil, fmt.Errorf("%s is a version %d move record; this hull reads version %d",
			path, rec.Version, migrateRecordVersion)
	}
	return &rec, nil
}

// findMigrationByTxID finds the record of move txid in s: in an instance
// directory, or in the ledger of moves that ended here (committed or
// aborted). The instance directory wins: it is the record a move's steps
// write, and the ledger keeps the move's outcome once the instance has gone
// or moved on. It returns the record's path too. os.ErrNotExist when this
// store never saw the move.
func findMigrationByTxID(s *store.Store, txid string) (*migrationRecord, string, error) {
	if err := checkTxID(txid); err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(filepath.Join(s.RootDir(), "instances"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	for _, e := range entries {
		if !e.IsDir() || store.ValidateInstanceID(e.Name()) != nil {
			continue
		}
		path := s.InstanceMigrationRecord(e.Name())
		rec, err := readMigrationRecord(path)
		if err == nil && rec.TxID == txid {
			return rec, path, nil
		}
	}
	path := filepath.Join(s.MigrationTombstoneDir(), txid+".json")
	rec, err := readMigrationRecord(path)
	if err != nil {
		return nil, "", err
	}
	return rec, path, nil
}

// lockMove takes <instance>/migrate.lock, which one step of a move holds on
// its host. wait > 0 waits that long for it; 0 refuses at once.
//
// This is not the migration.lock of checkpoint and restore: a move's steps
// run `hull restore` as a child process, which takes that one, and a move
// must keep everything else out while it does. The two nest in one order
// only: migrate.lock first.
func lockMove(dir string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "migrate.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the move lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock the move: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errMoveBusy
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// errMoveBusy is another step of the same move holding its lock.
var errMoveBusy = errors.New("another step of this move is running on this host")

// moveRefusal is the refusal for an instance that a move has in hand, for a
// verb that would run it or change what a move carries: "" when none does.
func moveRefusal(s *store.Store, id, verb string) error {
	rec, err := readMigrationRecord(s.InstanceMigrationRecord(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s of %s refused: %w", verb, id, err)
	}
	switch {
	case rec.Role == roleSource && rec.State == srcCommitted:
		return fmt.Errorf("instance %s moved to %s (move %s) and runs there at epoch %d; what is left "+
			"of it here is stale. Moving it back is a new `hull migrate` run on %s", id, rec.Peer,
			rec.TxID, rec.Epoch, rec.Peer)
	case rec.Role == roleSource && rec.State != migAborted:
		return fmt.Errorf("instance %s is being moved to %s (move %s, %s): no %s until the move "+
			"commits, or `hull migrate --abort %s` ends it", id, rec.Peer, rec.TxID, rec.State, verb, rec.TxID)
	case rec.Role == roleDestination && rec.State == migAborted && verb != "rm":
		// The abort's cleanup did not finish: whatever is left may be the
		// restored, paused guest, which the source now runs again.
		return fmt.Errorf("instance %s is what is left here of move %s from %s, which was aborted: no %s. "+
			"`hull migrate-abort --txid %s` here, or `hull rm --force %s`, removes it", id, rec.TxID, rec.Peer,
			verb, rec.TxID, id)
	case rec.Role == roleDestination && rec.State != dstRunning && rec.State != migAborted:
		return fmt.Errorf("instance %s is arriving from %s (move %s, %s): only the move's commit "+
			"runs it, and `hull migrate --abort %s` on the source ends it", id, rec.Peer, rec.TxID,
			rec.State, rec.TxID)
	}
	return nil
}

// refuseDuringMove refuses verb on an instance a move has in hand.
func refuseDuringMove(s *store.Store, id, verb string) error {
	return moveRefusal(s, id, verb)
}

// moveCommittedAway reports whether the instance is the stale source of a
// committed move, which may be removed like any stopped instance.
func moveCommittedAway(s *store.Store, id string) bool {
	rec, err := readMigrationRecord(s.InstanceMigrationRecord(id))
	return err == nil && rec.Role == roleSource && rec.State == srcCommitted
}
