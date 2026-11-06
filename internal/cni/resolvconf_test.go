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

package cni

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvConf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  string
		first []string
		want  string
	}{
		{
			name: "loopback, IPv6 and duplicate nameservers left out",
			host: "# generated\nnameserver 127.0.0.53\nnameserver 192.0.2.1\nnameserver ::1\n" +
				"nameserver 2001:db8::1\nnameserver 192.0.2.1\nsearch example.com\noptions edns0\n",
			want: "nameserver 192.0.2.1\nsearch example.com\noptions edns0\n",
		},
		{
			name:  "first comes first, and the host's copy of it goes",
			host:  "nameserver 192.0.2.1\nnameserver 10.0.2.3\n",
			first: []string{"10.0.2.3"},
			want:  "nameserver 10.0.2.3\nnameserver 192.0.2.1\n",
		},
		{
			name: "nothing reachable falls back",
			host: "nameserver 127.0.0.1\nsearch .\n",
			want: "nameserver 8.8.8.8\nnameserver 8.8.4.4\nsearch .\n",
		},
		{
			name: "an empty file falls back",
			want: "nameserver 8.8.8.8\nnameserver 8.8.4.4\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(ResolvConf([]byte(tc.host), tc.first))
			if got != tc.want {
				t.Errorf("ResolvConf =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestHostResolvConf(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "resolv.conf")
	resolved := filepath.Join(dir, "resolved.conf")
	oldHost, oldResolved := hostResolvConf, resolvedResolvConf
	hostResolvConf, resolvedResolvConf = host, resolved
	t.Cleanup(func() { hostResolvConf, resolvedResolvConf = oldHost, oldResolved })

	write := func(path, data string) {
		t.Helper()
		err := os.WriteFile(path, []byte(data), 0o644)
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		data, err := HostResolvConf()
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	stub := "nameserver 127.0.0.53\noptions edns0\n"
	write(host, stub)
	got := read()
	if got != stub {
		t.Errorf("with no systemd-resolved file, HostResolvConf = %q, want the host's", got)
	}
	upstream := "nameserver 192.0.2.1\n"
	write(resolved, upstream)
	got = read()
	if got != upstream {
		t.Errorf("behind systemd-resolved's stub, HostResolvConf = %q, want its upstream file", got)
	}
	own := "nameserver 127.0.0.53\nnameserver 192.0.2.7\n"
	write(host, own)
	got = read()
	if got != own {
		t.Errorf("with a nameserver besides the stub, HostResolvConf = %q, want the host's", got)
	}
	single := "nameserver 192.0.2.9\n"
	write(host, single)
	got = read()
	if got != single {
		t.Errorf("with one nameserver that is not the stub, HostResolvConf = %q, want the host's", got)
	}
}
