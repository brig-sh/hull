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
	"context"
	"errors"

	"github.com/urfave/cli/v3"
)

// errVzCheckpointMacOnly answers checkpoint and restore for anything but an
// hvi instance. On macOS those go to vz-runner, whose saved state is
// Virtualization.framework's and bound to the Mac that wrote it; on Linux
// every instance is hvi, so this is only reached by a record that is not.
var errVzCheckpointMacOnly = errors.New("checkpoint and restore on Linux take hvi instances only")

func vzCheckpointInstance(context.Context, *cli.Command) error { return errVzCheckpointMacOnly }

func vzRestoreInstance(context.Context, *cli.Command) error { return errVzCheckpointMacOnly }
