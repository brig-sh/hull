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
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/brig-sh/hull/internal/netgw"
)

// fakeForwards is a gateway's forwards without a gateway: the bookkeeping the
// endpoint drives, and none of the netstack underneath it.
//
// A real netgw.Network would start the DHCP and DNS servers and their
// goroutines, and those outlive the test that built them. Enough of them in
// this package perturbed the sampler test, which measures elapsed time. The
// netstack itself is covered where it belongs, in internal/netgw.
type fakeForwards struct {
	set []netgw.Forward
}

func (f *fakeForwards) Expose(fwd netgw.Forward) (netgw.Forward, error) {
	if fwd.Protocol == "" {
		fwd.Protocol = "tcp"
	}
	// Validated through netgw's own parser rather than a second copy of the
	// rule, so the endpoint's 400 is exercised against what really refuses.
	spec := fwd.Local + "=" + fwd.Remote
	if fwd.Protocol == "udp" {
		spec = "udp:" + spec
	}
	if _, err := netgw.ParseForward(spec); err != nil {
		return netgw.Forward{}, err
	}
	for _, have := range f.set {
		if have.Local == fwd.Local && have.Protocol == fwd.Protocol {
			return netgw.Forward{}, fmt.Errorf("%w: %s/%s carries %s", netgw.ErrForwardExists,
				have.Protocol, have.Local, have.Remote)
		}
	}
	f.set = append(f.set, fwd)
	return fwd, nil
}

func (f *fakeForwards) Unexpose(protocol, local string) (netgw.Forward, error) {
	if protocol == "" {
		protocol = "tcp"
	}
	for i, have := range f.set {
		if have.Local == local && have.Protocol == protocol {
			f.set = append(f.set[:i], f.set[i+1:]...)
			return have, nil
		}
	}
	return netgw.Forward{}, fmt.Errorf("%w: %s/%s", netgw.ErrForwardNotFound, protocol, local)
}

func (f *fakeForwards) Forwards() []netgw.Forward {
	if f.set == nil {
		return []netgw.Forward{}
	}
	return f.set
}

func forwardAPI(t *testing.T) http.Handler {
	t.Helper()
	return forwardsHandler(&fakeForwards{})
}

// hostPort is a distinct local address per call. Nothing binds it: the fake
// records a forward rather than listening, so these never touch the host.
var portCounter = 20000

func hostPort(t *testing.T) string {
	t.Helper()
	portCounter++
	return "127.0.0.1:" + strconv.Itoa(portCounter)
}

func call(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestForwardsAPIPublishesListsAndWithdraws(t *testing.T) {
	h := forwardAPI(t)
	local := hostPort(t)

	if w := call(t, h, http.MethodGet, "/forwards", ""); w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	} else if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("a gateway with nothing published answers %q", got)
	}

	w := call(t, h, http.MethodPost, "/forwards",
		`{"protocol":"tcp","local":"`+local+`","remote":"10.87.0.2:3000"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}

	var have []netgw.Forward
	if err := json.NewDecoder(call(t, h, http.MethodGet, "/forwards", "").Body).Decode(&have); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(have) != 1 || have[0].Local != local || have[0].Remote != "10.87.0.2:3000" {
		t.Fatalf("GET after POST: %+v", have)
	}

	if w := call(t, h, http.MethodDelete, "/forwards?protocol=tcp&local="+local, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if got := strings.TrimSpace(call(t, h, http.MethodGet, "/forwards", "").Body.String()); got != "[]" {
		t.Fatalf("after DELETE: %q", got)
	}
}

func TestForwardsAPITellsTheFailuresApart(t *testing.T) {
	h := forwardAPI(t)
	local := hostPort(t)
	if w := call(t, h, http.MethodPost, "/forwards",
		`{"local":"`+local+`","remote":"10.87.0.2:80"}`); w.Code != http.StatusCreated {
		t.Fatalf("first POST: %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct {
		what   string
		method string
		target string
		body   string
		want   int
	}{
		{"a local address already published", http.MethodPost, "/forwards",
			`{"local":"` + local + `","remote":"10.87.0.3:80"}`, http.StatusConflict},
		{"a port nothing published", http.MethodDelete, "/forwards?local=127.0.0.1:9", "", http.StatusNotFound},
		{"an address with no port", http.MethodPost, "/forwards",
			`{"local":"127.0.0.1","remote":"10.87.0.2:80"}`, http.StatusBadRequest},
		{"a guest named rather than addressed", http.MethodPost, "/forwards",
			`{"local":"127.0.0.1:1","remote":"web:80"}`, http.StatusBadRequest},
		{"a body that is not JSON", http.MethodPost, "/forwards", "{", http.StatusBadRequest},
		{"a verb the endpoint has no meaning for", http.MethodPut, "/forwards", "{}", http.StatusMethodNotAllowed},
	} {
		if w := call(t, h, tc.method, tc.target, tc.body); w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d (%s)", tc.what, w.Code, tc.want, w.Body)
		}
	}
}

// A POST answers with the forward as installed, not as asked for. A request
// that omits the protocol is answered "tcp", which is what a later GET says
// about the same forward -- the two must not describe it differently.
func TestForwardsAPIAnswersWithWhatItInstalled(t *testing.T) {
	h := forwardAPI(t)
	local := hostPort(t)
	w := call(t, h, http.MethodPost, "/forwards",
		`{"local":"`+local+`","remote":"10.87.0.2:80"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	var posted netgw.Forward
	if err := json.NewDecoder(w.Body).Decode(&posted); err != nil {
		t.Fatalf("decode the POST response: %v", err)
	}
	if posted.Protocol != "tcp" {
		t.Fatalf("POST answered protocol %q, want the tcp it installed", posted.Protocol)
	}
	var listed []netgw.Forward
	if err := json.NewDecoder(call(t, h, http.MethodGet, "/forwards", "").Body).Decode(&listed); err != nil {
		t.Fatalf("decode the GET: %v", err)
	}
	if len(listed) != 1 || listed[0] != posted {
		t.Fatalf("GET says %+v, POST said %+v", listed, posted)
	}
}

// The protocol is optional everywhere, because tcp is what a published port
// almost always is.
func TestForwardsAPIDefaultsToTCP(t *testing.T) {
	h := forwardAPI(t)
	local := hostPort(t)
	if w := call(t, h, http.MethodPost, "/forwards",
		`{"local":"`+local+`","remote":"10.87.0.2:80"}`); w.Code != http.StatusCreated {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, http.MethodDelete, "/forwards?local="+local, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
}
