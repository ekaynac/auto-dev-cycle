// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Injector puts a destination's credential on an outbound request's
// headers. It is the per-destination injection point: a destination that
// names another header, or a governed upstream's trust secret (D48, M2 or
// later), gets its own; the default is Bearer on Authorization.
type Injector func(h http.Header, c Credential)

// Bearer is the default injector.
func Bearer(h http.Header, c Credential) { h.Set("Authorization", "Bearer "+c.Reveal()) }

var (
	errRedirect  = errors.New("gateway: the upstream redirected; redirects are refused")
	errOffOrigin = errors.New("gateway: a dial to an address other than the registered origin")
)

// originKey carries, in a request's context, the one address its dial may
// reach.
type originKey struct{}

// client is the egress client: no proxy, no redirect, and a dial only to
// the origin the request's context names (#226 entries 34 and 38).
func client() *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
		Transport: &http.Transport{
			Proxy:               nil, // never the environment's proxy: the origin is dialled directly
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if want, _ := ctx.Value(originKey{}).(string); want == "" || addr != want {
					return nil, errOffOrigin
				}
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// dialAddr is u's address as the transport dials it: the host as written,
// with the scheme's default port made explicit.
func dialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// route is the outbound URL for path at d: its endpoint with path appended,
// when d's transport is admitted.
func route(d types.RegistryDestination, path string) (*url.URL, bool) {
	u, err := url.Parse(d.Endpoint)
	if err != nil || !transportOK(d.Kind, u) {
		return nil, false
	}
	t := *u
	t.Path, t.RawPath = strings.TrimSuffix(u.Path, "/")+path, ""
	return &t, true
}

// transportOK is M1's transport decision (#226 entry 37), re-checked at
// egress: a direct API is https, or plain http to a loopback IP literal; a
// local endpoint's host is a loopback, RFC 1918 or fc00::/7 IP literal,
// over http or https. The data floor is about the recipient, not the
// transport; plain http off loopback becomes an opt-in once the gateway
// can pin a CA.
func transportOK(kind types.RegistryDestinationKind, u *url.URL) bool {
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	literal := err == nil && ip.Zone() == "" // netip's IsLoopback and IsPrivate read an IPv4-mapped address as IPv4
	switch kind {
	case types.RegistryDestinationKindDirectApi:
		return u.Scheme == "https" || (literal && ip.IsLoopback())
	case types.RegistryDestinationKindLocalEndpoint:
		return literal && (ip.IsLoopback() || ip.IsPrivate())
	}
	return false // brokers and governed upstreams are not M1's
}

// allowed are the only top-level keys a request may carry: the OpenAI
// chat-completions keys P13's http-chat adapter needs, matched exactly. A
// denylist of model-selecting keys could never be complete (a router's
// provider or transforms, a vendor passthrough), so every other key is
// refused, a case variant included. P13 widens this list only with review.
var allowed = map[string]bool{
	"model": true, "messages": true, "temperature": true, "top_p": true, "max_tokens": true,
	"max_completion_tokens": true, "stream": true, "stream_options": true, "stop": true, "n": true,
	"seed": true, "presence_penalty": true, "frequency_penalty": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "response_format": true, "user": true, "logprobs": true, "top_logprobs": true,
}

// pinned is body with its model checked: body is one JSON object of
// allowed keys whose "model" is exactly the lease's pinned model. It is
// forwarded re-encoded from its decoded object, so a duplicated key cannot
// show the gateway one model and the upstream another. The model is never
// rewritten.
func pinned(body []byte, model string) ([]byte, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top == nil {
		return nil, false
	}
	for k := range top {
		if !allowed[k] {
			return nil, false
		}
	}
	var named string
	if raw, ok := top["model"]; !ok || json.Unmarshal(raw, &named) != nil || named != model {
		return nil, false
	}
	out, err := json.Marshal(top)
	return out, err == nil
}
