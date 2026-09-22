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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brig-sh/hull/internal/netgw"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
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
		Description: strings.TrimSpace(`
Egress filtering is off unless --egress-default is given. With it, every
connection a guest opens to the outside world is checked: deny rules beat
allow rules beat the default.

A cidr rule is matched against the address the connection is opened to. A
host rule is a glob matched against the name the guest asks this gateway's
resolver for, and is enforced on the addresses that resolver hands back.

Under --egress-default deny the resolver answers only names an allow glob
covers, and refuses the rest. A guest can then reach nothing it did not
resolve here, which is also why traffic sent straight to an IP,
DNS-over-HTTPS and DNS-over-TLS do not get out.

Under --egress-default allow, a host deny is best effort: traffic sent
straight to an IP never asks for a name, so it is not covered. A cidr deny
is airtight.

A rule naming one host is re-resolved every --egress-refresh, so a name whose
addresses rotate keeps working for a guest that cached one of them. A glob
cannot be resolved ahead of a query and stays driven by what guests ask for.

Rules are checked at startup and the gateway refuses to start on one it
cannot parse, rather than enforce part of what was asked for.

A policy belongs to this gateway, not to a guest. Every microVM behind it
answers to all of these rules, and no rule can name one member. A sandbox
that needs an egress policy of its own needs a gateway of its own, so run
one gateway per sandbox.

Two things this does not do. It does not separate one guest from another:
guests share a switch, and traffic between them never reaches the filter.
And --forward is ingress, not egress; it exposes a guest port on the host
and no egress rule applies to it.

With --api the gateway serves /forwards on that socket, which publishes a
guest port and withdraws it again while the guest is running. GET lists what
is published, POST takes {protocol, local, remote}, and DELETE takes protocol
and local as query parameters. This is the only part of a running gateway
that can change; the network and the rules are read once at startup.

IPv6 is dropped outright. The netstack does not forward IPv6 yet, so no
guest reaches the outside world over it, with or without a policy.`),
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Required: true, Usage: "control socket path (required)"},
			&cli.StringFlag{Name: "api", Usage: "HTTP API socket path (probe, leases and the /forwards endpoint)"},
			&cli.StringFlag{Name: "qemu-socket", Usage: "unix socket for the QEMU stream netdev, used by both the QEMU and HVI backends"},
			&cli.StringFlag{Name: "subnet", Value: "10.87.0.0/24", Usage: "virtual subnet CIDR"},
			&cli.StringFlag{Name: "gateway-ip", Value: "10.87.0.1", Usage: "gateway IP on the subnet"},
			&cli.StringSliceFlag{Name: "forward", Usage: "host port forward, hostaddr:port=guestip:port (repeatable)"},
			&cli.StringSliceFlag{Name: "host", Usage: "static DNS A record served by the gateway, name=ip (repeatable)"},
			&cli.StringFlag{Name: "egress-default", Usage: "verdict for a connection no egress rule matches, allow or deny; without it egress is unfiltered"},
			&cli.StringSliceFlag{Name: "egress-allow", Usage: "egress allow rule, host=<glob> or cidr=<cidr> (repeatable)"},
			&cli.StringSliceFlag{Name: "egress-deny", Usage: "egress deny rule, host=<glob> or cidr=<cidr> (repeatable)"},
			&cli.DurationFlag{Name: "egress-refresh", Value: egressRefreshDefault, Usage: "how often to re-resolve the named hosts in the egress rules, so a name whose addresses rotate keeps working; 0 disables"},
			&cli.StringFlag{Name: "project", Usage: "compose project to supervise (enables restart policies)"},
			&cli.DurationFlag{Name: "supervise-interval", Value: supervisorPollInterval, Usage: "liveness poll interval of the supervision loop"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Parse the policy before anything else starts: a rule the
			// gateway cannot make sense of has to stop it, not degrade it.
			policy, err := netgw.ParseEgressPolicy(cmd.String("egress-default"), cmd.StringSlice("egress-allow"), cmd.StringSlice("egress-deny"))
			if err != nil {
				return err
			}
			var forwards []netgw.Forward
			for _, f := range cmd.StringSlice("forward") {
				forward, err := netgw.ParseForward(f)
				if err != nil {
					return err
				}
				forwards = append(forwards, forward)
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
			return runGateway(ctx, cmd.String("socket"), cmd.String("api"), cmd.String("qemu-socket"), cmd.String("subnet"), cmd.String("gateway-ip"), forwards, cmd.StringSlice("host"), policy, cmd.Duration("egress-refresh"), sup)
		},
	}
}

// gatewayShutdownGrace bounds how long the daemon waits for an in-flight
// restart to finish before exiting anyway. A restart is one VM boot, so a
// few seconds beyond the usual boot time suffices; exceeding it is reported
// rather than waited out, because the caller has its own deadline.
const gatewayShutdownGrace = 20 * time.Second

// egressRefreshDefault is how often a named host in the egress rules is
// re-resolved. Short enough to follow a rotating record set well inside the
// window a guest might hold an address for, long enough that a gateway with a
// handful of rules is not a source of DNS traffic worth noticing.
const egressRefreshDefault = 30 * time.Second

func runGateway(ctx context.Context, sockPath, apiPath, qemuSockPath, subnet, gatewayIP string, forwards []netgw.Forward, hosts []string, policy *netgw.Policy, egressRefresh time.Duration, sup *supervisor) error {
	// Serve service names from the gateway's DNS in addition to the
	// /etc/hosts injection, so images that bypass /etc/hosts keep working.
	// Non-matching queries fall through to the host resolver.
	//
	// This reaches a guest only if the guest resolves against the gateway.
	// Unikernels placed by urunc do not: NetDevParams carries no DNS field,
	// so urunc renders a fixed 8.8.8.8 and the records below are invisible
	// to them. run warns when it sees that rendering. Giving them these
	// records needs a DNS field on urunc's NetDevParams defaulting to the
	// gateway address.
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

	vn, err := netgw.New(netgw.Config{
		MTU:               1500,
		Subnet:            subnet,
		GatewayIP:         gatewayIP,
		GatewayMacAddress: "5a:94:ef:e4:0c:dd",
		Forwards:          forwards,
		DNSZones:          dns,
		Egress:            policy,
		EgressRefresh:     egressRefresh,
	})
	if err != nil {
		return fmt.Errorf("failed to create virtual network: %w", err)
	}

	// Keep the named hosts in the rules resolved for as long as the gateway
	// runs, so the policy follows a name whose addresses rotate.
	netCtx, stopNet := context.WithCancel(ctx)
	defer stopNet()
	vn.Start(netCtx)

	l, err := claimUnixSocket(sockPath)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close(); _ = os.Remove(sockPath) }()
	log.Infof("network-gateway on %s (subnet %s, gw %s, %d forwards, %s)", sockPath, subnet, gatewayIP, len(forwards), policy.Summary())

	// Probe API: the gateway is the only process that can dial into the
	// virtual network, so TCP healthchecks run here.
	if apiPath != "" {
		apiL, err := claimOwnerOnlyUnixSocket(apiPath)
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
		// Leases, as gvisor-tap-vsock's own mux serves them: the address a
		// guest was given, keyed by address. A runtime that configured a
		// guest statically reads this to find out whether the guest agreed.
		mux.HandleFunc("/leases", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(vn.Leases())
		})
		mux.Handle("/forwards", forwardsHandler(vn))
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
			// The goroutine above closes the listener to unblock this Accept,
			// so a closed listener after a signal is the shutdown working.
			// Returned as an error it leaves every clean stop reporting
			// "use of closed network connection" and a non-zero exit.
			if sigCtx.Err() != nil && errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handleGatewayMember(ctx, vn, conn.(*net.UnixConn))
	}
}

// handleGatewayMember receives the member's datagram socket via SCM_RIGHTS
// and serves it as a vfkit-protocol endpoint (one Ethernet frame per
// datagram) until the member disappears.
func handleGatewayMember(ctx context.Context, vn *netgw.Network, conn *net.UnixConn) {
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

// claimUnixSocket listens on path, refusing to steal it from a live process.
// Three dial outcomes are conclusive, and only these three:
//
//	nil            something answered, so another gateway owns the path
//	ECONNREFUSED   the socket file outlived its listener, so it is stale
//	ENOENT         nothing is at the path, so there is nothing to remove
//
// Every other outcome means only that we could not reach the socket, which
// is not the same as proof that nobody is listening — EACCES because a
// directory on the way is not searchable, or because the socket's own mode
// was changed; ENOTSOCK because something that is not a socket sits at the
// path; a dial that times out. A live gateway we cannot dial looks exactly
// like a stale file, so an outcome we cannot classify refuses instead of
// removing. Refusing is recoverable and says why; unlinking the socket a
// running gateway is serving is not, and it detaches that gateway from its
// own address while it keeps running.
//
// The honest limit of dialling: on BSD a full accept queue also refuses, so
// a wedged-but-live gateway can still read as stale. Narrowing "unreachable"
// to "refused" does not make dialling a sound liveness test.
func claimUnixSocket(path string) (net.Listener, error) {
	if err := clearStaleUnixSocket("gateway", path,
		"move the store to a shorter path (--store-dir) or use a shorter instance name"); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", path, err)
	}
	return l, nil
}

// claimOwnerOnlyUnixSocket claims path the way claimUnixSocket does, and
// leaves it readable and writable by its owner alone.
//
// Only the API socket takes this. It publishes host ports, so its mode is the
// gate on who may, and net.Listen leaves whatever the umask leaves -- under
// umask 000, any user on the machine. The control and QEMU sockets are what a
// member connects to, and what may reach those is not this function's to
// narrow.
//
// The socket is bound in a directory of this gateway's own, given its mode
// there, and hard-linked to path. Listening on path and chmodding after would
// leave a window where the socket answers at the name callers use with the
// mode the umask left.
//
// A crash between making that directory and removing it leaves one behind,
// three syscalls wide. Nothing sweeps it: a sweep would have to tell debris
// from the staging directory of a gateway claiming a path right now, and
// removing one of those breaks a claim that was going to succeed.
//
// Two things the staging name has to be, both learned by getting them wrong.
// It is a fresh directory rather than a name beside path, because a name
// derived from path can be a socket another gateway is serving, and claiming
// it would unlink a live one. And it is linked rather than renamed, because
// rename replaces whatever it lands on: a gateway that bound path while this
// one was staging would be silently displaced, where a plain Listen would
// have refused. link fails with EEXIST instead, so that gateway keeps it.
//
// # Errors
//
// Returns an error when another process holds path, when path or the staged
// name is too long for a socket, or when the socket cannot be bound,
// restricted or linked.
func claimOwnerOnlyUnixSocket(path string) (net.Listener, error) {
	if err := clearStaleUnixSocket("gateway API", path,
		"pass a shorter --api path"); err != nil {
		return nil, err
	}
	// Beside path rather than in TMPDIR: link works within one filesystem.
	parent := filepath.Dir(path)
	if err := checkStagedSocketPath(path, parent); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, stagingPrefix)
	if err != nil {
		// The directory, not the name inside it that MkdirTemp invented and
		// the caller cannot act on.
		return nil, fmt.Errorf("cannot stage the gateway API socket in %s: %w\n"+
			"pass an --api path in a directory this user can write", parent, bareErr(err))
	}
	defer func() { _ = os.RemoveAll(dir) }()

	staging := filepath.Join(dir, stagedSocketName)
	l, err := net.Listen("unix", staging)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", path, err)
	}
	if err := os.Chmod(staging, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("failed to restrict %s: %w", path, err)
	}
	if err := os.Link(staging, path); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("failed to name %s: %w", path, err)
	}
	return l, nil
}

// stagingPrefix names the directory the API socket is bound in before it is
// linked to the path a caller gave.
const stagingPrefix = ".hull-api"

// stagedSocketName is the socket's name inside that directory.
const stagedSocketName = "s"

// stagedSocketOverhead is what staging adds to a socket's directory: the
// prefix, the ten digits MkdirTemp appends, the socket's own name and the two
// separators.
const stagedSocketOverhead = len(stagingPrefix) + 10 + len(stagedSocketName) + 2

// checkStagedSocketPath rejects an API socket path whose directory leaves no
// room to stage it.
//
// The refusal names the path the caller gave and the length staging needs.
// Naming the staged path instead reports a directory this gateway invented
// and has already removed, and a byte count for a string the caller never
// typed.
//
// # Errors
//
// Returns an error when the staged path would exceed what the kernel binds.
func checkStagedSocketPath(path, parent string) error {
	staged := len(parent) + stagedSocketOverhead
	if staged <= unixSocketPathMax {
		return nil
	}
	return fmt.Errorf("gateway API socket path is %d bytes and staging it needs %d, "+
		"over the %d a unix socket takes on this OS: %s\n"+
		"pass an --api path in a shorter directory",
		len(path), staged, unixSocketPathMax, path)
}

// bareErr strips the path a syscall wrapper repeats back, leaving the reason.
// The paths in this file are ones the caller did not choose, so repeating
// them buries the part they can act on.
func bareErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// clearStaleUnixSocket reports whether path is free to bind, and removes the
// socket file when the last listener is gone.
//
// # Errors
//
// Returns an error when another process is listening, and when the outcome
// cannot be classified.
func clearStaleUnixSocket(what, path, remedy string) error {
	if err := checkUnixSocketPathRemedy(what, path, remedy); err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("socket %s is in use by a running gateway", path)
	case errors.Is(err, syscall.ECONNREFUSED):
		// Nobody is listening: the file the last gateway left behind is
		// ours to clear.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("failed to remove stale socket %s: %w", path, rmErr)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Nothing at the path. net.Listen creates it.
	default:
		return fmt.Errorf("cannot tell whether socket %s is in use: %w\n"+
			"refusing to remove it; if no gateway is running, remove the socket by hand", path, err)
	}
	return nil
}

// qemuGatewaySock derives the QEMU stream socket path from the control
// socket path — the one place this convention lives.
func qemuGatewaySock(controlSock string) string {
	return controlSock + ".qemu"
}

// gatewayAPISock names the HTTP API socket beside a gateway's control socket.
// compose builds both from the project name, so the mapping is that pair's;
// a control socket named otherwise gets the suffix appended.
func gatewayAPISock(controlSock string) string {
	if rest, ok := strings.CutSuffix(controlSock, ".gateway.sock"); ok {
		return rest + ".api.sock"
	}
	return controlSock + ".api"
}

// gatewayLeases reads the gateway's DHCP lease table: the address a guest was
// given, keyed by address. Returns nothing when the gateway serves no API
// socket, which is the common case for a gateway hull did not start.
func gatewayLeases(apiSock string) (map[string]string, error) {
	if apiSock == "" {
		return nil, errors.New("no gateway API socket")
	}
	// Stat first. Without it a gateway started without --api costs the caller
	// its whole polling budget on connections that can only fail.
	if _, err := os.Stat(apiSock); err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get("http://gateway/leases")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var leases map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&leases); err != nil {
		return nil, err
	}
	return leases, nil
}

// leaseFor returns the address the gateway gave mac, if it gave one.
func leaseFor(leases map[string]string, mac string) (string, bool) {
	for ip, holder := range leases {
		if strings.EqualFold(holder, mac) {
			return ip, true
		}
	}
	return "", false
}

// leaseMismatchMessage describes a guest that took a DHCP address after the
// runtime configured a static one. A configured guest never asks, so a lease
// for its MAC is the guest overriding what it was told, and every host-side
// record then names an address the guest does not answer on. Unikraft images
// built without CONFIG_LIBUKNETDEV_EINFO_LIBPARAM fail exactly this way.
func leaseMismatchMessage(mac, leased, configured string) string {
	return fmt.Sprintf("guest %s took %s from the gateway although it was configured with %s; "+
		"host-side records name %s, which the guest does not answer on "+
		"(a Unikraft image built without CONFIG_LIBUKNETDEV_EINFO_LIBPARAM ignores netdev.ip)",
		mac, leased, configured, configured)
}

// warnOnLeaseMismatch polls the gateway until the guest has asked for an
// address or budget runs out, and warns if it asked for a different one.
//
// A healthy guest never asks, so the whole budget is spent on the good path.
// Callers that cannot afford that pass a zero budget, which makes it a single
// probe, or run it where the wait is already being paid.
func warnOnLeaseMismatch(apiSock, mac, configured string, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for {
		if leases, err := gatewayLeases(apiSock); err == nil {
			if ip, ok := leaseFor(leases, mac); ok && ip != configured {
				log.Warn(leaseMismatchMessage(mac, ip, configured))
				return
			}
		} else if os.IsNotExist(err) {
			return // no API socket: nothing to read, now or later
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
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

// forwardsHandler serves the gateway's host port forwards: what is published,
// and the two verbs that change it.
//
//	GET    /forwards                              every forward, as JSON
//	POST   /forwards  {protocol, local, remote}   publish one
//	DELETE /forwards?protocol=tcp&local=addr:port withdraw one
//
// A forward is the one thing about a running gateway that can change. The
// network and the egress rules are read once at startup, so a caller that
// wants either of those restarts the gateway; a caller that wants a port
// published does not, because restarting drops every member of the network
// and the guest whose port this is would be one of them.
//
// Publishing a port is widening the boundary of the sandbox behind it, so the
// endpoint reports what it did rather than only that it worked: the forward
// comes back on the response, and the conflict cases are told apart. A local
// address already taken is 409, one nothing holds is 404, and a request the
// gateway cannot parse is 400.
func forwardsHandler(vn forwarder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(vn.Forwards())
		case http.MethodPost:
			var f netgw.Forward
			if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			installed, err := vn.Expose(f)
			if err != nil {
				http.Error(w, err.Error(), forwardStatus(err))
				return
			}
			// What was installed, not what was asked for: a request that left
			// the protocol out is answered with the tcp it actually got, so
			// this and a later GET cannot describe one forward two ways.
			log.Infof("network-gateway: published %s", installed)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(installed)
		case http.MethodDelete:
			gone, err := vn.Unexpose(r.URL.Query().Get("protocol"), r.URL.Query().Get("local"))
			if err != nil {
				http.Error(w, err.Error(), forwardStatus(err))
				return
			}
			log.Infof("network-gateway: withdrew %s", gone)
			_ = json.NewEncoder(w).Encode(gone)
		default:
			http.Error(w, "use GET, POST or DELETE", http.StatusMethodNotAllowed)
		}
	})
}

// forwarder is what the endpoint needs of a gateway's network: the three
// operations on its host port forwards, and nothing else.
//
// An interface rather than *netgw.Network, so that a test of this handler does
// not stand up a netstack. Building one starts the DHCP and DNS servers and
// their goroutines, and those live as long as the test binary -- enough of
// them in one package to perturb a test elsewhere in it that measures elapsed
// time, which is what happened to the sampler test.
type forwarder interface {
	Expose(netgw.Forward) (netgw.Forward, error)
	Unexpose(protocol, local string) (netgw.Forward, error)
	Forwards() []netgw.Forward
}

// forwardStatus maps a forwarding failure to the status a caller can act on.
// Anything that is neither a conflict nor a miss is the gateway's own
// failure to install a forward it accepted.
func forwardStatus(err error) int {
	switch {
	case errors.Is(err, netgw.ErrForwardExists):
		return http.StatusConflict
	case errors.Is(err, netgw.ErrForwardNotFound):
		return http.StatusNotFound
	case errors.Is(err, netgw.ErrForwardInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
