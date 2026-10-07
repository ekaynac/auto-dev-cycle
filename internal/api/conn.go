// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"

	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

type connKey struct{}

// ConnContext is for http.Server.ConnContext: it registers each
// connection, so a stream on HTTP/1.1 can close it hard at a write cutoff.
// Without it a cutoff relies on the write deadline alone, which a graceful
// TLS close (close_notify, with its own five-second deadline) can outlast.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// hardAbort returns what closes r's connection at once, beneath any TLS
// layer, so no graceful shutdown waits on a peer that stopped reading; nil
// on HTTP/2 (a stream deadline cancels only that stream, never the shared
// connection) or when no connection was registered.
func hardAbort(r *http.Request) func() {
	c, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok || r.ProtoMajor != 1 {
		return nil
	}
	if tc, isTLS := c.(*tls.Conn); isTLS {
		c = tc.NetConn()
	}
	return func() { _ = c.Close() } // the stream ends either way
}

// Peer is who a connection came from, as the listener that admitted it
// says: the listener and, on a Unix socket, the peer's uid (SO_PEERCRED);
// over TCP, the remote address (and UID -1).
type Peer struct {
	Via  identity.Listener
	UID  int
	Addr netip.Addr
}

// Key is what every per-peer limit counts (connection and verification
// slots, the HTTPS failure throttle), one normalised key for them all:
// the uid on a socket; over TCP the address unmapped first, then grouped.
// An IPv4 address counts as itself. An IPv6 address counts by the /56 a
// site is commonly given, so a site cannot multiply its share by rotating
// addresses within it; a scoped (link-local) one by that /56 on its own
// interface, since a /56 drops the zone and would merge every link.
// 6to4 (2002::/16, deprecated by RFC 7526) is not folded to the IPv4
// address it embeds: a host on the daemon's link could claim any 2002:
// address through NDP and so share a chosen IPv4 client's key; it counts
// by its /56 like any IPv6 address. Nor are NAT64 (64:ff9b::/96) and
// Teredo (2001::/32) folded: every IPv4 client behind one translator, or
// one Teredo server, shares that translator's /56.
func (p Peer) Key() any {
	a := p.Addr.Unmap()
	switch {
	case !a.IsValid():
		return p.UID
	case a.Is4():
		return a
	}
	prefix := netip.PrefixFrom(a, 56).Masked()
	if zone := a.Zone(); zone != "" {
		return scopedKey{prefix, zone}
	}
	return prefix
}

// scopedKey is a link-local /56 on one interface.
type scopedKey struct {
	prefix netip.Prefix
	zone   string
}

type peerKey struct{}

// WithPeer records the connection's peer, for http.Server.ConnContext.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerOf is the peer WithPeer recorded; false when none was.
func PeerOf(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}
