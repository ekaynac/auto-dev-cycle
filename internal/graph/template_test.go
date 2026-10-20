// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
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
	evs, err := RecordTemplate(nodes, task, project, M1Kinds)
	if err != nil || len(evs) != len(nodes) {
		t.Fatalf("recording the template: %d events, %v", len(evs), err)
	}
	if _, err := RecordTemplate(nodes, task, project, []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept}); err == nil {
		t.Error("the guard passed a gate outside the allowed kinds")
	}
}
