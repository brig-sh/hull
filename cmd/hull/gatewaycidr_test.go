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

import "testing"

func TestGatewayNetConfig(t *testing.T) {
	addr, gw, mask, err := gatewayNetConfig("10.87.0.10/24")
	if err != nil || addr != "10.87.0.10" || gw != "10.87.0.1" || mask != "255.255.255.0" {
		t.Errorf("gatewayNetConfig = %s,%s,%s,%v", addr, gw, mask, err)
	}
	if _, _, _, err := gatewayNetConfig("not-a-cidr"); err == nil {
		t.Error("expected error for invalid CIDR")
	}
}
