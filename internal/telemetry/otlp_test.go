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
