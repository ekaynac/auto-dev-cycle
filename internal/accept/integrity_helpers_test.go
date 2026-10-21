// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"strings"
	"testing"
)

// helpersBase is P14d-w's test file: a named t.Run callback with one
// assertion, a helper with two, a method helper, and tests using them.
const helpersBase = `package pkg

import "testing"

func checkCase(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("add")
	}
}

func checkTwo(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("add")
	}
	if Add(2, 2) != 4 {
		t.Fatal("add2")
	}
}

func spare(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("spare")
	}
}

type env struct{}

func (env) check(t *testing.T) {
	if Add(0, 0) != 0 {
		t.Error("zero")
	}
}

func TestNamed(t *testing.T) { t.Run("a", checkCase) }

func TestDirect(t *testing.T) {
	checkTwo(t)
	env{}.check(t)
}
`

// TestIntegrityHelpers (P14d-w, #226 entry 122): a function of the test
// files whose own assertions the head weakens blocks, through a named
// t.Run callback or a direct call alike; an assertion under a branch a
// constant condition never takes is removed; a gain is clean; a function
// removed blocks while its name is still used.
func TestIntegrityHelpers(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/helpers_test.go": helpersBase, "pkg/race_h_test.go": raceNeedX("race", 2), "pkg/norace_h_test.go": raceNeedX("!race", 1)})
		plain(t, src, "add", "--all")
	})
	edit := func(old, new string) map[string]string {
		if !strings.Contains(helpersBase, old) {
			t.Fatalf("no %q in the base", old)
		}
		return map[string]string{"pkg/helpers_test.go": strings.Replace(helpersBase, old, new, 1)}
	}
	one := "\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n"
	weakened := "a helper's assertions weakened"
	for name, c := range map[string]struct {
		files map[string]string
		block string // "": accepted with no warning
	}{
		"a named callback's assertion removed":   {edit("func checkCase(t *testing.T) {\n"+one, "func checkCase(t *testing.T) {\n"), "pkg:pkg.checkCase " + weakened + " (1 → 0)"},
		"one of a helper's two removed":          {edit("func checkTwo(t *testing.T) {\n"+one, "func checkTwo(t *testing.T) {\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"if false around an assertion":           {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tif false {\n\t\tt.Fatal(\"add2\")\n\t}\n"), weakened + " (2 → 1)"},
		"for false around an assertion":          {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tfor false {\n\t\tt.Fatal(\"add2\")\n\t}\n"), weakened + " (2 → 1)"},
		"a dead switch clause":                   {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tswitch {\n\tcase false:\n\t\tt.Fatal(\"add2\")\n\t}\n"), weakened + " (2 → 1)"},
		"a method helper's assertion removed":    {edit("\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n", "\t_ = t\n"), "pkg:pkg.env.check " + weakened + " (1 → 0)"},
		"a test's own assertion made dead":       {map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "if Add(1, 2) != 3", "if false", 1)}, weakened},
		"a helper removed while still used":      {edit("func checkCase(t *testing.T) {\n"+one+"}\n", "var checkCase = func(t *testing.T) {}\n"), "a helper with assertions removed while still used"},
		"a helper gains an assertion (clean)":    {edit("func checkCase(t *testing.T) {\n", "func checkCase(t *testing.T) {\n\tif Add(0, 1) != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n"), ""},
		"a helper's assertion rewritten (clean)": {edit("Add(2, 2) != 4", "Add(3, 1) != 4"), ""},
		"another file only (control)":            {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		// The replay's refactor (openEffectStore → openEffectStoreAt): a body
		// extracted into a new helper it calls keeps its count.
		"a body extracted into a new helper (clean)": {edit("func checkTwo(t *testing.T) {\n", "func checkTwo(t *testing.T) { checkTwoAt(t) }\n\nfunc checkTwoAt(t *testing.T) {\n"), ""},
		"extracted, then weakened":                   {edit("func checkTwo(t *testing.T) {\n"+one, "func checkTwo(t *testing.T) { checkTwoAt(t) }\n\nfunc checkTwoAt(t *testing.T) {\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"an unused helper deleted (clean)":           {edit("func spare(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"spare\")\n\t}\n}\n", ""), ""},
		"a !race variant's helper weakened":          {map[string]string{"pkg/norace_h_test.go": raceNeedX("!race", 0)}, "pkg:pkg.needX " + weakened + " (1 → 0)"}, // the weakest declaration's count
	} {
		v := f.guarded(t, c.files)
		switch {
		case c.block != "" && !finds(v.Blocks, c.block):
			t.Errorf("%s: blocks %q, want %q", name, whats(v.Blocks), c.block)
		case c.block == "" && (!v.Accepted() || len(v.Warnings) > 0):
			t.Errorf("%s: blocks %q, warnings %q, want accepted clean", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
}

// TestIntegrityHelpersBound: the test files' declarations are charged to
// the case layer's bound; past it, the file is unverifiable.
func TestIntegrityHelpersBound(t *testing.T) {
	f := newGuardFixture(t)
	old := maxCaseWork
	t.Cleanup(func() { maxCaseWork = old })
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n")
	for i := range 20 {
		fmt.Fprintf(&b, "\nfunc helper%d(t *testing.T) {\n%s}\n", i, strings.Repeat("\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"x\")\n\t}\n", 200))
	}
	maxCaseWork = 1 << 30
	before := caseCharged.Load()
	if v := f.guarded(t, map[string]string{"pkg/helpers_test.go": b.String()}); !v.Accepted() {
		t.Fatalf("under the bound: %q", whats(v.Blocks))
	}
	charged := caseCharged.Load() - before
	if charged < int64(b.Len())/8 { // each node at least once (a node is a few bytes at most)
		t.Errorf("charged %d for %d bytes of helpers", charged, b.Len())
	}
	maxCaseWork = int(charged / 4)
	if v := f.guarded(t, map[string]string{"pkg/helpers_test.go": b.String()}); !finds(v.Blocks, "unverifiable: test helpers' assertions past the guard's work bound") {
		t.Errorf("past the bound: %q", whats(v.Blocks))
	}
}

// raceNeedX is needX under a build constraint with n assertions, and a
// test using it.
func raceNeedX(tag string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "//go:build %s\n\npackage pkg\n\nimport \"testing\"\n\nfunc needX(t *testing.T) {\n", tag)
	for i := range n {
		fmt.Fprintf(&b, "\tif Add(%d, 1) != %d {\n\t\tt.Fatal(\"x\")\n\t}\n", i, i+1)
	}
	b.WriteString("}\n\nfunc TestX(t *testing.T) { needX(t) }\n")
	return b.String()
}
