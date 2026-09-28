// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// nestedUnsupported starts the refusal hull prints and hvi prints too, each
// with its own detail after a colon; brig passes hull's words on as they
// are. One phrase then finds the failure in any of their logs.
const nestedUnsupported = "nested virtualization requested but not supported by this host"

// hviProbeTimeout bounds `hvi caps --json`. The probe asks Hypervisor.framework
// one question and creates no VM; an hvi that has not answered in this long
// is stuck, and a stuck probe must not hang `hull run` or brig's doctor.
var hviProbeTimeout = 5 * time.Second

// capabilitiesPayload is the `hull capabilities --json` document. Consumers
// (brig) read schemaVersion first; fields are only ever added under 1.
type capabilitiesPayload struct {
	SchemaVersion int           `json:"schemaVersion"`
	NestedVirt    nestedSupport `json:"nestedVirt"`
}

// nestedSupport says whether a guest can be given hardware virtualization of
// its own, which backend would provide it, and why not when it cannot.
//
// Answered separates the two kinds of "no". True means hvi answered the
// question with a schema-1 document, so Supported is the host's answer. False
// means hull never got an answer (no hvi, a timeout, a non-zero exit, output
// it could not read), so Supported is false because nothing said yes, and
// Detail is the failure. A caller that treats a host without EL2 differently
// from a broken probe keys on this and not on hvi's wording.
type nestedSupport struct {
	Supported bool   `json:"supported"`
	Answered  bool   `json:"answered"`
	Backend   string `json:"backend"`
	Detail    string `json:"detail"`
}

// hviCaps is the part of `hvi caps --json` hull reads.
type hviCaps struct {
	SchemaVersion int `json:"schemaVersion"`
	NestedVirt    *struct {
		Supported bool   `json:"supported"`
		Detail    string `json:"detail"`
	} `json:"nestedVirt"`
}

func capabilitiesCommand() *cli.Command {
	return &cli.Command{
		Name:  "capabilities",
		Usage: "report what this host can offer a guest (nested virtualization)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "machine-readable output"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			p := capabilitiesPayload{SchemaVersion: 1, NestedVirt: hostNestedVirt(ctx)}
			if cmd.Bool("json") {
				return printJSON(os.Stdout, p)
			}
			fmt.Println(nestedLine(p.NestedVirt))
			return nil
		},
	}
}

// nestedLine is the one-line text form of the probe.
func nestedLine(n nestedSupport) string {
	if n.Supported {
		return fmt.Sprintf("nested virtualization: supported (%s)", n.Backend)
	}
	if !n.Answered {
		return fmt.Sprintf("nested virtualization: not supported (%s did not answer): %s", n.Backend, n.Detail)
	}
	return fmt.Sprintf("nested virtualization: not supported (%s): %s", n.Backend, n.Detail)
}

// hostNestedVirt probes the hvi `hull run --hypervisor hvi` would start. A
// probe of any other copy could answer for a binary that never boots the
// guest, so the path comes from the same resolution urunc uses.
func hostNestedVirt(ctx context.Context) nestedSupport {
	path, err := resolveHviPath()
	if err != nil {
		return nestedSupport{Backend: string(hypervisors.HviVmm), Detail: err.Error()}
	}
	return probeNestedVirt(ctx, path)
}

// resolveHviPath returns the hvi binary urunc's darwin factory picks: the
// urunc config's monitor path, then hvi next to this executable, then PATH.
func resolveHviPath() (string, error) {
	monitors := map[string]types.MonitorConfig{}
	if cfg, _ := unikontainers.LoadUruncConfig(unikontainers.UruncConfigPath); cfg != nil && cfg.Monitors != nil {
		monitors = cfg.Monitors
	}
	vmm, err := hypervisors.NewVMM(hypervisors.HviVmm, monitors)
	if err != nil {
		return "", err
	}
	return vmm.Path(), nil
}

// probeNestedVirt runs `<hviPath> caps --json` and maps every outcome onto a
// yes or a no with a reason. Nothing here returns an error: a probe that
// cannot answer, whatever the cause, is a "no" carrying that cause, because
// the caller's only safe reading of "could not tell" is to not boot a guest
// that asked for EL2.
func probeNestedVirt(ctx context.Context, hviPath string) nestedSupport {
	no := func(format string, a ...any) nestedSupport {
		return nestedSupport{Backend: string(hypervisors.HviVmm), Detail: fmt.Sprintf(format, a...)}
	}
	stdout, stderr, err := runHviProbe(ctx, hviPath, "caps", "--json")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return no("%s caps --json did not answer within %s", hviPath, hviProbeTimeout)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return no("run %s caps --json: %v", hviPath, err)
		}
		if msg := firstLine(stderr); msg != "" {
			return no("hvi caps failed: %s", msg)
		}
		return no("hvi caps failed: %v", err)
	}
	var doc hviCaps
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return no("hvi caps printed something other than its JSON document: %v", err)
	}
	if doc.SchemaVersion != 1 {
		return no("hvi caps answered with schemaVersion %d; this hull reads 1", doc.SchemaVersion)
	}
	if doc.NestedVirt == nil {
		return no("hvi caps did not report nestedVirt")
	}
	return nestedSupport{
		Supported: doc.NestedVirt.Supported,
		Answered:  true,
		Backend:   string(hypervisors.HviVmm),
		Detail:    doc.NestedVirt.Detail,
	}
}

// requireNestedVirt is the `hull run --nested-virt` preflight. Without it a
// detached run on a host without EL2 would report success and the instance
// would die a moment later, which brig sees only as a sandbox that never
// became ready.
func requireNestedVirt(ctx context.Context) error {
	if n := hostNestedVirt(ctx); !n.Supported {
		return fmt.Errorf("%s: %s", nestedUnsupported, n.Detail)
	}
	return nil
}

func runHviProbe(ctx context.Context, path string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, hviProbeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, path, args...)
	c.Stdout, c.Stderr = &stdout, &stderr
	// A child that inherits the pipes and outlives hvi would otherwise keep
	// Wait blocked after the kill, and the timeout would bound nothing.
	c.WaitDelay = time.Second
	err := c.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
