// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// The liability ledger (docs/design/m1/00-slice-plan.md P12b, Q8; 05 "Usage and settlement"; 11 B5). A lease's
// settlement names each call bound to it (records/model-call) exactly once: settled when the call ledger holds its
// final or a provider reconciliation, otherwise an uncertain liability. At settlement the reservation returns and
// what the uncertain calls owe moves onto the group's ledger, in the same fold:
//   - in tokens, an uncertain call owes the larger of the lease's positive reservation and its reconciled tokens
//     (deltas deduplicated by (call, sequence)): an unmeasured call owes the reservation, and no report, however
//     small, lowers that until a final or a reconciliation settles the call (the lead's M1 reading of Q8). A lease
//     has one attempt, which binds one call (modelcall.Intend), so the reservation is the call's;
//   - in another unit (slots), the reservation stays held while the call is uncertain.
//
// Nothing a report says can refuse a settlement or a later report (ledger.StandingOf never fails), and amounts
// saturate at the contract's integer range, so no input wedges a lease or a group.
//
// Late usage (a report after the settlement, or after its model_call effect left pending) is expected: each report
// recomputes the lease's liability from the whole call ledger, so the amount does not depend on whether a report
// came before or after the settlement, and a final or a reconciliation releases it. Amounts are a fold over the log
// (lease and usage events), so a rebuild derives them; no price or configuration enters them.
//
// The fold reads earlier events' measurements from the ledger's projection, so it runs event by event
// (eventstore.ProjectEach, as adcd composes it) or with the settlement in a command of its own: a projector over a
// whole batch would settle before a measurement earlier in the same command is in the ledger.
const (
	// LiabilityProjection: what a settled lease's uncertain calls owe a group, under "group/lease".
	LiabilityProjection = "capacity_liabilities"
	// LiableProjection: each call settled as a liability, with its lease.
	LiableProjection = "capacity_liable_calls"
)

type liability struct {
	Amount int64 `json:"amount"`
	// Inconsistent: a report conflicted (inconsistent_usage), or could not be reconciled at all.
	Inconsistent bool `json:"inconsistent,omitempty"`
}

// Settlement is the settlement lease id's bound calls owe now, from the call ledger: a call with a final or a
// provider reconciliation is settled, every other one an uncertain liability. The settlement fold accepts nothing
// else.
func Settlement(tx *eventstore.Tx, id types.LeaseID) (types.LeaseSettlementValue, error) {
	s := types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}
	calls, err := ledger.BoundTo(tx, id)
	for _, c := range calls {
		var st ledger.Standing
		if st, err = standing(tx, c, nil); err != nil {
			return s, err
		}
		if st.Settled {
			s.Calls = append(s.Calls, c)
		} else {
			s.Liabilities = append(s.Liabilities, c)
		}
	}
	return s, err
}

// standing reads call's reports, with extra (a report being recorded) once.
func standing(tx *eventstore.Tx, call types.CallID, extra *types.UsageMeasurement) (ledger.Standing, error) {
	ms, err := ledger.Measurements(tx, call)
	if err != nil {
		return ledger.Standing{}, err
	}
	if extra != nil && !slices.ContainsFunc(ms, func(m types.UsageMeasurement) bool { return m.ID == extra.ID }) {
		ms = append(ms, *extra)
	}
	return ledger.StandingOf(ms, call), nil
}

// checkSettlement refuses a settlement that does not name each call bound to l exactly once, in its place: a call
// settled with no final or reconciliation (an unmeasured call among them), or one already settled kept as a
// liability. Leaving a bound call out would drop its charge. Every reservation of a lease with a bound call is
// positive (modelcall.Intend), so an uncertain call never owes zero.
func checkSettlement(tx *eventstore.Tx, l types.Lease) error {
	s := l.Settlement
	bound, err := ledger.BoundTo(tx, l.ID)
	if err != nil {
		return err
	}
	named := slices.Sorted(slices.Values(append(slices.Clone(s.Calls), s.Liabilities...)))
	if !slices.Equal(named, bound) {
		return fmt.Errorf("lease %s settles calls %v, but its bound calls are %v", l.ID, named, bound)
	}
	for _, c := range named {
		st, err := standing(tx, c, nil)
		listed := slices.Contains(s.Calls, c)
		switch {
		case err != nil:
			return err
		case st.Settled && !listed:
			return fmt.Errorf("lease %s keeps call %s as a liability, but its usage is settled", l.ID, c)
		case !st.Settled && listed:
			return fmt.Errorf("lease %s settles call %s with no final usage: it stays an uncertain liability", l.ID, c)
		}
	}
	return nil
}

// settle accepts l's settlement and moves what its uncertain calls owe onto their groups, over next (the tallies
// its returned reservations left).
func settle(tx *eventstore.Tx, l types.Lease, next map[types.ResourceGroupID]tally) error {
	if err := checkSettlement(tx, l); err != nil {
		return err
	}
	for _, c := range l.Settlement.Liabilities {
		if err := tx.Put(LiableProjection, string(c), json.RawMessage(fmt.Sprintf("%q", l.ID))); err != nil {
			return err
		}
	}
	return owe(tx, l, nil, next)
}

// lateUsage: a report on a call settled as a liability recomputes its lease's liability. A report on any other
// call changes nothing here: its lease has not settled yet, or the call was settled.
func lateUsage(tx *eventstore.Tx, m types.UsageMeasurement) error {
	raw, ok, err := tx.Get(LiableProjection, string(m.Call))
	if err != nil || !ok {
		return err
	}
	var id types.LeaseID
	if err := json.Unmarshal(raw, &id); err != nil {
		return err
	}
	l, _, ok, err := lease.LeaseOf(tx, id)
	if err == nil && (!ok || l.Settlement == nil) {
		err = fmt.Errorf("call %s's liability names lease %s, which has not settled", m.Call, id)
	}
	if err != nil {
		return err
	}
	return owe(tx, l, &m, map[types.ResourceGroupID]tally{})
}

// owe sets what settled lease l owes each of its groups, from the call ledger (with extra), and each changed
// group's Liable, in next or from its recorded tally. A change advances the group's capacity revision, once.
func owe(tx *eventstore.Tx, l types.Lease, extra *types.UsageMeasurement, next map[types.ResourceGroupID]tally) error {
	var uncertain []ledger.Standing
	for _, c := range l.Settlement.Liabilities {
		var x *types.UsageMeasurement
		if extra != nil && extra.Call == c {
			x = extra
		}
		st, err := standing(tx, c, x)
		if err != nil {
			return err
		}
		if !st.Settled {
			uncertain = append(uncertain, st)
		}
	}
	for _, res := range l.Reservations {
		want := owed(res, uncertain)
		key := string(res.Group) + "/" + string(l.ID)
		var prev liability
		if raw, ok, err := tx.Get(LiabilityProjection, key); err != nil {
			return err
		} else if ok {
			if err := json.Unmarshal(raw, &prev); err != nil {
				return err
			}
		}
		t, bumped := next[res.Group]
		if !bumped && want == prev {
			continue
		}
		var err error
		if want.Amount == 0 {
			err = tx.Delete(LiabilityProjection, key)
		} else {
			raw, _ := json.Marshal(want)
			err = tx.Put(LiabilityProjection, key, raw)
		}
		if err != nil {
			return err
		}
		if !bumped {
			if t, err = load(tx, res.Group); err != nil {
				return err
			}
			t.Revision++
		}
		if t.Liable, err = liable(tx, res.Group); err != nil {
			return err
		}
		next[res.Group] = t
	}
	return putAll(tx, next)
}

// owed is what the uncertain calls owe res's group: nothing once none is left.
func owed(res types.LeaseReservationsItem, uncertain []ledger.Standing) liability {
	if len(uncertain) == 0 {
		return liability{}
	}
	out := liability{Amount: res.Amount}
	var measured int64
	for _, st := range uncertain {
		out.Inconsistent = out.Inconsistent || st.Inconsistent
		if f := st.Usage; f != nil && res.Unit == types.CapacityUnitTokens {
			measured = saturated(measured, f.In, f.Out, f.CacheRead, f.CacheWrite)
		}
	}
	out.Amount = max(out.Amount, measured)
	return out
}

// saturated adds the amounts, stopping at the contract's integer range.
func saturated(a int64, more ...int64) int64 {
	for _, b := range more {
		a = min(maxAmount, a+min(maxAmount, max(b, 0)))
	}
	return a
}

// liable sums what settled leases owe group, saturating.
func liable(tx *eventstore.Tx, group types.ResourceGroupID) (int64, error) {
	keys, err := tx.Keys(LiabilityProjection, string(group)+"/")
	var sum int64
	for _, k := range keys {
		raw, _, gerr := tx.Get(LiabilityProjection, k)
		var v liability
		if gerr == nil {
			gerr = json.Unmarshal(raw, &v)
		}
		if gerr != nil {
			return 0, gerr
		}
		sum = saturated(sum, v.Amount)
	}
	return sum, err
}

// Liable is what settled leases' uncertain calls owe group: it counts against the group's limit like a
// reservation, until their usage settles.
func Liable(tx *eventstore.Tx, group types.ResourceGroupID) (int64, error) {
	t, err := load(tx, group)
	return t.Liable, err
}

// LiabilitiesOf lists what lease id owes each of groups, and whether any of it is inconsistent_usage.
func LiabilitiesOf(tx *eventstore.Tx, id types.LeaseID, groups []types.ResourceGroupID) (map[types.ResourceGroupID]int64, bool, error) {
	out, inconsistent := map[types.ResourceGroupID]int64{}, false
	for _, g := range groups {
		raw, ok, err := tx.Get(LiabilityProjection, string(g)+"/"+string(id))
		if err != nil {
			return nil, false, err
		}
		var v liability
		if ok {
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, false, err
			}
			out[g], inconsistent = v.Amount, inconsistent || v.Inconsistent
		}
	}
	return out, inconsistent, nil
}
