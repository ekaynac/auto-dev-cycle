// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"reflect"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// A run observes exactly the facts its workspace was prepared with.
func TestPrepareObserve(t *testing.T) {
	rt := &types.ProjectProfileGateEnvironmentsItemFactsRuntime{LanguageVersion: "1.27", Os: "linux", Arch: "amd64"}
	for _, f := range []graph.Facts{
		{Workspace: graph.SourceTests, DevDependencies: true, VCSMetadata: true, Network: "none"},
		{Workspace: graph.Package, Network: "loopback", Runtime: rt},
		{Workspace: graph.Package, VCSMetadata: true, Network: "full"},
	} {
		if got := Prepare(f).Observe(); !reflect.DeepEqual(got, f) {
			t.Errorf("prepared %+v, observed %+v", f, got)
		}
	}
	if w := Prepare(graph.Facts{Workspace: graph.Package}); len(w.Files) != 0 || len(w.Env) != 0 {
		t.Fatalf("an empty declaration gave %+v", w)
	}
}

// A scripted target fails every restore until healed; others restore.
func TestTargets(t *testing.T) {
	var ts Targets
	const a, b = types.EvidenceTargetID("tgt_01m3250v0aer9ss2097svmefj8"), types.EvidenceTargetID("tgt_01m3250vz2ezn81cgvbkcn17jd")
	ts.Fail(a, "snapshot gone")
	if ts.Restore(a) == nil || ts.Restore(a) == nil {
		t.Fatal("a failing target restored")
	}
	if err := ts.Restore(b); err != nil {
		t.Fatal(err)
	}
	ts.Heal(a)
	if err := ts.Restore(a); err != nil {
		t.Fatal(err)
	}
	if got := ts.Restored(); !reflect.DeepEqual(got, []types.EvidenceTargetID{b, a}) {
		t.Fatalf("restored %v", got)
	}
}
