package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// wantSizeCapErr fails t unless err is the size-cap error checkFileSize
// produces, so a test proves the cap fired rather than some unrelated
// failure (e.g. the oversized file's padding tripping compose-go's YAML
// parser) that would also leave err non-nil.
func wantSizeCapErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want size-cap error, got nil")
	}
	want := fmt.Sprintf("is larger than the %d MiB limit for a text file", MaxUserFileBytes>>20)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("want size-cap error containing %q, got %v", want, err)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadBasicProject(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx:latest
    command: ["nginx", "-g", "daemon off;"]
    environment:
      - MODE=prod
`)
	p, err := Load(context.Background(), Options{
		Files: []string{f}, ProjectName: "conf", WorkingDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := p.GetService("web")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Image != "nginx:latest" {
		t.Fatalf("image = %q", svc.Image)
	}
	if got := svc.Environment["MODE"]; got == nil || *got != "prod" {
		t.Fatalf("environment MODE = %v", got)
	}
}

func TestLoadUnknownKeyFails(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx
    imagee: typo
`)
	_, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err == nil {
		t.Fatal("want strict-validation error for unknown key 'imagee'")
	}
}

func TestLoadRejectsOversizedComposeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxUserFileBytes+1)
	copy(big, []byte("services: {}\n#"))
	f := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(f, big, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	wantSizeCapErr(t, err)
}

// TestLoadRejectsOversizedDefaultDiscoveredComposeFile covers the compose
// file compose-go discovers on its own (opts.Files empty; Options documents
// "empty means default discovery in WorkingDir"). The up-front cap has to
// check the resolved po.ConfigPaths, not just opts.Files, or a discovered
// file bypasses the cap entirely.
func TestLoadRejectsOversizedDefaultDiscoveredComposeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxUserFileBytes+1)
	copy(big, []byte("services: {}\n#"))
	f := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(f, big, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), Options{ProjectName: "conf", WorkingDir: dir})
	wantSizeCapErr(t, err)
}

func TestLoadRejectsOversizedEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", "services:\n  a:\n    image: img\n")
	envPath := filepath.Join(dir, "big.env")
	if err := os.WriteFile(envPath, make([]byte, MaxUserFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), Options{
		Files: []string{f}, ProjectName: "conf", WorkingDir: dir, EnvFiles: []string{envPath},
	})
	wantSizeCapErr(t, err)
}

// TestLoadRejectsOversizedDefaultEnvFile covers the .env compose-go
// discovers on its own (opts.EnvFiles empty): the bespoke loader capped this
// path too, via the same readTextFile default-.env fallback, so Load
// replicates the same discovery rule (see defaultEnvFile) to keep it capped.
func TestLoadRejectsOversizedDefaultEnvFile(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", "services:\n  a:\n    image: img\n")
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, make([]byte, MaxUserFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	wantSizeCapErr(t, err)
}

// TestLoadRejectsOversizedIncludedFile covers the size cap on a top-level
// `include:` target with a fixture that is both over the cap and otherwise
// valid YAML (padded with spaces after a comment marker): valid content
// means this alone cannot tell the pre-read cap (capTopLevelIncludes, what
// actually catches it now) apart from the weaker after-the-fact backstop a
// prior version of this fix relied on — both orderings reject it.
// TestLoadRejectsOversizedIncludedFileBeforeParse below is the test that
// proves the ordering; this one stays for plain coverage of the top-level
// case, padded with spaces rather than zero bytes so the failure is
// unambiguously the size cap and not compose-go's YAML parser choking on
// control characters.
func TestLoadRejectsOversizedIncludedFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, MaxUserFileBytes+1)
	for i := range big {
		big[i] = ' '
	}
	copy(big, []byte("services:\n  inc:\n    image: busybox\n#"))
	incPath := filepath.Join(sub, "included.yaml")
	if err := os.WriteFile(incPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	main := writeFile(t, dir, "compose.yaml", "include:\n  - ./subdir/included.yaml\nservices:\n  a:\n    image: nginx\n")

	_, err := Load(context.Background(), Options{Files: []string{main}, ProjectName: "conf", WorkingDir: dir})
	wantSizeCapErr(t, err)
}

// TestLoadEnvFileMergesIntoEnvironment locks in that env_file: entries end up
// merged into svc.Environment (the field the runtime layer reads), not just
// parsed into svc.EnvFiles. A gap here was suspected during investigation of
// this task (the assumption being that compose-go's
// Project.WithServicesEnvironmentResolved needs an explicit post-Load call),
// but compose-go v2.14.0's loader.ModelToProject already calls it
// internally (see loader/loader.go:658-659) whenever SkipResolveEnvironment
// is false, which is the default and which Load never overrides. This test
// (and the one below) exist to prove that behavior against the real
// pipeline and to catch a future compose-go bump that changes the default.
func TestLoadEnvFileMergesIntoEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.env", "FOO=fromfile\n")
	f := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx:latest
    env_file:
      - vars.env
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := p.GetService("web")
	if err != nil {
		t.Fatal(err)
	}
	got := svc.Environment["FOO"]
	if got == nil || *got != "fromfile" {
		t.Fatalf("environment FOO = %v, want \"fromfile\"", got)
	}
}

// TestLoadEnvironmentOverridesEnvFile covers docker's precedence rule:
// environment: always wins over env_file: on key collision.
func TestLoadEnvironmentOverridesEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.env", "FOO=fromfile\n")
	f := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx:latest
    env_file:
      - vars.env
    environment:
      - FOO=fromenv
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := p.GetService("web")
	if err != nil {
		t.Fatal(err)
	}
	got := svc.Environment["FOO"]
	if got == nil || *got != "fromenv" {
		t.Fatalf("environment FOO = %v, want \"fromenv\"", got)
	}
}

// TestLoadBareEnvironmentKeyUnresolved covers a bare `environment: [KEY]`
// entry with nothing to resolve it: not in .env, not in the process environ
// passed via Options.Environ, and no env_file defines it either. The key is
// namespaced so it cannot collide with anything actually set in the shell
// running the test (which would otherwise leak in via Options.Environ, since
// cli.WithEnv(nil) does not implicitly fall back to os.Environ() here - Load
// always passes opts.Environ verbatim). This documents (rather than assumes)
// what compose-go's environment resolution leaves behind for a later task
// (runtime service->VM mapping) to handle. Observed to be true whether or
// not resolution is skipped (verified experimentally while investigating
// this task), which places the "present with nil value" behavior in the
// bare-dict-to-MappingWithEquals transform, not in
// WithServicesEnvironmentResolved - so it is stable regardless of how that
// call is (or isn't) invoked.
func TestLoadBareEnvironmentKeyUnresolved(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  web:
    image: nginx:latest
    environment:
      - HULL_TEST_UNRESOLVED_KEY
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := p.GetService("web")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := svc.Environment["HULL_TEST_UNRESOLVED_KEY"]
	if !ok {
		t.Fatalf("environment key HULL_TEST_UNRESOLVED_KEY absent from map, want present with nil value")
	}
	if got != nil {
		t.Fatalf("environment HULL_TEST_UNRESOLVED_KEY = %q, want nil (unresolved)", *got)
	}
}

func TestLoadInterpolation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "TAG=9.9\n")
	f := writeFile(t, dir, "compose.yaml", `
services:
  db:
    image: postgres:${TAG}
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := p.GetService("db")
	if svc.Image != "postgres:9.9" {
		t.Fatalf("image = %q, want postgres:9.9 (default .env discovery)", svc.Image)
	}
}

// TestLoadUnreadableIncludeFailsByDefault pins the strict path a
// user-facing command (config, up) relies on: a missing include: target is
// the whole point of the error, so SkipUnreadableIncludes unset (the zero
// value, false) must still fail the load.
func TestLoadUnreadableIncludeFailsByDefault(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, dir, "compose.yaml", "include:\n  - ./missing.yaml\nservices:\n  a:\n    image: nginx\n")
	_, err := Load(context.Background(), Options{Files: []string{main}, ProjectName: "conf", WorkingDir: dir})
	if err == nil {
		t.Fatal("a missing include must fail the load when SkipUnreadableIncludes is not set")
	}
}

// TestLoadSkipUnreadableIncludes covers the reload path's tolerance: a
// service the missing include would have contributed is simply absent from
// the result, everything else in the file still loads, and Load itself
// never errors over the drift.
func TestLoadSkipUnreadableIncludes(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - ./gone.yaml\nservices:\n  own:\n    image: nginx\n")
	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("an unreadable include must not fail the load with SkipUnreadableIncludes set: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("the service whose own file never moved must still load: %v", err)
	}
	if _, err := p.GetService("fromgone"); err == nil {
		t.Error("a service only the missing include would have declared must not appear")
	}
}

// TestLoadSkipUnreadableIncludesKeepsReadableOnes proves the tolerance is
// per-include, not "any trouble anywhere drops every include": a file next
// to one that has gone missing must still be pulled in normally, and the
// still-readable include's own unsupported-key warnings must still fire.
func TestLoadSkipUnreadableIncludesKeepsReadableOnes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "present.yaml", "services:\n  frompresent:\n    image: redis\n    privileged: true\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - ./gone.yaml\n  - ./present.yaml\nservices:\n  own:\n    image: nginx\n")
	var warn strings.Builder
	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true, Warn: &warn,
	})
	if err != nil {
		t.Fatalf("a mix of a missing and a present include must still load: %v", err)
	}
	for _, want := range []string{"own", "fromgone_should_not_exist_marker", "frompresent"} {
		_, err := p.GetService(want)
		switch want {
		case "fromgone_should_not_exist_marker":
			if err == nil {
				t.Error("placeholder name must not exist (sanity check on the test itself)")
			}
		default:
			if err != nil {
				t.Errorf("service %q must still load: %v", want, err)
			}
		}
	}
	if !strings.Contains(warn.String(), `ignoring unsupported key "services.frompresent.privileged"`) {
		t.Errorf("the still-readable include's own warnings must still fire, got:\n%s", warn.String())
	}
}

// TestLoadSkipUnreadableIncludesLongForm covers the {path: ..., ...} form,
// not just the short scalar form.
func TestLoadSkipUnreadableIncludesLongForm(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - path: ./gone.yaml\n    project_directory: .\nservices:\n  own:\n    image: nginx\n")
	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("the long include form must tolerate a missing path too: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("the service whose own file never moved must still load: %v", err)
	}
}

// TestLoadSkipUnreadableIncludesNested is Gap A from the second review
// round: compose.yaml includes stack/a.yaml (readable), which itself
// includes ../shared/gone.yaml (missing). The bespoke loader's
// skipUnreadableIncludes recursed through the whole include chain; the
// first version of stripUnreadableIncludes only checked the top-level
// file's own include list, so this exact shape still failed the reload
// even with the flag set. a.yaml's own service must survive; only the
// service the missing nested include would have contributed is absent.
func TestLoadSkipUnreadableIncludesNested(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "stack/a.yaml",
		"include:\n  - ../shared/gone.yaml\nservices:\n  froma:\n    image: nginx\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - stack/a.yaml\nservices:\n  own:\n    image: nginx\n")

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("a missing include nested inside a readable one must not fail the load: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("the top-level file's own service must still load: %v", err)
	}
	if _, err := p.GetService("froma"); err != nil {
		t.Errorf("the readable nested include's own service must still load: %v", err)
	}
	if _, err := p.GetService("fromgone"); err == nil {
		t.Error("a service only the missing doubly-nested include would have declared must not appear")
	}
}

// TestLoadSkipUnreadableIncludesMalformedNested is Gap B: an include
// target that opens fine but does not parse as YAML (e.g. an editor
// mid-save on a shared file) — at least as likely as the file having been
// deleted — must be tolerated the same way, at any depth, not just when
// the file is missing outright.
func TestLoadSkipUnreadableIncludesMalformedNested(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Genuinely broken YAML: an unterminated flow mapping.
	writeFile(t, dir, "stack/broken.yaml", "services:\n  frombroken: [image: nginx\n")
	writeFile(t, dir, "stack/a.yaml",
		"include:\n  - broken.yaml\nservices:\n  froma:\n    image: nginx\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - stack/a.yaml\nservices:\n  own:\n    image: nginx\n")

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("an unparseable nested include must not fail the load: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("the top-level file's own service must still load: %v", err)
	}
	if _, err := p.GetService("froma"); err != nil {
		t.Errorf("the readable nested include's own service must still load: %v", err)
	}
	if _, err := p.GetService("frombroken"); err == nil {
		t.Error("a service only the malformed doubly-nested include would have declared must not appear")
	}
}

// TestLoadSkipUnreadableIncludesMalformedTopLevelInclude covers the
// simpler one-level case of Gap B: a directly-included (not doubly
// nested) file that opens but fails to parse.
func TestLoadSkipUnreadableIncludesMalformedTopLevelInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "broken.yaml", "services:\n  frombroken: [image: nginx\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - broken.yaml\nservices:\n  own:\n    image: nginx\n")

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("an unparseable top-level include must not fail the load: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("the service whose own file never moved must still load: %v", err)
	}
	if _, err := p.GetService("frombroken"); err == nil {
		t.Error("a service only the malformed include would have declared must not appear")
	}
}

// TestLoadSkipUnreadableIncludesProjectDirectory is the third review
// round's finding: an include entry that sets its own project_directory
// changes where the included file's *own* relative include paths resolve
// from (compose-go honors this; it is in the supported-include-keys
// allow-list). This package has no way to compute that override without
// reimplementing compose-go's own resolution, so recursing into such a
// target's own include: list with the wrong base (its own directory
// instead of project_directory) risks silently DROPPING a service
// compose-go itself would load — a wrong result, not just a missed
// tolerance. Reproduces the reviewer's exact shape: compose.yaml includes
// stack/a.yaml with project_directory: other; a.yaml includes
// nested.yaml, which only exists under other/ (i.e. only resolves
// correctly against the project_directory override, not against a.yaml's
// own directory).
func TestLoadSkipUnreadableIncludesProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	// nested.yaml only exists under other/, never under stack/ — so a
	// resolution against a.yaml's own directory (stack/) would find
	// nothing there, while a resolution against project_directory (other/)
	// finds it, exactly as compose-go itself does.
	writeFile(t, dir, "other/nested.yaml", "services:\n  fromnested:\n    image: nginx\n")
	writeFile(t, dir, "stack/a.yaml", "include:\n  - nested.yaml\nservices:\n  froma:\n    image: nginx\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - path: stack/a.yaml\n    project_directory: other\nservices:\n  own:\n    image: nginx\n")

	t.Run("strict load includes the project_directory-resolved service (baseline)", func(t *testing.T) {
		p, err := Load(context.Background(), Options{Files: []string{main}, ProjectName: "conf", WorkingDir: dir})
		if err != nil {
			t.Fatalf("compose-go itself must resolve nested.yaml via project_directory: %v", err)
		}
		if _, err := p.GetService("fromnested"); err != nil {
			t.Fatalf("baseline broken: compose-go did not include fromnested via project_directory: %v", err)
		}
	})

	t.Run("tolerant load must not silently drop the project_directory-resolved service", func(t *testing.T) {
		p, err := Load(context.Background(), Options{
			Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
			SkipUnreadableIncludes: true,
		})
		if err != nil {
			t.Fatalf("a project_directory include with nothing actually wrong must still load: %v", err)
		}
		if _, err := p.GetService("own"); err != nil {
			t.Errorf("the top-level file's own service must load: %v", err)
		}
		if _, err := p.GetService("froma"); err != nil {
			t.Errorf("the directly-included file's own service must load: %v", err)
		}
		if _, err := p.GetService("fromnested"); err != nil {
			t.Errorf("the project_directory-resolved nested service must not be silently dropped: %v", err)
		}
	})
}

// TestIncludeEntryHasProjectDirectoryMergeKey is Minor #6 from the
// whole-branch review: includeEntryHasProjectDirectory used to hand-scan
// an include entry's own item.Content pairwise, which only ever sees keys
// written literally in that mapping — a `project_directory` supplied
// through a YAML merge key ('<<: *anchor') was invisible to it, the
// "known limitation" previously recorded. Now that it goes through
// mappingEntries (already used elsewhere in this package, folds merge
// keys and resolves aliases with the same depth cap), a merge-key-supplied
// project_directory must be detected exactly like a literal one.
func TestIncludeEntryHasProjectDirectoryMergeKey(t *testing.T) {
	const src = `
anchor: &pd
  project_directory: other
entries:
  - <<: *pd
    path: stack/a.yaml
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	root := asMapping(&doc)
	var entries *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "entries" {
			entries = root.Content[i+1]
		}
	}
	if entries == nil || len(entries.Content) == 0 {
		t.Fatal("fixture malformed: no entries found")
	}
	item := entries.Content[0]
	if !includeEntryHasProjectDirectory(item) {
		t.Error("a project_directory supplied through a '<<' merge key must be detected, the same as a literal one")
	}

	// Sanity check on the test itself: an entry with no project_directory
	// at all, merged or literal, must not false-positive.
	const noOverride = `
entries:
  - path: stack/a.yaml
`
	var doc2 yaml.Node
	if err := yaml.Unmarshal([]byte(noOverride), &doc2); err != nil {
		t.Fatal(err)
	}
	root2 := asMapping(&doc2)
	var entries2 *yaml.Node
	for i := 0; i+1 < len(root2.Content); i += 2 {
		if root2.Content[i].Value == "entries" {
			entries2 = root2.Content[i+1]
		}
	}
	if includeEntryHasProjectDirectory(entries2.Content[0]) {
		t.Error("an entry with no project_directory at all must not report having one")
	}
}

// TestLoadSkipUnreadableIncludesCycle is the third review round's test
// gap: nothing exercised the chain cycle-detection logic itself. A
// includes B, B includes A. The bespoke loader's skipUnreadableIncludes
// tolerated a cycle the same way it tolerated any other include failure
// (drop the offending edge, keep going); this asserts the same: the load
// terminates (no hang, no panic — the actual highest-priority risk two
// review rounds probed for and did not find), and every service that IS
// reachable without completing the cycle still loads.
func TestLoadSkipUnreadableIncludesCycle(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - b.yaml\nservices:\n  froma:\n    image: nginx\n")
	writeFile(t, dir, "b.yaml",
		"include:\n  - compose.yaml\nservices:\n  fromb:\n    image: nginx\n")

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("a cycle must be dropped, not fail the load: %v", err)
	}
	if _, err := p.GetService("froma"); err != nil {
		t.Errorf("the top-level file's own service must still load: %v", err)
	}
	if _, err := p.GetService("fromb"); err != nil {
		t.Errorf("the once-included file's own service must still load: %v", err)
	}

	// Every temp file this mechanism stages is meant to be gone once Load
	// returns, cycle or not — confirm no .urunc-compose-reload-*.yaml file
	// is left behind.
	leaked, err := filepath.Glob(filepath.Join(dir, ".urunc-compose-reload-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leaked) != 0 {
		t.Errorf("staged temp file(s) leaked after a cycle: %v", leaked)
	}
}

// TestLoadSkipUnreadableIncludesDepth3 is the third review round's other
// test gap: nothing went past depth 2, so nothing locked in that the
// recursion isn't accidentally capped there. main -> level1 -> level2 ->
// level3 (missing). Every level up to the missing one must still load.
func TestLoadSkipUnreadableIncludesDepth3(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "level2.yaml",
		"include:\n  - level3.yaml\nservices:\n  fromlevel2:\n    image: nginx\n")
	writeFile(t, dir, "level1.yaml",
		"include:\n  - level2.yaml\nservices:\n  fromlevel1:\n    image: nginx\n")
	main := writeFile(t, dir, "compose.yaml",
		"include:\n  - level1.yaml\nservices:\n  own:\n    image: nginx\n")

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("a missing include three levels deep must not fail the load: %v", err)
	}
	for _, want := range []string{"own", "fromlevel1", "fromlevel2"} {
		if _, err := p.GetService(want); err != nil {
			t.Errorf("service %q (depth <= 3) must still load: %v", want, err)
		}
	}
	if _, err := p.GetService("fromlevel3"); err == nil {
		t.Error("a service only the missing depth-4 include would have declared must not appear")
	}
}

// TestLoadSkipUnreadableIncludesExtendsBaseDeleted is the whole-branch
// review's Important #1: 'include:' targets already got reload tolerance
// (the tests above), but 'service.extends' was flipped to supported on
// this same branch with none of its own — an extends base file that
// moves, disappears, or is mid-save failed the whole reload, which stops
// reloadProject's caller (the supervisor's targets() call, and 'down')
// restarting or tearing down every OTHER service in the project for as
// long as the drift lasts. This proves the degraded-not-failed contract
// stripUnreadableExtends now gives extends: the service whose base went
// missing loses its 'extends' (falls back to whatever it declares on its
// own), and every other service in the file still loads, the same way a
// broken 'include' target already does.
func TestLoadSkipUnreadableIncludesExtendsBaseDeleted(t *testing.T) {
	dir := t.TempDir()
	main := writeFile(t, dir, "compose.yaml",
		"services:\n"+
			"  own:\n    image: nginx\n"+
			"  web:\n    extends:\n      file: base.yaml\n      service: base\n"+
			"    image: fallback\n")
	// base.yaml is deliberately never written: this is the "moved or
	// deleted mid-reload" case Important #1 is about.

	p, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
		SkipUnreadableIncludes: true,
	})
	if err != nil {
		t.Fatalf("a missing extends base file must not fail the reload: %v", err)
	}
	if _, err := p.GetService("own"); err != nil {
		t.Errorf("a service with no extends at all must still load: %v", err)
	}
	web, err := p.GetService("web")
	if err != nil {
		t.Fatalf("the service whose extends base went missing must still load, degraded: %v", err)
	}
	if web.Image != "fallback" {
		t.Errorf("web.Image = %q, want its own declared fallback image (extends dropped, not the whole service)", web.Image)
	}

	// Without the tolerance flag, the same file is the whole point of the
	// error: pins that this is reload-only tolerance, not a general
	// weakening of extends validation.
	if _, err := Load(context.Background(), Options{
		Files: []string{main}, ProjectName: "conf", WorkingDir: dir,
	}); err == nil {
		t.Fatal("a missing extends base file must still fail the load when SkipUnreadableIncludes is not set")
	}
}

// TestLoadRejectsOversizedIncludedFileBeforeParse proves Important #2's
// fix: the size cap on a top-level `include:` target now runs BEFORE
// compose-go reads it, not after. The fixture is both over the cap AND
// invalid YAML (an unterminated flow sequence). If the cap only ran after
// compose-go's own read (the bug this locks in against), compose-go's
// parser would choke on the invalid syntax first — that error surfaces
// long before Load's post-hoc cap ever gets a turn, and it would not
// mention the size limit at all. With the cap running first, Load never
// hands compose-go this file, so the only error it can produce is the
// size-cap one.
func TestLoadRejectsOversizedIncludedFileBeforeParse(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// An unclosed flow sequence, repeated past the cap: never valid YAML no
	// matter how much of it a parser reads.
	big := make([]byte, MaxUserFileBytes+1)
	for i := range big {
		big[i] = '['
	}
	incPath := filepath.Join(sub, "included.yaml")
	if err := os.WriteFile(incPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	main := writeFile(t, dir, "compose.yaml", "include:\n  - ./subdir/included.yaml\nservices:\n  a:\n    image: nginx\n")

	_, err := Load(context.Background(), Options{Files: []string{main}, ProjectName: "conf", WorkingDir: dir})
	wantSizeCapErr(t, err)
}
