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

//go:build darwin || linux

package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// gatewayFlagKeys pairs each gateway flag a caller asks about with the key
// `hull version --json` answers under capabilities.gateway. The keys are a
// contract with callers, so they are spelled out here rather than read back
// from the struct tags.
var gatewayFlagKeys = []struct{ flag, key string }{
	{"api", "api"},
	{"egress-default", "egressDefault"},
	{"divert", "divert"},
}

// reportedGateway is capabilities.gateway as a caller decodes it from the
// command tree under root.
func reportedGateway(t *testing.T, root *cli.Command) map[string]bool {
	t.Helper()
	blob, err := json.Marshal(capabilitiesOf(root))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Gateway map[string]bool `json:"gateway"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", blob, err)
	}
	return got.Gateway
}

func gatewayHasFlag(gw *cli.Command, name string) bool {
	for _, f := range gw.Flags {
		if slices.Contains(f.Names(), name) {
			return true
		}
	}
	return false
}

// The report must not say a build has a gateway flag it lacks, or leave out
// one it has. It is checked against the network-gateway command this
// platform's hull registers, so a flag added, removed or renamed there moves
// the report with it or fails here.
func TestCapabilitiesFollowTheGatewayFlags(t *testing.T) {
	root := newApp()
	gw := root.Command("network-gateway")
	if gw == nil {
		t.Fatal("this platform's hull registers no network-gateway; the report would say nothing about one")
	}
	got := reportedGateway(t, root)
	for _, fk := range gatewayFlagKeys {
		reported, present := got[fk.key]
		if gatewayHasFlag(gw, fk.flag) {
			if !present || !reported {
				t.Errorf("the gateway has --%s, but capabilities.gateway.%s is %v (present: %v)", fk.flag, fk.key, reported, present)
			}
		} else if reported {
			t.Errorf("the gateway has no --%s, but capabilities.gateway.%s is true", fk.flag, fk.key)
		}
	}
}

// A capability is read from the flag, not written down beside it. A gateway
// command without the flag has to report false, or the check above would pass
// for a report that says true whatever the gateway does.
func TestCapabilitiesDropWithTheFlag(t *testing.T) {
	for _, fk := range gatewayFlagKeys {
		t.Run(fk.flag, func(t *testing.T) {
			gw := networkGatewayCommand()
			gw.Flags = slices.DeleteFunc(gw.Flags, func(f cli.Flag) bool {
				return slices.Contains(f.Names(), fk.flag)
			})
			root := &cli.Command{Name: "hull", Commands: []*cli.Command{gw}}
			got := reportedGateway(t, root)
			if got[fk.key] {
				t.Errorf("a gateway without --%s still reports capabilities.gateway.%s", fk.flag, fk.key)
			}
			if fk.flag == "divert" && got["divertLinkLocal"] {
				t.Error("a gateway without --divert still reports divertLinkLocal")
			}
		})
	}
}

// divertLinkLocal has to agree with what the gateway does when it is given a
// link-local divert. This asks the way a caller without the report does: the
// same link-local divert twice. A gateway that takes link-local refuses the
// second as diverted twice; one that does not refuses the first as
// link-local. Either way it stops before it starts anything.
func TestDivertLinkLocalAgreesWithTheGateway(t *testing.T) {
	divert := "169.254.170.2:80=/tmp/hull-capability-probe.sock"
	err := runGatewayCommand(t, "--divert", divert, "--divert", divert)
	if err == nil {
		t.Fatal("the gateway started with the same divert given twice")
	}
	accepts := strings.Contains(err.Error(), "diverted twice")
	if !accepts && !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("the gateway refused a link-local divert with %q, which names neither the duplicate nor link-local", err)
	}
	got := reportedGateway(t, newApp())
	if got["divertLinkLocal"] != accepts {
		t.Fatalf("capabilities.gateway.divertLinkLocal = %v, but the gateway answered a link-local divert with %q", got["divertLinkLocal"], err)
	}
}

// A tree with no network-gateway reports no gateway at all, rather than a
// gateway that can do nothing. A caller reads the missing key as false.
func TestCapabilitiesLeaveOutAMissingGateway(t *testing.T) {
	blob, err := json.Marshal(capabilitiesOf(&cli.Command{Name: "hull"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(blob) != "{}" {
		t.Fatalf("capabilities of a tree with no gateway = %s, want {}", blob)
	}
}
