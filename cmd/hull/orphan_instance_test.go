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

//go:build darwin

package main

// --- review finding: the orphan-instance window ------------------------------
//
// launchVMM used to spawn the VMM and only then write the store record. A
// SIGKILL of hull between the two -- and a SIGKILL runs no deferred cleanup --
// left a running VMM that nothing could reach: `ps` listed nothing, `stop`
// said "instance not found", and the instance directory kept the name squatted
// forever, so every later run failed ErrInstanceExists against a name `rm`
// claimed did not exist. rm.go's removeWedgedInstance closed the recovery half
// of that. These tests hold the other half: the window itself.
//
// The pid is genuinely not knowable before the process exists, so the record is
// published in two steps -- "starting" with no pid before the spawn, then the
// pid and "running" immediately after -- and every reader has to cope with the
// intermediate state.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"

	"github.com/brig-sh/hull/pkg/store"
)

// standInVMM is a process that behaves enough like a VMM for launchVMM: it
// records that it was started, and then stays up. The marker is what proves
// whether a VMM was spawned, which is the whole question here.
func standInVMM(t *testing.T, marker string) []string {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(marker)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return []string{"/bin/sh", "-c", "echo $$ > " + marker + "; exec sleep 30"}
}

// The invariant, end to end: hull must not have a VMM the store does not know
// about, so a store it cannot write is a reason not to start one.
//
// The crash is simulated rather than delivered. SIGKILLing a real `hull` at the
// exact instruction between fork/exec and the state write is not something a
// test can time, so the failure is injected at the same seam instead: the state
// file is made unwritable, which is the same thing the crash achieves -- the
// record does not land. Before the fix the VMM was spawned anyway and the
// failed write was a warning; now the spawn does not happen at all.
func TestNoVMMIsSpawnedWithoutARecordOfIt(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instanceDir, err := s.CreateInstance("vm1")
	if err != nil {
		t.Fatal(err)
	}
	// Make every SaveInstance for this instance fail, without touching the
	// permissions of the directory the log file also lives in: a directory
	// where state.json belongs is a write that can never succeed.
	if err := os.Mkdir(filepath.Join(instanceDir, "state.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "vmm-started")
	state := &store.InstanceState{ID: "vm1", LogFile: s.InstanceLogFile("vm1")}
	started, err := launchVMM(&cli.Command{}, s, state, standInVMM(t, marker),
		nil, hypervisors.VzVmm, true, "none", "")

	if err == nil {
		t.Errorf("launchVMM reported success with a store it could not write to; "+
			"a crash there leaves a VMM nothing can reach (started=%v)", started)
	}
	if started {
		t.Errorf("launchVMM reported the instance as started, so the caller keeps a " +
			"directory for a VMM that has no record")
	}
	// Only worth waiting on when something already went wrong: the marker
	// appears a moment after the fork, so a bare Stat would usually miss the
	// very orphan being reported. On the passing path nothing was spawned and
	// there is nothing to wait for.
	if started || err == nil {
		for range 50 {
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("a VMM was spawned even though its record could not be written: " +
					"that is exactly the orphan this fix exists to prevent")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// The record that a crash in the window leaves behind has to be usable, or the
// fix has only moved the wedge. Everything an operator would reach for must
// work on it, and none of it may read a missing pid as a live process.
func TestStartingRecordIsVisibleStoppableAndRemovable(t *testing.T) {
	// Exactly what launchVMM leaves on disk between the spawn and the pid:
	// written by hand rather than by killing hull, because the window is a few
	// microseconds wide and cannot be hit reliably from outside.
	newCrashedInstance := func(t *testing.T) (*store.Store, string) {
		t.Helper()
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateInstance("vm1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveInstance(&store.InstanceState{
			ID: "vm1", Status: "starting", PID: 0, CmdLine: []string{"vz-runner", "--kernel", "x"},
		}); err != nil {
			t.Fatal(err)
		}
		return s, "vm1"
	}

	t.Run("ps shows it without claiming it is running", func(t *testing.T) {
		s, _ := newCrashedInstance(t)
		var out strings.Builder
		if err := renderInstances(s, &out); err != nil {
			t.Fatal(err)
		}
		line := out.String()
		if !strings.Contains(line, "vm1") {
			t.Fatalf("the instance is invisible to ps:\n%s", line)
		}
		if strings.Contains(line, "running") {
			t.Errorf("ps calls a record with no pid running:\n%s", line)
		}
		// The PID column has to be empty rather than a zero, which would read
		// as a process number someone could signal.
		for _, field := range strings.Fields(line) {
			if field == "0" {
				t.Errorf("ps prints a zero pid, which reads as a process number:\n%s", line)
			}
		}
		t.Logf("ps renders the crashed instance as:\n%s", line)
	})

	t.Run("stop marks it down instead of signalling pid 0", func(t *testing.T) {
		s, id := newCrashedInstance(t)
		if err := stopInstanceIn(s, id, 1); err != nil {
			t.Fatalf("stop: %v", err)
		}
		st, err := s.GetInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != "stopped" {
			t.Errorf("status after stop is %q, want stopped", st.Status)
		}
		if !st.StoppedByUser {
			t.Error("stop did not record the intent, so the supervisor would restart it")
		}
	})

	t.Run("the name is still refused to a second run", func(t *testing.T) {
		// Publishing the record earlier must not make the name any easier to
		// take: a genuine second `hull run` of the same name, while the first
		// is mid-spawn, still has to be refused. The gate is the directory,
		// which CreateInstance makes before any of this, so the record's
		// arrival time does not enter into it.
		s, id := newCrashedInstance(t)
		if _, err := s.CreateInstance(id); !errors.Is(err, store.ErrInstanceExists) {
			t.Errorf("a second run of %q was not refused: %v", id, err)
		}
	})

	t.Run("rm clears it and frees the name", func(t *testing.T) {
		s, id := newCrashedInstance(t)
		// No --force: refusing here is what left the name squatted, since
		// there is no pid to force anything against.
		if err := removeInstanceIn(s, id, false); err != nil {
			t.Fatalf("rm: %v", err)
		}
		if _, err := os.Stat(s.InstanceDir(id)); !os.IsNotExist(err) {
			t.Errorf("the instance directory survived rm: %v", err)
		}
		if _, err := s.CreateInstance(id); err != nil {
			t.Errorf("the name is still squatted after rm: %v", err)
		}
	})
}

// The failure path. A spawn that never happened must not leave a record saying
// one is starting, or every failed run wedges its own name at "starting".
func TestFailedSpawnLeavesNoStartingRecord(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance("vm1"); err != nil {
		t.Fatal(err)
	}
	state := &store.InstanceState{ID: "vm1", LogFile: s.InstanceLogFile("vm1")}

	started, err := launchVMM(&cli.Command{}, s, state,
		[]string{filepath.Join(t.TempDir(), "no-such-vmm")}, nil,
		hypervisors.VzVmm, true, "none", "")
	if err == nil {
		t.Fatal("launchVMM succeeded with a VMM binary that does not exist")
	}
	if started {
		t.Error("launchVMM reported a started instance for a spawn that failed")
	}
	if _, err := s.GetInstance("vm1"); err == nil {
		t.Error("a failed spawn left a record behind; the instance is now stuck at starting")
	}
}

// The ordering, guarded at the source, because no test can stop the process
// between the two statements that matter.
//
// launchVMM must write the instance record before it spawns anything. The
// record is what makes an instance findable -- not the pid, which cannot exist
// yet -- so a spawn that happens first is a VMM that nothing can reach for as
// long as the write takes, and forever if hull dies in between.
func TestInstanceIsRecordedBeforeTheVMMIsSpawned(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "launchVMM" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("launchVMM is gone from run.go; this guard has stopped guarding anything")
	}

	firstSave, spawn := token.NoPos, token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "SaveInstance":
			if !firstSave.IsValid() || call.Pos() < firstSave {
				firstSave = call.Pos()
			}
		case "Start":
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "vmmCmd" && !spawn.IsValid() {
				spawn = call.Pos()
			}
		}
		return true
	})

	if !spawn.IsValid() {
		t.Fatal("no vmmCmd.Start() in launchVMM; this guard has stopped guarding anything")
	}

	// What the pre-spawn record SAYS matters as much as its existence. Writing
	// "running" there passes every other test in this package -- and it would
	// be worse than the bug it replaced: ps only self-corrects a record with a
	// pid on it, and brig's Running() matches this field exactly, so a VM that
	// never booted would be reported as up forever. Nothing pinned this, so
	// pin it here, next to the ordering it belongs with.
	var statusBeforeSave string
	var pidBeforeSave string
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Pos() > firstSave || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "state" {
			return true
		}
		switch sel.Sel.Name {
		case "Status":
			statusBeforeSave = types.ExprString(assign.Rhs[0])
		case "PID":
			pidBeforeSave = types.ExprString(assign.Rhs[0])
		}
		return true
	})

	if statusBeforeSave != "store.StatusStarting" {
		t.Errorf("the record published before the spawn has Status = %q, want "+
			"store.StatusStarting. A record that claims \"running\" before anything is "+
			"running never self-corrects -- ps only reconciles a record with a pid -- and "+
			"brig reports it as a live sandbox", statusBeforeSave)
	}
	if pidBeforeSave != "" && pidBeforeSave != "0" {
		t.Errorf("the record published before the spawn sets PID = %s; there is no pid "+
			"yet, and a non-zero one here would make stop and rm signal whatever owns it",
			pidBeforeSave)
	}
	if !firstSave.IsValid() || firstSave > spawn {
		t.Errorf("%s: the VMM is spawned before any SaveInstance in launchVMM. "+
			"A SIGKILL of hull in that window leaves a running VMM with no store record: "+
			"invisible to ps, unreachable by stop, and squatting its name against every "+
			"later run. Publish a \"starting\" record first and fill the pid in afterwards",
			fset.Position(spawn))
	} else {
		t.Logf("record at %s precedes the spawn at %s",
			fset.Position(firstSave), fset.Position(spawn))
	}
}
