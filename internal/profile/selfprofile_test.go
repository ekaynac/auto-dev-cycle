// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// This repository's profile and its prepared image (P32).
const (
	selfProfile = "../../fixtures/m1/profiles/auto-dev-cycle.json"
	repoRoot    = "../.."
	prepare     = "../../scripts/prepare-go-image.sh"
)

func loadSelf(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(selfProfile)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The profile passes the schema, the project.create scope and Check, and
// holds what 00 §2.3 names: github, main, a fully qualified digest pin, the
// go_test_json gate offline, and the lockfiles P9 checks.
func TestSelfProfile(t *testing.T) {
	raw := loadSelf(t)
	if err := contract.Validate("api/scope/project-create", raw); err != nil {
		t.Fatalf("the project.create scope: %v", err)
	}
	p, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "prj_01m4tmb43hefctwv52nmq34fdj" || p.Name != "auto-dev-cycle" || p.Forge.Kind != "github" ||
		p.Repo.BaseRef != "refs/heads/main" || p.Gate.ResultAdapter != results.GoTestJSON ||
		!slices.Equal(p.Gate.Command[:3], []string{"go", "test", "-json"}) ||
		len(p.Gate.Environments) != 1 || p.Gate.Environments[0].Facts.Network != "none" ||
		!slices.Equal(p.Toolchain.DependencyRecipe.Lockfiles, []string{"go.mod", "go.sum"}) {
		t.Fatalf("the profile: %+v", p)
	}
	// The sandbox clones https, ssh or an absolute path, never file:// (#226 entry 28).
	if !strings.HasPrefix(p.Repo.Url, "https://") {
		t.Fatalf("repo.url %q", p.Repo.Url)
	}
	// Pins are fully qualified: a registry host, then a digest (#226 entry 19).
	img, ok := p.Toolchain.Images["go"]
	registry, _, _ := strings.Cut(string(img), "/")
	if len(p.Toolchain.Images) != 1 || !ok || !strings.ContainsAny(registry, ".:") || !strings.Contains(string(img), "@sha256:") {
		t.Fatalf("toolchain.images %v", p.Toolchain.Images)
	}
}

// Every path of 00 §2.2 criterion 2, the contract packages of 04 §3, the
// lockfiles (00 §8 item 2), the recipe and the adapter are protected; an
// ordinary package is not.
func TestSelfProfileProtectedPaths(t *testing.T) {
	p, err := Load(loadSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	var globs [][]string
	for _, g := range p.ProtectedPaths {
		segs, err := compileGlob(string(g))
		if err != nil {
			t.Fatal(err)
		}
		globs = append(globs, segs)
	}
	protected := func(path string) bool { return anyMatches(globs, strings.Split(path, "/")) }
	for _, path := range []string{
		"schemas/config/project-profile.json", "gen/go/types/x.go", "spec/knowledge/a.md", "docs/plan/04-x.md",
		"docs/design/m1/00-slice-plan.md", ".github/workflows/ci.yml", "Makefile", "scripts/test-sandbox.sh",
		"scripts/test-sandbox-selftest.sh", "internal/authority/a.go", "internal/eventstore/a.go",
		"internal/capacity/a.go", "internal/attempt/a.go", "internal/receipt/a.go", "internal/sandbox/image.go",
		"internal/adapterhost/wire/a.go", "adapters/contract/src/a.ts", "go.mod", "go.sum",
		"scripts/bootstrap-go.sh", "scripts/prepare-go-image.sh", "fixtures/m1/images/go/Containerfile",
		"fixtures/m1/issues/170/task.json", "internal/results/gotest.go", "internal/testguard/fakes/x.go",
		"internal/profile/profile.go", "internal/contract/contract.go",
	} {
		if !protected(path) {
			t.Errorf("%s is not protected", path)
		}
	}
	for _, path := range []string{"internal/cli/cli.go", "internal/api/projects.go", "README.md"} {
		if protected(path) {
			t.Errorf("%s is protected", path)
		}
	}
}

// The recipe's manifest generator agrees with P9's check against this
// repository's lockfiles under the profile's toolchain key, and the check
// refuses it once a lockfile differs.
func TestPreparedManifestMatchesP9(t *testing.T) {
	p, err := Load(loadSelf(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", prepare, "manifest", repoRoot).Output()
	if err != nil {
		t.Fatalf("the generator: %v", err)
	}
	var img sandbox.Image
	if err := json.Unmarshal(out, &img.Manifest); err != nil {
		t.Fatalf("the label %q: %v", out, err)
	}
	lockfiles := p.Toolchain.DependencyRecipe.Lockfiles
	if err := img.CheckManifest("go", repoRoot, lockfiles); err != nil {
		t.Fatal(err)
	}

	// A base whose go.sum differs is refused (the discriminating control).
	base := t.TempDir()
	for _, f := range lockfiles {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "go.sum" {
			b = append(b, "example.com/x v1.0.0 h1:AAAA=\n"...)
		}
		if err := os.WriteFile(filepath.Join(base, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := img.CheckManifest("go", base, lockfiles); err == nil {
		t.Fatal("a base with another go.sum passed the manifest check")
	}
}
