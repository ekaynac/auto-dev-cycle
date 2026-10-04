// SPDX-License-Identifier: Apache-2.0

// Package ledger reconciles usage, in one place (docs/design/m0/05 "Usage
// and settlement" [M7]). Usage is per call, never a session total, and is
// accepted from any attempt, stale or not. Deltas are deduplicated by
// (call, sequence); a final replaces their sum only where it is not lower;
// only a provider reconciliation can lower a figure; a call with neither
// stays an uncertain liability at its best estimate. The result does not
// depend on the order reports arrive in.
package ledger

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Figure is a usage amount: tokens, turns, time and cost.
type Figure struct {
	In, Out, CacheRead, CacheWrite int64
	Turns, Milliseconds            int64
	CostMicros                     int64
}

func figureOf(m types.UsageMeasurement) Figure {
	return Figure{In: int64(m.Tokens.In), Out: int64(m.Tokens.Out), CacheRead: int64(m.Tokens.CacheRead),
		CacheWrite: int64(m.Tokens.CacheWrite), Turns: int64(m.Turns), Milliseconds: int64(m.Milliseconds),
		CostMicros: int64(m.Cost.AmountMicros)}
}

func (f Figure) fields() []int64 {
	return []int64{f.In, f.Out, f.CacheRead, f.CacheWrite, f.Turns, f.Milliseconds, f.CostMicros}
}

func fromFields(v []int64) Figure {
	return Figure{In: v[0], Out: v[1], CacheRead: v[2], CacheWrite: v[3], Turns: v[4], Milliseconds: v[5], CostMicros: v[6]}
}

// add sums two figures, refusing an overflow past the contract's integers.
func add(a, b Figure) (Figure, error) {
	x, y := a.fields(), b.fields()
	out := make([]int64, len(x))
	for i := range x {
		if y[i] > maxAmount-x[i] {
			return Figure{}, errors.New("ledger: usage overflows the contract's integer range")
		}
		out[i] = x[i] + y[i]
	}
	return fromFields(out), nil
}

// maxOf is the larger figure in each field; lower reports whether a had any
// field below b.
func maxOf(a, b Figure) (m Figure, lower bool) {
	x, y := a.fields(), b.fields()
	out := make([]int64, len(x))
	for i := range x {
		out[i] = max(x[i], y[i])
		lower = lower || x[i] < y[i]
	}
	return fromFields(out), lower
}

const maxAmount = 9007199254740991

// Call is one call's reconciled usage.
type Call struct {
	Call     types.CallID
	Usage    Figure
	Currency string // "" until a report names one
	// Uncertain: no final and no provider reconciliation yet; the figure is
	// the best estimate and stays a liability on the call's groups.
	Uncertain bool
	// Inconsistent: a report was lower than an earlier figure without a
	// provider reconciliation; the larger figure was kept (inconsistent_usage).
	Inconsistent bool
	Reason       vocab.Reason
}

type key struct {
	call types.CallID
	seq  int64
}

// Reconcile computes each call's usage from its reports, in any order.
func Reconcile(ms []types.UsageMeasurement) (map[types.CallID]Call, error) {
	deltas := map[key]Figure{}
	finals := map[types.CallID][]types.UsageMeasurement{}
	recon := map[types.CallID][]types.UsageMeasurement{}
	conflict := map[types.CallID]bool{}
	currency := map[types.CallID]string{}
	for _, m := range ms {
		if c, ok := currency[m.Call]; ok && c != m.Cost.Currency {
			return nil, fmt.Errorf("ledger: call %s reports in %s and %s", m.Call, c, m.Cost.Currency)
		}
		currency[m.Call] = m.Cost.Currency
		switch m.Kind {
		case types.UsageMeasurementKindDelta:
			k := key{m.Call, m.Sequence}
			f := figureOf(m)
			if prev, dup := deltas[k]; dup && prev != f {
				// The same delta reported twice with different figures:
				// keep the larger, never the smaller, and flag it.
				f, _ = maxOf(prev, f)
				conflict[m.Call] = true
			}
			deltas[k] = f
		case types.UsageMeasurementKindFinal:
			finals[m.Call] = append(finals[m.Call], m)
		case types.UsageMeasurementKindProviderReconciliation:
			recon[m.Call] = append(recon[m.Call], m)
		default:
			return nil, fmt.Errorf("ledger: report %s has kind %q", m.ID, m.Kind)
		}
	}
	sums := map[types.CallID]Figure{}
	keys := make([]key, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // a deterministic sum
		if keys[i].call != keys[j].call {
			return keys[i].call < keys[j].call
		}
		return keys[i].seq < keys[j].seq
	})
	for _, k := range keys {
		s, err := add(sums[k.call], deltas[k])
		if err != nil {
			return nil, err
		}
		sums[k.call] = s
	}
	out := map[types.CallID]Call{}
	for call, cur := range currency {
		c := Call{Call: call, Usage: sums[call], Currency: cur, Uncertain: true, Inconsistent: conflict[call]}
		if fs := finals[call]; len(fs) > 0 {
			f, clash := finalFigure(fs)
			var lower bool
			c.Usage, lower = maxOf(f, sums[call])
			c.Inconsistent = c.Inconsistent || clash || lower
			c.Uncertain = false
		}
		if rs := recon[call]; len(rs) > 0 {
			// The only report that can lower a figure: it replaces the rest.
			c.Usage, c.Inconsistent = latest(rs)
			c.Uncertain = false
		}
		if c.Inconsistent {
			c.Reason = vocab.ReasonInconsistentUsage
		}
		out[call] = c
	}
	return out, nil
}

// finalFigure combines a call's finals: no final can lower another's
// figure, so the result is each field's largest value over all of them. A
// final lower in any field than one with a lower or equal sequence, or two
// with one sequence and different figures, is flagged. Arrival order never
// matters.
func finalFigure(ms []types.UsageMeasurement) (f Figure, inconsistent bool) {
	sorted := slices.Clone(ms)
	slices.SortStableFunc(sorted, func(a, b types.UsageMeasurement) int { return cmp.Compare(a.Sequence, b.Sequence) })
	for i, m := range sorted {
		g := figureOf(m)
		if i > 0 {
			if _, lower := maxOf(g, f); lower {
				inconsistent = true
			}
		}
		f, _ = maxOf(f, g)
	}
	// Same-sequence reports differing only upward from an earlier one are
	// still a conflict.
	seen := map[int64]Figure{}
	for _, m := range ms {
		if prev, ok := seen[m.Sequence]; ok && prev != figureOf(m) {
			inconsistent = true
		}
		seen[m.Sequence] = figureOf(m)
	}
	return f, inconsistent
}

// latest resolves a call's provider reconciliations: the highest
// sequence wins; reports sharing it with different figures merge field by
// field to the larger and are flagged. Arrival order never matters.
func latest(ms []types.UsageMeasurement) (f Figure, conflict bool) {
	top := ms[0].Sequence
	for _, m := range ms {
		top = max(top, m.Sequence)
	}
	first := true
	for _, m := range ms {
		if m.Sequence != top {
			continue
		}
		g := figureOf(m)
		if !first && g != f {
			conflict = true
		}
		f, _ = maxOf(f, g)
		first = false
	}
	return f, conflict
}
