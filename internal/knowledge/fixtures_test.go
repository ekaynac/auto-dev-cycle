// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"go.yaml.in/yaml/v3"
)

const specDir = "../../spec/knowledge/"

// The fixture definitions of the shared spec (docs/design/m0/10 "Shared
// knowledge spec draft") are definitions only in M0; their shape is checked
// here so later milestones can implement them.
func TestFixtureDefinitions(t *testing.T) {
	b, err := os.ReadFile(specDir + "fixtures/definitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fixtures []struct {
			ID         string   `yaml:"id"`
			Title      string   `yaml:"title"`
			Invariants []int    `yaml:"invariants"`
			Milestone  string   `yaml:"milestone"`
			Given      []string `yaml:"given"`
			When       string   `yaml:"when"`
			Then       []string `yaml:"then"`
		} `yaml:"fixtures"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	inv, err := os.ReadFile(specDir + "invariants.md")
	if err != nil {
		t.Fatal(err)
	}
	invariants := map[int]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(\d+)\. \*\*`).FindAllStringSubmatch(string(inv), -1) {
		n, _ := strconv.Atoi(m[1])
		invariants[n] = true
	}
	if len(invariants) == 0 {
		t.Fatal("no numbered invariants found")
	}
	defined := map[string]bool{}
	for _, f := range doc.Fixtures {
		switch {
		case defined[f.ID]:
			t.Errorf("%s: defined twice", f.ID)
		case f.Title == "" || f.When == "" || len(f.Given) == 0 || len(f.Then) == 0:
			t.Errorf("%s: title, given, when and then are all required", f.ID)
		case !slices.Contains([]string{"M1", "M2", "M4"}, f.Milestone):
			t.Errorf("%s: milestone %q; each check belongs to M1, M2 or M4", f.ID, f.Milestone)
		case len(f.Invariants) == 0:
			t.Errorf("%s: names no invariant", f.ID)
		}
		for _, n := range f.Invariants {
			if !invariants[n] {
				t.Errorf("%s: invariant %d does not exist", f.ID, n)
			}
		}
		defined[f.ID] = true
	}
	// Note 10's required cases (revocation between selection and delivery
	// is split into before and after the intent is committed) and the KA1
	// disclosure controls.
	for _, id := range []string{"stale-view-after-finding-change", "same-bytes-different-lineage", "on-prem-lesson-to-open-project",
		"revocation-before-intent", "revocation-after-intent", "poisoned-builder-note", "real-but-irrelevant-citation",
		"compaction-turn", "declared-only-coverage", "mixed-project-handover", "refactor-under-anchored-item",
		"opaque-evidence-pointer", "automatic-versus-prose-export"} {
		if !defined[id] {
			t.Errorf("required fixture %s is not defined", id)
		}
	}
	// Every fixture the invariants mention is defined.
	for _, line := range regexp.MustCompile(`(?m)\*Fixtures?:\*.*$`).FindAllString(string(inv), -1) {
		for _, m := range regexp.MustCompile("`([a-z0-9-]+)`").FindAllStringSubmatch(line, -1) {
			if !defined[m[1]] {
				t.Errorf("invariants.md mentions fixture %s, which is not defined", m[1])
			}
		}
	}
}
