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
	"bytes"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// waitForGoroutinesOf fails t unless every goroutine running a closure of fn
// has ended within 5 seconds. Go names a closure after the function it is in,
// as fn.func1, so its frames name fn. fn is passed rather than its name, so
// that renaming fn cannot leave the check looking for a name nothing has.
//
// Only fn's own closures are looked for: a check of every goroutine would
// also find those of the clients a test keeps open.
func waitForGoroutinesOf(t *testing.T, fn any) {
	t.Helper()
	name := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := goroutinesOf(name)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines of %s still run:\n\n%s", len(left), name, strings.Join(left, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// goroutinesOf returns the stacks of the goroutines that are in a closure
// of the function named name.
func goroutinesOf(name string) []string {
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
	var stacks []string
	for _, stack := range strings.Split(b.String(), "\n\n") {
		for _, line := range strings.Split(stack, "\n") {
			if strings.HasPrefix(line, name+".") {
				stacks = append(stacks, stack)
				break
			}
		}
	}
	return stacks
}
