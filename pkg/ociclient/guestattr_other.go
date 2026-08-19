// Copyright (c) 2023-2026, Nubificus LTD
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

//go:build !darwin

package ociclient

// Everywhere else an unpack can hold the image's ownership in the filesystem
// itself, so there is nothing to record beside it and no reason to avoid
// containerd's extractor. See the darwin file for why macOS cannot.

const recordsOwnership = false

func recordGuestAttr(_ string, _ uint32, _, _ uint32, _ bool) error { return nil }

func GuestAttr(_ string, _ bool) (mode, uid, gid uint32, ok bool) { return 0, 0, 0, false }
