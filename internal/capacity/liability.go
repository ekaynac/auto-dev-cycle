// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// The liability ledger (docs/design/m1/00-slice-plan.md P12b; 05 "Usage and settlement"; 11 B5). A lease's
// settlement names each call bound to it (records/model-call) exactly once: settled when the call ledger holds its
// final or a provider reconciliation, otherwise an uncertain liability. At settlement the reservation returns and
// what the uncertain calls owe moves onto the group's ledger, in the same fold:
//   - in tokens, a measured call owes its reconciled figure (deltas deduplicated by (call, sequence));
//   - an unmeasured call, with no report at all, owes the lease's positive reservation on the group, held once for
//     all the lease's unmeasured calls (one call per attempt, P12a), never zero;
//   - in another unit (slots), the reservation stays held while any call is uncertain.
//
// Late usage (a report after the settlement, or after its model_call effect left pending) is expected: each report
// recomputes the lease's liability from the whole call ledger, so the amount does not depend on whether a report
// came before or after the settlement, and a final or a reconciliation releases it. Amounts are a fold over the log
// (lease and usage events), so a rebuild derives them; no price or configuration enters them.
const (
	// LiabilityProjection: what a settled lease's uncertain calls owe a group, under "group/lease".
	LiabilityProjection = "capacity_liabilities"
	// LiableProjection: each call settled as a liability, with its lease.
	LiableProjection = "capacity_liable_calls"
)

type liability struct {
	Amount int64 `json:"amount"`
}

// Settlement is the settlement lease id's bound calls owe now, from the call ledger: a call with a final or a
// provider reconciliation is settled, every other one an uncertain liability. The settlement fold accepts nothing
// else.
func Settlement(tx *eventstore.Tx, id types.LeaseID) (types.LeaseSettlementValue, error) {
	s := types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}
	calls, err := boundTo(tx, id)
	for _, c := range calls {
		var settled bool
		if settled, _, err = reconciled(tx, c, nil); err != nil {
			return s, err
		}
		if settled {
			s.Calls = append(s.Calls, c)
		} else {
			s.Liabilities = append(s.Liabilities, c)
		}
	}
	return s, err
}

// boundTo lists the calls bound to lease id, sorted.
func boundTo(tx *eventstore.Tx, id types.LeaseID) ([]types.CallID, error) {
	keys, err := tx.Keys(modelcall.LeaseProjection, string(id)+"/")
	out := make([]types.CallID, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.CallID(strings.TrimPrefix(k, string(id)+"/")))
	}
	slices.Sort(out)
	return out, err
}

// reconciled reads call's reports, with extra (a report being recorded) once: whether the call is settled, and,
// for an uncertain call, its figure, nil when it has no report at all.
func reconciled(tx *eventstore.Tx, call types.CallID, extra *types.UsageMeasurement) (bool, *ledger.Figure, error) {
	ms, err := ledger.Measurements(tx, call)
	if err != nil {
		return false, nil, err
	}
	if extra != nil && !slices.ContainsFunc(ms, func(m types.UsageMeasurement) bool { return m.ID == extra.ID }) {
		ms = append(ms, *extra)
	}
	if len(ms) == 0 {
		return false, nil, nil
	}
	got, err := ledger.Reconcile(ms)
	if err != nil {
		return false, nil, err
	}
	c := got[call]
	return !c.Uncertain, &c.Usage, nil
}

// checkSettlement refuses a settlement that does not name each call bound to l exactly once, in its place: a call
// settled with no final or reconciliation (an unmeasured call among them), or one already settled kept as a
// liability. Leaving a bound call out would drop its charge.
func checkSettlement(tx *eventstore.Tx, l types.Lease) error {
	s := l.Settlement
	bound, err := boundTo(tx, l.ID)
	if err != nil {
		return err
	}
	named := slices.Sorted(slices.Values(append(slices.Clone(s.Calls), s.Liabilities...)))
	if !slices.Equal(named, bound) {
		return fmt.Errorf("lease %s settles calls %v, but its bound calls are %v", l.ID, named, bound)
	}
	for _, c := range named {
		settled, _, err := reconciled(tx, c, nil)
		listed := slices.Contains(s.Calls, c)
		switch {
		case err != nil:
			return err
		case settled && !listed:
			return fmt.Errorf("lease %s keeps call %s as a liability, but its usage is settled", l.ID, c)
		case !settled && listed:
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

// owe sets what settled lease l owes each of its groups, from the call ledger (with extra), adjusting each
// group's tally in next, or its recorded one, by the change. A change advances the group's capacity revision,
// once.
func owe(tx *eventstore.Tx, l types.Lease, extra *types.UsageMeasurement, next map[types.ResourceGroupID]tally) error {
	settled := map[types.CallID]bool{}
	figures := map[types.CallID]*ledger.Figure{}
	for _, c := range l.Settlement.Liabilities {
		var x *types.UsageMeasurement
		if extra != nil && extra.Call == c {
			x = extra
		}
		s, f, err := reconciled(tx, c, x)
		if err != nil {
			return err
		}
		settled[c], figures[c] = s, f
	}
	for _, res := range l.Reservations {
		amount, err := owed(res, l.Settlement.Liabilities, settled, figures)
		if err != nil {
			return fmt.Errorf("lease %s: %w", l.ID, err)
		}
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
		if !bumped {
			if amount == prev.Amount {
				continue
			}
			if t, err = load(tx, res.Group); err != nil {
				return err
			}
			t.Revision++
		}
		t.Liable += amount - prev.Amount
		next[res.Group] = t
		if amount == 0 {
			err = tx.Delete(LiabilityProjection, key)
		} else {
			raw, _ := json.Marshal(liability{amount})
			err = tx.Put(LiabilityProjection, key, raw)
		}
		if err != nil {
			return err
		}
	}
	return putAll(tx, next)
}

// owed is what the uncertain calls among calls owe res's group.
func owed(res types.LeaseReservationsItem, calls []types.CallID, settled map[types.CallID]bool, figures map[types.CallID]*ledger.Figure) (int64, error) {
	var measured int64
	uncertain, unmeasured := false, false
	for _, c := range calls {
		if settled[c] {
			continue
		}
		uncertain = true
		f := figures[c]
		if f == nil {
			unmeasured = true
			continue
		}
		measured += f.In + f.Out + f.CacheRead + f.CacheWrite
		if measured > maxAmount {
			return 0, fmt.Errorf("liabilities on group %s overflow", res.Group)
		}
	}
	switch {
	case unmeasured && res.Amount <= 0:
		return 0, fmt.Errorf("an unmeasured call's liability on group %s needs a positive reservation", res.Group)
	case res.Unit != types.CapacityUnitTokens:
		if uncertain {
			return res.Amount, nil
		}
		return 0, nil
	case unmeasured:
		measured += res.Amount
	}
	if measured > maxAmount {
		return 0, fmt.Errorf("liabilities on group %s overflow", res.Group)
	}
	return measured, nil
}

// Liable is what settled leases' uncertain calls owe group: it counts against the group's limit like a
// reservation, until their usage settles.
func Liable(tx *eventstore.Tx, group types.ResourceGroupID) (int64, error) {
	t, err := load(tx, group)
	return t.Liable, err
}

// LiabilitiesOf lists what lease id owes each group, by group.
func LiabilitiesOf(tx *eventstore.Tx, id types.LeaseID, groups []types.ResourceGroupID) (map[types.ResourceGroupID]int64, error) {
	out := map[types.ResourceGroupID]int64{}
	for _, g := range groups {
		raw, ok, err := tx.Get(LiabilityProjection, string(g)+"/"+string(id))
		if err != nil {
			return nil, err
		}
		var v liability
		if ok {
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			out[g] = v.Amount
		}
	}
	return out, nil
}
