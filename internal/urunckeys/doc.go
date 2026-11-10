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

// Package urunckeys checks the urunc annotations hull writes on Linux
// against the urunc the brig-standalone-linux bundle ships.
//
// urunc's containerd shim copies any com.urunc.unikernel.* key the runtime
// spec lacks from the image, so hull writes every key urunc reads
// (uruncKeys in internal/containerd/urunc.go), and keeps images from setting
// the keys urunc reads from the spec alone (hostKeys). A key urunc starts
// reading that hull does not write is one an image can set.
//
// This is a module of its own because hull's go.mod requires urunc for the
// macOS backends alone, at whatever commit they need, and on Linux hull
// reaches urunc only through containerd. This module's go.mod requires the
// urunc the bundle ships instead, and moves when the bundle does. It cannot
// import hull's lists without taking hull's requirements, urunc's among
// them, so it reads them from keys.json, which a test in
// internal/containerd keeps equal to the lists.
package urunckeys
