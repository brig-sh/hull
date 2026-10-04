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

import "os"

// HostOS returns the distribution and its version from os-release (eg.
// "ubuntu 24.04") for the event envelope. It returns "" when neither file
// the specification names can be read.
func HostOS() string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		if data, err := os.ReadFile(path); err == nil {
			return parseOSRelease(string(data))
		}
	}
	return ""
}
