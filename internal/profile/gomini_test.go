// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
)

// go-mini, checkpoint (a)'s stub project (#226 entry 94): its profile passes
// the schema, the project.create scope and Check; it pins this repository's
// prepared Go image (provisional, #226 entry 97), gates T0 go-mini offline
// with go_test_json, and
// its remote is an absolute path the sandbox clones (#226 entry 28).
func TestGoMiniProfile(t *testing.T) {
	raw, err := os.ReadFile("../../fixtures/m1/profiles/go-mini.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate("api/scope/project-create", raw); err != nil {
		t.Fatalf("the project.create scope: %v", err)
	}
	p, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	self, err := Load(loadSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "go-mini" || p.Forge.Kind != "git" || p.Repo.BaseRef != "refs/heads/main" || !filepath.IsAbs(p.Repo.Url) ||
		p.Gate.ResultAdapter != results.GoTestJSON || !slices.Equal(p.Gate.Command, []string{"go", "test", "-count=1", "-json", "./..."}) ||
		len(p.Gate.Environments) != 1 || p.Gate.Environments[0].Facts.Network != "none" ||
		len(p.Toolchain.Images) != 1 || p.Toolchain.Images["go"] != self.Toolchain.Images["go"] ||
		!slices.Equal(p.Toolchain.DependencyRecipe.Lockfiles, []string{"go.mod"}) {
		t.Fatalf("the profile: %+v", p)
	}
	// The stub benchmark's count rails (slice plan §2.6): 2 attempts per
	// round, 3 rounds per task, 2 merge operations per generation.
	if r := p.Budget.Rails; r.AttemptsPerRound == nil || *r.AttemptsPerRound != 2 || r.RoundsPerTask == nil || *r.RoundsPerTask != 3 ||
		r.MergeOperationsPerGeneration == nil || *r.MergeOperationsPerGeneration != 2 {
		t.Errorf("the count rails: %+v", r)
	}
	if _, err := os.Stat(filepath.Join("../../fixtures/t0/go-mini/repo", "go.mod")); err != nil {
		t.Fatalf("the repository: %v", err)
	}
}
