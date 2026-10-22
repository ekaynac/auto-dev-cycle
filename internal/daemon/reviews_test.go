// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// stubReviewer is the stub registry's reviewer.
const stubReviewer = types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")

// stubRegistry is the stub registry with its executors edited: builders
// and reviewers each the stub builder's or reviewer's entry under that id
// and family (nil: the stub's own), the account's group listing exactly
// them.
func stubRegistry(t *testing.T, builders, reviewers map[types.ExecutorID]types.Name) registry.Registry {
	t.Helper()
	const config = "../../fixtures/m1/config/"
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []types.ResourceGroup
	raw, err := os.ReadFile(config + "groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	tmpl := map[types.RegistryExecutorRole]types.RegistryExecutor{}
	for _, e := range doc.Executors {
		tmpl[e.Role] = e
	}
	var executors []types.RegistryExecutor
	for role, set := range map[types.RegistryExecutorRole]map[types.ExecutorID]types.Name{types.RegistryExecutorRoleBuilder: builders, types.RegistryExecutorRoleReviewer: reviewers} {
		if set == nil {
			executors = append(executors, tmpl[role])
		}
		for id, family := range set {
			e := tmpl[role]
			e.ID, e.Family = id, family
			executors = append(executors, e)
		}
	}
	slices.SortFunc(executors, func(a, b types.RegistryExecutor) int { return strings.Compare(string(a.ID), string(b.ID)) })
	doc.Executors = executors
	for i, g := range gs { // the account's group lists exactly the registered executors on it
		if slices.Contains(g.Members, stubReviewer) {
			gs[i].Members = []types.ExecutorID{}
			for _, e := range executors {
				if e.Group == g.ID {
					gs[i].Members = append(gs[i].Members, e.ID)
				}
			}
		}
	}
	reg, err := registry.New(doc, gs)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// reviewing makes rw's loop act on reviews under reg (an adcd start with
// it): the review gate open, the reviews and the placer under reg; a
// gate's run is 22a-5c-2b-3's (gateWorld).
func (rw *runWorld) reviewing(reg registry.Registry) {
	rw.x.Env.Families = reg
	rw.x.Reviews = reviews{reg: reg, env: rw.x.Env}
	pl := rw.x.Placer.(placer)
	pl.d.Registry = reg
	rw.x.Placer = pl
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, io.Discard)
}

// reviewNodes are the task's review and requirement nodes, by kind, the
// last of each kind in record order that is not superseded.
func reviewNodes(rw *runWorld) map[types.NodeKind]graph.NodeView {
	out := map[types.NodeKind]graph.NodeView{}
	rw.w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.TaskNodes(tx, rw.task)
		for _, n := range nodes {
			if (n.Node.Kind == types.NodeKindReview || n.Node.Kind == types.NodeKindReviewRequirement) && n.State.State != types.NodeStateStateSuperseded {
				out[n.Node.Kind] = n
			}
		}
		return err
	})
	return out
}

// gateCompleted is a run world whose task's gate completed green.
func gateCompleted(t *testing.T) *runWorld {
	t.Helper()
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s, holds %+v", v.State.State, v.State.Holds)
	}
	return rw
}

// P22a-6a: once the gate completed, the loop records the task's review
// late, with the roster the registry derives (lead/p22a6a-design.md): its
// requirement, committed by the controller (its conclusions its params),
// and the review on the plan, the patch, the gate's result and the
// requirement, rostered on the registry's independent reviewer, claimed
// (P16b's claim half), then placed on that reviewer.
func TestTheReviewIsRecordedOnceTheGateCompleted(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 6, 0)
	got := reviewNodes(rw)
	review, req := got[types.NodeKindReview], got[types.NodeKindReviewRequirement]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Executor != stubReviewer || req.State.State != types.NodeStateStateCompleted ||
		review.State.State != types.NodeStateStateClaimed || len(review.Node.Inputs) != 3 {
		t.Fatalf("the review: %+v (%v), its requirement %s", review, err, req.State.State)
	}
	if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer || l.Role != "reviewer" {
		t.Errorf("the review's lease: %v %+v", live, l)
	}
}

// P22a-6a: with review inactive, nothing of the review is recorded.
func TestNoReviewWhileReviewIsInactive(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.passesFor(t, 4, 0)
	if got := reviewNodes(rw); len(got) != 0 {
		t.Errorf("review nodes while review is inactive: %+v", got)
	}
}

// P22a-6a (Main: the hold clears by itself; the Unblock pattern):
//   - a start whose registry no longer knows the build's executor (its
//     contributor's family unknown: no reviewer is independent of it) holds
//     the review reviewer_unavailable, and the hold stays while that is so;
//     a restart with the builder known again clears it, and the review is
//     claimed and placed;
//   - a start whose registry derives another family for the review's
//     reviewer (an independent one) holds it, then records the
//     requirement's next revision on that roster and supersedes the review
//     onto it, and the replacement is claimed and placed.
func TestAReviewerUnavailableHoldClearsByItself(t *testing.T) {
	t.Run("a contributor unknown, then known", func(t *testing.T) {
		rw := gateCompleted(t)
		rw.reviewing(stubRegistry(t, nil, nil))
		rw.passesFor(t, 2, 0) // the review and its requirement recorded, the requirement committed
		other := types.ExecutorID(newID(t, "exe"))
		rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{other: "stub-builder"}, nil)) // a restart: the build's executor unknown
		rw.passesFor(t, 6, 0)
		held := reviewNodes(rw)[types.NodeKindReview]
		if held.State.State != types.NodeStateStateHeld || len(held.State.Holds) != 1 || held.State.Holds[0].Hold != vocab.HoldReviewerUnavailable {
			t.Fatalf("the review with its contributor unknown: %s, holds %+v", held.State.State, held.State.Holds)
		}
		rw.passesFor(t, 4, 0)
		if still := reviewNodes(rw)[types.NodeKindReview]; len(still.State.Holds) != 1 || still.State.Holds[0].ID != held.State.Holds[0].ID {
			t.Fatalf("the hold did not stay while no reviewer is independent: %+v, was %+v", still.State.Holds, held.State.Holds)
		}
		rw.reviewing(stubRegistry(t, nil, nil)) // a restart with the builder known again
		rw.passesFor(t, 6, 0)
		review := reviewNodes(rw)[types.NodeKindReview]
		if review.Node.ID != held.Node.ID || review.State.State != types.NodeStateStateClaimed {
			t.Fatalf("the review after its contributor is known: %s, holds %+v", review.State.State, review.State.Holds)
		}
		if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer {
			t.Errorf("the review's lease: %v %+v", live, l)
		}
	})
	t.Run("its reviewer's family changed", func(t *testing.T) {
		rw := gateCompleted(t)
		rw.reviewing(stubRegistry(t, nil, nil))
		rw.passesFor(t, 2, 0)
		first := reviewNodes(rw)[types.NodeKindReview]
		rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "another-reviewer"})) // a restart
		rw.passesFor(t, 10, 0)
		review := reviewNodes(rw)[types.NodeKindReview]
		p, err := review.Node.ParamsAsReviewNodeParams()
		if err != nil || review.Node.ID == first.Node.ID || len(p.Roster) != 1 || p.Roster[0].Family != "another-reviewer" || review.State.State != types.NodeStateStateClaimed {
			t.Fatalf("the review after its reviewer's family changed: %+v (%v)", review, err)
		}
		if l, live := leaseOn(rw.w, review.Node.ID); !live || l.Executor != stubReviewer {
			t.Errorf("the replacement's lease: %v %+v", live, l)
		}
	})
}

// Mythos #337 L2: with review active, Validate refuses a configuration
// with no registry, or a registry with no reviewer.
func TestReviewNeedsARegistryWithAReviewer(t *testing.T) {
	reviewActive = func() bool { return true }
	t.Cleanup(func() {
		reviewActive = func() bool { return false }
	})
	if err := (Config{StateDir: "/srv/adc/state"}).Validate(); err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Errorf("review active, no registry: %v", err)
	}
}

// Mythos #337 L2, while review is inactive: adcd states at start, once,
// that reviews hold with no reviewer registered, and says nothing with one.
func TestAStartWithNoReviewerSaysReviewsHold(t *testing.T) {
	for name, c := range map[string]struct {
		reg  registry.Registry
		says bool
	}{"no registry": {registry.Registry{}, true}, "a reviewer registered": {stubRegistry(t, nil, nil), false}} {
		var log bytes.Buffer
		reconciler(&Daemon{}, c.reg, nil, nil, nil, 0, &log)
		if says := strings.Contains(log.String(), "reviews hold until a registry with a reviewer is given"); says != c.says {
			t.Errorf("%s: the start log %q", name, log.String())
		}
	}
}

// P22a-6a (P16b): the roster is the first admitted reviewer of no
// contributor's family: a start whose registry names the build's executor
// as a reviewer (its family the contributor's) records the review on
// another reviewer, never on it.
func TestTheRosterIsIndependentOfTheContributors(t *testing.T) {
	rw := gateCompleted(t)
	builder := types.ExecutorID(newID(t, "exe"))
	rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{builder: "another-builder"},
		map[types.ExecutorID]types.Name{stubBuilder: "stub-builder", stubReviewer: "stub-reviewer"}))
	rw.passesFor(t, 1, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Executor != stubReviewer {
		t.Errorf("the roster with a contributor's family among the reviewers: %+v (%v)", p.Roster, err)
	}
}

// Opus 22a-6a H1: two restarts with other families, the second right after
// the first's supersession (its replacement recorded, not yet claimed):
// each re-derivation records the next revision of the review's own
// requirement and supersedes onto it in one command, so the replacement is
// claimed on the last roster, and no pass refuses a review action.
func TestTwoRestartsSupersedeOntoTheLastRoster(t *testing.T) {
	rw := gateCompleted(t)
	rw.reviewing(stubRegistry(t, nil, nil))
	rw.passesFor(t, 2, 0)
	rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"}))
	rw.passesFor(t, 2, 0) // held, then superseded onto revision 2: its replacement waits for it
	var log bytes.Buffer
	rw.reviewing(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-c"}))
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
	rw.passesFor(t, 10, 0)
	review := reviewNodes(rw)[types.NodeKindReview]
	p, err := review.Node.ParamsAsReviewNodeParams()
	if err != nil || len(p.Roster) != 1 || p.Roster[0].Family != "fam-c" || review.State.State != types.NodeStateStateClaimed {
		t.Fatalf("the review after two restarts: %s, roster %+v (%v), holds %+v", review.State.State, p.Roster, err, review.State.Holds)
	}
	if strings.Contains(log.String(), "reconcile.review refused") {
		t.Errorf("a review action was refused: %s", log.String())
	}
}

// Opus 22a-6a M1: the loop commits only a requirement a review that is not
// superseded takes: a claimed requirement node no review takes (an orphan)
// is never given to Require.
func TestAnOrphanRequirementIsNotCommitted(t *testing.T) {
	rw := gateCompleted(t)
	reg := stubRegistry(t, nil, nil)
	rw.reviewing(reg)
	rw.passesFor(t, 2, 0)
	roster, err := graph.Roster([]types.ExecutorID{stubReviewer}, reg)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := reviews{reg: reg}.requirement(rw.task, graphProject, roster, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.RecordReview(orphan, reg) }); err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.ClaimReview(tx, orphan.Node.ID, map[string]graph.Resolved{}, 1, reg)
	}); err != nil {
		t.Fatal(err)
	}
	var s reconcile.Snapshot
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		s, err = reconcile.Observe(tx, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true})
		return err
	})
	for _, v := range s.Requirements {
		if v.ID == string(orphan.Node.ID) {
			t.Errorf("the orphan requirement is given to Require: %+v", s.Requirements)
		}
	}
}
