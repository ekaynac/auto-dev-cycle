// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const effID = "eff_01m3250v03fpq92bcrrpmkpsj9"

func commandRequest() map[string]any {
	return map[string]any{"idempotency_key": "grant-extend-2026-10-05-1", "expected": map[string]any{"version": 12, "digest": nil},
		"scope": map[string]any{"grant": grtID}, "reason": "the review runs past the expiry"}
}

func commandResponses() map[string]map[string]any {
	return map[string]map[string]any{
		"applied": {"disposition": "applied", "command": cmdID, "replayed": false, "approval": aprID, "first_seq": 41, "last_seq": 42,
			"effects": []any{map[string]any{"id": effID, "state": "uncertain"}}, "result": map[string]any{}},
		"proposed": {"disposition": "proposed", "proposal": prpID, "policy": "human", "binding_digest": dgA},
		"refused":  {"disposition": "refused", "outcome": "failed", "reason": "stale_expected_state", "detail": "version 12, current 13"},
		"applied with nothing to show": {"disposition": "applied", "command": cmdID, "replayed": true, "approval": nil, "first_seq": 0, "last_seq": 0,
			"effects": []any{}, "result": map[string]any{}},
		"proposed for review": {"disposition": "proposed", "proposal": prpID, "policy": "independent_review", "binding_digest": dgA},
		"could not run":       {"disposition": "refused", "outcome": "could_not_run", "reason": "ambiguous_state", "detail": "two projections disagree"},
	}
}

func with(d map[string]any, k string, v any) map[string]any {
	c := maps.Clone(d)
	c[k] = v
	return c
}

func without(d map[string]any, k string) map[string]any {
	c := maps.Clone(d)
	delete(c, k)
	return c
}

func TestCommandRequest(t *testing.T) {
	const schema = "api/command-request"
	if err := valid(t, schema, commandRequest()); err != nil {
		t.Fatal(err)
	}
	if err := valid(t, schema, with(commandRequest(), "expected", map[string]any{"version": 0, "digest": nil})); err != nil {
		t.Errorf("an object that does not exist yet: %v", err)
	}
	for name, d := range map[string]map[string]any{
		"no expected revision":      with(commandRequest(), "expected", map[string]any{"version": nil, "digest": nil}),
		"no expected field":         without(commandRequest(), "expected"),
		"an empty key":              with(commandRequest(), "idempotency_key", ""),
		"a key with a space":        with(commandRequest(), "idempotency_key", "a b"),
		"a key over 256 characters": with(commandRequest(), "idempotency_key", strings.Repeat("k", 257)),
		"no reason":                 with(commandRequest(), "reason", ""),
		"the action in the body":    with(commandRequest(), "action", "grant.extend"),
		"a scope that is no object": with(commandRequest(), "scope", "grant"),
	} {
		if valid(t, schema, d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCommandResponse(t *testing.T) {
	const schema = "api/command-response"
	for name, d := range commandResponses() {
		if err := valid(t, schema, d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// Each document decodes to its variant and encodes back to the same JSON.
		b, _ := json.Marshal(d)
		u, err := contract.Decode[types.CommandResponse](schema, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var variant bool
		switch d["disposition"] {
		case "applied":
			_, variant = u.Variant.(*types.CommandApplied)
		case "proposed":
			_, variant = u.Variant.(*types.CommandProposed)
		case "refused":
			_, variant = u.Variant.(*types.CommandRefused)
		}
		if !variant {
			t.Errorf("%s: decoded as %T", name, u.Variant)
		}
		out, err := contract.Encode(schema, u)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var got, want any
		if json.Unmarshal(out, &got) != nil || json.Unmarshal(b, &want) != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: re-encoded as %s", name, out)
		}
	}
	r := commandResponses()
	for name, d := range map[string]map[string]any{
		"a refusal with a found outcome":      with(r["refused"], "outcome", "found"),
		"an unregistered reason":              with(r["refused"], "reason", "no_such_reason"),
		"a proposal needing no confirmation":  with(r["proposed"], "policy", "none"),
		"applied fields on a refusal":         with(r["refused"], "command", cmdID),
		"an applied response with no effects": without(r["applied"], "effects"),
		"an unknown effect state":             with(r["applied"], "effects", []any{map[string]any{"id": effID, "state": "done"}}),
		"no disposition":                      without(r["proposed"], "disposition"),
		"an unknown disposition":              with(r["proposed"], "disposition", "deferred"),
		"a refusal with no detail":            with(r["refused"], "detail", ""),
	} {
		if valid(t, schema, d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadResponses(t *testing.T) {
	list := map[string]any{"resource": "grants", "outcome": "found", "items": []any{map[string]any{"id": grtID}}}
	who := map[string]any{"actor": actID, "kind": "supervisor", "token": "tok_01m3250vkwffc979nzjw0g2sf9", "listener": "shared_socket",
		"reads": map[string]any{"everything": false, "grants": []any{map[string]any{"grant": grtID, "projects": []any{prjID}, "kinds": []any{"grant"}}}}}
	for schema, d := range map[string]map[string]any{"api/read-list": list, "api/whoami": who} {
		if err := valid(t, schema, d); err != nil {
			t.Errorf("%s: %v", schema, err)
		}
	}
	for name, c := range map[string]struct {
		schema string
		d      map[string]any
	}{
		"a failed read":           {"api/read-list", with(list, "outcome", "failed")},
		"a resource path":         {"api/read-list", with(list, "resource", "v1/grants")},
		"no items":                {"api/read-list", without(list, "items")},
		"a scalar item":           {"api/read-list", with(list, "items", []any{"x"})},
		"the system actor":        {"api/whoami", with(who, "kind", "system")},
		"a token hash":            {"api/whoami", with(who, "token_hash", "$argon2id$")},
		"an unknown listener":     {"api/whoami", with(who, "listener", "tcp")},
		"reads without a summary": {"api/whoami", with(who, "reads", map[string]any{"everything": true})},
	} {
		if valid(t, c.schema, c.d) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The seat actions' scopes are empty: the seat, its holder and force_after
// are the controller's, never the caller's.
func TestSeatScopes(t *testing.T) {
	for _, schema := range []string{"api/scope/seat-take", "api/scope/seat-release"} {
		if err := valid(t, schema, map[string]any{}); err != nil {
			t.Errorf("%s: the empty scope: %v", schema, err)
		}
		for _, field := range []string{"force_after", "to", "from", "seat"} {
			if valid(t, schema, map[string]any{field: "x"}) == nil {
				t.Errorf("%s: a scope with %s", schema, field)
			}
		}
	}
}
