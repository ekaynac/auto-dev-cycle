// SPDX-License-Identifier: Apache-2.0

// Package sandbox is the fake sandbox of docs/design/m0/11 ("Contract-faithful
// fakes"): it prepares a workspace with exactly the facts it is given, which a
// check run in it observes, and it restores an evidence target, or fails to,
// as the test scripts.
//
// M0 scope (15 D4): declared facts and restore (G8). The three phases,
// artifact-before-teardown, the evidence manifest and abrupt loss land with
// the real sandbox (M1).
package sandbox

import (
	"fmt"
	"slices"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// Workspace is what a run sees: its files by path and its environment.
type Workspace struct {
	Kind    string          // graph.SourceTests or graph.Package
	Files   map[string]bool // ".git" with VCS metadata, "node_modules/.dev" with development dependencies
	Env     map[string]string
	Network string
}

// Prepare is the workspace facts f describe, and nothing more.
func Prepare(f graph.Facts) Workspace {
	w := Workspace{Kind: f.Workspace, Files: map[string]bool{}, Env: map[string]string{}, Network: f.Network}
	if f.VCSMetadata {
		w.Files[".git"] = true
	}
	if f.DevDependencies {
		w.Files["node_modules/.dev"] = true
	}
	if r := f.Runtime; r != nil {
		w.Env["LANGUAGE_VERSION"], w.Env["OS"], w.Env["ARCH"] = r.LanguageVersion, r.Os, r.Arch
	}
	return w
}

// Observe is what a check run in w finds: the facts it can see.
func (w Workspace) Observe() graph.Facts {
	f := graph.Facts{Workspace: w.Kind, VCSMetadata: w.Files[".git"], DevDependencies: w.Files["node_modules/.dev"], Network: w.Network}
	if v, ok := w.Env["LANGUAGE_VERSION"]; ok {
		f.Runtime = &types.ProjectProfileGateEnvironmentsItemFactsRuntime{LanguageVersion: v, Os: w.Env["OS"], Arch: w.Env["ARCH"]}
	}
	return f
}

// Targets restores evidence targets; a target scripted to fail fails every
// restore until the script is lifted. It is safe for concurrent use.
type Targets struct {
	mu       sync.Mutex
	failing  map[types.EvidenceTargetID]string
	restored []types.EvidenceTargetID
}

// Fail makes target's restores fail with reason; Heal lifts it.
func (t *Targets) Fail(target types.EvidenceTargetID, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failing == nil {
		t.failing = map[types.EvidenceTargetID]string{}
	}
	t.failing[target] = reason
}

func (t *Targets) Heal(target types.EvidenceTargetID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failing, target)
}

// Restore puts target back, or reports why it could not.
func (t *Targets) Restore(target types.EvidenceTargetID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if reason, ok := t.failing[target]; ok {
		return fmt.Errorf("sandbox: restore of %s failed: %s", target, reason)
	}
	t.restored = append(t.restored, target)
	return nil
}

// Restored lists the targets restored, in order.
func (t *Targets) Restored() []types.EvidenceTargetID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.restored)
}
