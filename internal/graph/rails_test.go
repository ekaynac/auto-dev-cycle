// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
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

// TestCapsOf: a profile's count rails, a default for each one absent, and
// its USD rails in micros (P17b); a USD amount that does not parse is 0.
func TestCapsOf(t *testing.T) {
	two := int64(2)
	usd := types.ProjectProfileBudgetRails{PerTaskUsd: "5", PerDayUsd: "20.05"}
	if c := CapsOf(usd); c != (Caps{3, 5, 2, 5_000_000, 20_050_000}) || DefaultCaps != (Caps{3, 5, 2, NoUSDRail, NoUSDRail}) {
		t.Errorf("no count rails: %+v", c)
	}
	usd.RoundsPerTask, usd.PerTaskUsd = &two, "0.5"
	if c := CapsOf(usd); c != (Caps{3, 2, 2, 500_000, 20_050_000}) {
		t.Errorf("rounds_per_task 2, per_task_usd 0.5: %+v", c)
	}
	for _, bad := range []types.USD{"", "x", "-1", "1.234", "1.x"} {
		if m, err := USDMicros(bad); err == nil {
			t.Errorf("USDMicros(%q) = %d", bad, m)
		}
	}
}

// TestRoundRail (P17a): a repair round past its task's round rail is held
// on budget_rail, owned by the operator, before it is claimed; raising the
// rail clears the hold and the round is claimed, while another task's hold
// on the same rail stays; a round within it is claimed at once.
func TestRoundRail(t *testing.T) {
	g := openStore(t)
	build, _ := g.completedWith(types.NodeKindBuild, types.ResultKindPatch, []types.Ref{})
	next, err := g.repair(build, g.reviewed(build, types.OutcomeFound))
	if err != nil {
		t.Fatal(err)
	}
	one := g.env(noRequirement)
	one.Rails = capped(Caps{3, 1, 2, NoUSDRail, NoUSDRail})
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
	otherPlan, _ := g.planOf("tsk_01m3250v9xft08ssr4ygq6g2nq", prjID)
	rec := g.nodeOn(types.NodeKindBuild, otherPlan)
	rec.Task = "tsk_01m3250v9xft08ssr4ygq6g2nq"
	other := g.record(rec)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		h, err := RailHold(RailRounds)
		if err != nil {
			return nil, err
		}
		return Raise(tx, other, h, nil)
	})
	g.must(clearRail(RailRounds))
	if _, held := g.railHold(other, RailRounds); !held {
		t.Error("a raise of one task cleared another task's hold")
	}
	two := g.env(noRequirement)
	two.Rails = capped(Caps{3, 2, 2, NoUSDRail, NoUSDRail})
	g.must(evaluate(two, next.Node.ID))
	if st := g.state(next.Node.ID).State; st != types.NodeStateStateClaimed {
		t.Errorf("round 2 once the rail is 2: %s", st)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return ClearRail(tx, tskID, "per_week_usd") }); err == nil {
		t.Error("an unknown rail cleared")
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
	env.Rails = capped(Caps{3, 5, 3, NoUSDRail, NoUSDRail})
	g.must(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
		raised, err = Operate(tx, gen.Merge, env)
		return nil, err
	})
	if raised.Effect == "" || raised.Rail != "" {
		t.Errorf("the third operation once the rail is 3: %+v", raised)
	}
}

// TestRoundAttempts (P17a; Opus #295 L5): a superseded review's
// replacement keeps its round, so the attempts started on the review count
// against the replacement's attempt rail; a node replacing none counts its
// own unit's alone.
func TestRoundAttempts(t *testing.T) {
	g := openStore(t)
	review, _, next := g.superseding()
	g.runningOn(review)
	count := func(n types.NodeID) (c int64) {
		if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) { c, err = RoundAttempts(tx, n); return err }); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := count(review); c != 1 {
		t.Fatalf("the review's own attempt: %d", c)
	}
	g.must(supersede(review, next))
	rep := *g.state(review).SupersededBy
	if c := count(rep); c != 1 {
		t.Errorf("the replacement's round, before its own attempt: %d", c)
	}
}
