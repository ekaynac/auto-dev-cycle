// SPDX-License-Identifier: Apache-2.0

// Package owedanswers is the owed-answer fake (11 "Contract-faithful fakes";
// 15 L5): the authority's answers to owed inputs until M1's input.answer
// records them (15 D1). It is faithful to what graph.Authority reads: the
// approval in force that answers an owed input at its revision, in the owed
// input's project, and the grant holder it waits on. A grant or a
// revocation is an event of its own, folded into a projection, so an
// evaluation sees exactly the answers its transaction sees and a rebuild
// reaches the same state. Who may answer (04) is M1's. An owed input the
// fake has not seen has no owner, which fails the evaluation: a fixture
// records it unanswered (a null approval) before it evaluates.
package owedanswers

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

const (
	// Kind is the fake's one event kind: an owed input's answer, granted or
	// revoked (a null approval).
	Kind = "fake.owed_answer/v1"
	// Projection holds each owed input's answer under its id.
	Projection = "fake_owed_answers"
)

// Answer is an owed input's state: its project, the actor it waits on and
// the approval answering it at revision (none: unanswered or revoked).
type Answer struct {
	Owed     types.OwedInputID `json:"owed"`
	Project  types.ProjectID   `json:"project"`
	Owner    types.ActorID     `json:"owner"`
	Revision int64             `json:"revision"`
	Approval *types.ApprovalID `json:"approval"`
}

// Kinds registers Kind; its payload is checked by the fold.
func Kinds() eventstore.Kinds { return eventstore.Kinds{Kind: {Schema: "common/defs"}} }

// Record is the event setting a's state.
func Record(a Answer) ([]eventstore.Event, error) {
	b, err := json.Marshal(a)
	id, ierr := ids.New("evt")
	p := string(a.Project)
	return []eventstore.Event{{ID: id, Kind: Kind, Subject: string(a.Owed), Project: &p, Payload: b}}, errors.Join(err, ierr)
}

// Project folds Kind; other kinds pass through.
func Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != Kind {
			continue
		}
		var a Answer
		if err := json.Unmarshal(ev.Payload, &a); err != nil || ev.Subject != string(a.Owed) || ev.Project == nil || *ev.Project != string(a.Project) {
			return errors.Join(err, errors.New("owedanswers: an answer is under its owed input, in its project"))
		}
		if err := tx.Put(Projection, string(a.Owed), ev.Payload); err != nil {
			return err
		}
	}
	return nil
}

// Authority answers from the projection, in the transaction it is given.
type Authority struct{}

func (Authority) load(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (Answer, bool, error) {
	var a Answer
	raw, ok, err := tx.Get(Projection, string(owed))
	if err != nil || !ok {
		return a, false, err
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, false, err
	}
	asks := slices.ContainsFunc(rec.Node.Inputs, func(in types.InputSpec) bool {
		ask, ok := in.Source.Variant.(*types.AnswerSource)
		return ok && ask.OwedInput == owed
	})
	return a, asks && a.Project == rec.Project, nil
}

// Answer is the approval answering owed at revision for rec's node, in its
// project.
func (f Authority) Answer(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64) (types.ApprovalID, bool, error) {
	a, ok, err := f.load(tx, rec, owed)
	if err != nil || !ok || a.Approval == nil || a.Revision != revision {
		return "", false, err
	}
	return *a.Approval, true, nil
}

// Owner is the actor owed waits on.
func (f Authority) Owner(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (types.ActorID, error) {
	a, ok, err := f.load(tx, rec, owed)
	if err == nil && !ok {
		err = errors.New("owedanswers: no such owed input of the node's project")
	}
	return a.Owner, err
}
