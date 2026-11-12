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
	"path/filepath"
	"slices"
)

// withGUIRecord returns the replayed runner command line with its recording
// set to file. A recording the instance was started with is dropped, since
// restore would otherwise overwrite that movie. An empty file only drops it.
func withGUIRecord(cmdArgs []string, file string) ([]string, error) {
	out := make([]string, 0, len(cmdArgs)+2)
	for i := 0; i < len(cmdArgs); i++ {
		if cmdArgs[i] == "--gui-record" {
			i++
			continue
		}
		out = append(out, cmdArgs[i])
	}
	if file == "" {
		return out, nil
	}
	if !slices.Contains(out, "--gui") {
		return nil, errors.New("--gui-record needs an instance that was started with --gui")
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	return append(out, "--gui-record", abs), nil
}
