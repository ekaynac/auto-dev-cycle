// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/netip"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
)

// Per-peer limits count a socket peer by uid, an IPv4 peer by address
// (the same when IPv4-mapped or 6to4), an IPv6 peer by its /56, and a
// link-local IPv6 peer by its /56 on its interface. A peer cannot step out
// of its key by its choice of form or of address within its allocation.
func TestPeerKey(t *testing.T) {
	key := func(addr string) any { return api.Peer{UID: -1, Addr: netip.MustParseAddr(addr)}.Key() }
	same := map[string][2]any{
		"a uid":                             {api.Peer{UID: 1000}.Key(), 1000},
		"IPv4 and IPv4-mapped IPv6":         {key("192.0.2.7"), key("::ffff:192.0.2.7")},
		"IPv4 and its 6to4 address":         {key("192.0.2.7"), key("2002:c000:207::1")},
		"two /56s of one 6to4 /48":          {key("2002:c000:207:100::1"), key("2002:c000:207:ff00::1")},
		"two addresses of one IPv6 /56":     {key("2001:db8:0:ff00::1"), key("2001:db8:0:ffff:1234::9")},
		"two link-local hosts on one link":  {key("fe80::1%eth0"), key("fe80::2:3:4:5%eth0")},
		"IPv4-mapped with a zone, and IPv4": {key("::ffff:192.0.2.7%eth0"), key("192.0.2.7")},
	}
	for name, p := range same {
		if p[0] != p[1] {
			t.Errorf("%s: %v != %v", name, p[0], p[1])
		}
	}
	differ := map[string][2]any{
		"two IPv4 addresses":                    {key("192.0.2.7"), key("192.0.2.8")},
		"two 6to4 /48s":                         {key("2002:c000:207::1"), key("2002:c000:208::1")},
		"two IPv6 /56s":                         {key("2001:db8:0:ff00::1"), key("2001:db8:0:fe00::1")},
		"a uid and an address":                  {api.Peer{UID: -1}.Key(), key("192.0.2.7")},
		"one link-local /56 on two links":       {key("fe80::1%eth0"), key("fe80::1%eth1")},
		"a link-local host and an unscoped /56": {key("fe80::1%eth0"), key("fe80::1")},
	}
	for name, p := range differ {
		if p[0] == p[1] {
			t.Errorf("%s: both %v", name, p[0])
		}
	}
}
