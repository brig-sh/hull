package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

// The OTLP resource and scope name is what dashboards select on, so it is a
// wire value rather than an internal label. Renaming it silently detaches
// every panel from the data, which is exactly the kind of break nobody
// notices until someone asks why a chart is empty.
func TestOTLPCarriesServiceName(t *testing.T) {
	out := otlpWrap([]byte(`{"event":"command"}`))
	if !strings.Contains(string(out), ServiceName) {
		t.Fatalf("envelope does not carry %q:\n%s", ServiceName, out)
	}

	var env map[string]any
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	rl := env["resourceLogs"].([]any)[0].(map[string]any)
	attrs := rl["resource"].(map[string]any)["attributes"].([]any)
	got := attrs[0].(map[string]any)["value"].(map[string]any)["stringValue"]
	if got != ServiceName {
		t.Errorf("service.name = %v, want %v", got, ServiceName)
	}
	scope := rl["scopeLogs"].([]any)[0].(map[string]any)["scope"].(map[string]any)
	if scope["name"] != ServiceName {
		t.Errorf("scope name = %v, want %v", scope["name"], ServiceName)
	}
}

// TestOTLPPromotesSchemaVersionAsAnAttribute pins the field a collector
// needs to route on. schema_version says how to read the rest of the
// record -- cpu_pct means one thing under 1 and another under 2 -- and it
// is a number in the payload, so a string-only promotion silently dropped
// it and left the body JSON as the only place it existed.
func TestOTLPPromotesSchemaVersionAsAnAttribute(t *testing.T) {
	flat := []byte(`{"schema_version":2,"event":"metrics","product":"brig",` +
		`"version":"0.1.0-rc28","install_id":"abc","captured_at":"2026-09-08T00:00:00Z",` +
		`"checksum":"deadbeef","cpu_pct":"48.5"}`)

	var out map[string]any
	if err := json.Unmarshal(otlpWrap(flat), &out); err != nil {
		t.Fatalf("wrapped payload is not JSON: %v", err)
	}
	rl := out["resourceLogs"].([]any)[0].(map[string]any)
	rec := rl["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)

	got := map[string]string{}
	for _, a := range rec["attributes"].([]any) {
		attr := a.(map[string]any)
		got[attr["key"].(string)] = attr["value"].(map[string]any)["stringValue"].(string)
	}
	// "2", not "2.0": a collector filter compares this to a version number.
	if got["schema_version"] != "2" {
		t.Errorf("schema_version attribute = %q, want %q", got["schema_version"], "2")
	}
	for _, k := range []string{"event", "product", "version", "install_id", "captured_at", "checksum"} {
		if got[k] == "" {
			t.Errorf("attribute %q was dropped", k)
		}
	}
}
