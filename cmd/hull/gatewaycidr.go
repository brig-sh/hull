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

import (
	"fmt"
	"net"
)

// gatewayNetConfig derives the guest address, gateway address (.1 on the
// subnet) and dotted netmask from a CIDR like 10.87.0.10/24 — the pieces of
// the kernel ip= parameter, in the same format urunc uses on Linux.
func gatewayNetConfig(cidr string) (addr, gw, mask string, err error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid gateway CIDR %q: %w", cidr, err)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", "", "", fmt.Errorf("gateway CIDR %q is not IPv4", cidr)
	}
	gwIP := ipnet.IP.To4()
	gwIP = net.IPv4(gwIP[0], gwIP[1], gwIP[2], gwIP[3]+1)
	m := ipnet.Mask
	return ip4.String(), gwIP.String(), net.IPv4(m[0], m[1], m[2], m[3]).String(), nil
}
