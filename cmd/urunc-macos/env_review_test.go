// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// --- review finding: the resolved value lands on disk world-readable ---------
//
// `--env KEY` keeps a secret out of argv, but on the `run` path the resolved
// value flows into BuildUrunitConfig and is written as urunit.conf. Those
// writes use 0644, so any local user can read the token out of
// <store>/instances/<id>/bundle/rootfs/urunit.conf for the lifetime of the
// instance. The store root is created 0700, but MkdirAll does not tighten a
// directory that already exists, and --store-dir can point somewhere shared.
//
// Only urunc-macos writes this file and only the guest reads it (over
// virtiofs), so 0600 costs nothing. This is a guard test rather than a
// behavioural one: it pins the mode at every call site, including any added
// later, without needing to boot a VM.
func TestUrunitConfIsNotGroupOrWorldReadable(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}

	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || sel.Sel.Name != "WriteFile" || len(call.Args) != 3 {
			return true
		}
		// Only the urunit.conf writes carry guest env; other writes in this
		// file are not in scope for this finding.
		target, ok := call.Args[0].(*ast.Ident)
		if !ok || target.Name != "confPath" {
			return true
		}
		found++

		lit, ok := call.Args[2].(*ast.BasicLit)
		if !ok {
			t.Errorf("%s: urunit.conf mode is not a literal; cannot verify it is 0600",
				fset.Position(call.Pos()))
			return true
		}
		mode, perr := strconv.ParseUint(lit.Value, 0, 32)
		if perr != nil {
			t.Errorf("%s: cannot parse mode %q: %v", fset.Position(call.Pos()), lit.Value, perr)
			return true
		}
		if mode&0o077 != 0 {
			t.Errorf("%s: urunit.conf written with mode %#o, which is readable by "+
				"group/other; it carries the resolved --env values, so it must be 0600",
				fset.Position(call.Pos()), mode)
		}
		return true
	})

	if found == 0 {
		t.Fatal("found no os.WriteFile(confPath, ...) calls in run.go; " +
			"this guard test needs updating to match the current write sites")
	}
	t.Logf("checked %d urunit.conf write site(s)", found)
}

// --- review finding: -t clobbered an explicit --env TERM ---------------------
//
// With -t, exec appends the host TERM so the guest gets a sane terminal. That
// append used to be unconditional and landed after the caller's own entries,
// and the guest hands the list to exec.Cmd.Env, whose dedup keeps the last
// duplicate -- so `--env TERM=dumb` was silently overridden by the host value.
//
// appendEnvDefault has its own table test; this guards the call site, which is
// where the defect actually lived and which that test cannot reach.
func TestExecAppliesTermAsADefaultNotAnOverride(t *testing.T) {
	src, err := os.ReadFile("exec.go")
	if err != nil {
		t.Fatalf("read exec.go: %v", err)
	}
	body := string(src)

	if strings.Contains(body, `append(env, "TERM="`) {
		t.Error(`exec.go appends TERM directly; use appendEnvDefault so an explicit ` +
			`--env TERM is not overridden (exec.Cmd.Env dedup keeps the last duplicate)`)
	}
	if !strings.Contains(body, `appendEnvDefault(env, "TERM"`) {
		t.Error(`exec.go should set the host TERM via appendEnvDefault(env, "TERM", ...)`)
	}
}
