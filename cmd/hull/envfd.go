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
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The --env-fd channel exists so a guest variable never travels through hull's
// own process environment. --env with a bare KEY resolves the value from
// hull's environment (resolveEnvEntries, os.LookupEnv), which means the caller
// had to export it there first; any name hull, its children, or the dynamic
// loader reads for itself -- DOCKER_CONFIG, LD_PRELOAD, HULL_* -- then takes
// the caller's value as its own configuration, on the host, before the guest
// exists. --env-fd closes that: the records are read from a descriptor the
// caller already holds open, handed to the guest verbatim, and never consulted
// as hull's environment and never placed on argv.
//
// The records are NUL-separated, not newline-separated, so a value may hold
// any byte: the newlines of a PEM private key or a JSON credential survive.

// parseEnvRecords splits the NUL-separated KEY=VALUE records of the --env-fd
// channel. A trailing NUL terminates the final record rather than starting an
// empty one. Each record must carry a non-empty name and an '='; a value may
// be empty and may contain any byte but NUL.
func parseEnvRecords(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	records := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(records))
	for i, rec := range records {
		// A writer that terminates each record leaves a trailing NUL, so the
		// last split element is empty. That is not a record.
		if len(rec) == 0 && i == len(records)-1 {
			continue
		}
		name, _, ok := strings.Cut(string(rec), "=")
		if !ok {
			return nil, fmt.Errorf("invalid --env-fd record %q: expected KEY=VALUE", rec)
		}
		if name == "" {
			return nil, fmt.Errorf("invalid --env-fd record %q: empty name", rec)
		}
		out = append(out, string(rec))
	}
	return out, nil
}

// resolveEnvFds reads the guest environment records from each descriptor named
// by --env-fd and returns them as KEY=VALUE strings. It never reads hull's own
// environment: that is the whole point of the channel.
//
// Each descriptor is validated and duplicated the way --shared-dir-fd is: a
// number of 3 or above (0, 1 and 2 are the standard streams), and dup rather
// than os.NewFile so a number the process does not hold fails here with EBADF
// instead of surfacing later, and so hull never closes a descriptor it did not
// own.
func resolveEnvFds(fds []string) ([]string, error) {
	var out []string
	for _, spec := range fds {
		fd, err := strconv.Atoi(spec)
		if err != nil || fd < 3 {
			return nil, fmt.Errorf("invalid --env-fd %q: not a descriptor number of 3 or above", spec)
		}
		dup, err := unix.Dup(fd)
		if err != nil {
			return nil, fmt.Errorf("--env-fd %q: %d is not a descriptor this process holds "+
				"(the caller must clear FD_CLOEXEC on it): %w", spec, fd, err)
		}
		unix.CloseOnExec(dup)
		f := os.NewFile(uintptr(dup), fmt.Sprintf("env-fd %d", fd))
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("--env-fd %q: %w", spec, err)
		}
		records, err := parseEnvRecords(data)
		if err != nil {
			return nil, err
		}
		out = append(out, records...)
	}
	return out, nil
}
