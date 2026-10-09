// SPDX-License-Identifier: Apache-2.0

package graph

// Successor rounds and the controller context (13 "Successor rounds" [G10],
// "Evaluation"; 10 "controller-supplied context"; plan 15 G5).
//
// Decisions:
//   - New information never changes a node: it makes one successor, round
//     n+1 of the same kind, definition version, params, requirements and
//     effect, whose inputs are its predecessor's followed by the new ones (a
//     repair's findings and candidate tree, an answer, demanded evidence).
//     An inherited answer may be replaced by a newer revision of the same
//     owed input. The node.recorded fold checks all of this, refuses a fork
//     and refuses an answer input on a first round.
//   - An answer is accepted only for an owed input this node's lineage (an
//     ancestor, or a review one replaced) asked for, at that owed input's
//     current revision: by
//     Successor for what it adds (ErrNotOwed), and by the evaluator and the
//     commit for every answer input, inherited or recorded directly.
//   - An answer input at a revision its owed input has moved past is
//     unanswered for its node: the evaluator leaves the node waiting (a
//     structural wait, never an error for the command) and the commit
//     refuses it as authority_revoked. The lineage moves on through a
//     successor that answers the current revision.
//   - Holds move only to a replacement (G6b-2): a successor is refused while its
//     predecessor holds anything but an awaiting_input wait on an answer
//     input it inherits: one the authority has given is cleared on its
//     signal, one the successor moves to a newer revision is retired as
//     revised. Both events precede node.recorded in the same command, and
//     the node.recorded fold refuses a successor of a node that holds
//     anything. An inherited answer not given yet refuses the round: it
//     would raise a second hold.
//   - The controller context of a node is rendered only from its claimed
//     inputs, in declared order: a result input gives its result's refs, an
//     answer input the authority's Ref to the exact answer the claim fixed
//     (approval, owed input, revision), for an owed input its lineage asked
//     for, at the current revision. Nothing else, an answer to an owed
//     input the node did not declare or its lineage did not ask for
//     included, reaches it, however the claim was made; a manifest.intent
//     lists exactly these refs (10). A result's artifacts are rendered by
//     their content class (content.go): a restricted one is omitted.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// ErrNotOwed: an answer to an owed input the node's lineage did not ask for.
type ErrNotOwed struct{ Owed types.OwedInputID }

func (e ErrNotOwed) Error() string {
	return "graph: no ancestor owes " + string(e.Owed) + " at that revision"
}

// lineageOwed are the owed inputs pred and its ancestors asked for, and
// those the reviews each of them replaced asked for (supersede.go).
func lineageOwed(tx *eventstore.Tx, pred *types.NodeID, auth Authority) ([]types.AnswerSource, error) {
	var out []types.AnswerSource
	for p := pred; p != nil; {
		anc, err := loadNode(tx, *p)
		for q := p; err == nil && q != nil; {
			var asker nodeRecord
			var owed []types.AnswerSource
			if asker, err = loadNode(tx, *q); err == nil {
				owed, err = auth.Owed(tx, asker.Recorded)
			}
			out, q = append(out, owed...), asker.Replaces
			if mutation.Is(mutation.LineageIgnoresSupersession) {
				q = nil
			}
		}
		if err != nil {
			return nil, err
		}
		p = anc.Recorded.Predecessor
	}
	return out, nil
}

// asks: owed holds owed input id, at any revision.
func asks(owed []types.AnswerSource, id types.OwedInputID) bool {
	return slices.ContainsFunc(owed, func(o types.AnswerSource) bool { return o.OwedInput == id })
}

// owedIn: ask is among owed, at its revision.
func owedIn(owed []types.AnswerSource, ask *types.AnswerSource) bool {
	return slices.ContainsFunc(owed, func(o types.AnswerSource) bool { return o.OwedInput == ask.OwedInput && o.Revision == ask.Revision })
}

// newerAnswer: in answers the owed input of p at a newer revision.
func newerAnswer(p, in types.InputSpec) bool {
	a, ok := p.Source.Variant.(*types.AnswerSource)
	b, ok2 := in.Source.Variant.(*types.AnswerSource)
	return ok && ok2 && p.Name == in.Name && a.OwedInput == b.OwedInput && b.Revision > a.Revision
}

// Successor is the events recording the round n+1 successor of pred, with
// pred's inputs followed by added: first those releasing pred's waits on
// the answers it inherits (cleared on their signal, or retired as revised
// for one it moves to a newer revision), then its node.recorded, last.
func Successor(tx *eventstore.Tx, pred types.NodeID, added []types.InputSpec, auth Authority) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, pred)
	id, ierr := ids.New("nod")
	owed, oerr := lineageOwed(tx, &pred, auth)
	if err = errors.Join(err, ierr, oerr, superseded(rec)); err != nil {
		return nil, err
	}
	n := rec.Recorded.Node
	n.ID, n.Round, n.Inputs = types.NodeID(id), n.Round+1, slices.Clone(n.Inputs)
	for _, in := range added {
		i := slices.IndexFunc(n.Inputs, func(p types.InputSpec) bool { return p.Name == in.Name })
		switch {
		case i >= len(rec.Recorded.Node.Inputs) || i >= 0 && !newerAnswer(n.Inputs[i], in):
			return nil, fmt.Errorf("graph: %s's successor declares %s twice", pred, in.Name)
		case i >= 0:
			n.Inputs[i] = in
		default:
			n.Inputs = append(n.Inputs, in)
		}
		if ask, ok := in.Source.Variant.(*types.AnswerSource); ok && !owedIn(owed, ask) && !mutation.Is(mutation.SuccessorTakesAnyAnswer) {
			return nil, ErrNotOwed{ask.OwedInput}
		}
	}
	next := types.NodeRecorded{Task: rec.Recorded.Task, Project: rec.Recorded.Project, Node: n, Predecessor: &pred}
	answered := func(in types.InputSpec) (bool, error) {
		ask := in.Source.Variant.(*types.AnswerSource)
		_, ok, err := auth.Answer(tx, next, ask.OwedInput, ask.Revision)
		return ok, err
	}
	for _, in := range n.Inputs[:len(rec.Recorded.Node.Inputs)] {
		if _, ask := in.Source.Variant.(*types.AnswerSource); ask {
			if ok, err := answered(in); err != nil || !ok {
				return nil, errors.Join(err, fmt.Errorf("graph: %s's answer %s is not given yet", pred, in.Name))
			}
		}
	}
	var evs []eventstore.Event
	for _, h := range rec.State.Holds {
		// pred waits only on answers it declares, each given in n (above).
		i := slices.IndexFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool {
			ask, ok := in.Source.Variant.(*types.AnswerSource)
			return ok && h.Hold == "awaiting_input" && h.SignalInstance == awaitSignal(ask)
		})
		if i < 0 {
			return nil, fmt.Errorf("graph: %s holds %s (%s): a hold moves to a successor only with G6b", pred, h.ID, h.Hold)
		}
		var cause types.HoldClearedCauseVariant = &types.Signalled{Type: "signal", SignalInstance: h.SignalInstance}
		if newerAnswer(rec.Recorded.Node.Inputs[i], n.Inputs[i]) {
			cause = &types.Retired{Type: "retired", Reason: types.RetiredReasonRevised}
		}
		release, err := cleared(tx, pred, h.ID, cause)
		if err != nil {
			return nil, err
		}
		evs = append(evs, release...)
	}
	recorded, err := record(next) // a review keeps its roster, checked again when claimed
	if err != nil {
		return nil, err
	}
	return append(evs, recorded...), nil
}

// Context is node's controller context under env: the refs of its claimed
// inputs, in declared order, a restricted artifact omitted. An answer its
// lineage did not ask for is ErrNotOwed, one at a revision its owed input
// has moved past ErrRevalidated (authority_revoked).
func Context(tx *eventstore.Tx, node types.NodeID, env Env) (Rendered, error) {
	out, err := renderContext(tx, node, env)
	if err != nil {
		return Rendered{}, err
	}
	return out, nil
}

func renderContext(tx *eventstore.Tx, node types.NodeID, env Env) (Rendered, error) {
	out, auth := Rendered{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{}}, env.Authority
	rec, err := loadNode(tx, node)
	c, ok, cerr := load[claim](tx, ClaimProjection, string(node))
	if err = errors.Join(err, cerr); err != nil || !ok {
		return out, errors.Join(err, fmt.Errorf("graph: %s is not claimed: its inputs are not fixed", node))
	}
	owed, err := lineageOwed(tx, rec.Recorded.Predecessor, auth)
	if err != nil {
		return out, err
	}
	if name, still, err := displaced(tx, rec, owed); err != nil || still {
		return out, errors.Join(err, ErrRevalidated{Input: name, Reason: vocab.ReasonAuthorityRevoked, Detail: "a question a supersession replaced is still owed"})
	}
	for i, in := range c.Inputs {
		switch {
		case in.Result != nil:
			r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(*in.Result))
			if err != nil || !ok {
				return out, errors.Join(err, fmt.Errorf("graph: %s's input %s names no recorded result", node, in.Name))
			}
			if err := classed(tx, r, env, &out); err != nil {
				return out, err
			}
		case in.Approval != nil && in.Revision != nil:
			ask := rec.Recorded.Node.Inputs[i].Source.Variant.(*types.AnswerSource)
			if !mutation.Is(mutation.AnswerLineageUnchecked) {
				if !asks(owed, ask.OwedInput) {
					return out, ErrNotOwed{ask.OwedInput}
				}
				if !owedIn(owed, &types.AnswerSource{OwedInput: ask.OwedInput, Revision: *in.Revision}) {
					return out, ErrRevalidated{Input: in.Name, Reason: vocab.ReasonAuthorityRevoked, Detail: "the answer is not at its owed input's current revision"}
				}
			}
			ref, err := auth.Approval(tx, rec.Recorded, ask.OwedInput, *in.Revision, *in.Approval)
			if err != nil {
				return out, err
			}
			out.Included = append(out.Included, ref)
		default:
			return out, fmt.Errorf("graph: %s's input %s resolved to nothing", node, in.Name)
		}
	}
	return out, nil
}
