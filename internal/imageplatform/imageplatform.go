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

// Package imageplatform parses the image platform a user names with
// --platform. It is apart from the pull code on each OS, which uses a
// different registry client on each, so both accept the same strings.
package imageplatform

import (
	"fmt"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Parse turns an "os/arch" or "os/arch/variant" string into a platform.
func Parse(s string) (ocispec.Platform, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return ocispec.Platform{OS: parts[0], Architecture: parts[1]}, nil
	case 3:
		return ocispec.Platform{OS: parts[0], Architecture: parts[1], Variant: parts[2]}, nil
	}
	return ocispec.Platform{}, fmt.Errorf("invalid platform %q, expected os/arch (e.g. linux/amd64)", s)
}
