// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// leaseOn requests a lease for executor e on n's unit: a builder attempt's
// lease, whether or not it ever starts.
func (g graphStore) leaseOn(n types.NodeID, e types.ExecutorID) {
	g.t.Helper()
	ok := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	l := types.Lease{ID: types.LeaseID(must(ids.New("lea"))), Unit: UnitOf(n), Role: "builder", Executor: e, State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{}, Footprint: types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok, PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) })
}

// reviewBy records a review of its own subject by exes.
func (g graphStore) reviewBy(exes ...types.ExecutorID) types.NodeID {
	r := g.newNode(types.NodeKindReview)
	r.Node.Params = reviewOf(types.Name(r.Node.ID), exes...)
	return g.record(r)
}

// evaluated is review n's state once evaluated: claimed, or held on
// reviewer_unavailable (ClaimReview refused it as ErrNotIndependent).
func (g graphStore) evaluated(n types.NodeID) string {
	g.t.Helper()
	g.must(evaluate(g.env(noRequirement), n))
	st := g.state(n)
	if st.State == types.NodeStateStateHeld && len(st.Holds) == 1 {
		return string(st.Holds[0].Hold)
	}
	return string(st.State)
}

// TestContributorFamilies (I7, P16b; fault 1): a review's claim refuses a
// roster member of a family that holds a lease on a build node of its
// task, another executor of that family included, and any member once a
// contributor's family is unknown; another task's builds and the task's
// other nodes do not contribute.
func TestContributorFamilies(t *testing.T) {
	g := openStore(t)
	const claimed, refused = "claimed", "reviewer_unavailable"
	if got := g.evaluated(g.reviewBy(exeA)); got != claimed {
		t.Fatalf("no builder attempt yet: %s", got)
	}
	gate := g.record(g.newNode(types.NodeKindGate))
	g.leaseOn(gate, exeA) // not a build: no contribution
	other := types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2nq")
	otherPlan, _ := g.planOf(other, prjID)
	otherBuild := g.nodeOn(types.NodeKindBuild, otherPlan)
	otherBuild.Task = other
	g.leaseOn(g.record(otherBuild), exeA) // another task's build
	if got := g.evaluated(g.reviewBy(exeA)); got != claimed {
		t.Errorf("a gate's lease and another task's build contributed: %s", got)
	}
	build := g.record(g.newNode(types.NodeKindBuild))
	g.leaseOn(build, exeB) // claude, another executor than the reviewer's
	if got := g.evaluated(g.reviewBy(exeA)); got != refused {
		t.Errorf("a reviewer of a contributing family (fault 1): %s", got)
	}
	if got := g.evaluated(g.reviewBy(exeA, exeC)); got != refused {
		t.Errorf("a roster with one member of a contributing family: %s", got)
	}
	if got := g.evaluated(g.reviewBy(exeC)); got != claimed {
		t.Errorf("a reviewer of another family: %s", got)
	}
	g.leaseOn(g.record(g.newNode(types.NodeKindBuild)), exeX) // a repair round on an unregistered executor
	if got := g.evaluated(g.reviewBy(exeC)); got != refused {
		t.Errorf("an unknown contributor: %s", got)
	}
	var got []string
	if err := g.s.View(t.Context(), func(tx *eventstore.Tx) (err error) {
		got, err = Contributors(tx, tskID, reviewers)
		return err
	}); err != nil || !slices.Equal(got, []string{"claude", "unknown"}) {
		t.Errorf("the contributors: %v %v", got, err)
	}
}

// reviewReceipt is a review receipt by families, about treeA.
func (g graphStore) reviewReceipt(families ...string) types.Ref {
	doc := receiptDoc(treeSubject(treeA, demandA), nil)
	doc["kind"] = "review"
	doc["provenance"].(map[string]any)["model_families"] = families
	return g.sealed(types.RefTypeReceipt, rcpID, g.derived(doc))
}

// TestReviewCommitIndependent (I7, P16b): a review's result is committed
// only by a roster independent of the contributors at the commit, a
// builder attempt after the claim included; a review receipt it holds is
// checked by receipt.Independent against them.
func TestReviewCommitIndependent(t *testing.T) {
	g := openStore(t)
	env := g.env(noRequirement)
	refused := func(err error) bool {
		var e ErrNotIndependent
		return errors.As(err, &e)
	}
	receiptRefused := func(err error) bool {
		var r *receipt.Refusal
		return errors.As(err, &r) && r.Reason == vocab.ReasonReviewerNotIndependent
	}
	g.leaseOn(g.record(g.newNode(types.NodeKindBuild)), exeA) // claude contributes
	claimed := func() types.NodeID {
		n := g.reviewBy(exeC)
		if got := g.evaluated(n); got != "claimed" {
			t.Fatalf("the review is %s", got)
		}
		return n
	}
	n := claimed()
	r := g.reviewResult(n)
	r.Value = []types.Ref{g.reviewReceipt("claude")}
	if err := g.do(commit(n, r, env)); !receiptRefused(err) {
		t.Errorf("a review receipt by a contributing family: %v", err)
	}
	r = g.reviewResult(n)
	r.Value = []types.Ref{g.reviewReceipt("gpt")}
	if err := g.do(commit(n, r, env)); err != nil {
		t.Errorf("a review receipt by another family: %v", err)
	}
	later := claimed()
	g.leaseOn(g.record(g.newNode(types.NodeKindBuild)), exeC) // gpt builds after the claim
	if err := g.do(commit(later, g.reviewResult(later), env)); !refused(err) {
		t.Errorf("a contributor after the claim: %v", err)
	}
}
