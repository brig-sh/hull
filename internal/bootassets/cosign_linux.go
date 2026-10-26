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

package bootassets

// cosignInstall is how the message for a missing cosign says to get one.
// Linux distributions package cosign under different names, or not at all,
// so it points at Sigstore's installation page rather than at one package
// manager.
const cosignInstall = "see https://docs.sigstore.dev/cosign/system_config/installation/"
