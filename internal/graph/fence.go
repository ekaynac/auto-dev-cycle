// SPDX-License-Identifier: Apache-2.0

package graph

// Fences on a live merge's inputs (13 "Merge-queue generations"; plan 15
// G7b-2a).
//
// Decisions:
//   - A merge's inputs are re-validated when its operation is intended and
//     not again on a resume, and RunEffect's re-check before the apply reads
//     a snapshot that ends before the push. So what the merge consumed must
//     not move while its operation is live (pending or uncertain): the
//     commands that would move it are refused, by builder and fold alike.
//   - A subject's requirement revision does not move (no supersession, of
//     whichever review) while a merge of the task taking a requirement of
//     that subject is live (subjectFenced).
//   - No producer a live merge depends on is invalidated or renewed
//     (inputsFenced): the producers its claim took and those its
//     carry_over's claim took, declared or recording the result (a reused
//     review's holder): the carry_over, the requirement, the refreshed
//     checks, the carried review, the candidate and the rebase.
//   - Once the operation settles (completed, or failed confirmed) nothing is
//     fenced: the effect is history.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// ErrMergeLive: a change to what merge consumed, refused while its
// operation is live.
type ErrMergeLive struct {
	Merge types.NodeID
	What  string
}

func (e ErrMergeLive) Error() string {
	return fmt.Sprintf("graph: %s is fenced while merge %s's operation is live", e.What, e.Merge)
}

// liveMerges are task's merge nodes whose operation's effect is pending or
// uncertain.
func liveMerges(tx *eventstore.Tx, task types.TaskID) ([]nodeRecord, error) {
	keys, err := tx.Keys(EffectProjection, "")
	var out []nodeRecord
	for _, k := range keys {
		rec, lerr := loadNode(tx, types.NodeID(k))
		op, _, oerr := load[types.NodeOperated](tx, EffectProjection, k)
		if err = errors.Join(err, lerr, oerr); err != nil {
			return nil, err
		}
		if rec.Recorded.Node.Kind != types.NodeKindMerge || rec.Recorded.Task != task {
			continue
		}
		e, err := tx.LoadEffect(string(op.Effect))
		if err != nil {
			return nil, err
		}
		if e.State == eventstore.EffectPending || e.State == eventstore.EffectUncertain {
			out = append(out, rec)
		}
	}
	return out, err
}

// subjectFenced: no live merge of task takes a requirement of subject.
func subjectFenced(tx *eventstore.Tx, task types.TaskID, subject types.Name) error {
	merges, err := liveMerges(tx, task)
	for _, m := range merges {
		req, lerr := loadNode(tx, producerOf(m.Recorded.Node, "requirement"))
		if err = errors.Join(err, lerr); err != nil {
			return err
		}
		if _, s, _ := rosterOf(req.Recorded.Node); s == subject {
			return ErrMergeLive{m.Recorded.Node.ID, "the requirement revision of " + string(subject)}
		}
	}
	return err
}

// dependsOn are the producers rec's claim took: each result input's
// declared producer and the node that recorded its result.
func dependsOn(tx *eventstore.Tx, rec nodeRecord) ([]types.NodeID, error) {
	c, _, err := load[claim](tx, ClaimProjection, string(rec.Recorded.Node.ID))
	out := []types.NodeID{}
	for _, in := range c.Inputs {
		if in.Result == nil || err != nil {
			continue
		}
		r, _, rerr := load[types.ResultRecorded](tx, ResultProjection, string(*in.Result))
		err = rerr
		out = append(out, producerOf(rec.Recorded.Node, string(in.Name)), r.Node)
	}
	return out, err
}

// inputsFenced: node is none of the producers a live merge of its task
// depends on, through its claim or its carry_over's.
func inputsFenced(tx *eventstore.Tx, node types.NodeID) error {
	rec, err := loadNode(tx, node)
	if err != nil {
		return err
	}
	merges, err := liveMerges(tx, rec.Recorded.Task)
	for _, m := range merges {
		deps, derr := dependsOn(tx, m)
		co, cerr := loadNode(tx, producerOf(m.Recorded.Node, "carry_over"))
		carried, rerr := dependsOn(tx, co)
		if err = errors.Join(err, derr, cerr, rerr); err != nil {
			return err
		}
		if slices.Contains(append(deps, carried...), node) {
			return ErrMergeLive{m.Recorded.Node.ID, "input " + string(node)}
		}
	}
	return err
}
