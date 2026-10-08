// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

func factsProfile() *types.ProjectProfile {
	prod := types.EnvironmentName("production")
	return &types.ProjectProfile{ID: prjID,
		SourceTests:         types.ProjectProfileSourceTests{DevDependencies: true, VcsMetadata: true},
		RuntimeVerification: types.ProjectProfileRuntimeVerification{Command: &types.Argv{"dist/service", "--self-check"}, FactsFrom: &prod},
		Gate: types.ProjectProfileGate{Environments: []types.ProjectProfileGateEnvironmentsItem{
			{Name: "staging", Facts: types.ProjectProfileGateEnvironmentsItemFacts{VcsMetadata: true, Network: "full",
				Runtime: types.ProjectProfileGateEnvironmentsItemFactsRuntime{LanguageVersion: "1.26", Os: "linux", Arch: "arm64"}}},
			{Name: "production", Facts: types.ProjectProfileGateEnvironmentsItemFacts{VcsMetadata: false, Network: "loopback",
				Runtime: types.ProjectProfileGateEnvironmentsItemFactsRuntime{LanguageVersion: "1.27", Os: "linux", Arch: "amd64"}}}}}}
}

func kindNode(kind types.NodeKind, inputs ...types.ResultKind) types.Node {
	n := types.Node{ID: "nod_01m3250v57fhm9v1z9xd1wk8gw", Kind: kind, Inputs: []types.InputSpec{}}
	for _, rk := range inputs {
		n.Inputs = append(n.Inputs, types.InputSpec{Name: types.Name(rk), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: "nod_01m3250v57ft7aehkqwq0yrq3n", ResultKind: rk}}})
	}
	return n
}

// A gate sees the source-test workspace; runtime verification sees the
// package with its named environment's runtime facts, and nothing else does.
func TestDeclaredFacts(t *testing.T) {
	p := factsProfile()
	gate, err := DeclaredFacts(kindNode(types.NodeKindGate, types.ResultKindTree), p)
	if want := (Facts{Workspace: SourceTests, DevDependencies: true, VCSMetadata: true, Network: "none"}); err != nil || gate != want {
		t.Fatalf("the gate's facts %+v (%v), want %+v", gate, err, want)
	}
	bare := factsProfile()
	bare.SourceTests = types.ProjectProfileSourceTests{}
	if f, err := DeclaredFacts(kindNode(types.NodeKindGate, types.ResultKindTree), bare); err != nil || f != (Facts{Workspace: SourceTests, Network: "none"}) {
		t.Fatalf("a bare source-test workspace's facts %+v (%v)", f, err)
	}
	rv, err := DeclaredFacts(kindNode(types.NodeKindRuntimeVerify, types.ResultKindPlan, types.ResultKindPackage), p)
	if err != nil || rv.Workspace != Package || rv.DevDependencies || rv.VCSMetadata || rv.Network != "loopback" || rv.Runtime == nil || *rv.Runtime != p.Gate.Environments[1].Facts.Runtime {
		t.Fatalf("runtime verification's facts %+v (%v)", rv, err)
	}
	none := factsProfile()
	none.RuntimeVerification = types.ProjectProfileRuntimeVerification{}
	gone := factsProfile()
	gone.Gate.Environments = gone.Gate.Environments[:1]
	for name, c := range map[string]struct {
		n types.Node
		p *types.ProjectProfile
	}{
		"a build":                          {kindNode(types.NodeKindBuild, types.ResultKindTree), p},
		"runtime verification of a tree":   {kindNode(types.NodeKindRuntimeVerify, types.ResultKindTree), p},
		"a package and its source tree":    {kindNode(types.NodeKindRuntimeVerify, types.ResultKindTree, types.ResultKindPackage), p},
		"a package and a patch":            {kindNode(types.NodeKindRuntimeVerify, types.ResultKindPatch, types.ResultKindPackage), p},
		"two packages":                     {kindNode(types.NodeKindRuntimeVerify, types.ResultKindPackage, types.ResultKindPackage), p},
		"no runtime verification declared": {kindNode(types.NodeKindRuntimeVerify, types.ResultKindPackage), none},
		"no such environment":              {kindNode(types.NodeKindRuntimeVerify, types.ResultKindPackage), gone},
	} {
		if f, err := DeclaredFacts(c.n, c.p); err == nil {
			t.Errorf("%s: facts %+v", name, f)
		}
	}
}
