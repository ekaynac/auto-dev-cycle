// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// capped are rails with caps for every task.
func capped(c Caps) Rails {
	return func(*eventstore.Tx, types.TaskID) (Caps, error) { return c, nil }
}

// railHold is n's budget_rail hold on rail, if it has one.
func (g graphStore) railHold(n types.NodeID, rail string) (types.NodeHold, bool) {
	for _, h := range g.state(n).Holds {
		if h.Hold == "budget_rail" && h.SignalInstance == RailSignal(rail) {
			return h, true
		}
	}
	return types.NodeHold{}, false
}

func clearRail(rail string) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return ClearRail(tx, tskID, rail) }
}

// TestCapsOf: a profile's count rails, a default for each one absent.
func TestCapsOf(t *testing.T) {
	two := int64(2)
	if c := CapsOf(types.ProjectProfileBudgetRails{}); c != DefaultCaps || c != (Caps{3, 5, 2}) {
		t.Errorf("no count rails: %+v", c)
	}
	if c := CapsOf(types.ProjectProfileBudgetRails{RoundsPerTask: &two}); c != (Caps{3, 2, 2}) {
		t.Errorf("rounds_per_task 2: %+v", c)
	}
}

// TestRoundRail (P17a): a repair round past its task's round rail is held
// on budget_rail, owned by the operator, before it is claimed; raising the
// rail clears the hold and the round is claimed; a round within it is
// claimed at once.
func TestRoundRail(t *testing.T) {
	g := openStore(t)
	build, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	next, err := g.repair(build, g.reviewed(build, types.OutcomeFound))
	if err != nil {
		t.Fatal(err)
	}
	one := g.env(noRequirement)
	one.Rails = capped(Caps{3, 1, 2})
	g.must(evaluate(one, next.Node.ID))
	h, held := g.railHold(next.Node.ID, RailRounds)
	if st := g.state(next.Node.ID); !held || st.State != types.NodeStateStateHeld || h.Owner != types.OwnerOperator {
		t.Fatalf("round 2 over a rail of 1: %+v", st)
	}
	g.must(evaluate(one, next.Node.ID)) // held: no second hold
	if hs := g.state(next.Node.ID).Holds; len(hs) != 1 {
		t.Errorf("evaluated again: %d holds", len(hs))
	}
	g.must(clearRail(RailMergeOps)) // another rail's raise clears nothing
	if _, held := g.railHold(next.Node.ID, RailRounds); !held {
		t.Error("another rail's raise cleared the round rail")
	}
	g.must(clearRail(RailRounds))
	two := g.env(noRequirement)
	two.Rails = capped(Caps{3, 2, 2})
	g.must(evaluate(two, next.Node.ID))
	if st := g.state(next.Node.ID).State; st != types.NodeStateStateClaimed {
		t.Errorf("round 2 once the rail is 2: %s", st)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return ClearRail(tx, tskID, "per_day_usd") }); err == nil {
		t.Error("a USD rail cleared as a count rail")
	}
}

// TestMergeRail (P17a, fault 1: "a counter reset across an invalidation"):
// a merge node's operations count across its invalidations; at its rail
// the next operation is not intended, and the node is held on budget_rail;
// raising the rail lets it operate.
func TestMergeRail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	g := openEffectStoreAt(t, dir)
	gen := g.claimedMerge("main", "B1")
	for i := range 2 { // the default rail: 2
		op := g.mustOperate(gen.Merge)
		if op.Effect == "" || op.Rail != "" {
			t.Fatalf("operation %d: %+v", i+1, op)
		}
		effectsSQL(t, dir, `UPDATE effects SET state = 'failed_confirmed' WHERE id = ?`, op.Effect)
		g.must(invalidate(gen.Merge))
	}
	op := g.mustOperate(gen.Merge)
	if _, held := g.railHold(gen.Merge, RailMergeOps); op.Effect != "" || op.Rail != RailMergeOps || !held {
		t.Fatalf("a third operation over a rail of 2: %+v, held %v", op, held)
	}
	g.must(clearRail(RailMergeOps))
	var raised Operation
	env := effectEnv
	env.Rails = capped(Caps{3, 5, 3})
	g.must(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
		raised, err = Operate(tx, gen.Merge, env)
		return nil, err
	})
	if raised.Effect == "" || raised.Rail != "" {
		t.Errorf("the third operation once the rail is 3: %+v", raised)
	}
}
