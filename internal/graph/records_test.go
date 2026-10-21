// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
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
	defer func(n, l, b int) { maxTaskRefs, maxTaskLineage, maxTaskIDBytes = n, l, b }(maxTaskRefs, maxTaskLineage, maxTaskIDBytes)
	maxTaskRefs = 10 // the task's results hold 10 refs
	if _, err := g.records(tskID); err != nil {
		t.Errorf("exactly the ref bound: %v", err)
	}
	if _, err := g.records(tskID, approval); err == nil {
		t.Error("one ref past the bound")
	}
	// The task's results hold 2 lineage entries and 90 bytes of ids, their
	// lineage's included.
	maxTaskRefs = 100
	for name, c := range map[string]struct {
		lineage, bytes int
		ok             bool
	}{
		"exactly the lineage bound": {2, 1 << 20, true},
		"one entry past it":         {1, 1 << 20, false},
		"exactly the byte bound":    {1 << 20, 90, true},
		"one byte past it":          {1 << 20, 89, false},
	} {
		maxTaskLineage, maxTaskIDBytes = c.lineage, c.bytes
		if _, err := g.records(tskID); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestRecordsDeclassified (22a-6L; #226 entries 91(3), 94(2); Astra #321
// H1): the receipt and blob of a gate node's gate_result holding exactly
// those two are Sanitised candidates, a review node's view ref is a
// BuilderSafe one, and nothing else is either: a gate_result's report
// alone, the refs of one carrying a third attachment (recorded around the
// fold, which refuses it), a view another node recorded, a review's
// decision, a tree.
func TestRecordsDeclassified(t *testing.T) {
	g := openStore(t)
	sealed := func(typ types.RefType, id, digest string) types.Ref { return sealedRef(t, typ, id, digest) }
	g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{sealed(types.RefTypeReceipt, "gate-receipt", "6"), sealed(types.RefTypeBlob, "gate-report", "1")}, nil)
	g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{sealed(types.RefTypeBlob, "lone-report", "7")}, nil)
	g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{sealed(types.RefTypeReceipt, "x-receipt", "8"), sealed(types.RefTypeBlob, "x-report", "9"),
		sealed(types.RefTypeBlob, "raw-results", "a")}, nil)
	g.completedWith(types.NodeKindReview, types.ResultKindReview, []types.Ref{sealed(types.RefTypeBlob, "decision", "2"), sealed(types.RefTypeView, "view", "3")})
	g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{sealed(types.RefTypeView, "other-view", "4"), sealed(types.RefTypeBlob, "tree", "5")})
	rs, err := g.records(tskID)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][2]bool{"gate-receipt": {true, false}, "gate-report": {true, false}, "view": {false, true}, "lone-report": {}, "x-receipt": {}, "x-report": {},
		"raw-results": {}, "decision": {}, "other-view": {}, "tree": {}} {
		if rec, ok, _ := rs.Resolve(id); !ok || rec.Sanitised != want[0] || rec.BuilderSafe != want[1] {
			t.Errorf("%s: %+v %v, want sanitised %v, builder-safe %v", id, rec, ok, want[0], want[1])
		}
	}
	// The fold (Astra #321 H1's input): a gate's result carrying an
	// attachment beside its receipt and report is refused.
	gate, stored := g.completedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{sealed(types.RefTypeBlob, "report-2", "b")})
	for name, c := range map[string]struct {
		value []types.Ref
		ok    bool
	}{
		"its report alone":              {[]types.Ref{sealed(types.RefTypeBlob, "report-2", "b")}, true},
		"a second blob beside it":       {[]types.Ref{sealed(types.RefTypeBlob, "report-2", "b"), sealed(types.RefTypeBlob, "raw-results", "a")}, false},
		"a view beside it":              {[]types.Ref{sealed(types.RefTypeBlob, "report-2", "b"), sealed(types.RefTypeView, "view-2", "c")}, false},
		"an attachment of another type": {[]types.Ref{sealed(types.RefTypeFinding, "finding-2", "d")}, false},
	} {
		r := stored
		r.ID, r.Value = "res_01m3250v9xft08ssr4ygq6g2nr", c.value
		err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
			_, err := recordResult(tx, types.ResultRecorded{Node: gate, Result: r}, g.blobs)
			return err
		})
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestRecordsResultBound (Astra #321 M5): Records lists at most
// maxTaskResults of the task's results, at its exact bound, refusing past
// it before reading any; other tasks' results, whose keys sort just before
// and after the task's, are not counted.
func TestRecordsResultBound(t *testing.T) {
	g := openStore(t)
	for _, d := range []string{"1", "2"} {
		g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{sealedRef(t, types.RefTypeBlob, "r"+d, d)})
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var err error
		for _, other := range []string{"tsk_01m3250v9xft08ssr4ygq6g2nn", "tsk_01m3250v9xft08ssr4ygq6g2nq", string(tskID) + "0"} {
			for _, r := range []string{"res_a", "res_b", "res_c"} {
				err = errors.Join(err, tx.Put(TaskResultProjection, other+"/"+r, json.RawMessage(`true`)))
			}
		}
		return nil, err
	})
	n := 0
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(TaskResultProjection, string(tskID)+"/")
		n = len(keys)
		return err
	}); err != nil || n < 2 {
		t.Fatal(n, err)
	}
	defer func(n int) { maxTaskResults = n }(maxTaskResults)
	for bound, ok := range map[int]bool{n: true, n - 1: false} {
		maxTaskResults = bound
		if _, err := g.records(tskID); (err == nil) != ok {
			t.Errorf("%d results under the bound %d: %v", n, bound, err)
		}
	}
}

// TestKeysAtMost (Astra #321 M5): the keys of a prefix's range, at most n.
func TestKeysAtMost(t *testing.T) {
	g := openStore(t)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var err error
		for _, k := range []string{"a.", "a/1", "a/2", "a/3", "a0", "b/1"} {
			err = errors.Join(err, tx.Put(TaskResultProjection, k, json.RawMessage(`true`)))
		}
		return nil, err
	})
	for n, want := range map[int]string{0: "", 2: "a/1 a/2", 3: "a/1 a/2 a/3", 9: "a/1 a/2 a/3"} {
		if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
			keys, err := tx.KeysAtMost(TaskResultProjection, "a/", n)
			if got := strings.Join(keys, " "); got != want {
				t.Errorf("at most %d: %q, want %q", n, got, want)
			}
			return err
		}); err != nil {
			t.Error(err)
		}
	}
}

// TestRecordsDecodeBound (22a-6L; Opus #321 M1, L5): a stored result is
// read only within maxResultBytes, and the task's results within
// maxResultsBytes, each at its exact bound; another task's results are
// never read, however large; a result past maxResultBytes is refused when
// it is recorded.
func TestRecordsDecodeBound(t *testing.T) {
	g := openStore(t)
	for _, d := range []string{"1", "2", "3"} {
		g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{sealedRef(t, types.RefTypeBlob, "r"+d, d)})
	}
	largest, total := int64(0), int64(0)
	var stored types.ResultRecorded
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(ResultProjection, "")
		for _, k := range keys {
			n, _, _ := tx.Size(ResultProjection, k)
			largest, total = max(largest, n), total+n
			stored, _, _ = load[types.ResultRecorded](tx, ResultProjection, k)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Another task's result, past every bound (put directly: the write
	// side refuses it).
	other := types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2nq")
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, errors.Join(tx.Put(ResultProjection, "res_other", json.RawMessage(`{"pad":"`+strings.Repeat("x", int(total))+`"}`)),
			tx.Put(TaskResultProjection, string(other)+"/res_other", json.RawMessage(`true`)))
	})
	defer func(one, all int) { maxResultBytes, maxResultsBytes = one, all }(maxResultBytes, maxResultsBytes)
	for name, c := range map[string]struct {
		one, all int64
		ok       bool
	}{
		"at both bounds":       {largest, total, true},
		"one past its bound":   {largest - 1, total, false},
		"all past their bound": {largest, total - 1, false},
	} {
		maxResultBytes, maxResultsBytes = int(c.one), int(c.all)
		if _, err := g.records(tskID); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	maxResultBytes, maxResultsBytes = int(largest), int(total)
	if _, err := g.records(other); err == nil {
		t.Error("another task's result past the bound read")
	}
	// A result's node record is read within maxNodeBytes.
	defer func(n int) { maxNodeBytes = n }(maxNodeBytes)
	maxNodeBytes = 10
	if _, err := g.records(tskID); err == nil {
		t.Error("a node record past its bound read")
	}
	maxNodeBytes = 4 << 20
	// The write side: a result exactly at the bound is recorded, one past
	// it refused.
	stored.Result.ID = "res_01m3250v9xft08ssr4ygq6g2nr"
	b, err := json.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	for bound, ok := range map[int]bool{len(b): true, len(b) - 1: false} {
		maxResultBytes = bound
		err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
			_, err := recordResult(tx, stored, g.blobs)
			return err
		})
		if (err == nil) != ok {
			t.Errorf("a result of %d bytes recorded under the bound %d: %v", len(b), bound, err)
		}
	}
}

// sealedRef is a sealed ref of the test project, with no lineage.
func sealedRef(t *testing.T, typ types.RefType, id, digest string) types.Ref {
	t.Helper()
	r, err := knowledge.Seal(types.Ref{Type: typ, ID: id, Scope: types.RefScope{Project: ptr(types.ProjectID(prjID))},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest("sha256:" + strings.Repeat(digest, 64))})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
