// SPDX-License-Identifier: Apache-2.0

package graph

// Declared facts (13 "Source tests, package, runtime verification" [G8];
// 07 source_tests, runtime_verification; plan 15 G8).
//
// Decisions:
//   - One tree cannot serve every purpose. A gate runs on the source-test
//     workspace: the candidate with its development dependencies, VCS
//     metadata only if the profile declares the tests need it, and no
//     network (07: the gate runs in phase 2). Runtime verification runs on
//     the package artifact, with the runtime facts of the gate environment
//     the profile names (facts_from): its VCS metadata, network and runtime,
//     and no development dependencies. It consumes exactly one package and
//     no source tree.
//   - Facts are what the sandbox provides, derived from the profile and the
//     node's kind, never from the executor. A runtime verification node
//     consumes a package result; a profile with no runtime verification
//     declares none, and another kind has no declared facts here.

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// The workspaces a sandbox provides.
const (
	SourceTests = "source_tests" // the candidate tree with its development dependencies
	Package     = "package"      // the package artifact's exact bytes
)

// Facts are the environment a sandbox gives a run.
type Facts struct {
	Workspace       string
	DevDependencies bool
	VCSMetadata     bool
	Network         string // none | loopback | full
	Runtime         *types.ProjectProfileGateEnvironmentsItemFactsRuntime
}

// sourceKinds are the results that are source, never the package.
var sourceKinds = []types.ResultKind{types.ResultKindTree, types.ResultKindPatch, types.ResultKindPartialPatch, types.ResultKindCarryOverAttestation}

// DeclaredFacts are the facts profile p declares for node n: the
// source-test workspace for a gate, the package with its environment's
// runtime facts for runtime verification.
func DeclaredFacts(n types.Node, p *types.ProjectProfile) (Facts, error) {
	source := Facts{Workspace: SourceTests, DevDependencies: p.SourceTests.DevDependencies, VCSMetadata: p.SourceTests.VcsMetadata, Network: "none"}
	switch n.Kind {
	case types.NodeKindGate:
		return source, nil
	case types.NodeKindRuntimeVerify:
	default:
		return Facts{}, fmt.Errorf("graph: a %s node has no declared facts", n.Kind)
	}
	packages, sources := 0, 0
	for _, in := range n.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindPackage {
			packages++
		} else if ok && slices.Contains(sourceKinds, src.ResultKind) {
			sources++
		}
	}
	if packages != 1 || sources != 0 {
		return Facts{}, fmt.Errorf("graph: runtime verification %s consumes %d packages and %d source trees, not one package", n.ID, packages, sources)
	}
	rv := p.RuntimeVerification
	if rv.FactsFrom == nil { // null exactly when the command is (07)
		return Facts{}, fmt.Errorf("graph: project %s declares no runtime verification", p.ID)
	}
	if mutation.Is(mutation.RuntimeOnSourceFacts) {
		return source, nil
	}
	i := slices.IndexFunc(p.Gate.Environments, func(e types.ProjectProfileGateEnvironmentsItem) bool { return e.Name == *rv.FactsFrom })
	if i < 0 {
		return Facts{}, fmt.Errorf("graph: no gate environment %s", *rv.FactsFrom)
	}
	env := p.Gate.Environments[i].Facts
	return Facts{Workspace: Package, VCSMetadata: env.VcsMetadata, Network: string(env.Network), Runtime: &env.Runtime}, nil
}
