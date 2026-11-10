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

package containerd

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

// uruncKeysFile is internal/urunckeys/keys.json.
type uruncKeysFile struct {
	UruncKeys []string `json:"uruncKeys"`
	HostKeys  []string `json:"hostKeys"`
}

// internal/urunckeys checks uruncKeys and hostKeys against the urunc the
// bundle ships, in a module of its own, and reads them from its keys.json.
// That file must hold the lists as they are here.
func TestUruncKeysFile(t *testing.T) {
	const path = "../urunckeys/keys.json"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got uruncKeysFile
	err = json.Unmarshal(data, &got)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	want := uruncKeysFile{slices.Sorted(slices.Values(uruncKeys)), slices.Sorted(maps.Keys(hostKeys))}
	if !slices.Equal(got.UruncKeys, want.UruncKeys) || !slices.Equal(got.HostKeys, want.HostKeys) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("%s does not hold uruncKeys and hostKeys, sorted. It should read:\n%s", path, wantJSON)
	}
}
