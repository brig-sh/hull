// Copyright (c) 2023-2026, Nubificus LTD
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
	"time"
)

// otlpWrap converts one flat event payload into an OTLP/HTTP JSON log
// export, so a stock OpenTelemetry collector can ingest it with zero
// custom code. The flat JSON rides as the log record body; envelope
// fields are duplicated as attributes for routing, filtering, and the
// ingestion-side checksum validation (the collector recomputes the
// checksum from these attributes and drops mismatches).
func otlpWrap(flat []byte) []byte {
	var fields map[string]any
	_ = json.Unmarshal(flat, &fields)

	var attrs []map[string]any
	for _, k := range []string{"event", "product", "version", "install_id", "captured_at", "checksum"} {
		if v, ok := fields[k].(string); ok {
			attrs = append(attrs, map[string]any{
				"key":   k,
				"value": map[string]any{"stringValue": v},
			})
		}
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
