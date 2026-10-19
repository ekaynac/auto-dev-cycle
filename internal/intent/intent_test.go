// SPDX-License-Identifier: Apache-2.0

package intent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

// digest is a content digest; ids below are never digests (Astra #252 r2
// H1: tests whose ids equal their digests hid the identity cases).
func digest(c string) types.Digest { return types.Digest("sha256:" + strings.Repeat(c, 64)) }

const acceptance = "e" // the task's acceptance blob's digest character

// store is a resolver over records by id, counting its lookups.
type store struct {
	recs    map[string]graph.RefRecord
	lookups map[string]int
}

func newStore(recs ...graph.RefRecord) *store {
	s := &store{recs: map[string]graph.RefRecord{}, lookups: map[string]int{}}
	for _, r := range recs {
		s.recs[r.ID] = r
	}
	return s
}

func (s *store) resolve(id string) (graph.RefRecord, bool, error) {
	s.lookups[id]++
	r, ok := s.recs[id]
	return r, ok, nil
}

func rec(id, d string, producer types.NodeKind, lineage ...string) graph.RefRecord {
	return graph.RefRecord{ID: id, ContentDigest: digest(d), Lineage: append([]string{}, lineage...), Producer: producer}
}

// ref is r's sealed ref, as a context renders it.
func ref(t *testing.T, r graph.RefRecord) types.Ref {
	t.Helper()
	p := types.ProjectID("prj_01m3250vkwffc979nzjw0gpym1")
	s, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: r.ID, Scope: types.RefScope{Project: &p},
		Lineage: r.Lineage, Restrictions: []types.Restriction{}, ContentDigest: r.ContentDigest})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func call() types.ManifestIntent {
	return types.ManifestIntent{ID: "cmf_01m3250vkwffc979nzjw0gpym1", Call: "cal_01m3250vkwffc979nzjw0gpym1",
		Consumer: types.ManifestIntentConsumer{Executor: "exe_01m3250vkwffc979nzjw0gpym1", ModelSnapshot: "model-2026-10", HarnessVersion: "1.0.0", CapabilityProfile: "large"},
		Policy:   types.ManifestIntentPolicy{UsageProfile: types.ManifestIntentPolicyUsageProfileStandard, DataPolicyDigest: digest("f"), RendererVersions: map[string]string{"packet": "1"}},
		Budget:   types.ManifestIntentBudget{Tokens: 1000, Bytes: 4000}, RenderedDigest: digest("f"), Destination: "local-gpu", Routing: types.ManifestIntentRoutingVerified}
}

func in(t *testing.T, s *store, inputs ...graph.RefRecord) In {
	ctx := graph.Rendered{}
	bytes := map[types.Digest]int64{}
	for _, r := range inputs {
		x := ref(t, r)
		ctx.Included = append(ctx.Included, x)
		bytes[x.IdentityDigest] = 10
	}
	return In{Call: call(), Context: ctx, Bytes: bytes, Acceptance: []types.Digest{digest(acceptance)}, Records: s.resolve}
}

// TestIntentAcceptance (I18, fault 2): an input that is, or derives at any
// depth from, an acceptance blob refuses the intent, through its id (a
// blob ref's id is its digest) or through its record's content digest
// under another id (Astra #252 r2 H1); an unknown or unrecorded ancestor
// refuses it as unresolved; a clean ancestry gives a valid intent.
func TestIntentAcceptance(t *testing.T) {
	tree := rec("tree-1", "1", "graph", "patch-1")
	clean := newStore(tree, rec("patch-1", "2", "build", "spec-1"), rec("spec-1", "3", ""))
	got, err := Builder(in(t, clean, tree))
	if err != nil || got.Consumer.Role != "builder" || len(got.Included) != 1 || got.Included[0].Purpose != "build input" {
		t.Fatalf("a clean ancestry: %+v, %v", got, err)
	}
	if got, err := Reviewer(in(t, clean, tree)); err != nil || got.Consumer.Role != "reviewer" {
		t.Fatalf("a clean ancestry, reviewed: %+v, %v", got, err)
	}
	accepted := "sha256:" + strings.Repeat(acceptance, 64)
	for name, c := range map[string]struct {
		s    *store
		want error
	}{
		"the input is an acceptance blob": {newStore(rec("tree-1", acceptance, "graph")), ErrAcceptanceInput},
		"a parent is one (by its id)":     {newStore(rec("tree-1", "1", "graph", accepted)), ErrAcceptanceInput},
		"at depth three, by its id": {newStore(tree, rec("patch-1", "2", "build", "spec-1"), rec("spec-1", "3", "", "notes-1"),
			rec("notes-1", "4", "", accepted)), ErrAcceptanceInput},
		"an ancestor under another id holds it (Astra #252 r2 H1)": {newStore(tree, rec("patch-1", "2", "build", "spec-1"), rec("spec-1", acceptance, "")), ErrAcceptanceInput},
		"an unknown ancestor":            {newStore(tree, rec("patch-1", "2", "build", "spec-1")), ErrAncestryUnresolved},
		"the input has no record":        {newStore(), ErrAncestryUnresolved},
		"its record has another digest":  {newStore(rec("tree-1", "9", "graph", "patch-1")), ErrAncestryUnresolved},
		"its record has another lineage": {newStore(rec("tree-1", "1", "graph")), ErrAncestryUnresolved},
	} {
		for role, intent := range map[string]func(In) (types.ManifestIntent, error){"builder": Builder, "reviewer": Reviewer} {
			if _, err := intent(in(t, c.s, tree)); !errors.Is(err, c.want) {
				t.Errorf("%s, %s: %v, want %v", name, role, err, c.want)
			}
		}
	}
	x := in(t, clean, tree)
	x.Records = nil
	if _, err := Builder(x); !errors.Is(err, ErrAncestryUnresolved) {
		t.Errorf("no resolver: %v", err)
	}
	// An omitted input is an input too.
	x = in(t, newStore(rec("tree-1", acceptance, "graph")))
	x.Context.Omitted = []types.ManifestIntentOmittedItem{{Ref: ref(t, rec("tree-1", acceptance, "graph")), Reason: types.ManifestIntentOmittedItemReasonRestricted}}
	if _, err := Builder(x); !errors.Is(err, ErrAcceptanceInput) {
		t.Errorf("an omitted acceptance input: %v", err)
	}
}

// TestIntentDigestApartFromWalk (fault 3, Astra #252 r2 H1): an ancestor
// whose id equals the input's content digest is still resolved, and its
// lineage walked; so is the input itself, whatever its lineage.
func TestIntentDigestApartFromWalk(t *testing.T) {
	d := string(digest("1"))
	input := rec("tree-1", "1", "graph", d) // its one parent's id is its own content digest
	s := newStore(input, rec(d, "2", "build", "hidden-1"), rec("hidden-1", acceptance, ""))
	if _, err := Builder(in(t, s, input)); !errors.Is(err, ErrAcceptanceInput) {
		t.Errorf("an ancestor id equal to the input's digest: %v", err)
	}
	if s.lookups[d] != 1 || s.lookups["tree-1"] != 1 {
		t.Errorf("lookups %v: the input and that ancestor each resolved once", s.lookups)
	}
	root := rec("root-1", "1", "graph") // no lineage: still resolved
	s = newStore(root)
	if _, err := Builder(in(t, s, root)); err != nil || s.lookups["root-1"] != 1 {
		t.Errorf("a root input: %v, %d lookups", err, s.lookups["root-1"])
	}
}

// TestIntentBounds (Astra #252 r2 M3): each id is resolved once, however
// many paths reach it; the record bound is inclusive, and the edge and
// byte bounds hold apart from it; a cycle ends.
func TestIntentBounds(t *testing.T) {
	defer func(r, e, b int) { maxRecords, maxEdges, maxIDBytes = r, e, b }(maxRecords, maxEdges, maxIDBytes)
	// A dense DAG: layer i's every node lists every node of layer i+1.
	const layers, width = 6, 8
	var recs []graph.RefRecord
	name := func(l, i int) string { return fmt.Sprintf("n-%d-%d", l, i) }
	for l := range layers {
		for i := range width {
			var parents []string
			for j := range width {
				if l+1 < layers {
					parents = append(parents, name(l+1, j))
				}
			}
			recs = append(recs, rec(name(l, i), "1", "build", parents...))
		}
	}
	input := rec("tree-1", "1", "graph", name(0, 0), name(0, 1))
	s := newStore(append(recs, input)...)
	if _, err := Builder(in(t, s, input)); err != nil {
		t.Fatalf("a dense DAG: %v", err)
	}
	for id, n := range s.lookups {
		if n != 1 {
			t.Errorf("%s resolved %d times", id, n)
		}
	}
	total := len(s.lookups) // the input, its two layer-0 parents, and every node below
	edges := 2 + 2*width + (layers-2)*width*width
	for name, c := range map[string]struct {
		records, edges, bytes int
		ok                    bool
	}{
		"exactly the record bound": {total, 1 << 20, 1 << 20, true},
		"one record past it":       {total - 1, 1 << 20, 1 << 20, false},
		"exactly the edge bound":   {total, edges, 1 << 20, true},
		"one edge past it":         {total, edges - 1, 1 << 20, false},
		"the byte bound":           {total, 1 << 20, 100, false},
	} {
		maxRecords, maxEdges, maxIDBytes = c.records, c.edges, c.bytes
		if _, err := Builder(in(t, newStore(append(recs, input)...), input)); (err == nil) != c.ok || err != nil && !errors.Is(err, ErrAncestryUnresolved) {
			t.Errorf("%s: %v", name, err)
		}
	}
	maxRecords, maxEdges, maxIDBytes = 1<<20, 1<<20, 1<<20
	cycle := newStore(rec("tree-1", "1", "graph", "a"), rec("a", "2", "build", "b"), rec("b", "3", "build", "a"))
	if _, err := Builder(in(t, cycle, rec("tree-1", "1", "graph", "a"))); err != nil {
		t.Errorf("a cycle: %v", err)
	}
}

// TestIntentFresh: a reviewer's input is never a review's result (a fresh
// context per round); a builder may take one (a repair round's findings),
// and a reviewer may take a ref that derives from one.
func TestIntentFresh(t *testing.T) {
	decision := rec("decision-1", "1", types.NodeKindReview)
	repaired := rec("tree-2", "2", "graph", "decision-1")
	s := newStore(decision, repaired)
	if _, err := Reviewer(in(t, s, decision)); !errors.Is(err, ErrNotFresh) {
		t.Errorf("a round's decision in the next round's review: %v", err)
	}
	if _, err := Builder(in(t, s, decision)); err != nil {
		t.Errorf("a repair round's builder taking the decision: %v", err)
	}
	if _, err := Reviewer(in(t, s, repaired)); err != nil {
		t.Errorf("a reviewer taking a tree repaired from a decision: %v", err)
	}
}
