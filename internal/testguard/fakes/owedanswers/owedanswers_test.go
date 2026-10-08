// SPDX-License-Identifier: Apache-2.0

package owedanswers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

const (
	owed = types.OwedInputID("own_01m3250vz2ezn81cgvbkcn17jd")
	apr  = types.ApprovalID("apr_01m3250vz2ezn81cgvbkcn17jd")
	act  = types.ActorID("act_01m3250vz2ezn81cgvbkcn17jd")
	prj  = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	prjB = types.ProjectID("prj_01m3250vm7f6ktw39dszqnyddr")
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
	record := func(a Answer) {
		t.Helper()
		cmd, err := ids.New("cmd")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(act), Action: "fake.answer", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := Record(a)
			return evs, json.RawMessage(`{}`), err
		}))
		if err != nil {
			t.Fatal(err)
		}
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
	record(Answer{Owed: owed, Project: prj, Owner: act, Revision: 1, Approval: &approval})
	if !answered(node(prj), 1) || answered(node(prj), 2) || answered(node(prjB), 1) || answered(types.NodeRecorded{Project: prj}, 1) {
		t.Fatal("the grant")
	}
	record(Answer{Owed: owed, Project: prj, Owner: act, Revision: 1})
	if answered(node(prj), 1) {
		t.Fatal("the revocation")
	}
}
