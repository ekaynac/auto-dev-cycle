// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
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
