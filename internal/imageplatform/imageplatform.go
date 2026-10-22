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
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Parse turns an "os/arch" or "os/arch/variant" string into a platform.
//
// An empty part is refused. Left in, "linux/" would match no image of a
// multi-platform index and fail only at the registry. A single-platform
// image has no index to match against, so it would be pulled whatever its
// architecture, and recorded as the platform "linux/".
func Parse(s string) (ocispec.Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || slices.Contains(parts, "") {
		return ocispec.Platform{}, fmt.Errorf("invalid platform %q, expected os/arch (e.g. linux/amd64)", s)
	}
	p := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}
