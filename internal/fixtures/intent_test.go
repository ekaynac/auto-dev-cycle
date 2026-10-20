// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// TestIntentLoad (P16a; Astra #289 L4): intent.Load joins a real store's
// task record and graph results for both roles. A tree whose ancestor
// alias holds the task's acceptance is refused; a clean tree passes; a
// review's decision re-recorded under another id is refused for the
// reviewer only; a task with no acceptance takes the alias's tree; an
// unknown task is refused. The task record is put into the task
// projection directly: authority's own fold of task.created is
// TestTaskCreate's.
func TestIntentLoad(t *testing.T) {
	g, done, err := openGraph(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	digest := func(c string) types.Digest { return types.Digest("sha256:" + strings.Repeat(c, 64)) }
	refOf := func(id string, d types.Digest, lineage ...string) types.Ref {
		r, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: id, Scope: types.RefScope{Project: ptrTo(gProject)},
			Lineage: append([]string{}, lineage...), Restrictions: []types.Restriction{}, ContentDigest: d})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	accepted := digest("e")
	alias, tree, clean := refOf("alias-1", accepted), refOf("tree-1", digest("1"), "alias-1"), refOf("tree-2", digest("2"))
	decision, relay := refOf("decision-1", digest("9")), refOf("relay-1", digest("9"))
	for i, value := range [][]types.Ref{{alias, tree}, {clean}, {relay}} {
		if _, _, err := g.complete(types.NodeKindPlan, json.RawMessage(`{"n":`+string(rune('0'+i))+`}`), []types.InputSpec{}, map[string]graph.Resolved{},
			types.ResultKindPlan, value, nil); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := g.gateOf(treeA, demandA, nil)
	st, _, serr := g.node(gate)
	if err = errors.Join(err, serr); err != nil {
		t.Fatal(err)
	}
	review, err := g.reviewOf(gate)
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.ClaimReview(tx, review, g.withRequired(review, map[string]graph.Resolved{"gate": {Result: types.ResultID(*st.Result)}}), 1, reviewers)
		})
	}
	attempts := map[types.NodeID]types.AttemptID{} // the consumers' attempts, each started on its claimed node
	if err == nil {
		attempts[review], err = g.nextAttempt(review)
	}
	if err == nil {
		_, err = g.selectResult(review, types.ResultKindReview, []types.Ref{decision}, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	seed := func(acceptance ...types.Digest) {
		t.Helper()
		task := types.Task{ID: gTask, Project: gProject, Acceptance: []types.AcceptanceBlob{}}
		for _, d := range acceptance {
			task.Acceptance = append(task.Acceptance, types.AcceptanceBlob{Name: "hidden/test.py", Digest: d, Class: types.AcceptanceBlobClassRestricted})
		}
		b, err := json.Marshal(authority.TaskRecord{Task: task, State: authority.TaskReady})
		if err == nil {
			err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return nil, tx.Put(authority.TaskProjection, string(gTask), b)
			})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	call, err := intentOf()
	if err != nil {
		t.Fatal(err)
	}
	build := func(role func(intent.In) (types.ManifestIntent, error), task types.TaskID, r types.Ref) error {
		return g.s.View(t.Context(), func(tx *eventstore.Tx) error {
			recs, err := intent.Load(tx, task)
			if err != nil {
				return err
			}
			_, err = role(intent.In{Call: call, Context: graph.Rendered{Included: []types.Ref{r}}, Bytes: map[types.Digest]int64{r.IdentityDigest: 10}, Records: recs})
			return err
		})
	}
	roles := map[string]func(intent.In) (types.ManifestIntent, error){"builder": intent.Builder, "reviewer": intent.Reviewer}
	seed()
	for role, fn := range roles {
		if err := build(fn, gTask, tree); err != nil {
			t.Errorf("a task with no acceptance, %s: %v", role, err)
		}
	}
	seed(accepted)
	for role, fn := range roles {
		if err := build(fn, gTask, tree); !errors.Is(err, intent.ErrAcceptanceInput) {
			t.Errorf("an ancestor alias holding the acceptance, %s: %v", role, err)
		}
		if err := build(fn, gTask, clean); err != nil {
			t.Errorf("a clean tree, %s: %v", role, err)
		}
		// Refused by Load itself, not by the walk.
		if err := build(fn, "tsk_01m3250vkwffc979nzjw0gpym1", clean); err == nil || errors.Is(err, intent.ErrAcceptanceInput) {
			t.Errorf("an unknown task, %s: %v", role, err)
		}
	}
	if err := build(intent.Reviewer, gTask, relay); !errors.Is(err, intent.ErrNotFresh) {
		t.Errorf("a review's decision re-recorded, reviewer: %v", err)
	}
	if err := build(intent.Builder, gTask, relay); err != nil {
		t.Errorf("a review's decision re-recorded, builder: %v", err)
	}
	// The fold-side check of a recorded intent, on the store (22a-6L): its
	// role is its attempt's node's (Opus #321 H1), never its own word.
	claimed := func(kind types.NodeKind) types.NodeID {
		t.Helper()
		n, err := g.record(kind, json.RawMessage(`{"n":"`+string(kind)+`"}`), []types.InputSpec{})
		if err == nil {
			err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return graph.Claim(tx, n, map[string]graph.Resolved{}, 1)
			})
		}
		if err == nil {
			attempts[n], err = g.nextAttempt(n)
		}
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	builder, plan := claimed(types.NodeKindBuild), claimed(types.NodeKindPlan)
	recorded := func(on types.NodeID, role string, r types.Ref) types.ManifestIntent {
		mi := call
		a := attempts[on]
		mi.Task, mi.Attempt = ptrTo(gTask), &a
		mi.Consumer.Role, mi.Inputs = role, []types.Ref{r}
		return mi
	}
	check := func(task types.TaskID, mi types.ManifestIntent) error {
		return g.s.View(t.Context(), func(tx *eventstore.Tx) error { return intent.CheckRecorded(tx, task, mi) })
	}
	if err := check(gTask, recorded(builder, "builder", clean)); err != nil {
		t.Errorf("a recorded intent of a clean tree: %v", err)
	}
	if err := check(gTask, recorded(builder, "builder", tree)); !errors.Is(err, intent.ErrAcceptanceInput) {
		t.Errorf("a recorded intent of the alias's tree: %v", err)
	}
	if err := check(gTask, recorded(review, "reviewer", relay)); !errors.Is(err, intent.ErrNotFresh) {
		t.Errorf("a recorded reviewer's intent of a review's decision: %v", err)
	}
	if err := check(gTask, recorded(review, "reviewer", clean)); err != nil {
		t.Errorf("a recorded reviewer's intent of a clean tree: %v", err)
	}
	if err := check("tsk_01m3250vkwffc979nzjw0gpym1", recorded(builder, "builder", clean)); !errors.Is(err, intent.ErrConsumer) {
		t.Errorf("a recorded intent of another task: %v", err)
	}
	// A review attempt's intent stating "builder", which would take a
	// review's decision: refused, never walked as a builder's.
	if err := check(gTask, recorded(review, "builder", relay)); !errors.Is(err, intent.ErrConsumer) {
		t.Errorf("a review attempt's intent stated as a builder's: %v", err)
	}
	if err := check(gTask, recorded(builder, "reviewer", clean)); !errors.Is(err, intent.ErrConsumer) {
		t.Errorf("a build attempt's intent stated as a reviewer's: %v", err)
	}
	if err := check(gTask, recorded(plan, "builder", clean)); !errors.Is(err, intent.ErrConsumer) {
		t.Errorf("a plan attempt's intent: %v", err)
	}
	noTask := recorded(builder, "builder", clean)
	noTask.Task = nil
	otherTask := recorded(builder, "builder", clean)
	otherTask.Task = ptrTo(types.TaskID("tsk_01m3250vkwffc979nzjw0gpym1"))
	noAttempt := recorded(builder, "builder", clean)
	noAttempt.Attempt = nil
	unknown := recorded(builder, "builder", clean)
	unknown.Attempt = ptrTo(types.AttemptID("att_01m3250vkwffc979nzjw0gpym1"))
	// Checked under the task it names, an attempt of another task's node.
	if err := check("tsk_01m3250vkwffc979nzjw0gpym1", otherTask); !errors.Is(err, intent.ErrConsumer) {
		t.Errorf("an intent of another task, its attempt this task's: %v", err)
	}
	for name, mi := range map[string]types.ManifestIntent{"no task": noTask, "another task": otherTask, "no attempt": noAttempt, "an unknown attempt": unknown} {
		if err := check(gTask, mi); !errors.Is(err, intent.ErrConsumer) {
			t.Errorf("a recorded intent with %s: %v", name, err)
		}
	}
}

// TestIntentViewRounds (Opus #321 H2): two review rounds whose no_go
// decisions hold the same finding's title publish two findings views, and
// the second round's resolves BuilderSafe from the store, so the repair
// builder's intent takes it.
func TestIntentViewRounds(t *testing.T) {
	g, done, err := openGraph(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var views []types.Ref
	for round, detail := range []string{"round 1", "round 2: still"} {
		dec, _ := json.Marshal(map[string]any{"decision": review.NoGo, "summary": "s",
			"findings": []map[string]string{{"severity": "high", "title": "negative amounts are not rejected", "detail": detail}}})
		d, err := g.blobs.Put(bytes.NewReader(dec), "")
		if err != nil {
			t.Fatal(err)
		}
		r, err := review.Result(types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("0", 64))}, review.Facts{Submitted: true, Decision: types.Digest(d), Project: gProject,
			CandidateRound: int64(round + 1), Inputs: []string{}, GateIDs: []string{}}, g.blobs)
		if err != nil {
			t.Fatal(err)
		}
		candidate, res, err := g.complete(types.NodeKindBuild, json.RawMessage(`{"round":`+string(rune('1'+round))+`}`), []types.InputSpec{}, map[string]graph.Resolved{},
			types.ResultKindPatch, []types.Ref{}, nil)
		if err == nil {
			_, _, err = g.complete(types.NodeKindReview, soleReviewer, []types.InputSpec{{Name: "patch",
				Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: candidate, ResultKind: types.ResultKindPatch}}}},
				map[string]graph.Resolved{"patch": {Result: res}}, types.ResultKindReview, r.Value, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, r.Value[1])
	}
	b, err := json.Marshal(authority.TaskRecord{Task: types.Task{ID: gTask, Project: gProject, Acceptance: []types.AcceptanceBlob{}}, State: authority.TaskReady})
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return nil, tx.Put(authority.TaskProjection, string(gTask), b)
		})
	}
	call, cerr := intentOf()
	if err = errors.Join(err, cerr); err != nil {
		t.Fatal(err)
	}
	err = g.s.View(t.Context(), func(tx *eventstore.Tx) error {
		recs, err := intent.Load(tx, gTask)
		if err != nil {
			return err
		}
		for i, v := range views {
			if rec, ok, err := recs.Resolve(v.ID); err != nil || !ok || !rec.BuilderSafe {
				t.Errorf("round %d's view: %+v %v %v", i+1, rec, ok, err)
			}
		}
		_, err = intent.Builder(intent.In{Call: call, Context: graph.Rendered{Included: []types.Ref{views[1]}}, Bytes: map[types.Digest]int64{views[1].IdentityDigest: 10}, Records: recs})
		return err
	})
	if err != nil {
		t.Errorf("the repair builder's intent of round 2's view: %v", err)
	}
}
