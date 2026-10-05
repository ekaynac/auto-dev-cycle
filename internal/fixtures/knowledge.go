// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// knowledgeStep maps a knowledge check to its step: passed is
// empty/completed_clean; a refusal carries its own outcome and reason. Any
// other error means the fixture could not run.
func knowledgeStep(err error) (Step, error) {
	var r *knowledge.Refusal
	switch {
	case err == nil:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	case errors.As(err, &r):
		return Step{r.Outcome, r.Reason.String()}, nil
	}
	return Step{}, err
}

// checked records a knowledge check's step.
func (o *observe) checked(err error) error {
	st, err := knowledgeStep(err)
	if err != nil {
		return err
	}
	o.steps = append(o.steps, st)
	return nil
}

// renderer renders every view in these fixtures.
var renderer = types.ViewFingerprintRenderer{Name: "review-packet", Version: "1", ConfigDigest: types.Digest("sha256:" + strings.Repeat("a", 64))}

// restrictedTo is an on-prem restriction of project p.
func restrictedTo(p types.ProjectID) types.Restriction {
	return types.Restriction{Project: p, PolicyDigest: types.Digest("sha256:" + strings.Repeat("b", 64)), Floor: types.RestrictionFloorOwn,
		Routing: types.RestrictionRoutingVerified, AllowedDestinations: []string{"local-gpu"}}
}

const (
	projectA = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	projectB = types.ProjectID("prj_01m3250vm7f6ktw39dszqnyddr")
)

// intentOf is a manifest intent that includes every input, in order.
func intentOf(inputs ...types.Ref) (types.ManifestIntent, error) {
	var idv [3]string
	for i, p := range []string{"cmf", "cal", "exe"} {
		v, err := ids.New(p)
		if err != nil {
			return types.ManifestIntent{}, err
		}
		idv[i] = v
	}
	included := make([]types.ManifestIntentIncludedItem, len(inputs))
	for i, r := range inputs {
		included[i] = types.ManifestIntentIncludedItem{Ref: r, Order: int64(i), Bytes: 1, Purpose: "input"}
	}
	return types.ManifestIntent{ID: types.ManifestID(idv[0]), Call: types.CallID(idv[1]),
		Consumer: types.ManifestIntentConsumer{Role: "reviewer", Executor: types.ExecutorID(idv[2]), ModelSnapshot: "model-2026-10", HarnessVersion: "1.0.0", CapabilityProfile: "large"},
		Inputs:   inputs,
		Policy:   types.ManifestIntentPolicy{UsageProfile: types.ManifestIntentPolicyUsageProfileStandard, DataPolicyDigest: renderer.ConfigDigest, RendererVersions: map[string]string{"review-packet": "1"}},
		Included: included, Omitted: []types.ManifestIntentOmittedItem{}, Budget: types.ManifestIntentBudget{Tokens: 1000, Bytes: 4000},
		RenderedDigest: renderer.ConfigDigest, Destination: "local-gpu", Routing: types.ManifestIntentRoutingVerified}, nil
}

// turn is the delivery event of turn n with the given tool results and
// adapter declarations.
func turn(in types.ManifestIntent, n int64, results []types.Ref, declarations ...string) types.ManifestDelivery {
	d := types.ManifestDelivery{Manifest: in.ID, Turn: n, ToolResults: []types.ManifestDeliveryToolResultsItem{}, Declarations: append([]string{}, declarations...)}
	for i, r := range results {
		d.ToolResults = append(d.ToolResults, types.ManifestDeliveryToolResultsItem{Handle: fmt.Sprintf("h%d", i), Ref: r})
	}
	return d
}

// sameBytesDifferentLineage: two refs over one stored blob, derived from
// different inputs with different restrictions, stay two refs; a view
// rendered from one is not served for the other, and a ref derived from
// their shared id keeps both restrictions (11 "identical bytes with
// different lineage"; spec/knowledge invariants 5 and 6).
func sameBytesDifferentLineage(_ context.Context, dir string) (steps []Step, violations []string, err error) {
	blobs, err := blobstore.Open(filepath.Join(dir, "state"))
	if err != nil {
		return nil, nil, err
	}
	var o observe
	// Store: the same bytes twice under two histories.
	var dg [2]string
	for i := range dg {
		if dg[i], err = blobs.Put(strings.NewReader("identical bytes\n"), ""); err != nil {
			return nil, nil, err
		}
	}
	ref := func(lineage string, p types.ProjectID) (types.Ref, error) {
		return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: dg[0], Scope: types.RefScope{Framework: true},
			Lineage: []string{lineage}, Restrictions: []types.Restriction{restrictedTo(p)}, ContentDigest: types.Digest(dg[0])})
	}
	a, errA := ref("lsn_from_a", projectA)
	b, errB := ref("lsn_from_b", projectB)
	if err := o.checked(errors.Join(errA, errB)); err != nil {
		return nil, nil, err
	}
	o.check(dg[0] == dg[1], "the same bytes were stored as two blobs: %s, %s", dg[0], dg[1])
	o.check(a.ContentDigest == b.ContentDigest, "the two refs name different bytes")
	o.check(a.IdentityDigest != b.IdentityDigest, "identical bytes with different lineage share the identity %s", a.IdentityDigest)
	o.check(slices.EqualFunc(a.Restrictions, []types.Restriction{restrictedTo(projectA)}, sameRestriction) && slices.EqualFunc(b.Restrictions, []types.Restriction{restrictedTo(projectB)}, sameRestriction),
		"a ref lost its restriction or inherited the other's")
	// Serve: a view rendered from a, looked up when the input is now b.
	view, err := knowledge.Fingerprint(renderer, []types.Ref{a})
	if err != nil {
		return nil, nil, err
	}
	if err := o.checked(knowledge.CheckServable(view, renderer, []types.Ref{b})); err != nil {
		return nil, nil, err
	}
	// Deliver: refs derived from the shared id, to a manifest with both.
	in, err := intentOf(a, b)
	if err != nil {
		return nil, nil, err
	}
	o.check(knowledge.CheckIntent(in) == nil, "a manifest with both refs as inputs is refused: %v", knowledge.CheckIntent(in))
	derived := func(rs ...types.Restriction) (types.Ref, error) {
		return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: "derived", Scope: types.RefScope{Framework: true},
			Lineage: []string{dg[0]}, Restrictions: rs, ContentDigest: types.Digest("sha256:" + strings.Repeat("c", 64))})
	}
	for _, rs := range [][]types.Restriction{{restrictedTo(projectA)}, {restrictedTo(projectA), restrictedTo(projectB)}} {
		r, err := derived(rs...)
		if err != nil {
			return nil, nil, err
		}
		if err := o.checked(knowledge.CheckDelivery(in, turn(in, 1, []types.Ref{r}))); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

func sameRestriction(x, y types.Restriction) bool {
	return x.Project == y.Project && x.PolicyDigest == y.PolicyDigest && x.Floor == y.Floor && x.Routing == y.Routing &&
		slices.Equal(x.AllowedDestinations, y.AllowedDestinations)
}

// declaredOnlyCoverage: a one-turn call whose prompt was delivered and
// whose only turn declares reading a file no input or tool result supplies.
// The declaration is recorded as declared-only, never as evidence: a record
// claiming complete coverage is refused, partial is accepted (11
// "declared-only coverage"; spec/knowledge invariant 9).
func declaredOnlyCoverage(_ context.Context, _ string) (steps []Step, violations []string, err error) {
	spec, err := knowledge.Seal(types.Ref{Type: types.RefTypeContract, ID: "spec", Scope: types.RefScope{Framework: true},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: renderer.ConfigDigest})
	if err != nil {
		return nil, nil, err
	}
	in, err := intentOf(spec)
	if err != nil {
		return nil, nil, err
	}
	const read = "read docs/design.md"
	deliveries := []types.ManifestDelivery{turn(in, 1, nil, read)}
	var o observe
	if err := o.checked(knowledge.CheckIntent(in)); err != nil {
		return nil, nil, err
	}
	if err := o.checked(knowledge.CheckDelivery(in, deliveries[0])); err != nil {
		return nil, nil, err
	}
	one := int64(1)
	c := types.ManifestFinalizedCoverage{Assembled: "complete", Prompt: types.ManifestFinalizedCoveragePromptDelivered, Turns: &one,
		DeliveredRefs: []types.Digest{spec.IdentityDigest}, PossiblyDeliveredRefs: []types.Digest{},
		DeclaredOnly: []types.ManifestFinalizedCoverageDeclaredOnlyItem{{Turn: 1, What: read}},
		Unobservable: []types.ManifestFinalizedCoverageUnobservableItem{}}
	for _, status := range []types.ManifestFinalizedCoverageStatus{types.ManifestFinalizedCoverageStatusComplete, types.ManifestFinalizedCoverageStatusPartial} {
		c.Status = status
		f := types.ManifestFinalized{Manifest: in.ID, Coverage: c, Delivery: types.ManifestFinalizedDelivery{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}}
		if err := o.checked(knowledge.CheckFinalized(in, deliveries, f)); err != nil {
			return nil, nil, err
		}
	}
	d, err := knowledge.Coverage(in, deliveries, c)
	o.check(err == nil && d.Status == types.ManifestFinalizedCoverageStatusPartial, "the evidence gives status %s (%v), want partial", d.Status, err)
	o.check(slices.Equal(d.Delivered, []types.Digest{spec.IdentityDigest}), "the delivered refs are %v: a declaration counted as delivery", d.Delivered)
	return o.steps, o.violations, nil
}
