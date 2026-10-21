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
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// readyTask is readyTaskIn graphProject.
func readyTask(t *testing.T, w *seeder, sys types.ActorID) (types.ActorID, types.TaskID) {
	t.Helper()
	return readyTaskIn(t, w, sys, graphProject)
}

// readyTaskIn records, through w, its operator (created by sys, once: the
// system creates only the first), project (once) and an approved task in
// it, in Ready; it returns the operator and the task.
func readyTaskIn(t *testing.T, w *seeder, sys types.ActorID, project types.ProjectID) (types.ActorID, types.TaskID) {
	t.Helper()
	if w.op == "" {
		op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys}
		w.do(sys, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
		w.op = op.ID
	}
	op, prj := w.op, string(project)
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if _, ok, err := tx.Get(authority.ProjectProjection, prj); err != nil || ok {
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
		doc["id"] = prj
		payload, err := json.Marshal(doc)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: prj, Project: &prj, Payload: payload}}, err
	})
	const accept = "sha256:abababababababababababababababababababababababababababababababab"
	spec := types.TaskCreateScope{Project: project, Title: "t", Requirement: "r", Attachments: []types.Attachment{{Name: "accept_test.py", Digest: accept}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := eventstore.ContentDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	task := types.TaskID(newID(t, "tsk"))
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		x := types.Task{ID: task, Project: project, Spec: spec, SpecDigest: dg, CreatedBy: op,
			CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano)), Acceptance: []types.AcceptanceBlob{{Name: "accept_test.py", Digest: accept,
				Class: types.AcceptanceBlobClassRestricted, WithheldFrom: []types.AcceptanceBlobWithheldFromItem{
					types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}}}}
		payload, err := json.Marshal(x)
		evs := []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.TaskCreated, Subject: string(task), Project: &prj, Payload: payload}}
		more, aerr := authority.ApprovalEvent(authority.TaskApprovalOf(x, raw, types.ApprovalID(newID(t, "apr")), x.CreatedAt))
		return append(evs, more...), errors.Join(err, aerr)
	})
	return op, task
}

// admittedTask is readyTask, admitted by sys with its candidate template
// (P22a-3): its nodes may be claimed.
func admittedTask(t *testing.T, w *seeder, sys types.ActorID) (types.ActorID, types.TaskID) {
	t.Helper()
	op, task := readyTask(t, w, sys)
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	return op, task
}

// plannedIn is the plan node of a new task in project, admitted with its
// template, the task admitted before it done first (its nodes neither
// claimed nor held), recorded as w's operator and system (w.op, w.sys).
func plannedIn(t *testing.T, w *seeder, project types.ProjectID) types.NodeID {
	t.Helper()
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if prev, busy, err := authority.AdmittedTask(tx); err != nil || busy {
			evs, derr := authority.DoneTask(tx, prev)
			return evs, errors.Join(err, derr)
		}
		return nil, nil
	})
	_, task := readyTaskIn(t, w, w.sys, project)
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	return templateNodes(w, task)[types.NodeKindPlan]
}

// templateNodes are task's template nodes by kind.
func templateNodes(w *seeder, task types.TaskID) map[types.NodeKind]types.NodeID {
	out := map[types.NodeKind]types.NodeID{}
	w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Task == task {
				out[n.Node.Kind] = n.Node.ID
			}
		}
		return err
	})
	return out
}

// withSystem is a seeder over adcd's projector, with quota for one lease,
// and its system actor.
func withSystem(t *testing.T) (*seeder, types.ActorID) {
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	return w, sys.ID
}

// P22a-3 (I22, I1 consumption; Opus #309 H1, M1, M3, M5), through adcd's
// own fold: a node of a Ready or unrecorded task is never recorded;
// admission records the candidate template after task.admitted; the
// admitted plan is claimed and its lease granted. Once the task's approval
// is revoked (its operator revoked), each run of its nodes is refused
// authority_revoked, appending nothing: a build's claim, the plan's
// re-claim by invalidation, and the attempt start on the plan's granted
// lease (no attempt recorded). The task is not abandoned while that lease
// is live, nor once it expires while it is still releasing (a lease that
// has not ended, Astra #316 H1); once it is released, it is, and the slot
// frees.
func TestDaemonClaimTakesAdmission(t *testing.T) {
	w, sys := withSystem(t)
	op, ready := readyTask(t, w, sys)
	early := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	var notAdmitted graph.ErrNotAdmitted
	for name, task := range map[string]types.TaskID{"a Ready task": ready, "an unrecorded task": types.TaskID(newID(t, "tsk"))} {
		err := w.try(sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Record(types.NodeRecorded{Task: task, Project: graphProject, Node: early})
		})
		if !errors.As(err, &notAdmitted) {
			t.Errorf("a node of %s: %v", name, err)
		}
	}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.Admit(tx, nodeKinds()) })
	nodes := templateNodes(w, ready)
	if len(nodes) != 4 {
		t.Fatalf("the admitted task's template: %v", nodes)
	}
	claim := func(n types.NodeID, resolved map[string]graph.Resolved) error {
		return w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Claim(tx, n, resolved, 1) })
	}
	if err := claim(nodes[types.NodeKindPlan], map[string]graph.Resolved{}); err != nil {
		t.Fatalf("a claim of the admitted task's plan: %v", err)
	}
	ls := w.lease(4)
	ls.Unit = graph.UnitOf(nodes[types.NodeKindPlan])
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, op, time.Now()) })
	head := w.head()
	revoked := func(err error) bool {
		var r graph.ErrRevalidated
		return errors.As(err, &r) && strings.Contains(err.Error(), "authority_revoked")
	}
	for name, err := range map[string]error{
		"a build's claim": claim(nodes[types.NodeKindBuild], map[string]graph.Resolved{"plan": {Result: types.ResultID(newID(t, "res"))}}),
		"the plan's re-claim by invalidation": w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Invalidate(tx, nodes[types.NodeKindPlan])
		}),
		"the attempt start on the plan's lease": w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.StartAttempt(tx, ls.ID, 2, types.AttemptID(newID(t, "att")), types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
		}),
	} {
		if !revoked(err) || w.head() != head {
			t.Errorf("%s after the revocation: %v", name, err)
		}
	}
	w.view(func(tx *eventstore.Tx) error {
		if _, started, err := lease.CurrentAttempt(tx, ls.Unit); err != nil || started {
			t.Errorf("an attempt is recorded on the plan's unit: %v", err)
		}
		return nil
	})
	abandon := func() error {
		return w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.AbandonTask(tx, ready) })
	}
	if err := abandon(); !errors.Is(err, eventstore.ErrStaleExpectedState) {
		t.Errorf("an abandonment while the plan's lease is live: %v", err)
	}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 2, lease.LeaseEvent{Kind: lease.Expired}, nil)
	})
	if err := abandon(); !errors.Is(err, eventstore.ErrStaleExpectedState) || !strings.Contains(err.Error(), "releasing") {
		t.Errorf("an abandonment while the plan's lease is releasing: %v", err)
	}
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 3, lease.LeaseEvent{Kind: lease.Released}, nil)
	})
	if err := abandon(); err != nil {
		t.Fatalf("the abandonment once the lease is released: %v", err)
	}
	w.view(func(tx *eventstore.Tx) error {
		if _, busy, err := authority.AdmittedTask(tx); err != nil || busy {
			t.Errorf("the abandoned task holds the slot: %v", err)
		}
		return nil
	})
}

// Opus #309 H1, input 2, through adcd's own fold: task A's plan completed
// and A done, B admitted; invalidating A's plan, which would claim it again
// at its next generation, is refused (A is not admitted): a done task runs
// nothing while another is admitted.
func TestDaemonDoneTaskRunsNothing(t *testing.T) {
	w, sys := withSystem(t)
	_, first := admittedTask(t, w, sys)
	plan := templateNodes(w, first)[types.NodeKindPlan]
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		nodes, err := graph.Nodes(tx)
		var key types.ComputationKey
		for _, n := range nodes {
			if err == nil && n.Node.ID == plan {
				key, err = graph.Key(n.Node, map[string]graph.Resolved{}, 1)
			}
		}
		if err != nil {
			return nil, err
		}
		r := types.Result{ID: types.ResultID(newID(t, "res")), Computation: key.Key, Kind: types.ResultKindPlan, Value: []types.Ref{},
			Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
		none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
		return nil, graph.Commit(tx, plan, r, graph.Env{Need: none, Authority: authority.Answers{}})
	})
	w.do(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.DoneTask(tx, first) })
	_, second := admittedTask(t, w, sys)
	if len(templateNodes(w, second)) != 4 {
		t.Fatalf("B is not admitted")
	}
	head := w.head()
	err := w.try(sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, plan) })
	var notAdmitted graph.ErrNotAdmitted
	if !errors.As(err, &notAdmitted) || w.head() != head {
		t.Errorf("invalidating the done task's plan: %v", err)
	}
}
