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
//   - A carry_over serves a merge only by its attestation: a demand for a
//     delta or replacement review is a structural wait.

import (
	"fmt"

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
// subject's selected revision there; a carry_over serves a merge only by
// its attestation.
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
	case r.Kind == types.ResultKindCarryOverAttestation && rec.Recorded.Node.Kind == types.NodeKindMerge && r.Outcome != types.OutcomeEmpty:
		return "the carry_over demands a delta or replacement review", nil
	}
	return "", nil
}
