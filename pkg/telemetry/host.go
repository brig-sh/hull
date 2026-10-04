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

package telemetry

import (
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Platform returns the OS family an event comes from: "macos", "linux", or
// GOOS for anything else.
func Platform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// HostUname renders the kernel's name, release and machine for the event
// envelope (eg. "Darwin 25.3.0 arm64"). It returns "" on failure.
//
// The hostname (Nodename) is never read, and neither is the version field:
// it is the kernel's build string, with a build date and on Linux whatever
// the person who built it put there, which can single out one machine.
func HostUname() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	b := func(f []byte) string { return unix.ByteSliceToString(f) }
	return strings.Join([]string{b(u.Sysname[:]), kernelRelease(b(u.Release[:])), b(u.Machine[:])}, " ")
}

// releasePrefix is the part of a kernel release that is a version: 6.8.0, and
// the distribution's ABI number after it, as in 6.8.0-45.
var releasePrefix = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,2}(-[0-9]+)?`)

// kernelRelease returns the version at the start of a kernel release, or ""
// when it does not start with one. Whoever builds a Linux kernel sets the
// rest, such as a CONFIG_LOCALVERSION that names a person or a machine.
func kernelRelease(release string) string {
	return releasePrefix.FindString(release)
}

// osReleaseField is the longest value kept from one os-release field.
const osReleaseField = 32

// parseOSRelease returns "ID VERSION_ID" from an os-release file, or just the
// ID for a rolling distribution with no VERSION_ID. It returns "" when the
// file names no ID.
//
// Only these two fields are read. Both are short lowercase identifiers by the
// os-release specification. A character outside that alphabet is dropped, and
// so is anything past 32 characters.
func parseOSRelease(content string) string {
	var id, version string
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = osReleaseValue(value)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	if id == "" {
		return ""
	}
	if version == "" {
		return id
	}
	return id + " " + version
}

// osReleaseValue unquotes one os-release value and returns it lowercased, or
// "" when it holds a character an identifier may not hold or is longer than
// osReleaseField. A value that fails is dropped whole, so no part of a
// hand-written one is sent.
func osReleaseValue(v string) string {
	v = strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
	if len(v) > osReleaseField {
		return ""
	}
	for _, r := range v {
		if !osReleaseRune(r) {
			return ""
		}
	}
	return v
}

// osReleaseRune reports whether r may appear in an os-release identifier.
func osReleaseRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}
