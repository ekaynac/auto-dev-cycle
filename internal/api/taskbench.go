// SPDX-License-Identifier: Apache-2.0

package api

// The task read's benchmark facts (M1 slice plan §2.8; P25): what adc dev
// bench record passes internal/bench.Record, read in the task read's own
// snapshot, under the attempt and model_call read kinds:
//   - each node of the task (its kind, its candidate's round, its state
//     and selected result, its first node.claimed and latest
//     result.selected times: graph.TimesOf) with every attempt on its unit
//     (lease.AttemptsOn) and every result recorded on it, selected or not
//     (graph.ResultsOf);
//   - the lease of each of those attempts, with its executor, role and
//     placed price, every call the ledger binds to it (ledger.BoundTo)
//     with its binding's attempt and every measurement the ledger holds of
//     it (ledger.Measurements), the registry entry it was placed on
//     (dispatch.EntryDigestOf), and its usd_micros liability as Record
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
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// benchOf is task t's benchmark facts over its nodes. Each hop from a node
// to the last measurement is checked against its source, and a mismatch
// fails the read (Opus #432 r2 H1′): each attempt is of the node's unit and
// superseded by the one before it (lease.AttemptsOn); each lease, call and
// measurement is the attempt's (attemptLease); each answer is of an owed
// input the node asked (authority.Answered); each raise is t's
// (authority.Raises), each hold its command cleared is of its rail
// (graph.RailCleared) and on a node of t.
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
	leases := map[types.LeaseID]types.Attempt{}
	results, err := graph.ResultsOf(tx, t.ID)
	if err != nil {
		return nil, err
	}
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
			bn.Attempts, leases[a.Lease] = append(bn.Attempts, types.BenchAttempt{ID: a.ID, Lease: a.Lease}), a
		}
		if bn.Results, err = benchResults(n, results[n.Node.ID], attempts); err != nil {
			return nil, err
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
		bl, err := benchLease(tx, leases[id])
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
			cleared, err := graph.RailCleared(tx, *r.Command, r.Rail)
			if err != nil {
				return nil, err
			}
			for _, n := range cleared {
				if !ofTask[n] { // a raise clears its own task's holds alone (ClearRail)
					return nil, damagef("task %s's raise of %s names a hold cleared on %s, no node of the task", t.ID, r.Rail, n)
				}
				br.Nodes = append(br.Nodes, n)
			}
		}
		b.RailRaises = append(b.RailRaises, br)
	}
	return b, nil
}

// benchResults are the results recorded on node n, rs (graph.ResultsOf),
// each with whether it is n's selected one and the attempt that recorded
// it, checked against its source: that attempt is one on n's unit
// (attempts, lease.AttemptsOn's checked chain); else the task's damage.
func benchResults(n graph.NodeView, rs []types.ResultRecorded, attempts []types.Attempt) ([]types.BenchResult, error) {
	out := make([]types.BenchResult, 0, len(rs))
	for _, r := range rs {
		if r.Attempt != nil && !slices.ContainsFunc(attempts, func(a types.Attempt) bool { return a.ID == *r.Attempt }) {
			return nil, damagef("result %s of node %s names attempt %s, none on the node's unit", r.Result.ID, n.Node.ID, *r.Attempt)
		}
		selected := n.State.Result != nil && *n.State.Result == r.Result.ID
		out = append(out, types.BenchResult{ID: r.Result.ID, Outcome: r.Result.Outcome, Reason: r.Result.Reason, Selected: selected, Attempt: r.Attempt})
	}
	return out, nil
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

// benchLease is the benchmark facts of attempt a's lease, its every hop
// checked (attemptLease). Its liability is read by the lease's own id from
// its own reservations' groups; the row holds an amount alone, with no id
// to check against.
func benchLease(tx *eventstore.Tx, a types.Attempt) (types.BenchLease, error) {
	l, calls, err := attemptLease(tx, a)
	if err != nil {
		return types.BenchLease{}, err
	}
	id := l.ID
	bl := types.BenchLease{ID: id, Executor: l.Executor, Role: l.Role, Price: l.Price, Calls: []types.BenchCall{}}
	if d, ok := dispatch.EntryDigestOf(l); ok {
		entry := types.Digest(d)
		bl.RegistryEntry = &entry
	}
	for _, c := range calls {
		ms := c.ms
		if ms == nil {
			ms = []types.UsageMeasurement{}
		}
		bl.Calls = append(bl.Calls, types.BenchCall{ID: c.id, Attempt: c.attempt, Measurements: ms})
	}
	var groups []types.ResourceGroupID
	liability := int64(0)
	for _, r := range l.Reservations {
		if r.Unit != types.CapacityUnitUsdMicros {
			continue
		} else if int64(r.Amount) > contract.MaxSafeInteger-liability {
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
			if owed[g] > contract.MaxSafeInteger-liability {
				return bl, nil
			}
			liability += owed[g]
		}
	}
	bl.LiabilityUsdMicros = &liability
	return bl, nil
}
