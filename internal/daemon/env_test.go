// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
)

// P22a-5b (Opus #312 L5): adcd's evaluation Env carries the receipt blobs,
// the registry's families, each task's rails and M1(a)'s requirements:
// none on the inputs of the plan, the build, accept and the gate; a
// review's refused until the review capability; and task.raise_rail is
// launch's action, wired only while launch is active.
func TestEvaluationEnv(t *testing.T) {
	d := restart(t, stateDir(t))
	t.Cleanup(func() { _ = d.Close() })
	e := env(d, registry.Registry{})
	if e.Need == nil || e.Blobs == nil || e.Families == nil || e.Rails == nil || e.Authority == nil {
		t.Fatalf("the Env: %+v", e)
	}
	in := types.InputSpec{Name: "x"}
	for _, k := range []types.NodeKind{types.NodeKindPlan, types.NodeKindBuild, types.NodeKindAccept, types.NodeKindGate} {
		if r, err := e.Need(types.Node{Kind: k}, in); r != nil || err != nil {
			t.Errorf("%s: %v, %v", k, r, err)
		}
	}
	for _, k := range []types.NodeKind{types.NodeKindReview, types.NodeKindReviewRequirement} {
		if _, err := e.Need(types.Node{Kind: k}, in); err == nil {
			t.Errorf("%s: a requirement resolved before the review capability", k)
		}
	}
	launch, _ := capability.Of("launch")
	_, wired := handlers()[authority.TaskRaiseRailAction]
	if !slices.Contains(launch.Actions, authority.TaskRaiseRailAction) || wired != launch.Active() {
		t.Errorf("task.raise_rail: launch's %v, wired %v while launch is active %v", slices.Contains(launch.Actions, authority.TaskRaiseRailAction), wired, launch.Active())
	}
}
