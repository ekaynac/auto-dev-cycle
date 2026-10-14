// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery_test

// The bounded model of restart recovery (D62; docs/design/m1/00-slice-plan.md §8 item 3, P21): publication, fencing,
// settlement and replacement after a controller died mid-call, explored exhaustively.
//
// What it is. An explicit-state model checker over one unit, its cut attempt A1 under L1 with one model call, and its
// replacement A2 under L2. Each action is one writer transaction. Every interleaving is explored of:
//   - crash: the controller dies again (at most maxCrashes times), at any point, and a new process starts;
//   - lose: X3 step 1 in that process (lease.LoseOnRestart on every running attempt);
//   - recover A1, recover A2: the step recovery.Next decides, applied with the real state machines
//     (lease.ApplyLease, lease.ApplyAttempt): revoke, seal, settle;
//   - grant L2, start A2: placement (dispatch.Match's guard, as lease.Ended decides it), then the launch, minting A2
//     with the real lease.Supersede;
//   - old output: A1's adapter, which outlived the controller, publishes; it lands only if the real lease.Fence
//     admits it;
//   - late usage: the call's final or reconciliation arrives (at most once), at any time (P12b).
//
// What it mirrors rather than calls (each a store-level check that needs a transaction): the lease fold's rule that a
// lease leaves active only once its attempt's publication is closed (lease.Projector.changeLease); the settlement
// naming the bound call as settled or as a liability (capacity.checkSettlement); the restart order, in which nothing
// runs in a process before step 1 (P22a runs Recovery.Run at start, before the writer admits any other command).
//
// The invariants, checked in every reachable state:
//   - I1 fencing: no output of A1 lands once the controller died;
//   - I2 one publisher: at most one attempt of the unit has its publication open;
//   - I3 settle before replace: A2 starts only once L1 has settled, so it is sized after step 4;
//   - I4 one transaction: A2 supersedes A1 with the next token, and A1 is superseded by A2, in the same step;
//   - I5 no dropped charge: the call is held by L1's reservation, settled, or kept as a liability;
//   - I6 recovery completes: from any state, step 1 then recovery's own steps alone end with A1 sealed or superseded,
//     L1 settled, and nothing left for Next; a further restart with nothing running changes nothing.
//
// The fault fixtures below each turn one of them red.

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
)

const (
	maxCrashes = 2
	a1, a2     = types.AttemptID("att_01m3250vm7f6ktw39dszqnydd1"), types.AttemptID("att_01m3250vm7f6ktw39dszqnydd2")
	l1, l2     = types.LeaseID("lea_01m3250vm7f6ktw39dszqnydd1"), types.LeaseID("lea_01m3250vm7f6ktw39dszqnydd2")
	unit       = types.UnitID("unt_01m3250vm7f6ktw39dszqnydd1")
	call       = types.CallID("cal_01m3250vm7f6ktw39dszqnydd1")
	record     = types.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
)

// mstate is one state of the model; its JSON is its identity.
type mstate struct {
	A1, A2        types.Attempt
	L1, L2        types.Lease
	Current       types.AttemptID
	Crashes       int
	Lost          bool // step 1 ran in this process
	Settled       bool // the call's usage settles
	Reported      bool // the late usage arrived
	Reserved      bool // L1's reservation is held
	Liability     bool // L1's settlement keeps the call as a liability
	A1Landed      bool // an output of A1 landed after the controller died
	StartedEarly  bool // A2 started while L1 had not settled
	SupersededBad bool // A2's start was not A1's supersession with the next token
}

// faults are the model's fault fixtures; next is the step decision (recovery.Next unless mutated).
type faults struct {
	skipLose      bool // step 1 loses nothing
	beforeLose    bool // the controller acts before step 1
	placeEarly    bool // placement does not wait for L1 to settle
	dropLiability bool // the settlement names no call
	next          func(types.Attempt, types.Lease) recovery.Step
	reach         func(mstate) // sees every reachable state
}

func initial() mstate {
	a := lease.Start(a1, unit, l1, base)
	l := types.Lease{ID: l1, Unit: unit, Role: "builder", Executor: "exe_01m3250vm7f6ktw39dszqnydd1", State: types.LeaseStateActive,
		Reservations: []types.LeaseReservationsItem{{Group: quota, Amount: reserve, Unit: types.CapacityUnitTokens}}, Attempt: ptr(a1),
		PlacementChecks: passing()}
	// The controller died mid-call: a new process starts. L2 is not requested yet.
	return mstate{A1: a, L1: l, L2: types.Lease{PlacementChecks: passing()}, Current: a1, Reserved: true}
}

func (s mstate) key() string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func (s mstate) attempt(id types.AttemptID) types.Attempt {
	if id == a1 {
		return s.A1
	}
	return s.A2
}

type move struct {
	name string
	to   mstate
}

// moves is every action enabled in s, with the state it leads to.
func (f faults) moves(s mstate) []move {
	var out []move
	if s.Crashes < maxCrashes {
		n := s
		n.Crashes, n.Lost = s.Crashes+1, false
		out = append(out, move{"crash", n})
	}
	if !s.Lost {
		n := s
		n.Lost = true
		if !f.skipLose {
			n.A1 = lease.LoseOnRestart(s.A1)
			if s.A2.ID != "" {
				n.A2 = lease.LoseOnRestart(s.A2)
			}
		}
		out = append(out, move{"lose", n})
		if !f.beforeLose {
			return out // nothing else runs in a process before step 1
		}
	}
	for _, id := range []types.AttemptID{a1, a2} {
		if n, ok := f.recover(s, id); ok {
			out = append(out, move{"recover " + string(id[len(id)-1:]), n})
		}
	}
	if s.A2.ID == "" && s.L2.State == "" && s.A1.Status != types.AttemptStatusRunning && (f.placeEarly || lease.Ended(s.L1.State)) {
		n := s
		n.L2 = types.Lease{ID: l2, Unit: unit, Role: "builder", Executor: s.L1.Executor, State: types.LeaseStateGranted, Reservations: s.L1.Reservations,
			PlacementChecks: s.L1.PlacementChecks}
		out = append(out, move{"grant L2", n})
	}
	if s.A2.ID == "" && s.L2.State == types.LeaseStateGranted {
		if retired, next, err := lease.Supersede(s.A1, a2, l2, base); err == nil {
			if active, err := lease.ApplyLease(s.L2, lease.LeaseEvent{Kind: lease.AttemptStarted, Attempt: ptr(a2)}, false, nil); err == nil {
				n := s
				n.A1, n.A2, n.L2, n.Current = retired, next, active, a2
				n.StartedEarly = s.StartedEarly || !lease.Ended(s.L1.State)
				n.SupersededBad = s.SupersededBad || next.FencingToken != s.A1.FencingToken+1 || next.Supersedes == nil ||
					*next.Supersedes != a1 || retired.SupersededBy == nil || *retired.SupersededBy != a2
				out = append(out, move{"start A2", n})
			}
		}
	}
	if !s.A1Landed && lease.Fence(s.attempt(s.Current), a1, s.A1.FencingToken) == nil {
		n := s
		n.A1Landed = true
		out = append(out, move{"old output lands", n})
	}
	if !s.Reported {
		n := s
		n.Reported, n.Settled, n.Liability = true, true, false // late usage recomputes the liability (P12b)
		out = append(out, move{"late usage", n})
	}
	return out
}

// recover applies the step Next decides for attempt id under its lease, as recovery's command would; false when
// there is none, or the store would refuse it.
func (f faults) recover(s mstate, id types.AttemptID) (mstate, bool) {
	a, l := s.A1, s.L1
	if id == a2 {
		a, l = s.A2, s.L2
	}
	if a.ID == "" {
		return s, false
	}
	var err error
	n := s
	switch f.next(a, l) {
	case recovery.Revoke:
		if a.Publication == types.AttemptPublicationOpen { // lease.Projector.changeLease refuses it
			return s, false
		}
		l, err = lease.ApplyLease(l, lease.LeaseEvent{Kind: lease.Revoked, Reason: "controller_restart"}, false, nil)
	case recovery.Seal:
		a, err = lease.ApplyAttempt(a, lease.AttemptEvent{Kind: lease.Sealed, Checkpoint: record})
	case recovery.Settle:
		st := types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}
		if id == a1 && !f.dropLiability {
			if s.Settled {
				st.Calls = append(st.Calls, call)
			} else {
				st.Liabilities = append(st.Liabilities, call)
			}
		}
		if l, err = lease.ApplyLease(l, lease.LeaseEvent{Kind: lease.Settled, Settlement: &st}, false, nil); err == nil && id == a1 {
			n.Reserved, n.Liability = false, len(st.Liabilities) > 0
		}
	default:
		return s, false
	}
	if err != nil {
		return s, false
	}
	if id == a1 {
		n.A1, n.L1 = a, l
	} else {
		n.A2, n.L2 = a, l
	}
	return n, true
}

// violated names the invariants s breaks.
func (f faults) violated(s mstate) []string {
	var v []string
	open := 0
	for _, a := range []types.Attempt{s.A1, s.A2} {
		if a.ID != "" && a.Publication == types.AttemptPublicationOpen {
			open++
		}
	}
	for _, c := range []struct {
		bad  bool
		name string
	}{
		{s.A1Landed, "I1 fencing"}, {open > 1, "I2 one publisher"}, {s.StartedEarly, "I3 settle before replace"},
		{s.SupersededBad, "I4 one transaction"}, {!s.Reserved && !s.Settled && !s.Liability, "I5 no dropped charge"},
		{!f.completes(s), "I6 recovery completes"},
	} {
		if c.bad {
			v = append(v, c.name)
		}
	}
	return v
}

// completes: from s, step 1 (if this process has not run it) and then recovery's steps alone end with A1 sealed or
// superseded, L1 settled and nothing left for Next; a restart then, with nothing running, changes nothing.
func (f faults) completes(s mstate) bool {
	drain := func(s mstate) mstate {
		for _, m := range f.moves(s) {
			if m.name == "lose" {
				s = m.to
			}
		}
		for range 6 {
			for _, id := range []types.AttemptID{a1, a2} {
				if n, ok := f.recover(s, id); ok {
					s = n
				}
			}
		}
		return s
	}
	d := drain(s)
	done := (d.A1.Status == types.AttemptStatusSealed || d.A1.Status == types.AttemptStatusSuperseded) && lease.Ended(d.L1.State) &&
		f.next(d.A1, d.L1) == recovery.Done && (d.A2.ID == "" || f.next(d.A2, d.L2) == recovery.Done)
	if !done || d.A2.Status == types.AttemptStatusRunning {
		return done
	}
	again := d
	again.Lost = false
	again = drain(again)
	again.Lost, d.Lost = true, true
	return again.key() == d.key()
}

// explore visits every state reachable from the initial one, and returns the count and each invariant broken, with
// the shortest path to a state breaking it.
func (f faults) explore() (int, map[string]string) {
	if f.next == nil {
		f.next = recovery.Next
	}
	type node struct {
		s    mstate
		path []string
	}
	start := initial()
	seen, broken := map[string]bool{start.key(): true}, map[string]string{}
	queue := []node{{start, nil}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if f.reach != nil {
			f.reach(n.s)
		}
		for _, v := range f.violated(n.s) {
			if _, ok := broken[v]; !ok {
				broken[v] = "[" + strings.Join(n.path, " → ") + "]"
			}
		}
		for _, m := range f.moves(n.s) {
			if k := m.to.key(); !seen[k] {
				seen[k] = true
				queue = append(queue, node{m.to, append(append([]string{}, n.path...), m.name)})
			}
		}
	}
	return len(seen), broken
}

// D62: the bounded model holds every invariant in every reachable state, and reaches the states that matter: A2
// started, a restart after it, the late usage after the settlement.
func TestTheRecoveryModel(t *testing.T) {
	reached := map[string]bool{}
	n, broken := faults{reach: func(s mstate) {
		reached["A2 started"] = reached["A2 started"] || s.A2.ID != ""
		reached["A2 cut by a later restart"] = reached["A2 cut by a later restart"] || s.A2.LostReason != nil
		reached["L2 settled"] = reached["L2 settled"] || s.L2.State == types.LeaseStateSettled
		reached["A1 refused after A2 started"] = reached["A1 refused after A2 started"] ||
			s.A2.ID != "" && lease.Fence(s.attempt(s.Current), a1, 1) != nil
		reached["a liability released by late usage"] = reached["a liability released by late usage"] || lease.Ended(s.L1.State) && s.Reported &&
			len(s.L1.Settlement.Liabilities) == 1
	}}.explore()
	if len(broken) > 0 {
		t.Fatalf("the model breaks %v", broken)
	}
	if len(reached) != 5 || slices.Contains(slices.Collect(maps.Values(reached)), false) {
		t.Fatalf("the model reached %v", reached)
	}
	t.Logf("%d states explored", n)
}

// The model's fault fixtures: each fault turns the invariant it names red.
func TestTheRecoveryModelsFaults(t *testing.T) {
	sealLast := func(a types.Attempt, l types.Lease) recovery.Step {
		if s := recovery.Next(a, l); s != recovery.Seal {
			return s
		}
		return recovery.Settle // settle before the seal
	}
	for name, c := range map[string]struct {
		f    faults
		want string
	}{
		"step 1 loses nothing":                 {faults{skipLose: true}, "I1 fencing"},
		"the controller acts before step 1":    {faults{beforeLose: true}, "I1 fencing"},
		"placement does not wait for L1":       {faults{placeEarly: true}, "I3 settle before replace"},
		"the settlement names no call":         {faults{dropLiability: true}, "I5 no dropped charge"},
		"the lease settles before its attempt": {faults{next: sealLast}, "I6 recovery completes"},
		"recovery revokes, then stops":         {faults{next: onlyRevoke}, "I6 recovery completes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, broken := c.f.explore(); broken[c.want] == "" {
				t.Fatalf("broken %v; want %s", broken, c.want)
			} else {
				t.Logf("%s after %s", c.want, broken[c.want])
			}
		})
	}
}

func onlyRevoke(a types.Attempt, l types.Lease) recovery.Step {
	if s := recovery.Next(a, l); s == recovery.Revoke {
		return s
	}
	return recovery.Done
}
