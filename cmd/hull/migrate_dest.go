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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"golang.org/x/sys/unix"

	"github.com/brig-sh/hull/pkg/store"
)

// The destination's half of a move: the hidden verbs `hull migrate` runs
// over ssh. Each prints one migrateReply on stdout and nothing else, and
// each is safe to run again: a step that finds its effect already there
// answers as if it had just done it.

// moveLockWait is how long a destination verb waits for another step of the
// same move to finish before it answers busy.
const moveLockWait = 30 * time.Second

// destRestore brings the received checkpoint of instance name up paused, for
// move txid. A variable so the tests can stand in for hvi.
var destRestore = runDestRestore

// destResume resumes the restored guest at commit. A variable for the same
// reason.
var destResume = func(s *store.Store, name string) error {
	state, err := s.GetInstance(name)
	if err != nil {
		return err
	}
	if err := markRestoredGuestRunning(s, name); err != nil {
		return err
	}
	_, err = hviOp(state.ControlSock, "resume")
	return err
}

// destHviPath is the hvi a destination uses: the one the operator named, or
// the one `hull run` would.
func destHviPath(override string) (string, error) {
	if override != "" {
		info, err := os.Stat(override)
		if err != nil {
			return "", fmt.Errorf("hvi %s: %w", override, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("hvi %s is not an executable", override)
		}
		if filepath.Base(override) != "hvi" {
			return "", fmt.Errorf("hvi %s must be named hvi: hull tells an hvi instance by its program's name", override)
		}
		return override, nil
	}
	vmm, err := newVMM(hypervisors.HviVmm, nil)
	if err != nil {
		return "", err
	}
	return vmm.Path(), nil
}

// hviVersion asks an hvi for its version line.
func hviVersion(binary string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// storeFreeBytes is the space left for an unprivileged user under dir.
func storeFreeBytes(dir string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) //nolint:gosec,unconvert // block counts fit; Bsize's type differs by OS
}

// destProbe reports what a source needs to know before it fences anything.
// It changes nothing.
func destProbe(s *store.Store, name, hviOverride, gatewaySock string) *migrateReply {
	p := &migrateProbe{
		HullVersion: build.String(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		StoreDir:    s.RootDir(),
		FreeBytes:   storeFreeBytes(s.RootDir()),
	}
	p.Hypervisor, p.HypervisorDetail = hypervisorAvailable()
	if hvi, err := destHviPath(hviOverride); err != nil {
		p.HviError = err.Error()
	} else {
		p.Hvi = hvi
		p.HviVersion = hviVersion(hvi)
		if ok, err := hviBootTakesControlFlags(hvi); err != nil {
			p.HviError = err.Error()
		} else {
			p.HviControlFlags = ok
		}
		if ok, err := hviHasRestore(hvi); err != nil {
			p.HviError = err.Error()
		} else {
			p.HviRestore = ok
		}
	}
	if name != "" {
		if err := store.ValidateInstanceID(name); err != nil {
			return refuse(codeFailed, err)
		}
		if _, err := os.Stat(s.InstanceDir(name)); err == nil {
			p.NameTaken = true
			if rec, err := readMigrationRecord(s.InstanceMigrationRecord(name)); err == nil {
				p.NameTxID = rec.TxID
				p.NameMove = rec.Role + " " + rec.State
			}
		}
	}
	if gatewaySock != "" {
		conn, err := net.DialTimeout("unix", gatewaySock, 2*time.Second)
		if err != nil {
			p.GatewayDetail = err.Error()
		} else {
			_ = conn.Close()
			p.GatewayOK = true
		}
	}
	return &migrateReply{OK: true, Probe: p}
}

// refuse is a definite no.
func refuse(code string, err error) *migrateReply {
	return &migrateReply{OK: false, Code: code, Error: err.Error()}
}

// refuseWith is a definite no that carries the record it is about.
func refuseWith(code string, err error, rec *migrationRecord) *migrateReply {
	r := refuse(code, err)
	r.Record = rec
	return r
}

// destReserve claims name for move txid: the instance directory and its
// migration.json, state reserved. Nothing is received yet, so a reservation
// costs the destination nothing but the name.
func destReserve(s *store.Store, txid, name string, req migrateReserveRequest) *migrateReply {
	if err := checkTxID(txid); err != nil {
		return refuse(codeFailed, err)
	}
	if err := store.ValidateInstanceID(name); err != nil {
		return refuse(codeFailed, err)
	}
	if rec, _, err := findMigrationByTxID(s, txid); err == nil {
		switch {
		case rec.State == migAborted:
			return refuseWith(codeAborted, fmt.Errorf("move %s was aborted here", txid), rec)
		case rec.Name != name || rec.Role != roleDestination:
			return refuseWith(codeFailed, fmt.Errorf("move %s is recorded here for %s as %s", txid, rec.Name, rec.Role), rec)
		}
		return &migrateReply{OK: true, Record: rec}
	}
	if _, err := destHviPath(req.Spec.Hvi); err != nil {
		return refuse(codeFailed, err)
	}
	for _, sock := range []struct{ what, path string }{
		{"agent", s.InstanceAgentSocket(name)}, {"control", s.InstanceControlSocket(name)},
	} {
		if err := checkUnixSocketPath(sock.what, sock.path); err != nil {
			return refuse(codeFailed, err)
		}
	}
	dir, err := s.CreateInstance(name)
	if errors.Is(err, store.ErrInstanceExists) {
		return refuse(codeNameTaken, fmt.Errorf("an instance named %s already exists here", name))
	}
	if err != nil {
		return refuse(codeFailed, err)
	}
	spec := req.Spec
	rec := &migrationRecord{
		TxID: txid, Role: roleDestination, Name: name, Epoch: req.Epoch, State: dstReserved,
		Peer: spec.SourceHost, Spec: &spec,
		History: []migrateHistory{{State: dstReserved, At: time.Now().UTC()}},
	}
	if err := writeMigrationRecord(s.InstanceMigrationRecord(name), rec); err != nil {
		_ = store.RemoveTree(dir)
		return refuse(codeFailed, err)
	}
	return &migrateReply{OK: true, Record: rec}
}

// destRecord finds move txid's record for a destination verb, locked. The
// unlock is nil when there is nothing to unlock.
func destRecord(s *store.Store, txid string) (*migrationRecord, string, func(), *migrateReply) {
	rec, path, err := findMigrationByTxID(s, txid)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil, refuse(codeUnknownTxID, fmt.Errorf("no move %s is recorded here", txid))
	}
	if err != nil {
		return nil, "", nil, refuse(codeFailed, err)
	}
	if rec.Role != roleDestination {
		return nil, "", nil, refuseWith(codeFailed, fmt.Errorf("move %s is recorded here as its %s", txid, rec.Role), rec)
	}
	if rec.State == migAborted {
		return rec, path, nil, nil
	}
	unlock, err := lockMove(filepath.Dir(path), moveLockWait)
	if err != nil {
		return nil, "", nil, refuseWith(codeBusy, err, rec)
	}
	// Read again under the lock: the step that held it may have moved it.
	if rec, err = readMigrationRecord(path); err != nil {
		unlock()
		return nil, "", nil, refuse(codeFailed, err)
	}
	return rec, path, unlock, nil
}

// destReceive reads the stream on in into the instance reserved for txid,
// checks it against its own trailer and the bundle's manifest, puts the
// bundle and the disk where `hull restore` will look, and has hvi verify the
// bundle. It ends in verified, with a record of the instance written in the
// incoming state that only this move can restore and resume.
func destReceive(s *store.Store, txid, name string, in io.Reader) *migrateReply {
	rec, path, unlock, no := destRecord(s, txid)
	if no != nil {
		return no
	}
	if unlock != nil {
		defer unlock()
	}
	if rec.State == migAborted {
		return refuseWith(codeAborted, fmt.Errorf("move %s was aborted here", txid), rec)
	}
	if rec.Name != name {
		return refuseWith(codeFailed, fmt.Errorf("move %s is for %s here, not %s", txid, rec.Name, name), rec)
	}
	if migrateStateOrder(roleDestination, rec.State) >= migrateStateOrder(roleDestination, dstVerified) {
		return &migrateReply{OK: true, Record: rec}
	}
	if err := rec.advance(path, dstReceiving); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	instDir := s.InstanceDir(name)
	staging := filepath.Join(instDir, "incoming")
	_ = store.RemoveTree(staging)
	if err := os.MkdirAll(filepath.Join(staging, "bundle"), 0o700); err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	started := time.Now()
	res, err := readMigrateStream(in, func(fname string, _ int64) (*os.File, error) {
		return os.OpenFile(filepath.Join(staging, filepath.FromSlash(fname)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	})
	if err == nil && res.Header.TxID != txid {
		err = fmt.Errorf("the stream is for move %s, not %s", res.Header.TxID, txid)
	}
	if err != nil {
		_ = store.RemoveTree(staging)
		return refuseWith(codeFailed, rec.fail(path, fmt.Errorf("receive: %w", err)), rec)
	}
	took := time.Since(started)
	disk, err := checkReceivedBundle(filepath.Join(staging, "bundle"), res)
	if err != nil {
		_ = store.RemoveTree(staging)
		return refuseWith(codeVerifyFailed, rec.fail(path, err), rec)
	}

	// Into place: the bundle below the checkpoint root its hvi will be
	// given, the disk where a run keeps it.
	root, err := hviCheckpointRoot(instDir)
	if err == nil {
		err = placeReceived(staging, root, txid, instDir)
	}
	if err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	rec.BundleDir = filepath.Join(root, txid)
	rec.ManifestSHA256 = res.Header.ManifestSHA256
	rec.Disk = &migrateDisk{Path: filepath.Join(instDir, "bundle", "rootfs.ext4"), Size: disk.Size,
		SHA256: disk.SHA256, DataBytes: disk.DataBytes}
	rec.Transfer = &migrateTransfer{Bytes: res.Bytes, Seconds: took.Seconds(), StartedAt: started.UTC(),
		BytesPerSec: int64(float64(res.Bytes) / max(took.Seconds(), 1e-3))}
	if err := rec.advance(path, dstReceived); err != nil {
		return refuseWith(codeFailed, err, rec)
	}

	hvi, err := destHviPath(rec.Spec.Hvi)
	if err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	verdict, err := hviCheckpointVerify(hvi, rec.BundleDir)
	if err != nil {
		return refuseWith(codeVerifyFailed, rec.fail(path, err), rec)
	}
	rec.Verify = verdict
	if err := writeIncomingInstance(s, rec, hvi, root); err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	if err := rec.advance(path, dstVerified); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	return &migrateReply{OK: true, Record: rec}
}

// checkReceivedBundle checks the received bundle against its manifest:
// manifest.json hashes to what the stream header says hvi reported at the
// fence, every component the manifest lists arrived with its size and
// sha256, nothing else did, and the disk is the size disk.json says. It
// returns the disk's line from the stream.
func checkReceivedBundle(dir string, res *migrateStreamResult) (streamSum, error) {
	got := map[string]streamSum{}
	for _, f := range res.Files {
		got[f.Name] = f
	}
	disk, ok := got["disk"]
	if !ok {
		return streamSum{}, errors.New("the stream carried no disk")
	}
	man, ok := got["bundle/manifest.json"]
	if !ok {
		return streamSum{}, errors.New("the stream carried no manifest.json")
	}
	if res.Header.ManifestSHA256 == "" || man.SHA256 != res.Header.ManifestSHA256 {
		return streamSum{}, fmt.Errorf("manifest.json arrived with sha256 %s, but hvi reported %s at the fence",
			man.SHA256, res.Header.ManifestSHA256)
	}
	comps, err := readManifestComponents(dir)
	if err != nil {
		return streamSum{}, err
	}
	want := map[string]bool{"disk": true, "bundle/manifest.json": true}
	for _, c := range comps {
		name := "bundle/" + c.Path
		want[name] = true
		f, ok := got[name]
		if !ok {
			return streamSum{}, fmt.Errorf("the manifest lists %s, which did not arrive", c.Path)
		}
		if f.Size != c.Size || f.SHA256 != c.SHA256 {
			return streamSum{}, fmt.Errorf("%s arrived with %d bytes and sha256 %s; the manifest says %d and %s",
				c.Path, f.Size, f.SHA256, c.Size, c.SHA256)
		}
	}
	for name := range got {
		if !want[name] {
			return streamSum{}, fmt.Errorf("the stream carried %s, which the manifest does not list", name)
		}
	}
	if size, ok, err := bundleDiskSize(dir); err != nil {
		return streamSum{}, err
	} else if ok && size != disk.Size {
		return streamSum{}, fmt.Errorf("the disk arrived as %d bytes, but disk.json says the guest's was %d", disk.Size, size)
	}
	return disk, nil
}

// manifestComponent is one entry of a bundle's manifest.json.
type manifestComponent struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// readManifestComponents reads the component list of dir/manifest.json. The
// rest of the manifest is hvi's to read.
func readManifestComponents(dir string) ([]manifestComponent, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read the bundle's manifest: %w", err)
	}
	var m struct {
		Components []manifestComponent `json:"components"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the bundle's manifest.json: %w", err)
	}
	if len(m.Components) == 0 || len(m.Components) > migrateMaxFiles-2 {
		return nil, fmt.Errorf("the bundle's manifest lists %d components", len(m.Components))
	}
	for _, c := range m.Components {
		if !migrateStreamName.MatchString("bundle/"+c.Path) || c.Path == "manifest.json" {
			return nil, fmt.Errorf("the bundle's manifest lists a component at %q", c.Path)
		}
	}
	return m.Components, nil
}

// bundleDiskSize reads size_bytes from dir/disk.json, when the bundle has
// one (a guest without a disk has none, and a move needs a disk anyway).
func bundleDiskSize(dir string) (int64, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "disk.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var d struct {
		SizeBytes *int64 `json:"size_bytes"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.SizeBytes == nil {
		return 0, false, fmt.Errorf("the bundle's disk.json has no size_bytes: %v", err)
	}
	return *d.SizeBytes, true, nil
}

// placeReceived renames the staged bundle to root/txid and the staged disk to
// instDir/bundle/rootfs.ext4, replacing what an earlier attempt left, and
// syncs both directories.
func placeReceived(staging, root, txid, instDir string) error {
	bundleDir := filepath.Join(root, txid)
	_ = store.RemoveTree(bundleDir)
	if err := os.Rename(filepath.Join(staging, "bundle"), bundleDir); err != nil {
		return fmt.Errorf("place the bundle: %w", err)
	}
	diskDir := filepath.Join(instDir, "bundle")
	if err := os.MkdirAll(diskDir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(staging, "disk"), filepath.Join(diskDir, "rootfs.ext4")); err != nil {
		return fmt.Errorf("place the disk: %w", err)
	}
	_ = store.RemoveTree(staging)
	if err := syncDir(root); err != nil {
		return err
	}
	return syncDir(diskDir)
}

// hviCheckpointVerify runs `hvi checkpoint-verify dir` and returns its
// verdict, the last line it printed.
func hviCheckpointVerify(hvi, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, hvi, "checkpoint-verify", dir)
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	text := strings.TrimSpace(out.String())
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	if err != nil {
		return "", fmt.Errorf("%s checkpoint-verify %s: %v: %s", hvi, dir, err, text)
	}
	return text, nil
}

// destCmdLine is the hvi command line the received instance is recorded
// with: a restore from the received bundle, with the destination's own disk,
// sockets and checkpoint root. hviRestoreArgv maps it to the command `hull
// restore` runs, as it does for a record of a local run.
func destCmdLine(s *store.Store, rec *migrationRecord, hvi, root string) []string {
	spec := rec.Spec
	instDir := s.InstanceDir(rec.Name)
	argv := []string{hvi, "restore", "--from", rec.BundleDir, "--disk", rec.Disk.Path}
	switch spec.Net {
	case "gateway":
		argv = append(argv, "--net-gateway", spec.GatewaySock)
	case "stub":
		argv = append(argv, "--net-stub")
	}
	if spec.MAC != "" {
		argv = append(argv, "--net-mac", spec.MAC)
	}
	if spec.Agent {
		argv = append(argv, "--agent-sock", s.InstanceAgentSocket(rec.Name))
	}
	if spec.SandboxID != "" {
		argv = append(argv, "--sandbox-id", spec.SandboxID)
	}
	if spec.Events {
		argv = append(argv, "--events", filepath.Join(instDir, "events"))
	}
	return append(argv, "--checkpoint-root", root)
}

// writeIncomingInstance writes the received instance's state.json: stopped,
// with the move's fenced bundle as its checkpoint in the incoming state.
func writeIncomingInstance(s *store.Store, rec *migrationRecord, hvi, root string) error {
	spec := rec.Spec
	state := &store.InstanceState{
		ID:          rec.Name,
		ImageDigest: spec.ImageDigest,
		Status:      "stopped",
		LogFile:     s.InstanceLogFile(rec.Name),
		CmdLine:     destCmdLine(s, rec, hvi, root),
		BundleDir:   s.InstanceBundleDir(rec.Name),
		MAC:         spec.MAC,
		IP:          spec.IP,
		Backend:     string(hypervisors.HviVmm),
		StartTime:   time.Now(),
		Migration: &store.MigrationState{State: store.MigrationIncoming, BundleDir: rec.BundleDir,
			TxID: rec.TxID, ManifestSHA256: rec.ManifestSHA256, MoveTxID: rec.TxID, Peer: rec.Peer},
	}
	return s.SaveInstance(state)
}

// runDestRestore runs `hull restore --detach --paused --move TXID NAME` as a
// child, so the restore takes the instance's migration lock and records its
// progress exactly as a restore an operator ran would.
func runDestRestore(s *store.Store, name, txid string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, self, "--store-dir", s.RootDir(), "restore", "--detach", "--paused",
		"--move", txid, name)
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		text := strings.TrimSpace(out.String())
		if i := strings.LastIndex(text, "error: "); i >= 0 {
			text = text[i+len("error: "):]
		}
		return fmt.Errorf("hull restore %s: %s", name, text)
	}
	return nil
}

// destRestoredPaused reports whether the instance's VM is up and paused by
// the restore, as hvi says.
func destRestoredPaused(s *store.Store, name string) bool {
	state, err := s.GetInstance(name)
	if err != nil {
		return false
	}
	return hviVMState(state) == store.MigrationRestoredPaused
}

// destPrepareLocked restores the verified checkpoint paused, unless it
// already is. The caller holds the move lock.
func destPrepareLocked(s *store.Store, rec *migrationRecord, path string) *migrateReply {
	switch rec.State {
	case dstVerified:
	case dstRestoredPaused:
		if destRestoredPaused(s, rec.Name) {
			return nil
		}
		// The paused VM went away (a reboot, a kill): the checkpoint is
		// still the fenced one and nothing ran it, so restore it again.
	default:
		return refuseWith(codeNotReady, fmt.Errorf("move %s is %s here; it has to be verified to be restored",
			rec.TxID, rec.State), rec)
	}
	started := time.Now()
	if err := destRestore(s, rec.Name, rec.TxID); err != nil {
		return refuseWith(codeRestoreRefused, rec.fail(path, err), rec)
	}
	if rec.Timings == nil {
		rec.Timings = map[string]int64{}
	}
	rec.Timings["restore"] = time.Since(started).Milliseconds()
	if err := rec.advance(path, dstRestoredPaused); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	return nil
}

// destPrepare is migrate-prepare: restore the received checkpoint paused,
// before any commit. A restore that hvi refuses (an incompatible host, an
// hvi with no restore) shows here, while the source can still abort.
func destPrepare(s *store.Store, txid string) *migrateReply {
	rec, path, unlock, no := destRecord(s, txid)
	if no != nil {
		return no
	}
	if unlock != nil {
		defer unlock()
	}
	switch rec.State {
	case migAborted:
		return refuseWith(codeAborted, fmt.Errorf("move %s was aborted here", txid), rec)
	case dstRunning:
		return &migrateReply{OK: true, Record: rec, VM: "running"}
	}
	if no := destPrepareLocked(s, rec, path); no != nil {
		return no
	}
	return &migrateReply{OK: true, Record: rec, VM: store.MigrationRestoredPaused}
}

// destCommit is migrate-commit: the source has recorded fenced and
// committing, and lets the destination run the VM. It records running
// before it sends the resume, so that from the moment the guest may run, an
// abort is refused; and it takes epoch as the instance's owner epoch.
func destCommit(s *store.Store, txid string, epoch uint64) *migrateReply {
	rec, path, unlock, no := destRecord(s, txid)
	if no != nil {
		return no
	}
	if unlock != nil {
		defer unlock()
	}
	if rec.Epoch != epoch {
		return refuseWith(codeWrongEpoch, fmt.Errorf("move %s hands over epoch %d, not %d", txid, rec.Epoch, epoch), rec)
	}
	switch rec.State {
	case migAborted:
		return refuseWith(codeAborted, fmt.Errorf("move %s was aborted here, so it cannot commit", txid), rec)
	case dstRunning:
		return &migrateReply{OK: true, Record: rec, VM: "running"}
	}
	if no := destPrepareLocked(s, rec, path); no != nil {
		return no
	}
	started := time.Now()
	state, err := s.GetInstance(rec.Name)
	if err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	state.OwnerEpoch = epoch
	if err := s.SaveInstance(state); err != nil {
		return refuseWith(codeFailed, rec.fail(path, err), rec)
	}
	// The destination's commit point: from here the guest may run, and
	// nothing may abort the move.
	if err := rec.advance(path, dstRunning); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	if err := destResume(s, rec.Name); err != nil {
		// The move committed, whatever the resume did: the record says
		// running, and the source, reading that, records the commit too.
		return refuseWith(codeFailed, rec.fail(path, fmt.Errorf("committed, but the resume failed: %w; "+
			"`hull resume %s` here", err, rec.Name)), rec)
	}
	if rec.Timings == nil {
		rec.Timings = map[string]int64{}
	}
	rec.Timings["resume"] = time.Since(started).Milliseconds()
	_ = writeMigrationRecord(path, rec)
	return &migrateReply{OK: true, Record: rec, VM: "running"}
}

// destAbort is migrate-abort: refused once the move has committed here;
// otherwise it records aborted first (so a commit that arrives later is
// refused), then stops any paused VM the move restored, keeps the record as
// a tombstone and frees the name. A move this store never heard of
// answers ok with code unknown_txid: nothing here can run it.
func destAbort(s *store.Store, txid string) *migrateReply {
	rec, path, unlock, no := destRecord(s, txid)
	if no != nil {
		if no.Code == codeUnknownTxID {
			no.OK = true
		}
		return no
	}
	if unlock != nil {
		defer unlock()
	}
	switch rec.State {
	case migAborted:
		return &migrateReply{OK: true, Record: rec}
	case dstRunning:
		return refuseWith(codeRunning, fmt.Errorf("move %s committed here and %s runs here, at epoch %d",
			txid, rec.Name, rec.Epoch), rec)
	}
	if err := rec.advance(path, migAborted); err != nil {
		return refuseWith(codeFailed, err, rec)
	}
	if err := os.MkdirAll(s.MigrationTombstoneDir(), 0o700); err == nil {
		_ = writeMigrationRecord(filepath.Join(s.MigrationTombstoneDir(), txid+".json"), rec)
	}
	var cleanup error
	if _, err := s.GetInstance(rec.Name); err == nil {
		cleanup = removeInstanceIn(s, rec.Name, true)
	} else {
		cleanup = s.DeleteInstance(rec.Name)
	}
	reply := &migrateReply{OK: true, Record: rec}
	if cleanup != nil {
		reply.Error = fmt.Sprintf("aborted, but removing %s failed: %v", rec.Name, cleanup)
	}
	return reply
}

// destStatus is migrate-status: the record, and what hvi says the VM is
// doing.
func destStatus(s *store.Store, txid string) *migrateReply {
	rec, _, err := findMigrationByTxID(s, txid)
	if errors.Is(err, os.ErrNotExist) {
		return refuse(codeUnknownTxID, fmt.Errorf("no move %s is recorded here", txid))
	}
	if err != nil {
		return refuse(codeFailed, err)
	}
	reply := &migrateReply{OK: true, Record: rec}
	if state, err := s.GetInstance(rec.Name); err == nil && rec.State != migAborted {
		reply.VM = hviVMState(state)
	}
	return reply
}

// migrateRemoteVerb wraps a destination verb: stdout carries its one reply
// and nothing else, so everything the verb prints on the way (stop and
// resume print to stdout) goes to stderr instead.
func migrateRemoteVerb(name, usage string, flags []cli.Flag,
	do func(cmd *cli.Command, s *store.Store) *migrateReply) *cli.Command {
	return &cli.Command{
		Name:   name,
		Usage:  usage,
		Hidden: true,
		Flags:  flags,
		Action: func(_ context.Context, cmd *cli.Command) error {
			out := os.Stdout
			os.Stdout = os.Stderr
			defer func() { os.Stdout = out }()
			var reply *migrateReply
			if s, err := globalStore(cmd); err != nil {
				reply = refuse(codeFailed, err)
			} else {
				reply = do(cmd, s)
			}
			reply.Protocol = migrateProtocol
			return json.NewEncoder(out).Encode(reply)
		},
	}
}

// migrateRemoteCommands are the destination's verbs, hidden: `hull migrate`
// runs them over ssh, and docs/migration.md describes them.
func migrateRemoteCommands() []*cli.Command {
	txidFlag := &cli.StringFlag{Name: "txid", Required: true, Usage: "the move's txid"}
	nameFlag := &cli.StringFlag{Name: "name", Usage: "the instance's name on this host"}
	return []*cli.Command{
		migrateRemoteVerb("migrate-probe", "report whether this host can take a moved instance",
			[]cli.Flag{nameFlag, &cli.StringFlag{Name: "hvi"}, &cli.StringFlag{Name: "gateway-sock"}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				return destProbe(s, cmd.String("name"), cmd.String("hvi"), cmd.String("gateway-sock"))
			}),
		migrateRemoteVerb("migrate-receive", "reserve a name for a move, or receive its stream on stdin",
			[]cli.Flag{txidFlag, nameFlag, &cli.BoolFlag{Name: "reserve"}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				if cmd.Bool("reserve") {
					var req migrateReserveRequest
					if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
						return refuse(codeFailed, fmt.Errorf("read the reservation: %w", err))
					}
					return destReserve(s, cmd.String("txid"), cmd.String("name"), req)
				}
				return destReceive(s, cmd.String("txid"), cmd.String("name"), os.Stdin)
			}),
		migrateRemoteVerb("migrate-prepare", "restore a received move paused",
			[]cli.Flag{txidFlag},
			func(cmd *cli.Command, s *store.Store) *migrateReply { return destPrepare(s, cmd.String("txid")) }),
		migrateRemoteVerb("migrate-commit", "take over a moved instance and run it",
			[]cli.Flag{txidFlag, &cli.Uint64Flag{Name: "epoch", Required: true}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				return destCommit(s, cmd.String("txid"), cmd.Uint64("epoch"))
			}),
		migrateRemoteVerb("migrate-status", "report a move's record on this host",
			[]cli.Flag{txidFlag},
			func(cmd *cli.Command, s *store.Store) *migrateReply { return destStatus(s, cmd.String("txid")) }),
		migrateRemoteVerb("migrate-abort", "abort a move on this host unless it committed",
			[]cli.Flag{txidFlag},
			func(cmd *cli.Command, s *store.Store) *migrateReply { return destAbort(s, cmd.String("txid")) }),
	}
}

// fileSHA256 hashes the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileModNs is a file's size and modification time in nanoseconds.
func fileModNs(path string) (int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return info.Size(), info.ModTime().UnixNano(), nil
}

// fileAllocated is the bytes a file occupies on disk, holes left out.
func fileAllocated(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}
