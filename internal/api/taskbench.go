// SPDX-License-Identifier: Apache-2.0

package api

// The task read's benchmark facts (M1 slice plan §2.8; P25): what adc dev
// bench record passes internal/bench.Record, read in the task read's own
// snapshot, under the attempt and model_call read kinds:
//   - each node of the task (its kind, its candidate's round, its state
//     and selected result, its first node.claimed and latest
//     result.selected times: graph.TimesOf) with every attempt on its unit
//     (lease.AttemptsOn);
//   - the lease of each of those attempts, with its executor, role and
//     placed price, every call the ledger binds to it (ledger.BoundTo)
//     with its binding's attempt and every measurement the ledger holds of
//     it (ledger.Measurements), and its usd_micros liability as Record
//     takes it: the group's liability (capacity.LiabilitiesOf) once the
//     lease's settlement is recorded, its usd_micros reservation before;
//     null when that liability is inconsistent or saturated;
//   - a candidate's closing review (graph.ClosesCandidate: the board's
//     done_candidate, before or after task.done records it), the latest
//     selected when two close it;
//   - the owed inputs its nodes asked for, answered (authority.Answered),
//     and its rail raises (authority.Raises) with the nodes each raise's
//     command released (graph.RailCleared).
// Every read is by the task's own nodes, leases and calls, never by
// scanning the installation's records.

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// benchOf is task t's benchmark facts over its nodes.
func benchOf(tx *eventstore.Tx, t authority.TaskRecord, nodes []graph.NodeView) (*types.TaskBench, error) {
	b := &types.TaskBench{Nodes: []types.BenchNode{}, Leases: []types.BenchLease{}, Answers: []types.NodeID{}, RailRaises: []types.BenchRailRaise{}}
	if t.Approval != nil {
		at := t.Approval.At
		b.ApprovedAt = &at
	}
	if t.DoneAs != "" {
		as := types.TaskBenchDoneAsValue(t.DoneAs)
		b.DoneAs = &as
	}
	ofTask := map[types.NodeID]bool{}
	leases := map[types.LeaseID]bool{}
	for _, n := range nodes {
		ofTask[n.Node.ID] = true
		times, err := graph.TimesOf(tx, n.Node.ID)
		if err != nil {
			return nil, err
		}
		bn := types.BenchNode{ID: n.Node.ID, Kind: types.BenchNodeKind(n.Node.Kind), Round: graph.CandidateRound(nodes, n.Node),
			State: types.BenchNodeState(n.State.State), Outcome: n.Outcome, Reason: n.Reason, ClaimedAt: stamped(times.Claimed),
			SelectedAt: stamped(times.Selected), Attempts: []types.BenchAttempt{}}
		attempts, err := lease.AttemptsOn(tx, graph.UnitOf(n.Node.ID))
		if err != nil {
			return nil, err
		}
		for _, a := range attempts {
			bn.Attempts, leases[a.Lease] = append(bn.Attempts, types.BenchAttempt{ID: a.ID, Lease: a.Lease}), true
		}
		b.Nodes = append(b.Nodes, bn)
		answered, err := authority.Answered(tx, n.Node.ID)
		if err != nil {
			return nil, err
		}
		for range answered {
			b.Answers = append(b.Answers, n.Node.ID)
		}
		if graph.ClosesCandidate(nodes, n) && n.Reason != nil && (b.Closure == nil || closer(times.Selected, n.Node.ID, b.Closure)) {
			if times.Selected.IsZero() {
				return nil, fmt.Errorf("api: the closing review %s has no result.selected time", n.Node.ID)
			}
			b.Closure = &types.BenchClosure{Node: n.Node.ID, SelectedAt: *stamped(times.Selected), Outcome: *n.Outcome, Reason: *n.Reason}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(leases)) {
		bl, err := benchLease(tx, id)
		if err != nil {
			return nil, err
		}
		b.Leases = append(b.Leases, bl)
	}
	raises, err := authority.Raises(tx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range raises {
		br := types.BenchRailRaise{Rail: types.RaisableRail(r.Rail), Nodes: []types.NodeID{}}
		if r.Command != nil {
			cleared, err := graph.RailCleared(tx, *r.Command)
			if err != nil {
				return nil, err
			}
			for _, n := range cleared {
				if ofTask[n] {
					br.Nodes = append(br.Nodes, n)
				}
			}
		}
		b.RailRaises = append(b.RailRaises, br)
	}
	return b, nil
}

// closer: a review selected at, with id, closes after c (the latest, then
// the larger id).
func closer(at time.Time, id types.NodeID, c *types.BenchClosure) bool {
	was, err := time.Parse(time.RFC3339Nano, string(c.SelectedAt))
	return err != nil || at.After(was) || at.Equal(was) && id > c.Node
}

// stamped is t as a contract time, nil for the zero time.
func stamped(t time.Time) *types.Time {
	if t.IsZero() {
		return nil
	}
	s := types.Time(t.UTC().Format(time.RFC3339Nano))
	return &s
}

// maxContract is the contract's maximum integer (2^53-1).
const maxContract = 1<<53 - 1

// benchLease is lease id's benchmark facts.
func benchLease(tx *eventstore.Tx, id types.LeaseID) (types.BenchLease, error) {
	l, _, ok, err := lease.LeaseOf(tx, id)
	if err == nil && !ok {
		err = fmt.Errorf("api: lease %s has no record", id)
	}
	if err != nil {
		return types.BenchLease{}, err
	}
	bl := types.BenchLease{ID: id, Executor: l.Executor, Role: l.Role, Price: l.Price, Calls: []types.BenchCall{}}
	calls, err := ledger.BoundTo(tx, id)
	if err != nil {
		return bl, err
	}
	slices.Sort(calls)
	for _, c := range calls {
		binding, bound, err := ledger.Binding(tx, c)
		if err == nil && !bound {
			err = fmt.Errorf("api: call %s of lease %s has no binding", c, id)
		}
		var ms []types.UsageMeasurement
		if err == nil {
			ms, err = ledger.Measurements(tx, c)
		}
		if err != nil {
			return bl, err
		}
		if ms == nil {
			ms = []types.UsageMeasurement{}
		}
		bl.Calls = append(bl.Calls, types.BenchCall{ID: c, Attempt: binding.Attempt, Measurements: ms})
	}
	var groups []types.ResourceGroupID
	liability := int64(0)
	for _, r := range l.Reservations {
		if r.Unit != types.CapacityUnitUsdMicros {
			continue
		} else if int64(r.Amount) > maxContract-liability {
			return bl, nil // past the contract's integers: null
		}
		groups, liability = append(groups, r.Group), liability+int64(r.Amount)
	}
	if l.Settlement != nil {
		owed, inconsistent, clipped, err := capacity.LiabilitiesOf(tx, id, groups)
		if err != nil || inconsistent || clipped {
			return bl, err // inconsistent or saturated: no figure bounds it (null)
		}
		liability = 0
		for _, g := range groups {
			if owed[g] > maxContract-liability {
				return bl, nil
			}
			liability += owed[g]
		}
	}
	bl.LiabilityUsdMicros = &liability
	return bl, nil
}
