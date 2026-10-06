// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

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
// says: the listener and, on a Unix socket, the peer's uid (SO_PEERCRED).
type Peer struct {
	Via identity.Listener
	UID int
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
