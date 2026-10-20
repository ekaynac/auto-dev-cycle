// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The candidate template (M1 P22a-3; 00-slice-plan §1 and §2.3; the P22a
// plan §4): the nodes a task's admission records, in the admitting
// command, after its task.admitted.
//   - plan, the task's spec as a recorded result (the controller's own: no
//     role, no model); build round 1 on the plan (a builder with the edit
//     capability, reserving account quota); accept on the build's patch
//     (the tree); gate on accept's tree (the gate result). accept and gate
//     are the controller's own.
//   - Variant candidate: no merge node, ever (merge-* inactive). The review
//     with its requirement joins the template only when the review
//     capability is active (P22a-6); until then the template stops before
//     review (§1's table).
//   - The guard (CheckSurface) runs on the template before any node is
//     recorded: the admitting command records nothing when it refuses.

// TemplateVersion is the candidate template's definition version, every
// node's DefinitionVersion: a change is an explicit invalidation (13 [G1]).
const TemplateVersion = "1"

// Candidate is task's candidate template in project, its nodes in record
// order: each node's producers before it.
func Candidate(task types.TaskID, project types.ProjectID) ([]types.NodeRecorded, error) {
	var nodeIDs [4]types.NodeID
	for i := range nodeIDs {
		id, err := ids.New("nod")
		if err != nil {
			return nil, err
		}
		nodeIDs[i] = types.NodeID(id)
	}
	plan, build, accept, gate := nodeIDs[0], nodeIDs[1], nodeIDs[2], nodeIDs[3]
	on := func(name string, producer types.NodeID, kind types.ResultKind) []types.InputSpec {
		return []types.InputSpec{{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}}}}
	}
	node := func(id types.NodeID, kind types.NodeKind, inputs []types.InputSpec) types.NodeRecorded {
		return types.NodeRecorded{Task: task, Project: project, Node: types.Node{ID: id, Kind: kind, DefinitionVersion: TemplateVersion, Round: 1,
			Params: json.RawMessage(`{}`), Inputs: inputs, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	}
	builder, edit := types.Name("builder"), types.Name("edit")
	b := node(build, types.NodeKindBuild, on("plan", plan, types.ResultKindPlan))
	b.Node.Requires = types.NodeRequires{Role: &builder, Capability: &edit, Resources: []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}}
	return []types.NodeRecorded{
		node(plan, types.NodeKindPlan, []types.InputSpec{}),
		b,
		node(accept, types.NodeKindAccept, on("patch", build, types.ResultKindPatch)),
		node(gate, types.NodeKindGate, on("tree", accept, types.ResultKindTree)),
	}, nil
}

// RecordTemplate is the events recording template, a task's nodes, once
// the guard passes it: every node of the task and project, of a kind in
// allowed, under an Env with no instrument contracts (I15 admission). It
// records nothing when the guard refuses.
func RecordTemplate(template []types.NodeRecorded, task types.TaskID, project types.ProjectID, allowed []types.NodeKind) ([]eventstore.Event, error) {
	nodes := make([]types.Node, len(template))
	for i, r := range template {
		if r.Task != task || r.Project != project {
			return nil, fmt.Errorf("graph: template node %s is of task %s in %s, not of %s in %s", r.Node.ID, r.Task, r.Project, task, project)
		}
		nodes[i] = r.Node
	}
	if err := CheckSurface(Env{}, nodes, allowed); err != nil {
		return nil, err
	}
	var evs []eventstore.Event
	for _, r := range template {
		ev, err := Record(r)
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev...)
	}
	return evs, nil
}

// Tasks is the task authority a claim takes (I22 consumption; I1's at the
// first claim): a node is claimed, or reused, only while its task is
// admitted and the task's approval is in force, so a revocation before the
// first claim refuses it and no attempt starts. adcd composes it from
// authority.Tasks.
type Tasks interface {
	// Claimable refuses a claim of a node of task: ErrNotAdmitted, or
	// ErrRevalidated (authority_revoked) when the task's approval is not in
	// force.
	Claimable(tx *eventstore.Tx, task types.TaskID) error
}

// ErrNotAdmitted: a node of a task that is not admitted is never claimed
// (I22 consumption).
type ErrNotAdmitted struct {
	Task  types.TaskID
	State string
}

func (e ErrNotAdmitted) Error() string {
	return fmt.Sprintf("graph: task %s is %s, not admitted: its nodes are not claimed", e.Task, e.State)
}

// taken: a claim or reuse in v takes a task tasks allows (claimable).
func taken(tx *eventstore.Tx, ev eventstore.Event, v types.GraphEventVariant, tasks Tasks) error {
	switch p := v.(type) {
	case *types.NodeClaimed:
		return claimable(tx, ev, p.Node, tasks)
	case *types.NodeReused:
		return claimable(tx, ev, p.Node, tasks)
	}
	return nil
}

// claimable: the claim (or reuse) of node, as ev fixes it, takes a task
// tasks allows.
func claimable(tx *eventstore.Tx, ev eventstore.Event, node types.NodeID, tasks Tasks) error {
	if tasks == nil {
		return nil
	}
	rec, err := nodeOf(tx, ev, node)
	if err != nil {
		return err
	}
	return tasks.Claimable(tx, rec.Recorded.Task)
}
