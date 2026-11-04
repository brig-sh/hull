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
	"reflect"
	"testing"

	"github.com/urfave/cli/v3"
)

// A comma in a value of --env or --annotation stays in the value, as in
// docker run, instead of splitting it in two.
func TestRunAndExecKeepCommasInValues(t *testing.T) {
	for _, tc := range []struct {
		cmd  *cli.Command
		argv []string
		flag string
		want []string
	}{
		{runCommand(), []string{"run", "-e", "NO_PROXY=a,b", "--env", "C=1", "img"}, "env", []string{"NO_PROXY=a,b", "C=1"}},
		{runCommand(), []string{"run", "--annotation", "k=v1,v2", "img"}, "annotation", []string{"k=v1,v2"}},
		{execCommand(), []string{"exec", "-e", "NO_PROXY=a,b", "vm", "true"}, "env", []string{"NO_PROXY=a,b"}},
	} {
		var got []string
		tc.cmd.Action = func(_ context.Context, cmd *cli.Command) error {
			got = cmd.StringSlice(tc.flag)
			return nil
		}
		err := tc.cmd.Run(context.Background(), tc.argv)
		if err != nil {
			t.Errorf("%v: %v", tc.argv, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: --%s = %q, want %q", tc.argv, tc.flag, got, tc.want)
		}
	}
}
