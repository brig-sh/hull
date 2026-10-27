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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// Pull policies, matching docker's vocabulary.
const (
	pullMissing = "missing" // use the cached image if there is one (default)
	pullAlways  = "always"  // re-resolve the reference and pull it again
	pullNever   = "never"   // never reach the network; fail if not cached
)

func validPullPolicy(p string) bool {
	return p == pullMissing || p == pullAlways || p == pullNever
}

// generateID generates a random instance ID
func generateID(length int) string {
	b := make([]byte, length/2)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID
		return fmt.Sprintf("%x", time.Now().UnixNano())[:length]
	}
	return hex.EncodeToString(b)[:length]
}

func parseAnnotationEntries(entries []string) (map[string]string, error) {
	annotations := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --annotation %q, expected KEY=VALUE", entry)
		}
		annotations[key] = value
	}
	return annotations, nil
}

// The run flags whose meaning is the same on every OS. Each is its own
// function so a command lists them in whatever order it lists the rest.

func pullFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "pull",
		Value: pullMissing,
		Usage: "pull policy: 'missing' (default), 'always' or 'never'. " +
			"A cached tag is not re-resolved, so use 'always' to pick up a republished tag",
	}
}

func memFlag() cli.Flag {
	return &cli.IntFlag{
		Name:  "mem",
		Value: 512,
		Usage: "memory size in MB",
	}
}

func cpusFlag() cli.Flag {
	return &cli.IntFlag{
		Name:  "cpus",
		Value: 1,
		Usage: "number of CPUs",
	}
}

func nameFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "name",
		Usage: "instance name (default: auto-generated)",
	}
}

func annotationFlag() cli.Flag {
	return &cli.StringSliceFlag{
		Name:  "annotation",
		Usage: "set an OCI runtime annotation: KEY=VALUE (repeatable)",
	}
}

func envFlag() cli.Flag {
	return &cli.StringSliceFlag{
		Name:    "env",
		Aliases: []string{"e"},
		Usage:   "set an environment variable in the guest: KEY=VALUE, or a bare KEY to inherit it from the host environment and keep the value out of argv (repeatable)",
	}
}
