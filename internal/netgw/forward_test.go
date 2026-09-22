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
	"strings"
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
		// Recorded as net.IP renders it, so one address has one spelling.
		{"[0:0:0:0:0:0:0:1]:8081=10.87.0.2:80", Forward{"tcp", "[::1]:8081", "10.87.0.2:80"}},
		// net.Listen reads an empty host as every interface, so the flag
		// keeps taking it and records the address it binds.
		{":8080=10.87.0.2:80", Forward{"tcp", "0.0.0.0:8080", "10.87.0.2:80"}},
		{"udp::53=10.87.0.2:53", Forward{"udp", "0.0.0.0:53", "10.87.0.2:53"}},
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
		"127.0.0.1:8080=guest:80",     // a name, not an address
		"localhost:8080=10.87.0.2:80", // the local named rather than addressed
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

// An empty host and 0.0.0.0 name one address, so publishing both is the
// same collision as publishing either twice.
func TestExposeReadsAnEmptyHostAsAllInterfaces(t *testing.T) {
	n := forwardNetwork(t)
	port := freePort(t)
	got, err := n.Expose(Forward{Local: ":" + port, Remote: "10.87.0.2:80"})
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if got.Local != "0.0.0.0:"+port {
		t.Fatalf("Expose installed %q, want 0.0.0.0:%s", got.Local, port)
	}
	if _, err := n.Expose(Forward{Local: "0.0.0.0:" + port, Remote: "10.87.0.3:80"}); !errors.Is(err, ErrForwardExists) {
		t.Fatalf("0.0.0.0 after the empty host: %v is not ErrForwardExists", err)
	}
	if _, err := n.Unexpose("tcp", ":"+port); err != nil {
		t.Fatalf("Unexpose by the empty host: %v", err)
	}
}

// Whether 0.0.0.0:P and 127.0.0.1:P can both be bound is the host kernel's
// answer: darwin takes the pair for TCP because Go sets SO_REUSEADDR on every
// TCP listener, and Linux refuses it. So the gateway does not rule on the
// pair itself. What it owes either way is an answer the caller can act on:
// the publication succeeds, or it is refused as already published.
func TestExposeLeavesAnOverlappingAddressToTheKernel(t *testing.T) {
	for _, order := range [][2]string{
		{"0.0.0.0", "127.0.0.1"},
		{"127.0.0.1", "0.0.0.0"},
	} {
		n := forwardNetwork(t)
		port := freePort(t)
		if _, err := n.Expose(Forward{Local: order[0] + ":" + port, Remote: "10.87.0.2:80"}); err != nil {
			t.Fatalf("Expose(%s): %v", order[0], err)
		}
		_, err := n.Expose(Forward{Local: order[1] + ":" + port, Remote: "10.87.0.3:80"})
		if err != nil && !errors.Is(err, ErrForwardExists) {
			t.Fatalf("Expose(%s) after %s: %v is neither installed nor ErrForwardExists",
				order[1], order[0], err)
		}
	}
}

// A port something else on the host already holds is refused as published,
// not as the gateway's own failure. The caller acts on the two the same way:
// the address is taken, pick another.
func TestExposeReportsAPortHeldOutsideTheGatewayAsPublished(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %v", err)
	}
	defer func() { _ = held.Close() }()

	n := forwardNetwork(t)
	_, err = n.Expose(Forward{Local: held.Addr().String(), Remote: "10.87.0.2:80"})
	if !errors.Is(err, ErrForwardExists) {
		t.Fatalf("a port held outside the gateway: %v is not ErrForwardExists", err)
	}
}

// A port refused because this gateway holds it on another address says so.
// Attributing the kernel's refusal to an outsider sends the reader looking
// for a process that is not there. UDP is the case that reaches it on both
// hosts: the set's keys differ, so only the bind refuses.
func TestExposeNamesTheForwardHoldingAPortItCannotBind(t *testing.T) {
	n := forwardNetwork(t)
	port := freePort(t)
	if _, err := n.Expose(Forward{Protocol: "udp", Local: "0.0.0.0:" + port, Remote: "10.87.0.2:53"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	_, err := n.Expose(Forward{Protocol: "udp", Local: "127.0.0.1:" + port, Remote: "10.87.0.3:53"})
	if !errors.Is(err, ErrForwardExists) {
		t.Fatalf("udp beside a published wildcard: %v is not ErrForwardExists", err)
	}
	if strings.Contains(err.Error(), "outside this gateway") {
		t.Fatalf("the holder is this gateway, and the message says otherwise: %v", err)
	}
	if !strings.Contains(err.Error(), "0.0.0.0:"+port) {
		t.Fatalf("the message must name the forward holding the port: %v", err)
	}
}

// A local address must be an address. A name is not what the listener binds,
// so a forward published under one could not be withdrawn under the address
// it actually holds.
func TestExposeRefusesALocalNamedRatherThanAddressed(t *testing.T) {
	n := forwardNetwork(t)
	if _, err := n.Expose(Forward{Local: "localhost:" + freePort(t), Remote: "10.87.0.2:80"}); !errors.Is(err, ErrForwardInvalid) {
		t.Fatalf("a named local: %v is not ErrForwardInvalid", err)
	}
}

// A withdrawal the gateway cannot read is invalid, not absent. As "no forward
// listens there" a caller reads a typo as a forward already gone.
func TestUnexposeTellsAnUnreadableTargetFromAnAbsentOne(t *testing.T) {
	n := forwardNetwork(t)
	for _, tc := range []struct{ protocol, local string }{
		{"sctp", "127.0.0.1:3000"},
		{"tcp", ""},
		{"tcp", "garbage"},
		{"tcp", "127.0.0.1:99999"},
	} {
		if _, err := n.Unexpose(tc.protocol, tc.local); !errors.Is(err, ErrForwardInvalid) {
			t.Fatalf("Unexpose(%q, %q): %v is not ErrForwardInvalid", tc.protocol, tc.local, err)
		}
	}
	if _, err := n.Unexpose("tcp", "127.0.0.1:9"); !errors.Is(err, ErrForwardNotFound) {
		t.Fatalf("a readable target nothing holds: %v is not ErrForwardNotFound", err)
	}
}

// A port is only taken on the address that holds it, so two specific
// addresses on one port are two forwards.
func TestExposeAllowsOnePortOnTwoSpecificAddresses(t *testing.T) {
	n := forwardNetwork(t)
	port := freePort(t)
	if _, err := n.Expose(Forward{Local: "127.0.0.1:" + port, Remote: "10.87.0.2:80"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if _, err := n.Expose(Forward{Protocol: "udp", Local: "127.0.0.1:" + port, Remote: "10.87.0.3:80"}); err != nil {
		t.Fatalf("udp beside tcp on one address: %v", err)
	}
	if got := n.Forwards(); len(got) != 2 {
		t.Fatalf("want two forwards, got %+v", got)
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
