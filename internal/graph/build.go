// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// BuildFacts are what a build's result is computed from (13 "Facts, not
// claims"), all observed by the controller.
type BuildFacts struct {
	// Submitted: the attempt's result was accepted and recorded
	// (attempt.result_submitted, 05). Reported is that result's outcome.
	// Completion is a submitted found or empty: a reported could_not_run or
	// failed is not completion, nor is a refused or missing result or a
	// lost attempt.
	Submitted bool
	Reported  types.Outcome
	// Diff is the digest of the controller's own diff of the sandbox output
	// tree against the base, published to the blob store; empty when the
	// tree is unchanged.
	Diff types.Digest
	// Exit is what the controller knows of an executor that did not
	// complete; it fills only the reason (02, internal/vocab/exitmap).
	Exit exitmap.Exit
	// Project scopes the diff's ref (10).
	Project types.ProjectID
}

// Completed reports whether the executor completed.
func (f BuildFacts) Completed() bool {
	return f.Submitted && (f.Reported == types.OutcomeFound || f.Reported == types.OutcomeEmpty)
}

// Build is the build outcome table (13 "Facts, not claims"):
//
//	completed, diff     → patch, found
//	completed, no diff  → patch, empty
//	incomplete, diff    → partial_patch, could_not_run, the exit map's reason
//	incomplete, no diff → patch, could_not_run, the exit map's reason; never empty
//
// The adapter's report decides only whether it completed; its reason and
// its "no changes" never decide the result. The result answers key, has a
// fresh res_ id, and its value is the diff, when there is one.
func Build(key types.ComputationKey, f BuildFacts) (types.Result, error) {
	id, err := ids.New("res")
	if err != nil {
		return types.Result{}, err
	}
	done := f.Completed()
	r := types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindPatch, Value: []types.Ref{}, Completed: done}
	changed := f.Diff != ""
	if changed && !(mutation.Is(mutation.PartialPatchDropped) && !done) {
		ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(f.Diff), Scope: types.RefScope{Project: &f.Project},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: f.Diff})
		if err != nil {
			return types.Result{}, fmt.Errorf("graph: the diff's ref: %w", err)
		}
		r.Value = []types.Ref{ref}
	}
	switch {
	case done && changed:
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "the build produced a change"
	case done:
		r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	case !changed && mutation.Is(mutation.IncompleteNoDiffEmpty):
		r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	default:
		r.Outcome, r.Reason = types.OutcomeCouldNotRun, exitmap.Reason(f.Exit)
		r.Detail = "no accepted result"
		if f.Submitted {
			r.Detail = "the executor reported " + string(f.Reported)
		}
		r.Detail += "; nothing changed"
		if changed && !mutation.Is(mutation.PartialPatchDropped) {
			r.Kind, r.Detail = types.ResultKindPartialPatch, strings.TrimSuffix(r.Detail, "; nothing changed")+"; its partial change is sealed"
		}
	}
	return r, nil
}

// Check validates a result against records/result, and its outcome and
// reason against the vocabulary (a writer's check: the reason is current
// and fits the outcome, 02).
func Check(r types.Result) error {
	if _, err := contract.Encode("records/result", r); err != nil {
		return err
	}
	reg, err := vocab.Load()
	if err != nil {
		return err
	}
	return reg.Check(vocab.Result{Outcome: vocab.Outcome(r.Outcome), Reason: r.Reason, Detail: r.Detail})
}
