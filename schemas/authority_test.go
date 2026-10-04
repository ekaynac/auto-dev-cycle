// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const (
	grtID = "grt_01m3250vkwffc979nzjw0g2sf9"
	aprID = "apr_01m3250v57fhm9v1z9xd1wk8gw"
	chlID = "chl_01m3250vz2ezn81cgvbkcn17jd"
	ownID = "own_01m3250vnfeyrv67x6w0ddqr6a"
	evtID = "evt_01m3250v9xft08ssr4ygq6g2np"
	opID  = "act_01m3250vm7f6ktw39dszqnyddr"
)

func object() map[string]any {
	return map[string]any{"type": "profile", "id": prjID, "version": 4, "digest": dgA}
}

// authorityDocs returns one valid document per authority record (04).
func authorityDocs() map[string]map[string]any {
	return map[string]map[string]any{
		"records/actor": {"id": actID, "kind": "supervisor", "name": "nightly supervisor", "declared_agent": "claude-code",
			"created_by": opID, "revoked_at": nil},
		"records/grant": {"id": grtID, "actor": actID, "projects": []any{prjID}, "actions": []any{"task.create", "review.request"},
			"reads":      map[string]any{"projects": "same", "kinds": []any{"task", "receipt"}},
			"valid_from": "2026-10-04T00:00:00Z", "expires_at": "2026-10-11T00:00:00Z",
			"confirmation_overrides": map[string]any{"task.create": "independent_review"}, "issued_by": opID, "approval": aprID},
		"records/approval": {"id": aprID, "actor": opID, "actor_kind": "operator", "action": "profile.adopt",
			"change_class": []any{"policy.widen_destinations"}, "project": prjID, "object": object(),
			"expected_state": map[string]any{"version": 4, "digest": nil}, "scope": map[string]any{"revision": 5},
			"grant": nil, "confirmation": map[string]any{"policy": "human", "challenge": chlID, "satisfied_by": []any{evtID}},
			"answers": []any{}, "supersedes": []any{}, "question": "Adopt profile revision 5?", "answer_verbatim": "yes",
			"reason": "adds the staging destination", "at": "2026-10-04T10:00:00Z"},
		"records/proposal": {"id": prpID, "action": "profile.adopt", "change_class": []any{"policy.widen_destinations"},
			"project": prjID, "object": object(), "expected_state": map[string]any{"version": 4, "digest": nil},
			"scope": map[string]any{"revision": 5}, "proposed_by": actID, "policy": "human", "binding_digest": dgB,
			"status": "open", "expires_at": "2026-10-05T10:00:00Z"},
		"records/challenge": {"id": chlID, "proposal": prpID, "binding_digest": dgB, "nonce": strings.Repeat("ab", 32),
			"allowed_confirmers": []any{opID}, "expires_at": "2026-10-04T10:15:00Z", "used_at": nil},
		"records/owed-input": {"id": ownID, "requested_by": actID, "kind": "ruling", "question": "Is the vendor licence compatible?",
			"subject": object(), "revision": 1},
	}
}

func TestAuthorityRecords(t *testing.T) {
	for schema, doc := range authorityDocs() {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.Validate(schema, b); err != nil {
			t.Errorf("%s: a valid document: %v", schema, err)
		}
	}
	// Every-project grants and listed read projects are accepted too.
	g := authorityDocs()["records/grant"]
	g["projects"] = "*"
	g["reads"] = map[string]any{"projects": []any{prjID}, "kinds": []any{}}
	if b, _ := json.Marshal(g); contract.Validate("records/grant", b) != nil {
		t.Error("an every-project grant with listed read projects")
	}

	cases := map[string]struct {
		schema string
		mutate func(d map[string]any)
	}{
		"actor kind":              {"records/actor", func(d map[string]any) { d["kind"] = "robot" }},
		"actor unknown key":       {"records/actor", func(d map[string]any) { d["token"] = "secret" }},
		"actor absent revoked_at": {"records/actor", func(d map[string]any) { delete(d, "revoked_at") }},
		"grant without expiry":    {"records/grant", func(d map[string]any) { delete(d, "expires_at") }},
		"grant loosening override": {"records/grant", func(d map[string]any) {
			d["confirmation_overrides"] = map[string]any{"task.create": "none"}
		}},
		"grant bad action":       {"records/grant", func(d map[string]any) { d["actions"] = []any{"Delete Everything"} }},
		"grant bad project list": {"records/grant", func(d map[string]any) { d["projects"] = "all" }},
		"grant read projects":    {"records/grant", func(d map[string]any) { d["reads"].(map[string]any)["projects"] = "everything" }},
		"approval supplied class": {"records/approval", func(d map[string]any) {
			d["change_class"] = []any{"policy.anything"}
		}},
		"approval policy":         {"records/approval", func(d map[string]any) { d["confirmation"].(map[string]any)["policy"] = "maybe" }},
		"approval without reason": {"records/approval", func(d map[string]any) { d["reason"] = "" }},
		"approval object":         {"records/approval", func(d map[string]any) { d["object"].(map[string]any)["version"] = -1 }},
		"proposal policy none":    {"records/proposal", func(d map[string]any) { d["policy"] = "none" }},
		"proposal status":         {"records/proposal", func(d map[string]any) { d["status"] = "applied" }},
		"challenge short nonce":   {"records/challenge", func(d map[string]any) { d["nonce"] = "abcd" }},
		"challenge no confirmer":  {"records/challenge", func(d map[string]any) { d["allowed_confirmers"] = []any{} }},
		"owed input kind":         {"records/owed-input", func(d map[string]any) { d["kind"] = "opinion" }},
		"owed input revision":     {"records/owed-input", func(d map[string]any) { d["revision"] = 0 }},
	}
	for name, c := range cases {
		d := authorityDocs()[c.schema]
		c.mutate(d)
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.Validate(c.schema, b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every authority record round-trips through the generated types unchanged.
func TestAuthorityRoundTrip(t *testing.T) {
	for schema, doc := range authorityDocs() {
		in, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		v, err := decodeAny(schema, in)
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		out, err := contract.Encode(schema, v)
		if err != nil {
			t.Fatalf("%s: %v", schema, err)
		}
		var a, b any
		if err := json.Unmarshal(in, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: round trip changed the document:\nin  %s\nout %s", schema, in, out)
		}
	}
}
