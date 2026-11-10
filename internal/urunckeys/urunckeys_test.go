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

package urunckeys

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

// specOnly names the key of each field of urunc's UnikernelConfig that urunc
// reads from the spec alone. Such a field is tagged json:"-", which names no
// key, since urunc keeps it out of an image's urunc.json. It may name fields
// of a urunc newer than the bundle's, such as those of the keys in ahead.
var specOnly = map[string]string{
	"VAccel":     "com.urunc.unikernel.vAccel",
	"RPCAddress": "com.urunc.unikernel.RPCAddress",
	"BootKernel": "com.urunc.unikernel.bootKernel",
	"BootInitrd": "com.urunc.unikernel.bootInitrd",
	"RootfsMode": "com.urunc.unikernel.rootfsMode",
	"NetSocket":  "com.urunc.unikernel.netSocket",
	"NetAddress": "com.urunc.unikernel.netAddress",
	"NetGateway": "com.urunc.unikernel.netGateway",
}

// ahead lists the keys hull writes that this urunc does not read yet, for a
// urunc newer than the bundle's. Each must be a host key: a urunc that
// reads it takes it from the spec alone.
var ahead = map[string]bool{
	// The user-mode network gateway hvi joins, which
	// urunc-dev/urunc@034929e reads and the bundle's urunc does not.
	"com.urunc.unikernel.netSocket":  true,
	"com.urunc.unikernel.netAddress": true,
	"com.urunc.unikernel.netGateway": true,
}

// hullKeys is keys.json: uruncKeys and hostKeys of
// internal/containerd/urunc.go.
type hullKeys struct {
	UruncKeys []string `json:"uruncKeys"`
	HostKeys  []string `json:"hostKeys"`
}

func readHullKeys(t *testing.T) (written, host map[string]bool) {
	t.Helper()
	data, err := os.ReadFile("keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var keys hullKeys
	err = json.Unmarshal(data, &keys)
	if err != nil {
		t.Fatalf("keys.json: %v", err)
	}
	written, host = map[string]bool{}, map[string]bool{}
	for _, k := range keys.UruncKeys {
		written[k] = true
	}
	for _, k := range keys.HostKeys {
		host[k] = true
	}
	return written, host
}

// uruncReads returns the keys urunc reads, each a field of its
// UnikernelConfig, and whether it reads each from the spec alone.
func uruncReads(t *testing.T) map[string]bool {
	t.Helper()
	reads := map[string]bool{}
	config := reflect.TypeFor[unikontainers.UnikernelConfig]()
	for i := range config.NumField() {
		field := config.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag != "-" {
			reads[tag] = false
			continue
		}
		key, ok := specOnly[field.Name]
		if !ok {
			t.Errorf("urunc reads %s from the spec alone, and specOnly does not name its key", field.Name)
			continue
		}
		reads[key] = true
	}
	return reads
}

// TestHullWritesTheKeysUruncReads checks hull's keys against urunc's in both
// directions. A key urunc reads that hull does not write, or a key urunc
// reads from the spec alone that hull lets an image set, is one an image
// can set through the shim. A key hull writes that urunc does not read is
// a stale or misspelled one, unless ahead names it.
func TestHullWritesTheKeysUruncReads(t *testing.T) {
	written, host := readHullKeys(t)
	reads := uruncReads(t)
	for _, k := range slices.Sorted(maps.Keys(reads)) {
		hostOnly := reads[k]
		switch {
		case !written[k]:
			t.Errorf("urunc reads %s, and uruncKeys lacks it, so the shim would fill it in from the image", k)
		case hostOnly && !host[k]:
			t.Errorf("urunc reads %s from the spec alone, and hostKeys lacks it, so an image could set it", k)
		case !hostOnly && host[k]:
			t.Errorf("urunc reads %s from an image's urunc.json too, and hostKeys keeps an image from setting it", k)
		}
		if ahead[k] {
			t.Errorf("urunc reads %s: take it out of ahead", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(written)) {
		_, read := reads[k]
		if !read && !ahead[k] {
			t.Errorf("uruncKeys has %s, which urunc does not read, and ahead does not name it", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(host)) {
		_, read := reads[k]
		if !read && !ahead[k] {
			t.Errorf("hostKeys has %s, which urunc does not read, and ahead does not name it", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(ahead)) {
		if !written[k] || !host[k] {
			t.Errorf("ahead names %s, which is not in both uruncKeys and hostKeys", k)
		}
	}
}
