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
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/pkg/store"
)

// baseStream writes the base stream of a fresh sparse image and returns it
// with the image's digest.
func baseStream(t *testing.T) ([]byte, string, string) {
	t.Helper()
	img := filepath.Join(t.TempDir(), "img")
	writeSparseDisk(t, img, 8<<20)
	// Something that compresses and something that does not.
	f, err := os.OpenFile(img, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte("compressible "), 20000), 1<<20); err != nil {
		t.Fatal(err)
	}
	noise := make([]byte, diskBlockSize)
	for i := range noise {
		noise[i] = byte(i*7919 + i>>3)
	}
	if _, err := f.WriteAt(noise, 5<<20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	sum, _, err := diskDigestFile(img)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	st, err := writeBaseStream(&buf, img, sum)
	if err != nil {
		t.Fatal(err)
	}
	if st.WireBytes != int64(buf.Len()) || st.Blocks == 0 || st.WireBytes >= st.DataBytes {
		t.Fatalf("stats %+v for %d bytes on the wire", st, buf.Len())
	}
	return buf.Bytes(), sum, img
}

// A base stream reads back into the same content, holes kept; a stream
// that is cut, damaged, or names another base is refused.
func TestBaseStreamRoundTrip(t *testing.T) {
	stream, sum, img := baseStream(t)
	dir := t.TempDir()
	read := func(name string, data []byte, want string) error {
		out, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = out.Close() }()
		got, err := readBaseStream(bytes.NewReader(data), out, want)
		if err == nil && got != want {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if err := read("ok", stream, sum); err != nil {
		t.Fatal(err)
	}
	assertSameContent(t, img, filepath.Join(dir, "ok"))

	cut := stream[:len(stream)-1]
	if err := read("cut", cut, sum); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("a stream without its end: %v", err)
	}
	damaged := bytes.Clone(stream)
	damaged[len(damaged)-200] ^= 0x40
	if err := read("damaged", damaged, sum); err == nil {
		t.Error("a damaged block was taken")
	}
	if err := read("other", stream, strings.Repeat("0", 64)); err == nil {
		t.Error("a stream of another base was taken")
	}
	trailing := append(bytes.Clone(stream), 'E')
	if err := read("trailing", trailing, sum); err == nil {
		t.Error("bytes after the end were taken")
	}
	// A block index past the disk is refused before anything is written.
	hdrEnd := len(baseStreamMagic) + 4 + int(binary.BigEndian.Uint32(stream[len(baseStreamMagic):]))
	bad := bytes.Clone(stream)
	binary.BigEndian.PutUint64(bad[hdrEnd+1:], 1<<40)
	if err := read("past", bad, sum); err == nil || !strings.Contains(err.Error(), "block") {
		t.Errorf("a block past the end: %v", err)
	}
}

// prepareFixture is a source store with a running hvi instance that kept a
// base and runs the portable profile, with that profile's kernel
// installed, and an empty destination store.
func prepareFixture(t *testing.T) (*moveFixture, *store.InstanceState) {
	t.Helper()
	for _, k := range []string{"HULL_BOOT_ASSETS", "BRIG_BOOT_ASSETS", bootassets.VerifyModeEnv} {
		t.Setenv(k, "")
	}
	f := newMoveFixture(t, true)
	sum, err := keepBase(f.src, f.disk)
	if err != nil {
		t.Fatal(err)
	}
	seedAssetDir(t, filepath.Join(f.src.RootDir(), "assets", bootassets.PortableArm64V1),
		map[string]string{bootassets.ProfileKernelName: "the portable kernel"})
	state := mustInstance(t, f.src, f.name)
	state.Base = &store.BaseState{SHA256: sum, FromBoot: true}
	state.Profile = bootassets.PortableArm64V1
	if err := f.src.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	return f, state
}

// migrate-prepare puts the base and the profile kernel on the destination,
// and run again finds both there and sends nothing; the probe lists them.
// An image it cannot pull is reported and does not fail the prepare.
func TestPrepareIsIdempotent(t *testing.T) {
	f, state := prepareFixture(t)
	peer := newLocalPeer(f.dst)
	remote := migrateRemote{Target: "pm@dest", Hvi: f.hvi}
	var out bytes.Buffer
	res, err := prepareDestination(context.Background(), f.src, state, peer, remote, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	results := map[string]string{}
	for _, st := range res.Steps {
		results[strings.Fields(st.What)[0]] = st.Result
	}
	if results["base"] != "sent" || results[bootassets.PortableArm64V1] != "sent" || results["image"] != "failed" {
		t.Fatalf("first prepare: %+v\n%s", res.Steps, out.String())
	}
	if !baseHeld(f.dst, state.Base.SHA256) {
		t.Fatal("the destination does not hold the base")
	}
	if got, _, err := diskDigestFile(f.dst.BasePath(state.Base.SHA256)); err != nil || got != state.Base.SHA256 {
		t.Fatalf("the destination's base hashes %s (%v)", got, err)
	}
	if !res.Holds.holdsBase(state.Base.SHA256) || res.Holds.ProfileKernels[bootassets.PortableArm64V1] == "" {
		t.Fatalf("the probe does not list what was sent: %+v", res.Holds)
	}
	if !bootassets.ProfileKernelPresent(f.dst.RootDir(), bootassets.PortableArm64V1) {
		t.Fatal("the kernel is not installed on the destination")
	}

	sent := peer.called("migrate-take-base") + peer.called("migrate-take-kernel")
	out.Reset()
	res, err = prepareDestination(context.Background(), f.src, state, peer, remote, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range res.Steps {
		if st.Result == "sent" {
			t.Errorf("the second prepare sent %s again", st.What)
		}
	}
	if again := peer.called("migrate-take-base") + peer.called("migrate-take-kernel"); again != sent {
		t.Errorf("the second prepare called the take verbs %d more times", again-sent)
	}
}

// A destination hull from before streamed moves is not prepared: the
// prepare says a move sends the whole checkpoint, and fails nothing.
func TestPrepareOfAnOldDestination(t *testing.T) {
	f, state := prepareFixture(t)
	peer := newLocalPeer(f.dst)
	peer.answer["migrate-probe"] = &migrateReply{OK: true, Probe: &migrateProbe{HullVersion: "old"}}
	var out bytes.Buffer
	res, err := prepareDestination(context.Background(), f.src, state, peer,
		migrateRemote{Target: "pm@dest"}, &out)
	if err != nil || res.Stream || len(res.Steps) != 0 || !strings.Contains(out.String(), "whole checkpoint") {
		t.Fatalf("err %v res %+v out %s", err, res, out.String())
	}
}

// `hull migrate-prepare --txid T` is still the destination's step of a
// move, answering one JSON reply, and the operator's form needs NAME and
// --to.
func TestMigratePrepareCommandDispatch(t *testing.T) {
	s := shortStore(t)
	// The macOS store marker, so globalStore takes the directory as is.
	if err := os.WriteFile(filepath.Join(s.RootDir(), ".case-sensitive-apfs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := func() *cli.Command {
		return &cli.Command{Name: "hull", Flags: []cli.Flag{&cli.StringFlag{Name: "store-dir", Value: s.RootDir()}},
			Commands: migrateRemoteCommands()}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := app().Run(context.Background(), []string{"hull", "migrate-prepare", "--txid", strings.Repeat("ab", 16)})
	os.Stdout = stdout
	_ = w.Close()
	data, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	reply, err := parseMigrateReply(data)
	if err != nil || reply.OK || reply.Code != codeUnknownTxID {
		t.Fatalf("reply %s: %+v %v", data, reply, err)
	}
	for _, args := range [][]string{{}, {"vm1"}, {"--to", "pm@h"}} {
		err := app().Run(context.Background(), append([]string{"hull", "migrate-prepare"}, args...))
		if err == nil || !strings.Contains(err.Error(), "NAME --to") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// A destination that holds another kernel of the profile keeps it: the
// prepare reports the difference and fails nothing, and the destination's
// verb refuses to replace it even when asked.
func TestPrepareKeepsAnotherKernel(t *testing.T) {
	f, state := prepareFixture(t)
	dstKernels := filepath.Join(f.dst.RootDir(), "assets", bootassets.PortableArm64V1)
	seedAssetDir(t, dstKernels, map[string]string{bootassets.ProfileKernelName: "the destination's own kernel"})
	before, err := os.ReadFile(filepath.Join(dstKernels, bootassets.ProfileKernelName))
	if err != nil {
		t.Fatal(err)
	}
	peer := newLocalPeer(f.dst)
	var out bytes.Buffer
	res, err := prepareDestination(context.Background(), f.src, state, peer,
		migrateRemote{Target: "pm@dest", Hvi: f.hvi}, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var kernel *prepareStep
	for i, st := range res.Steps {
		if strings.HasPrefix(st.What, bootassets.PortableArm64V1) {
			kernel = &res.Steps[i]
		}
	}
	if kernel == nil || kernel.Result != "skipped" || !strings.Contains(kernel.Detail, "not replaced") {
		t.Fatalf("kernel step %+v", kernel)
	}
	if peer.called("migrate-take-kernel") != 0 {
		t.Error("the prepare sent a kernel to a host that holds another")
	}
	sum := installedProfileKernel(f.src.RootDir(), bootassets.PortableArm64V1)
	kernelBytes := []byte("the portable kernel")
	if r := destTakeKernel(f.dst, bootassets.PortableArm64V1, sum, int64(len(kernelBytes)),
		bytes.NewReader(kernelBytes)); r.OK || !strings.Contains(r.Error, "not replaced") {
		t.Fatalf("take-kernel over another: %+v", r)
	}
	after, err := os.ReadFile(filepath.Join(dstKernels, bootassets.ProfileKernelName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the destination's kernel changed: %q %v", after, err)
	}
}
