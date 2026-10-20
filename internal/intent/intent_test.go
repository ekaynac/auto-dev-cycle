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
	recs     map[string]graph.RefRecord
	lookups  map[string]int
	reviewed map[types.Digest]bool
}

func newStore(recs ...graph.RefRecord) *store {
	s := &store{recs: map[string]graph.RefRecord{}, lookups: map[string]int{}, reviewed: map[types.Digest]bool{}}
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
	return In{Call: call(), Context: ctx, Bytes: bytes, Records: s.records()}
}

// records are s's Records, under the one acceptance blob.
func (s *store) records() Records {
	return Records{Recorded: graph.Recorded{Resolve: s.resolve, Reviewed: s.reviewed}, Acceptance: []types.Digest{digest(acceptance)}}
}

// roles are both intents, by role.
var roles = map[string]func(In) (types.ManifestIntent, error){"builder": Builder, "reviewer": Reviewer}

// TestIntentAcceptance (I18, fault 2): an input that is, or derives at any
// depth from, an acceptance blob refuses the intent, through its record's
// id or content digest (under another id: Astra #252 r2 H1); an unknown
// or unrecorded ancestor (an acceptance blob no result holds included)
// refuses it as unresolved; so do a missing resolver and acceptance not
// read; a clean ancestry gives a valid intent.
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
	hiddenInput := rec("tree-1", acceptance, "graph")
	withParent := rec("tree-1", "1", "graph", accepted) // its one parent's id is the acceptance digest
	for name, c := range map[string]struct {
		s     *store
		input graph.RefRecord
		want  error
	}{
		"the input is an acceptance blob":                          {newStore(hiddenInput), hiddenInput, ErrAcceptanceInput},
		"a parent is one, with no record (22a-6L: refused as one)": {newStore(withParent), withParent, ErrAcceptanceInput},
		"one at depth three, with no record (refused as one)": {newStore(tree, rec("patch-1", "2", "build", "spec-1"), rec("spec-1", "3", "", "notes-1"),
			rec("notes-1", "4", "", accepted)), tree, ErrAcceptanceInput},
		"an ancestor under another id holds it (Astra #252 r2 H1)": {newStore(tree, rec("patch-1", "2", "build", "spec-1"), rec("spec-1", acceptance, "")), tree, ErrAcceptanceInput},
		"an ancestor whose id is the acceptance digest, its content another (Opus #289 M1)": {newStore(withParent, rec(accepted, "2", "build")), withParent,
			ErrAcceptanceInput},
		"an unknown ancestor":            {newStore(tree, rec("patch-1", "2", "build", "spec-1")), tree, ErrAncestryUnresolved},
		"the input has no record":        {newStore(), tree, ErrAncestryUnresolved},
		"its record has another digest":  {newStore(rec("tree-1", "9", "graph", "patch-1")), tree, ErrAncestryUnresolved},
		"its record has another lineage": {newStore(rec("tree-1", "1", "graph")), tree, ErrAncestryUnresolved},
	} {
		for role, intent := range roles {
			// A resolved acceptance row is refused by its digest check,
			// not by an unresolved record.
			if _, err := intent(in(t, c.s, c.input)); !errors.Is(err, c.want) || c.want == ErrAcceptanceInput && errors.Is(err, ErrAncestryUnresolved) {
				t.Errorf("%s, %s: %v, want %v", name, role, err, c.want)
			}
		}
	}
	for role, intent := range roles {
		x := in(t, clean, tree)
		x.Records.Resolve = nil
		if _, err := intent(x); !errors.Is(err, ErrAncestryUnresolved) {
			t.Errorf("no resolver, %s: %v", role, err)
		}
		x = in(t, clean, tree)
		x.Records.Acceptance = nil
		if _, err := intent(x); !errors.Is(err, ErrAncestryUnresolved) {
			t.Errorf("the task's acceptance not read, %s: %v", role, err)
		}
		// An omitted input is an input too.
		x = in(t, newStore(rec("tree-1", acceptance, "graph")))
		x.Context.Omitted = []types.ManifestIntentOmittedItem{{Ref: ref(t, rec("tree-1", acceptance, "graph")), Reason: types.ManifestIntentOmittedItemReasonRestricted}}
		if _, err := intent(x); !errors.Is(err, ErrAcceptanceInput) || errors.Is(err, ErrAncestryUnresolved) {
			t.Errorf("an omitted acceptance input, %s: %v", role, err)
		}
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

// TestIntentBounds (Astra #252 r2 M3; Opus #289 M2, L1): each id is
// resolved once, however many paths reach it, and deduplicated before it
// is enqueued; the record bound is inclusive, and the edge and byte bounds
// hold apart from it, a wide lineage refused before any of it is enqueued;
// a cycle ends.
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
	w := newWalk(newStore(append(recs, input)...).records(), "builder")
	if _, err := w.start(ref(t, input)); err != nil || w.peak > total {
		t.Errorf("the queue's peak %d, over the %d distinct ids: %v", w.peak, total, err)
	}
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
	// One wide lineage: refused before its parents are enqueued.
	var many []string
	for i := range 10_000 {
		many = append(many, fmt.Sprintf("p-%05d", i)) // in sorted order, as a sealed ref keeps its lineage
	}
	wide := rec("tree-1", "1", "graph", many...)
	for name, b := range map[string][2]int{"the edge bound": {10, 1 << 20}, "the byte bound": {1 << 20, 100}} {
		maxRecords, maxEdges, maxIDBytes = 1<<20, b[0], b[1]
		w := newWalk(newStore(wide).records(), "builder")
		if _, err := w.start(ref(t, wide)); !errors.Is(err, ErrAncestryUnresolved) || !strings.Contains(fmt.Sprint(err), "ancestry is over") || len(w.walked) > 1 || w.peak != 0 {
			t.Errorf("one wide lineage past %s: %v, %d walked, a queue of %d", name, err, len(w.walked), w.peak)
		}
	}
	// The same lineage past the record bound only: refused before more ids
	// than the bound are scheduled (Astra #289 M1).
	maxRecords, maxEdges, maxIDBytes = 10, 1<<20, 1<<20
	w = newWalk(newStore(wide).records(), "builder")
	if _, err := w.start(ref(t, wide)); !errors.Is(err, ErrAncestryUnresolved) || !strings.Contains(fmt.Sprint(err), "ancestry is over") || len(w.walked) > maxRecords || w.peak >= maxRecords {
		t.Errorf("one wide lineage past the record bound: %v, %d scheduled, a queue of %d", err, len(w.walked), w.peak)
	}
	// A chain of exactly the record bound, its head and its leaf both
	// inputs: each record charged and resolved once (Astra #289 M2).
	maxRecords, maxEdges, maxIDBytes = 10_000, 100_000, 4<<20
	chain := make([]graph.RefRecord, maxRecords)
	for i := range chain {
		var parents []string
		if i+1 < len(chain) {
			parents = []string{fmt.Sprintf("c-%05d", i+1)}
		}
		chain[i] = rec(fmt.Sprintf("c-%05d", i), "1", "build", parents...)
	}
	s = newStore(chain...)
	if _, err := Builder(in(t, s, chain[0], chain[len(chain)-1])); err != nil {
		t.Errorf("a chain at the record bound, head and leaf supplied: %v", err)
	}
	for id, n := range s.lookups {
		if n != 1 {
			t.Errorf("%s resolved %d times", id, n)
		}
	}
	// A supplied ancestor is still compared with its record.
	forged := chain[len(chain)-1]
	forged.Lineage = []string{"c-00000"}
	x := in(t, newStore(chain...), chain[0])
	f := ref(t, forged)
	x.Context.Included, x.Bytes[f.IdentityDigest] = append(x.Context.Included, f), 10
	if _, err := Builder(x); !errors.Is(err, ErrAncestryUnresolved) {
		t.Errorf("a supplied ancestor whose ref is not its record: %v", err)
	}
	maxRecords, maxEdges, maxIDBytes = 1<<20, 1<<20, 1<<20
	cycle := newStore(rec("tree-1", "1", "graph", "a"), rec("a", "2", "build", "b"), rec("b", "3", "build", "a"))
	if _, err := Builder(in(t, cycle, rec("tree-1", "1", "graph", "a"))); err != nil {
		t.Errorf("a cycle: %v", err)
	}
}

// TestIntentFresh: a reviewer's input is never a review's result (a fresh
// context per round), nor its content under another id (Opus #289 L2); a
// builder may take one (a repair round's findings), and a reviewer may
// take a ref that derives from one.
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
	relay := rec("relay-1", "1", types.NodeKindIntegration) // the decision's content, re-recorded
	s = newStore(decision, relay)
	s.reviewed[decision.ContentDigest] = true
	if _, err := Reviewer(in(t, s, relay)); !errors.Is(err, ErrNotFresh) {
		t.Errorf("a review's content under another id: %v", err)
	}
	if _, err := Builder(in(t, s, relay)); err != nil {
		t.Errorf("a builder taking it: %v", err)
	}
}

// TestIntentDeclassified (22a-6L; #226 entries 91(3), 94(2)): a gate
// report derived from the acceptance (its truthful lineage) is taken by a
// reviewer through its record's Sanitised only, never by a builder; a
// review's findings view derived from it is taken by a builder through
// BuilderSafe only, never by a reviewer (a review's result); a
// declassified record whose own digest is acceptance is refused; a
// declassified ancestor is walked through.
func TestIntentDeclassified(t *testing.T) {
	accepted := string(digest(acceptance))
	report := rec("gate-report", "1", types.NodeKindGate, accepted)
	report.Sanitised = true
	decision := rec("decision", "2", types.NodeKindReview, "gate-report")
	view := rec("view", "3", types.NodeKindReview, "decision")
	view.BuilderSafe = true
	hidden := rec("hidden-report", acceptance, types.NodeKindGate)
	hidden.Sanitised, hidden.BuilderSafe = true, true
	via := rec("via", "4", types.NodeKindAccept, "gate-report")
	s := newStore(report, decision, view, hidden, via)
	for name, c := range map[string]struct {
		input graph.RefRecord
		want  map[string]error // by role; nil: taken
	}{
		"a sanitised gate report":           {report, map[string]error{"builder": ErrAcceptanceInput, "reviewer": nil}},
		"a builder-safe findings view":      {view, map[string]error{"builder": nil, "reviewer": ErrAcceptanceInput}}, // the reviewer walks it,
		"the decision it derives from":      {decision, map[string]error{"builder": ErrAcceptanceInput, "reviewer": ErrAcceptanceInput}},
		"a declassified acceptance blob":    {hidden, map[string]error{"builder": ErrAcceptanceInput, "reviewer": ErrAcceptanceInput}},
		"a sanitised report as an ancestor": {via, map[string]error{"builder": ErrAcceptanceInput, "reviewer": ErrAcceptanceInput}},
	} {
		for role, intent := range roles {
			_, err := intent(in(t, s, c.input))
			if want := c.want[role]; want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Errorf("%s, %s: %v, want %v", name, role, err, want)
			}
		}
	}
}

// TestIntentCheck (22a-6L; #226 entry 91(1)): a recorded intent is
// checked as it was built, under its consumer's role, each ref it lists
// (an input, an included or an omitted one); a recorded intent of another
// consumer, or with no records, is refused.
func TestIntentCheck(t *testing.T) {
	tree := rec("tree-1", "1", "graph")
	report := rec("gate-report", "2", types.NodeKindGate, string(digest(acceptance)))
	report.Sanitised = true
	s := newStore(tree, report)
	recorded := func(role string, inputs, included, omitted []graph.RefRecord) types.ManifestIntent {
		mi := call()
		mi.Consumer.Role = role
		for _, r := range inputs {
			mi.Inputs = append(mi.Inputs, ref(t, r))
		}
		for _, r := range included {
			mi.Included = append(mi.Included, types.ManifestIntentIncludedItem{Ref: ref(t, r), Bytes: 10, Purpose: "review input"})
		}
		for _, r := range omitted {
			mi.Omitted = append(mi.Omitted, types.ManifestIntentOmittedItem{Ref: ref(t, r), Reason: types.ManifestIntentOmittedItemReasonBudget})
		}
		return mi
	}
	one := []graph.RefRecord{report}
	for name, c := range map[string]struct {
		mi   types.ManifestIntent
		recs Records
		ok   bool
	}{
		"a reviewer's, the report an input": {recorded("reviewer", one, nil, nil), s.records(), true},
		"a builder's, the report an input":  {recorded("builder", one, nil, nil), s.records(), false},
		"a builder's, the report included":  {recorded("builder", nil, one, nil), s.records(), false},
		"a builder's, the report omitted":   {recorded("builder", nil, nil, one), s.records(), false},
		"a builder's, a clean tree":         {recorded("builder", []graph.RefRecord{tree}, nil, nil), s.records(), true},
		"another consumer's, a clean tree":  {recorded("planner", []graph.RefRecord{tree}, nil, nil), s.records(), false},
		"no records":                        {recorded("reviewer", one, nil, nil), Records{}, false},
		"the acceptance not read":           {recorded("reviewer", one, nil, nil), Records{Recorded: s.records().Recorded}, false},
	} {
		if err := Check(c.recs, c.mi); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}
