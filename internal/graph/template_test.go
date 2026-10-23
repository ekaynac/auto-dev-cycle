// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// P22a-3 (W12): the candidate template is plan, build, accept and gate of
// the task, round 1, each input a result its producer's kind produces,
// recorded before it; the build alone leases (a builder with the edit
// capability, on account quota); no merge, no review, nothing past M1(a)'s
// kinds, and every node passes the guard and Record.
func TestCandidateTemplate(t *testing.T) {
	const task, project = types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np"), types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	nodes, err := Candidate(task, project)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []types.NodeKind
	seen := map[types.NodeID]types.NodeKind{}
	for _, r := range nodes {
		n := r.Node
		kinds = append(kinds, n.Kind)
		if r.Task != task || r.Project != project || n.Round != 1 || n.DefinitionVersion != TemplateVersion || n.Effect != nil {
			t.Errorf("%s: %+v", n.Kind, r)
		}
		for _, in := range n.Inputs {
			src, ok := in.Source.Variant.(*types.ResultSource)
			if !ok || !slices.Contains(produces[seen[src.Producer]], src.ResultKind) {
				t.Errorf("%s's input %s does not take an earlier node's result", n.Kind, in.Name)
			}
		}
		leases := n.Requires.Role != nil || len(n.Requires.Resources) > 0
		if leases != (n.Kind == types.NodeKindBuild) {
			t.Errorf("%s leases: %+v", n.Kind, n.Requires)
		}
		seen[n.ID] = n.Kind
	}
	if b := nodes[1].Node.Requires; *b.Role != "builder" || *b.Capability != "edit" || !slices.Equal(b.Resources, []types.ResourceGroupKind{types.ResourceGroupKindAccountQuota}) {
		t.Errorf("the build requires %+v", b)
	}
	if !slices.Equal(kinds, []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept, types.NodeKindGate}) {
		t.Fatalf("the template's kinds: %v", kinds)
	}
	// I12 consumption (K2a): the gate takes accept's tree, one input, and nothing else.
	if g := nodes[3].Node; len(g.Inputs) != 1 {
		t.Errorf("the gate's inputs: %+v", g.Inputs)
	} else if src, ok := g.Inputs[0].Source.Variant.(*types.ResultSource); !ok || src.Producer != nodes[2].Node.ID || src.ResultKind != types.ResultKindTree {
		t.Errorf("the gate's input is not accept's tree: %+v", g.Inputs[0].Source.Variant)
	}
	evs, err := RecordTemplate(nodes, task, project, M1Kinds)
	if err != nil || len(evs) != len(nodes) {
		t.Fatalf("recording the template: %d events, %v", len(evs), err)
	}
	if _, err := RecordTemplate(nodes, task, project, []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept}); err == nil {
		t.Error("the guard passed a gate outside the allowed kinds")
	}
	other := slices.Clone(nodes)
	other[2].Task = tskB
	if _, err := RecordTemplate(other, task, project, M1Kinds); err == nil {
		t.Error("a template node of another task was recorded")
	}
}

// fakeTasks records the tasks it is asked about, and refuses every one.
type fakeTasks struct{ recordable, claimable *[]types.TaskID }

func (f fakeTasks) Recordable(_ *eventstore.Tx, task types.TaskID) error {
	*f.recordable = append(*f.recordable, task)
	return ErrNotAdmitted{Task: task, State: "ready"}
}

func (f fakeTasks) Claimable(_ *eventstore.Tx, task types.TaskID) error {
	*f.claimable = append(*f.claimable, task)
	return ErrNotAdmitted{Task: task, State: "ready"}
}

// P22a-3 (Opus #309 H1, M5, F5): the fold takes the task authority for a
// node recorded (Recordable), and for every event that runs a node again
// or anew: claimed, reused, invalidated (claimed at its next generation)
// and renewed (Claimable), each of the node's task; a review's supersession,
// which records its replacement, asks Recordable of the predecessor's task
// (Astra #316 M2: a done task's review is never replaced); nothing else asks.
func TestTakenAsksTheTask(t *testing.T) {
	g := openStore(t)
	var recordable, claimable []types.TaskID
	tasks := fakeTasks{&recordable, &claimable}
	project := string(prjID)
	ev := eventstore.Event{Project: &project}
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		for _, v := range []types.GraphEventVariant{
			&types.NodeRecorded{Task: tskID, Project: prjID},
			&types.NodeClaimed{Node: g.plan}, &types.NodeReused{Node: g.plan}, &types.NodeInvalidated{Node: g.plan}, &types.NodeRenewed{Node: g.plan},
			&types.HoldCleared{Node: g.plan}, &types.ReviewSuperseded{Predecessor: g.plan},
		} {
			_ = taken(tx, ev, v, tasks)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recordable, []types.TaskID{tskID, tskID}) || !slices.Equal(claimable, []types.TaskID{tskID, tskID, tskID, tskID}) {
		t.Errorf("asked: recordable %v, claimable %v", recordable, claimable)
	}
}
