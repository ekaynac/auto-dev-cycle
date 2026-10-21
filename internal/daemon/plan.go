// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// planner is adcd's reconcile.Planner (P22a-5c-3; #226 entries 103 and
// 117): the controller records a claimed plan node's result itself, one
// ref, the plan brief of its task's spec (internal/brief): the blob
// published in the controller's blob store, a root ref in the project's
// scope under the project's data policy. Inert while launch is inactive:
// the loop decides no Plan until then.
type planner struct {
	blobs *blobstore.Store
	env   graph.Env
}

var _ reconcile.Planner = planner{}

// Plan records node's result, its plan brief, at its claim's key
// (graph.Commit, which emits the events and re-validates the claim).
func (p planner) Plan(tx *eventstore.Tx, node types.NodeID) error {
	v, err := graph.ViewOf(tx, node)
	switch {
	case err != nil:
		return err
	case v.Node.Kind != types.NodeKindPlan || v.State.State != types.NodeStateStateClaimed || v.State.Key == nil:
		return fmt.Errorf("daemon: node %s is no claimed plan node", node)
	}
	task, err := authority.TaskOf(tx, v.Task)
	if err != nil {
		return err
	}
	policy, ok, err := authority.ProjectDataPolicy(tx, v.Project)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("daemon: node %s's project %s has no profile", node, v.Project))
	}
	r, err := brief.Restriction(v.Project, policy)
	if err != nil {
		return err
	}
	b, d, err := brief.Of(task, r.PolicyDigest)
	if err != nil {
		return err
	}
	if _, err := p.blobs.Put(bytes.NewReader(b), string(d)); err != nil { // published before the result that names it
		return err
	}
	ref, err := brief.Ref(d, v.Project, r)
	if err != nil {
		return err
	}
	id, err := ids.New("res")
	if err != nil {
		return err
	}
	return graph.Commit(tx, node, types.Result{ID: types.ResultID(id), Computation: v.State.Key.Key, Kind: types.ResultKindPlan,
		Value: []types.Ref{ref}, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}, p.env)
}
