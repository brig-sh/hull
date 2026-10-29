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

import "github.com/urfave/cli/v3"

// stopTimeoutFlag is the --timeout of hull stop, whose meaning is the same
// on every OS.
func stopTimeoutFlag() cli.Flag {
	return &cli.IntFlag{
		Name:    "timeout",
		Aliases: []string{"t"},
		Value:   10,
		Usage:   "timeout in seconds before force kill",
	}
}
