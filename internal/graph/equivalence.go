// SPDX-License-Identifier: Apache-2.0

package graph

// Typed-conclusion equivalence (13 "Reuse decisions use typed conclusions",
// "v1 scope"; plan 15 G5).
//
// Decisions:
//   - Only on the declared edges, a review's gate or evidence input: any
//     other edge is an error, never "equivalent".
//   - It is the review's decision about its own result: the review is
//     completed, at its identity's current generation, on a claim whose
//     input resolved to used; fresh is the declared producer's selected
//     result now. So the consumer's non-input identity is the review's own.
//   - Both results are completed and conclusive, and hold whole receipts
//     (receiptOf) with the same conclusions digest (06 rule 2a: the demand
//     and each step's name, outcome and reason), subject and instrument;
//     their receipt Refs have the same project scope and restrictions. Ref
//     lineage is not compared: it records derivation, not what was
//     concluded. Equivalence decides only whether the review runs again;
//     manifests still name the exact result ids.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Equivalent reports whether fresh may stand for used on review's input
// name, so the review need not run again.
func Equivalent(tx *eventstore.Tx, review types.NodeID, name types.Name, used, fresh types.ResultID, blobs receipt.Blobs) (bool, error) {
	rec, err := loadNode(tx, review)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool { return in.Name == name })
	var src *types.ResultSource
	if i >= 0 {
		src, _ = rec.Recorded.Node.Inputs[i].Source.Variant.(*types.ResultSource)
	}
	if rec.Recorded.Node.Kind != types.NodeKindReview || src == nil || !carriesConclusions(src.ResultKind) {
		return false, fmt.Errorf("graph: %s's input %s is no review←gate or review←evidence edge", review, name)
	}
	c, claimed, err := load[claim](tx, ClaimProjection, string(review))
	if err != nil || !claimed {
		return false, errors.Join(err, fmt.Errorf("graph: %s never ran", review))
	}
	cur, err := current(tx, rec, resolutions(c.Inputs))
	if err != nil || rec.State.State != types.NodeStateStateCompleted || *rec.State.Key != c.Key || c.Key.Generation != cur || resolutions(c.Inputs)[string(name)].Result != used {
		return false, errors.Join(err, fmt.Errorf("graph: %s has no standing result that used %s", review, used))
	}
	if now, why, err := selectedOf(tx, rec, src); err != nil || why != "" || now.Result.ID != fresh {
		return false, errors.Join(err, fmt.Errorf("graph: %s is not %s's selected result", fresh, src.Producer))
	}
	var refs [2]types.Ref
	var rcpts [2]types.Receipt
	for j, id := range []types.ResultID{used, fresh} {
		r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(id))
		// The claim and selectedOf fixed the kind; the outcome is checked
		// again here, as a selection is only of a conclusive result.
		if err != nil || !ok || !r.Result.Completed || r.Result.Outcome == types.OutcomeCouldNotRun {
			return false, errors.Join(err, fmt.Errorf("graph: %s is no conclusive %s result", id, src.ResultKind))
		}
		if refs[j], rcpts[j], err = receiptOf(r, blobs); err != nil {
			return false, err
		}
	}
	a, b := rcpts[0], rcpts[1]
	return a.ConclusionsDigest == b.ConclusionsDigest && reflect.DeepEqual(a.Subject, b.Subject) && a.Instrument == b.Instrument &&
		reflect.DeepEqual(refs[0].Scope, refs[1].Scope) &&
		(reflect.DeepEqual(refs[0].Restrictions, refs[1].Restrictions) || mutation.Is(mutation.EquivalenceIgnoresRestrictions)), nil
}
