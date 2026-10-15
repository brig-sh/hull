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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
)

// The fixture for move tests: two stores in one process, a source instance
// whose VMM is a process named hvi with a stub control socket that writes a
// bundle on checkpoint, a fake hvi program for the probes and
// checkpoint-verify, and a peer that runs the destination's verbs against
// the second store, which a test can make drop a reply or answer for it.

// writeFakeBundle writes a bundle with a manifest that lists its parts with
// their sizes and hashes, as hvi seals one, and a disk.json naming diskSize.
// It returns the manifest's sha256.
func writeFakeBundle(t *testing.T, dir string, diskSize int64) string {
	t.Helper()
	return writeFakeBundleWithProfile(t, dir, diskSize, "")
}

// writeFakeBundleWithProfile is writeFakeBundle for a guest of the given
// machine profile, which profile.json names unless it is native, as hvi
// writes it.
func writeFakeBundleWithProfile(t *testing.T, dir string, diskSize int64, profile string) string {
	t.Helper()
	profileJSON := `{"profile_version":1}`
	if profile != "" {
		profileJSON = `{"profile_version":1,"profile":"` + profile + `"}`
	}
	parts := map[string][]byte{
		"profile.json": []byte(profileJSON),
		"ram.bin":      bytes.Repeat([]byte("ram!"), 4096),
		"disk.json": []byte(fmt.Sprintf(`{"version":1,"path_hint":"/d","size_bytes":%d,"sha256_sparse":null,`+
			`"generation":3,"synced":true}`, diskSize)),
		"session.json": []byte(`{"version":1}`),
	}
	var comps []manifestComponent
	for _, name := range []string{"profile.json", "ram.bin", "disk.json", "session.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), parts[name], 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(parts[name])
		comps = append(comps, manifestComponent{Name: name, Path: name, Size: int64(len(parts[name])),
			SHA256: hex.EncodeToString(sum[:])})
	}
	man, _ := json.Marshal(map[string]any{"checkpoint_format": 1, "components": comps})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), man, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(man)
	return hex.EncodeToString(sum[:])
}

// moveVM is the source VM's stub hvi: it fences on a fence checkpoint and
// records what it was asked.
type moveVM struct {
	stub *stubHvi
	pid  int
	done <-chan struct{}

	mu             sync.Mutex
	fenced         bool
	stopped        bool
	failCheckpoint bool
	// profile is the machine profile the bundles it writes record.
	profile string
}

func (v *moveVM) ops() []string {
	var out []string
	for _, r := range v.stub.seen() {
		out = append(out, fmt.Sprint(r["op"]))
	}
	return out
}

// moveFixture is a source store with one running hvi instance, and a
// destination store.
type moveFixture struct {
	src, dst *store.Store
	hvi      string
	vm       *moveVM
	disk     string
	name     string
	restores int
}

const fixtureDiskSize = 64 << 20

func newMoveFixture(t *testing.T, hasRestore bool) *moveFixture {
	t.Helper()
	f := &moveFixture{src: shortStore(t), dst: shortStore(t), hvi: fakeHviProgram(t, hasRestore), name: "vm1"}
	dir, err := f.src.CreateInstance(f.name)
	if err != nil {
		t.Fatal(err)
	}
	root, err := hviCheckpointRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.disk = filepath.Join(dir, "bundle", "rootfs.ext4")
	writeSparseDisk(t, f.disk, fixtureDiskSize)

	vm := &moveVM{}
	vm.pid, vm.done = startProcessAs(t, "hvi", "/bin/sleep", "60")
	vm.stub = newStubHvi(t, func(req map[string]any) stubAnswer {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		state := "running"
		if vm.fenced {
			state = "fenced"
		}
		switch req["op"] {
		case "status":
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"` + state + `","vcpus":1,"vcpus_parked":0}`}
		case "checkpoint":
			if vm.fenced {
				return stubAnswer{reply: `{"v":1,"ok":false,"code":"fenced","error":"fenced"}`}
			}
			if vm.failCheckpoint {
				return stubAnswer{reply: `{"v":1,"ok":false,"code":"capture_failed","error":"disk sync failed"}`}
			}
			sum := writeFakeBundleWithProfile(t, req["dir"].(string), fixtureDiskSize, vm.profile)
			if req["then"] == "fence" {
				vm.fenced = true
			}
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"fenced","manifest_sha256":"` + sum +
				`","stats":{"pause_ms":1,"capture_ms":2,"ram_present_bytes":16384,"ram_total_bytes":65536}}`}
		case "resume":
			if vm.fenced {
				return stubAnswer{reply: `{"v":1,"ok":false,"code":"fenced","error":"the VM was fenced"}`}
			}
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
		case "stop":
			vm.stopped = true
			_ = syscall.Kill(vm.pid, syscall.SIGKILL)
			return stubAnswer{reply: `{"v":1,"ok":true,"state":"stopping"}`}
		}
		return stubAnswer{reply: `{"v":1,"ok":false,"code":"unknown_op","error":"?"}`}
	})
	f.vm = vm
	state := &store.InstanceState{
		ID: f.name, Status: "running", PID: vm.pid, Backend: "hvi", ControlSock: vm.stub.path,
		MAC: "52:54:00:12:34:56", IP: "10.0.2.15", ImageDigest: "sha256:abc",
		CmdLine: []string{f.hvi, "boot", "--kernel", "/k", "--mem-mib", "64", "--cpus", "1", "--disk", f.disk,
			"--net-mac", "52:54:00:12:34:56", "--agent-sock", filepath.Join(dir, "agent.sock"),
			"--sandbox-id", f.name, "--checkpoint-root", root},
	}
	if err := f.src.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	// The destination's restore: a process named hvi whose stub reports
	// restored-paused until it is resumed, recorded as `hull restore
	// --paused` records one.
	prevRestore, prevResume := destRestore, destResume
	t.Cleanup(func() { destRestore, destResume = prevRestore, prevResume })
	destRestore = func(s *store.Store, name, txid string) error {
		if !hasRestore {
			return errors.New(`the hvi it ran has no restore subcommand yet`)
		}
		f.restores++
		var mu sync.Mutex
		resumed, fenced := false, false
		stub := newStubHvi(t, func(req map[string]any) stubAnswer {
			mu.Lock()
			defer mu.Unlock()
			switch req["op"] {
			case "resume":
				if fenced {
					return stubAnswer{reply: `{"v":1,"ok":false,"code":"fenced","error":"fenced"}`}
				}
				resumed = true
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
			case "status":
				switch {
				case fenced:
					return stubAnswer{reply: `{"v":1,"ok":true,"state":"fenced"}`}
				case resumed:
					return stubAnswer{reply: `{"v":1,"ok":true,"state":"running"}`}
				}
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"restored-paused"}`}
			case "checkpoint":
				// A moved instance can move on: back, for one.
				sum := writeFakeBundle(t, req["dir"].(string), fixtureDiskSize)
				fenced = req["then"] == "fence"
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"fenced","manifest_sha256":"` + sum + `"}`}
			case "stop":
				return stubAnswer{reply: `{"v":1,"ok":true,"state":"stopping"}`}
			}
			return stubAnswer{reply: `{"v":1,"ok":false,"code":"unknown_op","error":"?"}`}
		})
		pid, _ := startProcessAs(t, "hvi", "/bin/sleep", "60")
		st, err := s.GetInstance(name)
		if err != nil {
			return err
		}
		if err := checkMoveRestore(s, st, hviRestoreOptions{move: txid, paused: true}); err != nil {
			return err
		}
		st.Status, st.PID, st.ControlSock = "running", pid, stub.path
		st.Migration.State = store.MigrationRestoredPaused
		return s.SaveInstance(st)
	}
	prevLocal := localRestore
	t.Cleanup(func() { localRestore = prevLocal })
	localRestore = func(*store.Store, string) error { return nil }
	return f
}

// options is a move to the fixture's destination.
func (f *moveFixture) options(allowNoRestore bool) migrateOptions {
	return migrateOptions{
		to:          "pm@dest",
		remote:      migrateRemote{Target: "pm@dest", Hvi: f.hvi, AllowNoRestore: allowNoRestore},
		fenceTimout: 0,
		out:         io.Discard,
	}
}

// srcRecord and dstRecord read the two sides' records of the move.
func (f *moveFixture) srcRecord(t *testing.T) *migrationRecord {
	t.Helper()
	rec, err := readMigrationRecord(f.src.InstanceMigrationRecord(f.name))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *moveFixture) dstRecord(t *testing.T, txid string) *migrationRecord {
	t.Helper()
	rec, _, err := findMigrationByTxID(f.dst, txid)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// localPeer runs the destination's verbs in this process against a store.
// drop names verbs whose reply is lost after they ran; down makes every
// call fail without running anything; answer replaces a verb's reply.
type localPeer struct {
	s      *store.Store
	mu     sync.Mutex
	drop   map[string]int
	down   bool
	answer map[string]*migrateReply
	calls  []string
	// mangle, when set, stands between a verb and its stdin, to damage or
	// cut what crosses the link.
	mangle func(verb string, args []string, in io.Reader) io.Reader
	// cutSession ends the next session's lines after its verified line.
	cutSession bool
}

func newLocalPeer(s *store.Store) *localPeer {
	return &localPeer{s: s, drop: map[string]int{}, answer: map[string]*migrateReply{}}
}

func (p *localPeer) call(_ context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error) {
	p.mu.Lock()
	p.calls = append(p.calls, verb)
	down, canned := p.down, p.answer[verb]
	dropped := p.drop[verb] > 0
	if dropped {
		p.drop[verb]--
	}
	p.mu.Unlock()
	if down {
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		return nil, fmt.Errorf("%w to %s: ssh: connect to host dest: Connection refused", errMoveNoAnswer, verb)
	}
	if canned != nil {
		canned.Protocol = migrateProtocol
		if !canned.OK {
			return canned, &migrateRefusal{Verb: verb, Reply: canned}
		}
		return canned, nil
	}
	if p.mangle != nil && stdin != nil {
		stdin = p.mangle(verb, args, stdin)
	}
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		if i+1 < len(args) && args[i] != "--reserve" && args[i] != "--stream" && args[i] != "--session" {
			flags[args[i]] = args[i+1]
			i++
		} else {
			flags[args[i]] = "true"
		}
	}
	var reply *migrateReply
	switch verb {
	case "migrate-probe":
		reply = destProbe(p.s, flags["--name"], flags["--hvi"], flags["--gateway-sock"])
	case "migrate-receive":
		if flags["--stream"] == "true" {
			reply = destReceiveStream(p.s, flags["--txid"], flags["--name"], stdin, nil)
			// As ssh: the call ends with the remote command, and what the
			// source still writes goes nowhere.
			go func() { _, _ = io.Copy(io.Discard, stdin) }()
		} else if flags["--reserve"] == "true" {
			var req migrateReserveRequest
			if err := json.NewDecoder(stdin).Decode(&req); err != nil {
				return nil, err
			}
			reply = destReserve(p.s, flags["--txid"], flags["--name"], req)
		} else {
			reply = destReceive(p.s, flags["--txid"], flags["--name"], stdin)
			_, _ = io.Copy(io.Discard, stdin)
		}
	case "migrate-prepare":
		reply = destPrepare(p.s, flags["--txid"])
	case "migrate-take-base":
		reply = destTakeBase(p.s, flags["--sha256"], stdin)
		_, _ = io.Copy(io.Discard, stdin)
	case "migrate-spare-base":
		reply = destSpareBase(p.s, flags["--sha256"])
	case "migrate-take-kernel":
		size, _ := strconv.ParseInt(flags["--size"], 10, 64)
		reply = destTakeKernel(p.s, flags["--profile"], flags["--sha256"], size, stdin)
	case "migrate-take-image":
		reply = destTakeImage(p.s, flags["--ref"], flags["--digest"])
	case "migrate-commit":
		epoch, _ := strconv.ParseUint(flags["--epoch"], 10, 64)
		reply = destCommit(p.s, flags["--txid"], epoch)
	case "migrate-status":
		reply = destStatus(p.s, flags["--txid"])
	case "migrate-abort":
		reply = destAbort(p.s, flags["--txid"])
	default:
		return nil, fmt.Errorf("no verb %s", verb)
	}
	reply.Protocol = migrateProtocol
	// Through JSON, as over ssh.
	data, _ := json.Marshal(reply)
	reply, err := parseMigrateReply(data)
	if err != nil {
		return nil, err
	}
	if dropped {
		return nil, fmt.Errorf("%w to %s: connection reset after the verb ran", errMoveNoAnswer, verb)
	}
	if !reply.OK {
		return reply, &migrateRefusal{Verb: verb, Reply: reply}
	}
	return reply, nil
}

// session is the streamed receive in a session, its lines through a pipe
// as JSON, as over ssh. noSession makes the peer one that has none;
// cutSession ends the next session's lines after its verified line, as a
// link lost there would, with the destination running on.
func (p *localPeer) session(ctx context.Context, verb string, args []string, stdin io.Reader,
	event func(*migrateReply)) (*migrateReply, error) {
	p.mu.Lock()
	p.calls = append(p.calls, verb+" --session")
	down, cut := p.down, p.cutSession
	p.cutSession = false
	p.mu.Unlock()
	if verb != "migrate-receive" || down {
		return p.call(ctx, verb, args, stdin)
	}
	if p.mangle != nil {
		stdin = p.mangle(verb, args, stdin)
	}
	flags := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i+1], "--") {
			continue
		}
		flags[args[i]] = args[i+1]
		i++
	}
	lr, lw := io.Pipe()
	out := &sessionOut{w: lw}
	if cut {
		out.w = &cutAfterVerified{w: lw}
	}
	go func() {
		reply := destReceiveStream(p.s, flags["--txid"], flags["--name"], stdin, out)
		out.send(reply)
		_ = lw.Close()
		go func() { _, _ = io.Copy(io.Discard, stdin) }()
	}()
	reply, err := readSession(lr, event)
	_, _ = io.Copy(io.Discard, lr)
	if err != nil {
		return nil, fmt.Errorf("%w to %s: %v", errMoveNoAnswer, verb, err)
	}
	if !reply.OK {
		return reply, &migrateRefusal{Verb: verb, Reply: reply}
	}
	return reply, nil
}

// cutAfterVerified passes a session's lines until its verified line, and
// none after it.
type cutAfterVerified struct {
	w   io.Writer
	cut bool
}

func (c *cutAfterVerified) Write(b []byte) (int, error) {
	if c.cut {
		return len(b), nil
	}
	if bytes.Contains(b, []byte(`"event":"verified"`)) {
		c.cut = true
		if _, err := c.w.Write(b); err != nil {
			return 0, err
		}
		if pw, ok := c.w.(*io.PipeWriter); ok {
			_ = pw.Close()
		}
		return len(b), nil
	}
	return c.w.Write(b)
}

func (p *localPeer) called(verb string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == verb {
			n++
		}
	}
	return n
}

// runHullEnv makes a re-exec'd test binary run hull's main, and fakeSSHEnv
// makes it stand in for ssh (see TestMoveOverFakeSSH).
const (
	runHullEnv = "HULL_TEST_RUN_HULL"
	fakeSSHEnv = "HULL_TEST_FAKE_SSH"
)

// testBinaryAsHull runs this test binary as ssh or as hull when the
// environment asks for it, and returns otherwise. Each TestMain calls it
// first.
func testBinaryAsHull() {
	if os.Getenv(fakeSSHEnv) != "" {
		// As ssh: the remote command, the last argument, runs through
		// sh as a remote login shell would run it, and the hull it names
		// is this binary again, as hull.
		_ = os.Unsetenv(fakeSSHEnv)
		_ = os.Setenv(runHullEnv, "1")
		sh := exec.Command("/bin/sh", "-c", os.Args[len(os.Args)-1])
		sh.Stdin, sh.Stdout, sh.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := sh.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			os.Exit(255)
		}
		os.Exit(0)
	}
	if os.Getenv(runHullEnv) != "" {
		// As hull: the destination's verbs, and their `hull restore`.
		main()
		os.Exit(0)
	}
}
