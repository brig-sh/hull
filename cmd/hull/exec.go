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

//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

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
	// This is how a detached VM (a urunc-claude sandbox) reports metrics:
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

	// Frame loop: relay output until the session exits.
	for {
		f, rerr := agentproto.ReadFrame(conn)
		if rerr != nil {
			restoreTerminal(origTermios)
			return fmt.Errorf("connection to guest agent lost: %w", rerr)
		}
		switch f.Type {
		case agentproto.TypeStdout:
			_, _ = os.Stdout.Write(f.Payload)
		case agentproto.TypeStderr:
			_, _ = os.Stderr.Write(f.Payload)
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
			return fmt.Errorf("guest agent: %s", ae.Message)
		}
	}
}
