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
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"
)

// networkGatewayCommand runs a user-mode network gateway (gvisor netstack)
// that gives each guest its single NIC: service-to-service switching, NAT
// egress to the outside world, DNS, and host-side port forwarding — all in
// one userspace process, with no entitlements and no root.
//
// This exists because Apple's NAT attachment isolates guests from each other
// (kernel bridge PRIVATE ports) and bridged mode needs the Apple-managed
// com.apple.vm.networking entitlement.
//
// Members connect to a UNIX stream control socket and pass one end of a
// socketpair(AF_UNIX, SOCK_DGRAM) via SCM_RIGHTS; each datagram is one
// Ethernet frame (the vfkit transport). The member's port lives until the
// control connection closes, which ties it to the VMM process lifetime.
//
// With --project it is also the project supervisor: the same process already
// has the right lifetime for restart policies, so it polls the project's
// instances and re-runs the ones that disappeared.
func networkGatewayCommand() *cli.Command {
	return &cli.Command{
		Name:   "network-gateway",
		Usage:  "run the user-mode network gateway (used by compose)",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Required: true, Usage: "control socket path"},
			&cli.StringFlag{Name: "api", Usage: "HTTP API socket path (probe endpoint)"},
			&cli.StringFlag{Name: "qemu-socket", Usage: "unix socket for QEMU stream netdev members"},
			&cli.StringFlag{Name: "subnet", Value: "10.87.0.0/24", Usage: "virtual subnet CIDR"},
			&cli.StringFlag{Name: "gateway-ip", Value: "10.87.0.1", Usage: "gateway IP on the subnet"},
			&cli.StringSliceFlag{Name: "forward", Usage: "host port forward, hostaddr:port=guestip:port (repeatable)"},
			&cli.StringSliceFlag{Name: "host", Usage: "static DNS A record served by the gateway, name=ip (repeatable)"},
			&cli.StringFlag{Name: "project", Usage: "compose project to supervise (enables restart policies)"},
			&cli.DurationFlag{Name: "supervise-interval", Value: supervisorPollInterval, Usage: "liveness poll interval of the supervision loop"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			forwards := map[string]string{}
			for _, f := range cmd.StringSlice("forward") {
				parts := strings.SplitN(f, "=", 2)
				if len(parts) != 2 {
					return fmt.Errorf("invalid --forward %q, expected hostaddr:port=guestip:port", f)
				}
				forwards[parts[0]] = parts[1]
			}
			// Restart policies are honored only for a project: a gateway
			// started by hand has no project state to supervise.
			var sup *supervisor
			if project := cmd.String("project"); project != "" {
				s, err := newProjectSupervisor(cmd, project, cmd.Duration("supervise-interval"))
				if err != nil {
					return fmt.Errorf("failed to start supervision of project %q: %w", project, err)
				}
				sup = s
			}
			return runGateway(ctx, cmd.String("socket"), cmd.String("api"), cmd.String("qemu-socket"), cmd.String("subnet"), cmd.String("gateway-ip"), forwards, cmd.StringSlice("host"), sup)
		},
	}
}

// gatewayShutdownGrace bounds how long the daemon waits for an in-flight
// restart to finish before exiting anyway. A restart is one VM boot, so a
// few seconds beyond the usual boot time suffices; exceeding it is reported
// rather than waited out, because the caller has its own deadline.
const gatewayShutdownGrace = 20 * time.Second

func runGateway(ctx context.Context, sockPath, apiPath, qemuSockPath, subnet, gatewayIP string, forwards map[string]string, hosts []string, sup *supervisor) error {
	// Serve service names from the gateway's DNS in addition to the
	// /etc/hosts injection: guests that resolve via plain DNS (unikernel
	// flavors without an nsswitch) get name resolution for free, and images
	// that bypass /etc/hosts keep working. Non-matching queries fall through
	// to the host resolver.
	var records []gvntypes.Record
	for _, h := range hosts {
		parts := strings.SplitN(h, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid --host %q, expected name=ip", h)
		}
		ip := net.ParseIP(parts[1])
		if ip == nil {
			return fmt.Errorf("invalid --host %q: %q is not an IP", h, parts[1])
		}
		records = append(records, gvntypes.Record{Name: parts[0], IP: ip})
	}
	dns := []gvntypes.Zone{}
	if len(records) > 0 {
		dns = append(dns, gvntypes.Zone{Name: ".", Records: records})
	}

	config := &gvntypes.Configuration{
		Debug:             false,
		MTU:               1500,
		Subnet:            subnet,
		GatewayIP:         gatewayIP,
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		Protocol:          gvntypes.VfkitProtocol,
		Forwards:          forwards,
		DNS:               dns,
	}
	vn, err := virtualnetwork.New(config)
	if err != nil {
		return fmt.Errorf("failed to create virtual network: %w", err)
	}

	l, err := claimUnixSocket(sockPath)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close(); _ = os.Remove(sockPath) }()
	log.Infof("network-gateway on %s (subnet %s, gw %s, %d forwards)", sockPath, subnet, gatewayIP, len(forwards))

	// Probe API: the gateway is the only process that can dial into the
	// virtual network, so TCP healthchecks run here.
	if apiPath != "" {
		apiL, err := claimUnixSocket(apiPath)
		if err != nil {
			return err
		}
		defer func() { _ = apiL.Close(); _ = os.Remove(apiPath) }()
		mux := http.NewServeMux()
		mux.HandleFunc("/probe/tcp", func(w http.ResponseWriter, r *http.Request) {
			addr := r.URL.Query().Get("addr")
			if addr == "" {
				http.Error(w, "missing addr", http.StatusBadRequest)
				return
			}
			dialCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			conn, err := vn.DialContextTCP(dialCtx, addr)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			_ = conn.Close()
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(apiL) }()
	}

	// QEMU members connect directly with a stream netdev
	// (-netdev stream,addr.type=unix,addr.path=...) — length-prefixed
	// Ethernet frames, no fd passing; the member lives until QEMU closes
	// the connection.
	// Note the lifetime asymmetry with vfkit members: a vfkit member's port
	// is pinned by the control-connection fd held by its VMM process, while
	// a QEMU stream member is tracked only by connection state — a wedged
	// QEMU holding the connection open keeps a dead member in the switch.
	if qemuSockPath != "" {
		qemuL, err := claimUnixSocket(qemuSockPath)
		if err != nil {
			return err
		}
		defer func() { _ = qemuL.Close(); _ = os.Remove(qemuSockPath) }()
		go func() {
			for {
				conn, err := qemuL.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					log.Debug("gateway: qemu member joined")
					if err := vn.AcceptQemu(ctx, c); err != nil && ctx.Err() == nil {
						log.WithError(err).Debug("gateway: qemu member ended")
					}
					log.Debug("gateway: qemu member left")
				}(conn)
			}
		}()
	}

	// Supervision starts only once the switch is serving: a service restarted
	// before that would have nothing to join.
	//
	// Shutdown runs through one ctx for both the supervised and unsupervised
	// cases, so runGateway's defers (the probe API socket among them, which
	// nothing else removes) always run, and a second TERM is not swallowed by
	// a handler blocked in the grace.
	sigCtx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	if sup != nil {
		// A restart mid-flight must not be abandoned: the child 'run' is
		// setpgid, so it would survive and leave an orphan VM that teardown
		// can no longer see, and its deterministic instance name would then
		// make the project's next 'up' fail on a duplicate. Cancel the
		// supervisor and wait for the boot it started, so its own post-boot
		// undo runs; on expiry, kill the child rather than leave it running.
		done := make(chan struct{})
		log.Infof("network-gateway: supervising project instances every %s", sup.interval)
		go func() {
			defer close(done)
			sup.run(sigCtx)
		}()
		defer func() {
			select {
			case <-done:
			case <-time.After(gatewayShutdownGrace):
				log.Warn("network-gateway: supervision did not stop within the grace period; killing the restart in flight")
				sup.killInFlight()
			}
		}()
	}

	go func() {
		<-sigCtx.Done()
		// Unblock Accept so the loop below returns and every defer runs.
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go handleGatewayMember(ctx, vn, conn.(*net.UnixConn))
	}
}

// handleGatewayMember receives the member's datagram socket via SCM_RIGHTS
// and serves it as a vfkit-protocol endpoint (one Ethernet frame per
// datagram) until the member disappears.
func handleGatewayMember(ctx context.Context, vn *virtualnetwork.VirtualNetwork, conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()

	dataFD, err := recvFD(conn)
	if err != nil {
		log.WithError(err).Warn("gateway: failed to receive member fd")
		return
	}
	f := os.NewFile(uintptr(dataFD), "vm-port")
	defer func() { _ = f.Close() }()
	fileConn, err := net.FileConn(f)
	if err != nil {
		log.WithError(err).Warn("gateway: fd is not a socket")
		return
	}
	defer func() { _ = fileConn.Close() }()
	log.Debug("gateway: member joined")

	memberCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The control connection closing (VMM exit) tears the member down.
	go func() {
		one := make([]byte, 1)
		for {
			if _, err := conn.Read(one); err != nil {
				break
			}
		}
		cancel()
	}()

	if err := vn.AcceptVfkit(memberCtx, fileConn); err != nil && memberCtx.Err() == nil {
		log.WithError(err).Debug("gateway: member connection ended")
	}
	log.Debug("gateway: member left")
}

// recvFD reads one file descriptor passed via SCM_RIGHTS.
func recvFD(conn *net.UnixConn) (int, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, err
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return -1, fmt.Errorf("control message truncated")
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return -1, fmt.Errorf("no control message: %v", err)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return -1, fmt.Errorf("no fd in control message: %v", err)
	}
	syscall.CloseOnExec(fds[0])
	return fds[0], nil
}

// claimUnixSocket listens on path, refusing to steal it from a live process:
// if something answers a dial, another gateway owns it; only a refused
// connection marks the socket file as stale and safe to remove.
func claimUnixSocket(path string) (net.Listener, error) {
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socket %s is in use by a running gateway", path)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", path, err)
	}
	return l, nil
}

// qemuGatewaySock derives the QEMU stream socket path from the control
// socket path — the one place this convention lives.
func qemuGatewaySock(controlSock string) string {
	return controlSock + ".qemu"
}

// joinGateway connects to a gateway's control socket, creates the datagram
// socketpair, sends one end to the gateway, and returns the local end plus
// the control connection (which must stay open for the member's lifetime).
func joinGateway(sockPath string) (*os.File, *net.UnixConn, error) {
	raddr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.DialUnix("unix", nil, raddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reach network gateway at %s: %w", sockPath, err)
	}

	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	// Generous buffers: each datagram is a full Ethernet frame. CLOEXEC so
	// nothing leaks into unrelated children — the VMM inherits its end
	// explicitly via ExtraFiles, which re-dups without the flag.
	for _, fd := range pair {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4<<20)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
		syscall.CloseOnExec(fd)
	}

	rights := unix.UnixRights(pair[1])
	if _, _, err := conn.WriteMsgUnix([]byte{0}, rights, nil); err != nil {
		_ = conn.Close()
		_ = syscall.Close(pair[0])
		_ = syscall.Close(pair[1])
		return nil, nil, fmt.Errorf("failed to send fd to gateway: %w", err)
	}
	// The gateway dup'ed the fd on receive; close our copy of its end.
	_ = syscall.Close(pair[1])

	return os.NewFile(uintptr(pair[0]), "vm-net"), conn, nil
}
