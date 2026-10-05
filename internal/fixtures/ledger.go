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

// reconciled records one step per call the ledger reconciles from ms, in
// call order: empty/completed_clean, or found/inconsistent_usage when it
// flags the call. Each call's tokens in must be want's, and no call may be
// missing or extra.
func (o *observe) reconciled(want map[types.CallID]int64, ms ...types.UsageMeasurement) error {
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
		o.check(c.Usage.In == want[call], "call %s counts %d tokens in, want %d", call, c.Usage.In, want[call])
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
	for _, c := range []struct {
		want map[types.CallID]int64
		ms   []types.UsageMeasurement
	}{
		{map[types.CallID]int64{callA: 140}, []types.UsageMeasurement{stale, cur}},
		{map[types.CallID]int64{callA: 140}, []types.UsageMeasurement{cur, stale}},
		{map[types.CallID]int64{callA: 40, callB: 30}, []types.UsageMeasurement{usage(attStale, callB, final, 0, 30), usage(attCurrent, callA, delta, 0, 40)}},
	} {
		if err := o.reconciled(c.want, c.ms...); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

// duplicateUsageDeltas: a delta repeated for one (call, sequence) counts
// once, also when it arrives after the call's final; equal figures at two
// sequences both count; a repeat with another figure keeps the larger and
// is flagged, whichever arrives first (11 "duplicate usage deltas"; 05
// "Usage and settlement").
func duplicateUsageDeltas(context.Context, string) (steps []Step, violations []string, err error) {
	d := func(seq, in int64) types.UsageMeasurement { return usage(attCurrent, callA, delta, seq, in) }
	fin := usage(attCurrent, callA, final, 0, 150)
	fin.Turns = 2 // the final covers both deltas' turns
	var o observe
	for _, c := range []struct {
		want int64
		ms   []types.UsageMeasurement
	}{
		{150, []types.UsageMeasurement{d(0, 100), d(1, 50), d(0, 100)}},
		{200, []types.UsageMeasurement{d(0, 100), d(1, 100)}},
		{150, []types.UsageMeasurement{d(0, 100), d(1, 50), fin, d(1, 50)}},
		{100, []types.UsageMeasurement{d(0, 100), d(0, 90)}},
		{100, []types.UsageMeasurement{d(0, 90), d(0, 100)}},
	} {
		if err := o.reconciled(map[types.CallID]int64{callA: c.want}, c.ms...); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}
