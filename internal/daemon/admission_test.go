// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// readyTask records, through w, an operator (created by sys), graphProject
// (once) and an approved task in it, in Ready; it returns the operator and
// the task.
func readyTask(t *testing.T, w *seeder, sys types.ActorID) (types.ActorID, types.TaskID) {
	t.Helper()
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys}
	w.do(sys, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	project := string(graphProject)
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if _, ok, err := tx.Get(authority.ProjectProjection, project); err != nil || ok {
			return nil, err
		}
		raw, err := os.ReadFile("../../fixtures/m1/profiles/py-mini.json")
		var doc map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &doc)
		}
		if err != nil {
			return nil, err
		}
		doc["id"] = project
		payload, err := json.Marshal(doc)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: project, Project: &project, Payload: payload}}, err
	})
	const accept = "sha256:abababababababababababababababababababababababababababababababab"
	spec := types.TaskCreateScope{Project: graphProject, Title: "t", Requirement: "r", Attachments: []types.Attachment{{Name: "accept_test.py", Digest: accept}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := eventstore.ContentDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	task := types.TaskID(newID(t, "tsk"))
	w.do(op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		x := types.Task{ID: task, Project: graphProject, Spec: spec, SpecDigest: dg, CreatedBy: op.ID,
			CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano)), Acceptance: []types.AcceptanceBlob{{Name: "accept_test.py", Digest: accept,
				Class: types.AcceptanceBlobClassRestricted, WithheldFrom: []types.AcceptanceBlobWithheldFromItem{
					types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}}
		payload, err := json.Marshal(x)
		evs := []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskCreated, Subject: string(task), Project: &project, Payload: payload}}
		more, aerr := authority.ApprovalEvent(authority.TaskApprovalOf(x, raw, types.ApprovalID(newID(t, "apr")), x.CreatedAt))
		return append(evs, more...), errors.Join(err, aerr)
	})
	return op.ID, task
}

// admittedTask is readyTask, admitted by sys with its candidate template
// (P22a-3): its nodes may be claimed.
func admittedTask(t *testing.T, w *seeder, sys types.ActorID) (types.ActorID, types.TaskID) {
	t.Helper()
	op, task := readyTask(t, w, sys)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	return op, task
}

// P22a-3 (I22, I1 consumption), through adcd's own fold: a node of a task
// in Ready is never claimed, admission records the candidate template
// after task.admitted, a node of the admitted task is claimed, and once
// the task's approval is revoked (its operator revoked) a claim of its
// build is refused authority_revoked, appending nothing.
func TestDaemonClaimTakesAdmission(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	_, ready := readyTask(t, w, sys.ID)
	early := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: ready, Project: graphProject, Node: early})
	})
	claim := func(n types.NodeID, resolved map[string]graph.Resolved) error {
		return w.try(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Claim(tx, n, resolved, 1) })
	}
	var notAdmitted graph.ErrNotAdmitted
	if err := claim(early.ID, map[string]graph.Resolved{}); !errors.As(err, &notAdmitted) {
		t.Fatalf("a claim of a Ready task's node: %v", err)
	}
	w.do(sys.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	var plan, build types.NodeID
	var kinds []types.NodeKind
	w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Task == ready && n.Node.ID != early.ID {
				kinds = append(kinds, n.Node.Kind)
				switch n.Node.Kind {
				case types.NodeKindPlan:
					plan = n.Node.ID
				case types.NodeKindBuild:
					build = n.Node.ID
				}
			}
		}
		return err
	})
	if len(kinds) != 4 || plan == "" || build == "" {
		t.Fatalf("the admitted task's template: %v", kinds)
	}
	if err := claim(plan, map[string]graph.Resolved{}); err != nil {
		t.Fatalf("a claim of the admitted task's plan: %v", err)
	}
	var op types.ActorID
	w.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(authority.TaskProjection, string(ready))
		var r authority.TaskRecord
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		op = r.CreatedBy
		return err
	})
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, op, time.Now()) })
	head := w.head()
	var revoked graph.ErrRevalidated
	err := claim(build, map[string]graph.Resolved{"plan": {Result: types.ResultID(newID(t, "res"))}})
	if !errors.As(err, &revoked) || !strings.Contains(err.Error(), "authority_revoked") || w.head() != head {
		t.Errorf("a build claim after the approval's revocation: %v", err)
	}
}
