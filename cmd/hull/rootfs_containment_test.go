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

// --- review finding: the pull escape ----------------------------------------
//
// pkg/ociclient/unpack.go preserves absolute symlinks out of image layers on
// purpose: real images are full of them ("usr/bin/awk -> /etc/alternatives/awk")
// and its own os.Root refuses to traverse one, so the extractor itself cannot
// leave the rootfs. The entry is therefore sitting in the unpacked image by
// design -- and hull's own host-side writers here in cmd/hull then walked
// straight through it, because they used filepath.Join and plain os.WriteFile,
// os.MkdirAll and os.OpenFile.
//
// An image shipping "urunit.conf" as a symlink to a file in the caller's home
// got that file truncated and rewritten with content the image chose;
// "etc/hosts" or "etc" as a symlink did the same to /etc/hosts; and the
// bootKernel/binary/kernel/initrd annotations -- which come from
// bundle/rootfs/urunc.json, i.e. image content, with no allowlist -- were
// joined onto the bundle path and handed to the VMM, so a fixed, over-long
// "../../../.." prefix copied an arbitrary host file into guest RAM regardless
// of how deep the bundle happens to be.
//
// Every test below plants what a malicious image would plant, and checks two
// things: that hull refuses, and that the victim file outside the rootfs is
// byte-for-byte and mode-for-mode what it was. Every victim lives under the
// test's own t.TempDir().

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// victim is a file outside the rootfs that an image is trying to reach. Its
// content and mode are recorded when it is planted so that any write through a
// symlink shows up as a difference, whether the write changed the bytes, the
// length or the permissions.
type victim struct {
	path    string
	content []byte
	mode    os.FileMode
}

func plantVictim(t *testing.T, dir, name string, content string) *victim {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &victim{path: path, content: []byte(content), mode: info.Mode()}
}

func (v *victim) mustBeUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(v.path)
	if err != nil {
		t.Fatalf("host file outside the rootfs is gone: %v", err)
	}
	if !bytes.Equal(got, v.content) {
		t.Errorf("HOST FILE OUTSIDE THE ROOTFS WAS OVERWRITTEN\n  was:  %q\n  now:  %q\n  path: %s",
			v.content, got, v.path)
	}
	info, err := os.Lstat(v.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != v.mode {
		t.Errorf("host file mode changed: was %v, now %v (%s)", v.mode, info.Mode(), v.path)
	}
}

// mustRefuse asserts that err is a containment refusal: a caller has to be able
// to tell "the image planted this" from "the disk is full", and the message has
// to name the entry so the person reading it knows which one.
// It reports rather than fatals, so that the victim check the caller runs next
// still gets to say whether the host file was actually written through.
func mustRefuse(t *testing.T, err error, entry string) {
	t.Helper()
	if err == nil {
		t.Errorf("no error: hull followed the planted %q out of the rootfs", entry)
		return
	}
	if !errors.Is(err, errImagePlantedPath) {
		t.Errorf("error is not a containment refusal (errors.Is(err, errImagePlantedPath) is false): %v", err)
	}
	if !strings.Contains(err.Error(), entry) {
		t.Errorf("refusal does not name the planted entry %q: %v", entry, err)
	}
}

// newRootfs makes an unpacked-image directory plus a place for host files that
// are none of the image's business, both inside the test's own temp dir.
func newRootfs(t *testing.T) (base, rootfs string) {
	t.Helper()
	base = t.TempDir()
	rootfs = filepath.Join(base, "bundle", "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	return base, rootfs
}

// --- sinks (1) and (2): urunit.conf, and the init wrappers beside it ---------

// The two urunit.conf writes (Vz and QEMU) and the two init wrappers all write
// a hull-chosen name into a directory the image controls, so all four are the
// same exposure. The content of urunit.conf is substantially the image's own:
// BuildUrunitConfig embeds ociSpec.Process.Env.
func TestRootfsWriteRefusesImagePlantedLinks(t *testing.T) {
	for name, tc := range map[string]struct {
		entry string
		// target is the symlink the image ships, resolved against base.
		target func(base string) string
	}{
		"urunit.conf as an absolute symlink": {
			entry:  "urunit.conf",
			target: func(base string) string { return filepath.Join(base, "home", "zshrc") },
		},
		"urunit.conf as a relative escaping symlink": {
			entry:  "urunit.conf",
			target: func(string) string { return "../../home/zshrc" },
		},
		"vz init wrapper as an absolute symlink": {
			entry:  ".vz-init",
			target: func(base string) string { return filepath.Join(base, "home", "zshrc") },
		},
		"qemu init wrapper as a relative escaping symlink": {
			entry:  ".qemu-init",
			target: func(string) string { return "../../home/zshrc" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			base, rootfs := newRootfs(t)
			v := plantVictim(t, filepath.Join(base, "home"), "zshrc", "export PATH=/usr/bin\n")
			if err := os.Symlink(tc.target(base), filepath.Join(rootfs, tc.entry)); err != nil {
				t.Fatal(err)
			}

			err := writeIntoRootfs(rootfs, tc.entry, []byte("URUNIT_ENV=attacker-controlled\n"), 0o600)
			mustRefuse(t, err, tc.entry)
			v.mustBeUntouched(t)
		})
	}
}

// The positive control for the same writer: with nothing planted, hull's files
// land in the rootfs with the mode it asked for.
func TestRootfsWriteStillWritesABenignImage(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := writeIntoRootfs(rootfs, "urunit.conf", []byte("URUNIT_ENV=PATH=/usr/bin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "urunit.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "URUNIT_ENV=PATH=/usr/bin\n" {
		t.Errorf("urunit.conf = %q", got)
	}
	info, err := os.Stat(filepath.Join(rootfs, "urunit.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("urunit.conf mode = %v, want 0600", info.Mode().Perm())
	}
}

// Real images do ship symlinks, including ones hull writes through. A link that
// stays inside the rootfs must keep working -- containment is about leaving the
// rootfs, not about symlinks.
func TestRootfsWriteFollowsAContainedSymlink(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("etc/urunit.conf", filepath.Join(rootfs, "urunit.conf")); err != nil {
		t.Fatal(err)
	}
	if err := writeIntoRootfs(rootfs, "urunit.conf", []byte("contained\n"), 0o600); err != nil {
		t.Fatalf("a symlink that stays inside the rootfs must still be written through: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "etc", "urunit.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "contained\n" {
		t.Errorf("link target = %q", got)
	}
}

// --- sink (3): injectHosts ---------------------------------------------------

// "etc" as a symlink is the cheaper attack: the image does not have to guess
// the file name, only the directory, and MkdirAll/ReadFile/OpenFile all follow
// it. "etc/hosts" as a symlink aims at one specific host file.
func TestInjectHostsRefusesImagePlantedLinks(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, base, rootfs string) *victim{
		"etc as an absolute symlinked directory": func(t *testing.T, base, rootfs string) *victim {
			v := plantVictim(t, filepath.Join(base, "hostetc"), "hosts", "127.0.0.1\tlocalhost\n")
			if err := os.Symlink(filepath.Join(base, "hostetc"), filepath.Join(rootfs, "etc")); err != nil {
				t.Fatal(err)
			}
			return v
		},
		"etc as a relative escaping symlinked directory": func(t *testing.T, base, rootfs string) *victim {
			v := plantVictim(t, filepath.Join(base, "hostetc"), "hosts", "127.0.0.1\tlocalhost\n")
			if err := os.Symlink("../../hostetc", filepath.Join(rootfs, "etc")); err != nil {
				t.Fatal(err)
			}
			return v
		},
		"etc/hosts as an absolute symlink": func(t *testing.T, base, rootfs string) *victim {
			v := plantVictim(t, filepath.Join(base, "home"), "zshrc", "export PATH=/usr/bin\n")
			if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(v.path, filepath.Join(rootfs, "etc", "hosts")); err != nil {
				t.Fatal(err)
			}
			return v
		},
		"etc/hosts as a relative escaping symlink": func(t *testing.T, base, rootfs string) *victim {
			v := plantVictim(t, filepath.Join(base, "home"), "zshrc", "export PATH=/usr/bin\n")
			if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../../home/zshrc", filepath.Join(rootfs, "etc", "hosts")); err != nil {
				t.Fatal(err)
			}
			return v
		},
	} {
		t.Run(name, func(t *testing.T) {
			base, rootfs := newRootfs(t)
			v := plant(t, base, rootfs)

			err := injectHosts(rootfs, []string{"web:10.87.0.10"})
			mustRefuse(t, err, "etc")
			v.mustBeUntouched(t)
		})
	}
}

// The read side of the same file. Nothing is written here, but the bytes go
// into the guest's /etc/hosts, so following a planted link hands the guest
// whichever host file the image named.
func TestHostsReadersRefuseAPlantedEtcHosts(t *testing.T) {
	const secret = "ssh-ed25519 AAAAsecretkeymaterial\n"
	for name, read := range map[string]func(rootfs string) ([]byte, error){
		"hostsWithEntries (block rootfs)": func(rootfs string) ([]byte, error) {
			return hostsWithEntries(rootfs, []string{"web:10.87.0.10"})
		},
		"containerHosts (generic container boot)": func(rootfs string) ([]byte, error) {
			s, err := containerHosts(rootfs, []string{"web:10.87.0.10"})
			return []byte(s), err
		},
	} {
		t.Run(name, func(t *testing.T) {
			base, rootfs := newRootfs(t)
			v := plantVictim(t, filepath.Join(base, "home"), "id_ed25519", secret)
			if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(v.path, filepath.Join(rootfs, "etc", "hosts")); err != nil {
				t.Fatal(err)
			}

			got, err := read(rootfs)
			if err == nil {
				t.Errorf("no error: hull read a host file as the image's /etc/hosts")
			}
			if bytes.Contains(got, []byte(secret)) {
				t.Errorf("HOST FILE CONTENTS LEAKED INTO THE GUEST'S /etc/hosts: %q", got)
			}
			v.mustBeUntouched(t)
		})
	}
}

// Positive control: a benign image, with a real /etc/hosts, is appended to
// exactly as before -- image content first, added entries after it, and the
// missing trailing newline supplied.
func TestInjectHostsStillEditsABenignImage(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "hosts"), []byte("127.0.0.1\tlocalhost"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := injectHosts(rootfs, []string{"web:10.87.0.10"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "127.0.0.1\tlocalhost\n10.87.0.10\tweb\n" {
		t.Errorf("hosts = %q", got)
	}
}

// Positive control for the shape real images actually use: "etc" is a symlink,
// but a relative one that stays inside the rootfs. It must still be written
// through.
func TestInjectHostsFollowsAContainedEtcSymlink(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "usr", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/etc", filepath.Join(rootfs, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := injectHosts(rootfs, []string{"web:10.87.0.10"}); err != nil {
		t.Fatalf("a contained etc symlink must still be written through: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "usr", "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "10.87.0.10\tweb\n" {
		t.Errorf("usr/etc/hosts = %q", got)
	}
}

// --- sink (4): image-nominated boot paths ------------------------------------

// The bootKernel/binary/kernel/initrd annotations come from
// bundle/rootfs/urunc.json, which is image content merged into the spec with no
// allowlist. filepath.Join cleans, so the traversal case needs no knowledge of
// where the bundle lives: a fixed over-long prefix escapes any depth. The
// initrd in particular is copied wholesale into guest RAM by
// VZLinuxBootLoader.initialRamdiskURL.
func TestImageNominatedBootPathsRefuseToLeaveTheRootfs(t *testing.T) {
	for name, annotation := range map[string]string{
		"relative traversal, depth independent": "../../../../../../../../secrets/id_ed25519",
		"traversal behind a plausible prefix":   "boot/../../../../../../../../secrets/id_ed25519",
		"absolute host path":                    "SECRETS/id_ed25519", // rewritten below
	} {
		t.Run(name, func(t *testing.T) {
			base, rootfs := newRootfs(t)
			v := plantVictim(t, filepath.Join(base, "secrets"), "id_ed25519",
				"ssh-ed25519 AAAAsecretkeymaterial\n")
			// A real "boot" directory, so the traversal cases are refused for
			// leaving the rootfs rather than dismissed for naming a directory
			// that was never there.
			if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
				t.Fatal(err)
			}
			if annotation == "SECRETS/id_ed25519" {
				annotation = v.path
			}

			got, err := imageFileHostPath(rootfs, annotation)
			switch {
			case err == nil:
				t.Errorf("no error: annotation %q resolved to %q, a file hull was never allowed to boot",
					annotation, got)
			case got == v.path:
				t.Errorf("annotation %q resolved straight onto the host file %q", annotation, got)
			case strings.HasPrefix(annotation, "..") || strings.Contains(annotation, "/../"):
				// A traversal is refused by name. An absolute path is not:
				// bunny writes "/unikernel/app.elf" and the leading slash has
				// always meant the image's own root, so it is read as
				// image-relative and simply is not there.
				mustRefuse(t, err, annotation)
			}
			v.mustBeUntouched(t)
		})
	}
}

// The other half of sink (4): the annotation names an ordinary file, but the
// image shipped that file as a symlink pointing out of the rootfs. Nothing in
// the name gives it away.
func TestImageNominatedBootPathsRefuseAPlantedSymlink(t *testing.T) {
	base, rootfs := newRootfs(t)
	v := plantVictim(t, filepath.Join(base, "secrets"), "id_ed25519", "ssh-ed25519 AAAAsecret\n")
	if err := os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v.path, filepath.Join(rootfs, "boot", "initrd")); err != nil {
		t.Fatal(err)
	}

	got, err := imageFileHostPath(rootfs, "/boot/initrd")
	if err == nil {
		t.Errorf("no error: a planted symlink resolved to %q, which would be copied into guest RAM", got)
	}
	mustRefuse(t, err, "/boot/initrd")
	v.mustBeUntouched(t)
}

// Positive control: the paths real images use -- bunny's leading slash and a
// plain relative name -- still resolve, and to the file inside the image.
func TestImageNominatedBootPathsResolveOrdinaryImages(t *testing.T) {
	_, rootfs := newRootfs(t)
	if err := os.MkdirAll(filepath.Join(rootfs, "unikernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(rootfs, "unikernel", "app.elf")
	if err := os.WriteFile(kernel, []byte("ELF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, annotation := range []string{"/unikernel/app.elf", "unikernel/app.elf"} {
		got, err := imageFileHostPath(rootfs, annotation)
		if err != nil {
			t.Fatalf("%q: %v", annotation, err)
		}
		if got != kernel {
			t.Errorf("%q resolved to %q, want %q", annotation, got, kernel)
		}
	}
	// A directory is not something to boot, and neither is a name that is not
	// there; both are refused, but as ordinary failures rather than as an
	// image planting something.
	for _, annotation := range []string{"unikernel", "unikernel/missing.elf"} {
		if _, err := imageFileHostPath(rootfs, annotation); err == nil {
			t.Errorf("%q should not resolve to a bootable file", annotation)
		} else if errors.Is(err, errImagePlantedPath) {
			t.Errorf("%q is an ordinary failure, not a containment refusal: %v", annotation, err)
		}
	}
}

// --- the call sites ----------------------------------------------------------
//
// The tests above hold the helpers. This one holds the call sites, which is
// where the defect actually lived and which no unit test can reach: runCommand
// needs a bundle, a store and a hypervisor. It is a source guard in the style
// of env_review_test.go -- it fails on the code as it was, because that code
// joined image-relative names onto the rootfs by hand and handed the result to
// the os package.
func TestRootfsPathsAreNotJoinedByHand(t *testing.T) {
	// The os functions that act on a path they resolve themselves, and so
	// follow whatever symlink the image left in the way. Stat and Lstat are
	// left out on purpose: hull calls them only on paths an os.Root has
	// already resolved.
	unsafeOps := map[string]map[string]bool{
		"os": {
			"WriteFile": true, "OpenFile": true, "Create": true,
			"MkdirAll": true, "Mkdir": true, "ReadFile": true,
			"Remove": true, "RemoveAll": true, "Open": true,
		},
		// syscall and x/sys/unix are the same defect with the libc names on it.
		// blockrootfs.go reached the host through syscall.Chmod, not through the
		// os package, so a guard that only knew about os would have watched that
		// escape go past: the mode restore joined an image-controlled name onto
		// the rootfs and chmod followed the symlink the image had put there.
		// Every call listed here resolves its own path and follows symlinks.
		"syscall": {
			"Chmod": true, "Chown": true, "Lchown": true, "Chtimes": true,
			"Open": true, "Unlink": true, "Rmdir": true, "Mkdir": true,
			"Link": true, "Symlink": true, "Rename": true, "Truncate": true,
			"Utimes": true, "Removexattr": true, "Setxattr": true, "Getxattr": true,
		},
		"unix": {
			"Chmod": true, "Chown": true, "Lchown": true,
			"Setxattr": true, "Getxattr": true, "Removexattr": true,
			"Open": true, "Unlink": true, "Link": true, "Symlink": true,
			"Rename": true, "Truncate": true, "Utimes": true,
		},
	}

	// The names that hold an unpacked image rootfs across cmd/hull. The guard
	// works on source text, so it has to be told what a rootfs is called: srcDir
	// and clone are what blockrootfs.go's mke2fs source and cp clone are named,
	// and missing one means the guard silently covers nothing in that function.
	rootfsNames := map[string]bool{
		"rootfsDir": true, "rootfs": true, "rootfsLink": true,
		"srcDir": true, "clone": true, "staging": true, "bundleRootfs": true,
	}

	for _, file := range []string{"run.go", "netdiscover.go", "blockrootfs.go"} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		render := func(node ast.Node) string {
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, node); err != nil {
				return ""
			}
			return buf.String()
		}

		// Names that hold a path built by joining onto the unpacked image, and
		// names built from those in turn.
		rooted := map[string]bool{}
		var isRooted func(expr ast.Expr) bool
		isRooted = func(expr ast.Expr) bool {
			switch e := expr.(type) {
			case *ast.CallExpr:
				if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" && len(e.Args) > 0 {
					if id, ok := e.Args[0].(*ast.Ident); ok && rootfsNames[id.Name] {
						return true
					}
				}
				// rootfsEntry is blockrootfs.go's one sanctioned join, and it is
				// sanctioned only for ociclient.GuestAttr, which needs a path
				// because macOS has no descriptor-based getxattr. Its result is
				// still a host path built from an image-controlled name, so
				// handing it to the os package is the same defect wearing a
				// helper's name.
				if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "rootfsEntry" {
					return true
				}
			}
			switch e := expr.(type) {
			case *ast.Ident:
				return rooted[e.Name]
			case *ast.CallExpr:
				// filepath.Join(<something rooted>, ...) is rooted too, which
				// is how etcDir became hostsPath became the append target.
				if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" && len(e.Args) > 0 {
					return isRooted(e.Args[0])
				}
			}
			return false
		}
		// filepath.Walk and filepath.WalkDir hand their callback a path built by
		// joining onto the tree being walked, so inside that callback the path
		// parameter is every bit as image-controlled as an explicit
		// filepath.Join -- which is how the mode restore in blockrootfs.go
		// reached the host: it walked the rootfs with filepath.WalkDir and gave
		// the callback's path straight to syscall.Chmod. Without this rule the
		// guard reads that function as clean.
		markWalkParamRooted := func(call *ast.CallExpr) {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) != 2 {
				return
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "filepath" ||
				(sel.Sel.Name != "Walk" && sel.Sel.Name != "WalkDir") {
				return
			}
			if !isRooted(call.Args[0]) {
				if id, ok := call.Args[0].(*ast.Ident); !ok || !rootfsNames[id.Name] {
					return
				}
			}
			lit, ok := call.Args[1].(*ast.FuncLit)
			if !ok || lit.Type.Params == nil || len(lit.Type.Params.List) == 0 {
				return
			}
			for _, name := range lit.Type.Params.List[0].Names {
				if name.Name != "_" {
					rooted[name.Name] = true
				}
			}
		}

		for range 3 { // enough passes for chains of assignments to settle
			ast.Inspect(parsed, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					markWalkParamRooted(call)
					return true
				}
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}
				id, ok := assign.Lhs[0].(*ast.Ident)
				if ok && isRooted(assign.Rhs[0]) {
					rooted[id.Name] = true
				}
				return true
			})
		}

		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}

			// (a) an os, syscall or unix call on a path joined onto the image
			// rootfs.
			if unsafeOps[pkg.Name][sel.Sel.Name] && isRooted(call.Args[0]) {
				t.Errorf("%s: %s.%s(%s) resolves an image-controlled path itself; "+
					"an image that ships that entry as a symlink to a host file gets that "+
					"host file read, written or chmodded. Go through an os.Root -- "+
					"writeIntoRootfs/readFromRootfs, or root.Chmod over a root.FS() walk "+
					"as restoreClonedModes does",
					fset.Position(call.Pos()), pkg.Name, sel.Sel.Name, render(call.Args[0]))
			}

			// (b) an image-supplied name joined onto the bundle's rootfs.
			// filepath.Join(bundleDir, "rootfs") is the rootfs itself and is
			// what the containment helpers are given; anything further is a
			// name from the image, and filepath.Join cleans ".." away rather
			// than refusing it.
			if pkg.Name == "filepath" && sel.Sel.Name == "Join" && len(call.Args) > 2 &&
				render(call.Args[0]) == "bundleDir" && render(call.Args[1]) == `"rootfs"` {
				t.Errorf("%s: %s joins a name onto the bundle rootfs; "+
					"resolve it with imageFileHostPath so a \"../..\" prefix or a planted "+
					"symlink cannot name a host file", fset.Position(call.Pos()), render(call))
			}
			return true
		})
	}
}

// The call sites, in the style of TestRootfsPathsAreNotJoinedByHand: no unit
// test can reach the kernel/initrd resolution inside runContainer, so the
// guard is on the source.
//
// imageFileHostPath still exists and is still correct for what it does, but it
// returns a name, and a name is re-resolved by whoever opens it next. It is
// therefore only safe where the result cannot be staged. Two such uses remain,
// both on hull's own literals, and both are listed here with the reason,
// because "it was already like that" is not one.
func TestBootFilesAreStagedRatherThanPathed(t *testing.T) {
	sanctioned := map[string]string{
		// Deliberately NOT staged, and the residual risk is accepted rather
		// than missed. urunc's darwin QEMU backend derives a sibling from this
		// path -- filepath.Dir(args.UnikernelPath) + "/disk.img" -- and boots
		// that disk directly under EDK2 when it is there. Moving the unikernel
		// into the instance directory moves that lookup with it, so the disk
		// image silently stops being found and the VM boots something else.
		// The path also has to survive the miss case, where hull deliberately
		// keeps a name for a file that is not in the image at all so the
		// monitor fails on it as it always has, and there is nothing to copy.
		// The exposure is narrow: the name is hull's own literal with no
		// separators, so the only thing an image can swap is that one entry in
		// its own rootfs. Closing it properly means teaching the pinned urunc
		// fork to take the disk image as its own argument.
		`"unikernel"`: "urunc's darwin QEMU backend resolves disk.img relative to it",
		// Never opened by hull, and never handed to a VMM: the guest opens
		// /.rosetta/busybox for itself, inside its own rootfs, long after this.
		// All hull wants here is whether the exec bit is set.
		`".rosetta/busybox"`: "stat'ed for its exec bit, never opened or booted by hull",
	}

	// Every file in the package, not just run.go: the function is package
	// scoped, so a new file could call it and this guard would never look.
	// Adding cmd/hull/unsafe_probe.go with a single call was enough to walk
	// straight past the version of this test that parsed one file.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no package files parsed; this guard has stopped guarding anything")
	}

	calls := 0
	inspect := func(n ast.Node) bool {
		// A reference to the function that is not a call -- `resolve :=
		// imageFileHostPath`, or passing it as an argument -- hands the same
		// capability to a name this guard does not follow. There is no reason
		// to take its address, so refuse it outright rather than trying to
		// track where it ends up.
		if id, ok := n.(*ast.Ident); ok && id.Name == "imageFileHostPath" {
			if _, isCall := identCallParents[id]; !isCall {
				t.Errorf("%s: imageFileHostPath is used as a value; that moves the "+
					"resolution behind a name this guard cannot follow. Call it directly, "+
					"or use stageImageBootFile", fset.Position(id.Pos()))
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "imageFileHostPath" || len(call.Args) != 2 {
			return true
		}
		calls++
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: imageFileHostPath is given a name from the image; "+
				"the path it returns is re-resolved when the VMM opens it, so anything "+
				"booted from it can be swapped after the check. Use stageImageBootFile, "+
				"which copies off the descriptor that was validated",
				fset.Position(call.Pos()))
			return true
		}
		if why, ok := sanctioned[lit.Value]; !ok {
			t.Errorf("%s: imageFileHostPath(%s) is a new path-returning use; "+
				"if the result is opened to be booted it must go through "+
				"stageImageBootFile instead, and if it is not, add it here with the reason",
				fset.Position(call.Pos()), lit.Value)
		} else {
			t.Logf("%s: sanctioned -- %s", fset.Position(call.Pos()), why)
		}
		return true
	}

	for _, file := range files {
		identCallParents = map[*ast.Ident]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					identCallParents[id] = true
				}
			}
			// The declaration's own name is not a use of it.
			if fn, ok := n.(*ast.FuncDecl); ok && fn.Name != nil {
				identCallParents[fn.Name] = true
			}
			return true
		})
		ast.Inspect(file, inspect)
	}

	if calls == 0 {
		t.Error("no imageFileHostPath calls found in the package; this guard has stopped guarding anything")
	}
}

// identCallParents marks the Ident nodes that are the callee of a call, so a
// bare reference to the same name can be told apart from an ordinary call.
var identCallParents = map[*ast.Ident]bool{}

// The refusal has to be usable by a caller, not just readable by a person: it
// carries a sentinel, so "the image planted this" is distinguishable from "the
// disk is full", and it still wraps the underlying error so a missing file is
// still a missing file.
func TestContainmentRefusalIsDistinguishableFromAnIOError(t *testing.T) {
	_, rootfs := newRootfs(t)
	if _, err := readFromRootfs(rootfs, "etc/hosts"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file must stay a missing file: %v", err)
	} else if errors.Is(err, errImagePlantedPath) {
		t.Errorf("a missing file must not read as a containment refusal: %v", err)
	}

	base, rootfs := newRootfs(t)
	v := plantVictim(t, filepath.Join(base, "home"), "zshrc", "export PATH=/usr/bin\n")
	if err := os.Symlink(v.path, filepath.Join(rootfs, "planted")); err != nil {
		t.Fatal(err)
	}
	err := writeIntoRootfs(rootfs, "planted", []byte("x"), 0o600)
	if !errors.Is(err, errImagePlantedPath) {
		t.Fatalf("planted path did not carry the sentinel: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "planted") {
		t.Errorf("refusal does not say the image planted the entry: %v", msg)
	}
	t.Logf("refusal reads: %v", err)
}
