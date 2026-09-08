// Copyright (c) 2026, NOFire AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// TestShortFlagsParse drives each command through the parser with the short
// spelling its docs advertise. urfave/cli v3 does not split a flag Name on
// commas, so a v1-style "detach,d" declaration silently drops the alias and
// the parser answers "flag provided but not defined: -d".
func TestShortFlagsParse(t *testing.T) {
	cases := []struct {
		build func() *cli.Command
		args  []string
		long  string
		check func(*cli.Command) bool
	}{
		{runCommand, []string{"run", "-d", "img"}, "detach", func(c *cli.Command) bool { return c.Bool("detach") }},
		{restoreCommand, []string{"restore", "-d", "inst"}, "detach", func(c *cli.Command) bool { return c.Bool("detach") }},
		{logsCommand, []string{"logs", "-f", "inst"}, "follow", func(c *cli.Command) bool { return c.Bool("follow") }},
		{logsCommand, []string{"logs", "-n", "7", "inst"}, "tail", func(c *cli.Command) bool { return c.Int("tail") == 7 }},
		{rmCommand, []string{"rm", "-f", "inst"}, "force", func(c *cli.Command) bool { return c.Bool("force") }},
		{stopCommand, []string{"stop", "-t", "3", "inst"}, "timeout", func(c *cli.Command) bool { return c.Int("timeout") == 3 }},
	}
	for _, tc := range cases {
		name := strings.Join(tc.args, " ")
		t.Run(name, func(t *testing.T) {
			cmd := tc.build()
			var seen *cli.Command
			cmd.Action = func(_ context.Context, c *cli.Command) error {
				seen = c
				return nil
			}
			if err := cmd.Run(context.Background(), tc.args); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if seen == nil {
				t.Fatalf("%s: action did not run", name)
			}
			if !tc.check(seen) {
				t.Fatalf("%s: --%s not set by its short flag", name, tc.long)
			}
		})
	}
}

// TestNoCommaFlagNames guards the whole command tree against the v1 spelling
// coming back: a Name with a comma is never what the author meant under v3.
func TestNoCommaFlagNames(t *testing.T) {
	var walk func(prefix string, c *cli.Command)
	walk = func(prefix string, c *cli.Command) {
		for _, f := range c.Flags {
			for _, n := range f.Names() {
				if strings.Contains(n, ",") {
					t.Errorf("%s: flag name %q contains a comma; use Aliases", prefix, n)
				}
			}
		}
		for _, sub := range c.Commands {
			walk(prefix+" "+sub.Name, sub)
		}
	}
	for _, c := range []*cli.Command{
		pullCommand(), runCommand(), execCommand(), psCommand(), stopCommand(),
		checkpointCommand(), restoreCommand(), rmCommand(), logsCommand(),
		inspectCommand(), imagesCommand(), assetsCommand(), storeCommand(),
		composeCommand(), networkGatewayCommand(), telemetryCommand(),
	} {
		walk("hull "+c.Name, c)
	}
}
