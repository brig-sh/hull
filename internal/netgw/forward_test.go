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

package netgw

import (
	"errors"
	"net"
	"reflect"
	"strconv"
	"testing"
)

func TestParseForwardReadsTheFlagGrammar(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Forward
	}{
		{"127.0.0.1:8080=10.87.0.2:80", Forward{"tcp", "127.0.0.1:8080", "10.87.0.2:80"}},
		{"udp:127.0.0.1:53=10.87.0.2:53", Forward{"udp", "127.0.0.1:53", "10.87.0.2:53"}},
		{"0.0.0.0:443=10.87.0.9:443", Forward{"tcp", "0.0.0.0:443", "10.87.0.9:443"}},
	} {
		got, err := ParseForward(tc.in)
		if err != nil {
			t.Fatalf("ParseForward(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseForward(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseForwardRefusesWhatCannotBeInstalled(t *testing.T) {
	for _, in := range []string{
		"127.0.0.1:8080",           // no guest address
		"127.0.0.1=10.87.0.2:80",   // no host port
		"127.0.0.1:8080=10.87.0.2", // no guest port
		"127.0.0.1:0=10.87.0.2:80",
		"127.0.0.1:70000=10.87.0.2:80",
		"127.0.0.1:8080=guest:80", // a name, not an address
		"sctp:127.0.0.1:80=10.87.0.2:80",
		// IPv6 parses as an address and is still not one this forwards: the
		// forwarder splits a remote on its colons and calls To4 on the result,
		// so it would reach the caller as the gateway's own failure.
		"127.0.0.1:8080=[fd00::2]:80",
	} {
		if _, err := ParseForward(in); err == nil {
			t.Fatalf("ParseForward(%q) was accepted", in)
		} else if !errors.Is(err, ErrForwardInvalid) {
			t.Fatalf("ParseForward(%q): %v is not ErrForwardInvalid", in, err)
		}
	}
}

// forwardNetwork builds a network with no egress of its own, for the forward
// bookkeeping alone.
func forwardNetwork(t *testing.T, start ...Forward) *Network {
	t.Helper()
	n, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Forwards:          start,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// freePort takes a port from the host and gives it straight back, so the
// number is one nothing else on this machine is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestExposeAndUnexposeChangeARunningGateway(t *testing.T) {
	n := forwardNetwork(t)
	if got := n.Forwards(); len(got) != 0 {
		t.Fatalf("a new gateway has %d forwards", len(got))
	}

	local := "127.0.0.1:" + freePort(t)
	installed, err := n.Expose(Forward{Local: local, Remote: "10.87.0.2:80"})
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	// What was installed, with the defaults filled in -- this is what the API
	// answers a POST with, and a caller that omitted the protocol must not be
	// told it is empty when a later read says tcp.
	if installed.Protocol != "tcp" || installed.Local != local {
		t.Fatalf("Expose returned %+v, want the forward as installed", installed)
	}
	want := []Forward{{"tcp", local, "10.87.0.2:80"}}
	if got := n.Forwards(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Forwards = %+v, want %+v", got, want)
	}
	// The listener is real: the host can connect to it while it is installed.
	conn, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatalf("the published port does not accept: %v", err)
	}
	_ = conn.Close()

	gone, err := n.Unexpose("tcp", local)
	if err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if gone.Remote != "10.87.0.2:80" {
		t.Fatalf("Unexpose returned %+v", gone)
	}
	if got := n.Forwards(); len(got) != 0 {
		t.Fatalf("after Unexpose there are %d forwards", len(got))
	}
}

func TestExposeRefusesALocalAddressAlreadyPublished(t *testing.T) {
	n := forwardNetwork(t)
	local := "127.0.0.1:" + freePort(t)
	if _, err := n.Expose(Forward{Local: local, Remote: "10.87.0.2:80"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	_, err := n.Expose(Forward{Local: local, Remote: "10.87.0.3:80"})
	if !errors.Is(err, ErrForwardExists) {
		t.Fatalf("second Expose: %v is not ErrForwardExists", err)
	}
	// The guest already published there is named, so the caller can say whose
	// port is in the way.
	if got := n.Forwards(); len(got) != 1 || got[0].Remote != "10.87.0.2:80" {
		t.Fatalf("the refused Expose changed the set: %+v", got)
	}
}

func TestUnexposeRefusesAPortNothingPublished(t *testing.T) {
	n := forwardNetwork(t)
	if _, err := n.Unexpose("tcp", "127.0.0.1:9"); !errors.Is(err, ErrForwardNotFound) {
		t.Fatalf("Unexpose: %v is not ErrForwardNotFound", err)
	}
}

func TestStartupForwardsAreListedLikeAnyOther(t *testing.T) {
	local := "127.0.0.1:" + freePort(t)
	n := forwardNetwork(t, Forward{Protocol: "tcp", Local: local, Remote: "10.87.0.2:8080"})
	want := []Forward{{"tcp", local, "10.87.0.2:8080"}}
	if got := n.Forwards(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Forwards = %+v, want %+v", got, want)
	}
	if _, err := n.Unexpose("tcp", local); err != nil {
		t.Fatalf("a startup forward cannot be withdrawn: %v", err)
	}
}

func TestNewRefusesAStartupForwardItCannotInstall(t *testing.T) {
	_, err := New(Config{
		MTU:               1500,
		Subnet:            testSubnet,
		GatewayIP:         testGatewayIP,
		GatewayMacAddress: testGatewayMA,
		Forwards:          []Forward{{Local: "127.0.0.1:8080", Remote: "not-an-ip:80"}},
	})
	if !errors.Is(err, ErrForwardInvalid) {
		t.Fatalf("New: %v is not ErrForwardInvalid", err)
	}
}
