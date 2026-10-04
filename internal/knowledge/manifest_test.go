// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func newID[T ~string](t *testing.T, prefix string) T {
	t.Helper()
	id, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return T(id)
}

type manifestFixture struct {
	intent          types.ManifestIntent
	tree, spec, old types.Ref // inputs: tree and spec included, old omitted
	x               types.Ref // a tool result derived from the tree
}

func fixture(t *testing.T) manifestFixture {
	t.Helper()
	mk := func(r types.Ref) types.Ref { return seal(t, r) }
	restricted := onPrem(prjA)
	f := manifestFixture{
		tree: mk(types.Ref{Type: types.RefTypeGitTree, ID: "git:" + strings.Repeat("c", 40), ContentDigest: dgA, Restrictions: []types.Restriction{restricted}}),
		spec: mk(types.Ref{Type: types.RefTypeContract, ID: "spec", ContentDigest: dgB}),
		old:  mk(types.Ref{Type: types.RefTypeFinding, ID: "fnd_old", ContentDigest: dgC}),
	}
	f.x = mk(types.Ref{Type: types.RefTypeGitPath, ID: "git:" + strings.Repeat("c", 40) + ":main.go", ContentDigest: dgC,
		Lineage: []string{f.tree.ID}, Restrictions: []types.Restriction{restricted}})
	f.intent = types.ManifestIntent{
		ID:       newID[types.ManifestID](t, "cmf"),
		Call:     newID[types.CallID](t, "cal"),
		Consumer: types.ManifestIntentConsumer{Role: "reviewer", Executor: newID[types.ExecutorID](t, "exe"), ModelSnapshot: "model-2026-10", HarnessVersion: "1.0.0", CapabilityProfile: "large"},
		Inputs:   []types.Ref{f.tree, f.spec, f.old},
		Policy:   types.ManifestIntentPolicy{UsageProfile: types.ManifestIntentPolicyUsageProfileStandard, DataPolicyDigest: dgA, RendererVersions: map[string]string{"review-packet": "1"}},
		Included: []types.ManifestIntentIncludedItem{
			{Ref: f.tree, Order: 0, Bytes: 100, Purpose: "candidate tree"},
			{Ref: f.spec, Order: 1, Bytes: 50, Purpose: "adopted contract"},
		},
		Omitted:        []types.ManifestIntentOmittedItem{{Ref: f.old, Reason: types.ManifestIntentOmittedItemReasonBudget}},
		Budget:         types.ManifestIntentBudget{Tokens: 1000, Bytes: 4000},
		RenderedDigest: dgB,
		Destination:    "local-gpu",
		Routing:        types.ManifestIntentRoutingVerified,
	}
	return f
}

func (f manifestFixture) turn(n int64, results ...types.Ref) types.ManifestDelivery {
	d := types.ManifestDelivery{Manifest: f.intent.ID, Turn: n, ToolResults: []types.ManifestDeliveryToolResultsItem{}, Declarations: []string{}}
	for i, r := range results {
		d.ToolResults = append(d.ToolResults, types.ManifestDeliveryToolResultsItem{Handle: "h" + string(rune('a'+i)), Ref: r})
	}
	return d
}

func coverage(prompt types.ManifestFinalizedCoveragePrompt, turns *int64, status types.ManifestFinalizedCoverageStatus, refs ...types.Digest) types.ManifestFinalizedCoverage {
	if refs == nil {
		refs = []types.Digest{}
	}
	return types.ManifestFinalizedCoverage{Assembled: "complete", Prompt: prompt, Turns: turns, DeliveredRefs: refs, PossiblyDeliveredRefs: []types.Digest{},
		DeclaredOnly: []types.ManifestFinalizedCoverageDeclaredOnlyItem{}, Unobservable: []types.ManifestFinalizedCoverageUnobservableItem{}, Status: status}
}

func (f manifestFixture) finalized(c types.ManifestFinalizedCoverage) types.ManifestFinalized {
	return types.ManifestFinalized{Manifest: f.intent.ID, Coverage: c,
		Delivery: types.ManifestFinalizedDelivery{Outcome: "empty", Reason: vocab.ReasonCompletedClean}}
}

func n64(v int64) *int64 { return &v }

const (
	delivered = types.ManifestFinalizedCoveragePromptDelivered
	notSent   = types.ManifestFinalizedCoveragePromptNotSent
	uncertain = types.ManifestFinalizedCoveragePromptUncertain
	complete  = types.ManifestFinalizedCoverageStatusComplete
	partial   = types.ManifestFinalizedCoverageStatusPartial
	unknown   = types.ManifestFinalizedCoverageStatusUnknown
)

func TestValidManifest(t *testing.T) {
	f := fixture(t)
	if err := CheckIntent(f.intent); err != nil {
		t.Fatal(err)
	}
	b, err := contract.Encode("records/manifest-intent", f.intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Decode[types.ManifestIntent]("records/manifest-intent", b); err != nil {
		t.Fatal(err)
	}
	deliveries := []types.ManifestDelivery{f.turn(1, f.x), f.turn(2)}
	fin := f.finalized(coverage(delivered, n64(2), complete, f.x.IdentityDigest, f.spec.IdentityDigest, f.tree.IdentityDigest))
	if err := CheckFinalized(f.intent, deliveries, fin); err != nil {
		t.Fatalf("a complete manifest: %v", err)
	}
}

// Cuts are named, never silent: every input is included or omitted exactly
// once, and nothing else is.
func TestIntentRefusals(t *testing.T) {
	cases := map[string]func(f *manifestFixture){
		"input neither included nor omitted": func(f *manifestFixture) { f.intent.Omitted = []types.ManifestIntentOmittedItem{} },
		"included in place of an input":      func(f *manifestFixture) { f.intent.Included[0].Ref = f.x },
		"included but not an input": func(f *manifestFixture) {
			f.intent.Included = append(f.intent.Included, types.ManifestIntentIncludedItem{Ref: f.x, Order: 2, Bytes: 1, Purpose: "x"})
		},
		"both included and omitted": func(f *manifestFixture) {
			f.intent.Omitted = append(f.intent.Omitted, types.ManifestIntentOmittedItem{Ref: f.tree, Reason: types.ManifestIntentOmittedItemReasonPolicy})
		},
		"input named twice":  func(f *manifestFixture) { f.intent.Inputs = append(f.intent.Inputs, f.tree) },
		"order gap":          func(f *manifestFixture) { f.intent.Included[1].Order = 2 },
		"order reversed":     func(f *manifestFixture) { f.intent.Included[0].Order, f.intent.Included[1].Order = 1, 0 },
		"unchecked input":    func(f *manifestFixture) { f.intent.Inputs[0].ContentDigest = dgC },
		"unchecked included": func(f *manifestFixture) { f.intent.Included[0].Ref.ContentDigest = dgC },
		"schema":             func(f *manifestFixture) { f.intent.Destination = "Not A Slug" },
	}
	for name, mutate := range cases {
		f := fixture(t)
		f.intent.Inputs = append([]types.Ref(nil), f.intent.Inputs...)
		f.intent.Included = append([]types.ManifestIntentIncludedItem(nil), f.intent.Included...)
		mutate(&f)
		if err := CheckIntent(f.intent); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Astra #38: a delivered tool result stays within the declared inputs.
func TestDeliveryRefusals(t *testing.T) {
	f := fixture(t)
	if err := CheckDelivery(f.intent, f.turn(1, f.spec)); err != nil {
		t.Fatalf("a declared input as a tool result: %v", err)
	}
	other := f.turn(1, f.x)
	other.Manifest = newID[types.ManifestID](t, "cmf")
	if err := CheckDelivery(f.intent, other); err == nil {
		t.Error("a delivery for another manifest")
	}
	dup := f.turn(1, f.x, f.tree)
	dup.ToolResults[1].Handle = dup.ToolResults[0].Handle
	if err := CheckDelivery(f.intent, dup); err == nil {
		t.Error("a handle supplied twice")
	}
	forged := f.turn(1, f.x)
	forged.ToolResults[0].Ref.ContentDigest = dgA
	if err := CheckDelivery(f.intent, forged); err == nil {
		t.Error("an unchecked tool result")
	}
	unrelated := seal(t, types.Ref{Type: types.RefTypeBlob, ID: string(dgC), ContentDigest: dgC})
	if err := CheckDelivery(f.intent, f.turn(1, unrelated)); err == nil {
		t.Error("a tool result unrelated to the declared inputs")
	}
	foreign := seal(t, types.Ref{Type: types.RefTypeBlob, ID: "derived", ContentDigest: dgC, Lineage: []string{"elsewhere"}})
	if err := CheckDelivery(f.intent, f.turn(1, foreign)); err == nil {
		t.Error("a tool result derived from something that is not a declared input")
	}
	laundered := seal(t, types.Ref{Type: types.RefTypeBlob, ID: "derived", ContentDigest: dgC, Lineage: []string{f.tree.ID}})
	if err := CheckDelivery(f.intent, f.turn(1, laundered)); err == nil || !strings.Contains(err.Error(), "drops a restriction") {
		t.Errorf("a derived tool result that drops its input's restriction: %v", err)
	}
}

// The status is derived from evidence, never from declarations alone.
func TestCoverageStatus(t *testing.T) {
	f := fixture(t)
	both := []types.ManifestDelivery{f.turn(1), f.turn(2)}
	declared := func(c types.ManifestFinalizedCoverage) types.ManifestFinalizedCoverage {
		c.DeclaredOnly = []types.ManifestFinalizedCoverageDeclaredOnlyItem{{Turn: 1, What: "read docs"}}
		return c
	}
	unobservable := func(c types.ManifestFinalizedCoverage) types.ManifestFinalizedCoverage {
		c.Unobservable = []types.ManifestFinalizedCoverageUnobservableItem{{Turn: nil, What: "harness_instructions"}}
		return c
	}
	cases := []struct {
		name       string
		deliveries []types.ManifestDelivery
		c          types.ManifestFinalizedCoverage
		want       types.ManifestFinalizedCoverageStatus
	}{
		{"every turn evidenced", both, coverage(delivered, n64(2), ""), complete},
		{"not sent", nil, coverage(notSent, n64(0), ""), complete},
		{"a turn without evidence", both[:1], coverage(delivered, n64(2), ""), partial},
		{"declared only", both, declared(coverage(delivered, n64(2), "")), partial},
		{"unobservable", both, unobservable(coverage(delivered, n64(2), "")), partial},
		{"turns unknown", both, coverage(delivered, nil, ""), unknown},
		{"prompt uncertain", both, coverage(uncertain, n64(2), ""), unknown},
	}
	for _, c := range cases {
		d, err := Coverage(f.intent, c.deliveries, c.c)
		if err != nil || d.Status != c.want {
			t.Errorf("%s: %s, %v; want %s", c.name, d.Status, err, c.want)
		}
	}
}

// Astra #38: the evidence the record must reflect, and the turns it may name.
func TestCoverageRefusesContradictions(t *testing.T) {
	f := fixture(t)
	compacted := f.turn(1)
	compacted.Compacted = true
	declaring := f.turn(1)
	declaring.Declarations = []string{"read docs"}
	withDeclared := coverage(delivered, n64(1), "")
	withDeclared.DeclaredOnly = []types.ManifestFinalizedCoverageDeclaredOnlyItem{{Turn: 99, What: "x"}}
	withUnobs := coverage(delivered, n64(1), "")
	withUnobs.Unobservable = []types.ManifestFinalizedCoverageUnobservableItem{{Turn: n64(5), What: "compaction"}}
	cases := map[string]struct {
		deliveries []types.ManifestDelivery
		c          types.ManifestFinalizedCoverage
	}{
		"compaction left out":            {[]types.ManifestDelivery{compacted}, coverage(delivered, n64(1), "")},
		"declaration left out":           {[]types.ManifestDelivery{declaring}, coverage(delivered, n64(1), "")},
		"delivery beyond the last turn":  {[]types.ManifestDelivery{f.turn(3)}, coverage(delivered, n64(1), "")},
		"declared-only beyond the turns": {nil, withDeclared},
		"unobservable beyond the turns":  {nil, withUnobs},
		"two deliveries for one turn":    {[]types.ManifestDelivery{f.turn(1), f.turn(1)}, coverage(delivered, n64(2), "")},
		"not sent with a delivery":       {[]types.ManifestDelivery{f.turn(1)}, coverage(notSent, n64(0), "")},
		"not sent with turns":            {nil, coverage(notSent, n64(1), "")},
	}
	for name, c := range cases {
		if _, err := Coverage(f.intent, c.deliveries, c.c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Recorded properly, the same evidence is partial, never complete.
	ok := coverage(delivered, n64(1), "")
	ok.Unobservable = []types.ManifestFinalizedCoverageUnobservableItem{{Turn: n64(1), What: "compaction"}}
	if d, err := Coverage(f.intent, []types.ManifestDelivery{compacted}, ok); err != nil || d.Status != partial {
		t.Fatalf("a recorded compaction: %s, %v", d.Status, err)
	}
}

// Astra #38: assembled is not delivered: refused egress delivers nothing,
// and an uncertain prompt is only possibly delivered.
func TestPromptDeliveryEvidence(t *testing.T) {
	f := fixture(t)
	refused := f.finalized(coverage(notSent, n64(0), complete))
	refused.Delivery = types.ManifestFinalizedDelivery{Outcome: "failed", Reason: mustParse(t, "routing_violation")}
	if err := CheckFinalized(f.intent, nil, refused); err != nil {
		t.Fatalf("refused egress with nothing delivered: %v", err)
	}
	claimed := refused
	claimed.Coverage.DeliveredRefs = []types.Digest{f.tree.IdentityDigest, f.spec.IdentityDigest}
	if err := CheckFinalized(f.intent, nil, claimed); err == nil {
		t.Fatal("refused egress recorded as delivering the prompt")
	}
	unsure := f.finalized(coverage(uncertain, nil, unknown))
	unsure.Coverage.PossiblyDeliveredRefs = []types.Digest{f.spec.IdentityDigest, f.tree.IdentityDigest}
	if err := CheckFinalized(f.intent, nil, unsure); err != nil {
		t.Fatalf("an uncertain prompt as possibly delivered: %v", err)
	}
	silent := unsure
	silent.Coverage.PossiblyDeliveredRefs = []types.Digest{}
	if err := CheckFinalized(f.intent, nil, silent); err == nil {
		t.Fatal("an uncertain prompt recorded as neither delivered nor possibly delivered")
	}
	confirmed := unsure
	confirmed.Coverage.DeliveredRefs, confirmed.Coverage.PossiblyDeliveredRefs = unsure.Coverage.PossiblyDeliveredRefs, []types.Digest{}
	if err := CheckFinalized(f.intent, nil, confirmed); err == nil {
		t.Fatal("an uncertain prompt recorded as confirmed delivery")
	}
}

func mustParse(t *testing.T, code string) types.ReasonCode {
	t.Helper()
	r, err := vocab.ParseReason(code)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFinalizedRefusals(t *testing.T) {
	f := fixture(t)
	deliveries := []types.ManifestDelivery{f.turn(1, f.x)}
	refs := []types.Digest{f.tree.IdentityDigest, f.spec.IdentityDigest, f.x.IdentityDigest}
	if err := CheckFinalized(f.intent, deliveries, f.finalized(coverage(delivered, n64(2), complete, refs...))); err == nil || !strings.Contains(err.Error(), "evidence gives partial") {
		t.Fatalf("a complete claim over a missing turn: %v", err)
	}
	if err := CheckFinalized(f.intent, deliveries, f.finalized(coverage(delivered, n64(2), partial, refs[2], refs[0], refs[1]))); err != nil {
		t.Fatalf("the derived status, refs in another order: %v", err)
	}
	if err := CheckFinalized(f.intent, deliveries, f.finalized(coverage(delivered, n64(1), complete, refs[:2]...))); err == nil {
		t.Fatal("delivered_refs without a delivered tool result")
	}
	if err := CheckFinalized(f.intent, deliveries, f.finalized(coverage(delivered, n64(1), complete, append(refs, f.old.IdentityDigest)...))); err == nil {
		t.Fatal("delivered_refs with an omitted input")
	}
	other := f.finalized(coverage(delivered, n64(1), complete, refs...))
	other.Manifest = newID[types.ManifestID](t, "cmf")
	if err := CheckFinalized(f.intent, deliveries, other); err == nil {
		t.Fatal("a finalized record for another manifest")
	}
	// Astra #38: the intent is checked too.
	silent := f
	silent.intent.Omitted = []types.ManifestIntentOmittedItem{}
	if err := CheckFinalized(silent.intent, deliveries, f.finalized(coverage(delivered, n64(1), complete, refs...))); err == nil {
		t.Fatal("finalization over an intent with a silent cut")
	}
}
