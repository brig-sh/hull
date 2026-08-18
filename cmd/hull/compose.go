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
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/brig-sh/hull/internal/compose"
	"github.com/brig-sh/hull/internal/telemetry"
	"github.com/brig-sh/hull/pkg/store"
)

// namedVolumeRe is compose-spec's own JSON-schema pattern for a top-level
// volume name (schema/compose-spec.json's "volumes" patternProperties key):
// compose-go enforces this before resolveServiceVolume ever sees a Source,
// so nothing in this package's own code validates against it anymore. It
// stays only as FuzzResolveServiceVolume's domain guard (fuzz_test.go) —
// kept matching the schema exactly, rather than the tighter shape a
// hand-rolled check once used, so the fuzz target's input space is what
// the loader actually accepts, not a narrower guess at it.
var namedVolumeRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Profile names are deliberately not validated. The spec's prose gives a
// shape ([a-zA-Z0-9][a-zA-Z0-9_.-]+), but neither the JSON schema nor
// compose-go enforces it, so rejecting a name here would fail a file that
// docker starts. A name that matches nothing is caught by the warning in
// warnUnmatchedProfiles instead. Nothing joins a profile name onto a path.

// allProfiles activates every profile the file declares, docker's '*'.
const allProfiles = "*"

// volumesRoot is where managed named volumes live under the store. It is
// computed from the flag string, never by creating the store, so 'config'
// can print resolved paths without mutating anything.
func volumesRoot(cmd *cli.Command) string {
	dir := cmd.String("store-dir")
	// store.New expands a leading ~; mirror it here so compose and the
	// store never disagree about where volumes live.
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[1:])
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "volumes")
}

// namedVolumeDir is the managed host directory for project's volume name,
// docker's <project>_<name> naming.
func namedVolumeDir(root, project, name string) string {
	return filepath.Join(root, project+"_"+name)
}

// composeProject is persisted under <store>/compose/<project>.json so that
// down/ps/logs can find the instances that up created.
type composeProject struct {
	Name       string            `json:"name"`
	File       string            `json:"file"`
	Order      []string          `json:"order"`      // services in startup order
	Services   map[string]string `json:"services"`   // service -> instance name
	IPs        map[string]string `json:"ips"`        // service -> gateway-subnet IP
	SwitchSock string            `json:"switchSock"` // gateway control socket
	SwitchPID  int               `json:"switchPid"`  // gateway daemon PID
	Created    time.Time         `json:"created"`
	// Subnet is the project network the services' addresses came from. The
	// supervisor needs its prefix length to re-run a service with the same
	// --gateway-cidr it first got.
	Subnet string `json:"subnet,omitempty"`
	// Supervise gates the project supervisor. 'up' sets
	// it; every teardown path clears it on disk before stopping anything, so
	// a service being torn down is never restarted underneath the teardown —
	// and a state file written by a binary without supervision reads as off.
	// EnvFiles is the --env-file list 'up' loaded with. Persisted because the
	// supervisor and 'down' reload the compose file, and a file using
	// ${VAR:?...} does not load without it: the reload would fail every tick
	// and the restart policy would be silently inert.
	EnvFiles []string `json:"envFiles,omitempty"`
	// Profiles is the --profile list 'up' started with. Persisted for the same
	// reason as EnvFiles: a reload without it sees the services the profiles
	// disabled, and a disabled dependent is enough for isOneShot to call a
	// running service a job, which stops the supervisor from restarting it.
	Profiles  []string `json:"profiles,omitempty"`
	Supervise bool     `json:"supervise"`
}

// reloadProject reloads the compose file a project was started from and
// re-applies the profiles that were active at 'up', so every later reader
// sees the same set of services 'up' worked with.
//
// A reload is best-effort about drift: the file can change under a live
// project, and it runs on every supervisor tick, so failing here would stop
// restarting every other service in the project for as long as the drift
// lasted. A name the file no longer declares (renamed, removed, or now
// gated behind a profile this project never activated) is dropped rather
// than failing the whole reload; if a survivor's own dependency closure can
// no longer be resolved at all (compose-go's WithSelectedServices errors),
// the whole reload degrades to "nothing survived" rather than propagating
// the error — coarser than the bespoke per-edge drop this replaces, but the
// same fail-open contract this exists for.
func reloadProject(ctx context.Context, proj *composeProject) (*types.Project, error) {
	p, err := compose.Load(ctx, compose.Options{
		Files:       []string{proj.File},
		ProjectName: proj.Name,
		WorkingDir:  filepath.Dir(proj.File),
		EnvFiles:    proj.EnvFiles,
		Profiles:    proj.Profiles,
		Environ:     os.Environ(),
		Warn:        io.Discard,
		// A reload runs on every supervisor tick; an include: target that
		// has become unreadable (a shared file living outside the project
		// directory, moved or deleted without the project's own file
		// changing) must not stop every other service in the project from
		// being restarted for as long as the drift lasts. loadProjectFromCLI
		// (the user-facing config/up path) deliberately does not set this:
		// there, a missing include is the whole point of the error.
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		return nil, err
	}
	if err := validateProject(p, "", io.Discard); err != nil {
		return nil, err
	}
	// Order is the selection 'up' settled on and is already closed under its
	// dependencies, so replaying it (minus anything the file dropped)
	// reproduces the same set. Replaying it as named services also means a
	// service the file has since put behind a profile stays in view, which
	// is what supervision wants: the daemon supervises what it started, not
	// what the file would select today.
	var names []string
	for _, name := range proj.Order {
		if _, ok := p.Services[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return p.WithServicesDisabled(p.ServiceNames()...), nil
	}
	sel, err := p.WithSelectedServices(names)
	if err != nil {
		return p.WithServicesDisabled(p.ServiceNames()...), nil
	}
	return sel, nil
}

// maskBits is the project subnet's prefix length, defaulting to the /24 that
// 'up --subnet' defaults to for state written before Subnet was recorded.
func (p *composeProject) maskBits() int {
	if p.Subnet != "" {
		if _, n, err := net.ParseCIDR(p.Subnet); err == nil {
			bits, _ := n.Mask.Size()
			return bits
		}
	}
	return 24
}

// projectHostEntries builds the --add-host list every service gets: each
// service name and, when it differs, its instance name, mapped to its static
// address. Shared by 'up' and the supervisor so a restarted service resolves
// exactly what its siblings do.
func projectHostEntries(order []string, ips map[string]string, project string, services types.Services) []string {
	var out []string
	for _, svcName := range order {
		ip, ok := ips[svcName]
		if !ok {
			continue
		}
		out = append(out, svcName+":"+ip)
		svc, ok := services[svcName]
		if !ok {
			continue
		}
		if inst := instanceNameFor(project, svcName, svc); inst != svcName {
			out = append(out, inst+":"+ip)
		}
	}
	return out
}

// serviceLaunch is everything the 'run' invocation for one service needs that
// does not come from the service itself. 'up' fills it from the compose
// invocation; the supervisor rebuilds it from the persisted project state.
type serviceLaunch struct {
	project     string
	gatewaySock string
	maskBits    int
	ip          string
	hostEntries []string
	volumesRoot string
	// oneShot boots the benign init instead of the service's command, so the
	// agent has a live guest to run the job's command in.
	oneShot bool
}

// serviceRunArgs builds the argv for the detached 'run' that carries one
// service. It is the single definition of how a compose service becomes a VM,
// so a supervisor restart cannot drift from what 'up' did.
func serviceRunArgs(svcName, instance string, svc types.ServiceConfig, l serviceLaunch) ([]string, error) {
	hv := compose.Hypervisor(svc)
	if hv == "" {
		hv = "vz" // vz is the default backend
	}
	args := []string{"run", "--detach", "--net", "shared", "--name", instance,
		"--hypervisor", hv,
		"--gateway-sock", l.gatewaySock,
		"--gateway-cidr", fmt.Sprintf("%s/%d", l.ip, l.maskBits)}
	if svc.MemLimit > 0 {
		// UnitBytes -> MB, plain floor division: matches the bespoke parser's
		// behavior for numeric-byte inputs, and is always exact for the m/g
		// suffixed forms that make up nearly all real compose files.
		args = append(args, "--mem", strconv.Itoa(int(svc.MemLimit)/(1024*1024)))
	}
	if svc.CPUS > 0 {
		// The rounding itself is warned at load time by validateProject, so
		// config and up report it identically.
		args = append(args, "--cpus", strconv.Itoa(roundUpCpus(float64(svc.CPUS))))
	}
	for _, env := range envSlice(svc.Environment) {
		args = append(args, "--env", env)
	}
	for _, h := range l.hostEntries {
		args = append(args, "--add-host", h)
	}
	for _, v := range svc.Volumes {
		entry, err := resolveServiceVolume(v, l.volumesRoot, l.project)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", svcName, err)
		}
		args = append(args, "--shared-dir", entry)
	}
	// "--" stops flag parsing so service commands like ["ping", "-c", "3"]
	// are not interpreted as run flags.
	args = append(args, "--", svc.Image)
	if l.oneShot {
		args = append(args, oneshotInitCommand...)
	} else {
		args = append(args, svc.Command...)
	}
	return args, nil
}

// envSlice renders a MappingWithEquals into sorted "k=v" pairs. By the time
// compose.Load returns, svc.Environment is already fully resolved:
// env_file content merged in, environment: having won any collision with
// env_file, and any bare 'environment: [FOO]' key resolved against the
// interpolation environment where possible. A nil value means the key was
// declared bare and could not be resolved from anywhere (not .env, not the
// process environ, not any env_file) — dropped here rather than guessed at
// from os.Environ(), matching docker: the guest never sees that variable.
func envSlice(m types.MappingWithEquals) []string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+*m[k])
	}
	return out
}

// resolveServiceVolume renders one already-typed, already-path-resolved
// volume entry as the "host:guest[:ro]" form run's --shared-dir takes. A
// named volume's host side is the managed store directory; a bind mount's
// source is already an absolute path (WithResolvedPaths(true) anchored it
// against the directory of the file that declared the service, or the
// project_directory of the include that pulled that file in).
func resolveServiceVolume(v types.ServiceVolumeConfig, volRoot, project string) (string, error) {
	var host string
	switch v.Type {
	case types.VolumeTypeVolume:
		host = namedVolumeDir(volRoot, project, v.Source)
	case types.VolumeTypeBind:
		host = v.Source
	default:
		return "", fmt.Errorf("volume type %q is not supported (only bind mounts and named volumes are)", v.Type)
	}
	entry := host + ":" + v.Target
	if v.ReadOnly {
		entry += ":ro"
	}
	return entry, nil
}

// resolvePort validates and renders one already-typed port mapping. Docker's
// short-form ports grammar already parsed at load time (ranges expand into
// one ServicePortConfig per port, protocol and the bare ephemeral-port form
// are already split out); what is rejected here is what run cannot actually
// forward: non-TCP, and the single-port (ephemeral host port) form.
// Publishing defaults to loopback when the file gives no host address — a
// deliberate security choice; an explicit 0.0.0.0 exposes a port beyond the
// machine.
func resolvePort(p types.ServicePortConfig) (hostAddr, hostPort, guestPort string, err error) {
	if p.Protocol != "" && !strings.EqualFold(p.Protocol, "tcp") {
		return "", "", "", fmt.Errorf("port %d/%s: %s port mappings are not supported yet (TCP only)", p.Target, p.Protocol, p.Protocol)
	}
	if p.Published == "" {
		return "", "", "", fmt.Errorf("port %d: single-port form (ephemeral host port) is not supported yet, use HOST:GUEST", p.Target)
	}
	hostAddr = p.HostIP
	if hostAddr == "" {
		hostAddr = "127.0.0.1"
	}
	return hostAddr, p.Published, strconv.Itoa(int(p.Target)), nil
}

func composeCommand() *cli.Command {
	return &cli.Command{
		Name:  "compose",
		Usage: "run multi-service compose files (subset; one service = one VM)",
		// Docker UX: -f/-p live on the parent command
		// (docker compose -f FILE logs -f SERVICE), freeing -f for follow
		// on logs. urfave/cli v3 resolves parent flags from subcommands.
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "file",
				Aliases: []string{"f"},
				Sources: cli.EnvVars("COMPOSE_FILE"),
				Usage:   "compose file (default: docker-compose.yml / compose.yaml)",
			},
			&cli.StringFlag{
				Name:    "project-name",
				Aliases: []string{"p"},
				Sources: cli.EnvVars("COMPOSE_PROJECT_NAME"),
				Usage:   "project name (default: current directory name)",
			},
			&cli.StringSliceFlag{
				Name:  "env-file",
				Usage: "interpolation env file(s); replaces the default .env beside the compose file (repeatable, later files win)",
			},
			&cli.StringSliceFlag{
				Name:    "profile",
				Sources: cli.EnvVars("COMPOSE_PROFILES"),
				Usage:   "activate a profile; a service that declares 'profiles' starts only when one of its own is active (repeatable, '*' activates all)",
			},
		},
		Commands: []*cli.Command{
			{
				Name:      "up",
				Usage:     "create and start services (detached; all of them when none is named)",
				ArgsUsage: "[SERVICE...]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "detach", Aliases: []string{"d"}, Usage: "accepted for compatibility; up always detaches"},
					&cli.StringFlag{Name: "subnet", Value: "10.87.0.0/24", Usage: "virtual subnet for the project network"},
				},
				Action: composeUp,
			},
			{
				Name:  "down",
				Usage: "stop and remove all services",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "volumes", Aliases: []string{"v"}, Usage: "also remove the project's named volumes"},
				},
				Action: composeDown,
			},
			{
				Name:   "ps",
				Usage:  "list project services",
				Action: composePs,
			},
			{
				Name:      "logs",
				Usage:     "show service logs (all services when none given)",
				ArgsUsage: "[SERVICE]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "follow log output (single service only)"},
				},
				Action: composeLogs,
			},
			{
				Name:      "config",
				Usage:     "validate the compose file and print the effective configuration",
				ArgsUsage: "[SERVICE...]",
				Action:    composeConfig,
			},
			{
				Name:      "exec",
				Usage:     "run a command in a running service (guest agent required)",
				ArgsUsage: "[-T] [-u USER] [-e K=V] [-w DIR] SERVICE COMMAND [ARGS...]",
				// Flags are hand-parsed docker-style: parsing stops at the
				// first positional so the guest command keeps its dashes.
				SkipFlagParsing: true,
				Action:          composeExec,
			},
			{
				Name:      "top",
				Usage:     "list guest processes per service (guest agent and /bin/ps required)",
				ArgsUsage: "[SERVICE...]",
				Action:    composeTop,
			},
		},
	}
}

func findComposeFile(cmd *cli.Command) (string, error) {
	if f := cmd.String("file"); f != "" {
		return f, nil
	}
	for _, cand := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", errors.New("no compose file found (looked for docker-compose.yml / compose.yaml); use --file")
}

func projectName(cmd *cli.Command) string {
	if p := cmd.String("project-name"); p != "" {
		return sanitizeName(p)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "compose"
	}
	return sanitizeName(filepath.Base(wd))
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "compose"
	}
	return b.String()
}

// loadProjectFromCLI is the one loader entry every compose subcommand action
// uses: it translates the compose command's flags into internal/compose's
// Options exactly as the bespoke loader read them, loads the project through
// compose-go, and applies the urunc-specific validation compose-go does not
// do (validateProject).
func loadProjectFromCLI(ctx context.Context, cmd *cli.Command, warn io.Writer) (*types.Project, error) {
	file, err := findComposeFile(cmd)
	if err != nil {
		return nil, err
	}
	// WorkingDir is the project directory: where a default (unnamed) .env is
	// discovered, and (via compose-go's own GetWorkingDir fallback) what
	// relative paths inside the file resolve against absent an explicit
	// project_directory. That has to be the compose file's own directory,
	// not the invoking shell's cwd — otherwise `compose --file
	// ../other/compose.yaml config` would look for .env beside the shell's
	// cwd instead of beside the file that declares the services, silently
	// dropping every default-.env-fed interpolation for a --file outside
	// cwd. findComposeFile's own default-discovery already searches cwd
	// when --file is not given, so by the time this runs, file always names
	// a real path; only its directory is needed here.
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	profiles := cmd.StringSlice("profile") // --profile, or COMPOSE_PROFILES via the same flag's Sources
	p, err := compose.Load(ctx, compose.Options{
		Files:       []string{file},
		ProjectName: projectName(cmd),
		WorkingDir:  filepath.Dir(abs),
		EnvFiles:    cmd.StringSlice("env-file"),
		Profiles:    profiles,
		Environ:     os.Environ(),
		Warn:        warn,
	})
	if err != nil {
		return nil, err
	}
	warnUnmatchedProfiles(p, profiles, warn)
	if err := validateProject(p, volumesRoot(cmd), warn); err != nil {
		return nil, err
	}
	return p, nil
}

// warnUnmatchedProfiles reports a requested profile no service in the whole
// project declares — almost always a typo, and its result (nothing extra
// starts) is indistinguishable from success otherwise. compose-go's own
// profile activation (run inside Load, unconditionally, for --profile and
// COMPOSE_PROFILES alike since they reach Options.Profiles through the same
// flag) has no equivalent check, so it has to happen here, against the
// project's full declared-profile set: p.AllServices() returns both the
// services a profile enabled and the ones still sitting disabled, which is
// what "no service declares it" needs to be checked against.
func warnUnmatchedProfiles(p *types.Project, requested []string, warn io.Writer) {
	declared := map[string]bool{}
	for _, svc := range p.AllServices() {
		for _, prof := range svc.Profiles {
			declared[prof] = true
		}
	}
	seen := map[string]bool{}
	var unmatched []string
	for _, prof := range requested {
		prof = strings.TrimSpace(prof)
		if prof == "" || prof == allProfiles || declared[prof] || seen[prof] {
			continue
		}
		seen[prof] = true
		unmatched = append(unmatched, prof)
	}
	sort.Strings(unmatched)
	for _, prof := range unmatched {
		composeWarn(warn, "profile %q is active, but no service declares it", prof)
	}
}

// validateProject checks what compose-go's own schema and consistency pass
// (loader/validate.go's checkConsistency, run unconditionally inside
// compose.Load) do not: urunc-specific shape and capability constraints.
// warn receives the same load-time warnings the bespoke validator emitted.
//
// A few checks the bespoke validateComposeFile performed are deliberately
// not ported here because compose-go's own loader now subsumes them with its
// own wording: a named volume referenced but not declared under the
// top-level 'volumes:' key ("service %q refers to undefined volume %s"), a
// depends_on target that does not exist at all ("service %q depends on
// undefined service %q" — the same wording the bespoke code used), and the
// volume-name character-class check (the compose-spec JSON schema already
// restricts top-level volume names to ^[a-zA-Z0-9._-]+$, tighter than a bare
// path-traversal check needs to be). volRoot is accepted for interface
// symmetry with the run-layer volume resolution but is not currently needed
// by any surviving check.
func validateProject(p *types.Project, volRoot string, warn io.Writer) error {
	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		if svc.ContainerName != "" {
			if err := validateContainerName(svc.ContainerName); err != nil {
				return fmt.Errorf("service %q: %w", name, err)
			}
		}
		hv := compose.Hypervisor(svc)
		if hv != "" && !supportedHypervisors[hv] {
			return fmt.Errorf("service %q: unsupported x-hypervisor %q (supported: vz, qemu, hvi)", name, hv)
		}
		// serviceRunArgs converts MemLimit to whole megabytes by plain floor
		// division (int(svc.MemLimit)/(1024*1024)); a value under 1 MiB
		// floors to 0 with nothing downstream to catch it — run.go's --mem
		// flag turns that straight into MemSizeB: uint64(0)*1024*1024, a
		// zero-memory VM request Virtualization.framework rejects with an
		// opaque error far from the compose file that actually caused it.
		// mem_limit: 512k is schema-valid and compose-go accepts it (the
		// bespoke parseMemLimit this loader replaced rejected byte/k-suffixed
		// forms outright; compose-go does not), so this can no longer be
		// caught upstream. Reject it here, at load/validate time, pointing at
		// the service and the value that would have silently zeroed out.
		if svc.MemLimit > 0 && svc.MemLimit < 1<<20 {
			return fmt.Errorf("service %q: mem_limit %d bytes is below the 1 MiB minimum; it would round down to a 0 MB VM request", name, svc.MemLimit)
		}
		if svc.CPUS > 0 {
			vcpus := roundUpCpus(float64(svc.CPUS))
			if float64(svc.CPUS) != float64(vcpus) {
				composeWarn(warn, "service %q: cpus %.2f rounded up to %d vCPU(s) (shares are not supported, only whole vCPUs)", name, float64(svc.CPUS), vcpus)
			}
		}
		htcp, err := compose.HealthTCPFor(svc)
		if err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if htcp.Declared && htcp.Port == 0 {
			composeWarn(warn, "service %q: x-healthcheck-tcp is ignored: no port set", name)
		}
		for _, v := range svc.Volumes {
			if v.Type != types.VolumeTypeBind && v.Type != types.VolumeTypeVolume {
				return fmt.Errorf("service %q: volume type %q is not supported (only bind mounts and named volumes are)", name, v.Type)
			}
			// An anonymous volume (no Source) is compose-go's short-form
			// result for "volumes: [/data]" — a single path with no host
			// side. checkConsistency deliberately skips validating it
			// (compose-go/loader/validate.go's own "non anonymous volumes"
			// comment) because docker mints a fresh, unnamed volume per
			// container for this form. hull has no such mechanism:
			// resolveServiceVolume would join every anonymous volume in a
			// project onto the same "<volRoot>/<project>_" directory (empty
			// name), a bogus path nothing creates and every anonymous mount
			// would silently share. Reject it here, at the same place the
			// bespoke loader did ("invalid volume ..., expected
			// /host/path:/guest/path[:mode]" for a bare guest-only entry).
			if v.Type == types.VolumeTypeVolume && v.Source == "" {
				return fmt.Errorf("service %q: anonymous volumes (a bare guest path with no host source or named volume) are not supported; declare a named volume or a bind mount", name)
			}
			if !strings.HasPrefix(v.Target, "/") {
				return fmt.Errorf("service %q: guest path %q must be absolute", name, v.Target)
			}
			if strings.ContainsAny(v.Target, " \t'\"") {
				return fmt.Errorf("service %q: guest path %q must not contain spaces or quotes", name, v.Target)
			}
			if v.ReadOnly {
				composeWarn(warn, "service %q: volume %q: read-only is not enforced yet, mounting read-write", name, v.String())
			}
		}
		if compose.OneShot(svc) && len(svc.Command) == 0 {
			return fmt.Errorf("service %q: x-oneshot needs a 'command' to run", name)
		}
		if err := validateHealthCheck(svc.HealthCheck); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if healthProbeArgv(svc.HealthCheck) != nil && htcp.Port != 0 {
			composeWarn(warn, "service %q: both healthcheck and x-healthcheck-tcp declared; the exec healthcheck wins", name)
		}
		for i, h := range svc.PostStart {
			if len(h.Command) == 0 {
				return fmt.Errorf("service %q: post_start[%d]: command is required", name, i)
			}
		}
		for i, h := range svc.PreStop {
			if len(h.Command) == 0 {
				return fmt.Errorf("service %q: pre_stop[%d]: command is required", name, i)
			}
		}
		pol, err := parseRestartPolicy(svc.Restart)
		if err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		warnRestartDivergence(warn, name, pol)
		// Whether the service is a job depends on which dependents start, so
		// that warning belongs to the selection, not the load. See
		// warnRestartOnJobs.
		for _, depName := range sortedDependsOnKeys(svc.DependsOn) {
			dep := svc.DependsOn[depName]
			target, ok := p.Services[depName]
			if !ok {
				// compose-go's own consistency check (run inside Load) already
				// hard-fails a *required* depends_on target that does not
				// exist or is disabled by profile; what reaches here is
				// always the required:false escape hatch, dropped and warned
				// about the same way the bespoke loader's pass 2 did.
				delete(svc.DependsOn, depName)
				composeWarn(warn, "service %q: optional dependency %q is not enabled by any active profile; starting without it", name, depName)
				continue
			}
			switch dep.Condition {
			case "service_started":
			case "service_healthy":
				dtcp, _ := compose.HealthTCPFor(target)
				if dtcp.Port == 0 && healthProbeArgv(target.HealthCheck) == nil {
					return fmt.Errorf("service %q requires %q healthy, but it declares no healthcheck or x-healthcheck-tcp", name, depName)
				}
			case "service_completed_successfully":
				// The dependency runs as a job; its command is what the
				// agent executes, so it must have one.
				if len(target.Command) == 0 {
					return fmt.Errorf("service %q waits for %q to complete, but %q declares no 'command' to run", name, depName, depName)
				}
			default:
				return fmt.Errorf("service %q: unsupported depends_on condition %q", name, dep.Condition)
			}
		}
	}
	// warnRestartOnJobs runs in selectProject, not here: whether a service is
	// a job depends on which dependents are actually selected, which is not
	// settled until selection runs (see warnRestartOnJobs's own comment).
	return nil
}

// sortedDependsOnKeys returns a depends_on map's target names in a stable
// order, so an error or warning produced while walking it never depends on
// map iteration order.
func sortedDependsOnKeys(deps types.DependsOnConfig) []string {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// selectionFor reads the selection one invocation asks for: --profile (and
// COMPOSE_PROFILES behind it) plus the SERVICE names given as arguments.
// selectProject reduces p to the named services and their dependency
// closure. Profile activation from --profile/COMPOSE_PROFILES already
// happened inside compose.Load (every invocation runs compose-go's own
// WithProfiles, whether or not --profile was given); this handles the
// SERVICE names an invocation named on the command line.
//
// Naming a service on the command line ('up SERVICE...', 'config
// SERVICE...') activates that service's own declared profiles too, docker's
// documented behavior (the profiles.md example this mirrors) and the
// bespoke loader's own behavior before it. compose-go ships exactly this as
// WithServicesEnabled: it folds a named-but-disabled service's profiles
// into the active set and recomputes which services are enabled, before
// WithSelectedServices narrows the (now larger) enabled set down to the
// named services and their dependency closure. A name that still does not
// exist at all falls through to WithSelectedServices' own
// "no such service" error.
func selectProject(p *types.Project, names []string, warn io.Writer) (*types.Project, error) {
	if len(names) > 0 {
		enabled, err := p.WithServicesEnabled(names...)
		if err != nil {
			return nil, err
		}
		p = enabled
	}
	sel, err := p.WithSelectedServices(names)
	if err != nil {
		return nil, err
	}
	if len(sel.Services) == 0 {
		return nil, errors.New("no service is enabled: every service in the file declares a profile, and none of those profiles is active (use --profile NAME or COMPOSE_PROFILES)")
	}
	warnRestartOnJobs(sel, warn)
	return sel, nil
}

// warnRestartOnJobs reports a restart policy that a job's lifecycle makes
// inert. A service is a job because some dependent gates on its completion,
// so the verdict only holds once the set of services is settled: computed at
// load time it fires for a service whose only such dependent a profile keeps
// out, and that service then gets supervised normally, making the warning a
// lie. It is emitted here, over the selected set, for that reason.
func warnRestartOnJobs(p *types.Project, warn io.Writer) {
	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		pol, err := parseRestartPolicy(svc.Restart)
		if err != nil {
			continue // already reported by validateProject's per-service pass
		}
		if pol.restarts() && isOneShot(p, name) {
			composeWarn(warn, "service %q: restart %q is ignored: the service is a job, and a completed job stays completed", name, pol.String())
		}
	}
}

// startupOrder returns the services topologically sorted by depends_on
// (dependencies first). Ties are broken alphabetically for determinism.
func startupOrder(p *types.Project) ([]string, error) {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for name, svc := range p.Services {
		if _, ok := indegree[name]; !ok {
			indegree[name] = 0
		}
		for dep := range svc.DependsOn {
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}
	var ready []string
	for name, d := range indegree {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		next := dependents[n]
		sort.Strings(next)
		for _, m := range next {
			indegree[m]--
			if indegree[m] == 0 {
				ready = append(ready, m)
			}
		}
	}
	if len(order) != len(p.Services) {
		return nil, errors.New("dependency cycle in depends_on")
	}
	return order, nil
}

func instanceNameFor(project, service string, svc types.ServiceConfig) string {
	if svc.ContainerName != "" {
		return svc.ContainerName
	}
	return project + "-" + sanitizeName(service)
}

// checkInstanceNames rejects two services resolving to the same instance name
// (explicit container_name collisions, or distinct service names sanitizing
// to the same string), which up would otherwise discover only after the
// first VM had already booted.
func checkInstanceNames(p *types.Project, project string) error {
	seen := map[string]string{}
	for _, name := range p.ServiceNames() {
		inst := instanceNameFor(project, name, p.Services[name])
		if prev, ok := seen[inst]; ok {
			return fmt.Errorf("services %q and %q both resolve to instance name %q; set distinct container_name values", prev, name, inst)
		}
		seen[inst] = name
	}
	return nil
}

// supportedHypervisors are the x-hypervisor values run's normalization
// understands (run.go): canonical names plus their aliases.
var supportedHypervisors = map[string]bool{
	"vz": true, "qemu": true, "hvi": true, "qemu-hvf": true, "virtualization": true, "apple": true,
}

// roundUpCpus converts a fractional compose cpus value to the whole vCPU
// count the VM actually gets. Shared by up and config so they cannot drift.
// Ceil, not +0.999: the warning promises "rounded up", and a request must
// never be rounded below itself (2.0005 gets 3, not 2).
func roundUpCpus(c float64) int {
	return int(math.Ceil(c))
}

// validateContainerName errors (like docker) instead of silently renaming.
func validateContainerName(name string) error {
	if sanitizeName(name) != name {
		return fmt.Errorf("invalid container_name %q: only lowercase letters, digits, - and _ are allowed", name)
	}
	return nil
}

func projectStatePath(s *store.Store, project string) string {
	return filepath.Join(s.RootDir(), "compose", project+".json")
}

func saveProject(s *store.Store, p *composeProject) error {
	path := projectStatePath(s, p.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically: 'up' saves after every service start while the
	// supervisor polls every 2s, and a truncate-then-write would let a tick
	// read half a file. The supervisor treats an unreadable state file as
	// "the project is gone" and stops for good, so a torn read here used to
	// end restart policies for the project's life.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func loadProject(s *store.Store, project string) (*composeProject, error) {
	data, err := os.ReadFile(projectStatePath(s, project))
	if err != nil {
		return nil, err
	}
	var p composeProject
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// selfExec runs this binary with the given args, propagating --store-dir and
// --debug, and returns combined output.
func selfExec(cmd *cli.Command, args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	full := append(selfExecGlobalArgs(cmd), args...)
	c := exec.Command(exe, full...)
	// One user command, one command event: the child re-invocations of
	// this binary stay out of telemetry.
	c.Env = append(os.Environ(), telemetry.EnvSuppress+"=1")
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func composeUp(ctx context.Context, cmd *cli.Command) error {
	file, err := findComposeFile(cmd)
	if err != nil {
		return err
	}
	p, err := loadProjectFromCLI(ctx, cmd, os.Stderr)
	if err != nil {
		return err
	}
	p, err = selectProject(p, cmd.Args().Slice(), os.Stderr)
	if err != nil {
		return err
	}
	order, err := startupOrder(p)
	if err != nil {
		return err
	}
	project := projectName(cmd)
	if err := checkInstanceNames(p, project); err != nil {
		return err
	}
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	if _, err := loadProject(s, project); err == nil {
		return fmt.Errorf("project %q is already up (run 'compose down' first)", project)
	}

	// Named volumes: create the managed directories up front so every
	// service (and a later 'down --volumes') sees the same set. Contents
	// persist across down/up by design.
	for _, name := range p.VolumeNames() {
		dir := namedVolumeDir(volumesRoot(cmd), project, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("named volume %q: %w", name, err)
		}
	}

	// Static gateway-subnet IPs are allocated before anything boots, so every
	// service gets the complete name→IP map regardless of start order, and
	// port forwards can be configured when the gateway starts. The gateway
	// takes network+1; services start at network+10.
	subnet := cmd.String("subnet")
	_, subnetNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("invalid --subnet %q: %w", subnet, err)
	}
	ones, bits := subnetNet.Mask.Size()
	hostSpace := 1<<(bits-ones) - 2 // minus network and broadcast
	if len(order) > hostSpace-10 {
		return fmt.Errorf("%d services exceed the %s subnet's capacity (%d usable service addresses); use a larger --subnet", len(order), subnet, hostSpace-10)
	}
	gatewayAddr := offsetIP(subnetNet.IP, 1).String()
	ips := map[string]string{}
	var forwards []string
	for i, svcName := range order {
		ip := offsetIP(subnetNet.IP, 10+i).String()
		ips[svcName] = ip
		for _, port := range p.Services[svcName].Ports {
			hostAddr, hostPort, guestPort, err := resolvePort(port)
			if err != nil {
				return fmt.Errorf("service %q: %w", svcName, err)
			}
			forwards = append(forwards, fmt.Sprintf("%s:%s=%s:%s", hostAddr, hostPort, ip, guestPort))
		}
	}
	hostEntries := projectHostEntries(order, ips, project, p.Services)

	// Start the project's user-mode network gateway: each service's single
	// NIC lives on its subnet, which provides service-to-service switching,
	// NAT egress, DNS, and the host-side port forwards computed above.
	gatewaySock := filepath.Join(s.RootDir(), "compose", project+".gateway.sock")
	gatewayAPI := filepath.Join(s.RootDir(), "compose", project+".api.sock")
	// Service names double as DNS A records on the gateway, so guests that
	// resolve via plain DNS (unikernels without /etc/hosts) also work.
	var dnsRecords []string
	for name, ip := range ips {
		dnsRecords = append(dnsRecords, name+"="+ip)
	}
	sort.Strings(dnsRecords)
	gatewayPID, err := startGatewayDaemon(cmd, s, project, gatewaySock, gatewayAPI, subnet, gatewayAddr, forwards, dnsRecords)
	if err != nil {
		return err
	}

	// Persist the file path absolutely: down reloads it (for --volumes and
	// pre_stop hooks)
	// and may run from a different working directory.
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	proj := &composeProject{
		Name:       project,
		File:       file,
		EnvFiles:   cmd.StringSlice("env-file"),
		Profiles:   cmd.StringSlice("profile"),
		Order:      order,
		Services:   map[string]string{},
		IPs:        ips,
		SwitchSock: gatewaySock,
		SwitchPID:  gatewayPID,
		Created:    time.Now(),
		Subnet:     subnet,
		// Supervision is on from the first save: the gateway daemon is
		// already running, and it polls the state file for this flag.
		Supervise: true,
	}
	// Persist immediately: if up dies before the first service starts, the
	// gateway daemon must not leak with no record pointing at it.
	if err := saveProject(s, proj); err != nil {
		stopGatewayDaemon(proj)
		return fmt.Errorf("failed to save project state: %w", err)
	}

	// Ctrl-C during up tears down whatever was created (docker parity).
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// teardown unwinds everything up created: recorded instances, the
	// possibly half-created instance of a failing service, the gateway,
	// and the project state.
	teardown := func(failingInstance string) {
		// Supervision goes first, on disk: a service that is about to be
		// stopped must not be restarted underneath this unwind.
		pauseSupervision(s, proj, os.Stderr)
		for _, started := range proj.Order {
			if inst, ok := proj.Services[started]; ok {
				_, _ = selfExec(cmd, "stop", inst)
				_, _ = selfExec(cmd, "rm", inst)
			}
		}
		if failingInstance != "" {
			_, _ = selfExec(cmd, "stop", failingInstance)
			_, _ = selfExec(cmd, "rm", failingInstance)
		}
		stopGatewayDaemon(proj)
		_ = os.Remove(projectStatePath(s, project))
	}

	for _, svcName := range order {
		svc := p.Services[svcName]
		instance := instanceNameFor(project, svcName, svc)

		if ctx.Err() != nil {
			teardown(instance)
			return fmt.Errorf("interrupted; project %q torn down", project)
		}

		// Completion-gated dependencies: the job already ran (startupOrder
		// puts dependencies first) and its recorded status decides.
		if err := checkCompletedDeps(s, proj, svcName, svc.DependsOn, os.Stderr); err != nil {
			teardown(instance)
			return err
		}

		// Health-gated dependencies: exec healthchecks probe through the
		// guest agent; x-healthcheck-tcp probes through the gateway API.
		for _, depName := range sortedDependsOnKeys(svc.DependsOn) {
			dep := svc.DependsOn[depName]
			if dep.Condition != "service_healthy" {
				continue
			}
			depSvc := p.Services[depName]
			if healthProbeArgv(depSvc.HealthCheck) != nil {
				fmt.Printf("Waiting for %s to be healthy (exec healthcheck)...\n", depName)
				if err := waitHealthyExec(s, proj.Services[depName], depSvc.HealthCheck); err != nil {
					if !dep.Required {
						composeWarn(os.Stderr, "service %q: optional dependency %q never became healthy: %v; starting anyway", svcName, depName, err)
						continue
					}
					teardown(instance)
					return fmt.Errorf("dependency %q never became healthy: %w", depName, err)
				}
				continue
			}
			depTCP, err := compose.HealthTCPFor(depSvc)
			if err != nil {
				teardown(instance)
				return fmt.Errorf("service %q: %w", depName, err)
			}
			addr := fmt.Sprintf("%s:%d", ips[depName], depTCP.Port)
			fmt.Printf("Waiting for %s to be healthy (tcp %d)...\n", depName, depTCP.Port)
			interval, budget := depTCP.ProbeBudget()
			if err := waitHealthy(gatewayAPI, addr, interval, budget); err != nil {
				if !dep.Required {
					composeWarn(os.Stderr, "service %q: optional dependency %q never became healthy: %v; starting anyway", svcName, depName, err)
					continue
				}
				teardown(instance)
				return fmt.Errorf("dependency %q never became healthy: %w", depName, err)
			}
		}

		// A job's VM runs a benign init so the agent has a live guest to
		// exec in; the service's command is what the agent runs, below.
		oneShot := isOneShot(p, svcName)
		maskBits, _ := subnetNet.Mask.Size()
		runArgs, err := serviceRunArgs(svcName, instance, svc, serviceLaunch{
			project:     project,
			gatewaySock: gatewaySock,
			maskBits:    maskBits,
			ip:          proj.IPs[svcName],
			hostEntries: hostEntries,
			volumesRoot: volumesRoot(cmd),
			oneShot:     oneShot,
		})
		if err != nil {
			teardown("")
			return err
		}

		fmt.Printf("Creating %s (%s)...\n", svcName, instance)
		if out, err := selfExec(cmd, runArgs...); err != nil {
			teardown(instance)
			return fmt.Errorf("service %q failed to start: %v\n%s", svcName, err, out)
		}
		proj.Services[svcName] = instance
		if err := saveProject(s, proj); err != nil {
			teardown(instance)
			return fmt.Errorf("failed to save project state: %w", err)
		}

		if oneShot {
			// Run the job to completion and keep its exit status. A non-zero
			// code fails the up: dependents gating on completion must never
			// start against a failed job.
			fmt.Printf("  running %s to completion...\n", svcName)
			// The job's command runs through the agent with the same
			// environment the VM was given.
			res, err := runOneShot(s, instance, svcName,
				svc.Command, envSlice(svc.Environment),
				oneShotDialWindow, oneShotCommandBudget)
			if err != nil {
				teardown("")
				return err
			}
			// The job is done either way: stop its VM before judging the code
			// so a failure does not leave a guest running.
			if out, serr := selfExec(cmd, "stop", instance); serr != nil {
				composeWarn(os.Stderr, "service %q: stop after the job finished: %v: %s", svcName, serr, out)
			}
			if res.code != 0 {
				teardown("")
				return oneShotFailure(svcName, res)
			}
			fmt.Printf("  %s completed successfully\n", svcName)
			continue
		}
		// post_start hooks run in the fresh guest; a failed hook fails the
		// up (spec semantics) and unwinds everything already created.
		if len(svc.PostStart) > 0 {
			fmt.Printf("  running %d post_start hook(s) for %s...\n", len(svc.PostStart), svcName)
			if err := runServiceHooks(s, instance, svcName, "post_start", svc.PostStart); err != nil {
				teardown("")
				return err
			}
		}
		fmt.Printf("  %s is up at %s\n", svcName, proj.IPs[svcName])
	}
	jobs := 0
	for _, svcName := range order {
		if isOneShot(p, svcName) {
			jobs++
		}
	}
	if jobs > 0 {
		fmt.Printf("Project %s: %d service(s) running, %d job(s) completed\n", project, len(order)-jobs, jobs)
	} else {
		fmt.Printf("Project %s: %d service(s) running\n", project, len(order))
	}
	return nil
}

func composeDown(ctx context.Context, cmd *cli.Command) error {
	project := projectName(cmd)
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	proj, err := loadProject(s, project)
	if err != nil {
		return fmt.Errorf("project %q is not up (no state at %s)", project, projectStatePath(s, project))
	}
	// Supervision stops before anything else does, and it stops on disk: the
	// supervisor lives in another process, so the flag it polls has to be
	// cleared before the first service is stopped or it would restart what
	// this teardown just stopped.
	pauseSupervision(s, proj, os.Stderr)
	// pre_stop hooks and --volumes both need the service/volume definitions;
	// the project state records the compose file it was started from. A
	// missing or changed file degrades to a warning: down must always be
	// able to stop things.
	reloaded, reloadErr := reloadProject(ctx, proj)
	if reloadErr != nil {
		composeWarn(os.Stderr, "pre_stop hooks skipped: cannot reload %s: %v", proj.File, reloadErr)
	}

	// Reverse startup order
	for i := len(proj.Order) - 1; i >= 0; i-- {
		svcName := proj.Order[i]
		instance, ok := proj.Services[svcName]
		if !ok {
			continue
		}
		if reloaded != nil {
			if svc, ok := reloaded.Services[svcName]; ok && len(svc.PreStop) > 0 {
				fmt.Printf("Running %d pre_stop hook(s) for %s...\n", len(svc.PreStop), svcName)
				if err := runServiceHooks(s, instance, svcName, "pre_stop", svc.PreStop); err != nil {
					// Stopping must proceed regardless; the hook failure is
					// reported, not fatal.
					composeWarn(os.Stderr, "%v", err)
				}
			}
		}
		fmt.Printf("Stopping %s (%s)...\n", svcName, instance)
		if out, err := selfExec(cmd, "stop", instance); err != nil {
			fmt.Printf("  stop: %v: %s\n", err, out)
		}
		if out, err := selfExec(cmd, "rm", instance); err != nil {
			fmt.Printf("  rm: %v: %s\n", err, out)
		}
	}
	stopGatewayDaemon(proj)
	if err := os.Remove(projectStatePath(s, project)); err != nil {
		return fmt.Errorf("failed to remove project state: %w", err)
	}

	// down --volumes: remove exactly the volumes the compose file declares
	// (never a prefix glob — overlapping project names must not be able to
	// delete each other's data). A missing file degrades to a warning:
	// plain teardown already succeeded.
	if cmd.Bool("volumes") {
		if reloaded != nil {
			for _, name := range reloaded.VolumeNames() {
				dir := namedVolumeDir(volumesRoot(cmd), project, name)
				if err := os.RemoveAll(dir); err != nil {
					composeWarn(os.Stderr, "remove named volume %q: %v", name, err)
				} else {
					fmt.Printf("Removed volume %s_%s\n", project, name)
				}
			}
		} else {
			composeWarn(os.Stderr, "--volumes skipped: cannot reload %s: %v", proj.File, reloadErr)
		}
	}
	fmt.Printf("Project %s is down\n", project)
	return nil
}

func composePs(ctx context.Context, cmd *cli.Command) error {
	project := projectName(cmd)
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	proj, err := loadProject(s, project)
	if err != nil {
		return fmt.Errorf("project %q is not up", project)
	}
	fmt.Printf("%-16s %-24s %-10s %-16s\n", "SERVICE", "INSTANCE", "STATUS", "IP")
	for _, svcName := range proj.Order {
		instance := proj.Services[svcName]
		status := "unknown"
		if st, err := s.GetInstance(instance); err == nil {
			status = st.Status
		}
		svcIP := proj.IPs[svcName]
		if svcIP == "" {
			svcIP = "-"
		}
		fmt.Printf("%-16s %-24s %-10s %-16s\n", svcName, instance, status, svcIP)
	}
	return nil
}

func composeLogs(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() > 1 {
		return errors.New("at most one SERVICE argument")
	}
	project := projectName(cmd)
	s, err := globalStore(cmd)
	if err != nil {
		return err
	}
	proj, err := loadProject(s, project)
	if err != nil {
		return fmt.Errorf("project %q is not up", project)
	}

	// No argument: all services with a name prefix, docker style.
	if args.Len() == 0 {
		if cmd.Bool("follow") {
			return errors.New("--follow requires a single SERVICE argument")
		}
		for _, svcName := range proj.Order {
			st, err := s.GetInstance(proj.Services[svcName])
			if err != nil || st.LogFile == "" {
				continue
			}
			data, err := os.ReadFile(st.LogFile)
			if err != nil {
				continue
			}
			// The console log is whatever the guest wrote to it, and this
			// path prints it with no filter -- unlike `hull logs`, which goes
			// through guestTerminalWriter. Same bytes, same terminal.
			for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				fmt.Printf("%s | %s\n", svcName, sanitizeGuestText(line))
			}
		}
		return nil
	}

	instance, ok := proj.Services[args.First()]
	if !ok {
		return fmt.Errorf("unknown service %q", args.First())
	}
	// Global flags must survive the re-exec: without --store-dir the child
	// reads the default store and reports the instance missing even though
	// this process just listed it.
	logArgs := append(selfExecGlobalArgs(cmd), "logs")
	if cmd.Bool("follow") {
		logArgs = append(logArgs, "-f")
	}
	logArgs = append(logArgs, instance)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, logArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// composeConfig validates the compose file the way up would and prints the
// effective configuration on stdout: docker-canonical output, exactly what
// compose-go's own MarshalYAML renders for the selected project. It boots
// nothing, reads and writes no instance or project state, and never
// contacts the gateway.
func composeConfig(ctx context.Context, cmd *cli.Command) error {
	out, err := composeConfigYAML(ctx, cmd)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

// composeConfigYAML loads, selects and validates the project the way up
// would — including the port-mapping checks up runs while computing its
// gateway forwards, so a malformed port never passes config clean only to
// fail at up — then renders it through compose-go's own MarshalYAML, with
// one adjustment: hydrateConfigOutput and collapseNamedVolumes first make
// the model (and then the rendered bytes) reflect the computed/effective
// values 'up' itself acts on, not just what the file literally states. This
// is precisely docker compose config's output now, not a hand-built urunc
// rendering — the hydration exists only because MarshalYAML on its own
// cannot render a value that isn't literally in the model. Kept as its own
// function (rather than inlined into composeConfig) because the ported unit
// tests call it directly with a captured warning stream.
func composeConfigYAML(ctx context.Context, cmd *cli.Command) ([]byte, error) {
	p, err := loadProjectFromCLI(ctx, cmd, os.Stderr)
	if err != nil {
		return nil, err
	}
	p, err = selectProject(p, cmd.Args().Slice(), os.Stderr)
	if err != nil {
		return nil, err
	}
	order, err := startupOrder(p)
	if err != nil {
		return nil, err
	}
	if err := checkInstanceNames(p, projectName(cmd)); err != nil {
		return nil, err
	}
	// Mirrors the pass composeUp runs while computing its gateway forwards:
	// every port mapping is validated up front, in startup order, before
	// anything is rendered.
	for _, name := range order {
		for _, port := range p.Services[name].Ports {
			if _, _, _, err := resolvePort(port); err != nil {
				return nil, fmt.Errorf("service %q: %w", name, err)
			}
		}
	}
	hydrated, err := hydrateConfigOutput(p)
	if err != nil {
		return nil, err
	}
	raw, err := hydrated.MarshalYAML()
	if err != nil {
		return nil, err
	}
	return collapseNamedVolumes(raw, volumesRoot(cmd), projectName(cmd))
}

// hypervisorAliasCanonical mirrors the normalization switch run.go applies
// to the value serviceRunArgs passes it as --hypervisor (run.go, the
// "Normalize hypervisor names" switch): the only place that currently owns
// this mapping. x-hypervisor accepts these names because validateProject's
// supportedHypervisors allows them, but the backend that actually boots is
// the canonical name on the right. compose config's hydration needs the
// same fold so its rendered x-hypervisor matches what 'up' will actually
// run, not the literal alias the file wrote. Duplicated by hand rather than
// extracted into a shared function because the only other owner is deep
// inside run's VM-boot path, and reaching into that runtime code from the
// config-rendering path is a larger, riskier change than keeping these
// three lines in sync.
var hypervisorAliasCanonical = map[string]string{
	"qemu-hvf":       "qemu",
	"virtualization": "vz",
	"apple":          "vz",
}

// effectiveHypervisor returns the backend a booted service actually gets:
// compose.Hypervisor's literal value normalized through
// hypervisorAliasCanonical, or "vz" when the extension is unset entirely —
// the same default serviceRunArgs falls back to.
func effectiveHypervisor(svc types.ServiceConfig) string {
	hv := compose.Hypervisor(svc)
	if hv == "" {
		return "vz"
	}
	if canon, ok := hypervisorAliasCanonical[hv]; ok {
		return canon
	}
	return hv
}

// impliedOneShotJobs returns the set of services that run as jobs only
// because some other service gates on their completion — isOneShot is true
// but the service never wrote x-oneshot itself. Computed once, up front,
// from the project isOneShot itself never mutates: hydrateConfigOutput's
// per-service callback runs concurrently (WithServicesTransform), and
// isOneShot inspects every service's DependsOn, so it cannot safely run
// once per service inside that callback.
func impliedOneShotJobs(p *types.Project) map[string]bool {
	implied := make(map[string]bool)
	for name, svc := range p.Services {
		if isOneShot(p, name) && !compose.OneShot(svc) {
			implied[name] = true
		}
	}
	return implied
}

// hydrateConfigOutput returns a deep copy of p with every service's
// computed/effective urunc extensions written into the copy's Extensions
// map: the hypervisor backend that will actually boot (default or
// alias-normalized) when x-hypervisor doesn't already name it, and
// x-oneshot: true on a job that is only implied by a dependent's
// service_completed_successfully condition. p.MarshalYAML() renders only
// what is already in the typed model's fields, so compose config's output
// needs this hydration step to match what 'up' itself computes and acts on
// (compose.Hypervisor/OneShot and isOneShot are reused unchanged, not
// reimplemented).
//
// WithServicesTransform deep-copies the whole project (compose-go's
// generated deriveDeepCopyProject) before invoking the callback below, and
// that deep copy independently allocates each service's Extensions map —
// verified directly against derived.gen.go, not assumed — so mutating
// svc.Extensions here can never alias the original p's map. p itself is
// never mutated: a concurrent reload of the same project (up, the
// supervisor) sees its own untouched copy, not this one.
func hydrateConfigOutput(p *types.Project) (*types.Project, error) {
	implied := impliedOneShotJobs(p)
	return p.WithServicesTransform(func(name string, svc types.ServiceConfig) (types.ServiceConfig, error) {
		if hv := effectiveHypervisor(svc); hv != compose.Hypervisor(svc) {
			if svc.Extensions == nil {
				svc.Extensions = types.Extensions{}
			}
			svc.Extensions[compose.XHypervisorKey] = hv
		}
		if implied[name] {
			if svc.Extensions == nil {
				svc.Extensions = types.Extensions{}
			}
			svc.Extensions[compose.XOneShotKey] = true
		}
		// cpus renders as the whole vCPU count the VM actually gets, not the
		// fractional value declared. roundUpCpus is shared by up and config so
		// the two cannot drift, and validateProject already warned that the
		// value was rounded: printing the declared 0.5 here would contradict
		// both that warning and the --cpus 1 up passes to the backend.
		if svc.CPUS > 0 {
			if vcpus := roundUpCpus(float64(svc.CPUS)); float32(vcpus) != svc.CPUS {
				svc.CPUS = float32(vcpus)
			}
		}
		// mem_limit renders the whole-megabyte amount the VM actually gets.
		// serviceRunArgs converts with floor division, so a declared 3.5 MiB
		// boots a 3 MB VM; rendering the declared byte count here would
		// overstate it. The byte form itself is compose-go canonical and is
		// kept, only the value is floored to what --mem receives.
		if svc.MemLimit > 0 {
			if floored := svc.MemLimit / (1024 * 1024) * (1024 * 1024); floored != svc.MemLimit {
				svc.MemLimit = floored
			}
		}
		return svc, nil
	})
}

// collapseNamedVolumes rewrites each service's Type: volume entries from
// compose-go's long-form rendering (type/source/target/volume as separate
// mapping keys) into docker's compact "host:target[:ro]" string, with
// Source resolved to the managed store directory via resolveServiceVolume —
// the same function 'up' already calls (through serviceRunArgs) to build
// its --shared-dir flags, reused here rather than reimplemented.
//
// ServiceVolumeConfig has no MarshalYAML of its own (unlike ServiceConfig),
// so p.MarshalYAML() always renders long form no matter what Source holds:
// mutating the model cannot produce the compact form alone. This walks the
// already-marshaled document instead, targeting only entries whose 'type'
// key is exactly "volume" — bind mounts and every other volume type are
// left exactly as compose-go rendered them, matching the manifest's scope
// (only the named-volume synthesis gap, nothing cosmetic).
//
// The raw-bytes prefilter skips the decode/re-encode round trip entirely
// for the (overwhelming majority of) invocations with no named volume to
// collapse, so cases with no volumes at all cannot pick up incidental
// reformatting from this pass.
func collapseNamedVolumes(raw []byte, volRoot, project string) ([]byte, error) {
	if !bytes.Contains(raw, []byte("type: "+types.VolumeTypeVolume)) {
		return raw, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return raw, nil
	}
	services := mappingValue(doc.Content[0], "services")
	if services == nil {
		return raw, nil
	}
	for i := 1; i < len(services.Content); i += 2 {
		volumes := mappingValue(services.Content[i], "volumes")
		if volumes == nil || volumes.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range volumes.Content {
			if item.Kind != yaml.MappingNode || mappingScalar(item, "type") != types.VolumeTypeVolume {
				continue
			}
			v := types.ServiceVolumeConfig{
				Type:     types.VolumeTypeVolume,
				Source:   mappingScalar(item, "source"),
				Target:   mappingScalar(item, "target"),
				ReadOnly: mappingScalar(item, "read_only") == "true",
			}
			entry, err := resolveServiceVolume(v, volRoot, project)
			if err != nil {
				return nil, err
			}
			*item = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mappingValue returns the value node for key in the YAML mapping n, or nil
// when n is not a mapping or has no such key.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mappingScalar returns key's scalar value in the YAML mapping n, or "" when
// absent.
func mappingScalar(n *yaml.Node, key string) string {
	if v := mappingValue(n, key); v != nil {
		return v.Value
	}
	return ""
}

// startGatewayDaemon spawns this binary's hidden network-gateway command as
// a detached daemon and waits for its control socket to appear.
func startGatewayDaemon(cmd *cli.Command, s *store.Store, project, sockPath, apiPath, subnet, gatewayAddr string, forwards, dnsRecords []string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		return 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// The daemon supervises the project's services, so it
	// needs the same store this invocation used: without it the daemon would
	// poll the default store and see none of these instances. This is the
	// copy of the global-flag list that must never drift, so it shares the
	// one helper.
	args := selfExecGlobalArgs(cmd)
	args = append(args, "network-gateway", "--socket", sockPath, "--api", apiPath,
		"--qemu-socket", qemuGatewaySock(sockPath),
		"--subnet", subnet, "--gateway-ip", gatewayAddr,
		"--project", project)
	for _, f := range forwards {
		args = append(args, "--forward", f)
	}
	for _, r := range dnsRecords {
		args = append(args, "--host", r)
	}
	logFile, err := os.OpenFile(filepath.Join(filepath.Dir(sockPath), project+".gateway.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	c := exec.Command(exe, args...)
	c.Stdout = logFile
	c.Stderr = logFile
	// The daemon and every service it later re-invokes stay out of
	// telemetry: the compose command that spawned it already counts.
	c.Env = append(os.Environ(), telemetry.EnvSuppress+"=1")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, fmt.Errorf("failed to start network gateway: %w", err)
	}
	pid := c.Process.Pid
	_ = c.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Dial, don't stat: a stale socket file from a crashed gateway
		// satisfies Stat while nothing is listening.
		if conn, err := net.DialTimeout("unix", sockPath, time.Second); err == nil {
			_ = conn.Close()
			return pid, nil
		}
		if syscall.Kill(pid, 0) != nil {
			return 0, fmt.Errorf("network gateway exited at startup; see %s", filepath.Join(filepath.Dir(sockPath), project+".gateway.log"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return 0, errors.New("network gateway did not come up")
}

// stopGatewayDaemon terminates the project's gateway, best effort. The pid
// may be stale (crash + reuse), so the process identity is verified before
// signaling.
// gatewayStopWait bounds how long 'down' waits for the daemon to exit. It
// must exceed the daemon's own shutdown grace, or 'down' would give up
// while the daemon is still undoing a restart on purpose.
const gatewayStopWait = gatewayShutdownGrace + 5*time.Second

func stopGatewayDaemon(proj *composeProject) {
	if proj.SwitchPID > 0 && gatewayProcessMatches(proj.SwitchPID, proj.SwitchSock) {
		_ = syscall.Kill(proj.SwitchPID, syscall.SIGTERM)
		// Wait for it to go: the daemon uses its own grace to finish a
		// restart in flight and undo it, and instance names are
		// deterministic, so a survivor would squat the name and make this
		// project's next 'up' fail on a duplicate. Reporting "down" before
		// that settles would also be a lie.
		if !waitForExit(proj.SwitchPID, gatewayStopWait) {
			log.Warnf("network gateway %d did not exit within %s; a restart may still be in flight", proj.SwitchPID, gatewayStopWait)
		}
	}
	if proj.SwitchSock != "" {
		_ = os.Remove(proj.SwitchSock)
		_ = os.Remove(qemuGatewaySock(proj.SwitchSock))
	}
}

// gatewayProcessMatches reports whether pid's argv looks like the gateway
// that owns sockPath.
func gatewayProcessMatches(pid int, sockPath string) bool {
	out, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	argv := string(out)
	return strings.Contains(argv, "network-gateway") && strings.Contains(argv, sockPath)
}

// waitHealthy polls the gateway probe API until the TCP address answers.
func waitHealthy(apiSock, addr string, interval, timeout time.Duration) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 5 * time.Second,
	}
	deadline := time.Now().Add(timeout)
	url := "http://gateway/probe/tcp?addr=" + url.QueryEscape(addr)
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout probing %s", addr)
		}
		time.Sleep(interval)
	}
}

// offsetIP returns the network address plus n (IPv4), e.g. 10.87.0.0+10.
func offsetIP(network net.IP, n int) net.IP {
	ip4 := network.To4()
	v := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	v += uint32(n)
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// composeWarn writes one warning line to w. Every compose front-end warning
// goes through here so the lines stay stable and greppable: exactly one line
// per problem, all of them prefixed "warning: compose: ".
func composeWarn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "warning: compose: "+format+"\n", args...)
}
