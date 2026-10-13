// SPDX-License-Identifier: Apache-2.0

package authority

// input.answer (04 "Owed inputs" [PR5-1]; 13 "Successor rounds"; M1 slice
// plan P5): a ruling or data owed input is answered at its current
// revision by a responder that may answer it: an operator, or its owner,
// a supervisor whose grant covers input.answer on the owed input's project.
//   - Derive binds the owed input as the object: --expect is its version,
//     which is its revision. It is never proposed (NotProposable). An
//     unknown owed input is stale_expected_state for an operator; a
//     supervisor is refused it as action_not_granted, exactly as for an
//     owed input of a project its grants do not cover, so no refusal tells
//     it whether an id exists.
//   - The logic refuses an answer by anyone else as action_not_granted; as
//     stale_expected_state, an answer at another revision than the owed
//     input's current one and an owed input already answered in force at
//     it; a receipt owed input as an invalid scope (a ruling never
//     satisfies it).
//   - It commits approval.recorded (proposal null, policy none) whose
//     answers list is the scope's owed input, and, for a supervisor, the
//     grant it answers under. recordApproval hands an input.answer approval
//     to recordAnswer, which admits it only binding the owed input's
//     current revision, by a responder that may answer it, while no answer
//     is in force. The fold reads the scope's two binding fields as
//     recorded, never through today's schema, so a stored answer replays.
//   - Inert until P6: adcd projects the answers; input.answer stays
//     deferred (the answers capability, slice plan §1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The answer action, its scope schema and the reason its approvals record.
const (
	InputAnswerAction = "input.answer"
	InputAnswerScope  = "api/scope/input-answer"
	answerReason      = "input.answer answers the owed input at the revision it binds"
)

// InputAnswerHandler derives input.answer.
type InputAnswerHandler struct{}

// Derive checks the scope and binds the owed input it answers, in its
// project.
func (InputAnswerHandler) Derive(tx *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors answer", by.ID, by.Kind)
	}
	in, err := answerInputOf(input)
	if err != nil {
		return Derived{}, err
	}
	o, ok, err := owedOf(tx, in.OwedInput)
	switch {
	case err != nil:
	case !ok && by.Kind == types.ActorKindSupervisor: // as Decide refuses a project its grants do not cover
		err = refusal(vocab.ReasonActionNotGranted, "no grant in force covers %s for actor %s", InputAnswerAction, by.ID)
	case !ok:
		err = fmt.Errorf("authority: %w: no owed input %s", eventstore.ErrStaleExpectedState, in.OwedInput)
	}
	return Derived{Object: string(in.OwedInput), ObjectType: OwedObjectType, Project: &o.Project, Record: OwedRecordOf(in.OwedInput),
		Version: func(tx *eventstore.Tx) (int64, error) {
			o, _, err := owedOf(tx, in.OwedInput)
			return o.Revision, err
		}, NotProposable: "an answer binds its owed input's revision when it is given, so it is never proposed"}, err
}

// answerInputOf is the scope, refused as ErrInvalidScope unless its schema
// admits it.
func answerInputOf(raw json.RawMessage) (types.InputAnswerScope, error) {
	in, err := contract.Decode[types.InputAnswerScope](InputAnswerScope, raw)
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, InputAnswerAction, err)
	}
	return in, nil
}

// InputAnswer is the logic for c, a command the Authorizer allowed: the
// approval answering the owed input at the revision the scope binds.
func InputAnswer(c eventstore.Command) eventstore.Decide {
	return eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		in, err := answerInputOf(c.Input)
		by, err2 := liveActor(tx, types.ActorID(c.Actor))
		if err = errors.Join(err, err2); err == nil && (c.Action != InputAnswerAction || !bound || id != string(in.OwedInput)) {
			err = fmt.Errorf("authority: %s is no derived %s", c.Action, InputAnswerAction)
		}
		if err != nil {
			return nil, nil, err
		}
		o, err := answerable(tx, in.OwedInput, in.OwedRevision, by)
		if err != nil {
			return nil, nil, err
		}
		grant, err := answerGrant(tx, by, o.Project)
		if err != nil {
			return nil, nil, err
		}
		apr, err := ids.New("apr")
		a := AnswerApprovalOf(o, by, grant, c.Input, types.ApprovalID(apr), types.Time(tx.Now().Format(time.RFC3339Nano)))
		evs, err2 := ApprovalEvent(a)
		res, err3 := json.Marshal(map[string]any{"approval": apr, "owed_input": o.ID, "owed_revision": o.Revision})
		return evs, res, errors.Join(err, err2, err3)
	})
}

// answerGrant is the grant a supervisor answers under in project, as
// Decide chooses it (nil for an operator); none in force is
// action_not_granted.
func answerGrant(tx *eventstore.Tx, by types.Actor, project types.ProjectID) (*types.GrantID, error) {
	if by.Kind == types.ActorKindOperator {
		return nil, nil
	}
	grants, revoked, err := GrantsOf(tx, by.ID)
	if err != nil {
		return nil, err
	}
	d := supervisor(Request{Actor: by, Action: InputAnswerAction, Project: &project, Grants: grants, Revoked: revoked, Now: tx.Now()}, nil, None)
	if d.Disposition != Allow || d.Grant == nil {
		return nil, refusal(vocab.ReasonActionNotGranted, "%s", d.Detail)
	}
	return d.Grant, nil
}

// answerable is owed, if by may record an answer at revision now: by is an
// operator or owed's owner (04: the responder may answer this kind for
// this project), owed is of kind ruling or data, at exactly that revision,
// with no answer in force.
func answerable(tx *eventstore.Tx, owed types.OwedInputID, revision int64, by types.Actor) (OwedRecord, error) {
	o, ok, err := owedOf(tx, owed)
	if err != nil {
		return o, err
	}
	var held *OwedAnswer
	if ok {
		held, err = answerInForce(tx, o)
	}
	switch {
	case err != nil:
		return o, err
	case !ok:
		return o, fmt.Errorf("authority: %w: no owed input %s", eventstore.ErrStaleExpectedState, owed)
	case by.Kind != types.ActorKindOperator && by.ID != o.Owner:
		return o, refusal(vocab.ReasonActionNotGranted, "actor %s is neither an operator nor the owner of %s", by.ID, o.ID)
	case o.Kind == types.OwedInputKindReceipt:
		return o, fmt.Errorf("%w: %s is a receipt owed input, which only an applicable receipt satisfies", ErrInvalidScope, o.ID)
	case revision != o.Revision:
		return o, fmt.Errorf("authority: %w: %s is at revision %d, not %d", eventstore.ErrStaleExpectedState, o.ID, o.Revision, revision)
	case held != nil:
		return o, fmt.Errorf("authority: %w: %s is answered at revision %d by %s", eventstore.ErrStaleExpectedState, o.ID, o.Revision, held.Approval)
	}
	return o, nil
}

// AnswerApprovalOf is by's approval answering o at its revision, under
// grant (a supervisor's; nil for an operator), scope the input.answer
// scope.
func AnswerApprovalOf(o OwedRecord, by types.Actor, grant *types.GrantID, scope json.RawMessage, id types.ApprovalID, at types.Time) types.Approval {
	rev, project, question := o.Revision, o.Project, o.Question
	return types.Approval{ID: id, Actor: by.ID, ActorKind: types.ApprovalActorKind(by.Kind), Action: InputAnswerAction,
		ChangeClass: []types.ChangeClass{}, Project: &project, Object: types.ObjectRef{Type: OwedObjectType, ID: string(o.ID), Version: &rev},
		ExpectedState: types.ExpectedState{Version: &rev}, Scope: scope, Grant: grant,
		Confirmation: types.ApprovalConfirmation{Policy: types.ApprovalConfirmationPolicyNone, SatisfiedBy: []types.EventID{}},
		Answers:      []types.OwedInputID{o.ID}, Supersedes: []types.ApprovalID{}, Question: &question, Reason: answerReason, At: at}
}

// recordAnswer admits a, an input.answer approval with no proposal, by a
// live operator, or a live supervisor under its own grant covering
// input.answer on the project, in force at the answer (answersUnder), in a
// command, under a fresh approval id, only as the one answer in force to
// its scope's owed input, by a responder that may answer it, of kind
// ruling or data, at that owed input's current revision, in its project.
// It checks the binding fields only, like recordTaskApproval, and the
// revision again, apart from the logic's check (answerable).
func recordAnswer(tx *eventstore.Tx, ev eventstore.Event, a types.Approval) error {
	var in struct {
		Owed     *types.OwedInputID `json:"owed_input"`
		Revision *int64             `json:"owed_revision"`
	}
	if err := json.Unmarshal(a.Scope, &in); err != nil || in.Owed == nil || in.Revision == nil {
		return errors.Join(err, errors.New("an answer's scope names its owed input and revision"))
	}
	by, err := liveActor(tx, a.Actor)
	if err != nil {
		return err
	}
	o, err := answerable(tx, *in.Owed, *in.Revision, by)
	if err != nil {
		return err
	}
	id := "approval_id/" + string(a.ID) // shared with proposal approvals; approvalOf finds no proposal under it
	if _, dup, err := tx.Get(ProposalProjection, id); err != nil || dup {
		return errors.Join(err, fmt.Errorf("approval %s is already recorded", a.ID))
	}
	supervised := by.Kind == types.ActorKindSupervisor
	if supervised && a.Grant != nil {
		supervised, err = answersUnder(tx, by.ID, *a.Grant, o.Project, ev.At)
	}
	x, ob := a.ExpectedState, a.Object
	if err != nil || by.Kind != types.ActorKindOperator && !supervised || a.ActorKind != types.ApprovalActorKind(by.Kind) || (a.Grant != nil) != supervised ||
		ev.Command == nil || a.At != types.Time(ev.At) || ev.Project == nil || *ev.Project != string(o.Project) || a.Project == nil || *a.Project != o.Project ||
		*in.Revision != o.Revision || ob.Type != OwedObjectType || ob.ID != string(o.ID) || ob.Version == nil || *ob.Version != o.Revision ||
		x.Version == nil || *x.Version != o.Revision ||
		!slices.Equal(a.Answers, []types.OwedInputID{o.ID}) || a.Confirmation.Policy != types.ApprovalConfirmationPolicyNone {
		return errors.Join(err, fmt.Errorf("the approval does not answer %s at revision %d, by a live operator or a supervisor under its grant", o.ID, o.Revision))
	}
	o.Answer = &OwedAnswer{Approval: a.ID, Actor: a.Actor, Revision: o.Revision, At: a.At}
	return errors.Join(putJSON(tx, ProposalProjection, id, o.ID), putJSON(tx, OwedProjection, string(o.ID), o),
		tx.Put(OwedProjection, "approval/"+string(a.ID), ev.Payload))
}

// answersUnder: grant is actor's, created and never revoked, in force at
// at (the answer's event time, never the clock), covering input.answer on
// project, as recorded.
func answersUnder(tx *eventstore.Tx, actor types.ActorID, grant types.GrantID, project types.ProjectID, at string) (bool, error) {
	e, ok, err := loadJSON[types.GrantEvent](tx, GrantProjection, grantKey(actor, grant))
	when, terr := time.Parse(time.RFC3339Nano, at)
	if err = errors.Join(err, terr); err != nil || !ok {
		return false, err
	}
	g := e.Grant
	return e.Transition == types.GrantEventTransitionGrantCreated && e.RevokedAt == nil && g.Actor == actor && g.ID == grant &&
		inForce(g, when) && covers(g, &project) && slices.Contains(g.Actions, types.ActionKind(InputAnswerAction)), nil
}
