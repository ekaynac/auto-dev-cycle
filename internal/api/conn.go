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

// Key is what per-peer quotas count: the uid on a socket; over TCP the
// remote IPv4 address, or the IPv6 /64 a host is commonly given, so one
// host cannot multiply its share by rotating addresses within it.
func (p Peer) Key() any {
	switch a := p.Addr.Unmap(); {
	case !a.IsValid():
		return p.UID
	case a.Is6():
		return netip.PrefixFrom(a, 64).Masked()
	default:
		return a
	}
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
