// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// The stream never carries credential material (token hashes, secrets,
// challenge nonces). No schema marks secret fields yet, so this is an
// explicit allow-list per event kind: the payload paths the stream may
// carry. A kind with no entry streams with its payload withheld; adding a
// kind to the stream is adding it here, after checking its schema.
var streamable = map[string][]string{}

func init() {
	identityPaths := []string{"transition",
		"actor.id", "actor.kind", "actor.name", "actor.declared_agent", "actor.created_by", "actor.revoked_at",
		"token.id", "token.actor", "token.created_by", "token.created_at", "token.revoked_at"} // never token.hash
	for k := range identity.Kinds() {
		streamable[k] = identityPaths
	}
	for k := range authority.GrantKinds() {
		streamable[k] = []string{"transition", "grant", "revoked_at", "replaces"} // a grant record holds no secret
	}
}

// streamed is an event as the stream carries it.
type streamed struct {
	eventstore.Event
	PayloadWithheld bool `json:"payload_withheld"`
}

// redact keeps only the allowed paths of e's payload, or withholds it.
func redact(e eventstore.Event) streamed {
	paths, ok := streamable[e.Kind]
	var doc map[string]any
	if !ok || json.Unmarshal(e.Payload, &doc) != nil {
		e.Payload = json.RawMessage(`null`)
		return streamed{Event: e, PayloadWithheld: true}
	}
	b, err := json.Marshal(pick(doc, paths))
	if err != nil {
		b = []byte(`null`)
	}
	e.Payload = b
	return streamed{Event: e, PayloadWithheld: err != nil}
}

// pick copies from src only the dotted paths; a null on the way is kept as
// null, anything else that is not an object is dropped.
func pick(src map[string]any, paths []string) map[string]any {
	out := map[string]any{}
	for _, p := range paths {
		head, rest, nested := strings.Cut(p, ".")
		v, ok := src[head]
		switch {
		case !ok:
		case !nested:
			out[head] = v
		case v == nil:
			if _, set := out[head]; !set {
				out[head] = nil
			}
		default:
			m, isObject := v.(map[string]any)
			if !isObject {
				continue
			}
			sub, _ := out[head].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
			}
			for k, x := range pick(m, []string{rest}) {
				sub[k] = x
			}
			out[head] = sub
		}
	}
	return out
}
