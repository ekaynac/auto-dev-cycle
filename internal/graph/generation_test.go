// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"errors"
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
