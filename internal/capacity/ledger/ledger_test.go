// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	calA   = types.CallID("cal_01m3250v9xft08ssr4ygq6g2np")
	calB   = types.CallID("cal_01m3250v57fhm9v1z9xd1wk8gw")
	attOld = types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6a")
	attNew = types.AttemptID("att_01m3250v57ft7aehkqwq0yrq3n")
)

// report is a usage report of tokens in, with cost and time proportional.
func report(call types.CallID, kind types.UsageMeasurementKind, seq int64, in int64) types.UsageMeasurement {
	src := types.UsageMeasurementSourceOfTruthAdapterReport
	if kind == types.UsageMeasurementKindProviderReconciliation {
		src = types.UsageMeasurementSourceOfTruthProviderUsage
	}
	return types.UsageMeasurement{ID: "msr_01m3250vm7f6ktw39dszqnyddr", Call: call, Attempt: attOld, Lease: "lea_01m3250v57fhm9v1z9xd1wk8gw",
		Kind: kind, Sequence: seq, Tokens: types.UsageMeasurementTokens{In: types.Amount(in), Out: types.Amount(in / 10)},
		Turns: 1, Milliseconds: types.Amount(in * 2),
		Cost:          types.UsageMeasurementCost{AmountMicros: types.Amount(in * 3), Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: src}
}

const (
	delta = types.UsageMeasurementKindDelta
	final = types.UsageMeasurementKindFinal
	recon = types.UsageMeasurementKindProviderReconciliation
)

func reconcile(t *testing.T, ms ...types.UsageMeasurement) map[types.CallID]Call {
	t.Helper()
	for _, m := range ms {
		if _, err := contract.Encode("records/usage-measurement", m); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	got, err := Reconcile(ms)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 05: duplicate and late deltas never double count; deltas alone stay an
// uncertain liability at their sum.
func TestDeltas(t *testing.T) {
	got := reconcile(t, report(calA, delta, 0, 100), report(calA, delta, 1, 50), report(calA, delta, 0, 100), report(calB, delta, 0, 7))
	a := got[calA]
	if a.Usage.In != 150 || a.Usage.Turns != 2 || a.Usage.CostMicros != 450 || !a.Uncertain || a.Inconsistent {
		t.Fatalf("call A: %+v", a)
	}
	if got[calB].Usage.In != 7 || len(got) != 2 {
		t.Fatalf("calls: %+v", got)
	}
	// The same delta with two figures keeps the larger and is flagged.
	c := reconcile(t, report(calA, delta, 0, 100), report(calA, delta, 0, 90))[calA]
	if c.Usage.In != 100 || !c.Inconsistent || c.Reason != vocab.ReasonInconsistentUsage {
		t.Fatalf("a conflicting duplicate: %+v", c)
	}
}

// 05: a final replaces the deltas' sum when not lower; a lower final does
// not lower usage without reconciliation; only a reconciliation lowers it.
func TestFinalAndReconciliation(t *testing.T) {
	up := reconcile(t, report(calA, delta, 0, 100), report(calA, final, 0, 130))[calA]
	if up.Usage.In != 130 || up.Uncertain || up.Inconsistent {
		t.Fatalf("a larger final: %+v", up)
	}
	down := reconcile(t, report(calA, delta, 0, 100), report(calA, delta, 1, 50), report(calA, final, 0, 120))[calA]
	if down.Usage.In != 150 || down.Uncertain || !down.Inconsistent || down.Reason != vocab.ReasonInconsistentUsage {
		t.Fatalf("a lower final: %+v", down)
	}
	fixed := reconcile(t, report(calA, delta, 0, 100), report(calA, delta, 1, 50), report(calA, final, 0, 120), report(calA, recon, 0, 110))[calA]
	if fixed.Usage.In != 110 || fixed.Uncertain || fixed.Inconsistent || fixed.Reason != (vocab.Reason{}) {
		t.Fatalf("a reconciliation: %+v", fixed)
	}
	later := reconcile(t, report(calA, final, 0, 100), report(calA, final, 1, 140))[calA]
	if later.Usage.In != 140 || later.Inconsistent {
		t.Fatalf("a later, larger final: %+v", later)
	}
	// Astra #49: a later final never lowers usage, in either arrival order.
	for _, order := range [][]types.UsageMeasurement{
		{report(calA, final, 0, 140), report(calA, final, 1, 100)},
		{report(calA, final, 1, 100), report(calA, final, 0, 140)},
	} {
		got := reconcile(t, order...)[calA]
		if got.Usage.In != 140 || !got.Inconsistent || got.Reason != vocab.ReasonInconsistentUsage {
			t.Fatalf("a decreasing final: %+v", got)
		}
	}
	mixedUp, mixedDown := report(calA, final, 0, 100), report(calA, final, 1, 100)
	mixedDown.Tokens.In, mixedDown.Tokens.Out = 90, 50 // in lower, out higher
	mixed := reconcile(t, mixedUp, mixedDown)[calA]
	if mixed.Usage.In != 100 || mixed.Usage.Out != 50 || !mixed.Inconsistent {
		t.Fatalf("a final lower in one field: %+v", mixed)
	}
	clash := reconcile(t, report(calA, final, 2, 100), report(calA, final, 2, 140))[calA]
	if clash.Usage.In != 140 || !clash.Inconsistent {
		t.Fatalf("two finals with one sequence: %+v", clash)
	}
	stale := reconcile(t, report(calA, recon, 1, 90), report(calA, recon, 0, 500))[calA]
	if stale.Usage.In != 90 {
		t.Fatalf("an earlier reconciliation won: %+v", stale)
	}
}

// 05: a stale attempt's usage is still counted; usage is per call.
func TestStaleAttemptsCount(t *testing.T) {
	old := report(calA, delta, 0, 100)
	cur := report(calA, delta, 1, 40)
	cur.Attempt = attNew
	if got := reconcile(t, old, cur)[calA]; got.Usage.In != 140 {
		t.Fatalf("a stale attempt's usage was dropped: %+v", got)
	}
}

// The result never depends on arrival order.
func TestOrderIndependent(t *testing.T) {
	ms := []types.UsageMeasurement{report(calA, delta, 0, 100), report(calA, delta, 1, 50), report(calA, delta, 0, 90),
		report(calA, final, 0, 120), report(calA, final, 1, 70), report(calA, final, 0, 300),
		report(calB, delta, 0, 5), report(calB, recon, 3, 4), report(calB, recon, 3, 6), report(calB, recon, 1, 900)}
	want := reconcile(t, ms...)
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		p := append([]types.UsageMeasurement(nil), ms...)
		r.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
		got := reconcile(t, p...)
		for call, w := range want {
			if got[call] != w {
				t.Fatalf("order %d: %s = %+v, want %+v", i, call, got[call], w)
			}
		}
	}
}

func TestRefusals(t *testing.T) {
	eur := report(calA, delta, 1, 5)
	eur.Cost.Currency = "EUR"
	if _, err := Reconcile([]types.UsageMeasurement{report(calA, delta, 0, 5), eur}); err == nil || !strings.Contains(err.Error(), "EUR") {
		t.Errorf("two currencies for one call: %v", err)
	}
	odd := report(calA, delta, 0, 5)
	odd.Kind = "session_total"
	if _, err := Reconcile([]types.UsageMeasurement{odd}); err == nil {
		t.Error("an unknown kind")
	}
	big := report(calA, delta, 0, 0)
	big.Tokens.In = maxAmount
	if _, err := Reconcile([]types.UsageMeasurement{big, report(calA, delta, 1, 1)}); err == nil {
		t.Error("an overflowing sum")
	}
}
