// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

func id[T ~string](t *testing.T, prefix string) T {
	t.Helper()
	s, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return T(s)
}

func ref(t *testing.T, digest string, lineage ...string) types.Ref {
	t.Helper()
	if lineage == nil {
		lineage = []string{}
	}
	d := types.Digest("sha256:" + strings.Repeat(digest, 64))
	r, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(d), Lineage: lineage, Restrictions: []types.Restriction{}, ContentDigest: d})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func call(t *testing.T) types.ManifestIntent {
	task, att := id[types.TaskID](t, "tsk"), id[types.AttemptID](t, "att")
	zero := types.Digest("sha256:" + strings.Repeat("0", 64))
	return types.ManifestIntent{ID: id[types.ManifestID](t, "cmf"), Call: id[types.CallID](t, "cal"),
		Consumer: types.ManifestIntentConsumer{Role: "reviewer", Executor: id[types.ExecutorID](t, "exe"), ModelSnapshot: "m", HarnessVersion: "1", CapabilityProfile: "large"},
		Task:     &task, Attempt: &att, RenderedDigest: zero, Inputs: []types.Ref{ref(t, "7")},
		Policy:      types.ManifestIntentPolicy{UsageProfile: types.ManifestIntentPolicyUsageProfileStandard, DataPolicyDigest: zero, RendererVersions: map[string]string{}},
		Destination: "stub", Routing: types.ManifestIntentRoutingVerified}
}

// records is an Ancestry over a fixed record set.
func records(refs ...types.Ref) Ancestry {
	m := map[string][]string{}
	for _, r := range refs {
		m[r.ID] = r.Lineage
	}
	return func(id string) ([]string, bool) { l, ok := m[id]; return l, ok }
}

// The builder's intent lists exactly its context: the included inputs in
// order, the omitted ones with their reasons.
func TestBuilderIntent(t *testing.T) {
	root := ref(t, "4")
	spec, base, old := ref(t, "1", root.ID), ref(t, "2"), ref(t, "3")
	ctx := graph.Rendered{Included: []types.Ref{spec, base}, Omitted: []types.ManifestIntentOmittedItem{{Ref: old, Reason: types.ManifestIntentOmittedItemReasonBudget}}}
	in, err := BuilderIntent(call(t), ctx, map[types.Digest]int64{spec.IdentityDigest: 10, base.IdentityDigest: 20}, nil, records(root))
	if err != nil {
		t.Fatal(err)
	}
	if in.Consumer.Role != "builder" || len(in.Inputs) != 3 || in.Included[1].Ref.ID != base.ID || in.Included[0].Bytes != 10 || len(in.Omitted) != 1 {
		t.Fatalf("intent %+v", in)
	}
	// L5: an included input without its size is an error, never 0 bytes.
	if _, err := BuilderIntent(call(t), ctx, map[types.Digest]int64{spec.IdentityDigest: 10}, nil, records(root)); err == nil {
		t.Error("an included input without its size was sized 0")
	}
}

// I18 (Astra H4): an acceptance blob, or anything derived from one at any
// depth, refuses the builder's intent, included or omitted, whether or not
// its intermediates are in the context; so does an unresolvable ancestor.
func TestBuilderIntentRefusesAcceptance(t *testing.T) {
	hidden := ref(t, "9")
	derived := ref(t, "8", hidden.ID)
	summary := ref(t, "6", derived.ID) // two hops from the acceptance blob
	known := records(hidden, derived, summary)
	for name, c := range map[string]struct {
		ctx  graph.Rendered
		want error
	}{
		"included":       {graph.Rendered{Included: []types.Ref{ref(t, "1"), hidden}}, ErrAcceptanceInput},
		"omitted":        {graph.Rendered{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{{Ref: hidden, Reason: types.ManifestIntentOmittedItemReasonRestricted}}}, ErrAcceptanceInput},
		"derived":        {graph.Rendered{Included: []types.Ref{derived}}, ErrAcceptanceInput},
		"summary only":   {graph.Rendered{Included: []types.Ref{summary}}, ErrAcceptanceInput},
		"unresolvable":   {graph.Rendered{Included: []types.Ref{ref(t, "5", "sha256:unknown")}}, ErrAncestryUnresolved},
		"no ancestry fn": {graph.Rendered{Included: []types.Ref{summary}}, ErrAncestryUnresolved},
	} {
		anc := known
		if name == "no ancestry fn" {
			anc = nil
		}
		if _, err := BuilderIntent(call(t), c.ctx, nil, []types.Digest{hidden.ContentDigest}, anc); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// The summary-only refusal comes from the ancestry records: with a
	// record of its parent that names no acceptance blob, it passes.
	clean := records(hidden, ref(t, "8"), summary)
	if _, err := BuilderIntent(call(t), graph.Rendered{Included: []types.Ref{summary}}, map[types.Digest]int64{summary.IdentityDigest: 1}, []types.Digest{hidden.ContentDigest}, clean); err != nil {
		t.Errorf("a summary of a clean ancestor: %v", err)
	}
}
