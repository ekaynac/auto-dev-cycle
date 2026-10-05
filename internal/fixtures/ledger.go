// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"maps"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	callA      = types.CallID("cal_01m3250v9xft08ssr4ygq6g2np")
	callB      = types.CallID("cal_01m3250v57fhm9v1z9xd1wk8gw")
	attStale   = types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6a")
	attCurrent = types.AttemptID("att_01m3250v57ft7aehkqwq0yrq3n")
)

const (
	delta = types.UsageMeasurementKindDelta
	final = types.UsageMeasurementKindFinal
)

// usage is a usage report by attempt for call: in tokens, with time and
// cost proportional.
func usage(att types.AttemptID, call types.CallID, kind types.UsageMeasurementKind, seq, in int64) types.UsageMeasurement {
	return types.UsageMeasurement{ID: "msr_01m3250vm7f6ktw39dszqnyddr", Call: call, Attempt: att, Lease: "lea_01m3250v57fhm9v1z9xd1wk8gw",
		Kind: kind, Sequence: seq, Tokens: types.UsageMeasurementTokens{In: types.Amount(in), Out: types.Amount(in / 10)},
		Turns: 1, Milliseconds: types.Amount(in * 2), SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport,
		Cost: types.UsageMeasurementCost{AmountMicros: types.Amount(in * 3), Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true}}
}

// counted is what the ledger must give a call: its whole figure, and
// whether it is still an uncertain liability (no final, no reconciliation).
type counted struct {
	usage     ledger.Figure
	uncertain bool
}

// figure is the usage of reports of in tokens totalling turns turns, as
// usage makes them (out, time and cost proportional to in).
func figure(in, turns int64) ledger.Figure {
	return ledger.Figure{In: in, Out: in / 10, Turns: turns, Milliseconds: in * 2, CostMicros: in * 3}
}

// reconciled records one step per call the ledger reconciles from ms, in
// call order: empty/completed_clean, or found/inconsistent_usage when it
// flags the call. Each call's figure and uncertainty must be want's, and no
// call may be missing or extra.
func (o *observe) reconciled(want map[types.CallID]counted, ms ...types.UsageMeasurement) error {
	for _, m := range ms {
		if _, err := contract.Encode("records/usage-measurement", m); err != nil {
			return err
		}
	}
	got, err := ledger.Reconcile(ms)
	if err != nil {
		return err
	}
	for _, call := range slices.Sorted(maps.Keys(got)) {
		c := got[call]
		st := Step{vocab.Empty, vocab.ReasonCompletedClean.String()}
		if c.Inconsistent {
			st = Step{vocab.Found, c.Reason.String()}
		}
		o.steps = append(o.steps, st)
		w, ok := want[call]
		o.check(ok, "call %s was counted but reported nothing", call)
		o.check(c.Usage == w.usage, "call %s counts %+v, want %+v", call, c.Usage, w.usage)
		o.check(c.Uncertain == w.uncertain, "call %s is uncertain %t, want %t", call, c.Uncertain, w.uncertain)
	}
	for call := range want {
		_, ok := got[call]
		o.check(ok, "call %s was not counted", call)
	}
	return nil
}

// staleAttemptUsageCounted: a replaced attempt's output is fenced, but its
// usage is still counted, in either arrival order and on a call of its own
// (11 "stale-attempt usage still counted"; 05 "Usage and settlement").
func staleAttemptUsageCounted(context.Context, string) (steps []Step, violations []string, err error) {
	var o observe
	current := types.Attempt{ID: attCurrent, FencingToken: 2, Status: types.AttemptStatusRunning, Publication: types.AttemptPublicationOpen}
	if err := o.step(lease.Fence(current, attStale, 1)); err != nil {
		return nil, nil, err
	}
	stale, cur := usage(attStale, callA, delta, 0, 100), usage(attCurrent, callA, delta, 1, 40)
	both := map[types.CallID]counted{callA: {figure(140, 2), true}}
	for _, c := range []struct {
		want map[types.CallID]counted
		ms   []types.UsageMeasurement
	}{
		{both, []types.UsageMeasurement{stale, cur}},
		{both, []types.UsageMeasurement{cur, stale}},
		{map[types.CallID]counted{callA: {figure(40, 1), true}, callB: {figure(30, 1), false}},
			[]types.UsageMeasurement{usage(attStale, callB, final, 0, 30), usage(attCurrent, callA, delta, 0, 40)}},
	} {
		if err := o.reconciled(c.want, c.ms...); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

// duplicateUsageDeltas: a delta repeated for one (call, sequence) counts
// once, also when it arrives after the call's final, and one call's
// sequence never deduplicates another's; equal figures at two sequences
// both count, and a delta unseen until after the final raises the call
// above it (flagged); a repeat with another figure keeps the larger and is
// flagged, whichever arrives first (11 "duplicate usage deltas"; 05 "Usage
// and settlement").
func duplicateUsageDeltas(context.Context, string) (steps []Step, violations []string, err error) {
	d := func(seq, in int64) types.UsageMeasurement { return usage(attCurrent, callA, delta, seq, in) }
	b := func(seq, in int64) types.UsageMeasurement { return usage(attCurrent, callB, delta, seq, in) }
	covering := usage(attCurrent, callA, final, 0, 150)
	covering.Turns = 2 // the final covers both deltas' turns
	short := usage(attCurrent, callA, final, 0, 150)
	one := func(c counted) map[types.CallID]counted { return map[types.CallID]counted{callA: c} }
	var o observe
	for _, c := range []struct {
		want map[types.CallID]counted
		ms   []types.UsageMeasurement
	}{
		{one(counted{figure(150, 2), true}), []types.UsageMeasurement{d(0, 100), d(1, 50), d(0, 100)}},
		{map[types.CallID]counted{callA: {figure(150, 2), true}, callB: {figure(70, 1), true}},
			[]types.UsageMeasurement{d(0, 100), b(0, 70), d(1, 50), b(0, 70)}},
		{one(counted{figure(200, 2), true}), []types.UsageMeasurement{d(0, 100), d(1, 100)}},
		{one(counted{figure(150, 2), false}), []types.UsageMeasurement{d(0, 100), d(1, 50), covering, d(1, 50)}},
		{one(counted{figure(200, 2), false}), []types.UsageMeasurement{d(0, 100), short, d(1, 100)}},
		{one(counted{figure(200, 2), false}), []types.UsageMeasurement{d(1, 100), short, d(0, 100)}},
		{one(counted{figure(100, 1), true}), []types.UsageMeasurement{d(0, 100), d(0, 90)}},
		{one(counted{figure(100, 1), true}), []types.UsageMeasurement{d(0, 90), d(0, 100)}},
	} {
		if err := o.reconciled(c.want, c.ms...); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}
