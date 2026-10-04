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
	intent             types.ManifestIntent
	tree, spec, old, x types.Ref // inputs (tree, spec included; old omitted); x is a tool result
}

func fixture(t *testing.T) manifestFixture {
	t.Helper()
	mk := func(typ types.RefType, id string, d types.Digest) types.Ref {
		return seal(t, types.Ref{Type: typ, ID: id, ContentDigest: d})
	}
	f := manifestFixture{
		tree: mk(types.RefTypeGitTree, "git:"+strings.Repeat("c", 40), dgA),
		spec: mk(types.RefTypeContract, "spec", dgB),
		old:  mk(types.RefTypeFinding, "fnd_old", dgC),
		x:    mk(types.RefTypeBlob, string(dgC), dgC),
	}
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

func (f manifestFixture) finalized(turns *int64, status types.ManifestFinalizedCoverageStatus, refs ...types.Digest) types.ManifestFinalized {
	return types.ManifestFinalized{
		Manifest: f.intent.ID,
		Coverage: types.ManifestFinalizedCoverage{Assembled: "complete", Turns: turns, DeliveredRefs: refs,
			DeclaredOnly: []types.ManifestFinalizedCoverageDeclaredOnlyItem{}, Unobservable: []types.ManifestFinalizedCoverageUnobservableItem{}, Status: status},
		Delivery: types.ManifestFinalizedDelivery{Outcome: "empty", Reason: vocab.ReasonCompletedClean},
	}
}

func n64(v int64) *int64 { return &v }

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
	fin := f.finalized(n64(2), types.ManifestFinalizedCoverageStatusComplete, f.x.IdentityDigest, f.spec.IdentityDigest, f.tree.IdentityDigest)
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

func TestDeliveryRefusals(t *testing.T) {
	f := fixture(t)
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
}

// The status is derived from evidence, never from declarations alone.
func TestCoverageStatus(t *testing.T) {
	f := fixture(t)
	both := []types.ManifestDelivery{f.turn(1), f.turn(2)}
	cases := []struct {
		name               string
		deliveries         []types.ManifestDelivery
		turns              *int64
		declared, unobserv int
		want               types.ManifestFinalizedCoverageStatus
	}{
		{"every turn evidenced", both, n64(2), 0, 0, types.ManifestFinalizedCoverageStatusComplete},
		{"no turns", nil, n64(0), 0, 0, types.ManifestFinalizedCoverageStatusComplete},
		{"a turn without evidence", both[:1], n64(2), 0, 0, types.ManifestFinalizedCoverageStatusPartial},
		{"declared only", both, n64(2), 1, 0, types.ManifestFinalizedCoverageStatusPartial},
		{"unobservable", both, n64(2), 0, 1, types.ManifestFinalizedCoverageStatusPartial},
		{"turns unknown", both, nil, 0, 0, types.ManifestFinalizedCoverageStatusUnknown},
		{"turns unknown, unobservable", both, nil, 0, 1, types.ManifestFinalizedCoverageStatusUnknown},
	}
	for _, c := range cases {
		_, got, err := Coverage(f.intent, c.deliveries, c.turns, c.declared, c.unobserv)
		if err != nil || got != c.want {
			t.Errorf("%s: %s, %v; want %s", c.name, got, err, c.want)
		}
	}
	if _, _, err := Coverage(f.intent, []types.ManifestDelivery{f.turn(1), f.turn(1)}, n64(2), 0, 0); err == nil {
		t.Error("two delivery events for one turn")
	}
	if _, _, err := Coverage(f.intent, []types.ManifestDelivery{f.turn(3)}, n64(2), 0, 0); err == nil {
		t.Error("a delivery beyond the last turn")
	}
}

func TestFinalizedRefusals(t *testing.T) {
	f := fixture(t)
	deliveries := []types.ManifestDelivery{f.turn(1, f.x)}
	refs := []types.Digest{f.tree.IdentityDigest, f.spec.IdentityDigest, f.x.IdentityDigest}
	claimed := f.finalized(n64(2), types.ManifestFinalizedCoverageStatusComplete, refs...)
	if err := CheckFinalized(f.intent, deliveries, claimed); err == nil || !strings.Contains(err.Error(), "evidence gives partial") {
		t.Fatalf("a complete claim over a missing turn: %v", err)
	}
	if err := CheckFinalized(f.intent, deliveries, f.finalized(n64(2), types.ManifestFinalizedCoverageStatusPartial, refs...)); err != nil {
		t.Fatalf("the derived status, refs in another order: %v", err)
	}
	missing := f.finalized(n64(1), types.ManifestFinalizedCoverageStatusComplete, f.tree.IdentityDigest, f.spec.IdentityDigest)
	if err := CheckFinalized(f.intent, deliveries, missing); err == nil {
		t.Fatal("delivered_refs without a delivered tool result")
	}
	extra := f.finalized(n64(1), types.ManifestFinalizedCoverageStatusComplete, append(refs, f.old.IdentityDigest)...)
	if err := CheckFinalized(f.intent, deliveries, extra); err == nil {
		t.Fatal("delivered_refs with an omitted input")
	}
	other := f.finalized(n64(1), types.ManifestFinalizedCoverageStatusComplete, refs...)
	other.Manifest = newID[types.ManifestID](t, "cmf")
	if err := CheckFinalized(f.intent, deliveries, other); err == nil {
		t.Fatal("a finalized record for another manifest")
	}
}
