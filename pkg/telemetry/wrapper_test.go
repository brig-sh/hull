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
	"bytes"
	"os"
	"strings"
	"testing"
)

// The variable is easy to read as an opt-out. A value that is not a list of
// event names suppresses everything, as "1" does, rather than turning the
// prompt off and the sending on.
func TestSuppressValueThatIsNoListSuppressesAll(t *testing.T) {
	for _, v := range []string{"1", " 1", "1,", "true", "yes", "0", "command,1", "command,bogus", "command,"} {
		t.Run(v, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(EnvSuppress, v)
			var prompt bytes.Buffer
			c := Init(Config{StoreDir: dir, Interactive: true, Stdin: strings.NewReader("\n"), Stderr: &prompt})
			if c.Enabled() || prompt.Len() != 0 {
				t.Fatalf("SUPPRESS=%q: enabled = %v, prompted = %q; want neither", v, c.Enabled(), prompt.String())
			}
			if _, err := os.Stat(statePath(dir)); !os.IsNotExist(err) {
				t.Fatalf("SUPPRESS=%q touched the state", v)
			}
		})
	}
}

// A wrapper passes a list only once its user has answered yes. Without an
// answer on file, a list must not fall back to hull's on-by-default.
func TestSuppressListKeepsAnUnansweredInstallOff(t *testing.T) {
	t.Setenv(EnvSuppress, "command")
	if c := Init(Config{StoreDir: t.TempDir()}); c.Enabled() {
		t.Fatal("a suppress list sent from an install nobody has asked")
	}
}

// Old brig names its product and not its version. runtime_version tells its
// events apart from the ones a newer brig sends itself.
func TestWrapperProductAloneReportsRuntimeVersion(t *testing.T) {
	t.Setenv(EnvProduct, "brig")
	c := Init(Config{StoreDir: t.TempDir(), Version: "0.1.0-rc31"})
	p := payloadOf(t, c, "start")
	if p["version"] != "0.1.0-rc31" || p["runtime_version"] != "0.1.0-rc31" {
		t.Fatalf("version = %v, runtime_version = %v; want hull's in both", p["version"], p["runtime_version"])
	}
}

func TestWrapperVersionIsCleaned(t *testing.T) {
	t.Setenv(EnvProduct, "brig")
	for _, tc := range []struct{ in, want string }{
		{"0.4.0", "0.4.0"},
		{"0.4.0-rc1+dirty", "0.4.0-rc1+dirty"},
		{"0.4.0 (feature/secret-branch)", "0.1.0-rc31"},
		{strings.Repeat("9", maxVersion), strings.Repeat("9", maxVersion)},
		{strings.Repeat("9", maxVersion+1), "0.1.0-rc31"},
		{"!!!", "0.1.0-rc31"},
	} {
		t.Setenv(EnvVersion, tc.in)
		c := Init(Config{StoreDir: t.TempDir(), Version: "0.1.0-rc31"})
		if got := payloadOf(t, c, "start")["version"]; got != tc.want {
			t.Errorf("HULL_TELEMETRY_VERSION=%q: version = %v, want %q", tc.in, got, tc.want)
		}
	}
}
