// Package compose wraps compose-spec/compose-go's project loader with the
// urunc-macos-specific behavior (file-size caps, the urunc x-* extensions)
// layered on top of the typed *types.Project model it returns.
package compose

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

const (
	// XHypervisorKey and XOneShotKey are exported so callers that need to
	// synthesize these extensions (compose config's hydration of computed
	// values, cmd/urunc-macos/compose.go) write the same key Hypervisor and
	// OneShot read, instead of duplicating the string literal.
	XHypervisorKey = "x-hypervisor"
	XOneShotKey    = "x-oneshot"
	xHealthTCPKey  = "x-healthcheck-tcp"
)

// HealthTCP mirrors the bespoke healthTCP (cmd/urunc-macos/compose.go:453):
// x-healthcheck-tcp accepts a bare port (x-healthcheck-tcp: 5432) or a
// mapping with docker-style tuning: {port, interval, retries, start_period}.
// The probe itself is a plain TCP connect: it proves a listener exists, not
// that the app is ready — start_period is the tool for services that
// restart during init.
type HealthTCP struct {
	Port        int
	Interval    time.Duration
	Retries     int
	StartPeriod time.Duration
	// Declared distinguishes an x-healthcheck-tcp key that was present but
	// useless (empty mapping, port 0) from the key being absent, so the
	// former can warn instead of vanishing.
	Declared bool
}

// ProbeBudget returns the TCP-connect retry interval and the total time
// budget (start_period plus interval*retries) a caller should allow before
// giving up. Same defaults as the bespoke implementation: a 1s interval and
// 60 retries when unset.
func (h HealthTCP) ProbeBudget() (interval, total time.Duration) {
	interval = h.Interval
	if interval <= 0 {
		interval = time.Second
	}
	retries := h.Retries
	if retries <= 0 {
		retries = 60
	}
	return interval, h.StartPeriod + interval*time.Duration(retries)
}

// Hypervisor returns the service's x-hypervisor value, or "" when the key
// is absent or not a string.
func Hypervisor(svc types.ServiceConfig) string {
	v, ok := svc.Extensions[XHypervisorKey]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// OneShot reports whether the service declares x-oneshot: true.
func OneShot(svc types.ServiceConfig) bool {
	v, ok := svc.Extensions[XOneShotKey]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// HealthTCPFor decodes the service's x-healthcheck-tcp extension. Declared
// is false when the key is absent; HealthTCPFor returns the zero HealthTCP
// in that case.
//
// Unlike the bespoke yaml.Node-based UnmarshalYAML this ports (which had a
// YAML line number available for its error messages), the typed
// compose-go model hands extension values back as already-decoded `any`
// with no source position attached, so the "line %d: " prefix the bespoke
// errors carried is dropped here.
func HealthTCPFor(svc types.ServiceConfig) (HealthTCP, error) {
	v, ok := svc.Extensions[xHealthTCPKey]
	if !ok {
		return HealthTCP{}, nil
	}

	h := HealthTCP{Declared: true}

	switch val := v.(type) {
	case string:
		port, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return HealthTCP{}, fmt.Errorf("invalid x-healthcheck-tcp port %q", val)
		}
		h.Port = port
		return h, nil
	case int:
		h.Port = val
		return h, nil
	case int64:
		h.Port = int(val)
		return h, nil
	case uint64:
		h.Port = int(val)
		return h, nil
	case float64:
		h.Port = int(val)
		return h, nil
	case map[string]any:
		if p, ok := val["port"]; ok {
			port, err := toInt(p)
			if err != nil {
				return HealthTCP{}, fmt.Errorf("invalid x-healthcheck-tcp port %q", fmt.Sprint(p))
			}
			h.Port = port
		}
		if r, ok := val["retries"]; ok {
			retries, err := toInt(r)
			if err != nil {
				return HealthTCP{}, fmt.Errorf("invalid x-healthcheck-tcp retries %q", fmt.Sprint(r))
			}
			h.Retries = retries
		}
		if iv, ok := val["interval"]; ok {
			s, ok := iv.(string)
			if !ok {
				return HealthTCP{}, fmt.Errorf("invalid interval %q", fmt.Sprint(iv))
			}
			if s != "" {
				d, err := time.ParseDuration(s)
				if err != nil {
					return HealthTCP{}, fmt.Errorf("invalid interval %q", s)
				}
				h.Interval = d
			}
		}
		if sp, ok := val["start_period"]; ok {
			s, ok := sp.(string)
			if !ok {
				return HealthTCP{}, fmt.Errorf("invalid start_period %q", fmt.Sprint(sp))
			}
			if s != "" {
				d, err := time.ParseDuration(s)
				if err != nil {
					return HealthTCP{}, fmt.Errorf("invalid start_period %q", s)
				}
				h.StartPeriod = d
			}
		}
		return h, nil
	default:
		// Any other scalar (bool, nil, ...) falls through the same "port"
		// wording the bespoke UnmarshalYAML used for every non-mapping
		// scalar node, since it decoded via node.Value + Atoi regardless of
		// the scalar's YAML type.
		return HealthTCP{}, fmt.Errorf("invalid x-healthcheck-tcp port %q", fmt.Sprint(v))
	}
}

// toInt converts a decoded YAML numeric any into an int, for the
// mapping-form "port" and "retries" fields specifically. compose-go's
// extension decoding can hand back any of these concrete numeric types
// depending on the value and its internal YAML decoder; see extensions.go's
// package comment / task-5 report for which one v2.14.0 actually produces.
//
// Deliberately no string case: the bespoke code decoded these two fields
// into typed struct fields (Port int, Retries int) via node.Decode, so a
// quoted numeric string there (e.g. {port: "80"}) was a hard decode error,
// not an accepted value. toInt preserves that reject behavior; only the
// bare top-level scalar form (x-healthcheck-tcp: "5432", handled separately
// in HealthTCPFor's `case string`) is a string-accepting port form, matching
// the bespoke UnmarshalYAML's ScalarNode branch.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case uint64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}
