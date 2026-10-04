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

package telemetry

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// otlpWrap converts one flat event payload into an OTLP/HTTP JSON log
// export, so a stock OpenTelemetry collector can ingest it with zero
// custom code. The flat JSON rides as the log record body; envelope
// fields are duplicated as attributes for routing, filtering, and the
// ingestion-side checksum validation (the collector recomputes the
// checksum from these attributes and drops mismatches).
// attrString renders an envelope value for an OTLP attribute. Everything
// promoted is a string except schema_version, which is a number in the
// payload and would otherwise be dropped by a string type assertion --
// leaving the collector unable to route on the one field that says how to
// read the rest.
func attrString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64: // every JSON number unmarshals to this
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

func otlpWrap(flat []byte) []byte {
	var fields map[string]any
	_ = json.Unmarshal(flat, &fields)

	var attrs []map[string]any
	for _, k := range []string{"schema_version", "event", "product", "version", "install_id", "captured_at", "checksum"} {
		v, ok := attrString(fields[k])
		if !ok {
			continue
		}
		attrs = append(attrs, map[string]any{
			"key":   k,
			"value": map[string]any{"stringValue": v},
		})
	}
	wrapped, err := json.Marshal(map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{map[string]any{
					"key":   "service.name",
					"value": map[string]any{"stringValue": ServiceName},
				}},
			},
			"scopeLogs": []any{map[string]any{
				"scope": map[string]any{"name": ServiceName},
				"logRecords": []any{map[string]any{
					"timeUnixNano": fmt.Sprintf("%d", time.Now().UnixNano()),
					"body":         map[string]any{"stringValue": string(flat)},
					"attributes":   attrs,
				}},
			}},
		}},
	})
	if err != nil {
		return flat
	}
	return wrapped
}
