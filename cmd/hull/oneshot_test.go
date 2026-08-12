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

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/brig-sh/hull/pkg/store"
)

// TestCheckCompletedDeps pins the ADR-0007 invariant directly: a dependent
// starts only when the service it gates on completed with code 0, and every
// unknown case is a refusal.
func TestCheckCompletedDeps(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed := func(id string, code *int) {
		t.Helper()
		if _, err := s.CreateInstance(id); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveInstance(&store.InstanceState{ID: id, Status: "stopped", ExitCode: code}); err != nil {
			t.Fatal(err)
		}
	}
	zero, seven := 0, 7
	seed("ok", &zero)
	seed("failed", &seven)
	seed("nostatus", nil)

	proj := &composeProject{Services: map[string]string{
		"ok": "ok", "failed": "failed", "nostatus": "nostatus",
	}}
	// Required mirrors what the loader produces: depends_on entries default
	// to required, and only an explicit 'required: false' is optional.
	gate := func(depName string) error {
		return checkCompletedDeps(s, proj, "api", types.DependsOnConfig{
			depName: {Condition: "service_completed_successfully", Required: true},
		}, io.Discard)
	}

	if err := gate("ok"); err != nil {
		t.Errorf("a job that exited 0 must let the dependent start, got %v", err)
	}
	for name, want := range map[string]string{
		"failed":   "exited 7",
		"nostatus": "reported no exit status",
		"missing":  "never ran",
	} {
		t.Run(name, func(t *testing.T) {
			err := gate(name)
			if err == nil {
				t.Fatalf("dependency %q must NOT let the dependent start", name)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		})
	}

	// Other conditions are not this gate's business.
	if err := checkCompletedDeps(s, proj, "api", types.DependsOnConfig{
		"failed":   {Condition: "service_started", Required: true},
		"nostatus": {Condition: "service_healthy", Required: true},
	}, io.Discard); err != nil {
		t.Errorf("non-completion conditions must be ignored here, got %v", err)
	}

	// 'required: false' never fails its dependent. Docker only warns when an
	// optional dependency is not started or not available, so every refusal
	// above becomes a warning line and the dependent starts.
	for _, name := range []string{"failed", "nostatus", "missing"} {
		t.Run("optional/"+name, func(t *testing.T) {
			var warn bytes.Buffer
			err := checkCompletedDeps(s, proj, "api", types.DependsOnConfig{
				name: {Condition: "service_completed_successfully"},
			}, &warn)
			if err != nil {
				t.Errorf("an optional dependency must not stop the dependent, got %v", err)
			}
			if !strings.Contains(warn.String(), `optional dependency "`+name+`"`) {
				t.Errorf("an optional dependency that did not complete must warn, got:\n%s", warn.String())
			}
		})
	}
}

// TestOneShotInitArgvIsTransportSafe guards the kernel-cmdline transport:
// guest argv is joined with spaces and re-split by the kernel, so an element
// containing whitespace would arrive as two and kill every job's init.
func TestOneShotInitArgvIsTransportSafe(t *testing.T) {
	if len(oneshotInitCommand) == 0 {
		t.Fatal("the one-shot init command must not be empty")
	}
	for _, arg := range oneshotInitCommand {
		if strings.ContainsAny(arg, " \t\n") {
			t.Errorf("one-shot init argv %q contains whitespace: the kernel cmdline would split it", arg)
		}
	}
}

func TestOneShotFailureCarriesCodeAndOutput(t *testing.T) {
	err := oneShotFailure("migrate", jobResult{code: 3, output: "  boom\n"})
	if !strings.Contains(err.Error(), "exited 3") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want the code and the output", err)
	}
	// A long output is truncated rather than dumped whole.
	long := oneShotFailure("m", jobResult{code: 1, output: strings.Repeat("x", 5000)})
	if !strings.Contains(long.Error(), "output truncated") {
		t.Error("a long job output must be truncated")
	}
}

func TestRunOneShotRejectsEmptyCommand(t *testing.T) {
	if _, err := runOneShot(nil, "inst", "svc", nil, nil, time.Second, time.Second); err == nil ||
		!strings.Contains(err.Error(), "needs a 'command'") {
		t.Errorf("want the missing-command error, got %v", err)
	}
}
