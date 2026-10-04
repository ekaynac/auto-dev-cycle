// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const (
	rgID   = "rg_01m3250vkwffc979nzjw0g2sf9"
	leaID  = "lea_01m3250v57fhm9v1z9xd1wk8gw"
	untID  = "unt_01m3250vz2ezn81cgvbkcn17jd"
	exeID  = "exe_01m3250vnfeyrv67x6w0ddqr6a"
	tskID  = "tsk_01m3250v9xft08ssr4ygq6g2np"
	msrID  = "msr_01m3250vm7f6ktw39dszqnyddr"
	attNew = "att_01m3250v57ft7aehkqwq0yrq3n"
)

func clean() map[string]any {
	return map[string]any{"outcome": "empty", "reason": "completed_clean", "detail": ""}
}

// leaseDocs returns one valid document per lease-family record (05).
func leaseDocs() map[string]map[string]any {
	checks := map[string]any{}
	for _, k := range []string{"data_floor", "destinations", "capability", "independence", "paid_authority", "budget", "fallback_scope"} {
		checks[k] = clean()
	}
	return map[string]map[string]any{
		"records/resource-group": {"id": rgID, "kind": "account_quota",
			"capacity_model": map[string]any{"unit": "tokens", "limit": 1000000, "window": "PT5H", "reset_at": nil, "quota_state": "estimated"},
			"release_rule":   "on_window_reset", "members": []any{exeID}},
		"records/lease": {"id": leaID, "unit": untID, "role": "builder", "executor": exeID, "state": "active",
			"reservations": []any{map[string]any{"group": rgID, "amount": 200000, "unit": "tokens"}}, "priority": 10,
			"footprint": map[string]any{"context_tokens": 120000, "expected_turns": 40, "gpu_seconds": nil},
			"deadline":  "2026-10-04T14:00:00Z", "fallback_step": 0, "placement_checks": checks, "hold": nil, "attempt": attID, "restore_receipt": nil, "settlement": nil},
		"records/attempt": {"id": attID, "unit": untID, "lease": leaID, "fencing_token": 3, "supersedes": nil,
			"base": map[string]any{"tree": git, "contract_revision": "7"}, "status": "running", "publication": "open",
			"stop_reason": nil, "lost_reason": nil, "checkpoint": nil, "completion": nil, "superseded_by": nil},
		"records/attempt-record": {"attempt": attID, "project": prjID, "task": tskID, "contract_revision": "7", "fencing_token": 3,
			"base_tree": git, "partial_diff": dgA, "failing_tests": []any{"TestLeaseGrant"}, "trail": dgB, "builder_notes": nil,
			"evidence_manifest": dgA, "seal": map[string]any{"reason": "drain", "at": "2026-10-04T13:00:00Z", "sealed_by": actID}, "pinned": true},
		"records/usage-measurement": {"id": msrID, "call": calID, "attempt": attID, "lease": leaID, "kind": "delta", "sequence": 0,
			"tokens": map[string]any{"in": 5000, "out": 800, "cache_read": 40000, "cache_write": 0}, "turns": 1, "milliseconds": 9500,
			"cost": map[string]any{"amount_micros": 31500, "currency": "USD", "basis": "cash", "estimate": true}, "source_of_truth": "adapter_report"},
	}
}

func valid(t *testing.T, schema string, d map[string]any) error {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Validate(schema, b)
}

func TestLeaseRecords(t *testing.T) {
	for schema, doc := range leaseDocs() {
		if err := valid(t, schema, doc); err != nil {
			t.Errorf("%s: a valid document: %v", schema, err)
		}
	}
	// Other valid states: waiting with its hold, settled with its settlement,
	// an evidence target, a sealed attempt, a superseded one.
	waiting := leaseDocs()["records/lease"]
	waiting["state"], waiting["attempt"] = "waiting", nil
	waiting["hold"] = map[string]any{"hold": "quota_reset", "signal_instance": "2026-10-04T15:00:00Z"}
	settled := leaseDocs()["records/lease"]
	settled["state"] = "settled"
	settled["settlement"] = map[string]any{"calls": []any{calID}, "liabilities": []any{}, "published": nil}
	verified := leaseDocs()["records/lease"]
	verified["state"], verified["restore_receipt"] = "settling_verified", rcpID
	target := leaseDocs()["records/resource-group"]
	target["kind"], target["release_rule"] = "evidence_target", "on_restore_verified"
	target["capacity_model"] = map[string]any{"unit": "slots", "limit": 1, "window": nil, "reset_at": nil, "quota_state": "known"}
	failed := leaseDocs()["records/lease"]
	failed["state"] = "restore_failed"
	failed["hold"] = map[string]any{"hold": "evidence_restore_pending", "signal_instance": "tgt_01m3250v57fhm9v1z9xd1wk8gw"}
	sealedLost := leaseDocs()["records/attempt"]
	sealedLost["status"], sealedLost["publication"], sealedLost["lost_reason"], sealedLost["checkpoint"] = "sealed", "closed", "agent_disconnect", dgA
	blocked := leaseDocs()["records/lease"]
	blocked["placement_checks"].(map[string]any)["budget"] = map[string]any{"outcome": "could_not_run", "reason": "quota_unknown", "detail": "no probe yet"}
	sealed := leaseDocs()["records/attempt"]
	sealed["status"], sealed["publication"], sealed["stop_reason"], sealed["checkpoint"] = "sealed", "closed", "revoke", dgA
	superseded := leaseDocs()["records/attempt"]
	superseded["status"], superseded["publication"], superseded["checkpoint"], superseded["superseded_by"] = "superseded", "closed", dgA, attNew
	for name, c := range map[string]struct {
		schema string
		doc    map[string]any
	}{"waiting": {"records/lease", waiting}, "settled": {"records/lease", settled}, "restore verified": {"records/lease", verified}, "evidence target": {"records/resource-group", target},
		"sealed": {"records/attempt", sealed}, "superseded": {"records/attempt", superseded},
		"restore failed with its hold": {"records/lease", failed}, "sealed after a loss": {"records/attempt", sealedLost},
		"a blocking check with its detail": {"records/lease", blocked}} {
		if err := valid(t, c.schema, c.doc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	cases := map[string]struct {
		schema string
		mutate func(d map[string]any)
	}{
		"group kind":                   {"records/resource-group", func(d map[string]any) { d["kind"] = "wallet" }},
		"fractional limit":             {"records/resource-group", func(d map[string]any) { d["capacity_model"].(map[string]any)["limit"] = 1.5 }},
		"unknown quota with a limit":   {"records/resource-group", func(d map[string]any) { d["capacity_model"].(map[string]any)["quota_state"] = "unknown" }},
		"known quota without a limit":  {"records/resource-group", func(d map[string]any) { d["capacity_model"].(map[string]any)["limit"] = nil }},
		"target releasing on settle":   {"records/resource-group", func(d map[string]any) { d["kind"] = "evidence_target" }},
		"restore rule on a non-target": {"records/resource-group", func(d map[string]any) { d["release_rule"] = "on_restore_verified" }},
		"lease state":                  {"records/lease", func(d map[string]any) { d["state"] = "paused" }},
		"negative reservation": {"records/lease", func(d map[string]any) {
			d["reservations"] = []any{map[string]any{"group": rgID, "amount": -1, "unit": "tokens"}}
		}},
		"waiting without a hold": {"records/lease", func(d map[string]any) { d["state"], d["attempt"] = "waiting", nil }},
		"hold while active": {"records/lease", func(d map[string]any) {
			d["hold"] = map[string]any{"hold": "quota_reset", "signal_instance": "x"}
		}},
		"settled without a settlement": {"records/lease", func(d map[string]any) { d["state"] = "settled" }},
		"settlement before settled": {"records/lease", func(d map[string]any) {
			d["settlement"] = map[string]any{"calls": []any{}, "liabilities": []any{}, "published": nil}
		}},
		"verified without its receipt": {"records/lease", func(d map[string]any) { d["state"] = "settling_verified" }},
		"restore receipt while active": {"records/lease", func(d map[string]any) { d["restore_receipt"] = rcpID }},
		"active without an attempt":    {"records/lease", func(d map[string]any) { d["attempt"] = nil }},
		"granted with an attempt":      {"records/lease", func(d map[string]any) { d["state"] = "granted" }},
		"check outcome": {"records/lease", func(d map[string]any) {
			d["placement_checks"].(map[string]any)["budget"].(map[string]any)["outcome"] = "ok"
		}},
		"token zero":                      {"records/attempt", func(d map[string]any) { d["fencing_token"] = 0 }},
		"running with publication closed": {"records/attempt", func(d map[string]any) { d["publication"] = "closed" }},
		"stopping with publication open":  {"records/attempt", func(d map[string]any) { d["status"], d["stop_reason"] = "stopping", "revoke" }},
		"stopping without a reason":       {"records/attempt", func(d map[string]any) { d["status"], d["publication"] = "stopping", "closed" }},
		"lost without a reason":           {"records/attempt", func(d map[string]any) { d["status"], d["publication"] = "lost", "closed" }},
		"sealed without a checkpoint":     {"records/attempt", func(d map[string]any) { d["status"], d["publication"] = "sealed", "closed" }},
		"checkpoint while running":        {"records/attempt", func(d map[string]any) { d["checkpoint"] = dgA }},
		"completed without completion": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["checkpoint"] = "completed", "closed", dgA
		}},
		"superseded without its successor": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["checkpoint"] = "superseded", "closed", dgA
		}},
		"completion while running": {"records/attempt", func(d map[string]any) {
			d["completion"] = map[string]any{"outcome": "empty", "reason": "completed_clean"}
		}},
		"successor while running": {"records/attempt", func(d map[string]any) { d["superseded_by"] = attNew }},
		// Astra #47.
		"restore failed without its hold": {"records/lease", func(d map[string]any) { d["state"] = "restore_failed" }},
		"a blocking check without detail": {"records/lease", func(d map[string]any) {
			d["placement_checks"].(map[string]any)["budget"] = map[string]any{"outcome": "could_not_run", "reason": "quota_unknown", "detail": ""}
		}},
		"running with a stop reason": {"records/attempt", func(d map[string]any) { d["stop_reason"] = "revoke" }},
		"running with a lost reason": {"records/attempt", func(d map[string]any) { d["lost_reason"] = "stream_dead" }},
		"finishing with a reason": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["stop_reason"] = "finishing", "closed", "drain"
		}},
		"stopping and lost": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["stop_reason"], d["lost_reason"] = "stopping", "closed", "revoke", "stream_dead"
		}},
		"lost and stopped": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["lost_reason"], d["stop_reason"] = "lost", "closed", "stream_dead", "revoke"
		}},
		"sealed with both reasons": {"records/attempt", func(d map[string]any) {
			d["status"], d["publication"], d["checkpoint"], d["stop_reason"], d["lost_reason"] = "sealed", "closed", dgA, "revoke", "stream_dead"
		}},
		"seal reason":                          {"records/attempt-record", func(d map[string]any) { d["seal"].(map[string]any)["reason"] = "gave_up" }},
		"record without a trail":               {"records/attempt-record", func(d map[string]any) { delete(d, "trail") }},
		"usage kind":                           {"records/usage-measurement", func(d map[string]any) { d["kind"] = "session_total" }},
		"fractional cost":                      {"records/usage-measurement", func(d map[string]any) { d["cost"].(map[string]any)["amount_micros"] = 0.5 }},
		"currency":                             {"records/usage-measurement", func(d map[string]any) { d["cost"].(map[string]any)["currency"] = "usd" }},
		"reconciliation not from the provider": {"records/usage-measurement", func(d map[string]any) { d["kind"] = "provider_reconciliation" }},
	}
	for name, c := range cases {
		d := leaseDocs()[c.schema]
		c.mutate(d)
		if err := valid(t, c.schema, d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every lease-family record round-trips through the generated types unchanged.
func TestLeaseRoundTrip(t *testing.T) {
	for schema, doc := range leaseDocs() {
		in, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		switch schema {
		case "records/resource-group":
			v, err = contract.Decode[types.ResourceGroup](schema, in)
		case "records/lease":
			v, err = contract.Decode[types.Lease](schema, in)
		case "records/attempt":
			v, err = contract.Decode[types.Attempt](schema, in)
		case "records/attempt-record":
			v, err = contract.Decode[types.AttemptRecord](schema, in)
		case "records/usage-measurement":
			v, err = contract.Decode[types.UsageMeasurement](schema, in)
		}
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
