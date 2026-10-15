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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/bootassets"
	"github.com/brig-sh/hull/pkg/store"
)

// `hull migrate-prepare NAME --to USER@HOST` puts on the destination, ahead
// of a move and while the guest runs, what a streamed move then does not
// have to carry: the instance's base image (by its disk digest), the
// portable kernel of its machine profile, and its OCI image. Each step
// finds its effect already there and skips, so it can be run any number of
// times; the destination's probe lists what it holds.

// moveCapStream is the capability a destination's probe lists when it takes
// a streamed move (`migrate-receive --stream`) and the prepare verbs. A hull
// from before them lists nothing and gets the whole-bundle move. v2 is the
// stream whose header names an attempt the destination accepts in its
// record before hvi's migrate starts, and whose trailer accounts for the
// disk rounds; a v1 hull refuses that trailer, so it gets the whole bundle.
const moveCapStream = "stream-v2"

// probeMaxBases bounds the bases a probe lists.
const probeMaxBases = 512

// destHolds is what the probe lists of the bases and profile kernels the
// destination store holds.
func destHolds(s *store.Store, p *migrateProbe) {
	p.Capabilities = []string{moveCapStream}
	if entries, err := os.ReadDir(s.BasesDir()); err == nil {
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ".ext4")
			if ok && e.Type().IsRegular() && checkDiskDigest(name) == nil && len(p.Bases) < probeMaxBases {
				p.Bases = append(p.Bases, name)
			}
		}
	}
	for _, profile := range []string{bootassets.PortableArm64V1} {
		if sum := installedProfileKernel(s.RootDir(), profile); sum != "" {
			if p.ProfileKernels == nil {
				p.ProfileKernels = map[string]string{}
			}
			p.ProfileKernels[profile] = sum
		}
	}
}

// installedProfileKernel is the sha256 a store's record gives its kernel of
// profile, once the kernel on disk matches it; "" when there is none.
func installedProfileKernel(storeDir, profile string) string {
	rec, err := bootassets.ProfileRecord(storeDir, profile)
	if err != nil {
		return ""
	}
	f, ok := rec.Files[bootassets.ProfileKernelName]
	if !ok {
		return ""
	}
	kernel, _, err := bootassets.ProfilePaths(storeDir, profile)
	if err != nil || bootassets.VerifyProfileKernel(storeDir, profile, kernel, "the installed kernel") != nil {
		return ""
	}
	return f.SHA256
}

// hasCapability reports whether a probe lists capability c.
func (p *migrateProbe) hasCapability(c string) bool {
	return p != nil && slices.Contains(p.Capabilities, c)
}

// holdsBase reports whether a probe lists the base of digest sum.
func (p *migrateProbe) holdsBase(sum string) bool {
	return p != nil && sum != "" && slices.Contains(p.Bases, sum)
}

// The base stream is what `migrate-take-base` reads on stdin:
//
//	"HULLBAS1"
//	u32 len, JSON {size, sha256, blockSize}
//	'B' u64 index, u8 codec, u32 n, n bytes     (one non-zero block)
//	'E'
//
// Blocks come in index order, each lz4-compressed (codec 1) unless that
// would not make it smaller (codec 0, raw). The receiver hashes what it
// writes with the disk digest and keeps the file only if the digest is the
// one the header and the request name.
const baseStreamMagic = "HULLBAS1"

const (
	baseCodecRaw = 0
	baseCodecLZ4 = 1
)

// baseStreamHeader is the base stream's header.
type baseStreamHeader struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	BlockSize int64  `json:"blockSize"`
}

// baseSendStats is what sending a base carried.
type baseSendStats struct {
	Blocks    int64
	DataBytes int64
	WireBytes int64
}

// writeBaseStream writes the base at path, whose disk digest is sum, to w.
func writeBaseStream(w io.Writer, path, sum string) (baseSendStats, error) {
	var st baseSendStats
	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return st, err
	}
	cw := &countingWriter{w: w}
	bw := bufio.NewWriterSize(cw, 1<<20)
	hdr, err := json.Marshal(baseStreamHeader{Size: info.Size(), SHA256: sum, BlockSize: diskBlockSize})
	if err != nil {
		return st, err
	}
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(hdr)))
	_, _ = bw.WriteString(baseStreamMagic)
	_, _ = bw.Write(n4[:])
	_, _ = bw.Write(hdr)
	var c lz4.Compressor
	comp := make([]byte, lz4.CompressBlockBound(diskBlockSize))
	err = forEachDataBlock(f, info.Size(), func(index int64, data []byte) error {
		codec, payload := byte(baseCodecRaw), data
		if n, err := c.CompressBlock(data, comp); err == nil && n > 0 && n < len(data) {
			codec, payload = baseCodecLZ4, comp[:n]
		}
		var rec [14]byte
		rec[0] = 'B'
		binary.BigEndian.PutUint64(rec[1:9], uint64(index))
		rec[9] = codec
		binary.BigEndian.PutUint32(rec[10:], uint32(len(payload)))
		if _, err := bw.Write(rec[:]); err != nil {
			return err
		}
		if _, err := bw.Write(payload); err != nil {
			return err
		}
		st.Blocks++
		st.DataBytes += int64(len(data))
		return nil
	})
	if err != nil {
		return st, err
	}
	if err := bw.WriteByte('E'); err != nil {
		return st, err
	}
	err = bw.Flush()
	st.WireBytes = cw.n
	return st, err
}

// readBaseStream reads a base stream from r into out, a new empty file, and
// returns the disk digest of what it wrote. Every length and index is held
// to the header before anything is allocated or written, and a stream that
// ends without its end record is refused.
func readBaseStream(r io.Reader, out *os.File, want string) (string, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	magic := make([]byte, len(baseStreamMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != baseStreamMagic {
		return "", fmt.Errorf("the base stream does not start with %s", baseStreamMagic)
	}
	var n4 [4]byte
	if _, err := io.ReadFull(br, n4[:]); err != nil {
		return "", fmt.Errorf("the base stream ended in its header: %w", err)
	}
	n := binary.BigEndian.Uint32(n4[:])
	if n > migrateMaxHeader {
		return "", fmt.Errorf("a base stream header of %d bytes is over the limit", n)
	}
	js := make([]byte, n)
	if _, err := io.ReadFull(br, js); err != nil {
		return "", fmt.Errorf("the base stream ended in its header: %w", err)
	}
	var hdr baseStreamHeader
	if err := strictUnmarshal(js, &hdr); err != nil {
		return "", fmt.Errorf("the base stream's header: %w", err)
	}
	switch {
	case hdr.SHA256 != want:
		return "", fmt.Errorf("the base stream carries base %s, not %s", hdr.SHA256, want)
	case hdr.BlockSize != diskBlockSize:
		return "", fmt.Errorf("the base stream's blocks are %d bytes, not %d", hdr.BlockSize, diskBlockSize)
	case hdr.Size <= 0 || hdr.Size > migrateMaxDiskSize:
		return "", fmt.Errorf("the base stream says the disk is %d bytes", hdr.Size)
	}
	if err := out.Truncate(hdr.Size); err != nil {
		return "", err
	}
	d := newDiskDigest(hdr.Size)
	comp := make([]byte, lz4.CompressBlockBound(diskBlockSize))
	block := make([]byte, diskBlockSize)
	blocks := diskBlocks(hdr.Size)
	for {
		kind, err := br.ReadByte()
		if err != nil {
			return "", fmt.Errorf("the base stream ended before its end: %w", err)
		}
		if kind == 'E' {
			break
		}
		if kind != 'B' {
			return "", fmt.Errorf("the base stream has a record of unknown kind %q", kind)
		}
		var rec [13]byte
		if _, err := io.ReadFull(br, rec[:]); err != nil {
			return "", fmt.Errorf("the base stream ended in a block: %w", err)
		}
		index := int64(binary.BigEndian.Uint64(rec[:8]))
		codec := rec[8]
		size := int64(binary.BigEndian.Uint32(rec[9:]))
		if index < 0 || index >= blocks {
			return "", fmt.Errorf("the base stream sends block %d of a disk of %d", index, blocks)
		}
		want := blockLen(hdr.Size, index)
		var data []byte
		switch codec {
		case baseCodecRaw:
			if size != want {
				return "", fmt.Errorf("block %d is %d bytes raw, not %d", index, size, want)
			}
			data = block[:want]
			if _, err := io.ReadFull(br, data); err != nil {
				return "", fmt.Errorf("the base stream ended in block %d: %w", index, err)
			}
		case baseCodecLZ4:
			if size <= 0 || size > int64(len(comp)) {
				return "", fmt.Errorf("block %d is %d bytes compressed", index, size)
			}
			if _, err := io.ReadFull(br, comp[:size]); err != nil {
				return "", fmt.Errorf("the base stream ended in block %d: %w", index, err)
			}
			got, err := lz4.UncompressBlock(comp[:size], block)
			if err != nil || int64(got) != want {
				return "", fmt.Errorf("block %d does not decompress to its %d bytes (%d, %v)", index, want, got, err)
			}
			data = block[:want]
		default:
			return "", fmt.Errorf("block %d has codec %d", index, codec)
		}
		// The digest refuses a block out of order before it is written.
		if err := d.add(index, data); err != nil {
			return "", err
		}
		if _, err := out.WriteAt(data, index*diskBlockSize); err != nil {
			return "", err
		}
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		return "", errors.New("the base stream goes on after its end")
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	return d.sum(), nil
}

// destTakeBase is migrate-take-base: it keeps the base the stream on in
// carries, if its digest is sum. A base already held is answered at once.
func destTakeBase(s *store.Store, sum string, in io.Reader) *migrateReply {
	if err := checkDiskDigest(sum); err != nil {
		return refuse(codeFailed, err)
	}
	reply := func() *migrateReply {
		p := &migrateProbe{}
		destHolds(s, p)
		return &migrateReply{OK: true, Probe: p}
	}
	if baseHeld(s, sum) {
		return reply()
	}
	if err := os.MkdirAll(s.BasesDir(), 0o700); err != nil {
		return refuse(codeFailed, err)
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(s.BasesDir(), ".in-"+hex.EncodeToString(rnd[:]))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return refuse(codeFailed, err)
	}
	got, err := readBaseStream(in, out, sum)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && got != sum {
		err = fmt.Errorf("the base arrived with digest %s, not %s", got, sum)
	}
	if err == nil {
		err = sealBase(s, tmp, sum)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return refuse(codeVerifyFailed, fmt.Errorf("take base %s: %w", sum, err))
	}
	// Kept until a prune with --bases: the move it was sent ahead of
	// needs it, and no instance here names it yet.
	if err := markBaseReceived(s, sum); err != nil {
		return refuse(codeFailed, err)
	}
	return reply()
}

// receivedKernelDir is where a destination keeps the profile kernels a
// prepare sent it, by sha256, so their install record names a file that
// stays.
func receivedKernelDir(s *store.Store) string {
	return filepath.Join(s.BasesDir(), "kernels")
}

// destTakeKernel is migrate-take-kernel: it installs the kernel on in as the
// kernel of profile, if it has sha256 sum and size bytes, and only when the
// store has none. A store that holds another kernel of the profile keeps
// it: a profile run boots only the kernel installed, and whoever installed
// that one, brig's BRIG_PORTABLE_KERNEL among them, expects those bytes. A
// move does not need the kernel at all.
func destTakeKernel(s *store.Store, profile, sum string, size int64, in io.Reader) *migrateReply {
	if _, err := bootassets.NormalizeProfile(profile); err != nil || profile == "" {
		return refuse(codeFailed, fmt.Errorf("profile %q: %v", profile, err))
	}
	if !diskDigestPattern.MatchString(sum) {
		return refuse(codeFailed, fmt.Errorf("%q is not a sha256", sum))
	}
	if size <= 0 || size > 256<<20 {
		return refuse(codeFailed, fmt.Errorf("a kernel of %d bytes", size))
	}
	reply := func() *migrateReply {
		p := &migrateProbe{}
		destHolds(s, p)
		return &migrateReply{OK: true, Probe: p}
	}
	if installed := installedProfileKernel(s.RootDir(), profile); installed == sum {
		return reply()
	} else if bootassets.ProfileKernelPresent(s.RootDir(), profile) {
		held := "one that does not match its install record"
		if installed != "" {
			held = "sha256 " + installed
		}
		return refuse(codeFailed, fmt.Errorf("this store holds another %s kernel (%s); it was not replaced. "+
			"`hull assets pull --profile %s --force` there installs another", profile, held, profile))
	}
	dir := receivedKernelDir(s)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return refuse(codeFailed, err)
	}
	path := filepath.Join(dir, sum+".Image")
	tmp := path + ".part"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return refuse(codeFailed, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, size+1))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != size || hex.EncodeToString(h.Sum(nil)) != sum) {
		err = fmt.Errorf("the kernel arrived with %d bytes and sha256 %x, not %d and %s", n, h.Sum(nil), size, sum)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = bootassets.ImportProfileKernel(s.RootDir(), profile, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return refuse(codeFailed, fmt.Errorf("install the %s kernel: %w", profile, err))
	}
	return reply()
}

// destTakeImage is migrate-take-image: the image of digest is pulled by ref
// unless the store has it. The move itself does not need it (the disk holds
// the root filesystem); a later `hull run` of the same image there does.
func destTakeImage(s *store.Store, ref, digest string) *migrateReply {
	if digest == "" || strings.ContainsAny(digest, "/\\") || strings.Contains(digest, "..") {
		return refuse(codeFailed, fmt.Errorf("%q is not an image digest", digest))
	}
	if s.ImageComplete(digest) {
		return &migrateReply{OK: true}
	}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return refuse(codeFailed, fmt.Errorf("image %s is not here, and the source knows no reference to pull it by", digest))
	}
	self, err := os.Executable()
	if err != nil {
		return refuse(codeFailed, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, self, "--store-dir", s.RootDir(), "pull", "--", ref)
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		text := strings.TrimSpace(out.String())
		if len(text) > 1000 {
			text = text[len(text)-1000:]
		}
		return refuse(codeFailed, fmt.Errorf("hull pull %s: %v: %s", ref, err, text))
	}
	if !s.ImageComplete(digest) {
		return refuse(codeFailed, fmt.Errorf("pulled %s, which is not image %s any more", ref, digest))
	}
	return &migrateReply{OK: true, Code: codePulled}
}

// codePulled is migrate-take-image's answer when it pulled the image, not
// found it there.
const codePulled = "pulled"

// prepareOptions are the flags of `hull migrate-prepare`.
type prepareOptions struct {
	remote migrateRemote
	out    io.Writer
	json   bool
}

// prepareStep is one line of what a prepare did.
type prepareStep struct {
	What    string  `json:"what"`
	Result  string  `json:"result"` // "held", "sent", "skipped", "failed"
	Detail  string  `json:"detail,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// prepareResult is `hull migrate-prepare --json`'s output.
type prepareResult struct {
	Name   string        `json:"name"`
	To     string        `json:"to"`
	Steps  []prepareStep `json:"steps"`
	Holds  *migrateProbe `json:"holds,omitempty"`
	Stream bool          `json:"stream"`
	Why    string        `json:"why,omitempty"`
}

// sourcePrepare is `hull migrate-prepare NAME --to TARGET`.
func sourcePrepare(ctx context.Context, s *store.Store, name string, opt prepareOptions, peer movePeer) error {
	if err := store.ValidateInstanceID(name); err != nil {
		return err
	}
	if err := checkSSHTarget(opt.remote.Target); err != nil {
		return err
	}
	state, err := s.GetInstance(name)
	if err != nil {
		return fmt.Errorf("instance not found: %s", name)
	}
	if !isHviInstance(state) {
		return fmt.Errorf("instance %s does not run on hvi; only an hvi instance moves", name)
	}
	rec := &migrationRecord{Name: name, Peer: opt.remote.Target, Remote: &opt.remote}
	p := peerFor(rec, peer)
	res, err := prepareDestination(ctx, s, state, p, opt.remote, opt.out)
	if res != nil && opt.json {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	}
	return err
}

// prepareDestination makes the destination hold what a streamed move of
// state needs and reports each step. It fails only when a step the move
// needs failed: the base. The image and the kernel are reported.
func prepareDestination(ctx context.Context, s *store.Store, state *store.InstanceState, p movePeer,
	remote migrateRemote, w io.Writer) (*prepareResult, error) {
	res := &prepareResult{Name: state.ID, To: remote.Target}
	logf := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	reply, err := p.call(ctx, "migrate-probe", probeArgs(remote, ""), nil)
	if err != nil {
		return res, fmt.Errorf("probe %s: %w", remote.Target, err)
	}
	probe := reply.Probe
	if !probe.hasCapability(moveCapStream) {
		res.Why = fmt.Sprintf("the hull on %s (%s) takes no streamed move", remote.Target, probe.HullVersion)
		logf("%s: nothing to prepare: %s; a move sends the whole checkpoint and disk", remote.Target, res.Why)
		res.Holds = probe
		return res, nil
	}
	var failed []string
	step := func(st prepareStep) {
		res.Steps = append(res.Steps, st)
		line := fmt.Sprintf("  %s: %s", st.What, st.Result)
		if st.Detail != "" {
			line += " (" + st.Detail + ")"
		}
		logf("%s", line)
	}
	logf("Preparing %s for %s", remote.Target, state.ID)

	// The image: a later run there needs it; the move does not.
	if state.ImageDigest != "" {
		started := time.Now()
		ref := ""
		if img, err := s.GetImage(state.ImageDigest); err == nil {
			ref = img.Ref
		}
		reply, err := p.call(ctx, "migrate-take-image", []string{"--digest", state.ImageDigest, "--ref", ref}, nil)
		switch {
		case err != nil:
			step(prepareStep{What: "image " + digestHead(state.ImageDigest), Result: "failed", Detail: refusalDetail(err)})
		case reply.Code == codePulled:
			step(prepareStep{What: "image " + digestHead(state.ImageDigest), Result: "sent",
				Detail: "pulled there as " + ref, Seconds: time.Since(started).Seconds()})
		default:
			step(prepareStep{What: "image " + digestHead(state.ImageDigest), Result: "held"})
		}
	}

	// The profile's kernel: a later boot there needs it, and brig's
	// sessions run portable.
	if state.Profile != "" {
		want := installedProfileKernel(s.RootDir(), state.Profile)
		what := state.Profile + " kernel"
		switch held := probe.ProfileKernels[state.Profile]; {
		case want == "":
			step(prepareStep{What: what, Result: "skipped", Detail: "none is installed here to send"})
		case held == want:
			step(prepareStep{What: what, Result: "held", Detail: digestHead(want)})
		case held != "":
			step(prepareStep{What: what, Result: "skipped", Detail: fmt.Sprintf("it holds another one (%s), "+
				"which is not replaced; this host's is %s", digestHead(held), digestHead(want))})
		default:
			st, err := sendProfileKernel(ctx, s, p, state.Profile, want)
			if err != nil {
				step(prepareStep{What: what, Result: "failed", Detail: refusalDetail(err)})
			} else {
				step(st)
			}
		}
	}

	// The base: what lets the move send only the guest's own writes.
	var baseWhat string
	switch b := state.Base; {
	case b == nil:
		baseWhat = "base"
		res.Why = "the instance keeps no base (it was started before bases, or not on a block rootfs)"
		step(prepareStep{What: baseWhat, Result: "skipped", Detail: res.Why})
	case !baseHeld(s, b.SHA256):
		baseWhat = "base " + digestHead(b.SHA256)
		res.Why = "its base is gone from this store"
		step(prepareStep{What: baseWhat, Result: "skipped", Detail: res.Why})
	case probe.holdsBase(b.SHA256):
		step(prepareStep{What: "base " + digestHead(b.SHA256), Result: "held"})
	default:
		baseWhat = "base " + digestHead(b.SHA256)
		st, err := sendBase(ctx, s, p, b.SHA256)
		if err != nil {
			failed = append(failed, baseWhat+": "+refusalDetail(err))
			step(prepareStep{What: baseWhat, Result: "failed", Detail: refusalDetail(err)})
		} else {
			step(st)
		}
	}

	if remote.GatewaySock != "" {
		if probe.GatewayOK {
			step(prepareStep{What: "gateway " + remote.GatewaySock, Result: "held", Detail: "it answers"})
		} else {
			step(prepareStep{What: "gateway " + remote.GatewaySock, Result: "failed", Detail: probe.GatewayDetail})
		}
	}

	if reply, err := p.call(ctx, "migrate-probe", probeArgs(remote, ""), nil); err == nil {
		res.Holds = reply.Probe
	}
	res.Stream = state.Base != nil && res.Holds.holdsBase(state.Base.SHA256) && res.Holds.HviStream
	switch {
	case len(failed) > 0:
		return res, fmt.Errorf("prepare %s for %s: %s", remote.Target, state.ID, strings.Join(failed, "; "))
	case res.Stream:
		logf("%s is prepared: a move of %s sends only what the guest changed", remote.Target, state.ID)
	case res.Holds != nil && !res.Holds.HviStream:
		res.Why = fmt.Sprintf("its hvi (%s) cannot restore from a stream", res.Holds.Hvi)
		logf("%s holds what it can, but %s; a move sends the whole checkpoint and disk", remote.Target, res.Why)
	default:
		logf("%s holds what it can; a move of %s sends the whole checkpoint and disk (%s)", remote.Target,
			state.ID, res.Why)
	}
	return res, nil
}

// probeArgs are migrate-probe's arguments for remote and, when not "", the
// instance name the move would use.
func probeArgs(remote migrateRemote, name string) []string {
	var args []string
	if name != "" {
		args = append(args, "--name", name)
	}
	if remote.Hvi != "" {
		args = append(args, "--hvi", remote.Hvi)
	}
	if remote.GatewaySock != "" {
		args = append(args, "--gateway-sock", remote.GatewaySock)
	}
	return args
}

// digestHead is the head of a digest, for a person.
func digestHead(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	return d[:min(len(d), 12)]
}

// sendBase streams the base of digest sum to the destination.
func sendBase(ctx context.Context, s *store.Store, p movePeer, sum string) (prepareStep, error) {
	started := time.Now()
	pr, pw := io.Pipe()
	type sent struct {
		st  baseSendStats
		err error
	}
	done := make(chan sent, 1)
	go func() {
		st, err := writeBaseStream(pw, s.BasePath(sum), sum)
		_ = pw.CloseWithError(err)
		done <- sent{st, err}
	}()
	_, err := p.call(ctx, "migrate-take-base", []string{"--sha256", sum}, pr)
	_ = pr.CloseWithError(errors.New("the destination stopped reading"))
	got := <-done
	if err != nil {
		return prepareStep{}, err
	}
	took := time.Since(started).Seconds()
	if got.err != nil && !errors.Is(got.err, io.ErrClosedPipe) {
		return prepareStep{}, fmt.Errorf("send: %w", got.err)
	}
	st := prepareStep{What: "base " + digestHead(sum), Result: "sent", Bytes: got.st.WireBytes, Seconds: took}
	if got.err == nil {
		st.Detail = fmt.Sprintf("%s of data as %s, %.1f s, %s/s", humanBytes(got.st.DataBytes),
			humanBytes(got.st.WireBytes), took, humanBytes(int64(float64(got.st.WireBytes)/max(took, 1e-3))))
	} else {
		// It answered before reading it all: it held it already.
		st.Result, st.Detail = "held", "the destination already had it"
	}
	return st, nil
}

// sendProfileKernel sends this store's kernel of profile, whose record says
// sha256 sum, to be installed there.
func sendProfileKernel(ctx context.Context, s *store.Store, p movePeer, profile, sum string) (prepareStep, error) {
	started := time.Now()
	kernel, _, err := bootassets.ProfilePaths(s.RootDir(), profile)
	if err != nil {
		return prepareStep{}, err
	}
	f, err := os.Open(kernel)
	if err != nil {
		return prepareStep{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return prepareStep{}, err
	}
	_, err = p.call(ctx, "migrate-take-kernel", []string{"--profile", profile, "--sha256", sum,
		"--size", strconv.FormatInt(info.Size(), 10)}, f)
	if err != nil {
		return prepareStep{}, err
	}
	return prepareStep{What: profile + " kernel", Result: "sent", Detail: digestHead(sum), Bytes: info.Size(),
		Seconds: time.Since(started).Seconds()}, nil
}

// migratePrepareCommand is `hull migrate-prepare`. Given NAME and --to it is
// the operator's command above. Given --txid it is the destination's hidden
// step of a move, which brings a received checkpoint up paused; a source
// hull runs that one over ssh, and it keeps its name for every hull that
// already does.
func migratePrepareCommand() *cli.Command {
	remote := migrateRemoteVerb("migrate-prepare", "", []cli.Flag{},
		func(cmd *cli.Command, s *store.Store) *migrateReply { return destPrepare(s, cmd.String("txid")) })
	return &cli.Command{
		Name: "migrate-prepare",
		Usage: "put on another host, ahead of a move and while the guest runs, what a fast move needs: " +
			"the instance's base image, its machine profile's kernel and its image; safe to run again",
		ArgsUsage: "NAME --to USER@HOST",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "to", Usage: "USER@HOST to prepare, over ssh"},
			&cli.StringFlag{Name: "remote-store", Usage: "the destination hull's --store-dir (default: its own default)"},
			&cli.StringFlag{Name: "remote-hull", Usage: "path of hull on the destination (default: hull on its PATH)"},
			&cli.StringFlag{Name: "remote-hvi", Usage: "path of hvi on the destination (default: the one its hull would run)"},
			&cli.StringFlag{Name: "remote-gateway-sock", Usage: "a network gateway on the destination to check"},
			&cli.BoolFlag{Name: "json", Usage: "print what was done and what the destination holds as JSON on stdout"},
			&cli.StringFlag{Name: "txid", Hidden: true, Usage: "a move's txid (the destination's step of a move)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.IsSet("txid") {
				if cmd.Args().Len() != 0 || cmd.IsSet("to") {
					return errors.New("--txid takes no NAME or --to")
				}
				return remote.Action(ctx, cmd)
			}
			if cmd.Args().Len() != 1 || cmd.String("to") == "" {
				return errors.New("usage: hull migrate-prepare NAME --to USER@HOST")
			}
			s, err := globalStore(cmd)
			if err != nil {
				return err
			}
			return sourcePrepare(ctx, s, cmd.Args().First(), prepareOptions{
				remote: migrateRemote{Target: cmd.String("to"), Store: cmd.String("remote-store"),
					Hull: cmd.String("remote-hull"), Hvi: cmd.String("remote-hvi"),
					GatewaySock: cmd.String("remote-gateway-sock")},
				out: os.Stderr, json: cmd.Bool("json"),
			}, nil)
		},
	}
}

// migratePrepareRemoteCommands are the destination's prepare verbs, hidden.
func migratePrepareRemoteCommands() []*cli.Command {
	return []*cli.Command{
		migrateRemoteVerb("migrate-take-base", "keep the base image the stream on stdin carries",
			[]cli.Flag{&cli.StringFlag{Name: "sha256", Required: true}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				return destTakeBase(s, cmd.String("sha256"), os.Stdin)
			}),
		migrateRemoteVerb("migrate-take-kernel", "install the profile kernel on stdin",
			[]cli.Flag{&cli.StringFlag{Name: "profile", Required: true}, &cli.StringFlag{Name: "sha256", Required: true},
				&cli.Int64Flag{Name: "size", Required: true}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				return destTakeKernel(s, cmd.String("profile"), cmd.String("sha256"), cmd.Int64("size"), os.Stdin)
			}),
		migrateRemoteVerb("migrate-take-image", "pull an image unless the store has it",
			[]cli.Flag{&cli.StringFlag{Name: "digest", Required: true}, &cli.StringFlag{Name: "ref"}},
			func(cmd *cli.Command, s *store.Store) *migrateReply {
				return destTakeImage(s, cmd.String("ref"), cmd.String("digest"))
			}),
	}
}
