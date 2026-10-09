// SPDX-License-Identifier: Apache-2.0

package graph

// The selected review requirement of a subject, and what a requirement
// serves (13 "The required review is a graph input"; plan 15 G7b-2).
//
// Decisions:
//   - A subject has one selected requirement revision per task
//     (graph_requirements, supersede.go); only a supersession moves it. The
//     projection keeps the node that selected it, which a generation's
//     carry_over and merge take.
//   - A review_requirement result serves a consumer only in its own task
//     and only at its subject's selected revision there (serves, read by
//     selectedOf). So the review's claim binds the subject's current
//     requirement; a carry_over and a merge consume it under the same rule;
//     and every re-validation (the resolver, a commit, an effect's intent,
//     the result folds) sees a roster change as a missing input: the merge
//     waits, or its intent is refused as stale_expected_state.
//   - A carry_over serves a merge only by its attestation (a demand for a
//     delta or replacement review is a structural wait), and only while the
//     review result it attested is its review's selected result and go:
//     the merge consumes the verdict, and waits for a re-run review
//     (G7b-2b).

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

func requirementIndex(task types.TaskID, subject types.Name) string {
	return string(task) + "/" + string(subject)
}

// selectedRequirement is subject's selected requirement revision in task.
func selectedRequirement(tx *eventstore.Tx, task types.TaskID, subject types.Name) (revision, error) {
	cur, ok, err := load[revision](tx, RequirementProjection, requirementIndex(task, subject))
	if err == nil && !ok {
		err = fmt.Errorf("graph: %s has no selected review requirement in task %s", subject, task)
	}
	return cur, err
}

// serves is why r, producer's selected result, does not serve rec ("": it
// does): a review requirement serves only in its own task, at its
// subject's selected revision there; a carry_over serves any consumer (a
// merge, a refreshed check) only by its attestation of a review that is
// still selected, current and go (verdict).
func serves(tx *eventstore.Tx, rec, producer nodeRecord, r types.Result) (string, error) {
	switch {
	case r.Kind == types.ResultKindReviewRequirement:
		req, err := requirementOf(r)
		if err != nil {
			return "", err
		}
		cur, ok, err := load[revision](tx, RequirementProjection, requirementIndex(producer.Recorded.Task, req.Subject))
		if err != nil || producer.Recorded.Task == rec.Recorded.Task && ok && (cur.Revision == req.Revision || mutation.Is(mutation.RequirementRevisionUnchecked)) {
			return "", err
		}
		return fmt.Sprintf("requirement revision %d of %s is not its selected revision in the task", req.Revision, req.Subject), nil
	case r.Kind == types.ResultKindCarryOverAttestation && r.Outcome != types.OutcomeEmpty: // a merge or a refreshed check
		return "the carry_over demands a delta or replacement review", nil
	case r.Kind == types.ResultKindCarryOverAttestation: // a merge or a refreshed check: the carried review must be go
		return verdict(tx, producer)
	}
	return "", nil
}

// verdict is why the review co, a carry_over node, attested does not let
// a merge go ("": it does): the result its claim took must still be its
// review producer's selected result, and go (empty: no blocking finding,
// 06 rule 4). A re-run review is waited for; a superseded one keeps its
// result, and its merge waits on the requirement instead.
func verdict(tx *eventstore.Tx, co nodeRecord) (string, error) {
	c, _, err := load[claim](tx, ClaimProjection, string(co.Recorded.Node.ID))
	if err != nil {
		return "", err
	}
	took := resolutions(c.Inputs)["review"].Result
	i := slices.IndexFunc(co.Recorded.Node.Inputs, func(in types.InputSpec) bool { return in.Name == "review" })
	src, ok := (*types.ResultSource)(nil), i >= 0
	if ok {
		src, ok = co.Recorded.Node.Inputs[i].Source.Variant.(*types.ResultSource)
	}
	if !ok {
		return "", fmt.Errorf("graph: carry_over %s takes no review", co.Recorded.Node.ID)
	}
	// The carried review resolves as any input does (selectedOf): completed
	// at its identity's current generation, so a reuse whose holder was
	// invalidated serves nothing even before its release.
	r, why, err := selectedOf(tx, co, src)
	switch {
	case err != nil:
		return "", err
	case why != "":
		return "the attested review serves nothing: " + why, nil
	case r.Result.ID != took:
		return fmt.Sprintf("the attested review result %s is no longer its review's", took), nil
	case r.Node != src.Producer && !holderServes(tx, r):
		return fmt.Sprintf("the attested review result %s is no longer its holder %s's selected result", took, r.Node), nil
	case r.Result.Outcome != types.OutcomeEmpty:
		return fmt.Sprintf("the attested review's verdict is no_go (%s/%s)", r.Result.Outcome, r.Result.Reason.String()), nil
	}
	return "", nil
}

// holderServes: r, a review result its reuse alias gives, is still its
// holder's: the holder is completed and selects r. A holder renewed (whose
// selection was dropped) or re-run moves its selection, and the alias's
// unreleased copy of r serves nothing, though the renewal kept the
// generation (selectedOf) and no release was recorded.
func holderServes(tx *eventstore.Tx, r types.ResultRecorded) bool {
	h, err := loadNode(tx, r.Node)
	return err == nil && h.State.State == types.NodeStateStateCompleted && h.State.Result != nil && *h.State.Result == r.Result.ID
}
