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

// Tasks is the task authority the graph takes (I22; I1's consumption): a
// node is recorded only for an admitted task, and claimed, reused,
// re-claimed (invalidated or renewed) or started only while its task is
// admitted with its approval in force, so a revocation refuses every run
// of the task's nodes from then on. adcd composes it from authority.Tasks.
type Tasks interface {
	// Recordable refuses a node of task unless task is admitted
	// (ErrNotAdmitted).
	Recordable(tx *eventstore.Tx, task types.TaskID) error
	// Claimable refuses a run of a node of task: ErrNotAdmitted, or
	// ErrRevalidated (authority_revoked) when the task's approval is not in
	// force.
	Claimable(tx *eventstore.Tx, task types.TaskID) error
}

// ErrNotAdmitted: a node of a task that is not admitted is never recorded
// or run (I22).
type ErrNotAdmitted struct {
	Task  types.TaskID
	State string
}

func (e ErrNotAdmitted) Error() string {
	return fmt.Sprintf("graph: task %s is %s, not admitted: its nodes are not recorded or run", e.Task, e.State)
}

// taken: v takes a task tasks allows: a node recorded, or a review's
// replacement recorded by its supersession, Recordable (Astra #316 M2); a
// node claimed, reused, invalidated (claimed again at its next generation)
// or renewed (its key claimed again), Claimable (Opus #309 H1).
func taken(tx *eventstore.Tx, ev eventstore.Event, v types.GraphEventVariant, tasks Tasks) error {
	if tasks == nil {
		return nil
	}
	var node types.NodeID
	switch p := v.(type) {
	case *types.NodeRecorded:
		return tasks.Recordable(tx, p.Task)
	case *types.NodeClaimed:
		node = p.Node
	case *types.NodeReused:
		node = p.Node
	case *types.NodeInvalidated:
		node = p.Node
	case *types.NodeRenewed:
		node = p.Node
	case *types.ReviewSuperseded: // it records the replacement: the predecessor's task must be admitted (Astra #316 M2)
		rec, err := nodeOf(tx, ev, p.Predecessor)
		if err != nil {
			return err
		}
		return tasks.Recordable(tx, rec.Recorded.Task)
	default:
		return nil
	}
	rec, err := nodeOf(tx, ev, node)
	if err != nil {
		return err
	}
	return tasks.Claimable(tx, rec.Recorded.Task)
}
