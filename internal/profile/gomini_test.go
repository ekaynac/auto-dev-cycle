// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// go-mini, checkpoint (a)'s stub project (#226 entry 94): its profile passes
// the schema, the project.create scope and Check; it pins go-mini's own
// prepared Go image (#226 entry 97), never this repository's; it gates T0
// go-mini offline with go_test_json, and its remote is an absolute path the
// sandbox clones (#226 entry 28).
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
		len(p.Toolchain.Images) != 1 || p.Toolchain.Images["go"] != goMiniImage || p.Toolchain.Images["go"] == self.Toolchain.Images["go"] ||
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

// goMiniImage is go-mini's own prepared image (#226 entry 97): built from
// main 842dde6 by scripts/prepare-go-image.sh build go-mini, tag
// go1.27.1-2026-10-21, and goMiniLabel its dev.adc.prepared label as
// pushed (podman image inspect).
const (
	goMiniImage = "ghcr.io/ekaynac/adc-prepared-go-mini@sha256:ee7758f5af72529c67bc34965bab43dc6dbbff19cc5ae69c315ce1f25f4e113d"
	goMiniLabel = `{"toolchain":"go","lockfiles":{"go.mod":"sha256:4c74b377c603a5b4a85eaed95d1f5709a6183175286d47321649fc5af9519f04"}}`
)

// go-mini's prepared image (#226 entry 97): the recipe's label for go-mini
// is the pinned image's, and passes P9's check against T0's repo/ under the
// profile's toolchain key and lockfiles; this repository's label is refused
// for it, and go-mini's for this repository (the discriminating controls).
func TestGoMiniPreparedManifest(t *testing.T) {
	raw, err := os.ReadFile("../../fixtures/m1/profiles/go-mini.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	label := func(project string) sandbox.Image {
		out, err := exec.Command("bash", prepare, "manifest", project).Output()
		if err != nil {
			t.Fatalf("the generator for %s: %v", project, err)
		}
		var img sandbox.Image
		if err := json.Unmarshal(out, &img.Manifest); err != nil {
			t.Fatalf("the label %q: %v", out, err)
		}
		return img
	}
	const repo = "../../fixtures/t0/go-mini/repo"
	goMini, self := label("go-mini"), label("auto-dev-cycle")
	lockfiles := p.Toolchain.DependencyRecipe.Lockfiles
	if err := goMini.CheckManifest("go", repo, lockfiles); err != nil {
		t.Errorf("go-mini's label for go-mini: %v", err)
	}
	var pushed sandbox.Image
	if err := json.Unmarshal([]byte(goMiniLabel), &pushed.Manifest); err != nil || !reflect.DeepEqual(pushed.Manifest, goMini.Manifest) {
		t.Errorf("the pinned image's label %+v, the recipe's %+v: %v", pushed.Manifest, goMini.Manifest, err)
	}
	if err := pushed.CheckManifest("go", repo, lockfiles); err != nil {
		t.Errorf("the pinned image's label for go-mini: %v", err)
	}
	if err := self.CheckManifest("go", repo, lockfiles); err == nil {
		t.Error("this repository's label passed for go-mini")
	}
	selfProfile, err := Load(loadSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := goMini.CheckManifest("go", repoRoot, selfProfile.Toolchain.DependencyRecipe.Lockfiles); err == nil {
		t.Error("go-mini's label passed for this repository")
	}
	if out, err := exec.Command("bash", prepare, "manifest", "another").CombinedOutput(); err == nil {
		t.Errorf("an unknown project: %s", out)
	}
}
