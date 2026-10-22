// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
)

// round is the task's nodes of one build round: the build, the accept and
// gate on its candidate, and the review that takes that gate.
type round struct{ build, accept, gate, review graph.NodeView }

// roundOf is rw's round n (build round n and the chain on its candidate).
func roundOf(t *testing.T, rw *runWorld, n int64) round {
	t.Helper()
	var r round
	rw.w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.TaskNodes(tx, rw.task)
		takes := func(v graph.NodeView, kind types.ResultKind, producer types.NodeID) bool {
			for _, in := range v.Node.Inputs {
				if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == kind && src.Producer == producer {
					return true
				}
			}
			return false
		}
		for _, v := range nodes {
			if v.Node.Kind == types.NodeKindBuild && v.Node.Round == n {
				r.build = v
			}
		}
		for _, v := range nodes {
			if v.Node.Kind == types.NodeKindAccept && takes(v, types.ResultKindPatch, r.build.Node.ID) {
				r.accept = v
			}
		}
		for _, v := range nodes {
			switch {
			case v.Node.Kind == types.NodeKindGate && takes(v, types.ResultKindTree, r.accept.Node.ID):
				r.gate = v
			case v.Node.Kind == types.NodeKindReview && takes(v, types.ResultKindPatch, r.build.Node.ID) && v.State.State != types.NodeStateStateSuperseded:
				r.review = v
			}
		}
		return err
	})
	return r
}

// repairIntent is the intent the first attempt of build recorded, and its
// prompt as intent.Build renders it from the log.
func repairIntent(t *testing.T, rw *runWorld, build types.NodeID) (types.ManifestIntent, []byte) {
	t.Helper()
	var aid types.AttemptID
	var prompt []byte
	rw.w.view(func(tx *eventstore.Tx) error {
		first, ok, err := lease.FirstOn(tx, graph.UnitOf(build))
		if err != nil || !ok {
			t.Fatalf("no attempt on the repair round: %v", err)
		}
		aid = first
		rendered, err := graph.Context(tx, build, rw.r.env)
		if err != nil {
			return err
		}
		pc, err := intent.Build(tx, rw.r.blobs, build, rendered)
		prompt = pc.Prompt
		return err
	})
	mi, ok := recordedIntent(t, rw.w, aid)
	if !ok {
		t.Fatal("no intent recorded on the repair round")
	}
	return mi, prompt
}

// untilAttempt runs rw's passes until build has an attempt (at most 12).
func untilAttempt(t *testing.T, rw *runWorld, build types.NodeID) {
	t.Helper()
	for range 12 {
		rw.passesFor(t, 1, 0)
		started := false
		rw.w.view(func(tx *eventstore.Tx) (err error) {
			_, started, err = lease.FirstOn(tx, graph.UnitOf(build))
			return err
		})
		if started {
			return
		}
	}
	t.Fatal("the repair round never ran")
}

// P22a-6d: a no_go repairs the build. In one command, round 2's build (on
// the plan, the review's findings and the candidate), its accept and gate,
// and its review (on the same requirement and roster), so that review is
// recorded before its gate completes (Opus 22a-6c L1). The repair builder's
// intent lists the brief, the findings view (BuilderSafe) and the candidate
// only, never the decision or the gate's report, and its prompt lists the
// findings, the gate's test id redacted.
func TestANoGoRepairsTheBuild(t *testing.T) {
	rw, _ := reviewRun(t, noGoDecision)
	var log bytes.Buffer // no review action refused in any pass: a review is repaired once
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
	one := roundOf(t, rw, 1)
	if one.review.State.State != types.NodeStateStateCompleted {
		t.Fatalf("round 1's review: %s", one.review.State.State)
	}
	rw.passesFor(t, 1, 0)
	two := roundOf(t, rw, 2)
	if two.build.Node.ID == "" || two.accept.Node.ID == "" || two.gate.Node.ID == "" || two.review.Node.ID == "" {
		t.Fatalf("round 2: %+v", two)
	}
	if two.gate.State.State == types.NodeStateStateCompleted || !graph.TakesGate(two.review.Node, two.gate.Node.ID) {
		t.Fatalf("round 2's review is not recorded before its gate: gate %s", two.gate.State.State)
	}
	p1, _ := one.review.Node.ParamsAsReviewNodeParams()
	p2, _ := two.review.Node.ParamsAsReviewNodeParams()
	if len(p1.Roster) != 1 || len(p2.Roster) != 1 || p1.Roster[0] != p2.Roster[0] {
		t.Errorf("round 2's roster %+v, round 1's %+v", p2.Roster, p1.Roster)
	}
	untilAttempt(t, rw, two.build.Node.ID)
	mi, prompt := repairIntent(t, rw, two.build.Node.ID)
	if len(mi.Inputs) != 3 || len(mi.Included) != 3 || len(mi.Omitted) != 0 || mi.Included[1].Ref.Type != types.RefTypeView ||
		mi.Included[0].Ref.Type != types.RefTypeBlob || mi.Included[2].Ref.Type != types.RefTypeBlob {
		t.Fatalf("the repair builder's intent: inputs %+v", mi.Inputs)
	}
	p := string(prompt)
	findings, candidate, _ := strings.Cut(p, "## Your previous candidate (unified diff)") // the candidate's own diff holds what it wrote
	if !strings.Contains(findings, "Its reviewer decided no_go") || !strings.Contains(findings, "- [high]") || !strings.Contains(findings, "wrong refund") ||
		strings.Contains(findings, hiddenTest) || !strings.Contains(candidate, "```diff") {
		t.Errorf("the repair builder's prompt: %s", p)
	}
	// Round 2 runs through its own accept, gate and review (the review
	// recorded first is the one claimed and run), and its no_go repairs
	// again: round 3.
	for range 24 {
		if roundOf(t, rw, 3).build.Node.ID != "" {
			break
		}
		rw.passesFor(t, 1, 0)
	}
	if two = roundOf(t, rw, 2); two.review.State.State != types.NodeStateStateCompleted || roundOf(t, rw, 3).build.Node.ID == "" {
		t.Fatalf("round 2's review %s, round 3 %v", two.review.State.State, roundOf(t, rw, 3).build.Node.ID)
	}
	rw.w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.TaskNodes(tx, rw.task)
		reviews := 0
		for _, n := range nodes {
			if n.Node.Kind == types.NodeKindReview && graph.TakesGate(n.Node, two.gate.Node.ID) {
				reviews++
			}
		}
		if reviews != 1 {
			t.Errorf("%d reviews of round 2's gate", reviews)
		}
		return err
	})
	if strings.Contains(log.String(), "reconcile.review refused") {
		t.Errorf("a review action was refused: %s", log.String())
	}
}

// Opus 22a-6c L4: an unreviewable verdict (failed/unsafe_output) repairs
// the build too: its builder is told why (the verdict's detail), and the
// candidate over review.MaxDiff is omitted for budget, not shown.
func TestAnUnreviewableVerdictRepairsTheBuild(t *testing.T) {
	big := strings.Repeat("a long line of the candidate's own text\n", 16<<10)
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"big.txt": big}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("the gate: %s", v.State.State)
	}
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	var two round
	for range 12 {
		rw.passesFor(t, 1, 0)
		if two = roundOf(t, rw, 2); two.build.Node.ID != "" {
			break
		}
	}
	if two.build.Node.ID == "" || two.review.Node.ID == "" {
		t.Fatalf("no round 2 after an unreviewable verdict: %+v", roundOf(t, rw, 1).review.State)
	}
	untilAttempt(t, rw, two.build.Node.ID)
	mi, prompt := repairIntent(t, rw, two.build.Node.ID)
	if len(mi.Included) != 1 || len(mi.Omitted) != 1 || mi.Omitted[0].Reason != types.ManifestIntentOmittedItemReasonBudget {
		t.Fatalf("the repair builder's intent: included %+v, omitted %+v", mi.Included, mi.Omitted)
	}
	if p := string(prompt); !strings.Contains(p, "It could not be reviewed:") || !strings.Contains(p, "over 524288 bytes") {
		t.Errorf("the repair builder's prompt: %s", p)
	}
}
