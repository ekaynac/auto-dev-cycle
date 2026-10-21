// SPDX-License-Identifier: Apache-2.0

package graph

// The answer folds (M1 P6; 13 [G4], "Successor rounds", "Moved
// questions"; 04 "Owed inputs").
//
// Decisions:
//   - A recorded answer is eligible for a node by one rule, whoever takes
//     it: it is the answer the authority holds in force now, for an owed
//     input the node's lineage asks, at that owed input's current revision,
//     and no question a supersession in the lineage replaced is still owed
//     (inForce, over answers). The builders check it (Claim through the
//     evaluator, Reuse, Commit), and so does the fold, from the log, for the
//     events that take an answer: node.claimed, node.reused and
//     result.recorded (a commit). An effect node's answers are re-checked at
//     its intent by graph.Operate (I1's effect row), not here.
//   - A successor's new answers are to owed inputs its lineage asks at
//     their current revisions (Successor's ErrNotOwed); a supersession's
//     moved or removed answers are to questions its lineage no longer asks
//     (movedQuestion, shared with the builder); an awaiting_input wait is
//     raised only on an answer the node declares, cleared on its signal
//     only once that answer is in force (inForce's rule), and retired as
//     revised only once its owed input has moved past the revision.
//   - The fold reads the authority in its transaction, at the event's
//     position in the log, so a rebuild reaches the same verdict. Only
//     Projector, which adcd composes with authority.Answers, runs these
//     checks; ProjectM0 is M0's fold, which reads no authority, kept for the
//     M0 fixtures and their owed-answer fake.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Projector is the graph fold that also checks, through Answers, the
// recorded answers each event takes (M1 P6). It fails closed: with no
// authority it admits no graph event.
type Projector struct {
	Answers Authority
	// Kinds, when set, are the node kinds a node.recorded may hold
	// (CheckSurface's allowlist, I15 admission: adcd's); nil, every kind.
	Kinds []types.NodeKind
}

// Project is the eventstore.Hooks.Project fold for the graph projections,
// with the answer checks.
func (p Projector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	if p.Answers == nil && slices.ContainsFunc(evs, func(ev eventstore.Event) bool { return slices.Contains(kinds, ev.Kind) }) {
		return errors.New("graph: the fold needs the authority that holds the recorded answers")
	}
	return project(tx, evs, p.Answers, p.Kinds)
}

// eligible: v takes only answers eligible for its node, under auth.
func eligible(tx *eventstore.Tx, ev eventstore.Event, v types.GraphEventVariant, auth Authority) error {
	switch p := v.(type) {
	case *types.NodeRecorded:
		if p.Predecessor != nil && !mutation.Is(mutation.RecordedFoldSkipsOwed) {
			return successorOwed(tx, p, auth)
		}
	case *types.NodeClaimed:
		if !mutation.Is(mutation.ClaimFoldSkipsAnswers) {
			return takesInForce(tx, ev, p.Node, p.Inputs, auth)
		}
	case *types.NodeReused:
		if !mutation.Is(mutation.ReuseFoldSkipsAnswers) {
			return takesInForce(tx, ev, p.Node, p.Inputs, auth)
		}
	case *types.ResultRecorded:
		c, ok, err := load[claim](tx, ClaimProjection, string(p.Node))
		rec, rerr := nodeOf(tx, ev, p.Node)
		if err = errors.Join(err, rerr); err != nil || !ok || rec.Recorded.Node.Effect != nil || mutation.Is(mutation.ResultFoldSkipsAnswers) {
			return err // unclaimed: the result fold refuses it; an effect's answers are re-checked at its intent
		}
		return inForce(tx, rec, resolutions(c.Inputs), auth)
	case *types.ReviewSuperseded:
		if !mutation.Is(mutation.SupersessionFoldSkipsMoves) {
			return movesHold(tx, ev, p, auth)
		}
	case *types.HoldRaised:
		if !mutation.Is(mutation.HoldRaiseSkipsAnswer) {
			return answerWaitRaised(tx, ev, p)
		}
	case *types.HoldCleared:
		if !mutation.Is(mutation.HoldFoldSkipsAnswers) {
			return answerWaitEnds(tx, ev, p, auth)
		}
	}
	return nil
}

// takesInForce: node's inputs, as the event fixes them, take only answers in
// force (inForce).
func takesInForce(tx *eventstore.Tx, ev eventstore.Event, node types.NodeID, inputs []types.ClaimedInput, auth Authority) error {
	rec, err := nodeOf(tx, ev, node)
	if err != nil {
		return err
	}
	return inForce(tx, rec, resolutions(inputs), auth)
}

// inForce: each of rec's answer inputs resolves to the answer auth holds
// in force now, for a question rec's lineage asks at its current revision,
// and no question a supersession in the lineage replaced is still owed;
// else authority_revoked, as at commit.
func inForce(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved, auth Authority) error {
	answered, stale, err := answers(tx, rec, auth)
	if err != nil {
		return err
	}
	for _, in := range rec.Recorded.Node.Inputs {
		got, want := resolved[string(in.Name)], answered[string(in.Name)]
		if _, ok := in.Source.Variant.(*types.AnswerSource); ok && (want.Approval == "" || got != want) {
			return ErrRevalidated{Input: in.Name, Reason: vocab.ReasonAuthorityRevoked, Detail: "the answer is not the one in force for a question its lineage asks"}
		}
	}
	if stale { // a displaced question the node declares no answer to
		return ErrRevalidated{Reason: vocab.ReasonAuthorityRevoked, Detail: "a question a supersession replaced is still owed"}
	}
	return nil
}

// successorOwed: each answer p's node declares anew (not its
// predecessor's, unchanged) is to an owed input its lineage asks, at that
// owed input's current revision (13 "Successor rounds").
func successorOwed(tx *eventstore.Tx, p *types.NodeRecorded, auth Authority) error {
	pred, err := loadNode(tx, *p.Predecessor)
	owed, oerr := lineageOwed(tx, p.Predecessor, auth)
	if err = errors.Join(err, oerr); err != nil {
		return err
	}
	for i, in := range p.Node.Inputs {
		ask, ok := in.Source.Variant.(*types.AnswerSource)
		inherited := i < len(pred.Recorded.Node.Inputs) && reflect.DeepEqual(pred.Recorded.Node.Inputs[i], in)
		if ok && !inherited && !owedIn(owed, ask) {
			return ErrNotOwed{ask.OwedInput}
		}
	}
	return nil
}

// movedQuestion: from's question, an answer input named name, has moved
// out of the lineage whose questions are owed, so its answer may be
// removed (to nil) or replaced by an answer to to, which the lineage asks
// at its current revision. The supersession builder and fold share it.
func movedQuestion(owed []types.AnswerSource, name types.Name, from, to *types.AnswerSource) error {
	switch {
	case asks(owed, from.OwedInput):
		return fmt.Errorf("graph: %s's question has not moved: its answer is not replaced or removed", name)
	case to != nil && !owedIn(owed, to): // the old owed input is no longer asked, so it is never to
		return ErrNotOwed{to.OwedInput}
	}
	return nil
}

// movesHold: every answer input of p's predecessor that its replacement
// replaces or removes is of a question that moved (movedQuestion).
func movesHold(tx *eventstore.Tx, ev eventstore.Event, p *types.ReviewSuperseded, auth Authority) error {
	rec, err := nodeOf(tx, ev, p.Predecessor)
	if err != nil {
		return err
	}
	var owed []types.AnswerSource
	for _, in := range rec.Recorded.Node.Inputs {
		from, ok := in.Source.Variant.(*types.AnswerSource)
		i := slices.IndexFunc(p.Replacement.Inputs, func(m types.InputSpec) bool { return m.Name == in.Name })
		if !ok || i >= 0 && reflect.DeepEqual(p.Replacement.Inputs[i], in) {
			continue
		}
		var to *types.AnswerSource
		if i >= 0 {
			if to, ok = p.Replacement.Inputs[i].Source.Variant.(*types.AnswerSource); !ok {
				continue // not an answer: the shape check refuses it (replaces)
			}
		}
		if owed == nil {
			if owed, err = lineageOwed(tx, rec.Recorded.Predecessor, auth); err != nil {
				return err
			}
		}
		if err := movedQuestion(owed, in.Name, from, to); err != nil {
			return err
		}
	}
	return nil
}

// answerWaitEnds: an awaiting_input wait ends only as its answer allows:
// cleared on its signal once the authority gives the answer, or retired as
// revised once its owed input has moved past the signal's revision.
func answerWaitEnds(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldCleared, auth Authority) error {
	rec, err := nodeOf(tx, ev, p.Node)
	i := slices.IndexFunc(rec.State.Holds, func(h types.NodeHold) bool { return h.ID == p.Hold })
	if err != nil || i < 0 || rec.State.Holds[i].Hold != "awaiting_input" {
		return err // not an answer wait: the hold fold decides
	}
	signal := rec.State.Holds[i].SignalInstance
	in, ask, err := declaredAnswer(rec, signal)
	if err != nil {
		return err
	}
	switch c := p.Cause.Variant.(type) {
	case *types.Signalled:
		// The rule inForce applies to this one input, as the evaluator clears
		// a wait (evaluate.go:clear): its answer is the one in force, for a
		// question the lineage asks at its current revision, not displaced.
		answered, _, err := answers(tx, rec, auth)
		if err != nil || answered[string(in.Name)].Approval == "" {
			return errors.Join(err, fmt.Errorf("the wait on %s clears only once its answer is given", signal))
		}
	case *types.Retired:
		if c.Reason != types.RetiredReasonRevised {
			return nil // the hold fold refuses any other retirement of an answer wait
		}
		owed, err := lineageOwed(tx, rec.Recorded.Predecessor, auth)
		if err != nil || !asks(owed, ask.OwedInput) || owedIn(owed, ask) {
			return errors.Join(err, fmt.Errorf("the wait on %s is retired as revised only once its owed input moved past it", signal))
		}
	}
	return nil
}

// declaredAnswer is rec's answer input whose awaiting_input signal is
// signal; a wait on no declared answer is refused, raised or cleared.
func declaredAnswer(rec nodeRecord, signal string) (types.InputSpec, *types.AnswerSource, error) {
	for _, in := range rec.Recorded.Node.Inputs {
		if ask, ok := in.Source.Variant.(*types.AnswerSource); ok && awaitSignal(ask) == signal {
			return in, ask, nil
		}
	}
	return types.InputSpec{}, nil, fmt.Errorf("the wait on %s is on no answer the node declares", signal)
}

// answerWaitRaised: an awaiting_input wait is raised only on an answer the
// node declares (evaluate.go:await), so its answer can always end it.
func answerWaitRaised(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldRaised) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || p.Hold.Hold != "awaiting_input" {
		return err // the hold fold decides
	}
	_, _, err = declaredAnswer(rec, p.Hold.SignalInstance)
	return err
}
