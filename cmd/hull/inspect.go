// Copyright (c) 2023-2026, Nubificus LTD
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
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urfave/cli/v3"
)

func inspectCommand() *cli.Command {
	return &cli.Command{
		Name:  "inspect",
		Usage: "inspect instance details",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return inspectInstance(ctx, cmd)
		},
	}
}

func inspectInstance(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() == 0 {
		return errors.New("instance ID required")
	}

	instanceID := args.First()

	s, err := globalStore(cmd)
	if err != nil {
		return err
	}

	state, err := s.GetInstance(instanceID)
	if err != nil {
		return fmt.Errorf("instance not found: %s", instanceID)
	}

	// Output as JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal instance state: %w", err)
	}

	// json.Marshal escapes control bytes below 0x20 but leaves U+0080-U+009F
	// alone, and those are the 8-bit spellings of CSI, OSC and DCS. An
	// instance name carrying one reaches the terminal through this line.
	fmt.Println(sanitizeGuestText(string(data)))
	return nil
}
