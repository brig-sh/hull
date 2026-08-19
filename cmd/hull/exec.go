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

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/agentproto"
)

func execCommand() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "run a command in a running instance",
		ArgsUsage: "<instance-id> <command> [args...]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "tty",
				Aliases: []string{"t"},
				Usage:   "allocate a pseudo-terminal in the guest",
			},
			&cli.StringFlag{
				Name:  "cwd",
				Usage: "working directory inside the guest",
			},
			&cli.StringFlag{
				Name:    "user",
				Aliases: []string{"u"},
				Usage:   "run as this guest user (name, uid, or uid:gid); default: the image's configured user",
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Aliases: []string{"e"},
				Usage:   "set an environment variable in the guest: KEY=VALUE, or a bare KEY to inherit it from the host environment and keep the value out of argv (repeatable)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return execInstance(ctx, cmd)
		},
	}
}

// bundleUser reads the uid:gid the image was configured with from the
// instance's OCI bundle. Empty (root) stays empty so the agent treats it
// as the default identity.
func bundleUser(bundleDir string) string {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return ""
	}
	var spec struct {
		Process struct {
			User struct {
				UID uint32 `json:"uid"`
				GID uint32 `json:"gid"`
			} `json:"user"`
		} `json:"process"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return ""
	}
	if spec.Process.User.UID == 0 && spec.Process.User.GID == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", spec.Process.User.UID, spec.Process.User.GID)
}

func execInstance(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() < 2 {
		return errors.New("usage: exec [-t] <instance-id> <command> [args...]")
	}
	instanceID := args.First()
	argv := args.Slice()[1:]

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}
	if state.Status != "running" {
		return fmt.Errorf("instance %s is not running (status: %s)", instanceID, state.Status)
	}

	sockPath := s.InstanceAgentSocket(instanceID)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("cannot reach guest agent at %s (instance started without agent transport, or image lacks /urunit-agent): %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	// While this exec session is attached, sample the VM it belongs to.
	// This is how a detached VM (a wrapped agent sandbox) reports metrics:
	// only while a session is actually using it. The per-instance flock
	// in the sampler keeps concurrent sessions from double-counting.
	metricsDone := make(chan struct{})
	defer close(metricsDone)
	startVMMMetricsSampler(state.PID, state.Backend, state.StartTime,
		s.InstanceDir(instanceID), metricsDone)

	useTTY := cmd.Bool("tty")
	stdinFd := int(os.Stdin.Fd())
	stdinIsTTY := isTerminal(stdinFd)

	env, err := resolveEnvEntries(cmd.StringSlice("env"), os.LookupEnv)
	if err != nil {
		return err
	}
	rows, cols := fallbackRows, fallbackCols
	// The descriptor the size was measured from, re-read on every SIGWINCH.
	// Whether the session is interactive is a question about stdin, but the
	// window size belongs to whichever descriptor is really a terminal: callers
	// that pipe or capture stdout while leaving stdin on the terminal are
	// ordinary, and measuring stdout there silently yields 24x80.
	sizeFd, sizeFdIsTerminal := -1, false
	if useTTY {
		rows, cols, sizeFd, sizeFdIsTerminal = windowSize(
			int(os.Stdout.Fd()), int(os.Stdin.Fd()), int(os.Stderr.Fd()))
		// A default, not an override: an explicit --env TERM must win.
		env = appendEnvDefault(env, "TERM", os.Getenv("TERM"))
	}

	// All frames of this exec ride stream 1: one CLI process, one session.
	const stream = 1
	var wmu sync.Mutex
	writeFrame := func(typ byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return agentproto.WriteFrame(conn, typ, stream, payload)
	}
	writeJSON := func(typ byte, v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return agentproto.WriteJSON(conn, typ, stream, v)
	}

	// Like docker exec, default to the identity the image was configured
	// with; --user overrides, and --user root gets the old behavior.
	userSpec := cmd.String("user")
	if userSpec == "" {
		userSpec = bundleUser(state.BundleDir)
	}

	req := agentproto.OpenRequest{
		Argv: argv,
		Env:  env,
		Cwd:  cmd.String("cwd"),
		User: userSpec,
		TTY:  useTTY,
		Rows: rows,
		Cols: cols,
	}
	if err := writeJSON(agentproto.TypeOpen, req); err != nil {
		return fmt.Errorf("send open request: %w", err)
	}

	// Raw mode is stdin's business: it is the descriptor whose echo and line
	// discipline we take over.
	var origTermios *syscall.Termios
	if useTTY && stdinIsTTY {
		origTermios, err = makeRawTerminal(stdinFd)
		if err != nil {
			return fmt.Errorf("set raw mode: %w", err)
		}
		defer restoreTerminal(origTermios)
	} else {
		// Without a raw-mode tty, forward termination signals explicitly.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			for sig := range sigs {
				if s, ok := sig.(syscall.Signal); ok {
					_ = writeJSON(agentproto.TypeSignal, agentproto.Signal{Signal: int(s)})
				}
			}
		}()
	}

	// Following resizes is a question about the descriptor we measured, not
	// about stdin: `exec -t web top </dev/null` measures stdout and would
	// otherwise start at the right geometry and then never follow the window.
	// SIGWINCH reaches us either way, since it is delivered to the foreground
	// process group of the controlling terminal rather than through a
	// descriptor.
	if useTTY && sizeFdIsTerminal {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				r, c := terminalSize(sizeFd)
				_ = writeJSON(agentproto.TypeResize, agentproto.Resize{Rows: r, Cols: c})
			}
		}()
	}

	// stdin pump.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := writeFrame(agentproto.TypeStdin, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = writeFrame(agentproto.TypeCloseStdin, nil)
				return
			}
		}
	}()

	// Guest bytes reaching an interactive terminal go through the filter --
	// see guestTerminalWriter for what it neutralises and why. Each stream
	// gets its own filter: they carry independent escape state.
	guestOut := guestTerminalWriter(os.Stdout)
	guestErr := guestTerminalWriter(os.Stderr)

	// Frame loop: relay output until the session exits.
	for {
		f, rerr := agentproto.ReadFrame(conn)
		if rerr != nil {
			restoreTerminal(origTermios)
			return fmt.Errorf("connection to guest agent lost: %w", rerr)
		}
		switch f.Type {
		case agentproto.TypeStdout:
			_, _ = guestOut.Write(f.Payload)
		case agentproto.TypeStderr:
			_, _ = guestErr.Write(f.Payload)
		case agentproto.TypeExit:
			var ex agentproto.Exit
			_ = json.Unmarshal(f.Payload, &ex)
			restoreTerminal(origTermios)
			// This os.Exit propagates the guest exit code and skips the
			// funnels in main; the command itself succeeded, so report
			// it here. A nonzero guest exit is not an urunc error.
			sendCommandEvent("ok", "")
			os.Exit(ex.Code)
		case agentproto.TypeError:
			var ae agentproto.Error
			_ = json.Unmarshal(f.Payload, &ae)
			restoreTerminal(origTermios)
			// The message is entirely the guest's, and the error it goes
			// into is printed by fatal(), which filters nothing.
			return fmt.Errorf("guest agent: %s", sanitizeGuestText(ae.Message))
		}
	}
}

// --- guest output filtering --------------------------------------------------
//
// Everything a guest prints lands on the operator's terminal, and a terminal
// is not a display: a handful of escape sequences make it *type*. The reply to
// such a sequence is written into the tty input buffer, where it is read
// either by the stdin pump above (and forwarded straight back into the guest)
// or, once hull has exited, by the operator's shell. That is what turns "the
// sandbox printed something" into "the sandbox read my clipboard" (an OSC 52
// query) or "the sandbox chose a line of text for my shell to see" (set the
// window title with OSC 2, ask for it back with CSI 21 t). `hull exec` puts
// stdin in raw mode with ISIG off while this happens, so the operator cannot
// even interrupt the sequence.
//
// The point of `hull exec` is that a coding-agent TUI works, so the policy is
// deliberately narrow rather than a blanket strip: SGR colour, cursor
// movement, scroll regions, mouse tracking, bracketed paste and the alternate
// screen all pass through untouched. What is dropped is the set of sequences
// that solicit a reply or otherwise reach beyond the display:
//
//   - OSC 52, the clipboard: a query answers with the host clipboard, and a
//     set writes it.
//   - OSC 1337, iTerm2's proprietary channel: file writes, clipboard, and
//     variable reports.
//   - Palette queries -- OSC 4/5/10-21 carrying a "?" field -- which answer
//     with the terminal's colours. Background detection degrades to a default;
//     that is the price of not having a reply channel.
//   - CSI c (device attributes), CSI n (device status, including the cursor
//     position report), CSI > q (XTVERSION), CSI $ p (DECRQM mode reports) and
//     CSI x (DECREQTPARM): identity and status queries, every one of which
//     answers into the input buffer.
//   - CSI t window operations, except the 22/23 title-stack push and pop that
//     TUIs use: 18/19 report the window geometry and 20/21 report the *title*,
//     which is the arbitrary-text injection described above.
//   - ESC Z, the obsolete identify-terminal escape.
//   - DCS, SOS, PM and APC strings as a class. They carry the terminfo query
//     (DCS + q) and the setting query (DCS $ q), but the decisive reason is
//     that tmux and screen forward DCS payloads verbatim to the *outer*
//     terminal, which would bypass this filter entirely. Sixel and the kitty
//     graphics protocol are the cost; a terminal coding agent does not need
//     them.
//   - C1 controls (0x80-0x9F) that appear where a UTF-8 lead byte belongs.
//     0x9B, 0x9D and 0x90 are CSI, OSC and DCS in their 8-bit spelling, so
//     leaving them would leave an unfiltered spelling of everything above.
//     Continuation bytes of a real multi-byte rune are consumed together with
//     their lead byte and are never touched, so UTF-8 output is unchanged.
//
// When the destination is not a terminal -- a pipe, a file, a CI log -- nothing
// is filtered: there is no input buffer to write into, and mangling captured
// output would be a bug of its own.

// guestTerminalWriter wraps f in the filter when f is an interactive terminal,
// and returns f unchanged otherwise.
func guestTerminalWriter(f *os.File) io.Writer {
	if f == nil || !isTerminal(int(f.Fd())) {
		return f
	}
	if terminalFilterDisabled() {
		return f
	}
	return &terminalSanitizer{w: f}
}

// TerminalFilterEnv turns the guest-output filter off.
//
// This exists because the filter broke copy/paste in the field and the only
// remedy was to wait for a new release. A control that can strand somebody with
// no way out is worse than one that can be switched off knowingly: people who
// hit a bug reach for the biggest hammer they have, and if that is "stop using
// hull" the filter has cost more than it saved.
//
// Turning it off gives the guest the operator's terminal: OSC 52 can then read
// the clipboard, DCS passes through tmux to the outer terminal, and a
// cursor-position query types its reply onto the shell's stdin. It is the right
// thing to reach for when the filter is in the way, and the wrong thing to
// leave set.
const TerminalFilterEnv = "HULL_TERMINAL_FILTER"

func terminalFilterDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(TerminalFilterEnv))) {
	case "off", "0", "none", "false":
		return true
	}
	// Anything unrecognised leaves the filter ON. A typo must not be the thing
	// that silently disables it -- the same reasoning as ParseVerifyMode.
	return false
}

// sanitizeGuestTextIfEnabled is sanitizeGuestText, honouring the same switch,
// so a person who turns the filter off is not left with half of it running.
func sanitizeGuestTextIfEnabled(sIn string) string {
	if terminalFilterDisabled() {
		return sIn
	}
	return sanitizeGuestText(sIn)
}

// maxHeldEscape bounds the bytes held back waiting for an escape sequence to
// finish. A sequence split across two frames is normal and must be reassembled;
// an "OSC" with no terminator is not, and must not be able to buffer the
// session's whole output or stall it forever. Past the bound the held bytes are
// dropped and the stream resynchronises on the next byte.
const maxHeldEscape = 4096

// terminalSanitizer applies the policy above to a byte stream. It is stateful
// because escape sequences do not respect write boundaries: a CSI can arrive
// split across two agent frames, and passing its head through and judging only
// its tail would defeat the whole filter.
type terminalSanitizer struct {
	w    io.Writer
	held []byte
}

func (t *terminalSanitizer) Write(p []byte) (int, error) {
	buf := p
	if len(t.held) > 0 {
		buf = append(append([]byte(nil), t.held...), p...)
	}

	out, consumed := sanitizeTerminalBytes(buf)
	rest := buf[consumed:]
	if len(rest) > maxHeldEscape {
		// An unterminated string sequence: drop it rather than grow without
		// bound or block the guest's output behind a terminator that is never
		// coming.
		rest = nil
	}
	t.held = append([]byte(nil), rest...)

	if len(out) > 0 {
		if _, err := t.w.Write(out); err != nil {
			return 0, err
		}
	}
	// The dropped bytes are still "written" as far as the caller is concerned;
	// reporting a short write would only make io.Copy report ErrShortWrite.
	return len(p), nil
}

// sanitizeGuestError makes an error safe to print when its message may carry
// guest-chosen bytes.
//
// Wrapping rather than reformatting keeps errors.Is and errors.As working for
// callers that check the class, while what reaches the terminal is filtered.
func sanitizeGuestError(err error) error {
	if err == nil {
		return nil
	}
	return sanitizedError{err}
}

type sanitizedError struct{ err error }

func (e sanitizedError) Error() string { return sanitizeGuestText(e.err.Error()) }
func (e sanitizedError) Unwrap() error { return e.err }

// sanitizeGuestText makes a guest-chosen string safe to put in a message hull
// prints itself.
//
// The stream filter only covers what goes through guestTerminalWriter. An
// error carrying the agent's own TypeError.Message reaches the terminal
// through fatal(), a compose healthcheck failure reaches it through a wrapped
// error, and `compose top` printed the guest's ps output with fmt.Printf. All
// of those are guest-controlled bytes on an unfiltered path, which is the same
// clipboard-write primitive with a different carrier.
//
// Everything unconsumed at the end is dropped: a trailing fragment is an
// incomplete escape, and there is no next write coming to complete it.
func sanitizeGuestText(sIn string) string {
	out, _ := sanitizeTerminalBytes([]byte(sIn))
	return string(out)
}

// sanitizeTerminalBytes returns the bytes safe to emit and how much of b was
// consumed. The tail that was not consumed is an escape sequence or a UTF-8
// rune that is still incomplete, and the caller must hold it for the next write.
func sanitizeTerminalBytes(b []byte) (out []byte, consumed int) {
	out = make([]byte, 0, len(b))
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == 0x1b:
			n, ok := escapeLen(b[i:])
			if !ok {
				return out, i
			}
			if seq := b[i : i+n]; escapeIsSafe(seq) {
				out = append(out, seq...)
			}
			i += n
		case c >= 0x80 && c < 0xc0:
			// Never a valid UTF-8 lead byte, so this is either a C1 control or
			// a stray continuation byte. The C1 introducers (0x9B is CSI, 0x9D
			// is OSC, 0x90 is DCS) start a sequence, and dropping the
			// introducer alone would leave its parameters to spill onto the
			// screen as text -- so measure the whole sequence and drop that.
			// Nothing legitimate emits 8-bit controls to a UTF-8 terminal, so
			// they go whether or not the 7-bit spelling would have been
			// allowed.
			equiv, isIntroducer := c1Introducer(c)
			if !isIntroducer {
				i++
				break
			}
			// Measured in place. Building "ESC <equiv> <the rest of the
			// buffer>" to hand to escapeLen copied everything after the
			// introducer, for every introducer -- so a guest emitting a
			// megabyte of 0x9b cost time quadratic in the frame and pegged a
			// core for seventeen seconds per MiB. The bytes are never needed:
			// 8-bit controls are dropped whatever they say, so only the length
			// matters.
			n, ok := escapeLenAfterIntro(equiv, b[i+1:])
			if !ok {
				return out, i
			}
			i += n
		case c >= 0xc0:
			if !utf8.FullRune(b[i:]) {
				return out, i
			}
			_, n := utf8.DecodeRune(b[i:])
			out = append(out, b[i:i+n]...)
			i += n
		default:
			out = append(out, c)
			i++
		}
	}
	return out, i
}

// c1Introducer maps an 8-bit C1 control to the byte that follows ESC in its
// 7-bit spelling, so one parser handles both forms.
func c1Introducer(c byte) (byte, bool) {
	switch c {
	case 0x90:
		return 'P', true // DCS
	case 0x98:
		return 'X', true // SOS
	case 0x9b:
		return '[', true // CSI
	case 0x9d:
		return ']', true // OSC
	case 0x9e:
		return '^', true // PM
	case 0x9f:
		return '_', true // APC
	}
	return 0, false
}

// escapeLen measures the escape sequence starting at b[0] (which must be ESC),
// reporting ok=false when the sequence is not complete yet.
func escapeLen(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	switch b[1] {
	case '[':
		// CSI: parameter bytes, then intermediate bytes, then one final byte.
		i := 2
		for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
			i++
		}
		for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
			i++
		}
		if i >= len(b) {
			return 0, false
		}
		if b[i] == 0x1b {
			// ESC is not a final byte. A real terminal treats it as an
			// "anywhere" transition and abandons this sequence, so measuring it
			// as the final byte is how "ESC [ ESC ] 52;c;... BEL" got through:
			// the CSI was judged complete and harmless, and the OSC after it was
			// then read as ordinary text. Stop before the ESC and report the
			// fragment, which escapeIsSafe drops -- the stream resynchronises on
			// the ESC and the OSC is judged as the OSC it is.
			return i, true
		}
		return i + 1, true
	case ']', 'P', 'X', '^', '_':
		// OSC, DCS, SOS, PM, APC: a string terminated by ST or (for OSC) BEL.
		return stringSeqLen(b)
	default:
		if b[1] == 0x1b {
			// ESC cancels whatever it interrupts, which is also how a tmux
			// passthrough smuggles a nested sequence past a naive scanner.
			// Consume just this one byte so the next ESC is judged on its own.
			return 1, true
		}
		if b[1] >= 0x20 && b[1] <= 0x2f {
			// Intermediates then a final byte, e.g. ESC ( B to pick a charset.
			i := 2
			for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
				i++
			}
			if i >= len(b) {
				return 0, false
			}
			if b[i] == 0x1b {
				return i, true // same as the CSI case above
			}
			return i + 1, true
		}
		return 2, true
	}
}

// stringSeqLen measures an ST-terminated string sequence. An embedded ESC that
// does not begin ST ends the measurement early: the stream resynchronises on
// that ESC instead of the sequence swallowing everything after it.
func stringSeqLen(b []byte) (int, bool) {
	for i := 2; i < len(b); i++ {
		switch b[i] {
		case 0x07, 0x9c:
			return i + 1, true
		case 0x1b:
			if i+1 >= len(b) {
				return 0, false
			}
			if b[i+1] == '\\' {
				return i + 2, true
			}
			return i, true
		}
	}
	return 0, false
}

// escapeLenAfterIntro measures a sequence whose introducer has already been
// consumed, without copying the remainder. intro is the byte that would follow
// ESC in the 7-bit spelling; rest is everything after the 8-bit introducer. The
// count it returns includes the introducer.
func escapeLenAfterIntro(intro byte, rest []byte) (int, bool) {
	switch intro {
	case '[':
		i := 0
		for i < len(rest) && rest[i] >= 0x30 && rest[i] <= 0x3f {
			i++
		}
		for i < len(rest) && rest[i] >= 0x20 && rest[i] <= 0x2f {
			i++
		}
		if i >= len(rest) {
			return 0, false
		}
		if rest[i] == 0x1b {
			return i + 1, true
		}
		return i + 2, true
	case ']', 'P', 'X', '^', '_':
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case 0x07, 0x9c:
				return i + 2, true
			case 0x1b:
				if i+1 >= len(rest) {
					return 0, false
				}
				if rest[i+1] == '\\' {
					return i + 3, true
				}
				return i + 1, true
			}
		}
		return 0, false
	default:
		return 1, true
	}
}

// escapeIsSafe decides whether a complete escape sequence may reach the
// terminal. The rationale for each rule is in the policy note above.
func escapeIsSafe(seq []byte) bool {
	if len(seq) < 2 {
		return false
	}
	// A sequence cut short at an ESC is a fragment, and a fragment must never
	// be emitted. The terminal does not stop where our scanner stopped: given
	// "ESC [ 1;2" it keeps consuming, so the next ordinary character we pass
	// through becomes its final byte -- "hello" turns the fragment into
	// ESC[1;2h and sets a mode. Refusing any sequence that does not end in a
	// real final byte is what actually closes this; testing for a trailing ESC
	// does not, because the fragment does not include it.
	if seq[len(seq)-1] == 0x1b {
		return false
	}
	// An 8-bit C1 introducer after ESC.
	//
	// escapeLen treats "ESC <byte>" as a complete two-byte sequence for any
	// byte outside its known set, and 0x90/0x9b/0x9d are outside it -- so
	// "ESC \x9d 52;c;<base64> BEL" was emitted whole, the terminal took the
	// 0x9d as an anywhere-transition into OSC, and everything the filter then
	// passed as ordinary text became the sequence body. Clipboard write,
	// clipboard read and the reporting sequences all went through.
	//
	// "But the terminal is in UTF-8, where C1 bytes are not controls" does not
	// hold either, because this same filter passes ESC % @, which takes xterm
	// out of UTF-8. Nothing legitimate sends ESC followed by a C1 byte.
	if seq[1] >= 0x80 {
		return false
	}
	// ESC % <final> selects a character encoding, and ESC % @ leaves UTF-8.
	// Dropping it is what makes the rule above hold rather than merely usually
	// hold.
	if seq[1] == '%' {
		return false
	}
	// A CSI must end in a proper final byte. This is what stops a fragment
	// being emitted: the terminal does not stop parsing where our scanner
	// stopped, so "ESC [ 1;2" plus the next ordinary character it is handed
	// becomes ESC[1;2h and sets a mode.
	if seq[1] == '[' {
		if last := seq[len(seq)-1]; last < 0x40 || last > 0x7e {
			return false
		}
	}
	// ESC <intermediate> <final> is a different family with a different final
	// range: 0x30-0x7e, not 0x40-0x7e.
	//
	// Applying the CSI range to it was a real regression. ESC ( 0 is ncurses'
	// smacs -- the box-drawing charset -- and its final byte is '0', 0x30. So
	// every border in every TUI (dialog, mc, vim's window splits) rendered as
	// the literal letters lqqqk. ESC # 8, DECALN, went the same way.
	if seq[1] >= 0x20 && seq[1] <= 0x2f {
		if last := seq[len(seq)-1]; last < 0x30 || last > 0x7e {
			return false
		}
	}
	switch seq[1] {
	case '[':
		return csiIsSafe(seq)
	case ']':
		return oscIsSafe(seq)
	case 'P', 'X', '^', '_':
		return false
	case 'Z':
		// DECID: answers with a device attributes string.
		return false
	case '\\':
		// A string terminator with no string in front of it: it draws
		// nothing, and it is what is left over when a smuggled sequence is
		// unpicked. Dropping it keeps the output to bytes that render.
		return false
	}
	return true
}

func csiIsSafe(seq []byte) bool {
	body := seq[2:]
	if len(body) == 0 {
		return false
	}
	final := body[len(body)-1]
	rest := body[:len(body)-1]
	// Intermediate bytes sit between the parameters and the final byte.
	k := len(rest)
	for k > 0 && rest[k-1] >= 0x20 && rest[k-1] <= 0x2f {
		k--
	}
	inter := string(rest[k:])
	params := string(rest[:k])

	switch final {
	case 'c':
		// Device attributes, primary/secondary/tertiary.
		return false
	case 'n':
		// Device status report, including the cursor position report.
		return false
	case 'x':
		// DECREQTPARM asks for the terminal's parameters; DECSACE, which only
		// sets an attribute rectangle, carries an intermediate byte.
		return inter != ""
	case 'q':
		// CSI > q (XTVERSION) answers with the terminal's name and version.
		// CSI Ps SP q (DECSCUSR) just picks a cursor shape.
		return !strings.HasPrefix(params, ">")
	case 'p':
		// CSI Ps $ p and CSI ? Ps $ p (DECRQM) answer with a mode's state.
		return inter != "$"
	case 't':
		// Window operations. 22 and 23 push and pop the title stack, reply with
		// nothing, and are used by ordinary TUIs; everything else here either
		// reports geometry, reports the title, or moves the operator's window.
		return params == "22" || params == "23" ||
			strings.HasPrefix(params, "22;") || strings.HasPrefix(params, "23;")
	}
	return true
}

// oscClipboardIsQuery reports whether an OSC 52 payload asks the terminal to
// report the clipboard rather than to set it.
//
// Shape: OSC 52 ; <selector> ; <data>. The data field is "?" for a query and
// base64 for a set. A malformed or absent data field is treated as a query,
// because refusing something we cannot parse is the safe direction here: a set
// that is wrongly refused costs a copy, a query that is wrongly allowed costs
// whatever was on the clipboard.
func oscClipboardIsQuery(fields []string) bool {
	if len(fields) < 3 {
		return true
	}
	data := strings.TrimSpace(fields[len(fields)-1])
	return data == "?" || data == ""
}

// oscQueryable are the OSC codes that set colours and accept "?" as a query,
// answering with the current value.
var oscQueryable = map[string]bool{
	"4": true, "5": true, "10": true, "11": true, "12": true, "13": true,
	"14": true, "15": true, "16": true, "17": true, "18": true, "19": true,
	"20": true, "21": true,
}

func oscIsSafe(seq []byte) bool {
	body := seq[2:]
	switch {
	case len(body) >= 1 && (body[len(body)-1] == 0x07 || body[len(body)-1] == 0x9c):
		body = body[:len(body)-1]
	case len(body) >= 2 && body[len(body)-2] == 0x1b && body[len(body)-1] == '\\':
		body = body[:len(body)-2]
	default:
		// Unterminated: either a truncated write this filter could not
		// reassemble or an attempt to hide what follows inside the string.
		return false
	}

	fields := strings.Split(string(body), ";")
	// Compared as a number, not as text: terminals parse the code numerically,
	// so "052" and "0052" are OSC 52 to them while being different strings to
	// us. An unparseable code is refused rather than allowed -- if we cannot
	// say what it is, we cannot say it is safe.
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return false
	}
	switch code {
	case 52:
		// Clipboard. A SET is how every TUI copies -- vim, tmux, an agent's own
		// copy command -- and blocking it broke copy/paste for real users, which
		// is a bad trade for the threat it answers: the worst a set does is
		// overwrite what you were about to paste.
		//
		// A QUERY is the dangerous one and stays blocked. "ESC ] 52 ; c ; ? BEL"
		// makes the TERMINAL reply with the clipboard's current contents,
		// delivered into the guest's stdin -- so whatever the operator last
		// copied, which on this machine is as likely as not a token or a
		// password, is handed to the sandbox. That is exfiltration, not
		// annoyance, and no legitimate program inside a sandbox needs to read
		// the host's clipboard.
		//
		// The selector field may name several targets (c, p, s, 0-7) and the
		// data field is what distinguishes the two: "?" is the query, base64 is
		// a set. Anything that is not a plain "?" is treated as a set.
		return !oscClipboardIsQuery(fields)
	case 1337:
		// iTerm2's channel: file writes, clipboard, variable reports.
		return false
	}
	if oscQueryable[strconv.Itoa(code)] {
		for _, f := range fields[1:] {
			if f == "?" {
				return false
			}
		}
	}
	return true
}
