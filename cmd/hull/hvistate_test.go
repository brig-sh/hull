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

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/pkg/store"
)

func statusStub(t *testing.T, vmState string) *stubHvi {
	t.Helper()
	return newStubHvi(t, func(map[string]any) stubAnswer {
		return stubAnswer{reply: `{"v":1,"ok":true,"state":"` + vmState + `","vcpus":1,"vcpus_parked":1}`}
	})
}

// A paused or fenced hvi VM is a live process running nothing. ps shows it
// in a column of its own, and keeps STATUS "running" in the place brig's
// Running() and List() read it from.
func TestPsShowsAPausedOrFencedVM(t *testing.T) {
	stub := statusStub(t, "fenced")
	s := newCmdTestStore(t)
	fakeHviInstance(t, s, "vm1", stub.path)
	withMigration(t, s, "vm1", store.MigrationState{State: store.MigrationFenced, BundleDir: "/b", TxID: "t"})

	var out bytes.Buffer
	if err := renderInstances(s, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ps:\n%s", out.String())
	}
	head, row := strings.Fields(lines[0]), strings.Fields(lines[1])
	if head[0] != "ID" || head[1] != "STATUS" || row[0] != "vm1" || row[1] != "running" {
		t.Errorf("ID and STATUS moved: %q / %q", lines[0], lines[1])
	}
	if n := len(row); n < 2 || row[n-2] != "fenced" || row[n-1] != "fenced" {
		t.Errorf("row %q does not end with VM fenced, CHECKPOINT fenced", lines[1])
	}
}

func TestPsShowsDashesForAnInstanceWithoutASocket(t *testing.T) {
	s := newCmdTestStore(t)
	st, _ := fakeHviInstance(t, s, "vm1", "")
	st.ControlSock = ""
	_ = s.SaveInstance(st)
	var out bytes.Buffer
	if err := renderInstances(s, &out); err != nil {
		t.Fatal(err)
	}
	row := strings.Fields(strings.Split(strings.TrimSpace(out.String()), "\n")[1])
	if n := len(row); row[n-2] != "-" || row[n-1] != "-" {
		t.Errorf("row %v, want - for VM and CHECKPOINT", row)
	}
}

// exec into a paused hvi guest used to hang, deaf to SIGTERM, until it was
// killed. It must refuse at once and name the way out.
func TestExecRefusesAHaltedGuest(t *testing.T) {
	for vmState, want := range map[string]string{
		"paused":          "hull resume vm1",
		"restored-paused": "hull resume vm1",
		"fenced":          "hull restore vm1",
		"running":         "",
	} {
		t.Run(vmState, func(t *testing.T) {
			stub := statusStub(t, vmState)
			s := newCmdTestStore(t)
			st, _ := fakeHviInstance(t, s, "vm1", stub.path)
			start := time.Now()
			err := refuseIfGuestHalted(st)
			if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
				t.Errorf("err = %v, want one naming %q", err, want)
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("took %s", took)
			}
		})
	}
	// No socket answering: let through, as before the socket existed.
	s := newCmdTestStore(t)
	st, _ := fakeHviInstance(t, s, "vm1", filepath.Join(shortSocketDir(t), "gone.sock"))
	if err := refuseIfGuestHalted(st); err != nil {
		t.Errorf("an hvi with no socket was refused: %v", err)
	}
}

func TestInspectCarriesTheVMState(t *testing.T) {
	stub := statusStub(t, "paused")
	s := newCmdTestStore(t)
	st, _ := fakeHviInstance(t, s, "vm1", stub.path)
	var out bytes.Buffer
	if err := printJSON(&out, inspectView(st)); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["vmState"] != "paused" || doc["status"] != "running" || doc["controlSock"] != stub.path {
		t.Errorf("inspect = %s", out.String())
	}
}
