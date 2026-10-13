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

// The builder's intent lists exactly its context: the included inputs in
// order, the omitted ones with their reasons.
func TestBuilderIntent(t *testing.T) {
	spec, base, old := ref(t, "1"), ref(t, "2"), ref(t, "3")
	ctx := graph.Rendered{Included: []types.Ref{spec, base}, Omitted: []types.ManifestIntentOmittedItem{{Ref: old, Reason: types.ManifestIntentOmittedItemReasonBudget}}}
	in, err := BuilderIntent(call(t), ctx, map[types.Digest]int64{spec.IdentityDigest: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Consumer.Role != "builder" || len(in.Inputs) != 3 || in.Included[1].Ref.ID != base.ID || in.Included[0].Bytes != 10 || len(in.Omitted) != 1 {
		t.Fatalf("intent %+v", in)
	}
}

// I18: an acceptance blob, or anything derived from one, refuses the
// builder's intent, included or omitted.
func TestBuilderIntentRefusesAcceptance(t *testing.T) {
	hidden := ref(t, "9")
	derived := ref(t, "8", hidden.ID)
	for name, ctx := range map[string]graph.Rendered{
		"included": {Included: []types.Ref{ref(t, "1"), hidden}},
		"omitted":  {Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{{Ref: hidden, Reason: types.ManifestIntentOmittedItemReasonRestricted}}},
		"derived":  {Included: []types.Ref{derived}},
	} {
		if _, err := BuilderIntent(call(t), ctx, nil, []types.Digest{hidden.ContentDigest}); !errors.Is(err, ErrAcceptanceInput) {
			t.Errorf("%s: %v, want ErrAcceptanceInput", name, err)
		}
	}
}
