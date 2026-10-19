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

// records is the task's RefRecords, read now.
func (g graphStore) records(task types.TaskID, extra ...types.Ref) RefRecords {
	g.t.Helper()
	var rs RefRecords
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		rs, err = Records(tx, task, extra...)
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return rs
}

// TestRecords (P16a): a task's ref records come from its recorded
// results, each with its identity, its lineage and its producer's kind;
// another task's refs, an unknown id and an id recorded with two
// identities resolve to nothing; the controller's extra refs resolve with
// no producer.
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
	approval := derived("apr_1", "5")
	rs := g.records(tskID, approval)
	rec, ok, err := rs("findings-1")
	if err != nil || !ok || rec.ContentDigest != findings.ContentDigest || !slices.Equal(rec.Lineage, []string{"patch-1"}) || rec.Producer != types.NodeKindIntegration {
		t.Errorf("a result's ref: %+v %v %v", rec, ok, err)
	}
	if rec, ok, _ := rs("apr_1"); !ok || rec.Producer != "" || rec.ContentDigest != approval.ContentDigest {
		t.Errorf("an extra ref: %+v %v", rec, ok)
	}
	for _, id := range []string{"twice", "unknown"} {
		if _, ok, _ := rs(id); ok {
			t.Errorf("%s resolved", id)
		}
	}
	if _, ok, _ := g.records("tsk_01m3250v9xft08ssr4ygq6g2nq")("findings-1"); ok {
		t.Errorf("another task's ref resolved")
	}
}
