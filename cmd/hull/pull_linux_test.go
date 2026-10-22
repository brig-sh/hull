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
	"bytes"
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/hull/internal/containerd"
	"github.com/brig-sh/hull/internal/containerd/containerdtest"
	"github.com/containerd/containerd/v2/core/images"
)

func TestDefaultPlatformIsTheHosts(t *testing.T) {
	if defaultPlatform != "linux/"+runtime.GOARCH {
		t.Errorf("defaultPlatform = %q, want linux/%s", defaultPlatform, runtime.GOARCH)
	}
}

// The arguments are checked before hull dials containerd, so these fail the
// same way with no daemon.
func TestPullCommandRefusesBadArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pull"}, "image reference required"},
		{[]string{"pull", "--platform", "arm64", "busybox"}, `invalid platform "arm64"`},
		{[]string{"pull", "--platform", "linux/", "busybox"}, `invalid platform "linux/"`},
	} {
		app := newApp()
		app.Writer = &bytes.Buffer{}
		err := app.Run(context.Background(), append([]string{"hull", "--containerd-address", "/nonexistent.sock"}, tc.args...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("hull %v: error %v, want one containing %q", tc.args, err, tc.want)
		}
	}
}

// hull pull through the command tree a user runs, against a live daemon:
// what it prints, and that hull images then lists the image under the name
// the pull stored it by.
func TestPullCommandAgainstDaemon(t *testing.T) {
	addr := containerdtest.DaemonAddress(t)
	ns := fmt.Sprintf("hull-test-cli-pull-%d", time.Now().UnixNano())
	const ref = "ghcr.io/containerd/busybox:1.36"
	c, err := containerd.Connect(context.Background(), addr, ns)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last in, first out, so the client is still open while
	// the image and namespace are removed.
	t.Cleanup(func() { _ = c.Close() })
	t.Cleanup(func() {
		ctx := context.Background()
		_ = c.ImageService().Delete(ctx, ref, images.SynchronousDelete())
		_ = c.NamespaceService().Delete(ctx, ns)
	})

	run := func(args ...string) (string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		app := newApp()
		app.Writer = &out
		app.ErrWriter = &errOut
		argv := append([]string{"hull", "--containerd-address", addr, "--namespace", ns}, args...)
		err := app.Run(context.Background(), argv)
		if err != nil {
			t.Fatalf("hull %v: %v", args, err)
		}
		return out.String(), errOut.String()
	}

	out, progress := run("pull", ref)
	want := regexp.MustCompile(`^Pulling ` + regexp.QuoteMeta(ref) + `\.\.\.\n` +
		`Successfully pulled image\nDigest: sha256:[0-9a-f]{64}\nSize: [1-9][0-9]* bytes\n$`)
	if !want.MatchString(out) {
		t.Errorf("pull printed:\n%s", out)
	}
	if !strings.HasSuffix(progress, " downloaded\n") {
		t.Errorf("pull progress = %q, want a final downloaded line", progress)
	}

	table, _ := run("images")
	if !strings.Contains(table, "ghcr.io/containerd/busybox  1.36") {
		t.Errorf("images does not list the pulled image:\n%s", table)
	}
}
