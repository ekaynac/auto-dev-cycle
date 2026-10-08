// SPDX-License-Identifier: Apache-2.0

// Package owedanswers is the owed-answer fake (11 "Contract-faithful fakes";
// 15 L5): the authority's answers to owed inputs until M1's input.answer
// records them (15 D1). It is faithful to what graph.Authority reads: the
// approval in force that answers an owed input at its revision, in the owed
// input's project, the grant holder it waits on, the owed inputs a node
// asked for, and the Ref to an answering approval. A grant or a
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
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

const (
	// Kind is the fake's one event kind: an owed input's answer, granted or
	// revoked (a null approval).
	Kind = "fake.owed_answer/v1"
	// Projection holds each owed input's answer under its id.
	Projection = "fake_owed_answers"
	// Approvals holds each answering approval's immutable artifact, under
	// the approval id: the one owed input and revision it answers.
	Approvals = "fake_owed_approvals"
)

// Artifact is an approval's answer artifact: what it answers, in which
// project. An approval answers one owed input at one revision, ever.
type Artifact struct {
	Approval types.ApprovalID  `json:"approval"`
	Owed     types.OwedInputID `json:"owed"`
	Revision int64             `json:"revision"`
	Project  types.ProjectID   `json:"project"`
}

// Answer is an owed input's state: its project, the node that asked for it
// (its requested_by), the actor it waits on and the approval answering it at
// revision (none: unanswered or revoked).
type Answer struct {
	Owed     types.OwedInputID `json:"owed"`
	Project  types.ProjectID   `json:"project"`
	Asker    types.NodeID      `json:"asker"`
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
		if a.Approval != nil {
			art := Artifact{Approval: *a.Approval, Owed: a.Owed, Revision: a.Revision, Project: a.Project}
			var was Artifact
			raw, bound, err := tx.Get(Approvals, string(*a.Approval))
			if err == nil && bound {
				err = json.Unmarshal(raw, &was)
			}
			if err != nil || bound && was != art {
				return errors.Join(err, errors.New("owedanswers: an approval answers one owed input at one revision"))
			}
			b, err := json.Marshal(art)
			if err = errors.Join(err, tx.Put(Approvals, string(*a.Approval), b)); err != nil {
				return err
			}
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
	// A node reads an owed input it declares an answer to.
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

// Owed are the owed inputs rec's node asked for, at their current revisions.
func (Authority) Owed(tx *eventstore.Tx, rec types.NodeRecorded) ([]types.AnswerSource, error) {
	keys, err := tx.Keys(Projection, "")
	out := []types.AnswerSource{}
	for _, k := range keys {
		raw, _, gerr := tx.Get(Projection, k)
		var a Answer
		if err = errors.Join(err, gerr, json.Unmarshal(raw, &a)); err != nil {
			return nil, err
		}
		if a.Asker == rec.Node.ID && a.Project == rec.Project {
			out = append(out, types.AnswerSource{Type: "answer", OwedInput: a.Owed, Revision: a.Revision})
		}
	}
	return out, err
}

// Approval is the Ref to the artifact of approval, which must answer owed at
// revision in rec's project: its content is that artifact.
func (Authority) Approval(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64, approval types.ApprovalID) (types.Ref, error) {
	raw, ok, err := tx.Get(Approvals, string(approval))
	var art Artifact
	if err == nil && ok {
		err = json.Unmarshal(raw, &art)
	}
	if err != nil || !ok || art != (Artifact{Approval: approval, Owed: owed, Revision: revision, Project: rec.Project}) {
		return types.Ref{}, errors.Join(err, errors.New("owedanswers: the approval does not answer that owed input in the node's project"))
	}
	d, err := eventstore.ContentDigest(raw)
	if err != nil {
		return types.Ref{}, err
	}
	project := rec.Project
	return knowledge.Seal(types.Ref{Type: types.RefTypeApproval, ID: string(approval), Scope: types.RefScope{Project: &project},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
}

// Owner is the actor owed waits on.
func (f Authority) Owner(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (types.ActorID, error) {
	a, ok, err := f.load(tx, rec, owed)
	if err == nil && !ok {
		err = errors.New("owedanswers: no such owed input of the node's project")
	}
	return a.Owner, err
}
