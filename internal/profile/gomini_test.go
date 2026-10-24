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

// goMiniImage is go-mini's own prepared image (#226 entries 97 and 194):
// built from main 8adbed7a by scripts/prepare-go-image.sh build go-mini,
// for linux/arm64 only, and goMiniLabel its dev.adc.prepared label as
// pushed (podman image inspect): the recipe's lockfile part and the arm64
// adapter digest, the release of 8adbed7a's.
const (
	goMiniImage = "ghcr.io/ekaynac/adc-prepared-go-mini@sha256:66b8cd3a5aaee0cc9d6007e226f37e07d258b82edc49d91891048ae242268a31"
	goMiniLabel = `{"toolchain":"go","lockfiles":{"go.mod":"sha256:4c74b377c603a5b4a85eaed95d1f5709a6183175286d47321649fc5af9519f04"},"adapter":{"linux/arm64":"sha256:f2143fea3a47fda98a327353f8c8f3e70b58ddc5d9cfaf45bbc40e41f4ac83aa"}}`
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
	// The recipe's manifest is the label's lockfile part; the pushed label
	// adds each built platform's adapter digest (P22a-5d). The pin is built
	// for linux/arm64 alone (#226 entries 185 and 194): its label names that one
	// platform's adapter, and no amd64 entry. An amd64 controller fails
	// closed: the pinned list has no amd64 image to resolve, and an image's
	// adapter is checked only against its own platform's entry
	// (sandbox.Image.CheckAdapter), which an amd64 image would lack.
	err = json.Unmarshal([]byte(goMiniLabel), &pushed.Manifest)
	lockPart := pushed.Manifest
	lockPart.Adapter = nil
	if _, amd64 := pushed.Manifest.Adapter["linux/amd64"]; err != nil || !reflect.DeepEqual(lockPart, goMini.Manifest) || len(pushed.Manifest.Adapter) != 1 ||
		pushed.Manifest.Adapter["linux/arm64"] == "" || amd64 {
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
