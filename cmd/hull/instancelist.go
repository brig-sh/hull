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

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/brig-sh/hull/pkg/store"
)

// writeInstanceTable prints one row per instance, as `hull ps` shows them.
func writeInstanceTable(out io.Writer, instances []*store.InstanceState) error {
	if len(instances) == 0 {
		_, _ = fmt.Fprintln(out, "No instances found")
		return nil
	}

	// Create table output
	w := tabwriter.NewWriter(out, 4, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATUS\tEXIT\tPID\tIP\tCREATED")

	for _, state := range instances {
		// "-" is an honest rendering of "ended without a reportable
		// status"; a code only appears when one was actually observed.
		exitStr := "-"
		if state.ExitCode != nil {
			exitStr = strconv.Itoa(*state.ExitCode)
		}

		// An empty cell, not a "0". A record can legitimately carry no pid --
		// a stopped instance, or one still at StatusStarting whose run died
		// between spawning the VMM and recording the pid -- and printing 0
		// there would read as a real process number and invite someone (or
		// brig) to signal it. The status column already says which it is.
		pidStr := ""
		if state.PID > 0 {
			pidStr = fmt.Sprintf("%d", state.PID)
		}

		ip := state.IP
		if ip == "" {
			ip = "-"
		}
		// A zero StartTime is a row built because the record could not be
		// read. Year 1 is not a creation time; "-" is the same empty cell
		// the IP column already uses.
		created := "-"
		if !state.StartTime.IsZero() {
			created = state.StartTime.Format("2006-01-02 15:04:05")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			state.ID,
			state.Status,
			exitStr,
			pidStr,
			ip,
			created,
		)
	}

	return w.Flush()
}

// instanceReadError reports a GetInstance failure.
//
// Only ErrInstanceNotFound becomes "instance not found". Every other
// failure, including an unreadable state file, keeps its own text. Callers
// match that sentence when they decide a VM is gone, so a broken record
// must not wear it.
func instanceReadError(id string, err error) error {
	if errors.Is(err, store.ErrInstanceNotFound) {
		return fmt.Errorf("instance not found: %s", id)
	}
	return fmt.Errorf("instance %s: %w", id, err)
}
