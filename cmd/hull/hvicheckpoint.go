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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/pkg/store"
)

// hviCheckpointRecordName is the file in <instance>/checkpoint/ that names
// the last hvi checkpoint. vz-runner writes a file of the same name with its
// own fields into the same directory of a Vz instance; an instance is one or
// the other, and every reader dispatches on the backend before reading it.
const hviCheckpointRecordName = "latest.json"

// Checkpoint follow-ups, as hvi spells them.
const (
	hviThenContinue = "continue"
	hviThenFence    = "fence"
)

// hviCheckpoint asks hvi to write a checkpoint bundle into dir, an absolute
// path, and then either resume the guest or fence it. txid goes into the
// bundle's session record, so the bundle names the checkpoint hull recorded.
// epoch, when not 0, is the owner epoch a move hands over with this bundle;
// hvi records it beside the txid.
func hviCheckpoint(sock, dir, then, txid string, epoch uint64, timeout time.Duration) (*hviReply, error) {
	req := map[string]any{"op": "checkpoint", "dir": dir, "then": then, "txid": txid}
	if epoch != 0 {
		req["epoch"] = epoch
	}
	return hviControl(sock, req, timeout)
}

// hviCheckpointable refuses an hvi command line whose guest a checkpoint
// cannot carry, naming the reason.
//
// A checkpoint is the guest's RAM and device state plus the disk it was using.
// Everything the guest depends on has to be in those two, so:
//   - the root must be a disk (--disk). A virtio-fs root lives in a host
//     directory and in the guest's FUSE caches, which no bundle captures.
//   - no virtio-fs share (--share-ro, --share-rw). hvi refuses a profile with
//     shares anyway; refusing here says so before the VM is paused.
func hviCheckpointable(cmdLine []string) error {
	if len(cmdLine) == 0 || filepath.Base(cmdLine[0]) != "hvi" {
		return errors.New("checkpoint of this kind needs the hvi backend")
	}
	if !slices.Contains(cmdLine, "--disk") {
		return errors.New("checkpoint needs a block rootfs, which a checkpoint carries as a disk: " +
			"start the instance with `run --rootfs-type block` (a virtiofs root lives in a host " +
			"directory no checkpoint captures)")
	}
	for _, arg := range cmdLine {
		if arg == "--share-ro" || arg == "--share-rw" {
			return errors.New("checkpoint refuses an instance with a shared host directory " +
				"(--shared-dir): the guest's view of it lives in host files and FUSE caches no " +
				"checkpoint captures. Run it with no shares")
		}
	}
	return nil
}

// checkpointCommand is one verb over two backends. A Vz instance is saved by
// vz-runner on a signal (vzCheckpointInstance, macOS only); an hvi instance
// over its control socket. The record says which, so the user never has to.
func checkpointCommand() *cli.Command {
	return &cli.Command{
		Name: "checkpoint",
		Usage: "checkpoint a running instance: Vz saves VM and disk state and resumes; hvi " +
			"writes a checkpoint bundle and continues or fences",
		ArgsUsage: "NAME",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "timeout",
				Value: 60,
				Usage: "seconds to wait for the checkpoint to complete",
			},
			&cli.StringFlag{
				Name:  "then",
				Value: hviThenContinue,
				Usage: "hvi: what the guest does after the checkpoint, 'continue' or 'fence' " +
					"(never runs again in this process; for a move or a restore)",
			},
			&cli.StringFlag{
				Name:  "dir",
				Usage: "hvi: empty directory below <instance>/checkpoint to write the bundle to (default: a new one there)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				return errors.New("instance ID required")
			}
			s, err := globalStore(cmd)
			if err != nil {
				return err
			}
			id := cmd.Args().First()
			state, err := s.GetInstance(id)
			if err != nil {
				return fmt.Errorf("instance not found: %s", id)
			}
			if !isHviInstance(state) {
				if cmd.IsSet("then") || cmd.IsSet("dir") {
					return errors.New("--then and --dir apply to hvi instances only")
				}
				return vzCheckpointInstance(ctx, cmd)
			}
			return hviCheckpointInstance(s, state, cmd.String("then"), cmd.String("dir"),
				time.Duration(cmd.Int("timeout"))*time.Second)
		},
	}
}

// hviCheckpointStats is the stats object hvi's checkpoint reply carries.
// Pointers, so a field this hvi does not send is told apart from a zero.
type hviCheckpointStats struct {
	PauseMs         *int64 `json:"pause_ms"`
	CaptureMs       *int64 `json:"capture_ms"`
	RAMPresentBytes *int64 `json:"ram_present_bytes"`
	RAMTotalBytes   *int64 `json:"ram_total_bytes"`
}

// hviCheckpointRecord is <instance>/checkpoint/latest.json for an hvi
// instance: which bundle is the latest, how it was taken, and what hvi
// reported. The bundle's own manifest is the authority on its contents;
// manifestSha256 ties this record to that manifest.
type hviCheckpointRecord struct {
	Dir            string          `json:"dir"`
	ManifestSHA256 string          `json:"manifestSha256"`
	Then           string          `json:"then"`
	Stats          json.RawMessage `json:"stats,omitempty"`
	SavedAt        time.Time       `json:"savedAt"`
	TxID           string          `json:"txid"`
}

// writeHviCheckpointRecord publishes rec as ckptDir/latest.json.
//
// Through a temp file, fsync and rename, so a reader sees the previous record
// or this one and never half of either: a restore that read a torn record
// would pick the wrong bundle or none, and the crash it would follow is the
// moment a checkpoint matters most.
func writeHviCheckpointRecord(ckptDir string, rec hviCheckpointRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ckptDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", ckptDir, err)
	}
	tmp, err := os.CreateTemp(ckptDir, hviCheckpointRecordName+".tmp-*")
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
	if err := os.Rename(tmpName, filepath.Join(ckptDir, hviCheckpointRecordName)); err != nil {
		return err
	}
	if d, err := os.Open(ckptDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readHviCheckpointRecord reads ckptDir/latest.json.
func readHviCheckpointRecord(ckptDir string) (hviCheckpointRecord, error) {
	var rec hviCheckpointRecord
	data, err := os.ReadFile(filepath.Join(ckptDir, hviCheckpointRecordName))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("%s in %s does not read as a checkpoint record: %w",
			hviCheckpointRecordName, ckptDir, err)
	}
	return rec, nil
}

// newTxID names a checkpoint. hull draws it and hands it to hvi, which
// records it in the bundle: it names the default bundle directory and, in a
// later move, keys each step so a retried step finds its own earlier work.
func newTxID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// instanceCheckpointDir is the instance's checkpoint directory: vz-runner's
// state dir for a Vz instance; latest.json and the bundles hull places by
// default for an hvi one.
func instanceCheckpointDir(s *store.Store, id string) string {
	return filepath.Join(s.InstanceDir(id), "checkpoint")
}

func hviCheckpointInstance(s *store.Store, state *store.InstanceState, then, dir string, timeout time.Duration) error {
	id := state.ID
	if then != hviThenContinue && then != hviThenFence {
		return fmt.Errorf("--then must be %q or %q (got %q)", hviThenContinue, hviThenFence, then)
	}
	if err := requireRunningHvi(state, "checkpoint"); err != nil {
		return err
	}
	if err := hviCheckpointable(state.CmdLine); err != nil {
		return err
	}
	if err := refuseDuringMove(s, id, "checkpoint"); err != nil {
		return err
	}
	unlock, err := lockInstanceMigration(s, id)
	if err != nil {
		return err
	}
	defer unlock()

	txid, err := newTxID()
	if err != nil {
		return err
	}
	started := time.Now()
	rec, err := hviCheckpointLocked(s, state, then, dir, txid, 0, timeout)
	if err != nil {
		return err
	}
	fmt.Printf("Checkpoint of %s written to %s in %s (%s)\n", id, rec.Dir,
		time.Since(started).Round(time.Millisecond), describeCheckpointStats(then, rec.Stats))
	return nil
}

// hviCheckpointLocked takes checkpoint txid of a running hvi instance into
// dir (a new directory below the checkpoint root when ""), records it in
// latest.json and the instance record, and returns what it recorded. The
// caller holds the instance's migration lock and has checked that the
// instance can be checkpointed; a move calls it with its own txid and the
// epoch it hands over.
func hviCheckpointLocked(s *store.Store, state *store.InstanceState, then, dir, txid string,
	epoch uint64, timeout time.Duration) (*hviCheckpointRecord, error) {
	id := state.ID
	if timeout < hviControlTimeout {
		timeout = hviControlTimeout
	}
	ckptDir := instanceCheckpointDir(s, id)
	root, ok := flagValue(state.CmdLine, "--checkpoint-root")
	if !ok {
		return nil, fmt.Errorf("instance %s was started without a checkpoint root, so its hvi may not "+
			"write a bundle anywhere; run it again with an hvi that takes --checkpoint-root", id)
	}
	var err error
	if dir == "" {
		dir = filepath.Join(root, txid)
	} else if dir, err = bundleDirBelow(root, dir); err != nil {
		return nil, err
	}
	created, err := prepareBundleDir(dir)
	if err != nil {
		return nil, err
	}

	// A restored guest that was never resumed may be resumed by this
	// checkpoint: by "continue" after the capture, and by hvi after a
	// failed capture of either kind unless the fence was already applied.
	// A reply that never comes says nothing either way. On success the
	// record moves on to the new bundle, so this costs nothing then.
	if err := markRestoredGuestRunning(s, id); err != nil {
		return nil, err
	}
	reply, err := hviCheckpoint(state.ControlSock, dir, then, txid, epoch, timeout)
	if err != nil {
		// hvi resumes the guest after a failed capture (unless the fence was
		// already applied), and what it wrote is not a bundle: the manifest
		// is written last and is missing. A directory hull made for it goes;
		// a --dir that was there before is emptied and kept, so a failed
		// attempt leaves no RAM-sized debris and the same --dir can be
		// retried (prepareBundleDir refuses one that is not empty). With no
		// reply hvi may still be writing, and everything stays.
		if !errors.Is(err, errHviNoReply) {
			if created {
				_ = store.RemoveTree(dir)
			} else {
				emptyDir(dir)
			}
		}
		switch {
		case errors.Is(err, errHviNoReply):
			return nil, fmt.Errorf("%w; hvi may still be writing the bundle to %s with the guest "+
				"paused. Once it is done, `hull resume %s` either resumes the guest or answers "+
				"that it was fenced; retry with a longer --timeout", err, dir, id)
		case hviRefusalCode(err) == "not_implemented":
			return nil, fmt.Errorf("this hvi (%s) cannot checkpoint yet: %w", state.CmdLine[0], err)
		}
		return nil, err
	}

	rec := hviCheckpointRecord{
		Dir:            dir,
		ManifestSHA256: reply.ManifestSHA256,
		Then:           then,
		Stats:          reply.Stats,
		SavedAt:        time.Now().UTC(),
		TxID:           txid,
	}
	if err := writeHviCheckpointRecord(ckptDir, rec); err != nil {
		return nil, fmt.Errorf("the bundle is in %s but recording it failed: %w", dir, err)
	}

	fresh, err := s.GetInstance(id)
	if err != nil {
		return nil, err
	}
	previous := fresh.Migration
	migrationState := store.MigrationCheckpointed
	if then == hviThenFence {
		migrationState = store.MigrationFenced
	}
	fresh.Migration = &store.MigrationState{State: migrationState, BundleDir: dir, TxID: txid,
		ManifestSHA256: reply.ManifestSHA256}
	if err := s.SaveInstance(fresh); err != nil {
		return nil, fmt.Errorf("the bundle is in %s but the instance record was not updated: %w", dir, err)
	}
	// One bundle per instance of hull's own naming: each is the size of the
	// guest's touched RAM, and only the latest is ever restored. A bundle
	// placed with --dir was named by whoever chose it and is never removed
	// here.
	if previous != nil && previous.BundleDir != dir && previous.BundleDir == filepath.Join(root, previous.TxID) {
		_ = store.RemoveTree(previous.BundleDir)
	}
	return &rec, nil
}

// hviCheckpointRoot creates <instanceDir>/checkpoint, 0700, and returns it
// with links resolved, for --checkpoint-root.
//
// hvi is confined before the guest runs, and its confinement grants file
// writes below this one directory and nowhere else, so without it every
// checkpoint is refused. It must exist at boot. It is passed resolved, and
// every bundle path hull sends is built from or checked against this same
// resolved spelling, so the two always agree as strings: on macOS /tmp is a
// link to /private/tmp, and a check that mixed spellings would put a
// directory below the root outside it.
func hviCheckpointRoot(instanceDir string) (string, error) {
	root := filepath.Join(instanceDir, "checkpoint")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	return filepath.EvalSymlinks(root)
}

// bundleDirBelow resolves a --dir and keeps it below root, the checkpoint
// root the instance's hvi was started with.
//
// hvi's confinement lets it write there and nowhere else, and it refuses a
// bundle directory outside it; saying so here names the root instead of
// leaving the user an EPERM. The root on the recorded command line is
// resolved (hviCheckpointRoot), so the directory is resolved too before the
// prefix check: on macOS /tmp is a link to /private/tmp, and a --dir under
// /tmp/... would otherwise never be "below" /private/tmp/.... hvi is sent
// the same resolved path, which matches the root it was given.
func bundleDirBelow(root, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("--dir %s: %w", dir, err)
	}
	resolved := filepath.Join(parent, filepath.Base(abs))
	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("--dir %s is not below %s, the one directory this instance's hvi "+
			"may write a bundle to", dir, root)
	}
	return resolved, nil
}

// prepareBundleDir makes dir ready for hvi to write a bundle into: created
// 0700 if missing, or an existing empty directory. It reports whether it made
// the directory. A directory with files in it is refused rather than mixed:
// a bundle's manifest lists its components by name, and a stale file beside
// them would sit in a directory that no longer describes one checkpoint.
func prepareBundleDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("create %s: %w", dir, err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("%s: %w", dir, err)
	case len(entries) > 0:
		return false, fmt.Errorf("%s is not empty; a checkpoint bundle needs a directory of its own", dir)
	}
	return false, nil
}

// emptyDir removes everything inside dir and keeps dir itself.
func emptyDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = store.RemoveTree(filepath.Join(dir, e.Name()))
	}
}

// lockInstanceMigration takes <instance>/migration.lock for one checkpoint
// or restore of instance id, refusing at once if another hull holds it.
//
// Each of them reads the record's migration, talks to hvi and writes the
// record back. Two at once interleave those steps: a checkpoint deletes the
// bundle it read as previous, which the other one may just have recorded in
// latest.json, and a restore could start from a bundle a checkpoint is about
// to replace. The lock sits in the instance directory, not in the checkpoint
// root hvi may write to.
func lockInstanceMigration(s *store.Store, id string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.InstanceDir(id), "migration.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the migration lock of %s: %w", id, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another checkpoint or restore of %s is in progress", id)
		}
		return nil, fmt.Errorf("lock the migration of %s: %w", id, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// describeCheckpointStats renders hvi's stats for a person, leaving out what
// this hvi did not report.
func describeCheckpointStats(then string, raw json.RawMessage) string {
	parts := []string{"then " + then}
	var st hviCheckpointStats
	if len(raw) > 0 && json.Unmarshal(raw, &st) == nil {
		if st.PauseMs != nil {
			parts = append(parts, fmt.Sprintf("paused %d ms", *st.PauseMs))
		}
		if st.CaptureMs != nil {
			parts = append(parts, fmt.Sprintf("capture %d ms", *st.CaptureMs))
		}
		if st.RAMPresentBytes != nil && st.RAMTotalBytes != nil {
			parts = append(parts, fmt.Sprintf("RAM %.1f of %.1f MiB stored",
				float64(*st.RAMPresentBytes)/(1<<20), float64(*st.RAMTotalBytes)/(1<<20)))
		}
	}
	return strings.Join(parts, ", ")
}
