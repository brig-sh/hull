// Copyright (c) 2026, NOFire AI
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

// TestConformanceStatic is the executable half of ADR 0002's "every claim is
// executable" invariant, for the static tier (issue #58). It runs one subtest
// per static-tier manifest entry, named exactly by the capability id so that
// TestConformanceStatic/<id> matches the manifest's `test` field, and asserts:
//
//	supported   - the capability renders per spec, exit 0, no warning about it.
//	partial     - the exact documented divergence from the entry's notes
//	              reproduces (rounding, normalization, targeted rejection).
//	unsupported - the key produces a loud warning line (or a validation error);
//	              a silent drop is a conformance failure.
//
// Runtime-tier entries are exempt here; they arrive in issue #59.

package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMain resolves the black-box binary once for the whole package (honoring
// HULL_BIN, else building on darwin) and removes any temp build dir on
// teardown. It must live in a _test.go file for the test runner to recognize
// it; the locate/build logic itself is resolveBinary in harness.go.
func TestMain(m *testing.M) {
	var buildDir string
	var err error
	binPath, skipReason, buildDir, err = resolveBinary()
	if err != nil {
		// A failed build or a broken HULL_BIN is a failure, never a
		// skip: green-by-skip on a non-compiling SUT is the worst outcome.
		fmt.Fprintln(os.Stderr, "conformance: ", err)
		os.Exit(1)
	}
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// TestSourcesAreCacheInputs reads every source file of the binary under test
// so the go test cache records them as inputs. This package imports nothing
// from cmd/hull — the binary is built by shelling out — so without
// these reads a cached PASS would survive changes to the very parser the
// suite exists to test. The reads must happen inside a test function: the
// testing framework only records cache inputs during m.Run, so doing this in
// TestMain records nothing.
func TestSourcesAreCacheInputs(t *testing.T) {
	if err := recordSourceDeps(); err != nil {
		t.Fatal(err)
	}
}

// staticCase is one capability's check against the black-box binary.
type staticCase func(t *testing.T)

// staticCases builds the id -> case registry. Supported/partial entries and
// the two special unsupported entries (spec.interpolation, the cli verbs) get
// explicit handlers; the remaining unsupported top-level and service keys are
// data-driven from the manifest — one generated fixture per key — so the ~100
// "ignoring unsupported key" cases are not hand-written.
func staticCases(m *Manifest) map[string]staticCase {
	cases := explicitCases()
	for _, c := range m.Capabilities {
		if c.Tier != TierStatic {
			continue
		}
		if _, ok := cases[c.ID]; ok {
			continue
		}
		// out-of-scope is tested identically to unsupported: scoping changes
		// the score's denominator, never the loud-warning contract (ADR-0003).
		if c.Status != StatusUnsupported && c.Status != StatusOutOfScope {
			continue // supported/partial without an explicit handler: the guard flags it.
		}
		switch {
		case strings.HasPrefix(c.ID, "toplevel."):
			cases[c.ID] = unsupportedTopLevelCase(strings.TrimPrefix(c.ID, "toplevel."))
		case strings.HasPrefix(c.ID, "service."):
			cases[c.ID] = unsupportedServiceCase(strings.TrimPrefix(c.ID, "service."))
		case strings.HasPrefix(c.ID, "cli."):
			cases[c.ID] = cliAbsentCase(strings.TrimPrefix(c.ID, "cli."))
		}
	}
	return cases
}

// explicitCases are the hand-written handlers for the capabilities whose
// behavior cannot be data-driven from a key name alone.
func explicitCases() map[string]staticCase {
	return map[string]staticCase{
		"toplevel.services":      caseTopLevelServices,
		"service.image":          caseServiceImage,
		"service.command":        caseServiceCommand,
		"service.container_name": caseContainerName,
		"service.environment":    caseEnvironment,
		"service.mem_limit":      caseMemLimit,
		"service.cpus":           caseCpus,
		"service.restart":        caseRestart,
		"ext.x-hypervisor":       caseHypervisor,
		"ext.x-oneshot":          caseOneShot,
		"cli.config":             caseCliConfig,
		"spec.interpolation":     caseInterpolation,
		"toplevel.volumes":       caseNamedVolumeDeclarations,
		"toplevel.include":       caseInclude,
		"service.env_file":       caseEnvFile,
		"service.profiles":       caseProfiles,
		"service.extends":        caseExtends,
		"service.label_file":     caseLabelFile,
	}
}

// profilesFixture is one core service and two profiled ones, the smallest file
// that separates "always starts" from "starts only when activated".
const profilesFixture = `services:
  web:
    image: alpine:3.19
  debugger:
    image: alpine:3.19
    profiles:
      - debug
  seeder:
    image: alpine:3.19
    profiles:
      - debug
      - tools
`

func caseProfiles(t *testing.T) {
	// A service without profiles always starts; a service with profiles does
	// not, and neither is warned about — profiles are read, not dropped.
	base := runConfig(t, writeFixture(t, profilesFixture))
	requireExit0(t, base)
	requireNoWarnings(t, base)
	mustContain(t, base.stdout, "web:", "a service without profiles must always be enabled")
	mustNotContain(t, base.stdout, "debugger:", "a profiled service must not start with no profile active")
	mustNotContain(t, base.stdout, "seeder:", "a profiled service must not start with no profile active")

	// --profile is repeatable and sits on the compose command, beside -f/-p.
	path := writeFixture(t, profilesFixture)
	one := runBinary(t, "compose", "--file", path, "--project-name", projectName, "--profile", "tools", "config")
	requireExit0(t, one)
	mustContain(t, one.stdout, "seeder:", "an active profile must enable the services that declare it")
	mustNotContain(t, one.stdout, "debugger:", "an inactive profile must stay inactive")
	mustContain(t, one.stdout, "profiles:", "the declared profiles must survive into the rendered config")

	// The wildcard activates every declared profile.
	all := runBinary(t, "compose", "--file", path, "--project-name", projectName, "--profile", "*", "config")
	requireExit0(t, all)
	mustContain(t, all.stdout, "debugger:", "'*' must activate every declared profile")
	mustContain(t, all.stdout, "seeder:", "'*' must activate every declared profile")

	// COMPOSE_PROFILES is the environment form, comma-separated.
	env := runBinaryEnv(t, []string{"COMPOSE_PROFILES=debug,tools"},
		"compose", "--file", path, "--project-name", projectName, "config")
	requireExit0(t, env)
	mustContain(t, env.stdout, "debugger:", "COMPOSE_PROFILES must activate the profiles it lists")
	mustContain(t, env.stdout, "seeder:", "COMPOSE_PROFILES must split its value on commas")

	// An activated profile no service declares warns: nothing extra starts,
	// which is indistinguishable from success without the line.
	typo := runBinary(t, "compose", "--file", path, "--project-name", projectName, "--profile", "nope", "config")
	requireExit0(t, typo)
	mustContain(t, typo.stderr, `profile "nope" is active, but no service declares it`,
		"an unmatched profile must warn")

	// Divergence, per the manifest notes: a dependency no active profile
	// enables is an error, not an automatic activation. compose-go's own
	// consistency check (run inside compose.Load, before hull's code
	// ever sees the project) raises this, in its own wording — the disabled
	// target is invisible to GetService, so it reads exactly like a
	// depends_on target that does not exist at all, which is the structural
	// reason it does not name the enabling profile the way the bespoke
	// loader's own message used to.
	gated := runConfig(t, writeFixture(t, `services:
  web:
    image: alpine:3.19
    depends_on:
      - db
  db:
    image: alpine:3.19
    profiles:
      - storage
`))
	requireExitNonZero(t, gated)
	mustContain(t, gated.stderr, `service "web" depends on undefined service "db"`,
		"a dependency disabled by profiles must fail (compose-go's own consistency-check wording)")

	// docker's escape hatch for the case above: 'required: false' warns and
	// drops the edge, and the dependent starts without it.
	optional := runConfig(t, writeFixture(t, `services:
  web:
    image: alpine:3.19
    depends_on:
      db:
        condition: service_started
        required: false
  db:
    image: alpine:3.19
    profiles:
      - storage
`))
	requireExit0(t, optional)
	mustContain(t, optional.stderr, `optional dependency "db" is not enabled by any active profile`,
		"an optional dependency disabled by profiles must warn")
	mustContain(t, optional.stdout, "web:", "the dependent must still start")
	mustNotContain(t, optional.stdout, "db:", "the disabled dependency must not start")

	// Naming a service starts it whatever its profiles say, together with its
	// depends_on closure and nothing else. This is docker's own example from
	// the profiles documentation.
	selPath := writeFixture(t, `services:
  backend:
    image: alpine:3.19
  db:
    image: alpine:3.19
  db-migrations:
    image: alpine:3.19
    command: ["migrate"]
    depends_on:
      - db
    profiles:
      - tools
  debugger:
    image: alpine:3.19
    profiles:
      - tools
`)
	named := runBinary(t, "compose", "--file", selPath, "--project-name", projectName, "config", "db-migrations")
	requireExit0(t, named)
	mustContain(t, named.stdout, "db-migrations:", "a named service must start whatever its profiles say")
	mustContain(t, named.stdout, "db:", "a named service's declared dependency must come with it")
	// The sibling rule: naming a service does not activate its profile, so a
	// service that only shares that profile stays out. An implementation that
	// folds the name's profiles into the active set fails exactly here.
	mustNotContain(t, named.stdout, "debugger:",
		"naming a service must not start another service that shares its profile")
	mustNotContain(t, named.stdout, "backend:",
		"naming a service must not start unrelated services")

	// Naming a service does not excuse another enabled service's
	// unsatisfiable dependency: the whole enabled project is validated before
	// the file is narrowed to the named services. Verified against docker
	// compose v5.1.1, which fails the same file the same way.
	unrelated := writeFixture(t, `services:
  web:
    image: alpine:3.19
  other:
    image: alpine:3.19
    depends_on:
      - db
  db:
    image: alpine:3.19
    profiles:
      - storage
`)
	scoped := runBinary(t, "compose", "--file", unrelated, "--project-name", projectName, "config", "web")
	requireExitNonZero(t, scoped)
	mustContain(t, scoped.stdout+scoped.stderr, `service "other" depends on undefined service "db"`,
		"naming one service must still report another enabled service's gated dependency (compose-go's own wording)")

	unknown := runBinary(t, "compose", "--file", selPath, "--project-name", projectName, "config", "nosuch")
	requireExitNonZero(t, unknown)
	mustContain(t, unknown.stdout+unknown.stderr, "no such service: nosuch",
		"an unknown service name must be reported as such")

	// A depends_on target that no active profile enables is still validated
	// (a required target's absence is checked cross-service before profile
	// filtering ever narrows anything further, per the 'gated'/'unrelated'
	// cases above). What is NOT validated is a disabled service's own
	// shape: compose-go's checkConsistency runs after WithProfiles
	// (loader.go), so a profile-disabled service is invisible to it —
	// 'broken' below has no image and stays disabled, and load succeeds.
	// This is compose-go's (and so docker's) own load order, not a gap
	// hull introduces; the assertion is pinned so a future
	// compose-go bump that reorders this is caught rather than silently
	// passing.
	invalid := runConfig(t, writeFixture(t, `services:
  web:
    image: alpine:3.19
  broken:
    profiles:
      - debug
`))
	requireExit0(t, invalid)
	mustNotContain(t, invalid.stderr, "image",
		"a service disabled by an inactive profile is not shape-checked (its missing image is never seen)")
	mustContain(t, invalid.stdout, "web:", "the enabled service must still render")
	mustNotContain(t, invalid.stdout, "broken:", "the disabled service must not render")
}

func TestConformanceStatic(t *testing.T) {
	// Record source deps here too: a developer running with -run
	// 'TestConformanceStatic' would otherwise filter out the recording test
	// and resurrect the stale-cache hole.
	if err := recordSourceDeps(); err != nil {
		t.Fatal(err)
	}
	if skipReason != "" {
		t.Skip(skipReason)
	}
	m := loadForTest(t)
	cases := staticCases(m)
	for _, c := range m.Capabilities {
		if c.Tier != TierStatic {
			continue
		}
		fn, ok := cases[c.ID]
		if !ok {
			// The coverage guard reports this authoritatively; fail the
			// subtest too so a run surfaces it by id.
			t.Run(c.ID, func(t *testing.T) {
				t.Fatalf("no conformance case registered for static entry %s", c.ID)
			})
			continue
		}
		t.Run(c.ID, fn)
	}
}

// TestConformanceStaticCoverage is the manifest<->suite coverage guard: every
// static-tier entry must have a registered case and every registered case must
// map to a static-tier entry. It needs no binary, so it runs (and must pass)
// on every host, including where TestConformanceStatic skips.
func TestConformanceStaticCoverage(t *testing.T) {
	if err := recordSourceDeps(); err != nil {
		t.Fatal(err)
	}
	m := loadForTest(t)
	cases := staticCases(m)

	staticIDs := map[string]bool{}
	for _, c := range m.Capabilities {
		if c.Tier == TierStatic {
			staticIDs[c.ID] = true
		}
	}

	var missing []string
	for id := range staticIDs {
		if _, ok := cases[id]; !ok {
			missing = append(missing, id)
		}
	}
	var stray []string
	for id := range cases {
		if !staticIDs[id] {
			stray = append(stray, id)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("static manifest entries with no registered case (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("registered cases with no static manifest entry (%d):\n  %s",
			len(stray), strings.Join(stray, "\n  "))
	}
}

// -- supported ---------------------------------------------------------------

func caseTopLevelServices(t *testing.T) {
	res := runConfig(t, fixturePath(t, "base.yml"))
	requireExit0(t, res)
	mustContain(t, res.stdout, "services:", "top-level services must render")
	mustContain(t, res.stdout, "web:", "the defined service must render")
	requireNoWarnings(t, res)
}

func caseServiceImage(t *testing.T) {
	res := runConfig(t, fixturePath(t, "base.yml"))
	requireExit0(t, res)
	mustContain(t, res.stdout, "image: alpine:3.19", "image must render verbatim")
	requireNoWarnings(t, res)
}

func caseServiceCommand(t *testing.T) {
	res := runConfig(t, fixturePath(t, "command.yml"))
	requireExit0(t, res)
	// The string form shell-word splits into a list.
	for _, word := range []string{"- echo", "- hello", "- world"} {
		mustContain(t, res.stdout, word, "string command must split into a list")
	}
	requireNoWarnings(t, res)
}

// -- partial (exact documented divergence) -----------------------------------

func caseContainerName(t *testing.T) {
	// Divergence: uppercase AND dots are rejected with an error, not silently
	// renamed (both documented in the manifest notes).
	res := runConfig(t, fixturePath(t, "container_name_uppercase.yml"))
	requireExitNonZero(t, res)
	mustContain(t, res.stderr, "invalid container_name",
		"an uppercase container_name must be rejected")
	dotted := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    container_name: web.1\n"))
	requireExitNonZero(t, dotted)
	mustContain(t, dotted.stderr, "invalid container_name",
		"a dotted container_name must be rejected")
}

func caseEnvironment(t *testing.T) {
	// Map and list (KEY=VAL) forms are both accepted, and both normalize to
	// a sorted KEY: VAL mapping in canonical output. A bare/null key with no
	// host value renders null; the whole block is asserted at once, since
	// an independent "EMPTY: null" contains would also pass in any order.
	res := runConfig(t, fixturePath(t, "environment.yml"))
	requireExit0(t, res)
	mustContain(t, res.stdout, "environment:\n      EMPTY: null\n      FOO: bar\n",
		"map env must normalize to a sorted mapping, with an unset bare key rendering null")
	requireNoWarnings(t, res)
	// The list form is accepted and normalizes to the same mapping shape.
	list := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    environment:\n      - FOO=bar\n"))
	requireExit0(t, list)
	mustContain(t, list.stdout, "environment:\n      FOO: bar\n", "list env must normalize to a mapping")

	// A bare key DOES inherit the host's value now (the bespoke loader's
	// documented divergence is gone): proven in both the map form (a null
	// value) and the list form (a bare name, no '='), with a host-process
	// env var of the same name actually set.
	mapFixture := writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    environment:\n      CONF_ENV_INHERIT_42:\n")
	inheritedMap := runBinaryEnv(t, []string{"CONF_ENV_INHERIT_42=frommap"},
		"compose", "--file", mapFixture, "--project-name", projectName, "config")
	requireExit0(t, inheritedMap)
	mustContain(t, inheritedMap.stdout, "CONF_ENV_INHERIT_42: frommap",
		"a bare map-form key must inherit the host's value")

	listFixture := writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    environment:\n      - CONF_ENV_INHERIT_LIST_42\n")
	inheritedList := runBinaryEnv(t, []string{"CONF_ENV_INHERIT_LIST_42=fromlist"},
		"compose", "--file", listFixture, "--project-name", projectName, "config")
	requireExit0(t, inheritedList)
	mustContain(t, inheritedList.stdout, "CONF_ENV_INHERIT_LIST_42: fromlist",
		"a bare list-form key must inherit the host's value")
}

func caseMemLimit(t *testing.T) {
	// Divergence: 'compose config' renders a byte count, quoted, not a
	// normalized "<N>m" string — so 1g renders as "1073741824", not
	// "1024m". The byte form is compose-go canonical; the value is the
	// whole-megabyte amount the VM actually gets, floored the same way
	// serviceRunArgs floors it for --mem, so config cannot overstate the
	// VM's memory (see the truncating case below).
	res := runConfig(t, fixturePath(t, "mem_limit.yml"))
	requireExit0(t, res)
	mustContain(t, res.stdout, `mem_limit: "1073741824"`, "1g must render as its raw byte count")
	requireNoWarnings(t, res)
	// Divergence: compose-go's unit vocabulary is wider than the bespoke
	// parser's — k/b suffixes are accepted (not rejected as "too small"),
	// parsed to their raw byte count like any other unit. 2048k sits above
	// the 1 MiB floor below, so this is purely the unit-parsing assertion.
	small := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    mem_limit: 2048k\n"))
	requireExit0(t, small)
	mustContain(t, small.stdout, `mem_limit: "2097152"`, "k units are accepted, parsed to their byte count")
	// A value that is not a whole number of megabytes floors to the amount
	// the VM actually receives: 3.5 MiB declared boots a 3 MB VM, so config
	// must render 3 MiB, not the declared 3670016. Anchored on a
	// non-multiple deliberately — an exact multiple cannot tell flooring
	// and rendering-as-declared apart.
	trunc := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    mem_limit: 3670016\n"))
	requireExit0(t, trunc)
	mustContain(t, trunc.stdout, `mem_limit: "3145728"`, "a fractional-MB byte count must floor to what --mem receives")
	// A value under 1 MiB would floor to 0 MB (--mem 0) at the run layer
	// with nothing there to catch it; validateProject rejects it here
	// instead, at load/validate time, naming the service and the byte
	// value that would have been silently zeroed out.
	tooSmall := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    mem_limit: 512k\n"))
	requireExitNonZero(t, tooSmall)
	mustContain(t, tooSmall.stdout+tooSmall.stderr,
		`service "web": mem_limit 524288 bytes is below the 1 MiB minimum`,
		"a sub-1-MiB mem_limit must be rejected, not silently floored to a 0 MB VM request")
}

func caseCpus(t *testing.T) {
	// Divergence: fractional cpus rounds UP to a whole vCPU with a warning,
	// and config renders that rounded count so it cannot drift from what
	// up passes as --cpus. Docker renders the declared fraction instead,
	// but docker can actually deliver it; urunc allocates whole vCPUs only.
	// The newline anchors matter: "cpus: 1" alone also matches "cpus: 16".
	res := runConfig(t, fixturePath(t, "cpus.yml"))
	requireExit0(t, res)
	mustContain(t, res.stdout, "cpus: 1\n", "0.5 cpus must render as exactly 1 vCPU")
	mustContain(t, res.stderr, "rounded up to 1 vCPU(s)", "the rounding must warn")
	// The quoted-string form is accepted and rounds identically.
	quoted := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    cpus: \"0.5\"\n"))
	requireExit0(t, quoted)
	mustContain(t, quoted.stdout, "cpus: 1\n", "quoted cpus must round the same way")
	mustContain(t, quoted.stderr, "rounded up to 1 vCPU(s)", "quoted cpus must warn the same way")
}

func caseHypervisor(t *testing.T) {
	// Divergence: the implemented default is vz when unset (base.yml sets no
	// x-hypervisor), while the docs claim the image annotation.
	def := runConfig(t, fixturePath(t, "base.yml"))
	requireExit0(t, def)
	mustContain(t, def.stdout, "x-hypervisor: vz", "unset x-hypervisor must default to vz")
	// An unknown backend is rejected at parse time.
	bad := runConfig(t, fixturePath(t, "hypervisor_unknown.yml"))
	requireExitNonZero(t, bad)
	mustContain(t, bad.stderr, "unsupported x-hypervisor", "an unknown backend must be rejected")
	// Documented aliases normalize to the canonical backend name.
	alias := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    x-hypervisor: apple\n"))
	requireExit0(t, alias)
	mustContain(t, alias.stdout, "x-hypervisor: vz\n", "the apple alias must normalize to vz")
}

func caseRestart(t *testing.T) {
	// Every accepted form parses and renders; the divergences are what the
	// manifest notes describe (ADR-0007 section 3). What the supervisor then
	// DOES with a policy — bring a killed service back, refuse to undo an
	// explicit stop — is not visible without a booted project and is proven
	// in TestConformanceRuntime/cli.up.
	res := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: unless-stopped\n"))
	requireExit0(t, res)
	mustContain(t, res.stdout, "restart: unless-stopped\n", "a declared restart policy must render")
	// unless-stopped is accepted as written and warns about nothing: it is
	// indistinguishable from always here, because the StoppedByUser marker
	// suppresses a restart under every policy (ADR-0007 section 2).
	requireNoWarnings(t, res)

	always := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: always\n"))
	requireExit0(t, always)
	mustContain(t, always.stdout, "restart: always\n", "always must render as declared")
	requireNoWarnings(t, always)

	// Divergence: on-failure cannot be distinguished from a clean exit for a
	// plain service, so it degrades to always and says so. The :N cap is kept.
	capped := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: on-failure:3\n"))
	requireExit0(t, capped)
	mustContain(t, capped.stdout, "restart: on-failure:3\n", "the on-failure attempt cap must render")
	mustContain(t, capped.stderr, `degrades to "always"`,
		"on-failure must warn that no exit code is observable for a plain service")
	mustContain(t, capped.stderr, "3-attempt cap is still honored",
		"the degrade warning must say the ':N' cap survives it")
	// The rendering is what the file declared, not the mode the supervisor
	// degrades it to: the degradation is a property of the guest image and
	// Phase B removes it without the file changing.
	mustNotContain(t, capped.stdout, "restart: always",
		"config must render the declared policy, not the degraded one")

	// Bare on-failure warns too, and renders without a cap.
	bare := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: on-failure\n"))
	requireExit0(t, bare)
	mustContain(t, bare.stdout, "restart: on-failure\n", "bare on-failure must render uncapped")
	mustContain(t, bare.stderr, `degrades to "always"`, "bare on-failure must warn as well")

	// An explicit 'restart: no' is a declaration and renders (quoted, since
	// YAML would otherwise read the bare word as a boolean); an absent key
	// declares nothing and renders nothing.
	explicit := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: \"no\"\n"))
	requireExit0(t, explicit)
	mustContain(t, explicit.stdout, "restart: \"no\"\n", "an explicit 'restart: no' must render as declared")
	absent := runConfig(t, fixturePath(t, "base.yml"))
	requireExit0(t, absent)
	mustNotContain(t, absent.stdout, "restart:", "an absent restart key must not render a policy")

	// An unknown value is a load error naming the accepted set, never a
	// silent "no restarts".
	bad := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: sometimes\n"))
	requireExitNonZero(t, bad)
	mustContain(t, bad.stdout+bad.stderr, "invalid restart policy",
		"an unknown restart policy must fail at load")
	mustContain(t, bad.stdout+bad.stderr, "no, always, on-failure, on-failure:N, unless-stopped",
		"the error must name the accepted set")

	// A malformed attempt cap is rejected the same way: 'on-failure:abc' read
	// as bare on-failure would silently grant unlimited restarts.
	badCap := runConfig(t, writeFixture(t,
		"services:\n  web:\n    image: alpine:3.19\n    restart: on-failure:abc\n"))
	requireExitNonZero(t, badCap)
	mustContain(t, badCap.stdout+badCap.stderr, `"abc" is not a retry count`,
		"a non-numeric attempt cap must fail at load")
}

// -- ext.x-oneshot (partial) -------------------------------------------------

// caseOneShot pins the static half of the job contract (ADR-0007 section 1):
// the marker renders whether it was declared or implied, a job with nothing to
// run fails at load from either direction, and a restart policy on a job warns
// that it is ignored. What a job DOES with its exit status needs a booted
// agent-bearing guest and is proven in
// TestConformanceRuntime/service.depends_on.
func caseOneShot(t *testing.T) {
	// Declared: the marker survives normalization, and marking a job is not
	// itself a divergence to warn about.
	declared := runConfig(t, writeFixture(t,
		"services:\n  migrate:\n    image: alpine:3.19\n    x-oneshot: true\n    command: [\"/bin/true\"]\n"))
	requireExit0(t, declared)
	mustContain(t, declared.stdout, "x-oneshot: true", "a declared x-oneshot must render")
	requireNoWarnings(t, declared)

	// Implied: a service targeted by service_completed_successfully is a job
	// even though it says nothing. Counting the occurrences is what makes the
	// negative half real — asserting only "the marker is present" would also
	// pass on an implementation that marked every service, dependent included.
	implied := runConfig(t, writeFixture(t,
		"services:\n  migrate:\n    image: alpine:3.19\n    command: [\"/bin/true\"]\n"+
			"  app:\n    image: alpine:3.19\n    depends_on:\n      migrate:\n"+
			"        condition: service_completed_successfully\n"))
	requireExit0(t, implied)
	if n := strings.Count(implied.stdout, "x-oneshot: true"); n != 1 {
		t.Errorf("exactly the completion-gated service must be marked a job, got %d markers in:\n%s",
			n, implied.stdout)
	}
	mIdx := strings.Index(implied.stdout, "  migrate:")
	oIdx := strings.Index(implied.stdout, "x-oneshot: true")
	if mIdx < 0 || oIdx < 0 || oIdx < mIdx {
		t.Errorf("the marker must land on the gated dependency (migrate), not the dependent; stdout:\n%s",
			implied.stdout)
	}

	// A job needs something to run, and the requirement is enforced from both
	// directions: an explicit marker without a command, and a completion-gated
	// dependency without one. Either would otherwise boot a VM that could
	// report no status at all.
	noCmd := runConfig(t, writeFixture(t,
		"services:\n  migrate:\n    image: alpine:3.19\n    x-oneshot: true\n"))
	requireExitNonZero(t, noCmd)
	mustContain(t, noCmd.stdout+noCmd.stderr, "x-oneshot needs a 'command' to run",
		"x-oneshot without a command must fail at load")

	gatedNoCmd := runConfig(t, writeFixture(t,
		"services:\n  migrate:\n    image: alpine:3.19\n"+
			"  app:\n    image: alpine:3.19\n    depends_on:\n      migrate:\n"+
			"        condition: service_completed_successfully\n"))
	requireExitNonZero(t, gatedNoCmd)
	mustContain(t, gatedNoCmd.stdout+gatedNoCmd.stderr,
		`service "app" waits for "migrate" to complete, but "migrate" declares no 'command' to run`,
		"a completion-gated dependency with no command must fail at load")

	// A restart policy on a job is ignored, and saying so at load is the
	// whole point: a completed job stays completed, so a policy that looks
	// honored in the file must not look honored in practice.
	withRestart := runConfig(t, writeFixture(t,
		"services:\n  migrate:\n    image: alpine:3.19\n    x-oneshot: true\n"+
			"    command: [\"/bin/true\"]\n    restart: always\n"))
	requireExit0(t, withRestart)
	mustContain(t, withRestart.stderr, `restart "always" is ignored: the service is a job`,
		"a restart policy on a job must warn that it is ignored")
	mustContain(t, withRestart.stdout, "restart: always\n",
		"the ignored policy still renders as declared")
}

// -- cli.config (partial) ----------------------------------------------------

func caseCliConfig(t *testing.T) {
	help := runBinary(t, "compose", "--help")
	requireExit0(t, help)
	if !helpListsCommand(help.stdout+"\n"+help.stderr, "config") {
		t.Errorf("`compose --help` must list the config subcommand:\n%s%s", help.stdout, help.stderr)
	}
	// It validates and prints the effective config without booting anything.
	res := runConfig(t, fixturePath(t, "base.yml"))
	requireExit0(t, res)
	// Prefix-anchored: a bare contains of "name: conf" would also match
	// "container_name: conf-web" and could never fail.
	if !strings.HasPrefix(res.stdout, "name: "+projectName+"\n") {
		t.Errorf("config must start with the top-level project name, got:\n%s", res.stdout)
	}
	mustContain(t, res.stdout, "services:", "config must print the normalized services")
	// Divergence: none of docker's config flags are accepted — each must be
	// rejected as an unknown flag, not crash or be silently ignored.
	for _, flag := range []string{
		"--services", "--volumes", "--hash", "--format",
		"--quiet", "--no-interpolate", "--resolve-image-digests",
	} {
		flagged := runBinary(t, "compose", "--file", fixturePath(t, "base.yml"),
			"--project-name", projectName, "config", flag)
		requireExitNonZero(t, flagged)
		mustContain(t, flagged.stdout+flagged.stderr, "flag provided but not defined",
			flag+" must be rejected as an unknown flag")
	}
}

// -- spec.interpolation and service.env_file (supported) ---------------------

func caseInterpolation(t *testing.T) {
	// ${VAR:-default} resolves; a sibling .env feeds interpolation; process
	// env wins; $$ stays a literal dollar; :? fails loudly.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("CONF_ENV_PROBE=fromdotenv\nHELLO=fromdotenv-hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "compose.yaml")
	content := "services:\n  web:\n    image: alpine:${TAG_UNSET_42:-3.19}\n    environment:\n      PROBE: ${CONF_ENV_PROBE}\n      HELLO: ${HELLO}\n      LIT: $$notavar\n"
	if err := os.WriteFile(fixture, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runConfig(t, fixture)
	requireExit0(t, res)
	mustContain(t, res.stdout, "image: alpine:3.19\n", "${VAR:-default} must resolve to the default")
	mustContain(t, res.stdout, "PROBE: fromdotenv", "the sibling .env must feed interpolation")
	mustContain(t, res.stdout, "LIT: $notavar", "$$ must escape to a literal dollar")
	mustNotContain(t, res.stdout, "${", "no unresolved references may remain")

	// :? must fail loudly when unset.
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("services:\n  web:\n    image: img:${REQ_UNSET_42:?set it}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := runConfig(t, bad)
	requireExitNonZero(t, fail)
	mustContain(t, fail.stdout+fail.stderr, "required variable REQ_UNSET_42 is missing a value: set it",
		":? must fail loudly with its message (compose-go's own wording)")

	// Comments are inert (the node-tree model): a :? inside a comment must
	// neither fail nor warn.
	commented := filepath.Join(dir, "commented.yaml")
	if err := os.WriteFile(commented, []byte("services:\n  web:\n    image: alpine:3.19  # was ${OLD_TAG:?pinned}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	quiet := runConfig(t, commented)
	requireExit0(t, quiet)
	mustNotContain(t, quiet.stderr, "OLD_TAG", "comments must not be interpolated")

	// Documented divergence: $ before a non-variable character stays
	// literal (docker errors). This is the one divergence that survives —
	// self-expansion and nested defaults, previously documented as
	// divergences too, do not (see below).
	lenient := filepath.Join(dir, "lenient.yaml")
	if err := os.WriteFile(lenient, []byte("services:\n  web:\n    image: alpine:3.19\n    environment:\n      COST: $1.50\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := runConfig(t, lenient)
	requireExit0(t, l)
	mustContain(t, l.stdout, "COST: $1.50", "a non-variable $ must pass through literally (documented divergence)")

	// A default nested inside another default resolves fully, docker's
	// behavior; the bespoke loader capped this at one level.
	nested := filepath.Join(dir, "nested.yaml")
	if err := os.WriteFile(nested, []byte("services:\n  web:\n    image: alpine:3.19\n    environment:\n      A: ${L1_UNSET_42:-${L2_UNSET_42:-${L3_UNSET_42:-bottom}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := runConfig(t, nested)
	requireExit0(t, n)
	mustContain(t, n.stdout, "A: bottom", "a default nested several levels deep must resolve fully")

	// .env values ARE expanded against earlier entries in the same file,
	// docker's behavior; the bespoke loader left them literal.
	selfDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(selfDir, ".env"), []byte("BASE=1.2.3.4\nADDR=${BASE}:5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selfFixture := filepath.Join(selfDir, "compose.yaml")
	if err := os.WriteFile(selfFixture, []byte("services:\n  web:\n    image: alpine:3.19\n    environment:\n      ADDR: ${ADDR}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf := runConfig(t, selfFixture)
	requireExit0(t, sf)
	mustContain(t, sf.stdout, "ADDR: 1.2.3.4:5432", ".env values must expand against earlier entries in the same file")
}

func caseEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svc.env"), []byte("FROM_FILE=1\nSHARED=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "compose.yaml")
	content := "services:\n  web:\n    image: alpine:3.19\n    env_file: svc.env\n    environment:\n      SHARED: envkey\n"
	if err := os.WriteFile(fixture, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runConfig(t, fixture)
	requireExit0(t, res)
	mustContain(t, res.stdout, "FROM_FILE: \"1\"", "env_file values must reach the environment")
	mustContain(t, res.stdout, "SHARED: envkey", "'environment' must win over env_file")

	// A missing required file fails at load.
	missing := filepath.Join(dir, "missing.yaml")
	if err := os.WriteFile(missing, []byte("services:\n  web:\n    image: alpine:3.19\n    env_file: nope.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := runConfig(t, missing)
	requireExitNonZero(t, fail)
	mustContain(t, fail.stdout+fail.stderr, "env file "+filepath.Join(dir, "nope.env")+" not found",
		"a missing required env_file must fail loudly")
}

// -- service.extends (supported) ---------------------------------------------

// caseExtends pins service extension through the black-box binary:
// compose-go resolves 'extends' during load (same-file and cross-file), so
// the warn-walk must not flag the key as ignored, scalar/list fields from
// the extending service win over the base's, map fields (environment) merge
// key by key, and a target that does not exist fails loudly at load.
func caseExtends(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Same-file: the extending service inherits everything it does not
	// declare itself, overrides what it does, and merges maps key by key.
	sameFile := write("same.yaml", "services:\n"+
		"  base:\n    image: alpine:3.19\n    command: [\"base-cmd\"]\n"+
		"    environment:\n      SHARED: from-base\n"+
		"  web:\n    extends:\n      service: base\n"+
		"    command: [\"web-cmd\"]\n    environment:\n      EXTRA: from-web\n")
	same := runConfig(t, sameFile)
	requireExit0(t, same)
	requireNoWarnings(t, same)
	// 'base' renders too (it is its own service, unaffected by anything
	// extending it), so the extending service's fields are asserted as one
	// block: a bare "web-cmd" would also match if base's own "base-cmd" were
	// mistakenly used, but the two commands must not be interchangeable.
	mustContain(t, same.stdout, "  web:\n    command:\n      - web-cmd\n"+
		"    environment:\n      EXTRA: from-web\n      SHARED: from-base\n"+
		"    image: alpine:3.19\n",
		"the extending service must have its own command, its own and the base's environment entries, and the base's image")

	// Cross-file: 'file' names the compose file the base service lives in,
	// resolved relative to the extending file's own directory.
	write("base.yaml", "services:\n  base:\n    image: alpine:3.19\n"+
		"    command: [\"true\"]\n    environment:\n      SHARED: from-base\n")
	xfile := write("main.yaml", "services:\n  web:\n    extends:\n"+
		"      file: base.yaml\n      service: base\n    environment:\n      EXTRA: from-web\n")
	x := runConfig(t, xfile)
	requireExit0(t, x)
	requireNoWarnings(t, x)
	mustContain(t, x.stdout, "image: alpine:3.19", "a cross-file base's image must be inherited")
	mustContain(t, x.stdout, "EXTRA: from-web", "the extending file's own environment entry must be present")
	mustContain(t, x.stdout, "SHARED: from-base", "the cross-file base's environment entry must be inherited")
	mustNotContain(t, x.stdout, "\n  base:\n", "only the extending service, not the base file's own service name, joins the project")

	// A target that does not exist is a load error naming the service.
	bad := write("bad.yaml", "services:\n  web:\n    extends:\n      service: nosuch\n")
	b := runConfig(t, bad)
	requireExitNonZero(t, b)
	mustContain(t, b.stdout+b.stderr, `"nosuch" not found`,
		"extending an undeclared service must fail loudly, naming it")

	// An unsupported key declared on the extends TARGET (not the extending
	// service) must still warn: compose-go fires a distinct "extends" event
	// for a cross-file 'extends: {file: ...}', separate from the "include"
	// event the warn-walk was already wired to, and it is easy to plumb one
	// but not the other. A silent drop here is exactly the "worst possible
	// UX" internal/compose/warn.go's own doc comment says the walk exists
	// to prevent — an unsupported key would vanish with no trace merely
	// because it arrived via 'extends' instead of being declared directly
	// or via 'include'.
	write("unsupported_base.yaml", "services:\n  base:\n    image: alpine:3.19\n    privileged: true\n")
	unsupported := write("unsupported_main.yaml", "services:\n  web:\n    extends:\n"+
		"      file: unsupported_base.yaml\n      service: base\n")
	u := runConfig(t, unsupported)
	requireExit0(t, u)
	baseFile := filepath.Join(dir, "unsupported_base.yaml")
	mustContain(t, u.stderr, baseFile+`: ignoring unsupported key "services.base.privileged"`,
		"an unsupported key declared on an extends target file must warn, naming that file and that file's own service")
}

// -- toplevel.volumes (partial) ----------------------------------------------

// caseNamedVolumeDeclarations pins the declaration contract statically: a
// declared volume resolves to its managed store directory, an undeclared
// one fails at load, a hostile name is rejected before it can be joined
// onto a path, and declaration options warn instead of being honored. The
// data lifecycle (persist across down/up, removed by down --volumes) is
// proven by the runtime tier.
func caseNamedVolumeDeclarations(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	ok := write("ok.yaml", "volumes:\n  vdata: {}\nservices:\n  web:\n    image: alpine:3.19\n    volumes:\n      - vdata:/data\n")
	// An explicit store dir pins volumesRoot's derivation from the flag,
	// not just the path's tail: a root outside the store would fail here.
	store := t.TempDir()
	res := runBinary(t, "--store-dir", store, "compose", "--file", ok,
		"--project-name", projectName, "config")
	requireExit0(t, res)
	mustContain(t, res.stdout, filepath.Join(store, "volumes", projectName+"_vdata")+":/data",
		"a declared volume must resolve to <store>/volumes/<project>_<name>")
	requireNoWarnings(t, res)

	// compose-go's own consistency check catches an undeclared named volume
	// before hull's code ever sees it, in its own wording.
	undeclared := write("undeclared.yaml", "services:\n  web:\n    image: alpine:3.19\n    volumes:\n      - vdata:/data\n")
	u := runConfig(t, undeclared)
	requireExitNonZero(t, u)
	mustContain(t, u.stdout+u.stderr, `service "web" refers to undefined volume vdata`,
		"an undeclared named volume must fail at load (compose-go's own wording)")

	// A volume name that would escape the volumes root is rejected by
	// compose-go's own JSON-schema validation ([a-zA-Z0-9._-]+). There is no
	// separate urunc-side path-traversal check anymore: the schema pattern
	// already rules out '/' and '..', and namedVolumeDir's own
	// '<project>_<name>' prefix makes a schema-valid name unable to escape
	// regardless.
	hostile := write("hostile.yaml", "volumes:\n  '../../escape': {}\nservices:\n  web:\n    image: alpine:3.19\n")
	h := runConfig(t, hostile)
	requireExitNonZero(t, h)
	mustContain(t, h.stdout+h.stderr, `volumes additional properties '../../escape' not allowed`,
		"a volume name that would escape the volumes root must be rejected (compose-go's own schema wording)")

	opts := write("opts.yaml", "volumes:\n  vdata:\n    driver: local\nservices:\n  web:\n    image: alpine:3.19\n    volumes:\n      - vdata:/data\n")
	o := runConfig(t, opts)
	requireExit0(t, o)
	mustContain(t, o.stderr, `ignoring unsupported key "volumes.vdata.driver"`,
		"declaration options must warn instead of being honored")
}

// -- toplevel.include (supported) ---------------------------------------------

// caseInclude pins the include contract through the black-box binary: an
// included file's services join the project, its relative paths resolve
// against itself, a single include entry naming several paths merges them
// (later overriding earlier, docker's own multi-file semantics), and a
// service declared in two files field-merges rather than erroring — the
// bespoke loader's own stricter "identical or refuse" rule for the latter
// two is gone under compose-go, matching how docker itself treats multiple
// files defining the same service (an ordinary multi-file '-f' merge).
func caseInclude(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	write("sub/api.env", "TOKEN=abc\n")
	write("sub/compose.yaml", "services:\n  api:\n    image: alpine:3.19\n"+
		"    env_file: api.env\n    volumes:\n      - ./data:/data\n")
	main := write("compose.yaml", "include:\n  - sub/compose.yaml\nservices:\n  web:\n    image: alpine:3.19\n")

	res := runConfig(t, main)
	requireExit0(t, res)
	mustContain(t, res.stdout, "api:", "an included service must join the project")
	mustContain(t, res.stdout, "web:", "the including file keeps its own services")
	mustContain(t, res.stdout, "TOKEN: abc", "env_file must be read from the included file's directory")
	mustContain(t, res.stdout, "source: "+filepath.Join(dir, "sub", "data"),
		"a bind mount in an included file must resolve against that file")
	requireNoWarnings(t, res)

	// A single include entry naming several paths merges them: the first is
	// the base, later ones override, field by field — not refused the way
	// the bespoke loader refused it.
	write("sub/override.yaml", "services:\n  api:\n    image: alpine:3.20\n")
	list := write("list.yaml", "include:\n  - path:\n      - sub/compose.yaml\n      - sub/override.yaml\n")
	l := runConfig(t, list)
	requireExit0(t, l)
	mustContain(t, l.stdout, "image: alpine:3.20", "a later path in an include's path list must override an earlier one")
	mustContain(t, l.stdout, "TOKEN: abc", "a field only the earlier path sets must still be inherited")

	// A service declared in the including file AND an included file merges
	// field by field, the including file's fields winning ties — the same
	// merge a multi-file '-f' would do, not an error.
	dup := write("dup.yaml", "include:\n  - sub/compose.yaml\nservices:\n  api:\n    image: alpine:3.21\n")
	d := runConfig(t, dup)
	requireExit0(t, d)
	mustContain(t, d.stdout, "image: alpine:3.21", "the including file's own field must win over the included file's")
	mustContain(t, d.stdout, "TOKEN: abc", "a field only the included file sets must still be inherited")

	// A cycle is reported as a cycle rather than recursing forever; this is
	// the plain 'config'/'up' load path, not the SkipUnreadableIncludes
	// reload-tolerance path internal/compose/load_test.go's own cycle test
	// covers. There is no depth cap otherwise: compose-go imposes none, so
	// only a genuine cycle stops an include chain.
	write("cyclea.yaml", "include:\n  - cycleb.yaml\nservices:\n  a:\n    image: alpine:3.19\n")
	write("cycleb.yaml", "include:\n  - cyclea.yaml\nservices:\n  b:\n    image: alpine:3.19\n")
	c := runConfig(t, filepath.Join(dir, "cyclea.yaml"))
	requireExitNonZero(t, c)
	mustContain(t, c.stdout+c.stderr, "include cycle detected", "an include cycle must fail loudly, not recurse forever")
}

// -- service.label_file (unsupported, but no longer a no-op probe) ----------

// caseLabelFile pins a behavior change the generic unsupportedServiceCase
// probe cannot: compose-go reads and parses a declared label_file during
// load (to populate the service's resolved Labels), even though nothing in
// hull's runtime path ever reads svc.Labels — the key still warns as
// unsupported, but a label file that does not exist now fails the load
// instead of being silently unreachable decoration.
func caseLabelFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	write("labels.txt", "com.example.foo=bar\n")
	fixture := write("compose.yaml", "services:\n  web:\n    image: alpine:3.19\n    label_file: labels.txt\n")
	res := runConfig(t, fixture)
	requireExit0(t, res)
	mustContain(t, res.stderr, `ignoring unsupported key "services.web.label_file"`,
		"label_file must still warn as unsupported: its content is parsed at load but never applied to anything")

	missing := write("missing.yaml", "services:\n  web:\n    image: alpine:3.19\n    label_file: nope.txt\n")
	fail := runConfig(t, missing)
	requireExitNonZero(t, fail)
	mustContain(t, fail.stdout+fail.stderr, "label file "+filepath.Join(dir, "nope.txt")+" not found",
		"a missing label_file must fail the load, even though its content is never used")
}

// -- data-driven unsupported keys --------------------------------------------

// unsupportedServiceCase generates a minimal valid service carrying key and
// asserts the loud "ignoring unsupported key" warning for services.web.<key>.
func unsupportedServiceCase(key string) staticCase {
	return func(t *testing.T) {
		content := "services:\n  web:\n    image: alpine:3.19\n    " + key + ": " + serviceKeyValue(key) + "\n"
		res := runConfig(t, writeFixture(t, content))
		requireExit0(t, res)
		mustContain(t, res.stderr, `ignoring unsupported key "services.web.`+key+`"`,
			"an unsupported service key must warn loudly (a silent drop is a conformance failure)")
	}
}

// unsupportedTopLevelCase generates a valid base project plus an unsupported
// top-level key and asserts the loud warning for that dotted path.
func unsupportedTopLevelCase(key string) staticCase {
	return func(t *testing.T) {
		content := "services:\n  web:\n    image: alpine:3.19\n" + key + ": " + topLevelKeyValue(key) + "\n"
		res := runConfig(t, writeFixture(t, content))
		requireExit0(t, res)
		mustContain(t, res.stderr, `ignoring unsupported key "`+key+`"`,
			"an unsupported top-level key must warn loudly (a silent drop is a conformance failure)")
	}
}

// cliAbsentCase asserts an unsupported compose verb is absent from the help and
// exits non-zero with an unknown-command error — specifically the CLI's
// unknown-topic message, so a crash on the verb cannot pass as conformance.
func cliAbsentCase(verb string) staticCase {
	return func(t *testing.T) {
		res := runBinary(t, "compose", verb)
		requireExitNonZero(t, res)
		mustContain(t, res.stdout+res.stderr, "No help topic for",
			verb+" must fail as an unknown command, not crash")
		help := runBinary(t, "compose", "--help")
		if helpListsCommand(help.stdout+"\n"+help.stderr, verb) {
			t.Errorf("`compose --help` lists %q, but cli.%s is marked unsupported", verb, verb)
		}
	}
}

// serviceKeyValue returns a type-appropriate YAML value for a probed service
// key. The warning walker never recurses into an unsupported service key, so
// hull itself never reads the value — but compose-go validates every
// recognized key's SHAPE against the compose-spec JSON schema before urunc
// ever sees it, even keys hull ignores. A shape mismatch (e.g. a bare
// string for a key the schema models as an object) makes compose-go fail the
// load with a schema error, which would mask the "ignoring unsupported key"
// warning this test exists to prove. The value therefore must be schema-valid
// for its key so the ONLY thing that fires is that warning, not a load error.
func serviceKeyValue(key string) string {
	switch key {
	// booleans (schema: boolean|string)
	case "attach", "init", "oom_kill_disable", "privileged", "read_only",
		"stdin_open", "tty", "use_api_socket":
		return "true"
	// numbers (schema: number/integer|string)
	case "cpu_count", "cpu_percent", "cpu_period", "cpu_quota", "cpu_rt_period",
		"cpu_rt_runtime", "cpu_shares", "mem_reservation", "mem_swappiness",
		"memswap_limit", "oom_score_adj", "pids_limit", "scale", "shm_size":
		return "1"
	// constrained strings (schema: enum or pattern)
	case "cgroup":
		return `"host"`
	case "pull_policy":
		return `"always"`
	case "stop_grace_period":
		return `"1s"` // schema type is string, but the field decodes as a Go Duration
	case "gpus":
		return `"all"`
	// empty lists (schema: array, no minItems)
	case "cap_add", "cap_drop", "configs", "device_cgroup_rules", "devices",
		"dns_opt", "expose", "external_links", "extra_hosts", "group_add",
		"links", "models", "networks", "pre_start", "secrets", "security_opt",
		"volumes_from":
		return "[]"
	// empty mappings (schema: object, no required properties)
	case "annotations", "blkio_config", "credential_spec", "deploy", "develop",
		"healthcheck", "labels", "logging", "storage_opt", "sysctls", "ulimits":
		return "{}"
	// objects with a required sub-property
	case "provider":
		return "{type: conformance}"
	default:
		return `"conformance"`
	}
}

// topLevelKeyValue returns a type-appropriate YAML value for a probed
// top-level key (a mapping for the collection elements, a scalar otherwise).
// As with serviceKeyValue, compose-go schema-validates every recognized
// top-level key regardless of whether hull reads it, so the probe
// value must satisfy that key's shape. secrets/configs/models go further:
// compose-go additionally requires a resource-declaration block to name a
// source (file|environment[|content], or the model name itself), so a bare
// empty mapping fails validation even though the outer shape is a mapping.
func topLevelKeyValue(key string) string {
	switch key {
	case "secrets", "configs":
		return "{probe: {file: /dev/null}}"
	case "models":
		return "{probe: {model: conformance}}"
	case "networks", "volumes":
		return "{probe: {}}"
	default:
		return `"conformance"`
	}
}
