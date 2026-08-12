package compose

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// wantWarning asserts one exact warning line is present. Warnings are
// asserted line by line, never as a blob, so each capability's warning is
// independently checkable (the conformance harness greps them the same way).
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

func TestWarnUnsupportedKeys_TopLevel(t *testing.T) {
	src := `
version: "3.9"
name: other
networks:
  front: {}
volumes:
  data:
    driver: local
secrets:
  pw:
    external: true
configs:
  cfg:
    file: ./c
models:
  llm: {}
bogus: 1
x-anchors:
  base: &base
    image: img:1
services:
  web:
    <<: *base
`
	cases := map[string]string{
		"version":        `warning: compose: ignoring unsupported key "version": the top-level 'version' field is obsolete in the compose spec and has no effect`,
		"name":           `warning: compose: ignoring unsupported key "name": the project name comes from -p/--project-name, COMPOSE_PROJECT_NAME, or the working directory name`,
		"networks":       `warning: compose: ignoring unsupported key "networks": every service joins one flat project network; set its range with 'compose up --subnet CIDR'`,
		"volumes-option": `warning: compose: ignoring unsupported key "volumes.data.driver": named-volume declaration options are not supported; only the bare declaration is honored`,
		"secrets":        `warning: compose: ignoring unsupported key "secrets": secrets are not implemented; pass the value through the service's 'environment' instead`,
		"configs":        `warning: compose: ignoring unsupported key "configs": configs are not implemented; bind-mount the file through the service's 'volumes' list instead`,
		"models":         `warning: compose: ignoring unsupported key "models": model definitions are not implemented`,
		"bogus":          `warning: compose: ignoring unsupported key "bogus": hull compose reads only the top-level 'services' key`,
		"x-anchors":      `warning: compose: ignoring unsupported key "x-anchors": top-level extension keys are not interpreted; a YAML anchor defined here still merges where it is referenced`,
	}

	var warn bytes.Buffer
	WarnUnsupportedKeys(&warn, []byte(src), "")
	warnings := warn.String()

	for key, want := range cases {
		t.Run(key, func(t *testing.T) { wantWarning(t, warnings, want) })
	}
	// One line per key, nothing else: no aggregate line, no repeats.
	if got := warningCount(warnings); got != len(cases) {
		t.Errorf("got %d warning lines, want %d:\n%s", got, len(cases), warnings)
	}
	// The merge key itself is not a compose key and must not be warned about.
	if strings.Contains(warnings, `"<<"`) || strings.Contains(warnings, "services.web.<<") {
		t.Errorf("merge key warned about:\n%s", warnings)
	}
}

func TestWarnUnsupportedKeys_ServiceLevel(t *testing.T) {
	src := `
services:
  web:
    image: img:1
    build: .
    restart: always
    deploy:
      replicas: 3
    entrypoint: /bin/sh
    networks:
      - front
    labels:
      a: b
    healthcheck:
      test: ["CMD", "true"]
      bogus: 1
    x-unknown: 1
    depends_on:
      db:
        condition: service_started
        restart: true
    x-healthcheck-tcp:
      port: 80
      timeout: 30s
      bogus: 1
  db:
    image: img:2
    restart: always
    build: .
`
	cases := map[string]string{
		"build":             `warning: compose: ignoring unsupported key "services.web.build": images are never built; push the image and point 'image' at it`,
		"deploy":            `warning: compose: ignoring unsupported key "services.web.deploy": deploy (replicas, resource reservations, placement) is not implemented; one service is exactly one VM`,
		"entrypoint":        `warning: compose: ignoring unsupported key "services.web.entrypoint": the image entrypoint cannot be overridden; put the whole command line in 'command'`,
		"networks":          `warning: compose: ignoring unsupported key "services.web.networks": every service joins the single flat project network; per-service network selection is ignored`,
		"healthcheck.bogus": `warning: compose: ignoring unsupported key "services.web.healthcheck.bogus": not supported by hull compose; the exec probe reads test, interval, timeout, retries, start_period and disable`,
		"labels":            `warning: compose: ignoring unsupported key "services.web.labels": not supported by hull compose (see docs/compose.md for the supported service keys)`,
		"x-unknown":         `warning: compose: ignoring unsupported key "services.web.x-unknown": unknown extension key; hull compose defines only x-hypervisor and x-healthcheck-tcp`,
		// The same unsupported key on a second service must warn again: a
		// regression that deduped by bare key name across services would
		// otherwise pass.
		"db.build": `warning: compose: ignoring unsupported key "services.db.build": images are never built; push the image and point 'image' at it`,
		// Nested mappings the parser looks inside are checked too.
		"depends_on.restart":        `warning: compose: ignoring unsupported key "services.web.depends_on.db.restart": restarting a dependent when its dependency restarts is not implemented`,
		"x-healthcheck-tcp.timeout": `warning: compose: ignoring unsupported key "services.web.x-healthcheck-tcp.timeout": there is no per-probe timeout; size the wait with 'retries' and 'start_period'`,
		"x-healthcheck-tcp.bogus":   `warning: compose: ignoring unsupported key "services.web.x-healthcheck-tcp.bogus": not supported by hull compose; the TCP probe reads port, interval, retries and start_period`,
	}
	// Note: the original bespoke fixture also asserts a warning for
	// declaring both healthcheck forms ("both healthcheck and
	// x-healthcheck-tcp declared"). That warning comes from
	// validateComposeFile, not warnUnsupportedKeys, so it is out of scope
	// here.

	var warn bytes.Buffer
	WarnUnsupportedKeys(&warn, []byte(src), "")
	warnings := warn.String()

	for name, want := range cases {
		t.Run(name, func(t *testing.T) { wantWarning(t, warnings, want) })
	}
	if got := warningCount(warnings); got != len(cases) {
		t.Errorf("got %d warning lines, want %d:\n%s", got, len(cases), warnings)
	}
}

// TestLoad_WarnsUnsupportedKey exercises the wiring through Load itself: a
// compose file with one unsupported top-level key, loaded via Load, must
// have its warning reach the writer Load was given.
func TestLoad_WarnsUnsupportedKey(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
networks:
  front: {}
services:
  web:
    image: img:1
`)

	var warn bytes.Buffer
	_, err := Load(context.Background(), Options{
		Files:      []string{f},
		WorkingDir: dir,
		Warn:       &warn,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := `warning: compose: ignoring unsupported key "networks": every service joins one flat project network; set its range with 'compose up --subnet CIDR'`
	wantWarning(t, warn.String(), want)
}

// TestLoad_WarnsBeforeLoadProjectFails pins a deliberate deviation from the
// brief's literal "after a successful LoadProject" wording: the warn walk
// runs on po.ConfigPaths before po.LoadProject is called, same order the
// bespoke loader used ("Warnings come first so a file that also fails
// validation still reports every key it would lose" — see the moved comment
// on WarnUnsupportedKeys' call site). Under compose-go's strict schema, a
// typo key hard-fails the load; a file with both a typo (services.web.imagee)
// and an unsupported key (top-level networks) must still surface the
// networks warning even though Load ultimately returns an error, or the
// warning is silently lost on exactly the files that need it most.
func TestLoad_WarnsBeforeLoadProjectFails(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
networks:
  front: {}
services:
  web:
    image: nginx
    imagee: typo
`)

	var warn bytes.Buffer
	_, err := Load(context.Background(), Options{
		Files:      []string{f},
		WorkingDir: dir,
		Warn:       &warn,
	})
	if err == nil {
		t.Fatal("want strict-validation error for unknown key 'imagee'")
	}
	want := `warning: compose: ignoring unsupported key "networks": every service joins one flat project network; set its range with 'compose up --subnet CIDR'`
	wantWarning(t, warn.String(), want)
}

// TestLoad_WarnsUnsupportedKeyInIncludedFile proves the warn walk reaches
// files pulled in via `include:`, not just the file(s) Load was called
// with directly — the same include-visibility gap Task 4 solved for the
// file-size cap, via the same cli.ProjectOptions.WithListeners mechanism.
func TestLoad_WarnsUnsupportedKeyInIncludedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "included.yaml", `
networks:
  front: {}
services:
  db:
    image: img:2
`)
	f := writeFile(t, dir, "compose.yaml", `
include:
  - included.yaml
services:
  web:
    image: img:1
`)

	var warn bytes.Buffer
	_, err := Load(context.Background(), Options{
		Files:      []string{f},
		WorkingDir: dir,
		Warn:       &warn,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	included := filepath.Join(dir, "included.yaml")
	want := `warning: compose: ` + included + `: ignoring unsupported key "networks": every service joins one flat project network; set its range with 'compose up --subnet CIDR'`
	wantWarning(t, warn.String(), want)
}
