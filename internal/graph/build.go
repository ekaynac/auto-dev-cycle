// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// BuildFacts are the two facts a build's result is computed from (13
// "Facts, not claims"), both observed by the controller.
type BuildFacts struct {
	// Completed: the executor reached a terminal result the controller
	// accepted (05), and that result was found or empty. A could_not_run or
	// failed report, a refused or missing result and a lost attempt are not
	// completion.
	Completed bool
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

// Build is the build outcome table (13 "Facts, not claims"):
//
//	completed, diff     → patch, found
//	completed, no diff  → patch, empty
//	incomplete, diff    → partial_patch, could_not_run, the exit map's reason
//	incomplete, no diff → patch, could_not_run, the exit map's reason; never empty
//
// The adapter's own outcome never decides it. The result answers key, has a
// fresh res_ id, and its value is the diff, when there is one.
func Build(key types.ComputationKey, f BuildFacts) (types.Result, error) {
	id, err := ids.New("res")
	if err != nil {
		return types.Result{}, err
	}
	r := types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindPatch, Value: []types.Ref{}, Completed: f.Completed}
	changed := f.Diff != ""
	if changed && !(mutation.Is(mutation.PartialPatchDropped) && !f.Completed) {
		ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(f.Diff), Scope: types.RefScope{Project: &f.Project},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: f.Diff})
		if err != nil {
			return types.Result{}, fmt.Errorf("graph: the diff's ref: %w", err)
		}
		r.Value = []types.Ref{ref}
	}
	switch {
	case f.Completed && changed:
		r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, "the build produced a change"
	case f.Completed:
		r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	case !changed && mutation.Is(mutation.IncompleteNoDiffEmpty):
		r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
	default:
		r.Outcome, r.Reason = types.OutcomeCouldNotRun, exitmap.Reason(f.Exit)
		r.Detail = "the executor did not complete; nothing changed"
		if changed && !mutation.Is(mutation.PartialPatchDropped) {
			r.Kind, r.Detail = types.ResultKindPartialPatch, "the executor did not complete; its partial change is sealed"
		}
	}
	return r, nil
}

// Check validates a result against records/result.
func Check(r types.Result) error {
	_, err := contract.Encode("records/result", r)
	return err
}
