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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/brig-sh/hull/internal/buildinfo"
	"github.com/urfave/cli/v3"
)

// The version flag answers with printVersion rather than urfave's own, which
// would write "hull version <v>" and read oddly once the version carries a
// parenthesised build. Installed here rather than in main so the two
// spellings cannot come apart by one of them being forgotten.
func init() { cli.VersionPrinter = printVersion }

// printVersion writes the one line both spellings answer with.
func printVersion(*cli.Command) { fmt.Printf("hull %s\n", build) }

// versionCommand is `hull version`: one line naming the build, or the same
// build as JSON.
//
// `hull --version` prints the same line. The subcommand exists because it
// can take --json; a global flag cannot.
func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print the version and the build behind this binary",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "machine-readable output"},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.Bool("json") {
				return printJSON(os.Stdout, versionData(build))
			}
			printVersion(nil)
			return nil
		},
	}
}

// versionPayload is the JSON form: the same fields the line prints, with the
// commit in full. commit and commitTime are absent, not empty, when the build
// carried no VCS data, so a consumer tests for the key rather than
// recognising a zero time.
type versionPayload struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	Modified   bool   `json:"modified"`
	GoVersion  string `json:"goVersion"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

func versionData(info buildinfo.Info) versionPayload {
	p := versionPayload{
		Version:   info.Version,
		Commit:    info.Commit,
		Modified:  info.Modified,
		GoVersion: info.GoVersion,
		OS:        info.OS,
		Arch:      info.Arch,
	}
	if !info.CommitTime.IsZero() {
		p.CommitTime = info.CommitTime.UTC().Format(time.RFC3339)
	}
	return p
}
