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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A host boot asset must be copied before it is checked, and the copy is what
// boots.
//
// Verifying the original and then handing the VMM the same path string meant
// verifying one resolution of a name and executing another, with the whole of
// rootfs preparation in between -- for a block rootfs, an mke2fs over a
// multi-gigabyte tree.
func TestHostBootFileIsStagedNotPathed(t *testing.T) {
	src := t.TempDir()
	kernel := filepath.Join(src, "vmlinuz")
	if err := os.WriteFile(kernel, []byte("REAL KERNEL"), 0o644); err != nil {
		t.Fatal(err)
	}
	instanceDir := t.TempDir()

	staged, err := stageHostBootFile(kernel, instanceDir, stagedHostKernelName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(staged, src+string(os.PathSeparator)) {
		t.Errorf("the staged copy is still inside the source directory: %s", staged)
	}

	// Swap the original for an attacker kernel, exactly as the review did.
	if err := os.WriteFile(kernel, []byte("ATTACKER KERNEL"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "REAL KERNEL" {
		t.Errorf("the bytes the VMM would boot changed after staging: %q", got)
	}
}

// The staged copy is bounded, refuses a non-regular source, and will not follow
// a symlink at its destination.
func TestStageHostBootFileRefusals(t *testing.T) {
	t.Run("a fifo source does not block", func(t *testing.T) {
		src := t.TempDir()
		fifo := filepath.Join(src, "vmlinuz")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := stageHostBootFile(fifo, t.TempDir(), stagedHostKernelName)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Error("a FIFO was accepted as a boot asset")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("staging blocked on a FIFO")
		}
	})

	t.Run("oversized is refused and leaves nothing", func(t *testing.T) {
		src := t.TempDir()
		big := filepath.Join(src, "vmlinuz")
		f, err := os.Create(big)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxBootFileBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		instanceDir := t.TempDir()
		if _, err := stageHostBootFile(big, instanceDir, stagedHostKernelName); err == nil {
			t.Error("an oversized boot asset was staged")
		}
		if _, err := os.Lstat(filepath.Join(instanceDir, stagedHostKernelName)); !os.IsNotExist(err) {
			t.Errorf("a refused staging left a partial file: %v", err)
		}
	})

	t.Run("a symlinked destination is refused", func(t *testing.T) {
		src, elsewhere := t.TempDir(), t.TempDir()
		kernel := filepath.Join(src, "vmlinuz")
		if err := os.WriteFile(kernel, []byte("k"), 0o644); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(elsewhere, "victim")
		if err := os.WriteFile(victim, []byte("HOST FILE"), 0o600); err != nil {
			t.Fatal(err)
		}
		instanceDir := t.TempDir()
		if err := os.Symlink(victim, filepath.Join(instanceDir, stagedHostKernelName)); err != nil {
			t.Fatal(err)
		}
		if _, err := stageHostBootFile(kernel, instanceDir, stagedHostKernelName); err == nil {
			t.Error("staging followed a symlink at its destination")
		}
		got, _ := os.ReadFile(victim)
		if string(got) != "HOST FILE" {
			t.Errorf("the host file was overwritten through the link: %q", got)
		}
	})
}

// The block image holds the guest environment, which on an agent sandbox is
// where the forwarded credentials are. mke2fs created it under the umask and
// nothing chmod'ed it, so it came out world readable and `strings rootfs.ext4`
// was the whole attack.
func TestBlockImageIsNotWorldReadable(t *testing.T) {
	if _, err := os.Stat(mke2fsBin); err != nil {
		t.Skipf("mke2fs is not installed at %s", mke2fsBin)
	}
	rootfs := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootfs, "placeholder"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(t.TempDir(), "rootfs.ext4")

	secret := "SUPER-SECRET-API-KEY-42"
	injects := []blockInject{{
		guestPath: "/urunit.conf",
		content:   []byte("API_KEY=" + secret + "\n"),
		mode:      0o600,
	}}
	if err := buildBlockRootfs(disk, rootfs, injects, 16); err != nil {
		t.Fatalf("buildBlockRootfs: %v", err)
	}

	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the block image is %04o: any account on this machine can read the "+
			"forwarded credentials out of it", perm)
	}

	// Prove the secret really is in there, so the mode is doing real work
	// rather than guarding an empty file.
	blob, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), secret) {
		t.Error("the injected secret is not in the raw image, so this test is not " +
			"exercising what it was written for -- the mode assertion above is only " +
			"meaningful because the credential really is in these bytes")
	}
}

// The console log is the guest's own output. On an agent sandbox that includes
// whatever the agent echoes -- tokens it was given, contents of files it read.
//
// This reads the source rather than the filesystem, because opening the log
// happens inside launchVMM and reaching it needs a VMM. The first version of
// this test created its own file at 0600 and asserted it was 0600, which
// proved nothing about hull at all; that is the shape round 6 spent its length
// on and it is not going in on the same day.
func TestTheConsoleLogIsOpenedPrivate(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "OpenFile" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
				return true
			}
			// Only the call that opens the instance log.
			arg := types.ExprString(call.Args[0])
			if !strings.Contains(arg, "LogFile") {
				return true
			}
			found++
			mode := types.ExprString(call.Args[2])
			if mode != "0o600" {
				t.Errorf("%s: the instance log is opened %s; it holds the guest's own "+
					"output, which on an agent sandbox includes whatever the agent "+
					"echoed. The rest of the store is 0600",
					fset.Position(call.Pos()), mode)
			}
			return true
		})
	}
	if found == 0 {
		t.Error("no os.OpenFile of an instance LogFile found; this guard has stopped " +
			"guarding anything")
	}
}

// The boot branches must actually call the staging helper.
//
// Reverting `kernelPath = stageHostBootFile(...)` to `kernelPath = abs` passed
// every test above, because they exercise the helper and never assert that
// anything uses it. That is the exact shape round 6 found three times over --
// a fix that can be removed with a green suite -- so the call site gets its own
// assertion.
func TestHostBootAssetsGoThroughStaging(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	staged := map[string]bool{}
	verified := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || len(call.Args) == 0 {
				return true
			}
			switch id.Name {
			case "stageHostBootFile":
				if len(call.Args) == 3 {
					staged[types.ExprString(call.Args[2])] = true
				}
			case "verifyStagedBootAsset":
				if len(call.Args) == 3 {
					verified[types.ExprString(call.Args[2])] = true
				}
			}
			return true
		})
	}

	for _, want := range []string{"stagedHostKernelName", "stagedHostInitrdName"} {
		if !staged[want] {
			t.Errorf("nothing stages the host boot asset %s. A host kernel or initrd "+
				"handed to the VMM as a path is re-resolved when the VMM opens it, so "+
				"what was checked and what boots are two resolutions of one name", want)
		}
	}
	for _, want := range []string{"kernelPath", "initrdPath"} {
		if !verified[want] {
			t.Errorf("the staged copy assigned to %s is never verified against the "+
				"provenance record; staging without verifying just moves the bytes", want)
		}
	}
}
