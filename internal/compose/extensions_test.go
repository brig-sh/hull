package compose

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHealthTCPForms(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  bare:
    image: img
    x-healthcheck-tcp: 5432
  tuned:
    image: img
    x-healthcheck-tcp: {port: 80, interval: 5s, retries: 3, start_period: 10s}
  none:
    image: img
  job:
    image: img
    x-oneshot: true
    x-hypervisor: qemu
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	bare, _ := p.GetService("bare")
	h, err := HealthTCPFor(bare)
	if err != nil || !h.Declared || h.Port != 5432 {
		t.Fatalf("bare: %+v %v", h, err)
	}
	tuned, _ := p.GetService("tuned")
	h, _ = HealthTCPFor(tuned)
	if h.Port != 80 || h.Interval != 5*time.Second || h.Retries != 3 || h.StartPeriod != 10*time.Second {
		t.Fatalf("tuned: %+v", h)
	}
	none, _ := p.GetService("none")
	if h, _ := HealthTCPFor(none); h.Declared {
		t.Fatal("none: Declared should be false")
	}
	job, _ := p.GetService("job")
	if !OneShot(job) || Hypervisor(job) != "qemu" {
		t.Fatal("job extensions")
	}
}

// TestHealthTCPInvalidPort covers the brief's required "invalid port
// string (error)" case: a bare scalar that isn't a valid integer must fail
// HealthTCPFor with an error naming the bad value, mirroring the bespoke
// UnmarshalYAML's port-parse error.
func TestHealthTCPInvalidPort(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  bad:
    image: img
    x-healthcheck-tcp: "not-a-port"
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := p.GetService("bad")
	h, err := HealthTCPFor(bad)
	if err == nil {
		t.Fatalf("want error for invalid port, got %+v", h)
	}
	if !strings.Contains(err.Error(), `invalid x-healthcheck-tcp port "not-a-port"`) {
		t.Fatalf("error = %q, want it to name the bad port value", err.Error())
	}
}

// TestHealthTCPMappingQuotedPortRejected covers a parity gap a task-5
// review found: the mapping form's "port" (and "retries") fields must
// reject a quoted numeric string exactly like the bespoke code did,
// because the bespoke code decoded them into typed struct fields
// (Port int, Retries int) via node.Decode, and yaml.v3 refuses to
// unmarshal a quoted string like "80" into an int field. Only the bare
// top-level scalar form (x-healthcheck-tcp: "5432") accepts a string.
func TestHealthTCPMappingQuotedPortRejected(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  bad:
    image: img
    x-healthcheck-tcp: {port: "80"}
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := p.GetService("bad")
	h, err := HealthTCPFor(bad)
	if err == nil {
		t.Fatalf("want error for quoted port in mapping form, got %+v", h)
	}
	if !strings.Contains(err.Error(), `invalid x-healthcheck-tcp port "80"`) {
		t.Fatalf("error = %q, want it to name the bad port value", err.Error())
	}
}

// TestHealthTCPEmptyMapping covers the Declared-vs-absent distinction the
// bespoke type's doc comment calls out: an x-healthcheck-tcp key that is
// present but useless (an empty mapping, port 0) must still report
// Declared=true, unlike the key being absent entirely (see "none" in
// TestHealthTCPForms).
func TestHealthTCPEmptyMapping(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  empty:
    image: img
    x-healthcheck-tcp: {}
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	empty, _ := p.GetService("empty")
	h, err := HealthTCPFor(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Declared || h.Port != 0 {
		t.Fatalf("empty mapping: %+v", h)
	}
}

// TestHealthTCPNonStringInterval covers a mapping form where interval (or
// start_period) is given as a bare number instead of a duration string
// (e.g. "interval: 5" instead of "interval: 5s"). The bespoke
// UnmarshalYAML decoded into a struct field typed `Interval string`, so a
// non-string YAML value there was a decode error, not a silent no-op; this
// preserves that by erroring instead of dropping the value.
func TestHealthTCPNonStringInterval(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "compose.yaml", `
services:
  bad:
    image: img
    x-healthcheck-tcp: {port: 80, interval: 5}
`)
	p, err := Load(context.Background(), Options{Files: []string{f}, ProjectName: "conf", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := p.GetService("bad")
	h, err := HealthTCPFor(bad)
	if err == nil {
		t.Fatalf("want error for non-string interval, got %+v", h)
	}
	if !strings.Contains(err.Error(), "invalid interval") {
		t.Fatalf("error = %q, want it to mention interval", err.Error())
	}
}
