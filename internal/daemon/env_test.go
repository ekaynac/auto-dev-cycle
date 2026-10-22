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
// the registry's families, each task's rails and M1(a)'s requirements, an
// allow-list by the consuming node's kind and the result kind its input
// takes; and task.raise_rail is launch's action, wired only while launch is
// active.
func TestEvaluationEnv(t *testing.T) {
	d := restart(t, stateDir(t))
	t.Cleanup(func() { _ = d.Close() })
	e := env(d, registry.Registry{})
	if e.Need == nil || e.Blobs == nil || e.Families == nil || e.Rails == nil || e.Authority == nil {
		t.Fatalf("the Env: %+v", e)
	}
	takes := func(kind types.ResultKind) types.InputSpec {
		return types.InputSpec{Name: "x", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", ResultKind: kind}}}
	}
	for _, ok := range []struct {
		node  types.NodeKind
		takes types.ResultKind
	}{{types.NodeKindBuild, types.ResultKindPlan}, {types.NodeKindBuild, types.ResultKindReview}, {types.NodeKindBuild, types.ResultKindPatch}, // a repair round's (P22a-6d)
		{types.NodeKindAccept, types.ResultKindPatch}, {types.NodeKindGate, types.ResultKindTree}} {
		if r, err := e.Need(nil, types.Node{Kind: ok.node}, takes(ok.takes)); r != nil || err != nil {
			t.Errorf("%s taking %s: %v, %v", ok.node, ok.takes, r, err)
		}
	}
	// Opus #325 L6: an allow-list by the pair: a build taking a gate
	// result, a plan taking anything, a gate taking a patch, and a
	// requirement taking a tree are refused. A review's gate result takes
	// the gate requirement the log derives (gateNeed: TestTheReviewsGateRequirement).
	for _, no := range []struct {
		node  types.NodeKind
		takes types.ResultKind
	}{{types.NodeKindBuild, types.ResultKindGateResult}, {types.NodeKindPlan, types.ResultKindPlan}, {types.NodeKindGate, types.ResultKindPatch},
		{types.NodeKindReviewRequirement, types.ResultKindTree}} {
		if _, err := e.Need(nil, types.Node{Kind: no.node}, takes(no.takes)); err == nil {
			t.Errorf("%s taking %s: a requirement resolved", no.node, no.takes)
		}
	}
	launch, _ := capability.Of("launch")
	_, wired := handlers()[authority.TaskRaiseRailAction]
	if !slices.Contains(launch.Actions, authority.TaskRaiseRailAction) || wired != launch.Active() {
		t.Errorf("task.raise_rail: launch's %v, wired %v while launch is active %v", slices.Contains(launch.Actions, authority.TaskRaiseRailAction), wired, launch.Active())
	}
}
