// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
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
	findings, candidate, _ := strings.Cut(p, "## Your round 1 candidate (candidate-1, unified diff)") // the candidate's own diff holds what it wrote
	if !strings.Contains(findings, "## The review of your round 1 candidate (findings-1)") || !strings.Contains(findings, "Its reviewer decided no_go") || !strings.Contains(findings, "- [high]") || !strings.Contains(findings, "wrong refund") ||
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
	// Opus 22a-6d H1 (#226 entry 161): the reviewer repeats its decision
	// and the builder its patch (the same bytes, so the same
	// content-addressed ids, each round's decision of another lineage), and
	// the rounds go on: round 3 runs (its attempt starts on the findings of
	// two rounds), and the rounds rail (5) holds round 6, the repeats'
	// only stop.
	untilAttempt(t, rw, roundOf(t, rw, 3).build.Node.ID)
	one, two = roundOf(t, rw, 1), roundOf(t, rw, 2)
	if one.build.State.Result == nil || two.build.State.Result == nil || one.review.State.Result == nil || two.review.State.Result == nil {
		t.Fatal("rounds 1 and 2 hold no results")
	}
	pa, pb, da, db := selectedRef(t, rw, one.build), selectedRef(t, rw, two.build), selectedRef(t, rw, one.review), selectedRef(t, rw, two.review)
	if pa.ID != pb.ID || da.ID != db.ID || slices.Equal(da.Lineage, db.Lineage) {
		t.Fatalf("the rounds do not repeat their patch and their decision under another lineage: %+v %+v / %+v %+v", pa, pb, da, db)
	}
	var six round
	for range 120 {
		if six = roundOf(t, rw, 6); six.build.Node.ID != "" && slices.ContainsFunc(six.build.State.Holds, func(h types.NodeHold) bool {
			return h.Hold == vocab.HoldBudgetRail && h.SignalInstance == graph.RailSignal(graph.RailRounds)
		}) {
			break
		}
		rw.passesFor(t, 1, 0)
	}
	if six.build.Node.ID == "" || len(six.build.State.Holds) == 0 {
		t.Fatalf("round 6: %+v", six.build.State)
	}
	for n := int64(3); n <= 5; n++ {
		if r := roundOf(t, rw, n); r.review.State.State != types.NodeStateStateCompleted {
			t.Errorf("round %d's review: %s", n, r.review.State.State)
		}
	}
	if strings.Contains(log.String(), "refused") || strings.Contains(log.String(), "runner: failed") {
		t.Errorf("an action was refused: %s", log.String())
	}
}

// selectedRef is the first ref of v's selected result.
func selectedRef(t *testing.T, rw *runWorld, v graph.NodeView) types.Ref {
	t.Helper()
	refs := selectedRefs(t, rw, v)
	if len(refs) == 0 {
		t.Fatalf("%s selects a result with no ref", v.Node.ID)
	}
	return refs[0]
}

// Astra chunk 4b H1: a blob a repair builder's intent reads whose stored
// bytes are not its digest's (the no_go's decision, or the gate's report
// its lineage holds) is the host's store damaged: the round's build is held
// on a bounded retry, never refused as acceptance-derived nor held as the
// content's. Repaired within the retry window, the repair runs; left
// damaged, the retries are spent and it is held "runner: failed", visibly.
func TestADamagedBlobUnderARepairIsRetried(t *testing.T) {
	retryBase = 50 * time.Millisecond
	t.Cleanup(func() { retryBase = time.Minute })
	decision := func(refs []types.Ref) types.Digest { // the one the findings view names
		for _, v := range refs {
			for _, x := range refs {
				if v.Type == types.RefTypeView && len(v.Lineage) == 1 && x.ID == v.Lineage[0] {
					return x.ContentDigest
				}
			}
		}
		return ""
	}
	report := func(refs []types.Ref) types.Digest {
		for _, x := range refs {
			if x.Type == types.RefTypeBlob {
				return x.ContentDigest
			}
		}
		return ""
	}
	for _, c := range []struct {
		name     string
		of       func(round) graph.NodeView // the node whose selected result holds the blob
		blob     func([]types.Ref) types.Digest
		repaired bool
	}{
		{"a damaged decision, repaired", func(r round) graph.NodeView { return r.review }, decision, true},
		{"a damaged report, left so", func(r round) graph.NodeView { return r.gate }, report, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rw, _ := reviewRun(t, noGoDecision)
			d := c.blob(selectedRefs(t, rw, c.of(roundOf(t, rw, 1))))
			if d == "" {
				t.Fatal("no such blob")
			}
			hex := strings.TrimPrefix(string(d), "sha256:")
			p := filepath.Join(rw.w.dir, "blobs", "sha256", hex[:2], hex[2:4], hex)
			good, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			write := func(b []byte) {
				if err := os.Chmod(p, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(append(slices.Clone(good), '\n'))
			var two round
			for range 12 {
				if rw.passesFor(t, 1, 0); len(roundOf(t, rw, 2).build.State.Holds) > 0 {
					break
				}
			}
			if two = roundOf(t, rw, 2); len(two.build.State.Holds) != 1 || !strings.HasPrefix(two.build.State.Holds[0].SignalInstance, runnerText("retry 1 until ")) {
				t.Fatalf("round 2's build over a damaged blob: %s, holds %+v", two.build.State.State, two.build.State.Holds)
			}
			if c.repaired {
				write(good)
				for range 40 {
					rw.passesFor(t, 1, 25*time.Millisecond)
					started := false
					rw.w.view(func(tx *eventstore.Tx) (err error) {
						_, started, err = lease.FirstOn(tx, graph.UnitOf(two.build.Node.ID))
						return err
					})
					if started {
						return
					}
				}
				t.Fatalf("the repair never ran once its blob was repaired: %+v", roundOf(t, rw, 2).build.State.Holds)
			}
			for range 60 {
				if rw.passesFor(t, 1, 50*time.Millisecond); slices.ContainsFunc(roundOf(t, rw, 2).build.State.Holds, func(h types.NodeHold) bool { return h.SignalInstance == runnerFailed }) {
					return
				}
			}
			t.Fatalf("a damaged blob never held visibly: %+v", roundOf(t, rw, 2).build.State.Holds)
		})
	}
}

// selectedRefs are v's selected result's refs.
func selectedRefs(t *testing.T, rw *runWorld, v graph.NodeView) []types.Ref {
	t.Helper()
	for _, rr := range reviewResults(rw, v.Node.ID) {
		if v.State.Result != nil && rr.Result.ID == *v.State.Result {
			return rr.Result.Value
		}
	}
	t.Fatalf("%s selects no result", v.Node.ID)
	return nil
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

// Main (6d): a registry change between a no_go and its repair never leaves
// the task stuck unheld. The repair records at once, its review on its
// requirement's roster, which the registry no longer gives
// (graph.RecordRoundReview); round 2 runs, its review is held
// reviewer_unavailable at its claim (graph.HoldNotIndependent), and
// reviews.Unheld supersedes it onto the registry's independent roster,
// which is claimed and run. No review action is refused in any pass.
func TestARegistryChangeBeforeARepairIsHeldNotStuck(t *testing.T) {
	rw, _ := reviewRun(t, noGoDecision)
	rw.reviewRunning(stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"}), reviewer(noGoDecision, false, nil))
	var log bytes.Buffer
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true}, time.Hour, &log)
	rw.passesFor(t, 1, 0)
	if two := roundOf(t, rw, 2); two.build.Node.ID == "" || two.review.Node.ID == "" {
		t.Fatalf("the repair did not record: %+v", two)
	}
	held, onB := false, false
	for range 30 {
		rw.passesFor(t, 1, 0)
		two := roundOf(t, rw, 2)
		held = held || slices.ContainsFunc(two.review.State.Holds, func(h types.NodeHold) bool { return h.Hold == vocab.HoldReviewerUnavailable })
		p, _ := two.review.Node.ParamsAsReviewNodeParams()
		if onB = len(p.Roster) == 1 && p.Roster[0].Family == "fam-b" &&
			(two.review.State.State == types.NodeStateStateClaimed || two.review.State.State == types.NodeStateStateCompleted); onB {
			break
		}
	}
	if !held || !onB {
		t.Fatalf("round 2's review: held %v, then on fam-b %v (%s)", held, onB, roundOf(t, rw, 2).review.State.State)
	}
	if strings.Contains(log.String(), "refused") {
		t.Errorf("an action was refused: %s", log.String())
	}
}

// graph.RecordRoundReview records an unregistered roster only when it is
// its recorded requirement's: a round review on another roster is refused.
func TestARoundReviewOnAnotherRosterIsRefused(t *testing.T) {
	rw, _ := reviewRun(t, noGoDecision)
	one := roundOf(t, rw, 1)
	plan, _ := producerOfView(t, rw, one.review.Node.ID, types.ResultKindPlan)
	required, _ := producerOfView(t, rw, one.review.Node.ID, types.ResultKindReviewRequirement)
	other := []types.RosterMember{{Executor: stubReviewer, Family: "fam-x"}}
	r, err := reviewNode(rw.task, graphProject, plan, one.build.Node.ID, one.gate.Node.ID, required, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.RecordRoundReview(tx, r, rw.r.env.Families)
	}); err == nil || !strings.Contains(err.Error(), "not its recorded requirement") {
		t.Errorf("a round review on another roster: %v", err)
	}
}

// producerOfView is producerOf in a view of rw's store.
func producerOfView(t *testing.T, rw *runWorld, node types.NodeID, kind types.ResultKind) (types.NodeID, error) {
	t.Helper()
	var p types.NodeID
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		p, err = producerOf(tx, node, kind)
		return err
	})
	return p, nil
}
