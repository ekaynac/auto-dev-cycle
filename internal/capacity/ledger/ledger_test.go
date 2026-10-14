// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"math/rand"
	"slices"
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

// counted is the gateway's count of one request of call: in tokens of
// input, out of output.
func counted(call types.CallID, seq, in, out int64) types.UsageMeasurement {
	m := report(call, delta, seq, in)
	m.Tokens.Out, m.Turns, m.Milliseconds, m.Cost.AmountMicros = types.Amount(out), 0, 0, 0
	m.SourceOfTruth = types.UsageMeasurementSourceOfTruthGatewayCount
	return m
}

// floor is the gateway's floor for a request whose usage it did not read.
func floor(call types.CallID, seq, in int64) types.UsageMeasurement {
	m := counted(call, seq, in, 0)
	m.SourceOfTruth = types.UsageMeasurementSourceOfTruthGatewayFloor
	return m
}

// withOut is m with out output tokens.
func withOut(m types.UsageMeasurement, out int64) types.UsageMeasurement {
	m.Tokens.Out = types.Amount(out)
	return m
}

func standing(t *testing.T, ms ...types.UsageMeasurement) Standing {
	t.Helper()
	reconcile(t, ms...) // schema-valid, and reconcilable
	return StandingOf(ms, calA)
}

// P11b (#226 entry 69): a final settles a call only when it is not below
// the gateway's own count (input and cache tokens summed, output apart);
// a final with no count, under it, or with a floor among the counts (usage
// the gateway did not read), leaves the call uncertain at the
// larger of the reports and the count. A provider reconciliation settles
// it whatever the floors, but never below the observed count (P20; see
// TestAReconciliationNeverSettlesBelowTheCounts). The count never enters Reconcile's usage, and
// counts replayed under one sequence are one count.
func TestAFinalSettlesOnlyOnTheGatewaysCount(t *testing.T) {
	zero := report(calA, final, 0, 0)
	agreeing := report(calA, final, 0, 300)
	agreeing.Tokens.Out = 30
	split := agreeing // the same input total, split into cache reads
	split.Tokens.In, split.Tokens.CacheRead = 100, 200
	shortOut := agreeing
	shortOut.Tokens.Out = 29
	for name, c := range map[string]struct {
		ms      []types.UsageMeasurement
		settled bool
		in, out int64
	}{
		"a final, no count":             {[]types.UsageMeasurement{agreeing}, false, 300, 30},
		"a zero final under the count":  {[]types.UsageMeasurement{zero, counted(calA, 0, 200, 20), counted(calA, 1, 100, 10)}, false, 300, 30},
		"zero deltas, a zero final":     {[]types.UsageMeasurement{report(calA, delta, 0, 0), zero, counted(calA, 0, 300, 30)}, false, 300, 30},
		"a final agreeing":              {[]types.UsageMeasurement{agreeing, counted(calA, 0, 300, 30)}, true, 300, 30},
		"a final split otherwise":       {[]types.UsageMeasurement{split, counted(calA, 0, 300, 30)}, true, 100, 30},
		"a final short in output":       {[]types.UsageMeasurement{shortOut, counted(calA, 0, 300, 30)}, false, 300, 30},
		"a count replayed":              {[]types.UsageMeasurement{agreeing, counted(calA, 0, 300, 30), counted(calA, 0, 300, 30)}, true, 300, 30},
		"deltas and a count":            {[]types.UsageMeasurement{report(calA, delta, 0, 50), counted(calA, 0, 300, 30)}, false, 300, 30},
		"a reconciliation under it":     {[]types.UsageMeasurement{zero, report(calA, recon, 0, 10), counted(calA, 0, 300, 30)}, true, 300, 30},
		"a final over a floor":          {[]types.UsageMeasurement{agreeing, counted(calA, 0, 200, 30), floor(calA, 1, 5)}, false, 300, 30},
		"a reconciliation over a floor": {[]types.UsageMeasurement{report(calA, recon, 0, 10), floor(calA, 0, 50)}, true, 50, 1},
		"a count superseding its floor": {[]types.UsageMeasurement{agreeing, floor(calA, 0, 900), counted(calA, 0, 300, 30)}, true, 300, 30},
	} {
		st := standing(t, c.ms...)
		if st.Settled != c.settled || st.Usage == nil || st.Usage.In != c.in || st.Usage.Out != c.out {
			t.Errorf("%s: %+v (usage %+v), want settled %v at %d in, %d out", name, st, st.Usage, c.settled, c.in, c.out)
		}
		hasFinal := slices.ContainsFunc(c.ms, func(m types.UsageMeasurement) bool { return m.Kind == final })
		under := !c.settled && hasFinal && len(c.ms) > 1 && !strings.Contains(name, "floor")
		if st.Inconsistent != under && !strings.Contains(name, "reconciliation") {
			t.Errorf("%s: inconsistent %v, want %v", name, st.Inconsistent, under)
		}
	}
	got := reconcile(t, report(calA, delta, 0, 50), counted(calA, 0, 300, 30), counted(calA, 1, 1, 0))[calA]
	if got.Usage.In != 50 || got.Counted == nil || got.Counted.In != 301 || got.Counted.Out != 30 {
		t.Fatalf("reconciled %+v, counted %+v", got, got.Counted)
	}
	eur := counted(calA, 0, 300, 30)
	eur.Cost.Currency = "EUR"
	if _, err := Reconcile([]types.UsageMeasurement{report(calA, final, 0, 300), eur}); err != nil {
		t.Fatalf("a count's currency is not the call's: %v", err)
	}
	// L2 (Opus on #256): an honest EUR final beside the gateway's USD count
	// is neither mixed nor inconsistent.
	euro := agreeing
	euro.Cost.Currency = "EUR"
	if st := standing(t, euro, counted(calA, 0, 300, 30)); !st.Settled || st.Inconsistent {
		t.Fatalf("a EUR final: %+v", st)
	}
	// L3: the gateway's sources are deltas only.
	odd := counted(calA, 0, 1, 1)
	odd.Kind = final
	if _, err := contract.Encode("records/usage-measurement", odd); err == nil {
		t.Fatal("a gateway count of kind final validates")
	}
}

// P20 (Opus on #258, H1 and H2): a provider reconciliation replaces the
// adapter's reports and the gateway's floors, but never settles a call below
// the gateway's observed counts. Below them it settles at the larger of the
// two, flagged; a count recorded after it raises the figure; a later
// reconciliation supersedes it.
func TestAReconciliationNeverSettlesBelowTheCounts(t *testing.T) {
	split := report(calA, recon, 0, 100) // the count's 300 input, split into cache reads
	split.Tokens.CacheRead, split.Tokens.Out = 200, 30
	big := report(calA, delta, 0, 0)
	big.Tokens.In = maxAmount
	for name, c := range map[string]struct {
		ms           []types.UsageMeasurement
		in, out      int64
		below, flags bool
	}{
		"a found zero under a count": {[]types.UsageMeasurement{report(calA, recon, 0, 0), counted(calA, 0, 300, 30)}, 300, 30, true, true},
		"an answer split otherwise":  {[]types.UsageMeasurement{split, counted(calA, 0, 300, 30)}, 100, 30, false, false},
		// Mythos on #258, H2: a floor's input is the gateway's observation, its output (max_tokens) is not.
		"an answer under a floor's input": {[]types.UsageMeasurement{report(calA, recon, 0, 10), floor(calA, 0, 900)}, 900, 1, true, true},
		"a found zero over a floor":       {[]types.UsageMeasurement{report(calA, recon, 0, 0), withOut(floor(calA, 0, 900), 64)}, 900, 0, true, true},
		"an answer over a floor's input":  {[]types.UsageMeasurement{report(calA, recon, 0, 1000), withOut(floor(calA, 0, 900), 4096)}, 1000, 100, false, false},
		"a count after the answer": {[]types.UsageMeasurement{report(calA, recon, 0, 300), counted(calA, 0, 300, 30),
			counted(calA, 1, 200, 20)}, 500, 50, true, true},
		"a later answer superseding": {[]types.UsageMeasurement{report(calA, recon, 0, 10), report(calA, recon, 1, 400),
			counted(calA, 0, 300, 30)}, 400, 40, false, false},
	} {
		st := standing(t, c.ms...)
		if !st.Settled || st.Usage.In != c.in || st.Usage.Out != c.out || st.Below != c.below || st.Inconsistent != c.flags {
			t.Errorf("%s: %+v (usage %+v), want settled at %d in, %d out, below %v, flagged %v", name, st, st.Usage, c.in, c.out, c.below, c.flags)
		}
	}
	// Reports Reconcile cannot sum: the reconciliation still settles, at the counts.
	over := []types.UsageMeasurement{big, report(calA, delta, 1, 1), report(calA, recon, 0, 10), counted(calA, 0, 300, 30)}
	if st := StandingOf(over, calA); !st.Settled || !st.Below || st.Usage.In != 300 || st.Usage.Out != 30 {
		t.Errorf("an overflow: %+v (usage %+v), want settled at the count", st, st.Usage)
	}
}
