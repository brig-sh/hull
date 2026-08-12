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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/urfave/cli/v3"

	"github.com/brig-sh/hull/internal/compose"
)

// writeComposeFile drops src in a fresh temp directory and returns its path.
func writeComposeFile(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadTestProject loads and validates src through the same two calls
// loadProjectFromCLI makes (compose.Load, then validateProject), the way
// every compose subcommand does, without going through the cli.Command flag
// layer. warnings captures everything either step wrote.
func loadTestProject(t *testing.T, src string) (*types.Project, string, error) {
	t.Helper()
	path := writeComposeFile(t, src)
	var warn bytes.Buffer
	p, err := compose.Load(context.Background(), compose.Options{
		Files:       []string{path},
		ProjectName: "proj",
		WorkingDir:  filepath.Dir(path),
		Environ:     os.Environ(),
		Warn:        &warn,
	})
	if err != nil {
		return nil, warn.String(), err
	}
	if err := validateProject(p, "", &warn); err != nil {
		return nil, warn.String(), err
	}
	return p, warn.String(), nil
}

// mustContain / mustNotContain are helpers for
// this package's own tests.
func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: expected to find %q in:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: did not expect %q in:\n%s", what, needle, haystack)
	}
}

// wantWarning asserts one exact warning line is present. Warnings are
// asserted line by line, never as a blob, so each capability's warning is
// independently checkable.
func wantWarning(t *testing.T, warnings, want string) {
	t.Helper()
	n := 0
	for _, line := range strings.Split(strings.TrimRight(warnings, "\n"), "\n") {
		if line == want {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warning line appeared %d times, want exactly 1:\n  want: %s\n  got:\n%s", n, want, warnings)
	}
}

func warningCount(warnings string) int {
	if strings.TrimSpace(warnings) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimRight(warnings, "\n"), "\n"))
}

// renderConfig loads, validates and selects src the way composeConfigYAML
// does, and renders it through compose-go's own MarshalYAML: this is
// docker-canonical output now, not a hand-built urunc rendering. It exists
// so tests that only care about the warning stream or an error, and tests
// pinned to the old normalizedConfig shape, can be told apart from tests
// that assert the new canonical shape.
func renderConfig(t *testing.T, src string) (out, warnings string, err error) {
	t.Helper()
	p, warn, err := loadTestProject(t, src)
	if err != nil {
		return "", warn, err
	}
	var buf bytes.Buffer
	sel, err := selectProject(p, nil, &buf)
	warn += buf.String()
	if err != nil {
		return "", warn, err
	}
	order, err := startupOrder(sel)
	if err != nil {
		return "", warn, err
	}
	if err := checkInstanceNames(sel, "proj"); err != nil {
		return "", warn, err
	}
	// Mirrors composeConfigYAML's own port-validation pass: config must
	// reject a malformed port mapping instead of only up catching it.
	for _, name := range order {
		for _, port := range sel.Services[name].Ports {
			if _, _, _, err := resolvePort(port); err != nil {
				return "", warn, fmt.Errorf("service %q: %w", name, err)
			}
		}
	}
	yamlOut, err := sel.MarshalYAML()
	if err != nil {
		return "", warn, err
	}
	return string(yamlOut), warn, nil
}

func TestStartupOrder(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"app":   {Name: "app", Image: "i", DependsOn: types.DependsOnConfig{"db": {}, "cache": {}}},
		"db":    {Name: "db", Image: "i"},
		"cache": {Name: "cache", Image: "i", DependsOn: types.DependsOnConfig{"db": {}}},
	}}
	order, err := startupOrder(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"db", "cache", "app"}) {
		t.Errorf("order = %v", order)
	}

	cyclic := &types.Project{Services: types.Services{
		"app": {Name: "app", Image: "i", DependsOn: types.DependsOnConfig{"db": {}}},
		"db":  {Name: "db", Image: "i", DependsOn: types.DependsOnConfig{"app": {}}},
	}}
	if _, err := startupOrder(cyclic); err == nil {
		t.Error("expected cycle error")
	} else if err.Error() != "dependency cycle in depends_on" {
		t.Errorf("cycle error = %q, want the exact bespoke wording", err)
	}
}

func TestCheckInstanceNames(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"web.api": {Name: "web.api", Image: "i"},
		"webapi":  {Name: "webapi", Image: "i"},
	}}
	err := checkInstanceNames(p, "proj")
	if err == nil {
		t.Fatal("expected a collision error")
	}
	want := `services "web.api" and "webapi" both resolve to instance name "proj-webapi"; set distinct container_name values`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestIsOneShot(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"job":   {Name: "job", Image: "i", Command: types.ShellCommand{"a"}, Extensions: types.Extensions{"x-oneshot": true}},
		"tgt":   {Name: "tgt", Image: "i", Command: types.ShellCommand{"a"}},
		"plain": {Name: "plain", Image: "i"},
		"dep": {Name: "dep", Image: "i", DependsOn: types.DependsOnConfig{
			"tgt":   {Condition: "service_completed_successfully"},
			"plain": {Condition: "service_started"},
		}},
	}}
	for name, want := range map[string]bool{"job": true, "tgt": true, "plain": false, "dep": false} {
		if got := isOneShot(p, name); got != want {
			t.Errorf("isOneShot(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestEnvSlice(t *testing.T) {
	set := "set"
	env := types.MappingWithEquals{"A": &set, "B": nil, "C": &set}
	if got, want := envSlice(env), []string{"A=set", "C=set"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envSlice = %v, want %v (nil values dropped, sorted)", got, want)
	}
}

func TestResolveServiceVolume(t *testing.T) {
	t.Run("named volume resolves under the managed store", func(t *testing.T) {
		got, err := resolveServiceVolume(types.ServiceVolumeConfig{
			Type: types.VolumeTypeVolume, Source: "pgdata", Target: "/var/lib/postgresql/data",
		}, "/store/volumes", "proj")
		if err != nil {
			t.Fatal(err)
		}
		if want := "/store/volumes/proj_pgdata:/var/lib/postgresql/data"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("bind mount passes its already-resolved source through", func(t *testing.T) {
		got, err := resolveServiceVolume(types.ServiceVolumeConfig{
			Type: types.VolumeTypeBind, Source: "/srv/static", Target: "/static", ReadOnly: true,
		}, "/store/volumes", "proj")
		if err != nil {
			t.Fatal(err)
		}
		if want := "/srv/static:/static:ro"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("an unsupported volume type is rejected loudly", func(t *testing.T) {
		if _, err := resolveServiceVolume(types.ServiceVolumeConfig{Type: types.VolumeTypeTmpfs}, "/vr", "proj"); err == nil {
			t.Fatal("a tmpfs mount must not silently become a --shared-dir with an empty source")
		}
	})
}

func TestResolvePort(t *testing.T) {
	t.Run("default host address is loopback", func(t *testing.T) {
		hostAddr, hostPort, guestPort, err := resolvePort(types.ServicePortConfig{Published: "8080", Target: 80, Protocol: "tcp"})
		if err != nil || hostAddr != "127.0.0.1" || hostPort != "8080" || guestPort != "80" {
			t.Errorf("resolvePort = %s,%s,%s,%v", hostAddr, hostPort, guestPort, err)
		}
	})
	t.Run("an explicit host address is kept", func(t *testing.T) {
		hostAddr, _, _, err := resolvePort(types.ServicePortConfig{HostIP: "0.0.0.0", Published: "8080", Target: 80})
		if err != nil || hostAddr != "0.0.0.0" {
			t.Errorf("resolvePort = %s, %v", hostAddr, err)
		}
	})
	t.Run("non-tcp is rejected", func(t *testing.T) {
		if _, _, _, err := resolvePort(types.ServicePortConfig{Published: "8080", Target: 80, Protocol: "udp"}); err == nil {
			t.Error("expected a non-TCP rejection")
		}
	})
	t.Run("the ephemeral single-port form is rejected", func(t *testing.T) {
		if _, _, _, err := resolvePort(types.ServicePortConfig{Target: 80, Protocol: "tcp"}); err == nil {
			t.Error("expected an ephemeral-port rejection")
		}
	})
}

// TestServiceRunArgs exercises the full field-mapping table serviceRunArgs
// implements: this is the whole point of the compose-go swap for the run
// layer, so every row gets its own assertion.
func TestServiceRunArgs(t *testing.T) {
	trueVal := "envvalue"
	svc := types.ServiceConfig{
		Name:     "web",
		Image:    "img:1",
		Command:  types.ShellCommand{"/app", "--serve"},
		MemLimit: 512 * 1024 * 1024, // 512m, exact
		CPUS:     1.5,
		Environment: types.MappingWithEquals{
			"A":          &trueVal,
			"UNRESOLVED": nil,
		},
		Volumes: []types.ServiceVolumeConfig{
			{Type: types.VolumeTypeBind, Source: "/srv/data", Target: "/data"},
			{Type: types.VolumeTypeVolume, Source: "vol", Target: "/var/lib/x"},
		},
		Extensions: types.Extensions{"x-hypervisor": "qemu"},
	}
	args, err := serviceRunArgs("web", "proj-web", svc, serviceLaunch{
		project:     "proj",
		gatewaySock: "/tmp/gw.sock",
		maskBits:    24,
		ip:          "10.87.0.10",
		volumesRoot: "/store/volumes",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--hypervisor qemu",
		"--mem 512",
		"--cpus 2", // 1.5 rounds up
		"--env A=envvalue",
		"--shared-dir /srv/data:/data",
		"--shared-dir /store/volumes/proj_vol:/var/lib/x",
		"-- img:1 /app --serve",
	} {
		mustContain(t, joined, want, "serviceRunArgs field mapping")
	}
	mustNotContain(t, joined, "UNRESOLVED", "an unresolved bare environment key must be dropped, not passed bare")

	t.Run("no mem_limit means no --mem flag", func(t *testing.T) {
		args, err := serviceRunArgs("web", "proj-web", types.ServiceConfig{Image: "i"}, serviceLaunch{maskBits: 24})
		if err != nil {
			t.Fatal(err)
		}
		mustNotContain(t, strings.Join(args, " "), "--mem", "MemLimit is UnitBytes(0) when unset; no floor-of-1 clamp")
	})

	t.Run("default hypervisor is vz", func(t *testing.T) {
		args, err := serviceRunArgs("web", "proj-web", types.ServiceConfig{Image: "i"}, serviceLaunch{maskBits: 24})
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, strings.Join(args, " "), "--hypervisor vz", "vz is the default backend")
	})

	t.Run("a one-shot launch runs the benign init instead of the service command", func(t *testing.T) {
		args, err := serviceRunArgs("job", "proj-job", types.ServiceConfig{Image: "i", Command: types.ShellCommand{"migrate"}},
			serviceLaunch{maskBits: 24, oneShot: true})
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, strings.Join(args, " "), strings.Join(oneshotInitCommand, " "), "a job's VM must boot the benign init")
		mustNotContain(t, strings.Join(args, " "), "migrate", "a job's own command must not be the VM's process")
	})

	t.Run("an unsupported volume type fails the whole build with the service named", func(t *testing.T) {
		bad := types.ServiceConfig{Image: "i", Volumes: []types.ServiceVolumeConfig{{Type: "tmpfs"}}}
		if _, err := serviceRunArgs("web", "proj-web", bad, serviceLaunch{maskBits: 24}); err == nil {
			t.Fatal("expected an error for an unsupported volume type")
		}
	})
}

// TestProjectHostEntries pins the --add-host contract: every service name
// maps to its IP, and a service whose container_name differs from its
// compose name gets a second entry for that name too.
func TestProjectHostEntries(t *testing.T) {
	services := types.Services{
		"web": {Name: "web", ContainerName: "thedb"},
		"db":  {Name: "db"},
	}
	ips := map[string]string{"web": "10.87.0.10", "db": "10.87.0.11"}
	got := projectHostEntries([]string{"web", "db"}, ips, "proj", services)
	// "db" has no container_name of its own, so its instance name
	// (project + "-" + service) still differs from the bare service name
	// and gets its own entry too, exactly as "web"/"thedb" does.
	want := []string{"web:10.87.0.10", "thedb:10.87.0.10", "db:10.87.0.11", "proj-db:10.87.0.11"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("projectHostEntries = %v, want %v", got, want)
	}
}

// TestValidateProject spot-checks the urunc-specific checks that survive
// compose-go's own schema and consistency pass: everything compose-go
// already enforces (image required, undefined depends_on target, undefined
// volume, healthcheck test prefix) is asserted black-box by the integration
// suite instead, not re-tested here.
func TestValidateProject(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			"bad container name",
			"services:\n  web:\n    image: img:1\n    container_name: Web/1\n",
			`service "web": invalid container_name "Web/1": only lowercase letters, digits, - and _ are allowed`,
		},
		{
			"unsupported hypervisor",
			"services:\n  web:\n    image: img:1\n    x-hypervisor: kvm\n",
			`service "web": unsupported x-hypervisor "kvm" (supported: vz, qemu)`,
		},
		{
			"oneshot needs a command",
			"services:\n  web:\n    image: img:1\n    x-oneshot: true\n",
			`service "web": x-oneshot needs a 'command' to run`,
		},
		{
			"healthy dependency without a probe",
			"services:\n  web:\n    image: img:1\n    depends_on:\n      db:\n        condition: service_healthy\n  db:\n    image: img:2\n",
			`service "web" requires "db" healthy, but it declares no healthcheck or x-healthcheck-tcp`,
		},
		{
			"completed dependency without a command",
			"services:\n  web:\n    image: img:1\n    depends_on:\n      db:\n        condition: service_completed_successfully\n  db:\n    image: img:2\n",
			`service "web" waits for "db" to complete, but "db" declares no 'command' to run`,
		},
		// compose-go's own JSON schema already requires the 'command'
		// property on a hook entry, so an entry that omits the key entirely
		// fails inside compose.Load with the schema's own wording before
		// validateProject ever runs; what survives to be tested here is the
		// narrower case the schema does not catch, an explicit empty list.
		{
			"post_start with an empty command",
			"services:\n  web:\n    image: img:1\n    post_start:\n      - command: []\n        user: root\n",
			`service "web": post_start[0]: command is required`,
		},
		{
			"pre_stop with an empty command",
			"services:\n  web:\n    image: img:1\n    pre_stop:\n      - command: []\n        user: root\n",
			`service "web": pre_stop[0]: command is required`,
		},
		{
			"healthcheck empty",
			"services:\n  web:\n    image: img:1\n    healthcheck:\n      interval: 2s\n",
			`service "web": healthcheck: 'test' is required (or set 'disable: true')`,
		},
		{
			"healthcheck missing command",
			"services:\n  web:\n    image: img:1\n    healthcheck:\n      test: [\"CMD\"]\n",
			`service "web": healthcheck: CMD needs a command`,
		},
		{
			"unsupported volume type",
			"services:\n  web:\n    image: img:1\n    volumes:\n      - type: tmpfs\n        target: /data\n",
			`service "web": volume type "tmpfs" is not supported (only bind mounts and named volumes are)`,
		},
		// A bare guest-only entry ("/data") is compose-go's anonymous-volume
		// short form: Type=volume, Source="". checkConsistency deliberately
		// does not validate it (docker mints a fresh unnamed volume per
		// container), but hull has no such mechanism and would
		// silently alias every anonymous volume in a project onto the same
		// "<volRoot>/<project>_" path.
		{
			"anonymous volume (bare guest path)",
			"services:\n  web:\n    image: img:1\n    volumes:\n      - /data\n",
			`service "web": anonymous volumes (a bare guest path with no host source or named volume) are not supported; declare a named volume or a bind mount`,
		},
		{
			"relative guest path",
			"services:\n  web:\n    image: img:1\n    volumes:\n      - /srv:data\n",
			`service "web": guest path "data" must be absolute`,
		},
		// serviceRunArgs floors MemLimit to whole megabytes; a value under
		// 1 MiB floors to 0 with nothing downstream to catch it before it
		// reaches the hypervisor as a zero-memory VM request. compose-go
		// accepts byte/k-suffixed mem_limit values the bespoke parser used
		// to reject outright, so this can no longer be caught upstream.
		{
			"mem_limit below 1 MiB",
			"services:\n  web:\n    image: img:1\n    mem_limit: 512k\n",
			`service "web": mem_limit 524288 bytes is below the 1 MiB minimum; it would round down to a 0 MB VM request`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadTestProject(t, tc.src)
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	t.Run("cpus rounding warns", func(t *testing.T) {
		_, warnings, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    cpus: 0.5\n")
		if err != nil {
			t.Fatal(err)
		}
		wantWarning(t, warnings, `warning: compose: service "web": cpus 0.50 rounded up to 1 vCPU(s) (shares are not supported, only whole vCPUs)`)
	})

	t.Run("x-healthcheck-tcp with no port warns", func(t *testing.T) {
		_, warnings, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    x-healthcheck-tcp: {}\n")
		if err != nil {
			t.Fatal(err)
		}
		wantWarning(t, warnings, `warning: compose: service "web": x-healthcheck-tcp is ignored: no port set`)
	})

	t.Run("declaring both healthchecks warns that the exec one wins", func(t *testing.T) {
		_, warnings, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n    x-healthcheck-tcp: 80\n")
		if err != nil {
			t.Fatal(err)
		}
		wantWarning(t, warnings, `warning: compose: service "web": both healthcheck and x-healthcheck-tcp declared; the exec healthcheck wins`)
	})

	t.Run("read-only volume warns it is mounted read-write", func(t *testing.T) {
		_, warnings, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    volumes:\n      - /srv:/data:ro\n")
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, warnings, "read-only is not enforced yet, mounting read-write", "a :ro volume must warn")
	})

	t.Run("mem_limit exactly at 1 MiB passes through unchanged", func(t *testing.T) {
		p, _, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    mem_limit: 1048576\n")
		if err != nil {
			t.Fatal(err)
		}
		svc, err := p.GetService("web")
		if err != nil {
			t.Fatal(err)
		}
		if svc.MemLimit != 1048576 {
			t.Errorf("MemLimit = %d, want 1048576 (1 MiB, the floor boundary) untouched", svc.MemLimit)
		}
	})
}

// TestSelectProject covers naming services on the command line: the
// dependency closure WithSelectedServices already gives, and the warning
// this layer adds for a name that exists only behind an inactive profile.
func TestSelectProject(t *testing.T) {
	const src = `
services:
  web:
    image: img:1
  debugger:
    image: img:2
    profiles:
      - debug
`
	t.Run("naming a service pulls in its dependencies", func(t *testing.T) {
		p, _, err := loadTestProject(t, `
services:
  backend:
    image: img:1
  db:
    image: img:2
  db-migrations:
    image: img:3
    command: ["migrate"]
    depends_on:
      - db
`)
		if err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		sel, err := selectProject(p, []string{"db-migrations"}, &warn)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"db", "db-migrations"}; !reflect.DeepEqual(sel.ServiceNames(), want) {
			t.Errorf("selected %v, want %v", sel.ServiceNames(), want)
		}
	})

	t.Run("naming a service activates its own profile", func(t *testing.T) {
		// docker's documented behavior (and the bespoke loader's before it):
		// naming a profile-gated service on the command line, without
		// --profile, starts it anyway. compose-go ships this as
		// WithServicesEnabled; selectProject must call it before narrowing
		// to the named services.
		p, _, err := loadTestProject(t, src)
		if err != nil {
			t.Fatal(err)
		}
		var warn bytes.Buffer
		sel, err := selectProject(p, []string{"debugger"}, &warn)
		if err != nil {
			t.Fatalf("naming a profile-gated service must activate its own profile, got: %v", err)
		}
		if want := []string{"debugger"}; !reflect.DeepEqual(sel.ServiceNames(), want) {
			t.Errorf("selected %v, want %v", sel.ServiceNames(), want)
		}
		if warn.String() != "" {
			t.Errorf("activating a service's own profile by naming it must not warn, got: %s", warn.String())
		}
	})

	t.Run("an unknown service name is reported as such", func(t *testing.T) {
		p, _, err := loadTestProject(t, src)
		if err != nil {
			t.Fatal(err)
		}
		_, err = selectProject(p, []string{"nope"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "no such service: nope") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no service enabled at all is an error", func(t *testing.T) {
		p, _, err := loadTestProject(t, "services:\n  web:\n    image: img:1\n    profiles: [dev]\n")
		if err != nil {
			t.Fatal(err)
		}
		_, err = selectProject(p, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "no service is enabled") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestLoadProjectFromCLIUsesComposeFileDirectory is the regression test for
// a real bug found and fixed during this task: loadProjectFromCLI initially
// set compose.Options.WorkingDir to os.Getwd() (the process's cwd), which
// broke default .env discovery for any --file pointing outside the
// invoking shell's cwd — both internal/compose.Load's own size-cap
// pre-check and compose-go's own default-.env discovery key off WorkingDir
// directly, and the bespoke loader always resolved the default .env beside
// the compose file itself, never the shell's cwd. This must keep working
// independent of the integration suite.
func TestLoadProjectFromCLIUsesComposeFileDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=fromdotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte("services:\n  web:\n    image: alpine:${TAG}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The invariant this test depends on: the process's actual cwd (wherever
	// `go test` runs from, cmd/hull/) must differ from dir. t.TempDir()
	// is never nested under the package directory, so this always holds; the
	// explicit check just makes a silently-vacuous test impossible.
	if cwd, err := os.Getwd(); err != nil || cwd == dir {
		t.Fatalf("test invariant broken: process cwd %q must differ from the fixture directory %q", cwd, dir)
	}

	root := &cli.Command{
		Name: "hull",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "file", Aliases: []string{"f"}},
			&cli.StringFlag{Name: "project-name", Aliases: []string{"p"}},
			&cli.StringSliceFlag{Name: "env-file"},
			&cli.StringSliceFlag{Name: "profile", Sources: cli.EnvVars("COMPOSE_PROFILES")},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p, err := loadProjectFromCLI(ctx, cmd, io.Discard)
			if err != nil {
				return err
			}
			svc, err := p.GetService("web")
			if err != nil {
				return err
			}
			if svc.Image != "alpine:fromdotenv" {
				t.Errorf("image = %q, want alpine:fromdotenv (the .env beside --file, not beside the process cwd)", svc.Image)
			}
			return nil
		},
		Writer: io.Discard,
	}
	t.Setenv("COMPOSE_PROFILES", "")
	// path is already absolute (under t.TempDir()), the same way a real
	// invocation's --file resolves once findComposeFile hands it onward —
	// this is deliberately not a path relative to dir, so the test cannot
	// pass by accident of the default-file-discovery fallback instead.
	if err := root.Run(context.Background(), []string{"hull", "--file", path}); err != nil {
		t.Fatal(err)
	}
}

// TestWarnUnmatchedProfiles pins the direct logic: a requested profile no
// service (enabled or still disabled) declares anywhere in the project must
// warn exactly once, a declared one must stay silent, and the wildcard and
// blank entries are never themselves "unmatched".
func TestWarnUnmatchedProfiles(t *testing.T) {
	p := &types.Project{
		Services:         types.Services{"web": {Name: "web"}},
		DisabledServices: types.Services{"debugger": {Name: "debugger", Profiles: []string{"debug"}}},
	}
	var warn bytes.Buffer
	warnUnmatchedProfiles(p, []string{"debug", "typo", "typo", "  ", allProfiles}, &warn)
	wantWarning(t, warn.String(), `warning: compose: profile "typo" is active, but no service declares it`)
	if got := warningCount(warn.String()); got != 1 {
		t.Errorf("got %d warning lines, want exactly 1 (declared/blank/wildcard/duplicate must stay silent):\n%s", got, warn.String())
	}
}

// TestWarnUnmatchedProfilesIsWired proves the wiring, not just the helper:
// loadProjectFromCLI, driven through the real flag-parsing path
// (--profile), must actually call warnUnmatchedProfiles with what the CLI
// parsed. This is also what makes a typo'd --profile distinguishable from
// success for a real invocation, not just for a direct call to the helper.
func TestWarnUnmatchedProfilesIsWired(t *testing.T) {
	path := writeComposeFile(t, "services:\n  web:\n    image: img:1\n")
	var warn bytes.Buffer
	var loadErr error
	// A minimal command tree carrying the same --file/--profile flags
	// composeCommand() registers, with an Action that calls
	// loadProjectFromCLI directly so the warning stream can be captured
	// (composeConfig itself hardcodes os.Stderr, which a unit test should
	// not have to redirect to prove this).
	root := &cli.Command{
		Name: "hull",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "file", Aliases: []string{"f"}},
			&cli.StringFlag{Name: "project-name", Aliases: []string{"p"}},
			&cli.StringSliceFlag{Name: "env-file"},
			&cli.StringSliceFlag{Name: "profile", Sources: cli.EnvVars("COMPOSE_PROFILES")},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, loadErr = loadProjectFromCLI(ctx, cmd, &warn)
			return nil
		},
		Writer: io.Discard,
	}
	t.Setenv("COMPOSE_PROFILES", "")
	if err := root.Run(context.Background(), []string{"hull", "--file", path, "--profile", "nope"}); err != nil {
		t.Fatal(err)
	}
	if loadErr != nil {
		t.Fatalf("loadProjectFromCLI: %v", loadErr)
	}
	mustContain(t, warn.String(), `profile "nope" is active, but no service declares it`,
		"a typo'd --profile must not be indistinguishable from success")
}

func TestComposeConfigCanonicalOutput(t *testing.T) {
	out, warnings, err := renderConfig(t, "services:\n  web:\n    image: img:1\n")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "name: proj", "the project name must render")
	mustContain(t, out, "image: img:1", "the service must render")
	if warnings != "" {
		t.Errorf("unexpected warnings for a fully supported file:\n%s", warnings)
	}
}

// TestComposeConfigValidatesPorts pins that 'config' rejects a port mapping
// run cannot forward, the same way 'up' does, instead of only failing once
// 'up' tries to compute its gateway forwards. composeConfigYAML's own doc
// comment claims config "validates the project the way up would" — this is
// the check that makes that claim true for ports specifically.
//
// This calls the real composeConfigYAML(ctx, cmd) directly, driven through
// actual flag parsing — the same pattern TestWarnUnmatchedProfilesIsWired
// uses — rather than renderConfig, which hand-reimplements
// composeConfigYAML's load/select/validate/render sequence (including its
// own copy of the port-validation loop) for tests that only need the
// warning stream or an error and don't care whether the *production*
// entry point performs a given check. A prior version of this test called
// renderConfig and kept passing even with the production port-validation
// loop deleted, because it was really exercising renderConfig's own
// hand-copied loop — proving nothing about composeConfigYAML itself. See
// this test's own revert-proof, recorded in the task report, for the
// empirical check that this version does not have the same problem.
func TestComposeConfigValidatesPorts(t *testing.T) {
	path := writeComposeFile(t, "services:\n  web:\n    image: img:1\n    ports:\n      - \"8080:80/udp\"\n")
	var out []byte
	var callErr error
	root := &cli.Command{
		Name: "hull",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "file", Aliases: []string{"f"}},
			&cli.StringFlag{Name: "project-name", Aliases: []string{"p"}},
			&cli.StringSliceFlag{Name: "env-file"},
			&cli.StringSliceFlag{Name: "profile", Sources: cli.EnvVars("COMPOSE_PROFILES")},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out, callErr = composeConfigYAML(ctx, cmd)
			return nil
		},
		Writer: io.Discard,
	}
	t.Setenv("COMPOSE_PROFILES", "")
	if err := root.Run(context.Background(), []string{"hull", "--file", path, "--project-name", "proj"}); err != nil {
		t.Fatal(err)
	}
	if callErr == nil {
		t.Fatalf("a non-TCP port mapping must fail composeConfigYAML, not just serviceRunArgs; got output:\n%s", out)
	}
	mustContain(t, callErr.Error(), `service "web"`, "the error must name the service")
	mustContain(t, callErr.Error(), "udp", "the error must say why the mapping was rejected")
}

// runComposeConfigYAML drives the real composeConfigYAML production entry
// point through cli flag parsing, the same pattern
// TestComposeConfigValidatesPorts uses and for the same reason: renderConfig
// hand-copies composeConfigYAML's load/select/validate/render sequence
// without the hydration step these tests exist to pin, so calling it would
// prove nothing about whether hydrateConfigOutput and collapseNamedVolumes
// are actually wired into the command a user runs.
func runComposeConfigYAML(t *testing.T, storeDir, src string) ([]byte, error) {
	t.Helper()
	path := writeComposeFile(t, src)
	var out []byte
	var callErr error
	root := &cli.Command{
		Name: "hull",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "file", Aliases: []string{"f"}},
			&cli.StringFlag{Name: "project-name", Aliases: []string{"p"}},
			&cli.StringFlag{Name: "store-dir"},
			&cli.StringSliceFlag{Name: "env-file"},
			&cli.StringSliceFlag{Name: "profile", Sources: cli.EnvVars("COMPOSE_PROFILES")},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out, callErr = composeConfigYAML(ctx, cmd)
			return nil
		},
		Writer: io.Discard,
	}
	t.Setenv("COMPOSE_PROFILES", "")
	if err := root.Run(context.Background(),
		[]string{"hull", "--file", path, "--project-name", "proj", "--store-dir", storeDir}); err != nil {
		t.Fatal(err)
	}
	return out, callErr
}

// TestComposeConfigHydratesHypervisorDefault pins the regression this task
// fixes (issue #125): an unset x-hypervisor must render the effective
// default "vz", and a documented alias ("apple") must render its canonical
// name — neither is literally present in the loaded model, so
// p.MarshalYAML() alone cannot produce either without hydrateConfigOutput.
func TestComposeConfigHydratesHypervisorDefault(t *testing.T) {
	store := t.TempDir()
	out, err := runComposeConfigYAML(t, store, "services:\n  web:\n    image: alpine:3.19\n")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "x-hypervisor: vz", "unset x-hypervisor must render the effective default")

	out, err = runComposeConfigYAML(t, store, "services:\n  web:\n    image: alpine:3.19\n    x-hypervisor: apple\n")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "x-hypervisor: vz", "the apple alias must render its canonical name, vz")
}

// TestComposeConfigHydratesImpliedOneShot mirrors the integration suite's
// caseOneShot "Implied" sub-case: a service named only by a dependent's
// service_completed_successfully condition must render x-oneshot: true on
// its own block, exactly once, even though it never declares the key
// itself. Counting the marker (not just finding it) is what proves the
// dependent did not also get marked.
func TestComposeConfigHydratesImpliedOneShot(t *testing.T) {
	store := t.TempDir()
	out, err := runComposeConfigYAML(t, store,
		"services:\n  migrate:\n    image: alpine:3.19\n    command: [\"/bin/true\"]\n"+
			"  app:\n    image: alpine:3.19\n    depends_on:\n      migrate:\n"+
			"        condition: service_completed_successfully\n")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if n := strings.Count(got, "x-oneshot: true"); n != 1 {
		t.Fatalf("want exactly 1 x-oneshot: true marker, got %d in:\n%s", n, got)
	}
	mIdx := strings.Index(got, "  migrate:")
	oIdx := strings.Index(got, "x-oneshot: true")
	if mIdx < 0 || oIdx < 0 || oIdx < mIdx {
		t.Errorf("the marker must land on the gated dependency (migrate), not the dependent; got:\n%s", got)
	}
}

// TestComposeConfigHydratesNamedVolumePath pins the resolved-host-path half
// of toplevel.volumes: a service's named-volume entry must render the
// managed store directory ("<store>/volumes/<project>_<name>"), the same
// path 'up' computes via resolveServiceVolume, not the bare declared volume
// name compose-go's loader keeps in Source. A bind mount in the same
// fixture is asserted to stay in compose-go's long form, pinning that the
// Type == "volume" scope in collapseNamedVolumes does not widen to every
// volume kind.
func TestComposeConfigHydratesNamedVolumePath(t *testing.T) {
	store := t.TempDir()
	bindDir := t.TempDir()
	out, err := runComposeConfigYAML(t, store,
		"volumes:\n  vdata: {}\nservices:\n  web:\n    image: alpine:3.19\n    volumes:\n"+
			"      - vdata:/data\n      - "+bindDir+":/host\n")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	mustContain(t, got, filepath.Join(store, "volumes", "proj_vdata")+":/data",
		"a declared named volume must resolve to <store>/volumes/<project>_<name>")
	mustContain(t, got, "type: bind", "a bind mount must stay in compose-go's long form")
	mustNotContain(t, got, bindDir+":/host", "a bind mount must not be collapsed to a compact source:target string")
}

// TestHydrateConfigOutputDoesNotMutateOriginal closes the aliasing question
// empirically rather than by reading derived.gen.go: after
// hydrateConfigOutput returns, the project it was called with must still
// report an unset x-hypervisor, proving the deep copy WithServicesTransform
// performs really does give each service its own Extensions map. A future
// compose-go upgrade that changed deriveDeepCopyProject's behavior would
// fail this test, not just the source-reading argument in the task report.
func TestHydrateConfigOutputDoesNotMutateOriginal(t *testing.T) {
	p, warn, err := loadTestProject(t, "services:\n  web:\n    image: alpine:3.19\n")
	if err != nil {
		t.Fatalf("loadTestProject: %v (warnings: %s)", err, warn)
	}
	hydrated, err := hydrateConfigOutput(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := compose.Hypervisor(hydrated.Services["web"]); got != "vz" {
		t.Fatalf("hydrated copy: want x-hypervisor vz, got %q", got)
	}
	if got := compose.Hypervisor(p.Services["web"]); got != "" {
		t.Errorf("hydrateConfigOutput mutated the original project's service: x-hypervisor is now %q, want unset", got)
	}
}
