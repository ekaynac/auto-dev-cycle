// SPDX-License-Identifier: Apache-2.0

// Package ledger reconciles usage, in one place (docs/design/m0/05 "Usage
// and settlement" [M7]). Usage is per call, never a session total, and is
// accepted from any attempt, stale or not. Deltas are deduplicated by
// (call, sequence); a final replaces their sum only where it is not lower;
// only a provider reconciliation can lower a figure; a call with neither
// stays an uncertain liability at its best estimate. The gateway's own
// counts (gateway_count, gateway_floor) are kept apart from the adapter's reports, and the
// liability ledger settles a call on a final only when it is not below them
// (StandingOf). The result does not depend on the order reports arrive in.
package ledger

import (
	"cmp"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
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

// Standing is a call's standing for the liability ledger (internal/capacity).
type Standing struct {
	Settled bool    // a provider reconciliation, or a final the gateway's count bears out (trusted)
	Usage   *Figure // the reconciled figure, the gateway's count included while unsettled; nil with no report at all
	// Inconsistent: a report conflicted (inconsistent_usage), its currencies
	// among them, or the reports could not be summed.
	Inconsistent bool
	// Below: the latest provider reconciliation is below the gateway's observed counts, so the call settles at the
	// counts (P20, H1), flagged; a later reconciliation may correct it.
	Below bool
}

// StandingOf is call's standing from its reports ms. Unlike Reconcile it
// never fails, so no report can wedge a settlement (and trusted decides
// what settles):
//   - token figures do not depend on currency, so reports in several
//     currencies are reconciled as one and only flagged (pricing reads the
//     executor's declared price, never the reported currency or cost);
//   - reports Reconcile cannot sum (an overflow) leave the call uncertain at
//     a saturated figure, flagged, unless a provider reconciliation settles
//     it, never below the gateway's observed counts (trusted).
func StandingOf(ms []types.UsageMeasurement, call types.CallID) Standing {
	if len(ms) == 0 {
		return Standing{}
	}
	one, mixed, cur := slices.Clone(ms), false, ""
	for i := range one {
		if counts(one[i]) {
			continue // the gateway's currency is no report's
		}
		cur = cmp.Or(cur, one[i].Cost.Currency)
		mixed = mixed || one[i].Cost.Currency != cur
		one[i].Cost.Currency = cur
	}
	if got, err := Reconcile(one); err == nil {
		return trusted(got[call], mixed)
	}
	var recon []types.UsageMeasurement
	seen := map[key]Figure{}
	for _, m := range one {
		if m.Kind == types.UsageMeasurementKindProviderReconciliation {
			recon = append(recon, m)
		}
		if m.Call == call && m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount {
			seen[key{call, m.Sequence}], _ = maxOf(seen[key{call, m.Sequence}], figureOf(m))
		}
	}
	counted, err := sum(seen)
	if len(recon) == 0 || err != nil {
		return Standing{Usage: &Figure{In: maxAmount}, Inconsistent: true}
	}
	f, _ := latest(recon)
	st := Standing{Settled: true, Usage: &f, Inconsistent: true}
	if c, ok := counted[call]; ok && !covers(f, c) {
		u, _ := maxOf(f, c)
		st.Usage, st.Below = &u, true
	}
	return st
}

// trusted is c's standing (#226 entry 69): a provider reconciliation settles
// it, and so does a final, but only one not below the gateway's own count of
// the call, none of it a floor. A final with no count, with a floor, or
// under the count, is the adapter's word alone (a hostile one reports zero):
// the call stays uncertain at the larger of the reports and the count, a
// final under the count flagged. A reconciliation replaces the adapter's
// reports and the floors, but never settles below the counts the gateway
// observed (P20): an upstream answering its own usage query is no more
// trusted than its in-band usage, and a usage API may lag. Below them, the
// call settles at the larger of the two, flagged, so a count that lands
// after the reconciliation still raises the figure.
func trusted(c Call, mixed bool) Standing {
	st := Standing{Settled: c.Reconciled, Usage: &c.Usage, Inconsistent: mixed || c.Inconsistent}
	if c.Reconciled && c.Seen != nil && !covers(c.Usage, *c.Seen) {
		u, _ := maxOf(c.Usage, *c.Seen)
		st.Usage, st.Inconsistent, st.Below = &u, true, true
	}
	if c.Reconciled || c.Counted == nil {
		return st
	}
	under := !covers(c.Usage, *c.Counted)
	st.Settled = !c.Uncertain && !c.Floored && !under
	if !st.Settled {
		u, _ := maxOf(c.Usage, *c.Counted)
		st.Usage, st.Inconsistent = &u, st.Inconsistent || !c.Uncertain && under
	}
	return st
}

// covers: f is not below count in input tokens (input and cache, priced
// alike) or in output tokens. Comparing the input sum keeps an honest
// final whose cache split differs from the gateway's.
func covers(f, count Figure) bool {
	in := func(f Figure) int64 { return f.In + f.CacheRead + f.CacheWrite }
	return in(f) >= in(count) && f.Out >= count.Out
}

// PriceMicros prices a figure in USD micros at an executor's declared price
// (config/executor-registry), from its tokens alone, rounding up: input and
// cache tokens at the input rate, output at the output rate. It never reads
// CostMicros, the adapter-reported amount, estimated or not: an upstream can
// report any cost, zero included (#226 entry 62).
func PriceMicros(f Figure, p types.RegistryPrice) (int64, error) {
	return priced([][2]int64{{f.In, p.InputUsdMicrosPerMtok}, {f.CacheRead, p.InputUsdMicrosPerMtok},
		{f.CacheWrite, p.InputUsdMicrosPerMtok}, {f.Out, p.OutputUsdMicrosPerMtok}})
}

// PriceTokens prices tokens of unknown kind, such as an unmeasured call's
// reservation, at the higher of the two rates.
func PriceTokens(tokens int64, p types.RegistryPrice) (int64, error) {
	return priced([][2]int64{{tokens, max(p.InputUsdMicrosPerMtok, p.OutputUsdMicrosPerMtok)}})
}

// priced sums tokens × micros-per-million-tokens over terms, rounding up,
// refusing a negative term or a total past the contract's integers.
func priced(terms [][2]int64) (int64, error) {
	sum := new(big.Int)
	for _, t := range terms {
		if t[0] < 0 || t[1] < 0 {
			return 0, errors.New("ledger: a negative amount is not priced")
		}
		sum.Add(sum, new(big.Int).Mul(big.NewInt(t[0]), big.NewInt(t[1])))
	}
	sum.Add(sum, big.NewInt(999_999)).Quo(sum, big.NewInt(1_000_000))
	if !sum.IsInt64() || sum.Int64() > maxAmount {
		return 0, errors.New("ledger: the price overflows the contract's integer range")
	}
	return sum.Int64(), nil
}

// Call is one call's reconciled usage.
type Call struct {
	Call     types.CallID
	Usage    Figure
	Currency string // "" until a report names one
	// Uncertain: no final and no provider reconciliation yet; the figure is
	// the best estimate and stays a liability on the call's groups. It is not
	// settlement: a final settles a call only as StandingOf trusts it, which
	// is what P17, P20 and the liability ledger read.
	Uncertain bool
	// Inconsistent: a report was lower than an earlier figure without a
	// provider reconciliation; the larger figure was kept (inconsistent_usage).
	Inconsistent bool
	Reason       vocab.Reason
	// Counted is the gateway's own count (gateway_count and gateway_floor
	// reports, summed over its sequences), nil when it counted none; Usage
	// leaves it out. Floored: some request has a floor and no count.
	Counted *Figure
	Floored bool
	// Seen is the gateway's observed counts alone (gateway_count, summed over the sequences it counted), nil with
	// none: no reconciliation settles the call below it.
	Seen *Figure
	// Reconciled: a provider reconciliation is recorded.
	Reconciled bool
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
	observed, floors := map[key]Figure{}, map[key]Figure{}
	for _, m := range ms {
		if mutation.Is(mutation.StaleAttemptUsageDropped) && m.Attempt != ms[len(ms)-1].Attempt {
			continue
		}
		if counts(m) {
			// The gateway's floor and count of one request share its
			// sequence; a replay is the same report, and a clash keeps the
			// larger.
			at, k := floors, key{m.Call, m.Sequence}
			if m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount {
				at = observed
			}
			at[k], _ = maxOf(at[k], figureOf(m))
			continue
		}
		if c, ok := currency[m.Call]; ok && c != m.Cost.Currency {
			return nil, fmt.Errorf("ledger: call %s reports in %s and %s", m.Call, c, m.Cost.Currency)
		}
		currency[m.Call] = m.Cost.Currency
		switch m.Kind {
		case types.UsageMeasurementKindDelta:
			k := key{m.Call, m.Sequence}
			f := figureOf(m)
			if prev, dup := deltas[k]; dup && mutation.Is(mutation.DeltasNotDeduplicated) {
				f, _ = add(prev, f)
			} else if dup && prev != f {
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
	sums, err := sum(deltas)
	if err != nil {
		return nil, err
	}
	seen, err := sum(observed)
	if err != nil {
		return nil, err
	}
	floored := map[types.CallID]bool{}
	for k, f := range floors {
		if _, ok := observed[k]; !ok {
			observed[k], floored[k.call] = f, true // a count supersedes its request's floor
		}
	}
	counted, err := sum(observed)
	if err != nil {
		return nil, err
	}
	out := map[types.CallID]Call{}
	for call, f := range counted {
		out[call] = Call{Call: call, Uncertain: true, Counted: &f, Floored: floored[call]}
		if s, ok := seen[call]; ok {
			c := out[call]
			c.Seen = &s
			out[call] = c
		}
	}
	for call, cur := range currency {
		c := Call{Call: call, Usage: sums[call], Currency: cur, Uncertain: true, Inconsistent: conflict[call],
			Counted: out[call].Counted, Floored: floored[call], Seen: out[call].Seen}
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
			c.Uncertain, c.Reconciled = false, true
		}
		if c.Inconsistent {
			c.Reason = vocab.ReasonInconsistentUsage
		}
		out[call] = c
	}
	return out, nil
}

// counts: m is the gateway's own count of a request, or its floor.
func counts(m types.UsageMeasurement) bool {
	return m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount ||
		m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayFloor
}

// sum adds each call's figures, deterministically.
func sum(figures map[key]Figure) (map[types.CallID]Figure, error) {
	keys := make([]key, 0, len(figures))
	for k := range figures {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].call != keys[j].call {
			return keys[i].call < keys[j].call
		}
		return keys[i].seq < keys[j].seq
	})
	sums := map[types.CallID]Figure{}
	for _, k := range keys {
		s, err := add(sums[k.call], figures[k])
		if err != nil {
			return nil, err
		}
		sums[k.call] = s
	}
	return sums, nil
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
