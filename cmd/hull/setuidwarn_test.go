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
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// captureWarnings collects everything logged while fn runs.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := logrus.StandardLogger().Out
	prevLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.DebugLevel)
	t.Cleanup(func() {
		logrus.SetOutput(prev)
		logrus.SetLevel(prevLevel)
	})
	fn()
	return buf.String()
}

// writeRootfs lays out a rootfs containing rel at the given mode.
func writeRootfs(t *testing.T, rel string, mode os.FileMode) string {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte("binary"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Set the mode explicitly: WriteFile's is masked by umask, and the setuid
	// bit needs os.ModeSetuid rather than an octal 04000, which os.FileMode
	// does not carry and silently drops. (That is the same trap the unpack
	// walks around by calling syscall.Chmod with a raw POSIX mode.)
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if mode&os.ModeSetuid != 0 && fi.Mode()&os.ModeSetuid == 0 {
		t.Skipf("this filesystem will not hold a setuid bit on %s", p)
	}
	return root
}

// The reported symptom: sudo ships setuid, the vz backend shares the root
// through Apple's virtio-fs, and sudo silently never elevates. Saying nothing
// is the bug -- the guest looks fine and the user is left to work out why
// `ls -la /usr/bin` and `ls -la /usr/bin/sudo` disagree about who owns sudo.
func TestSetuidOnAppleVirtiofsIsReported(t *testing.T) {
	root := writeRootfs(t, "usr/bin/sudo", 0o755|os.ModeSetuid)

	out := captureWarnings(t, func() { warnSetuidUnsupported(root) })

	if !strings.Contains(out, "usr/bin/sudo") {
		t.Errorf("the warning should name the binary that will not work:\n%s", out)
	}
	// It has to be actionable: both configurations that do work are named.
	if !strings.Contains(out, "--rootfs-type block") {
		t.Errorf("the warning should offer a block rootfs:\n%s", out)
	}
	if !strings.Contains(out, "hvi") {
		t.Errorf("the warning should offer the hvi backend:\n%s", out)
	}
}

// Every other setuid binary that matters is covered, not just sudo.
func TestSetuidWarningCoversTheUsualBinaries(t *testing.T) {
	for _, rel := range setuidProbePaths {
		t.Run(rel, func(t *testing.T) {
			root := writeRootfs(t, rel, 0o755|os.ModeSetuid)
			out := captureWarnings(t, func() { warnSetuidUnsupported(root) })
			if !strings.Contains(out, rel) {
				t.Errorf("setuid %s went unreported:\n%s", rel, out)
			}
		})
	}
}

// An image with nothing setuid loses nothing to a share that cannot express
// one, so it must not be warned at. A warning on every run is a warning nobody
// reads.
func TestNoSetuidNoWarning(t *testing.T) {
	root := writeRootfs(t, "usr/bin/sudo", 0o755)

	out := captureWarnings(t, func() { warnSetuidUnsupported(root) })

	if strings.Contains(out, "setuid") {
		t.Errorf("a rootfs with no setuid binary should be quiet:\n%s", out)
	}
}

// An empty or missing rootfs is not a crash and not a warning.
func TestSetuidWarningToleratesAMissingRootfs(t *testing.T) {
	out := captureWarnings(t, func() {
		warnSetuidUnsupported(filepath.Join(t.TempDir(), "does-not-exist"))
	})
	if strings.Contains(out, "setuid") {
		t.Errorf("a missing rootfs should be quiet:\n%s", out)
	}
}

// Testing the function is not testing that it runs. This has bitten this
// codebase before: a check was written, tested, and never wired to its call
// site, and every test passed. Assert run.go actually calls it.
func TestSetuidWarningIsWiredIntoRun(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "warnSetuidUnsupported" {
			calls++
		}
		return true
	})
	if calls == 0 {
		t.Error("run.go never calls warnSetuidUnsupported: the check exists but " +
			"nothing on the vz virtiofs path reaches it")
	}
}

// The qemu shared-folder e2e broke the moment images started being unpacked
// with their setuid bits intact. QEMU's 9pfs runs security_model=none, which
// reports the host's ownership to the guest as well as the mode, so a restored
// setuid bit on /usr/bin/mount means "become uid 501" -- the guest's init
// dropped out of root on its first mount and spent the rest of the boot
// answering "must be superuser to use mount", then restart-looped.
//
// So on that one path the bits must stay off. Everywhere else they must not:
// hvi reads ownership from the record beside the file, and a block rootfs
// carries it in ext4, and both need setuid for sudo to work at all.
func TestSetIDBitsAreDroppedOnlyFor9pfs(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
		want os.FileMode
	}{
		{"9pfs drops it", dropSetID, 0},
		{"other shares keep it", keepSetID, os.ModeSetuid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone := t.TempDir()
			p := filepath.Join(clone, "mount")
			if err := os.WriteFile(p, []byte("binary"), 0755); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			// What the image shipped, recorded beside the file the way unpack
			// does, with the copy having already cleared the bit on disk.
			recordAttr(t, p, 0o4755, 0, 0)

			if err := restoreClonedModes(clone, tc.drop); err != nil {
				t.Fatalf("restoreClonedModes: %v", err)
			}

			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatalf("Lstat: %v", err)
			}
			if got := fi.Mode() & os.ModeSetuid; got != tc.want {
				t.Errorf("setuid bit = %v, want %v (mode %v)", got, tc.want, fi.Mode())
			}
			if fi.Mode().Perm() != 0o755 {
				t.Errorf("permission bits = %04o, want 0755", fi.Mode().Perm())
			}
		})
	}
}
