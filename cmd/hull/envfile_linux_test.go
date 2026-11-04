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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadEnvFile(t *testing.T) {
	host := map[string]string{"TOKEN": "s3cret", "EMPTY_SET": ""}
	lookup := func(k string) (string, bool) {
		v, ok := host[k]
		return v, ok
	}
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{"KEY=VALUE as written", "FOO=bar\nURL=http://x/?a=b\n", []string{"FOO=bar", "URL=http://x/?a=b"}},
		{"quotes and trailing white space kept", "Q=\"a b\"\nT=x \n", []string{`Q="a b"`, "T=x "}},
		{"leading white space dropped", "  \tFOO=bar\n", []string{"FOO=bar"}},
		{"empty value kept", "EMPTY=\n", []string{"EMPTY="}},
		{"comments and blank lines skipped", "# a comment\n\n   \n  # indented\nFOO=bar # not a comment\n", []string{"FOO=bar # not a comment"}},
		{"bare key inherited", "TOKEN\nEMPTY_SET\n", []string{"TOKEN=s3cret", "EMPTY_SET="}},
		{"bare key unset dropped", "MISSING\n", []string{}},
		{"CRLF line ends", "FOO=bar\r\nBAZ=qux\r\n", []string{"FOO=bar", "BAZ=qux"}},
		{"UTF-8 BOM dropped", "\xef\xbb\xbfFOO=bar\n", []string{"FOO=bar"}},
		{"no final newline", "FOO=bar", []string{"FOO=bar"}},
	} {
		got, err := readEnvFile(writeEnvFile(t, tc.content), lookup)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadEnvFileRefusals(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		content string
		want    string
	}{
		{"FOO=bar\n=value\n", "line 2 has no variable name"},
		{"MY VAR=x\n", `line 1: variable "MY VAR" contains white space`},
		{"TOKEN \n", `line 1: variable "TOKEN " contains white space`},
		{"FOO=\xff\n", "line 1 is not UTF-8"},
		{"FOO=" + strings.Repeat("x", 70000) + "\n", "token too long"},
	} {
		path := writeEnvFile(t, tc.content)
		_, err := readEnvFile(path, lookup)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%.20q: error = %v, want %q with the path", tc.content, err, tc.want)
		}
	}
	_, err := readEnvFile(filepath.Join(t.TempDir(), "missing"), lookup)
	if err == nil || !strings.Contains(err.Error(), "--env-file: open") {
		t.Errorf("a missing file: error = %v, want the open error", err)
	}
}

// Files are read in order and --env comes last, and the last value given
// for a variable wins, where the first one was.
func TestRunEnv(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == "TOKEN" {
			return "s3cret", true
		}
		return "", false
	}
	first := writeEnvFile(t, "A=file1\nB=file1\nTOKEN\n")
	second := writeEnvFile(t, "B=file2\nC=file2\n")
	got, err := runEnv([]string{first, second}, []string{"C=flag", "D=flag", "D=flag2"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=file1", "B=file2", "TOKEN=s3cret", "C=flag", "D=flag2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	_, err = runEnv(nil, []string{"=x"}, lookup)
	if err == nil || !strings.Contains(err.Error(), "invalid --env") {
		t.Errorf("an invalid --env: error = %v, want it refused", err)
	}
}
