// Copyright (c) 2023-2026, Nubificus LTD
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

//go:build darwin

// hull-guest-agent is the exec agent for macOS guests. It runs inside the
// guest as a launchd daemon and starts processes for `hull exec`.
//
// It is a port of urunc's urunit-agent and speaks the same agentproto frames
// on the same vsock port, so the host side is unchanged: vz-runner bridges
// the instance's agent socket to the port, and `hull exec` dials the socket.
//
// Three things differ from the Linux agent:
//
//   - vsock is the only transport. A macOS guest runs on Vz only, which
//     always has a vsock device.
//   - An empty user means the user logged in at the console, and root when
//     nobody is. A macOS guest has no image-configured user, and the console
//     user owns the desktop session that GUI applications need.
//   - Users come from Directory Services, not /etc/passwd.
//   - A session for the console user starts inside that user's launchd
//     session. See desktopSessionArgv.
//
// It also mounts what hull asked for at boot. See applyGuestMounts.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/urunc-dev/urunc/pkg/agentproto"
	"golang.org/x/sys/unix"
)

// defaultPath is the PATH a session gets when the request sets none.
// Homebrew's prefix comes first, as it does in a login shell on Apple silicon.
const defaultPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// maxGroups is NGROUPS_MAX on Darwin. setgroups(2) fails with more.
const maxGroups = 16

func main() {
	log.SetPrefix("hull-guest-agent: ")
	log.SetFlags(log.LstdFlags)

	if len(os.Args) > 1 && os.Args[1] == execAsFlag {
		execAs(os.Args[2:])
	}

	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}

	// Only the daemon, which runs as root, can mount.
	if os.Geteuid() == 0 {
		go applyGuestMounts()
	}

	// launchd can start the daemon before the vsock device is ready, so a
	// failure to listen is retried.
	for {
		if err := serveVsock(); err != nil {
			log.Printf("vsock transport unavailable: %v; retrying", err)
		}
		time.Sleep(time.Second)
	}
}

// serveVsock listens on the agent vsock port and serves each connection
// concurrently. It returns only on error.
func serveVsock() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("vsock socket: %w", err)
	}
	unix.CloseOnExec(fd)
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agentproto.DefaultVsockPort}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 8); err != nil {
		unix.Close(fd)
		return fmt.Errorf("vsock listen: %w", err)
	}
	defer unix.Close(fd)
	log.Printf("listening on vsock port %d", agentproto.DefaultVsockPort)
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("vsock accept: %w", err)
		}
		// Darwin has no accept4. Without close-on-exec, every process the
		// agent starts inherits the connection to the host.
		unix.CloseOnExec(nfd)
		conn := os.NewFile(uintptr(nfd), "vsock-conn")
		go func() {
			serveConn(conn)
			conn.Close()
		}()
	}
}

// connState is one protocol peer: a frame writer shared by all sessions
// multiplexed on the connection.
type connState struct {
	mu sync.Mutex
	w  io.Writer

	// closed is set when the peer is gone, so a session that outlives it
	// stops writing.
	closed atomic.Bool

	sessMu   sync.Mutex
	sessions map[uint32]*session
}

func (c *connState) writeFrame(typ byte, stream uint32, payload []byte) error {
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	return agentproto.WriteFrame(c.w, typ, stream, payload)
}

func (c *connState) writeJSON(typ byte, stream uint32, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(typ, stream, payload)
}

func (c *connState) session(id uint32) *session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessions[id]
}

func (c *connState) addSession(id uint32, s *session) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessions[id] = s
}

func (c *connState) removeSession(id uint32) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	delete(c.sessions, id)
}

// outputDrainTimeout bounds how long a session's exit report waits for the
// rest of its output once the process has exited.
const outputDrainTimeout = 2 * time.Second

// session is one running exec'd process.
type session struct {
	cmd   *exec.Cmd
	ptmx  *os.File       // tty sessions
	stdin io.WriteCloser // non-tty sessions
}

// relayOutput forwards everything r yields to write, one read at a time. It
// returns when write fails or r reports an error, and it never closes r.
func relayOutput(r io.Reader, write func([]byte) error) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func serveConn(rw io.ReadWriter) {
	c := &connState{w: rw, sessions: make(map[uint32]*session)}
	for {
		f, err := agentproto.ReadFrame(rw)
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			// The peer is gone, and its sessions go with it.
			c.closed.Store(true)
			c.sessMu.Lock()
			for _, s := range c.sessions {
				if s.cmd.Process != nil {
					_ = s.cmd.Process.Kill()
				}
			}
			c.sessMu.Unlock()
			return
		}
		switch f.Type {
		case agentproto.TypeOpen:
			var req agentproto.OpenRequest
			if err := json.Unmarshal(f.Payload, &req); err != nil {
				_ = c.writeJSON(agentproto.TypeError, f.Stream, agentproto.Error{Message: "bad open request: " + err.Error()})
				continue
			}
			if err := c.open(f.Stream, req); err != nil {
				_ = c.writeJSON(agentproto.TypeError, f.Stream, agentproto.Error{Message: err.Error()})
			}
		case agentproto.TypeStdin:
			if s := c.session(f.Stream); s != nil {
				if s.ptmx != nil {
					_, _ = s.ptmx.Write(f.Payload)
				} else if s.stdin != nil {
					_, _ = s.stdin.Write(f.Payload)
				}
			}
		case agentproto.TypeCloseStdin:
			if s := c.session(f.Stream); s != nil && s.stdin != nil {
				_ = s.stdin.Close()
			}
		case agentproto.TypeResize:
			var rs agentproto.Resize
			if err := json.Unmarshal(f.Payload, &rs); err == nil {
				if s := c.session(f.Stream); s != nil && s.ptmx != nil {
					_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: rs.Rows, Cols: rs.Cols})
				}
			}
		case agentproto.TypeSignal:
			var sg agentproto.Signal
			if err := json.Unmarshal(f.Payload, &sg); err == nil {
				if s := c.session(f.Stream); s != nil && s.cmd.Process != nil {
					_ = s.cmd.Process.Signal(syscall.Signal(sg.Signal))
				}
			}
		default:
			log.Printf("unknown frame type %d", f.Type)
		}
	}
}

// open starts the requested process on the given stream.
func (c *connState) open(stream uint32, req agentproto.OpenRequest) error {
	if len(req.Argv) == 0 {
		return errors.New("empty argv")
	}
	if c.session(stream) != nil {
		return fmt.Errorf("stream %d already in use", stream)
	}

	gu, err := resolveUser(req.User)
	if err != nil {
		return err
	}

	argv := req.Argv
	var credential *syscall.Credential
	if gu != nil {
		if os.Geteuid() == 0 && gu.uid == consoleUser() {
			argv, err = desktopSessionArgv(gu, req.Argv)
			if err != nil {
				return err
			}
		} else {
			credential = gu.credential()
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: running the requested argv is the agent's job
	cmd.Env = defaultEnv(req.Env, gu)
	cmd.Dir = req.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	if gu != nil {
		if cmd.Dir == "" {
			cmd.Dir = gu.home
		}
	}
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}

	s := &session{cmd: cmd}
	// outputs are the agent's ends of the process's stdout and stderr. The
	// relays forward them to the peer, and the exit report waits for them.
	var outputs []*os.File
	var relays sync.WaitGroup
	relay := func(r io.Reader, typ byte) {
		relays.Add(1)
		go func() {
			defer relays.Done()
			relayOutput(r, func(b []byte) error {
				return c.writeFrame(typ, stream, b)
			})
		}()
	}
	if req.TTY {
		ws := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
		if ws.Rows == 0 || ws.Cols == 0 {
			ws.Rows, ws.Cols = 24, 80
		}
		// The slave is chowned to the target user, like login(1), so a
		// program that reopens its own tty keeps working.
		ptmx, tts, err := pty.Open()
		if err != nil {
			return fmt.Errorf("open pty: %w", err)
		}
		if gu != nil && os.Geteuid() == 0 {
			if cherr := os.Chown(tts.Name(), int(gu.uid), int(gu.gid)); cherr != nil {
				log.Printf("chown %s: %v", tts.Name(), cherr)
			}
		}
		_ = pty.Setsize(ptmx, ws)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tts, tts, tts
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
		cmd.SysProcAttr.Ctty = 0
		if err := cmd.Start(); err != nil {
			_ = ptmx.Close()
			_ = tts.Close()
			return fmt.Errorf("start (tty): %w", err)
		}
		_ = tts.Close()
		s.ptmx = ptmx
		outputs = []*os.File{ptmx}
		relay(ptmx, agentproto.TypeStdout)
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		// cmd.Wait closes the pipes from StdoutPipe and StderrPipe as soon
		// as the process exits, and any output still unread in them is
		// lost. Plain pipes stay open until the agent closes them.
		stdoutR, stdoutW, err := os.Pipe()
		if err != nil {
			return err
		}
		stderrR, stderrW, err := os.Pipe()
		if err != nil {
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			return err
		}
		cmd.Stdout, cmd.Stderr = stdoutW, stderrW
		err = cmd.Start()
		// The child holds its own copies of the write ends. Closing ours lets
		// the relays see EOF once the process and its descendants are done.
		_ = stdoutW.Close()
		_ = stderrW.Close()
		if err != nil {
			_ = stdoutR.Close()
			_ = stderrR.Close()
			return fmt.Errorf("start: %w", err)
		}
		s.stdin = stdin
		outputs = []*os.File{stdoutR, stderrR}
		relay(stdoutR, agentproto.TypeStdout)
		relay(stderrR, agentproto.TypeStderr)
	}

	c.addSession(stream, s)
	go func() {
		code := 0
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
				if code < 0 {
					// Killed by signal: report 128+sig like a shell.
					if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
						code = 128 + int(st.Signal())
					}
				}
			} else {
				code = 126
			}
		}
		// The peer ends the session on the Exit frame and drops any output
		// that follows it, so relay the output first. A process left running
		// in the background can hold the output open, so the wait is bounded.
		drained := make(chan struct{})
		go func() {
			relays.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(outputDrainTimeout):
			log.Printf("stream %d: output still open %v after exit", stream, outputDrainTimeout)
		}
		for _, f := range outputs {
			_ = f.Close()
		}
		c.removeSession(stream)
		_ = c.writeJSON(agentproto.TypeExit, stream, agentproto.Exit{Code: code})
	}()

	return nil
}

// defaultEnv fills in PATH, TERM and, when running as a resolved user, the
// login-style identity variables, unless the request carries them.
func defaultEnv(env []string, gu *guestUser) []string {
	has := func(key string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, key+"=") {
				return true
			}
		}
		return false
	}
	if !has("PATH") {
		env = append(env, "PATH="+defaultPath)
	}
	if !has("TERM") {
		env = append(env, "TERM=xterm-256color")
	}
	if gu != nil {
		if !has("HOME") {
			env = append(env, "HOME="+gu.home)
		}
		if !has("USER") {
			env = append(env, "USER="+gu.name)
		}
		if !has("LOGNAME") {
			env = append(env, "LOGNAME="+gu.name)
		}
		if !has("SHELL") {
			env = append(env, "SHELL="+gu.shell)
		}
	}
	return env
}

// guestUser is a resolved guest identity for an exec session.
type guestUser struct {
	uid    uint32
	gid    uint32
	groups []uint32
	name   string
	home   string
	shell  string
}

// credential returns the credential a session runs with, or nil to run as the
// agent itself. An agent that is not root cannot change identity, and one
// that already is the requested user has nothing to change.
func (gu *guestUser) credential() *syscall.Credential {
	if os.Geteuid() != 0 {
		return nil
	}
	groups := gu.groups
	if len(groups) > maxGroups {
		groups = groups[:maxGroups]
	}
	return &syscall.Credential{Uid: gu.uid, Gid: gu.gid, Groups: groups}
}

// execAsFlag selects the agent's helper mode, which desktopSessionArgv uses.
const execAsFlag = "--exec-as"

// desktopSessionArgv wraps argv so that it runs as gu inside gu's launchd
// session.
//
// A process the daemon starts is in the system session, whatever its uid. It
// cannot reach the user's window server from there, so `open` and every GUI
// program fail. `launchctl asuser` moves a process into the user's session,
// but it needs root and leaves the process as root. The agent's helper mode
// runs next: it drops to the user and execs the command. Both steps exec in
// place, so the session keeps one pid, its stdio and its tty.
func desktopSessionArgv(gu *guestUser, argv []string) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	wrapped := []string{
		"/bin/launchctl", "asuser", strconv.FormatUint(uint64(gu.uid), 10),
		self, execAsFlag, gu.execAsSpec(), "--",
	}
	return append(wrapped, argv...), nil
}

// execAsSpec encodes the identity for the helper mode as uid:gid:g1,g2,...
func (gu *guestUser) execAsSpec() string {
	groups := gu.groups
	if len(groups) > maxGroups {
		groups = groups[:maxGroups]
	}
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = strconv.FormatUint(uint64(g), 10)
	}
	return fmt.Sprintf("%d:%d:%s", gu.uid, gu.gid, strings.Join(parts, ","))
}

// parseExecAs decodes what execAsSpec wrote.
func parseExecAs(spec string) (uid, gid int, groups []int, err error) {
	fields := strings.Split(spec, ":")
	if len(fields) != 3 {
		return 0, 0, nil, fmt.Errorf("bad identity %q", spec)
	}
	if uid, err = strconv.Atoi(fields[0]); err != nil {
		return 0, 0, nil, fmt.Errorf("bad uid in %q", spec)
	}
	if gid, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, nil, fmt.Errorf("bad gid in %q", spec)
	}
	for _, f := range strings.Split(fields[2], ",") {
		if f == "" {
			continue
		}
		g, gerr := strconv.Atoi(f)
		if gerr != nil {
			return 0, 0, nil, fmt.Errorf("bad group in %q", spec)
		}
		groups = append(groups, g)
	}
	return uid, gid, groups, nil
}

// execAs is the helper mode: args are <uid:gid:groups> -- <argv...>. It
// drops to that identity and execs argv, and it does not return. Exit status
// 126 and 127 follow the shell's convention for a command that could not be
// run or found.
func execAs(args []string) {
	fail := func(code int, err error) {
		fmt.Fprintf(os.Stderr, "hull-guest-agent: %v\n", err)
		os.Exit(code)
	}
	if len(args) < 3 || args[1] != "--" {
		fail(126, errors.New("usage: "+execAsFlag+" <uid:gid:groups> -- <command> [args...]"))
	}
	uid, gid, groups, err := parseExecAs(args[0])
	if err != nil {
		fail(126, err)
	}
	argv := args[2:]
	// The groups go first: once the uid is dropped, the process may no
	// longer change them.
	if err := syscall.Setgroups(groups); err != nil {
		fail(126, fmt.Errorf("setgroups: %w", err))
	}
	if err := syscall.Setgid(gid); err != nil {
		fail(126, fmt.Errorf("setgid: %w", err))
	}
	if err := syscall.Setuid(uid); err != nil {
		fail(126, fmt.Errorf("setuid: %w", err))
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fail(127, err)
	}
	fail(126, syscall.Exec(path, argv, os.Environ()))
}

// consoleUser is the uid that owns /dev/console, which is the user logged in
// at the guest's display. It is 0 when nobody is.
var consoleUser = func() uint32 {
	var st unix.Stat_t
	if err := unix.Stat("/dev/console", &st); err != nil {
		return 0
	}
	return st.Uid
}

// resolveUser turns an OpenRequest user spec (name, uid, or uid:gid) into a
// guest identity. An empty spec means the console user. It returns nil for
// root, which runs with the agent's own identity.
func resolveUser(spec string) (*guestUser, error) {
	if spec == "" {
		uid := consoleUser()
		if uid == 0 {
			return nil, nil
		}
		spec = strconv.FormatUint(uint64(uid), 10)
	}

	uidPart, gidPart, hasGID := strings.Cut(spec, ":")

	var u *user.User
	var err error
	if _, nerr := strconv.ParseUint(uidPart, 10, 32); nerr == nil {
		u, err = user.LookupId(uidPart)
	} else {
		u, err = user.Lookup(uidPart)
	}

	gu := &guestUser{home: "/", shell: "/bin/zsh"}
	if err == nil {
		uid, uerr := strconv.ParseUint(u.Uid, 10, 32)
		gid, gerr := strconv.ParseUint(u.Gid, 10, 32)
		if uerr != nil || gerr != nil {
			return nil, fmt.Errorf("non-numeric uid/gid for user %q", spec)
		}
		gu.uid, gu.gid = uint32(uid), uint32(gid)
		gu.name = u.Username
		if u.HomeDir != "" {
			gu.home = u.HomeDir
		}
		if sh := lookupShell(u.Username); sh != "" {
			gu.shell = sh
		}
		// The primary group goes first, so it survives the cut to maxGroups.
		gu.groups = []uint32{gu.gid}
		if ids, gerr := u.GroupIds(); gerr == nil {
			for _, id := range ids {
				if g, perr := strconv.ParseUint(id, 10, 32); perr == nil && uint32(g) != gu.gid {
					gu.groups = append(gu.groups, uint32(g))
				}
			}
		}
	} else {
		// Not a known account: accept a raw numeric uid.
		uid, nerr := strconv.ParseUint(uidPart, 10, 32)
		if nerr != nil {
			return nil, fmt.Errorf("unknown user %q", uidPart)
		}
		gu.uid = uint32(uid)
		gu.gid = uint32(uid)
		gu.name = uidPart
	}

	if hasGID {
		gid, nerr := strconv.ParseUint(gidPart, 10, 32)
		if nerr != nil {
			g, gerr := user.LookupGroup(gidPart)
			if gerr != nil {
				return nil, fmt.Errorf("unknown group %q", gidPart)
			}
			gid, nerr = strconv.ParseUint(g.Gid, 10, 32)
			if nerr != nil {
				return nil, fmt.Errorf("non-numeric gid for group %q", gidPart)
			}
		}
		gu.gid = uint32(gid)
	}

	if gu.uid == 0 {
		return nil, nil
	}
	return gu, nil
}

// lookupShell returns the login shell of a user. os/user does not expose it,
// and on macOS it is in Directory Services.
func lookupShell(username string) string {
	out, err := exec.Command("/usr/bin/dscl", ".", "-read", "/Users/"+username, "UserShell").Output() //nolint:gosec // G204: fixed binary, the name is one argument
	if err != nil {
		return ""
	}
	return parseUserShell(string(out))
}

// parseUserShell extracts the shell from dscl's "UserShell: /bin/zsh".
func parseUserShell(out string) string {
	_, shell, ok := strings.Cut(strings.TrimSpace(out), "UserShell:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(shell)
}
