// SPDX-License-Identifier: Apache-2.0

package owedanswers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const (
	owed  = types.OwedInputID("own_01m3250vz2ezn81cgvbkcn17jd")
	owedB = types.OwedInputID("own_01m3250vnfeyrv67x6w0ddqr6a")
	apr   = types.ApprovalID("apr_01m3250vz2ezn81cgvbkcn17jd")
	act   = types.ActorID("act_01m3250vz2ezn81cgvbkcn17jd")
	prj   = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	prjB  = types.ProjectID("prj_01m3250vm7f6ktw39dszqnyddr")
)

// An answer is read through the transaction: granted, then revoked, only
// for a node of its project that declares the owed input, at its revision.
func TestAuthority(t *testing.T) {
	dir := testguard.StateDir(t)
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: Project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	record := func(a Answer) error {
		t.Helper()
		cmd, err := ids.New("cmd")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(act), Action: "fake.answer", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := Record(a)
			return evs, json.RawMessage(`{}`), err
		}))
		return err
	}
	node := func(project types.ProjectID) types.NodeRecorded {
		return types.NodeRecorded{Project: project, Node: types.Node{Inputs: []types.InputSpec{{Name: "ruling",
			Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: owed, Revision: 1}}}}}}
	}
	answered := func(rec types.NodeRecorded, revision int64) bool {
		var ok bool
		if err := s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
			_, ok, err = Authority{}.Answer(tx, rec, owed, revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	approval := apr
	asker := types.NodeID("nod_01m3250vz2ezn81cgvbkcn17jd")
	if err := record(Answer{Owed: owed, Project: prj, Asker: asker, Owner: act, Revision: 1, Approval: &approval}); err != nil {
		t.Fatal(err)
	}
	if !answered(node(prj), 1) || answered(node(prj), 2) || answered(node(prjB), 1) || answered(types.NodeRecorded{Project: prj}, 1) {
		t.Fatal("the grant")
	}
	// What a node owes, and an approval's artifact, stay in their project.
	if err := s.View(context.Background(), func(tx *eventstore.Tx) error {
		mine, err := Authority{}.Owed(tx, types.NodeRecorded{Project: prj, Node: types.Node{ID: asker}})
		theirs, terr := Authority{}.Owed(tx, types.NodeRecorded{Project: prjB, Node: types.Node{ID: asker}})
		_, here := Authority{}.Approval(tx, node(prj), owed, 1, apr)
		_, there := Authority{}.Approval(tx, node(prjB), owed, 1, apr)
		_, stale := Authority{}.Approval(tx, node(prj), owed, 2, apr)
		if err = errors.Join(err, terr, here); err == nil && (len(mine) != 1 || len(theirs) != 0 || there == nil || stale == nil) {
			t.Errorf("isolation: %v %v %v %v", mine, theirs, there, stale)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := record(Answer{Owed: owedB, Project: prj, Owner: act, Revision: 1, Approval: &approval}); err == nil {
		t.Fatal("one approval answered two owed inputs")
	}
	if err := record(Answer{Owed: owed, Project: prj, Owner: act, Revision: 1}); err != nil || answered(node(prj), 1) {
		t.Fatalf("the revocation: %v", err)
	}
}
