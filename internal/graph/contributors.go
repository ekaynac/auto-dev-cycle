// SPDX-License-Identifier: Apache-2.0

package graph

// Contributor families (I7, P16b): a review's roster must be disjoint from
// every model family that contributed to its task's candidate.
//
// Decisions:
//   - The contributors are the families of the task's builder attempts:
//     every lease on the unit of a build node of the task (each round,
//     repairs included), through its executor and the registry
//     (Families). A lease counts whether or not its attempt landed, so
//     the set errs toward more: a reviewer is never independent of a
//     family that could have shaped the candidate.
//   - An executor the registry does not hold counts as
//     receipt.UnknownFamily, and so does a build node of the task with a
//     recorded result and no lease on its unit (its proposer's family is
//     not known, Opus #291 M1). An unknown contributor makes no reviewer
//     independent (06 rule 3).
//   - The family is today's registry's for the lease's executor, not a
//     snapshot taken at the lease: the operator does not re-declare an
//     executor id under another family (Opus #291 L4).
//   - A review's claim (ClaimReview) and the commit of its result both
//     refuse a roster member of a contributing family (ErrNotIndependent:
//     the evaluator holds the node on reviewer_unavailable), each against
//     the contributors at its own time. A could_not_run result is
//     committed regardless: it is never selected, and it is how P22a
//     records a review that cannot run independently (Opus #291 L3). A
//     review result that holds a review receipt is also checked by
//     receipt.Independent: its recorded model families against the
//     contributors.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Contributors are the model families of task's builder attempts, sorted.
func Contributors(tx *eventstore.Tx, task types.TaskID, families Families) ([]string, error) {
	if families == nil {
		return nil, errors.New("graph: contributors are read through an executor registry")
	}
	keys, err := tx.Keys(lease.LeaseProjection, "")
	if err != nil {
		return nil, err
	}
	var out []string
	leased := map[types.NodeID]bool{}
	for _, k := range keys {
		l, _, ok, err := lease.LeaseOf(tx, types.LeaseID(k))
		if err != nil || !ok {
			return nil, errors.Join(err, fmt.Errorf("graph: lease %s is missing", k))
		}
		node, onNode := NodeOf(l.Unit)
		if !onNode {
			continue
		}
		rec, ok, err := load[nodeRecord](tx, NodeProjection, string(node))
		if err != nil {
			return nil, err
		}
		if ok && rec.Recorded.Task == task && rec.Recorded.Node.Kind == types.NodeKindBuild {
			out, leased[node] = append(out, families.Family(l.Executor)), true
		}
	}
	nodes, err := tx.Keys(NodeProjection, "")
	if err != nil {
		return nil, err
	}
	for _, k := range nodes {
		rec, err := loadNode(tx, types.NodeID(k))
		if err != nil {
			return nil, err
		}
		if rec.Recorded.Task == task && rec.Recorded.Node.Kind == types.NodeKindBuild && rec.State.Result != nil && !leased[rec.Recorded.Node.ID] {
			out = append(out, receipt.UnknownFamily) // a result no lease of ours produced
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// independentOf: rec is no review, or no member of its roster is of a
// contributing family and every contributor's family is known. It returns
// the contributors it read.
func independentOf(tx *eventstore.Tx, rec nodeRecord, families Families) ([]string, error) {
	if rec.Recorded.Node.Kind != types.NodeKindReview {
		return nil, nil
	}
	roster, _, err := rosterOf(rec.Recorded.Node)
	if err != nil {
		return nil, err
	}
	contributors, err := Contributors(tx, rec.Recorded.Task, families)
	if err != nil {
		return nil, err
	}
	unknown := slices.Contains(contributors, receipt.UnknownFamily)
	for _, m := range roster {
		switch {
		case unknown:
			return nil, ErrNotIndependent{m.Executor, "a contributor's family is unknown"}
		case slices.Contains(contributors, string(m.Family)):
			return nil, ErrNotIndependent{m.Executor, "its family " + string(m.Family) + " contributed to the candidate"}
		}
	}
	return contributors, nil
}

// reviewIndependent: rr, a review's result, is committed only by a roster
// independent of the contributors now, and a review receipt it holds is
// independent of them (receipt.Independent). A could_not_run result is
// exempt.
func reviewIndependent(tx *eventstore.Tx, rr types.ResultRecorded, env Env) error {
	rec, err := loadNode(tx, rr.Node)
	if err != nil || rec.Recorded.Node.Kind != types.NodeKindReview || rr.Result.Outcome == types.OutcomeCouldNotRun {
		return err
	}
	contributors, err := independentOf(tx, rec, env.Families)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rr.Result.Value, func(r types.Ref) bool { return r.Type == types.RefTypeReceipt }) {
		return nil // a review result need not hold a receipt; its roster was checked
	}
	ref, err := receiptRef(rr.Result)
	if err != nil {
		return err
	}
	rcpt, err := readReceipt(ref, env.Blobs)
	if err != nil || rcpt.Kind != types.ReceiptKindReview {
		return err
	}
	return receipt.Independent(rcpt, contributors)
}
