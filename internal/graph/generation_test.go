// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	fakeforge "github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/forge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// An observation older than the current generation's is refused; a
// genuine return to an earlier head is a new generation.
func TestStaleObservations(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	spec := g.queueSpec("main", g.ref(types.RefTypeBlob, "P"))
	observe := func() Observation { h, v := f.Observe("main"); return Observation{Head: h, Version: v} }
	f.Move("main", "A")
	atA := observe() // a caller observes A, then pauses
	if gen, err := g.generateAt(spec, atA); err != nil || gen.Number != 1 {
		t.Fatalf("A: %+v (%v)", gen, err)
	}
	f.Move("main", "B")
	if gen, err := g.generateAt(spec, observe()); err != nil || gen.Number != 2 {
		t.Fatalf("B: %+v (%v)", gen, err)
	}
	var stale ErrStaleObservation
	if gen, err := g.generateAt(spec, atA); !errors.As(err, &stale) || gen.Number != 0 {
		t.Fatalf("the delayed observation of A: %+v (%v)", gen, err)
	}
	f.Move("main", "A") // a genuine return to A
	if gen, err := g.generateAt(spec, observe()); err != nil || gen.Number != 3 || gen.Base != "A" {
		t.Fatalf("A again: %+v (%v)", gen, err)
	}
	if gen, err := g.generateAt(spec, observe()); err != nil || gen.Number != 0 {
		t.Fatalf("A observed again: %+v (%v)", gen, err)
	}

	// The fold refuses an older version too, whatever the builder did.
	cur, _ := view[Generation](g, GenerationProjection, generationKey(tskID, "main"))
	rec := g.recordOf(cur.Rebase)
	rec.Node.ID = "nod_01m4e289fveqtbx5mgt0y0559z"
	params, _ := json.Marshal(rebaseParams{Branch: "main", Base: "C", Version: cur.Version - 1, Candidate: cur.Candidate, Generation: cur.Number + 1})
	rec.Node.Params = params
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }); err == nil {
		t.Fatal("a generation on an older observation was recorded")
	}
}

// Each attempt to merge a candidate result is a generation: a rebuilt
// candidate on an unmoved base starts one.
func TestRebuiltCandidate(t *testing.T) {
	g := openEffectStore(t)
	spec := g.queueSpec("main", g.ref(types.RefTypeBlob, "P"))
	first, err := g.generate(spec, "B1")
	if err != nil || first.Number != 1 {
		t.Fatalf("%+v (%v)", first, err)
	}
	g.must(invalidate(spec.Candidate))
	if _, err := g.generate(spec, "B1"); err == nil {
		t.Fatal("a generation was started for a candidate with no selected result")
	}
	r := unrecorded(t, *g.state(spec.Candidate).Key)
	r.Value, r.Outcome, r.Reason, r.Detail = []types.Ref{g.ref(types.RefTypeBlob, "P2")}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	g.must(commit(spec.Candidate, r, effectEnv))
	second, err := g.generate(spec, "B1")
	if err != nil || second.Number != 2 || second.Base != "B1" || second.Candidate != r.ID {
		t.Fatalf("the rebuilt candidate: %+v (%v)", second, err)
	}
	if again, err := g.generate(spec, "B1"); err != nil || again.Number != 0 {
		t.Fatalf("the same candidate on the same base: %+v (%v)", again, err)
	}

	// The fold refuses a generation naming another result than the
	// candidate's selected one.
	rec := g.recordOf(second.Rebase)
	rec.Node.ID = "nod_01m4e289fveqtbx5mgt0y0559z"
	params, _ := json.Marshal(rebaseParams{Branch: "main", Base: "B9", Version: 1 << 40, Candidate: first.Candidate, Generation: 3})
	rec.Node.Params = params
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }); err == nil {
		t.Fatal("a generation of a superseded candidate result was recorded")
	}
	rec.Node.Inputs = rec.Node.Inputs[:0]
	if err := g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return Record(rec) }); err == nil {
		t.Fatal("a rebase without its candidate was recorded")
	}
}

// A generation's candidate result is pinned through execution: recorded
// with C1, its rebase is not claimed once C2 is selected, and its
// carry_over does not take a candidate reselected after its rebase ran;
// C2's attempt is the next generation's.
func TestPinnedCandidate(t *testing.T) {
	g := openEffectStore(t)
	p, tree := g.ref(types.RefTypeBlob, "P"), g.ref(types.RefTypeGitTree, "T")
	spec := g.queueSpec("main", p)
	rebuild := func() types.ResultID {
		g.must(invalidate(spec.Candidate))
		r := unrecorded(t, *g.state(spec.Candidate).Key)
		r.Value, r.Outcome, r.Reason, r.Detail = []types.Ref{p}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
		g.must(commit(spec.Candidate, r, effectEnv))
		return r.ID
	}
	g1, err := g.generate(spec, "B1")
	if err != nil || g1.Number != 1 {
		t.Fatalf("G1: %+v (%v)", g1, err)
	}
	c2 := rebuild() // C2 is selected before G1's rebase is claimed
	if err := g.do(evaluate(effectEnv, g1.Rebase)); err == nil || !strings.Contains(err.Error(), "the candidate it pins") {
		t.Fatalf("G1's rebase claimed over C2: %v", err)
	}
	if st := g.state(g1.Rebase); st.State == types.NodeStateStateClaimed {
		t.Fatal("G1's rebase is claimed")
	}
	g2, err := g.generate(spec, "B1")
	if err != nil || g2.Number != 2 || g2.Candidate != c2 {
		t.Fatalf("C2's generation: %+v (%v)", g2, err)
	}
	g.must(evaluate(effectEnv, g2.Rebase))
	g.must(commit(g2.Rebase, g.rebaseResult(g2, tree, p), effectEnv))
	rebuild() // C3, after G2's rebase ran on C2
	g.must(evaluate(effectEnv, g2.CarryOver))
	if _, err := g.carryOver(g2.CarryOver); err == nil || !strings.Contains(err.Error(), "the candidate it pins") {
		t.Fatalf("G2's carry_over over C3: %v", err)
	}
	attest := unrecorded(t, *g.state(g2.CarryOver).Key)
	attest.Kind, attest.Value, attest.Outcome, attest.Reason, attest.Detail = types.ResultKindCarryOverAttestation, []types.Ref{tree}, types.OutcomeEmpty, vocab.ReasonCompletedClean, ""
	if err := g.do(raw(g2.CarryOver, attest, g.blobs)); err == nil || !strings.Contains(err.Error(), "the candidate it pins") {
		t.Fatalf("a raw attestation of G2's carry_over over C3: %v", err)
	}
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The latest observation is kept even when it starts no generation: after
// A, B and A again (no generation, the same head), the delayed observation
// of B is refused; another head at the latest version is refused too.
func TestLatestObservationKept(t *testing.T) {
	g := openEffectStore(t)
	f := fakeforge.New()
	spec := g.queueSpec("main", g.ref(types.RefTypeBlob, "P"))
	observe := func() Observation { h, v := f.Observe("main"); return Observation{Head: h, Version: v} }
	f.Move("main", "A")
	if gen, err := g.generateAt(spec, observe()); err != nil || gen.Number != 1 {
		t.Fatalf("A: %+v (%v)", gen, err)
	}
	f.Move("main", "B")
	atB := observe() // delayed
	f.Move("main", "A")
	if gen, err := g.generateAt(spec, observe()); err != nil || gen.Number != 0 {
		t.Fatalf("A again: %+v (%v)", gen, err)
	}
	var stale ErrStaleObservation
	if gen, err := g.generateAt(spec, atB); !errors.As(err, &stale) || gen.Number != 0 || stale.Latest.Version != 3 {
		t.Fatalf("the delayed observation of B: %+v (%v)", gen, err)
	}
	if r := stale.Result(); r.Outcome != vocab.Failed || r.Reason != vocab.ReasonStaleExpectedState {
		t.Fatalf("the refusal an operator sees: %s/%s", r.Outcome, r.Reason.String())
	}
	if _, err := g.generateAt(spec, Observation{Head: "C", Version: 3}); !errors.As(err, &stale) {
		t.Fatalf("another head at the latest version: %v", err)
	}
	if cur, _ := view[Observation](g, ObservedProjection, generationKey(tskID, "main")); cur != (Observation{Head: "A", Version: 3}) {
		t.Fatalf("the latest observation is %+v", cur)
	}

	// The fold refuses what the builder would: an older or a contradicting
	// observation, and one of an unknown task.
	observed := func(task types.TaskID, head string, version int64) logic {
		return forge(QueueObserved, string(task), prjID, &types.QueueObserved{Type: "queue_observed", Task: task, Branch: "main", Head: head, Version: version})
	}
	for name, l := range map[string]logic{
		"older":        observed(tskID, "B", 2),
		"contradicts":  observed(tskID, "C", 3),
		"unknown task": observed("tsk_01m3250vkwffc979nzjw0g2sf9", "A", 9),
	} {
		if err := g.do(l); err == nil {
			t.Errorf("%s: folded", name)
		}
	}
	g.must(observed(tskID, "A", 3))
	g.must(observed(tskID, "D", 4))
}
