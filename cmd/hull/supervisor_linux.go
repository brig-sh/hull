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

//go:build linux

package main

import (
	"context"
	"errors"
	"time"

	"github.com/urfave/cli/v3"
)

// supervisorPollInterval is the default of network-gateway's
// --supervise-interval, which only --project uses.
const supervisorPollInterval = 2 * time.Second

// supervisor stands in for the restart supervision of a compose project,
// which network-gateway runs with --project. Compose is macOS only, so on
// Linux newProjectSupervisor refuses the flag and no supervisor exists.
type supervisor struct {
	interval time.Duration
}

func newProjectSupervisor(_ *cli.Command, _ string, _ time.Duration) (*supervisor, error) {
	return nil, errors.New("compose is macOS only, so there is no project to supervise on Linux")
}

func (s *supervisor) run(context.Context) {}

func (s *supervisor) killInFlight() {}
