// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brig-sh/hull/pkg/store"
	"github.com/urfave/cli/v3"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
)

func TestParseLabelEntriesPreservesOperatorValues(t *testing.T) {
	got, err := parseLabelEntries([]string{
		"owner=old", "owner=operator", "empty=", "expression=a=b,c=d",
	})
	want := map[string]string{"owner": "operator", "empty": "", "expression": "a=b,c=d"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, %v; want %v", got, err, want)
	}
	got, err = parseLabelEntries(nil)
	if err != nil || got != nil {
		t.Fatalf("no labels = %v, %v; want nil, nil", got, err)
	}
}

// Label flags use the same comma-separated entry syntax as the existing
// annotation flags, but annotations must not become instance labels.
func TestRunLabelFlagsUseExistingCommaSeparatedSyntax(t *testing.T) {
	cmd := runCommand()
	cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
	var got map[string]string
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		var err error
		got, err = parseLabelEntries(c.StringSlice("label"))
		return err
	}
	err := cmd.Run(context.Background(), []string{
		"run", "--annotation", "image.only=not-instance-metadata",
		"--label", "owner=old", "--label", "owner=operator",
		"--label", "empty=", "--label", "first=one,second=two", "image",
	})
	want := map[string]string{"owner": "operator", "empty": "", "first": "one", "second": "two"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("run labels = %v, %v; want %v", got, err, want)
	}
}

// Invalid metadata must be rejected before the store is opened or an image is
// pulled. The store path cannot be opened: if validation moves after it, the
// caller gets an unrelated store error instead of the bad flag's diagnosis.
func TestMalformedRunLabelsAreRefusedBeforeOpeningTheStore(t *testing.T) {
	for _, entry := range []string{"missing-separator", "=missing-key", ""} {
		t.Run(entry, func(t *testing.T) {
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			app := &cli.Command{
				Name: "hull", Writer: io.Discard, ErrWriter: io.Discard,
				Flags:    []cli.Flag{&cli.StringFlag{Name: "store-dir"}},
				Commands: []*cli.Command{runCommand()},
			}
			err := app.Run(context.Background(), []string{
				"hull", "--store-dir", filepath.Join(blocker, "store"), "run",
				"--label", entry, "image.invalid/should-not-pull:latest",
			})
			if err == nil || !strings.Contains(err.Error(), "invalid --label") || !strings.Contains(err.Error(), "KEY=VALUE") {
				t.Fatalf("malformed label %q did not fail before store/image access: %v", entry, err)
			}
			data, readErr := os.ReadFile(blocker)
			if readErr != nil || string(data) != "untouched" {
				t.Fatalf("label validation changed the store parent: %q, %v", data, readErr)
			}
		})
	}
}

// launchVMM is shared by run and restore. The instance's identity and labels
// must survive its process changing; an old unlabeled instance must not gain
// metadata invented from a restore command or from its VMM arguments.
func TestLaunchAndStopPreserveInstanceLabelsAndCreationIdentity(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "labeled"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			s, state := labelLaunchFixture(t)
			wantID := "creation-kept-across-restore"
			wantLabels := map[string]string{"owner": "operator", "network": "isolated", "expression": "a=b,c=d"}
			if legacy {
				wantID, wantLabels = "", nil
			}
			state.CreationID, state.Labels = wantID, wantLabels
			for range 2 {
				started, err := launchVMM(&cli.Command{}, s, state, []string{"/bin/sh", "-c", "exit 0"},
					nil, hypervisors.VzVmm, true, "none", "")
				if err != nil || !started {
					t.Fatalf("launch = %v, %v", started, err)
				}
				// A real child exercises the save-after-spawn path without a VM.
				proc, err := os.FindProcess(state.PID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := proc.Wait(); err != nil {
					t.Fatal(err)
				}
				markStopped(s, state, true)
				state, err = s.GetInstance(state.ID)
				if err != nil {
					t.Fatal(err)
				}
				if state.Status != "stopped" || state.PID != 0 {
					t.Fatalf("stop did not persist: %+v", state)
				}
				if state.CreationID != wantID || !reflect.DeepEqual(state.Labels, wantLabels) {
					t.Fatalf("process lifecycle changed metadata: creationId=%q labels=%v", state.CreationID, state.Labels)
				}
				var output bytes.Buffer
				if err := printJSON(&output, state); err != nil {
					t.Fatal(err)
				}
				var inspected map[string]json.RawMessage
				if err := json.Unmarshal(output.Bytes(), &inspected); err != nil {
					t.Fatal(err)
				}
				_, hasID := inspected["creationId"]
				_, hasLabels := inspected["labels"]
				if hasID == legacy || hasLabels == legacy {
					t.Fatalf("inspect did not preserve metadata presence: %s", output.String())
				}
			}
		})
	}
}

func TestFailedLaunchRestoresPreviousInstanceLabelsAndCreationIdentity(t *testing.T) {
	s, state := labelLaunchFixture(t)
	state.Status = "stopped"
	state.CreationID = "previous-creation"
	state.Labels = map[string]string{"owner": "original", "network": "offline"}
	if err := s.SaveInstance(state); err != nil {
		t.Fatal(err)
	}
	candidate := *state
	candidate.CreationID = "replacement-creation"
	candidate.Labels = map[string]string{"owner": "replacement", "network": "shared"}
	started, err := launchVMM(&cli.Command{}, s, &candidate,
		[]string{filepath.Join(t.TempDir(), "no-such-vmm")}, nil, hypervisors.VzVmm, true, "none", "")
	if err == nil || started {
		t.Fatalf("missing VMM launch = %v, %v; want false, error", started, err)
	}
	after, err := s.GetInstance(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "stopped" || after.CreationID != state.CreationID || !reflect.DeepEqual(after.Labels, state.Labels) {
		t.Fatalf("failed spawn replaced prior metadata: %+v", after)
	}
}

func labelLaunchFixture(t *testing.T) (*store.Store, *store.InstanceState) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name = "label-test"
	if _, err := s.CreateInstance(name); err != nil {
		t.Fatal(err)
	}
	return s, &store.InstanceState{ID: name, LogFile: s.InstanceLogFile(name)}
}
