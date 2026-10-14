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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// migrateSSHEnv names a program to run instead of ssh, with the same
// arguments. The tests point it at a script that runs the remote command
// locally against a second store, so a whole move runs on one machine with
// no network (docs/migration.md, "Testing a move without a second host").
const migrateSSHEnv = "HULL_MIGRATE_SSH"

// migrateProtocol is the version of the remote verbs' requests and replies.
// The probe reports it, and a source refuses a destination that speaks
// another.
const migrateProtocol = 1

// migrateReply is what every remote verb prints on stdout, and nothing else:
// one JSON object. ok:false is a definite answer from the destination, with
// a code the source branches on; an exit, an ssh failure or output that is
// not one of these is no answer at all, and the source must not read
// anything into it.
type migrateReply struct {
	Protocol int              `json:"protocol"`
	OK       bool             `json:"ok"`
	Code     string           `json:"code,omitempty"`
	Error    string           `json:"error,omitempty"`
	Record   *migrationRecord `json:"record,omitempty"`
	Probe    *migrateProbe    `json:"probe,omitempty"`
	// VM is what the destination's hvi says its VM is doing, when asked.
	VM string `json:"vm,omitempty"`
}

// migrateProbe is what migrate-probe reports about the destination.
type migrateProbe struct {
	HullVersion      string `json:"hullVersion"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	Hvi              string `json:"hvi,omitempty"`
	HviVersion       string `json:"hviVersion,omitempty"`
	HviControlFlags  bool   `json:"hviControlFlags"`
	HviRestore       bool   `json:"hviRestore"`
	HviError         string `json:"hviError,omitempty"`
	Hypervisor       bool   `json:"hypervisor"`
	HypervisorDetail string `json:"hypervisorDetail,omitempty"`
	StoreDir         string `json:"storeDir"`
	FreeBytes        int64  `json:"freeBytes"`
	NameTaken        bool   `json:"nameTaken"`
	NameTxID         string `json:"nameTxid,omitempty"`
	// NameMove is the role and state of that move here, "source
	// committed" for the stale copy a move away left behind.
	NameMove      string `json:"nameMove,omitempty"`
	GatewayOK     bool   `json:"gatewayOk,omitempty"`
	GatewayDetail string `json:"gatewayDetail,omitempty"`
	// HviProfile is whether the destination's hvi takes --profile: one
	// that does not cannot restore a portable instance.
	HviProfile bool `json:"hviProfile,omitempty"`
	// HviStream is whether the destination's hvi restores from a stream
	// (`restore --from-stream`), which a streamed move needs.
	HviStream bool `json:"hviStream,omitempty"`
	// Capabilities are the move features this hull takes beyond protocol
	// 1, such as moveCapStream. A hull from before them lists none.
	Capabilities []string `json:"capabilities,omitempty"`
	// Bases are the disk digests of the bases the store holds.
	Bases []string `json:"bases,omitempty"`
	// ProfileKernels maps a machine profile to the sha256 of the kernel
	// installed for it.
	ProfileKernels map[string]string `json:"profileKernels,omitempty"`
}

// Reply codes a source branches on.
const (
	codeUnknownTxID    = "unknown_txid"
	codeAborted        = "aborted"
	codeRunning        = "running"
	codeNameTaken      = "name_taken"
	codeBusy           = "busy"
	codeRestoreRefused = "restore_refused"
	codeWrongEpoch     = "wrong_epoch"
	codeNotReady       = "not_ready"
	codeVerifyFailed   = "verify_failed"
	codeFailed         = "failed"
	// codeNoBase is a streamed receive refused because the destination
	// does not hold the base the disk blocks apply to.
	codeNoBase = "no_base"
)

// errMoveNoAnswer is a remote verb that produced no reply: ssh could not
// connect, the remote hull is missing or crashed, or the connection broke.
// For a step that changes state on the other side that is not a "no": the
// step may have happened.
var errMoveNoAnswer = errors.New("no answer from the destination")

// migrateRefusal is the destination answering ok:false.
type migrateRefusal struct {
	Verb  string
	Reply *migrateReply
}

func (e *migrateRefusal) Error() string {
	return fmt.Sprintf("the destination refused %s: %s (%s)", e.Verb, e.Reply.Error, e.Reply.Code)
}

// refusalCode returns the code of a migrateRefusal in err's chain, or "".
func refusalCode(err error) string {
	var r *migrateRefusal
	if errors.As(err, &r) {
		return r.Reply.Code
	}
	return ""
}

// refusalDetail returns what the destination said in a migrateRefusal in
// err's chain, or err itself.
func refusalDetail(err error) string {
	var r *migrateRefusal
	if errors.As(err, &r) {
		return r.Reply.Error
	}
	return err.Error()
}

// movePeer runs one remote verb on the destination. stdin, when not nil, is
// copied to the verb's standard input. The source drives every step through
// this, so the tests can put the destination in the same process, drop a
// reply, or answer for it.
type movePeer interface {
	call(ctx context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error)
}

// sshPeer runs `hull <verb>` on the destination over ssh.
type sshPeer struct {
	target string
	remote migrateRemote
	// warnedMux is set once the move said it runs without the mux.
	warnedMux bool
}

// validSSHTarget is [USER@]HOST, nothing ssh could read as an option.
var validSSHTarget = regexp.MustCompile(`^([A-Za-z0-9._][A-Za-z0-9._-]*@)?[A-Za-z0-9._:\[\]-]+$`)

// checkSSHTarget refuses a --to that is not [USER@]HOST. One that starts
// with '-' would be an ssh option (-oProxyCommand=... runs a program), and
// one with spaces or control characters is not a host.
func checkSSHTarget(target string) error {
	if strings.HasPrefix(target, "-") || !validSSHTarget.MatchString(target) {
		return fmt.Errorf("--to %q is not USER@HOST", target)
	}
	return nil
}

// shellQuote quotes s for a POSIX shell. ssh hands the remote side one
// string, which the remote login shell splits, so every argument hull sends
// is quoted: a store path with a space, or a name a shell would expand,
// arrives as the one argument it was.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return r >= unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("@%+=:,./_-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand is the command line the destination's shell runs for verb.
func (p *sshPeer) remoteCommand(verb string, args []string) string {
	hull := p.remote.Hull
	if hull == "" {
		hull = "hull"
	}
	argv := []string{hull}
	if p.remote.Store != "" {
		argv = append(argv, "--store-dir", p.remote.Store)
	}
	argv = append(argv, verb)
	argv = append(argv, args...)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// migrateSSHMuxEnv set to "off" makes every step of a move its own ssh
// connection.
const migrateSSHMuxEnv = "HULL_MIGRATE_SSH_MUX"

// sshArgv is the ssh command line for verb: batch mode (a password prompt
// would hang a move with the source fenced), keepalives so a dead link ends
// the step instead of blocking it, no tty, and "--" so the target can never
// be read as an option.
//
// The steps share one connection (ControlMaster), which outlives each step
// by a minute: the commit, which a streamed move runs while the guest is
// stopped, then pays for no handshake. The socket is ssh's hash of the
// connection in a directory only this user can write (sshControlDir):
// anyone who can create the socket first would get the move's stream,
// guest RAM and all, or answer its steps. Without such a directory each
// step makes its own connection.
func (p *sshPeer) sshArgv(verb string, args []string) []string {
	ssh := os.Getenv(migrateSSHEnv)
	if ssh == "" {
		ssh = "ssh"
	}
	argv := []string{ssh, "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4"}
	if os.Getenv(migrateSSHMuxEnv) != "off" {
		if dir, err := sshControlDir(); err == nil {
			argv = append(argv, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(dir, "%C"),
				"-o", "ControlPersist=60")
		} else if !p.warnedMux {
			p.warnedMux = true
			log.Warnf("Each step of the move makes its own ssh connection: %v", err)
		}
	}
	return append(argv, "--", p.target, p.remoteCommand(verb, args))
}

// sshControlRoot is where sshControlDir makes its directory: short, since
// a Unix socket path holds at most 104 bytes on macOS, and ssh adds a
// 17-character suffix to the socket's name while it makes it.
var sshControlRoot = "/tmp"

// sshControlDir is the directory of this user's ssh mux sockets,
// <sshControlRoot>/hull-ssh-<uid>, made 0700 if it is not there. One that is
// there must be a directory, not a link, owned by this user and writable by
// no one else; another user who made it first gets no socket of ours.
func sshControlDir() (string, error) {
	dir := filepath.Join(sshControlRoot, fmt.Sprintf("hull-ssh-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return "", fmt.Errorf("%s is not a directory", dir)
	case !ok || int(st.Uid) != os.Getuid():
		return "", fmt.Errorf("%s belongs to another user", dir)
	case info.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("%s can be written by others (mode %o)", dir, info.Mode().Perm())
	}
	return dir, nil
}

func (p *sshPeer) call(ctx context.Context, verb string, args []string, stdin io.Reader) (*migrateReply, error) {
	argv := p.sshArgv(verb, args)
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	c.Stdin = stdin
	// In a process group of its own, so a Ctrl-C at the terminal reaches
	// hull alone, which holds it while hvi's stream is not on record
	// (moveSignals) and otherwise ends ssh through ctx.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A destination that dies mid-stream must not leave the copy blocked
	// on a pipe nobody reads.
	c.WaitDelay = 5 * time.Second
	runErr := c.Run()
	reply, perr := parseMigrateReply(stdout.Bytes())
	if perr != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2000 {
			detail = detail[len(detail)-2000:]
		}
		if runErr == nil {
			runErr = perr
		}
		if detail != "" {
			detail = "; it said: " + detail
		}
		return nil, fmt.Errorf("%w to %s on %s: %v%s", errMoveNoAnswer, verb, p.target, runErr, detail)
	}
	if !reply.OK {
		return reply, &migrateRefusal{Verb: verb, Reply: reply}
	}
	return reply, nil
}

// parseMigrateReply reads the one JSON object a remote verb prints. Output
// that is anything else is not a reply.
func parseMigrateReply(out []byte) (*migrateReply, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, errors.New("it printed nothing")
	}
	var reply migrateReply
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&reply); err != nil {
		return nil, fmt.Errorf("its output is not a reply: %w", err)
	}
	if dec.More() {
		return nil, errors.New("its output has more than one reply")
	}
	if reply.Protocol != migrateProtocol {
		return nil, fmt.Errorf("it speaks move protocol %d, this hull %d", reply.Protocol, migrateProtocol)
	}
	return &reply, nil
}
