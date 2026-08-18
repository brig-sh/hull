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
	"strings"
	"testing"
)

// Guest-supplied text must be sanitised at the point it enters a message.
//
// Round 5 sanitised three sites; round 6 found that reverting ANY of them left
// the suite green, because the tests exercised sanitizeGuestText itself and
// never asserted that a caller used it. It also found five more sites that had
// never been sanitised at all. This guard is the answer to both: it names the
// expressions that carry guest bytes and fails when one appears in a message
// without going through the filter.
//
// It walks every file in the package. The single-file version of this idea was
// walked past by adding one file, which is documented at the top of
// TestBootFilesAreStagedRatherThanPathed and was not propagated to its
// neighbours until round 6 pointed that out.
func TestGuestTextIsSanitisedAtEveryCallSite(t *testing.T) {
	// Expressions whose value is chosen by the guest.
	guestExprs := map[string]string{
		"ae.Message":             "the guest agent's own error message",
		"strings.TrimSpace(out)": "captured guest stdout/stderr",
		"out":                    "captured guest stdout/stderr",
		"line":                   "a line of guest console output",
		"string(data)":           "the guest console log",
	}
	// Calls that render into something a person reads.
	renderers := map[string]bool{
		"Errorf": true, "Printf": true, "Sprintf": true,
		"Fprintf": true, "Println": true, "Fatalf": true,
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
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
			fn := ""
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				fn = f.Sel.Name
			case *ast.Ident:
				fn = f.Name
			}
			if !renderers[fn] {
				return true
			}
			for _, arg := range call.Args {
				expr := types.ExprString(arg)
				why, guest := guestExprs[expr]
				if !guest {
					continue
				}
				checked++
				t.Errorf("%s: %s (%s) is rendered without sanitizeGuestText. "+
					"It reaches the operator's terminal through fatal() or a bare "+
					"Printf, where OSC 52 reads and writes the clipboard and CSI 21t "+
					"types its reply into the shell's input",
					fset.Position(arg.Pos()), expr, why)
			}
			return true
		})
	}
	t.Logf("scanned the package for unsanitised guest expressions (%d offending)", checked)
}

// And the filter itself must still be reachable from the places that hold the
// guest's bytes, so a future refactor cannot quietly orphan it.
func TestSanitizersAreUsed(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	uses := map[string]int{}
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
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					uses[id.Name]++
				}
			}
			return true
		})
	}
	for _, fn := range []string{"sanitizeGuestText", "sanitizeGuestError"} {
		if uses[fn] == 0 {
			t.Errorf("%s is defined and never called; guest output is reaching the "+
				"terminal unfiltered somewhere", fn)
		}
	}
	if uses["guestTerminalWriter"] == 0 {
		t.Error("guestTerminalWriter is never used; the streaming filter is orphaned")
	}
}
