// SPDX-License-Identifier: Apache-2.0

package authority

// Owed inputs and the graph's authority over their answers (04 "Owed
// inputs"; 13 "Successor rounds"; M1 slice plan P5):
//   - An owed input is recorded by the node that asked (owed_input.recorded,
//     records/owed-input), in that node's project, waiting on a live
//     operator or supervisor. A later record of the same id is its next
//     revision, of the same project, node and kind; it drops the answer to
//     the revision before, which no longer answers it.
//   - An answer is an input.answer approval (answer.go), at most one in
//     force per owed input: by a live actor, at its current revision. An
//     answering actor's revocation withdraws it, and the owed input can be
//     answered again at that revision.
//   - Answers is the real graph.Authority over these records, read in the
//     transaction it is given. A receipt owed input is never an answer
//     source: its node declares the evidence node's result instead (E1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The owed input event kind, the projection holding each owed input
// (OwedRecord by id, the asker/<node>/<id> index and each answering
// approval as recorded under approval/<id>) and an owed input's object
// type.
const (
	OwedInputRecorded = "owed_input.recorded/v1"
	OwedProjection    = "owed_inputs"
	OwedObjectType    = "owed_input"
)

// OwedRecord is an owed input at its current revision, and its answer at
// that revision (nil: none recorded).
type OwedRecord struct {
	types.OwedInput
	Answer *OwedAnswer `json:"answer"`
}

// OwedAnswer is the approval answering an owed input at a revision.
type OwedAnswer struct {
	Approval types.ApprovalID `json:"approval"`
	Actor    types.ActorID    `json:"actor"`
	Revision int64            `json:"revision"`
	At       types.Time       `json:"at"`
}

// OwedKinds registers the owed input event kind.
func OwedKinds() eventstore.Kinds {
	return eventstore.Kinds{OwedInputRecorded: {Schema: "records/owed-input"}}
}

// RecordOwed is the event recording o: a new owed input at revision 1, or
// the next revision of one.
func RecordOwed(o types.OwedInput) ([]eventstore.Event, error) {
	return event(OwedInputRecorded, string(o.ID), &o.Project, o)
}

// OwedRecordOf is owed input id's record, which --expect is checked
// against; its version is its revision (InputAnswerHandler.Derive).
func OwedRecordOf(id types.OwedInputID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(OwedProjection, string(id)) }
}

// OwedProjector folds owed_input.recorded; it runs after the identity
// projector. The asking node is the graph's, recorded by an earlier event;
// the proposals projector folds the answers (recordAnswer).
type OwedProjector struct{}

// Project is the eventstore.Hooks.Project fold for owed inputs.
func (OwedProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != OwedInputRecorded {
			continue
		}
		if err := recordOwed(tx, ev); err != nil {
			return fmt.Errorf("authority: event %s: owed input: %w", ev.ID, err)
		}
	}
	return nil
}

// recordOwed admits o under its id and project, asked by a node of that
// project, waiting on a live operator or supervisor, at revision 1 or the
// next revision of the same project, node and kind.
func recordOwed(tx *eventstore.Tx, ev eventstore.Event) error {
	var o types.OwedInput
	if err := json.Unmarshal(ev.Payload, &o); err != nil {
		return err
	}
	if ev.Subject != string(o.ID) || ev.Project == nil || *ev.Project != string(o.Project) {
		return errors.New("an owed input's event is under its id, in its project")
	}
	project, err := graph.ProjectOf(tx, o.RequestedBy)
	if err != nil || project != o.Project {
		return errors.Join(err, fmt.Errorf("node %s of project %s did not ask for it", o.RequestedBy, o.Project))
	}
	owner, err := liveActor(tx, o.Owner)
	if err != nil || owner.Kind != types.ActorKindOperator && owner.Kind != types.ActorKindSupervisor {
		return errors.Join(err, fmt.Errorf("owner %s is no live operator or supervisor", o.Owner))
	}
	was, ok, err := loadJSON[OwedRecord](tx, OwedProjection, string(o.ID))
	switch {
	case err != nil:
		return err
	case !ok && o.Revision != 1:
		return fmt.Errorf("a new owed input is at revision 1, not %d", o.Revision)
	case ok && (o.Revision != was.Revision+1 || o.Project != was.Project || o.RequestedBy != was.RequestedBy || o.Kind != was.Kind):
		return fmt.Errorf("revision %d of %s is not the next one of its project, node and kind", o.Revision, o.ID)
	}
	return errors.Join(putJSON(tx, OwedProjection, string(o.ID), OwedRecord{OwedInput: o}),
		putJSON(tx, OwedProjection, "asker/"+string(o.RequestedBy)+"/"+string(o.ID), o.ID))
}

// owedOf is owed input id's record.
func owedOf(tx *eventstore.Tx, id types.OwedInputID) (OwedRecord, bool, error) {
	return loadJSON[OwedRecord](tx, OwedProjection, string(id))
}

// answerInForce is o's answer at its current revision by a live actor, if any.
func answerInForce(tx *eventstore.Tx, o OwedRecord) (*OwedAnswer, error) {
	if o.Answer == nil || o.Answer.Revision != o.Revision {
		return nil, nil
	}
	by, ok, err := loadJSON[types.Actor](tx, identity.ActorProjection, string(o.Answer.Actor))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("authority: %s's answer is by no actor", o.ID)) // damage, not a revocation
	}
	if by.RevokedAt != nil {
		return nil, nil
	}
	return o.Answer, nil
}

// Answers is the graph's authority over owed inputs (graph.Authority):
// what it reads is this package's records, in the transaction it is given.
type Answers struct{}

// declared is owed's record when rec's node declares an answer to it and
// it is of rec's project; ok false otherwise.
func declared(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (OwedRecord, bool, error) {
	o, ok, err := owedOf(tx, owed)
	asks := slices.ContainsFunc(rec.Node.Inputs, func(in types.InputSpec) bool {
		a, ok := in.Source.Variant.(*types.AnswerSource)
		return ok && a.OwedInput == owed
	})
	return o, err == nil && ok && asks && o.Project == rec.Project && o.Kind != types.OwedInputKindReceipt, err
}

// Answer is the approval in force answering owed at revision, its current
// one, for rec's node.
func (Answers) Answer(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64) (types.ApprovalID, bool, error) {
	o, ok, err := declared(tx, rec, owed)
	if err != nil || !ok || o.Revision != revision {
		return "", false, err
	}
	a, err := answerInForce(tx, o)
	if err != nil || a == nil {
		return "", false, err
	}
	return a.Approval, true, nil
}

// Owner is the actor owed's wait is on.
func (Answers) Owner(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (types.ActorID, error) {
	o, ok, err := declared(tx, rec, owed)
	if err == nil && !ok {
		err = fmt.Errorf("authority: node %s declares no owed input %s of its project", rec.Node.ID, owed)
	}
	return o.Owner, err
}

// Owed are the ruling and data owed inputs rec's node asked for, at their
// current revisions.
func (Answers) Owed(tx *eventstore.Tx, rec types.NodeRecorded) ([]types.AnswerSource, error) {
	prefix := "asker/" + string(rec.Node.ID) + "/"
	keys, err := tx.Keys(OwedProjection, prefix)
	out := []types.AnswerSource{}
	for _, k := range keys {
		o, ok, lerr := owedOf(tx, types.OwedInputID(k[len(prefix):]))
		if err = errors.Join(err, lerr); err != nil || !ok {
			return nil, errors.Join(err, fmt.Errorf("authority: the asker index names no owed input: %s", k))
		}
		if o.Project == rec.Project && o.Kind != types.OwedInputKindReceipt {
			out = append(out, types.AnswerSource{Type: "answer", OwedInput: o.ID, Revision: o.Revision})
		}
	}
	return out, err
}

// Answered are the owed inputs node asked for, ruling and data, that hold
// an answer recorded at their current revision (in force or since
// withdrawn), in id order: the benchmark's answers (§2.8; adc dev bench
// record, P25). Only the current revision's answer counts: a new revision
// drops the answer to the one before (RecordOwed), which is then no longer
// counted (Opus #430 L2). No production command records an owed input
// yet (RecordOwed's callers are tests), so it is empty in production.
func Answered(tx *eventstore.Tx, node types.NodeID) ([]types.OwedInputID, error) {
	prefix := "asker/" + string(node) + "/"
	keys, err := tx.Keys(OwedProjection, prefix)
	out := []types.OwedInputID{}
	for _, k := range keys {
		o, ok, lerr := owedOf(tx, types.OwedInputID(k[len(prefix):]))
		if err = errors.Join(err, lerr); err != nil || !ok || o.RequestedBy != node || string(o.ID) != k[len(prefix):] {
			return nil, errors.Join(err, fmt.Errorf("authority: the asker index names no owed input %s asked: %s", node, k)) // Opus #432 r2 H1′
		}
		if o.Kind != types.OwedInputKindReceipt && o.Answer != nil && o.Answer.Revision == o.Revision {
			out = append(out, o.ID)
		}
	}
	return out, err
}

// Approval is the sealed Ref to approval as recorded, which must be the
// answer in force to owed, which rec's node declares, at revision, its
// current one, in rec's project; otherwise authority_revoked
// (graph.ErrRevalidated), never a Ref to an answer no longer in force.
func (Answers) Approval(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64, approval types.ApprovalID) (types.Ref, error) {
	o, declares, err := declared(tx, rec, owed)
	var held *OwedAnswer
	if err == nil && declares && o.Revision == revision {
		held, err = answerInForce(tx, o)
	}
	if err != nil {
		return types.Ref{}, err
	}
	if held == nil || held.Approval != approval {
		return types.Ref{}, graph.ErrRevalidated{Reason: vocab.ReasonAuthorityRevoked,
			Detail: fmt.Sprintf("approval %s is not the answer in force to %s at revision %d", approval, owed, revision)}
	}
	raw, ok, err := tx.Get(OwedProjection, "approval/"+string(approval))
	var a types.Approval
	var in types.InputAnswerScope
	if err == nil && ok {
		err = errors.Join(json.Unmarshal(raw, &a), json.Unmarshal(a.Scope, &in))
	}
	if err != nil || !ok || !slices.Equal(a.Answers, []types.OwedInputID{owed}) || in.OwedInput != owed || in.OwedRevision != revision ||
		a.Project == nil || *a.Project != rec.Project {
		return types.Ref{}, errors.Join(err, fmt.Errorf("authority: approval %s does not answer %s at revision %d in the node's project", approval, owed, revision))
	}
	d, err := eventstore.ContentDigest(raw)
	if err != nil {
		return types.Ref{}, err
	}
	project := rec.Project
	return knowledge.Seal(types.Ref{Type: types.RefTypeApproval, ID: string(approval), Scope: types.RefScope{Project: &project},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
}
