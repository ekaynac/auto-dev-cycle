// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

// records is the task's Recorded, read now.
func (g graphStore) records(task types.TaskID, extra ...types.Ref) (Recorded, error) {
	g.t.Helper()
	var rs Recorded
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		rs, err = Records(tx, task, extra...)
		return err
	})
	return rs, err
}

// TestRecords (P16a): a task's ref records come from its recorded
// results, each with its identity, its lineage and its producer's kind;
// another task's refs, an unknown id and an id recorded twice with another
// digest, lineage or producer (a result's ref against an extra ref
// included) resolve to nothing; the controller's extra refs resolve with
// no producer; a review's result's digests are exposed; past the ref
// bound, Records refuses.
func TestRecords(t *testing.T) {
	g := openStore(t)
	derived := func(id, digest string, lineage ...string) types.Ref {
		r, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: id, Scope: types.RefScope{Project: ptr(types.ProjectID(prjID))},
			Lineage: append([]string{}, lineage...), Restrictions: []types.Restriction{}, ContentDigest: types.Digest("sha256:" + strings.Repeat(digest, 64))})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	findings := derived("findings-1", "1", "patch-1")
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{derived("patch-1", "2"), findings})
	twice := derived("twice", "3")
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{twice})
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{derived("twice", "4")})
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{derived("lineages", "6", "patch-1"), derived("producers", "7")})
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{derived("lineages", "6")})
	g.completedWith(types.NodeKindAccept, types.ResultKindTree, []types.Ref{derived("producers", "7")})
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{derived("apr_2", "8")})
	decision := derived("decision-1", "9")
	g.completedWith(types.NodeKindReview, types.ResultKindReview, []types.Ref{decision})
	approval := derived("apr_1", "5")
	rs, err := g.records(tskID, approval, derived("apr_2", "8"))
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := rs.Resolve("findings-1")
	if err != nil || !ok || rec.ContentDigest != findings.ContentDigest || !slices.Equal(rec.Lineage, []string{"patch-1"}) || rec.Producer != types.NodeKindIntegration {
		t.Errorf("a result's ref: %+v %v %v", rec, ok, err)
	}
	if rec, ok, _ := rs.Resolve("apr_1"); !ok || rec.Producer != "" || rec.ContentDigest != approval.ContentDigest {
		t.Errorf("an extra ref: %+v %v", rec, ok)
	}
	for name, id := range map[string]string{"another digest": "twice", "another lineage": "lineages", "another producer": "producers",
		"a result's ref and an extra ref": "apr_2", "an unknown id": "unknown"} {
		if _, ok, _ := rs.Resolve(id); ok {
			t.Errorf("%s: %s resolved", name, id)
		}
	}
	if !rs.Reviewed[decision.ContentDigest] || len(rs.Reviewed) != 1 {
		t.Errorf("a review's digests: %v", rs.Reviewed)
	}
	if other, err := g.records("tsk_01m3250v9xft08ssr4ygq6g2nq"); err != nil {
		t.Fatal(err)
	} else if _, ok, _ := other.Resolve("findings-1"); ok {
		t.Errorf("another task's ref resolved")
	}
	defer func(n int) { maxTaskRefs = n }(maxTaskRefs)
	maxTaskRefs = 10 // the task's results hold 10 refs
	if _, err := g.records(tskID); err != nil {
		t.Errorf("exactly the ref bound: %v", err)
	}
	if _, err := g.records(tskID, approval); err == nil {
		t.Error("one ref past the bound")
	}
}
