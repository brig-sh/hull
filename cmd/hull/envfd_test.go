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
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// parseEnvRecords reads the guest environment brig hands hull over a
// descriptor. Unlike --env, a value here is never looked up in hull's own
// environment and never rides argv, so a guest value for a name hull or the
// dynamic loader reads for itself cannot steer the host. The records are
// NUL-separated so a value may hold any byte, including the newlines of a PEM
// key or JSON credential.
func TestParseEnvRecordsReadsNulSeparatedPairs(t *testing.T) {
	data := []byte("FOO=bar\x00BAZ=qux")
	got, err := parseEnvRecords(data)
	if err != nil {
		t.Fatalf("parseEnvRecords: %v", err)
	}
	want := []string{"FOO=bar", "BAZ=qux"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEnvRecordsKeepsEveryByteInAValue(t *testing.T) {
	// A multiline secret must survive verbatim: this is the reason the channel
	// is NUL-separated rather than line-oriented.
	pem := "-----BEGIN KEY-----\nline2=with=equals\n-----END KEY-----\n"
	data := []byte("TOKEN=" + pem + "\x00EMPTY=")
	got, err := parseEnvRecords(data)
	if err != nil {
		t.Fatalf("parseEnvRecords: %v", err)
	}
	want := []string{"TOKEN=" + pem, "EMPTY="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEnvRecordsIgnoresATrailingSeparator(t *testing.T) {
	// A writer that terminates every record, rather than separating them,
	// leaves a trailing NUL. That is not an empty record.
	data := []byte("FOO=bar\x00")
	got, err := parseEnvRecords(data)
	if err != nil {
		t.Fatalf("parseEnvRecords: %v", err)
	}
	if want := []string{"FOO=bar"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseEnvRecordsOnEmptyInput(t *testing.T) {
	got, err := parseEnvRecords(nil)
	if err != nil {
		t.Fatalf("parseEnvRecords: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestParseEnvRecordsRejectsARecordWithoutEquals(t *testing.T) {
	_, err := parseEnvRecords([]byte("FOO=bar\x00NOEQUALS"))
	if err == nil {
		t.Fatal("want an error for a record without '='")
	}
	if !strings.Contains(err.Error(), "NOEQUALS") {
		t.Fatalf("error should name the bad record, got %v", err)
	}
}

func TestParseEnvRecordsRejectsAnEmptyName(t *testing.T) {
	_, err := parseEnvRecords([]byte("=value"))
	if err == nil {
		t.Fatal("want an error for an empty name")
	}
}

// resolveEnvFds reads from a real descriptor the caller holds open. A temp
// file standing in for brig's pipe keeps the test free of a live hull.
func TestResolveEnvFdsReadsFromADescriptor(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "envfd")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("FOO=bar\x00SECRET=multi\nline\x00"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got, err := resolveEnvFds([]string{strconv.Itoa(int(f.Fd()))})
	if err != nil {
		t.Fatalf("resolveEnvFds: %v", err)
	}
	want := []string{"FOO=bar", "SECRET=multi\nline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveEnvFdsRejectsAStandardStream(t *testing.T) {
	for _, fd := range []string{"0", "1", "2", "notanumber"} {
		if _, err := resolveEnvFds([]string{fd}); err == nil {
			t.Errorf("resolveEnvFds(%q): want an error", fd)
		}
	}
}

func TestResolveEnvFdsRejectsADescriptorNotHeld(t *testing.T) {
	// A high number the process does not hold: dup fails with EBADF here
	// rather than surfacing as an unreadable file later.
	if _, err := resolveEnvFds([]string{"2047"}); err == nil {
		t.Error("want an error for a descriptor the process does not hold")
	}
}

func TestResolveEnvFdsEmptyWhenNoneGiven(t *testing.T) {
	got, err := resolveEnvFds(nil)
	if err != nil {
		t.Fatalf("resolveEnvFds: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}
