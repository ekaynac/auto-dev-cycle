// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
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
}
