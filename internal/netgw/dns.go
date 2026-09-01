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
	"context"
	"net"
	"strings"

	gvntypes "github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

// The gateway's resolver answers two kinds of name. The records the gateway
// was started with are served from here, so a guest with no /etc/hosts still
// finds its peers. Everything else is passed to the host's resolver.

// resolver is the host-side resolver, narrowed to what this handler asks of
// it so tests can stand in for it.
type resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type dnsHandler struct {
	zones    []gvntypes.Zone
	upstream resolver
}

func (h *dnsHandler) handle(w dns.ResponseWriter, r *dns.Msg, maxSize int) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	h.addAnswers(m)
	if edns0 := r.IsEdns0(); edns0 != nil {
		maxSize = int(edns0.UDPSize())
	}
	m.Truncate(maxSize)
	if err := w.WriteMsg(m); err != nil {
		log.Errorf("dns: %v", err)
	}
}

func (h *dnsHandler) addAnswers(m *dns.Msg) {
	for _, q := range m.Question {
		if h.addLocalAnswers(m, q) {
			continue
		}
		h.addUpstreamAnswers(m, q)
	}
}

// addLocalAnswers serves the records the gateway was given. It reports
// whether the name belongs to the gateway, which is what stops a service name
// from being asked of the host's resolver.
//
// A zone named "." holds names with no domain of their own, which is how
// compose registers its services. Such a zone is not authoritative for the
// whole namespace: a name it has no record for falls through to the host's
// resolver, or nothing outside the project would resolve at all.
func (h *dnsHandler) addLocalAnswers(m *dns.Msg, q dns.Question) bool {
	name := normalizeName(q.Name)
	for _, zone := range h.zones {
		zoneName := normalizeName(zone.Name)
		withoutZone := name
		switch {
		case zoneName == "":
		case strings.HasSuffix(name, "."+zoneName):
			withoutZone = strings.TrimSuffix(name, "."+zoneName)
		default:
			continue
		}

		for _, record := range zone.Records {
			matched := (record.Name != "" && normalizeName(record.Name) == withoutZone) ||
				(record.Regexp != nil && record.Regexp.MatchString(withoutZone))
			if !matched {
				continue
			}
			// The gateway holds one address per name, so a query for
			// anything else about that name is answered, emptily, by the
			// gateway rather than asked of the host.
			if q.Qtype == dns.TypeA {
				m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 0), A: record.IP})
			}
			return true
		}

		if zoneName == "" {
			continue
		}
		if len(zone.DefaultIP) > 0 {
			if q.Qtype == dns.TypeA {
				m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 0), A: zone.DefaultIP})
			}
			return true
		}
		m.Rcode = dns.RcodeNameError
		return true
	}
	return false
}

func (h *dnsHandler) addUpstreamAnswers(m *dns.Msg, q dns.Question) {
	ctx := context.TODO()
	switch q.Qtype {
	case dns.TypeA:
		addrs, err := h.upstream.LookupIPAddr(ctx, q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		for _, a := range addrs {
			v4 := a.IP.To4()
			if v4 == nil {
				continue
			}
			m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 0), A: v4})
		}
	case dns.TypeCNAME:
		cname, err := h.upstream.LookupCNAME(ctx, q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		m.Answer = append(m.Answer, &dns.CNAME{Hdr: rrHeader(q.Name, dns.TypeCNAME, 0), Target: cname})
	case dns.TypeMX:
		records, err := h.upstream.LookupMX(ctx, q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		for _, mx := range records {
			m.Answer = append(m.Answer, &dns.MX{Hdr: rrHeader(q.Name, dns.TypeMX, 0), Mx: mx.Host, Preference: mx.Pref})
		}
	case dns.TypeNS:
		records, err := h.upstream.LookupNS(ctx, q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		for _, ns := range records {
			m.Answer = append(m.Answer, &dns.NS{Hdr: rrHeader(q.Name, dns.TypeNS, 0), Ns: ns.Host})
		}
	case dns.TypeSRV:
		_, records, err := h.upstream.LookupSRV(ctx, "", "", q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		for _, srv := range records {
			m.Answer = append(m.Answer, &dns.SRV{
				Hdr:      rrHeader(q.Name, dns.TypeSRV, 0),
				Port:     srv.Port,
				Priority: srv.Priority,
				Target:   srv.Target,
				Weight:   srv.Weight,
			})
		}
	case dns.TypeTXT:
		txts, err := h.upstream.LookupTXT(ctx, q.Name)
		if err != nil {
			m.Rcode = dns.RcodeNameError
			return
		}
		for _, txt := range txts {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: rrHeader(q.Name, dns.TypeTXT, 0), Txt: splitTXT(txt)})
		}
	}
	// Every other type, AAAA among them, is answered with no records. IPv6
	// forwarding is not wired up, so handing a guest an IPv6 address would
	// only send it down a path the netstack drops.
}

func rrHeader(name string, rrtype uint16, ttl uint32) dns.RR_Header {
	return dns.RR_Header{Name: name, Rrtype: rrtype, Class: dns.ClassINET, Ttl: ttl}
}

// splitTXT cuts a TXT value into the 255-byte strings the wire format holds.
func splitTXT(s string) []string {
	const max = 255
	var out []string
	for len(s) > max {
		out = append(out, s[:max])
		s = s[max:]
	}
	return append(out, s)
}
