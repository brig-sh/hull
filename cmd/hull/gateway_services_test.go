// Copyright 2026 The hull Authors
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
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/netgw"
)

// serviceAPI serves a real service table with no netstack under it: the
// table is usable on its own, so this needs no fake.
func serviceAPI(t *testing.T, cidr string) http.Handler {
	t.Helper()
	var prefix netip.Prefix
	if cidr != "" {
		prefix = netip.MustParsePrefix(cidr)
	}
	return servicesHandler(netgw.NewServiceTable(prefix, netip.MustParsePrefix("10.87.0.0/24")))
}

// The table macnode sends: cluster DNS on a guest, and the apiserver on the
// host.
const clusterServices = `[
 {"vip":"10.96.0.10","port":53,"protocol":"udp","endpoints":[{"ip":"10.87.0.12","port":53}]},
 {"vip":"10.96.0.1","port":443,"protocol":"tcp","endpoints":[{"ip":"127.0.0.1","port":6443,"host":true}]}
]`

func TestServicesAPIReplacesAndLists(t *testing.T) {
	h := serviceAPI(t, "10.96.0.0/12")

	if w := call(t, h, http.MethodGet, "/services", ""); w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	} else if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("a gateway with no services answers %q", got)
	}

	w := call(t, h, http.MethodPut, "/services", clusterServices)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	var put []netgw.Service
	if err := json.NewDecoder(w.Body).Decode(&put); err != nil {
		t.Fatalf("decode the PUT response: %v", err)
	}

	got := call(t, h, http.MethodGet, "/services", "")
	var listed []netgw.Service
	if err := json.NewDecoder(got.Body).Decode(&listed); err != nil {
		t.Fatalf("decode the GET: %v", err)
	}
	if len(listed) != 2 || len(put) != 2 {
		t.Fatalf("GET %+v, PUT answered %+v", listed, put)
	}
	// Ordered by address, so the apiserver comes first whatever order it was
	// sent in, and the PUT answers with what a GET then says.
	if listed[0].VIP.String() != "10.96.0.1" || !listed[0].Endpoints[0].Host ||
		listed[1].VIP.String() != "10.96.0.10" || listed[1].Protocol != "udp" {
		t.Fatalf("GET: %+v", listed)
	}
	for i := range listed {
		if listed[i].VIP != put[i].VIP || listed[i].Port != put[i].Port || listed[i].Protocol != put[i].Protocol {
			t.Fatalf("GET says %+v, PUT said %+v", listed, put)
		}
	}

	// Replace-whole: a PUT of one service leaves only that one.
	if w := call(t, h, http.MethodPut, "/services",
		`[{"vip":"10.96.0.20","port":80,"endpoints":[]}]`); w.Code != http.StatusOK {
		t.Fatalf("second PUT: %d %s", w.Code, w.Body)
	}
	listed = nil
	if err := json.NewDecoder(call(t, h, http.MethodGet, "/services", "").Body).Decode(&listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].VIP.String() != "10.96.0.20" || listed[0].Protocol != "tcp" {
		t.Fatalf("after the second PUT: %+v", listed)
	}

	if w := call(t, h, http.MethodPut, "/services", `[]`); w.Code != http.StatusOK {
		t.Fatalf("emptying PUT: %d %s", w.Code, w.Body)
	}
	if got := strings.TrimSpace(call(t, h, http.MethodGet, "/services", "").Body.String()); got != "[]" {
		t.Fatalf("after emptying: %q", got)
	}
}

// The JSON a caller writes, field by field: a host endpoint carries
// "host":true, and a guest endpoint leaves it out.
func TestServicesAPIRendersTheContract(t *testing.T) {
	h := serviceAPI(t, "10.96.0.0/12")
	if w := call(t, h, http.MethodPut, "/services", clusterServices); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	want := `[{"vip":"10.96.0.1","port":443,"protocol":"tcp","endpoints":[{"ip":"127.0.0.1","port":6443,"host":true}]},` +
		`{"vip":"10.96.0.10","port":53,"protocol":"udp","endpoints":[{"ip":"10.87.0.12","port":53}]}]`
	if got := strings.TrimSpace(call(t, h, http.MethodGet, "/services", "").Body.String()); got != want {
		t.Fatalf("GET renders\n%s\nwant\n%s", got, want)
	}
}

func TestServicesAPITellsTheFailuresApart(t *testing.T) {
	for _, tc := range []struct {
		what   string
		cidr   string
		method string
		body   string
		want   int
	}{
		{"a body that is not JSON", "10.96.0.0/12", http.MethodPut, "[", http.StatusBadRequest},
		{"one service rather than a list", "10.96.0.0/12", http.MethodPut,
			`{"vip":"10.96.0.1","port":443}`, http.StatusBadRequest},
		{"a field the gateway does not know", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.1","port":443,"endpoint":[]}]`, http.StatusBadRequest},
		{"an address that does not parse", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.300","port":443}]`, http.StatusBadRequest},
		{"a port out of range", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.1","port":70000}]`, http.StatusBadRequest},
		{"a virtual address outside the range", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.112.0.1","port":443}]`, http.StatusUnprocessableEntity},
		{"an unknown protocol", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.1","port":443,"protocol":"sctp"}]`, http.StatusUnprocessableEntity},
		{"a guest endpoint off the subnet", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.1","port":443,"endpoints":[{"ip":"127.0.0.1","port":6443}]}]`, http.StatusUnprocessableEntity},
		{"one service twice", "10.96.0.0/12", http.MethodPut,
			`[{"vip":"10.96.0.1","port":443},{"vip":"10.96.0.1","port":443,"protocol":"tcp"}]`, http.StatusUnprocessableEntity},
		{"a table on a gateway with no service range", "", http.MethodPut,
			`[{"vip":"10.96.0.1","port":443}]`, http.StatusUnprocessableEntity},
		{"an empty table on a gateway with no service range", "", http.MethodPut, `[]`, http.StatusOK},
		{"a verb the endpoint has no meaning for", "10.96.0.0/12", http.MethodPost, "[]", http.StatusMethodNotAllowed},
		{"a delete", "10.96.0.0/12", http.MethodDelete, "", http.StatusMethodNotAllowed},
	} {
		if w := call(t, serviceAPI(t, tc.cidr), tc.method, "/services", tc.body); w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d (%s)", tc.what, w.Code, tc.want, w.Body)
		}
	}
}

// A refused table leaves the installed one in place, so a caller that sent a
// bad table is still routing on its last good one.
func TestServicesAPIKeepsTheTableOnARefusal(t *testing.T) {
	h := serviceAPI(t, "10.96.0.0/12")
	if w := call(t, h, http.MethodPut, "/services", clusterServices); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	before := call(t, h, http.MethodGet, "/services", "").Body.String()
	if w := call(t, h, http.MethodPut, "/services",
		`[{"vip":"10.96.0.1","port":443},{"vip":"192.0.2.1","port":80}]`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad PUT: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, http.MethodPut, "/services", "[{"); w.Code != http.StatusBadRequest {
		t.Fatalf("unparseable PUT: %d %s", w.Code, w.Body)
	}
	if after := call(t, h, http.MethodGet, "/services", "").Body.String(); after != before {
		t.Fatalf("a refused PUT changed the table:\n%s\nto\n%s", before, after)
	}
}

func TestParseServiceCIDR(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", "invalid Prefix", true},
		{"10.96.0.0/12", "10.96.0.0/12", true},
		{"10.96.0.1/12", "10.96.0.0/12", true},
		{"10.96.0.0", "", false},
		{"fd00::/108", "", false},
		{"garbage", "", false},
	} {
		got, err := parseServiceCIDR(tc.in)
		if tc.ok != (err == nil) {
			t.Fatalf("%q: err %v", tc.in, err)
		}
		if tc.ok && got.String() != tc.want {
			t.Fatalf("%q: got %s, want %s", tc.in, got, tc.want)
		}
	}
}

// unroutableForwards refuses every forward the way a gateway refuses a remote
// it cannot reach.
type unroutableForwards struct{ *fakeForwards }

func (unroutableForwards) Expose(f netgw.Forward) (netgw.Forward, error) {
	return netgw.Forward{}, fmt.Errorf("%w: %s", netgw.ErrForwardUnroutable, f.Remote)
}

// A forward whose remote the gateway cannot reach, a service address on a
// gateway with no service range among them, is a 422: the request was read,
// and it names something this gateway cannot carry.
func TestForwardsAPIRefusesAnUnroutableRemote(t *testing.T) {
	h := forwardsHandler(unroutableForwards{&fakeForwards{}})
	w := call(t, h, http.MethodPost, "/forwards", `{"local":"127.0.0.1:1","remote":"10.96.0.20:80"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422 (%s)", w.Code, w.Body)
	}
}
