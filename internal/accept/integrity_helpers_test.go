// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"slices"
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

func unused(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("unused")
	}
}

func mid(t *testing.T) {
	if Add(1, 0) != 1 {
		t.Fatal("mid")
	}
	leaf(t)
}

func leaf(t *testing.T) {
	if Add(0, 1) != 1 {
		t.Fatal("leaf")
	}
}

func TestMid(t *testing.T) { mid(t) }

func viaLib(t *testing.T) {
	if Add(3, 0) != 3 {
		t.Fatal("via")
	}
}

func TestViaLib(t *testing.T) { viaLib(t) }

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

// libFile is a package file with an asserting helper.
const libFile = `package pkg

import "testing"

func libCheck(t testing.TB) {
	if Add(1, 1) != 2 {
		t.Fatal("lib")
	}
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
		write(t, src, map[string]string{"pkg/lib.go": libFile, "pkg/helpers_test.go": helpersBase, "pkg/race_h_test.go": raceNeedX("race", 2), "pkg/norace_h_test.go": raceNeedX("!race", 1)})
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
		"a test's own assertion made dead":       {map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "if Add(1, 2) != 3", "if false", 1)}, "a test's assertions made dead (1 → 0 live)"},
		"a helper removed while still used":      {edit("func checkCase(t *testing.T) {\n"+one+"}\n", "var checkCase = func(t *testing.T) {}\n"), "a helper with assertions removed while still used"},
		"a helper gains an assertion (clean)":    {edit("func checkCase(t *testing.T) {\n", "func checkCase(t *testing.T) {\n\tif Add(0, 1) != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n"), ""},
		"a helper's assertion rewritten (clean)": {edit("Add(2, 2) != 4", "Add(3, 1) != 4"), ""},
		"another file only (control)":            {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		// The replay's refactor (openEffectStore → openEffectStoreAt): a body
		// extracted into a new helper it calls keeps its count.
		"a body extracted into a new helper (clean)": {edit("func checkTwo(t *testing.T) {\n", "func checkTwo(t *testing.T) { checkTwoAt(t) }\n\nfunc checkTwoAt(t *testing.T) {\n"), ""},
		"extracted, then weakened":                   {edit("func checkTwo(t *testing.T) {\n"+one, "func checkTwo(t *testing.T) { checkTwoAt(t) }\n\nfunc checkTwoAt(t *testing.T) {\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		// Opus #369 M1: a caller's lost assertion, a callee's gain: blocks.
		"a caller loses, a callee gains (M1)": {map[string]string{"pkg/helpers_test.go": strings.NewReplacer("\tif Add(1, 0) != 1 {\n\t\tt.Fatal(\"mid\")\n\t}\n", "", "func leaf(t *testing.T) {\n", "func leaf(t *testing.T) {\n\tif Add(2, 0) != 2 {\n\t\tt.Fatal(\"more\")\n\t}\n").Replace(helpersBase)}, "pkg:pkg.mid " + weakened + " (2 → 1)"},
		// main counts an asserting helper of a package file as one: so does this
		"an assertion replaced by a package file's asserting helper (clean)": {edit("\tif Add(3, 0) != 3 {\n\t\tt.Fatal(\"via\")\n\t}\n", "\tlibCheck(t)\n"), ""},
		"an unused helper weakened (L1, clean)":                              {edit("\t\tt.Fatal(\"unused\")\n", "\t\t_ = t\n"), ""},
		"an unused helper deleted (clean)":                                   {edit("func spare(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"spare\")\n\t}\n}\n", ""), ""},
		"a !race variant's helper weakened":                                  {map[string]string{"pkg/norace_h_test.go": raceNeedX("!race", 0)}, "pkg:pkg.needX " + weakened + " (1 → 0)"}, // the weakest declaration's count
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

// TestIntegrityHelpersSaturate (Opus #369 H1): duplicated calls multiply a
// total; past maxHelperTotal the helper is unverifiable, never compared
// (an overflow would read a weakening as a gain).
func TestIntegrityHelpersSaturate(t *testing.T) {
	old := maxHelperTotal
	t.Cleanup(func() { maxHelperTotal = old })
	maxHelperTotal = 1000
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc c4(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"c\")\n\t}\n}\n")
	for i := 3; i >= 1; i-- {
		fmt.Fprintf(&b, "\nfunc c%d(t *testing.T) {\n%s}\n", i, strings.Repeat(fmt.Sprintf("\tc%d(t)\n", i+1), 16))
	}
	b.WriteString("\nfunc bcheck(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"b\")\n\t}\n}\n\nfunc top(t *testing.T) {\n\tc1(t)\n\tbcheck(t)\n}\n\nfunc TestTop(t *testing.T) { top(t) }\n")
	base := b.String()
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/sat_test.go": base})
		plain(t, src, "add", "--all")
	})
	v := f.guarded(t, map[string]string{"pkg/sat_test.go": strings.Replace(base, "\tc1(t)\n\tbcheck(t)\n", "\tc1(t)\n", 1)})
	if !finds(v.Blocks, "pkg:pkg.top unverifiable: test helpers' totals past the guard's bound") {
		t.Errorf("a saturated total: %q", whats(v.Blocks))
	}

	// Opus's input, the default bound: c16 asserts once, each c_i calls
	// c_{i+1} 16 times (c1: 2⁶⁰), top calls c1 7 times and c2 … c16 15
	// times each (2⁶³ − 1), then bcheck. Unsaturated, top's base total
	// wrapped to MinInt64 and the head's, bcheck removed, read as a gain.
	maxHelperTotal = old
	b.Reset()
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc c16(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"c\")\n\t}\n}\n")
	for i := 15; i >= 1; i-- {
		fmt.Fprintf(&b, "\nfunc c%d(t *testing.T) {\n%s}\n", i, strings.Repeat(fmt.Sprintf("\tc%d(t)\n", i+1), 16))
	}
	b.WriteString("\nfunc bcheck(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"b\")\n\t}\n}\n\nfunc top(t *testing.T) {\n" + strings.Repeat("\tc1(t)\n", 7))
	for i := 2; i <= 16; i++ {
		b.WriteString(strings.Repeat(fmt.Sprintf("\tc%d(t)\n", i), 15))
	}
	b.WriteString("\tbcheck(t)\n}\n\nfunc TestTop(t *testing.T) { top(t) }\n")
	opus := b.String()
	f = newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/sat_test.go": opus})
		plain(t, src, "add", "--all")
	})
	v = f.guarded(t, map[string]string{"pkg/sat_test.go": strings.Replace(opus, "\tbcheck(t)\n}\n\nfunc TestTop", "}\n\nfunc TestTop", 1)})
	if !finds(v.Blocks, "pkg:pkg.top unverifiable: test helpers' totals past the guard's bound") || finds(v.Blocks, "weakened") {
		t.Errorf("Opus's overflow: %q", whats(v.Blocks))
	}
}

// TestIntegrityHelpersDeterministic (Opus #369 H2): an unchanged chain of
// 200 helpers under a comment-only change is accepted, every run alike:
// the calls are read from the syntax, never from main's map-order fixed
// point.
func TestIntegrityHelpersDeterministic(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc e200(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"e\")\n\t}\n}\n")
	for i := 199; i >= 0; i-- {
		fmt.Fprintf(&b, "\nfunc e%d(t *testing.T) { e%d(t) }\n", i, i+1)
	}
	b.WriteString("\nfunc TestE(t *testing.T) { e0(t) }\n")
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/chain_test.go": b.String()})
		plain(t, src, "add", "--all")
	})
	var first []string
	for run := range 5 {
		v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"})
		if !v.Accepted() || len(v.Warnings) > 0 {
			t.Errorf("run %d: blocks %q, warnings %q", run, whats(v.Blocks), whats(v.Warnings))
		}
		if got := whats(v.Blocks); run == 0 {
			first = got
		} else if !slices.Equal(got, first) {
			t.Errorf("run %d differs from run 0: %q, %q", run, got, first)
		}
	}
}
