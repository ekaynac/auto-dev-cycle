// SPDX-License-Identifier: Apache-2.0

package vocab_test

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const receipt = `{"id":"rcp_01m3250v57fhm9v1z9xd1wk8gw","kind":"gate","instrument":{"name":"go-test","version":"1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
	`"subject":{"type":"call","call":"cal_01m3250v9xft08ssr4ygq6g2np","demand":null},` +
	`"provenance":{"command":"cmd_01m3250vz2ezn81cgvbkcn17jd","attempt":null,"calls":[],"model_families":[],"contributors":[]},` +
	`"produced_at":"2026-10-04T10:00:00Z","valid_until":null,` +
	`"predicates":[{"name":"tests","outcome":"empty","reason":"completed_clean","detail":"","evidence":[]}],` +
	`"outcome":"empty","reason":"completed_clean","detail":"","verdict":null,` +
	`"conclusions_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","supersedes":null}`

// Every way of serialising a record checks its reasons: an unregistered
// reason (here built with the unchecked constructor, as only a test may) is
// refused through contract.Encode, plain encoding/json, an interface value,
// a containing struct and encoding/json/v2 alike.
func TestRecordReasonsAreCheckedHoweverSerialised(t *testing.T) {
	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipt))
	if err != nil {
		t.Fatalf("a receipt with registered reasons: %v", err)
	}
	if _, err := contract.Encode("records/receipt", r); err != nil {
		t.Fatalf("encode a valid receipt: %v", err)
	}
	r.Predicates[0].Reason = types.ReasonCodeOf("typo_unchecked")
	var asAny any = r
	var buf bytes.Buffer
	for name, write := range map[string]func() error{
		"contract.Encode":  func() error { _, err := contract.Encode("records/receipt", r); return err },
		"json.Marshal":     func() error { _, err := json.Marshal(r); return err },
		"interface value":  func() error { _, err := json.Marshal(asAny); return err },
		"containing value": func() error { _, err := json.Marshal(struct{ R types.Receipt }{r}); return err },
		"json/v2 writer":   func() error { return jsonv2.MarshalWrite(&buf, r) },
		"a lone predicate": func() error { _, err := json.Marshal(r.Predicates[0]); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); err == nil || !strings.Contains(err.Error(), "unregistered reason") {
				t.Fatalf("serialised an unregistered reason: %v", err)
			}
		})
	}
	bad := strings.Replace(receipt, `"reason":"completed_clean","detail":"","evidence"`, `"reason":"typo","detail":"","evidence"`, 1)
	if _, err := contract.Decode[types.Receipt]("records/receipt", []byte(bad)); err == nil {
		t.Fatal("decoded a record with an unregistered reason")
	}
	var direct types.Receipt
	if err := json.Unmarshal([]byte(bad), &direct); err == nil {
		t.Fatal("plain json.Unmarshal read an unregistered reason")
	}
}

// A deprecated reason stays readable in a historical record and result, and
// is refused whenever one is written.
func TestDeprecatedReasonsAreReadButNotWritten(t *testing.T) {
	const code = "completed_clean"
	t.Cleanup(vocab.DeprecateForTest(code, "0.2.0"))

	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipt))
	if err != nil {
		t.Fatalf("a historical receipt with a deprecated reason must stay readable: %v", err)
	}
	if _, err := contract.Encode("records/receipt", r); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("encode with a deprecated reason: %v", err)
	}
	if _, err := json.Marshal(r); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("json.Marshal with a deprecated reason: %v", err)
	}
	// An untyped document is checked by the contract's writer schemas.
	var doc map[string]any
	if err := json.Unmarshal([]byte(receipt), &doc); err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate("records/receipt", []byte(receipt)); err != nil {
		t.Fatalf("validating (reading) a historical receipt: %v", err)
	}
	if _, err := contract.Encode("records/receipt", doc); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("encode of an untyped document with a deprecated reason: %v", err)
	}
	if _, err := vocab.ParseReason(code); err != nil {
		t.Fatalf("ParseReason refused a readable deprecated code: %v", err)
	}
	var historical vocab.Result
	if err := json.Unmarshal([]byte(`{"outcome":"empty","reason":"completed_clean","detail":""}`), &historical); err != nil {
		t.Fatalf("a historical result with a deprecated reason must stay readable: %v", err)
	}
	if _, err := json.Marshal(vocab.Result{Outcome: vocab.Empty, Reason: mustReason(t, "completed_clean")}); err == nil {
		t.Fatal("json.Marshal wrote a result with a deprecated reason")
	}
}

// A Result is checked when it is marshalled: a registered reason allowed
// with the outcome and a detail for every non-empty outcome.
func TestResultMarshalIsTheEmissionBoundary(t *testing.T) {
	for name, res := range map[string]vocab.Result{
		"unregistered reason":   {Outcome: vocab.Failed, Reason: types.ReasonCodeOf("typo"), Detail: "x"},
		"zero reason":           {Outcome: vocab.Failed, Detail: "x"},
		"wrong outcome":         {Outcome: vocab.Empty, Reason: mustReason(t, "quota_exhausted")},
		"failed without detail": {Outcome: vocab.Failed, Reason: mustReason(t, "stale_attempt")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := json.Marshal(res); err == nil {
				t.Fatalf("json.Marshal wrote %+v", res)
			}
		})
	}
	b, err := json.Marshal(vocab.Result{Outcome: vocab.Failed, Reason: mustReason(t, "stale_attempt"), Detail: "x"})
	if err != nil {
		t.Fatalf("a valid result: %v", err)
	}
	var back vocab.Result
	if err := json.Unmarshal(b, &back); err != nil || back.Reason != mustReason(t, "stale_attempt") {
		t.Fatalf("read back %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"outcome":"failed","reason":"typo","detail":"x"}`), &back); err == nil {
		t.Fatal("read a result with an unregistered reason")
	}
	if _, err := vocab.ParseReason("typo"); err == nil {
		t.Fatal("ParseReason accepted an unregistered code")
	}
}

// Astra #28: the hooks are installed once, by the vocabulary package only;
// a replacement attempt fails and checking continues.
func TestReasonHooksCannotBeReplaced(t *testing.T) {
	allow := func(string, bool) error { return nil }
	if err := types.SetReasonCheck(allow); err == nil {
		t.Fatal("the generated types' reason check was replaced")
	}
	if err := contract.SetReasonCheck(allow); err == nil {
		t.Fatal("the contract runtime's reason check was replaced")
	}
	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipt))
	if err != nil {
		t.Fatal(err)
	}
	r.Reason = types.ReasonCodeOf("typo_override")
	if _, err := contract.Encode("records/receipt", r); err == nil {
		t.Fatal("an unregistered reason was written after a replacement attempt")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(receipt), &doc); err != nil {
		t.Fatal(err)
	}
	doc["reason"] = "typo_override"
	if _, err := contract.Encode("records/receipt", doc); err == nil {
		t.Fatal("an untyped document with an unregistered reason was written")
	}
}

// Astra #28: null and missing reasons are refused on read.
func TestNullAndMissingReasonsAreRefused(t *testing.T) {
	var res vocab.Result
	for _, doc := range []string{`{"outcome":"empty","detail":""}`, `{"outcome":"empty","reason":null,"detail":""}`} {
		if err := json.Unmarshal([]byte(doc), &res); err == nil {
			t.Errorf("read %s", doc)
		}
	}
	var code types.ReasonCode
	if err := json.Unmarshal([]byte(`null`), &code); err == nil {
		t.Error("read a null reason code")
	}
	nullPredicate := strings.Replace(receipt, `"reason":"completed_clean","detail":"","evidence"`, `"reason":null,"detail":"","evidence"`, 1)
	var direct types.Receipt
	if err := json.Unmarshal([]byte(nullPredicate), &direct); err == nil {
		t.Error("plain json.Unmarshal read a null predicate reason")
	}
}

// Astra #28 round 2: Load returns a copy; changing it changes no decision.
func TestLoadedRegistryIsACopy(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := reg.Reasons["completed_clean"]
	entry.Code = "typo_mutated_registry"
	reg.Reasons["typo_mutated_registry"] = entry
	if _, err := vocab.ParseReason("typo_mutated_registry"); err == nil {
		t.Fatal("adding to a loaded registry registered a reason")
	}
	since := types.Semver("0.2.0")
	dep := reg.Reasons["stale_attempt"]
	dep.Deprecated = &since
	reg.Reasons["stale_attempt"] = dep
	if _, err := json.Marshal(mustReason(t, "stale_attempt")); err != nil {
		t.Fatalf("deprecating in a loaded registry changed the writer check: %v", err)
	}
	if x := reg.Reasons["completed_clean"]; len(x.Outcomes) > 0 {
		x.Outcomes[0] = "failed"
		again, err := vocab.Load()
		if err != nil {
			t.Fatal(err)
		}
		if again.Reasons["completed_clean"].Outcomes[0] == "failed" {
			t.Fatal("a nested slice is shared between copies")
		}
	}
}

// mustReason parses a registered reason code.
func mustReason(t *testing.T, code string) vocab.Reason {
	t.Helper()
	r, err := vocab.ParseReason(code)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
