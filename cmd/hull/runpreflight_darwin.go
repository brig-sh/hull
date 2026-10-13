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

// platformRunPreflight has nothing to narrow on macOS: every backend and
// rootfs mode run.go knows is available here.
func platformRunPreflight(r runRequest) (runRequest, error) { return r, nil }

// defaultHypervisor is qemu, for backward compatibility: it was the only
// backend when the flag and the annotation were both optional.
const defaultHypervisor = "qemu"

// platformCheckHypervisor has nothing to refuse on macOS; an unknown name
// fails in newVMM.
func platformCheckHypervisor(string, string) error { return nil }
