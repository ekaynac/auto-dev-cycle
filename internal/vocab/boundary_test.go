// SPDX-License-Identifier: Apache-2.0

package vocab_test

import (
	"encoding/json"
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

func bad[T ~string]() T { return "typo_generic" }

// Astra #24: reasons the scan cannot see (generic literals, []byte
// conversions, plain strings in the generated record types) are refused when
// their record is encoded or decoded.
func TestUnregisteredReasonsAreRefusedAtTheContractBoundary(t *testing.T) {
	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipt))
	if err != nil {
		t.Fatalf("a receipt with registered reasons: %v", err)
	}
	for name, reason := range map[string]types.ReasonCode{
		"plain string field": "typo_field",
		"generic":            bad[types.ReasonCode](),
		"from bytes":         types.ReasonCode([]byte("typo_bytes")),
	} {
		t.Run(name, func(t *testing.T) {
			x := r
			x.Predicates = append([]types.Predicate(nil), r.Predicates...)
			x.Predicates[0].Reason = reason
			if _, err := contract.Encode("records/receipt", x); err == nil || !strings.Contains(err.Error(), "unregistered reason") {
				t.Fatalf("encode with reason %q: %v", reason, err)
			}
			b, _ := json.Marshal(x)
			if _, err := contract.Decode[types.Receipt]("records/receipt", b); err == nil {
				t.Fatalf("decode with reason %q accepted", reason)
			}
		})
	}
}

// Astra #24: a Result is validated when it is marshalled, however it is
// marshalled; reading accepts every registered code.
func TestResultMarshalIsTheEmissionBoundary(t *testing.T) {
	if _, err := json.Marshal(vocab.Result{Outcome: vocab.Failed, Reason: vocab.Reason([]byte("typo_bytes")), Detail: "x"}); err == nil {
		t.Fatal("json.Marshal wrote a result with an unregistered reason")
	}
	if _, err := json.Marshal(vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonStaleAttempt}); err == nil {
		t.Fatal("json.Marshal wrote a failed result without detail")
	}
	b, err := json.Marshal(vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonStaleAttempt, Detail: "x"})
	if err != nil {
		t.Fatalf("a valid result: %v", err)
	}
	var back vocab.Result
	if err := json.Unmarshal(b, &back); err != nil || back.Reason != vocab.ReasonStaleAttempt {
		t.Fatalf("read back %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"outcome":"failed","reason":"typo","detail":"x"}`), &back); err == nil {
		t.Fatal("read a result with an unregistered reason")
	}
}

// Astra #24: a deprecated reason stays readable in a historical record but
// is refused when a record is written.
func TestDeprecatedReasonsAreReadButNotWritten(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	const code = "completed_clean"
	orig := reg.Reasons[code]
	dep := orig
	since := "0.2.0"
	dep.Deprecated = &since
	reg.Reasons[code] = dep
	t.Cleanup(func() { reg.Reasons[code] = orig })

	r, err := contract.Decode[types.Receipt]("records/receipt", []byte(receipt))
	if err != nil {
		t.Fatalf("a historical receipt with a deprecated reason must stay readable: %v", err)
	}
	if _, err := contract.Encode("records/receipt", r); err == nil || !strings.Contains(err.Error(), "deprecated") {
		t.Fatalf("encode with a deprecated reason: %v", err)
	}
	if _, err := json.Marshal(vocab.Result{Outcome: vocab.Empty, Reason: vocab.ReasonCompletedClean}); err == nil {
		t.Fatal("json.Marshal wrote a result with a deprecated reason")
	}
}
