// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// helpersBase is P14d-w1's test file: a named t.Run callback with one
// assertion, a helper with two, a method helper, a function literal a var
// holds, a constant switch's default, and tests using them.
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

func outer(t *testing.T) {
	inner(t)
}

func inner(t *testing.T) {
	if Add(4, 0) != 4 {
		t.Fatal("inner")
	}
}

func TestOuter(t *testing.T) { outer(t) }

func five(t *testing.T) {
	if Add(5, 0) != 5 {
		t.Fatal("five")
	}
	if Add(5, 1) != 6 {
		t.Fatal("six")
	}
	if Add(5, 2) != 7 {
		t.Fatal("seven")
	}
}

func TestFive(t *testing.T) { five(t) }

func four(t *testing.T) {
	if Add(4, 1) != 5 {
		t.Fatal("4+1")
	}
	if Add(4, 2) != 6 {
		t.Fatal("4+2")
	}
	if Add(4, 3) != 7 {
		t.Fatal("4+3")
	}
}

func TestFour(t *testing.T) { four(t) }

func TestOther(t *testing.T) { four(t) }

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

var checkVar = func(t *testing.T) {
	if Add(7, 0) != 7 {
		t.Fatal("var")
	}
}

func checkDefault(t *testing.T) {
	switch false {
	case true:
	default:
		if Add(1, 2) != 3 {
			t.Fatal("check")
		}
	}
}

func TestVar(t *testing.T) {
	checkVar(t)
	checkDefault(t)
}
`

// aliasFile is Astra #369 H1's input: a test reaches check through a var.
const aliasFile = `package pkg

import "testing"

var callback = check

func TestAlias(t *testing.T) { t.Run("check", callback) }

func check(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("first")
	}
	if Add(2, 2) != 4 {
		t.Fatal("second")
	}
}
`

// cycleFile is Astra #369 H2's input: two helpers calling each other.
const cycleFile = `package pkg

import "testing"

func a(t *testing.T, n int) {
	if Add(1, 2) != 3 {
		t.Fatal("a")
	}
	if n > 0 {
		b(t, n-1)
	}
}

func b(t *testing.T, n int) {
	if Add(2, 2) != 4 {
		t.Fatal("b")
	}
	if n > 0 {
		a(t, n-1)
	}
}

func TestCycle(t *testing.T) { a(t, 1) }
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

// moreFile is Opus #376's inputs: helpers renamed (through a var, a t.Run
// name, a direct call), a table of closures, and testing values reached
// through a field and an embedding.
const moreFile = `package pkg

import "testing"

var renVar = renCase

func renCase(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("ren")
	}
}

func TestRenVar(t *testing.T) { t.Run("a", renVar) }

func renRun(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("run")
	}
}

func TestRenRun(t *testing.T) { t.Run("a", renRun) }

func renTwo(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("two")
	}
	if Add(2, 2) != 4 {
		t.Fatal("two2")
	}
}

func TestRenTwo(t *testing.T) { renTwo(t) }

func TestRenTwoB(t *testing.T) { renTwo(t) }

func outer2(t *testing.T) {
	inner2(t)
	renRun(t)
}

func inner2(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("in1")
	}
	if Add(2, 2) != 4 {
		t.Fatal("in2")
	}
}

func TestOuter2(t *testing.T) { outer2(t) }

var table = []struct {
	name string
	f    func(*testing.T)
}{{"one", func(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("one")
	}
}}}

func TestTable(t *testing.T) {
	for _, c := range table {
		t.Run(c.name, c.f)
	}
}

type suite struct{ t *testing.T }

func (s suite) check() {
	if Add(1, 2) != 3 {
		s.t.Fatal("suite")
	}
}

type emb struct{ *testing.T }

func (e *emb) check() {
	if Add(1, 2) != 3 {
		e.Fatal("emb")
	}
}

func TestSuite(t *testing.T) {
	suite{t}.check()
	(&emb{t}).check()
}

type own struct{ *testing.T }

func (own) Fatal(...any) {}

func (o own) check() {
	if Add(1, 2) != 3 {
		o.Fatal("own")
	}
}

func TestOwn(t *testing.T) { own{t}.check() }
`

// TestIntegrityHelpers (P14d-w1, #226 entries 122, 129): a function of
// the test files whose own direct assertions the head reduces blocks,
// whether a test reaches it or not; an assertion under a branch a constant
// condition never takes is removed; a gain is clean; a function removed
// blocks while its name is still used. Calls are not followed: the stated
// false blocks and silent forms have rows too.
func TestIntegrityHelpers(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/lib.go": libFile, "pkg/helpers_test.go": helpersBase, "pkg/alias_test.go": aliasFile, "pkg/cycle_test.go": cycleFile, "pkg/more_test.go": moreFile,
			"pkg/race_h_test.go": raceNeedX("race", 2), "pkg/norace_h_test.go": raceNeedX("!race", 1), "pkg/race_r_test.go": raceTestR("race"), "pkg/norace_r_test.go": raceTestR("!race")})
		plain(t, src, "add", "--all")
	})
	edit := func(old, new string) map[string]string {
		if !strings.Contains(helpersBase, old) {
			t.Fatalf("no %q in the base", old)
		}
		return map[string]string{"pkg/helpers_test.go": strings.Replace(helpersBase, old, new, 1)}
	}
	in := func(file, base, old, new string) map[string]string {
		if !strings.Contains(base, old) {
			t.Fatalf("no %q in %s", old, file)
		}
		return map[string]string{file: strings.Replace(base, old, new, 1)}
	}
	inner2Base := "func outer2(t *testing.T) {\n\tinner2(t)\n\trenRun(t)\n}\n\nfunc inner2(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"in1\")\n\t}\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"in2\")\n\t}\n}\n"
	inner2Head := "func outer2(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"in1\")\n\t}\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"in2\")\n\t}\n\trenRun(t)\n}\n"
	fives := []string{"\tif Add(5, 0) != 5 {\n\t\tt.Fatal(\"five\")\n\t}\n", "\tif Add(5, 1) != 6 {\n\t\tt.Fatal(\"six\")\n\t}\n", "\tif Add(5, 2) != 7 {\n\t\tt.Fatal(\"seven\")\n\t}\n"}
	one := "\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n"
	weakened := "a helper's assertions weakened"
	for name, c := range map[string]struct {
		files map[string]string
		block string // "": accepted with no warning
	}{
		"a named callback's assertion removed":                 {edit("func checkCase(t *testing.T) {\n"+one, "func checkCase(t *testing.T) {\n"), "pkg:pkg.checkCase " + weakened + " (1 → 0)"},
		"one of a helper's two removed":                        {edit("func checkTwo(t *testing.T) {\n"+one, "func checkTwo(t *testing.T) {\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"if false around an assertion":                         {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tif false {\n\t\tt.Fatal(\"add2\")\n\t}\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"for false around an assertion":                        {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tfor false {\n\t\tt.Fatal(\"add2\")\n\t}\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"a dead switch clause":                                 {edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tswitch {\n\tcase false:\n\t\tt.Fatal(\"add2\")\n\t}\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 1)"},
		"a method helper's assertion removed":                  {edit("\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n", "\t_ = t\n"), "pkg:pkg.env.check " + weakened + " (1 → 0)"},
		"a var's function literal weakened":                    {edit("\tif Add(7, 0) != 7 {\n\t\tt.Fatal(\"var\")\n\t}\n", "\t_ = t\n"), "pkg:pkg.checkVar " + weakened + " (1 → 0)"},
		"a test's own assertion made dead":                     {map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "if Add(1, 2) != 3", "if false", 1)}, "a test's assertions made dead (1 → 0 live)"},
		"a helper removed while still used":                    {map[string]string{"pkg/helpers_test.go": strings.NewReplacer("func checkCase(t *testing.T) {\n"+one+"}\n", "", "t.Run(\"a\", checkCase)", "checkCase := checkTwo\n\tt.Run(\"a\", checkCase)").Replace(helpersBase)}, "pkg:pkg.checkCase a helper with assertions removed while still used"},
		"a helper gains an assertion (clean)":                  {edit("func checkCase(t *testing.T) {\n", "func checkCase(t *testing.T) {\n\tif Add(0, 1) != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n"), ""},
		"a helper's assertion rewritten (clean)":               {edit("Add(2, 2) != 4", "Add(3, 1) != 4"), ""},
		"another file only (control)":                          {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		"a !race variant's helper weakened":                    {map[string]string{"pkg/norace_h_test.go": raceNeedX("!race", 0)}, "pkg:pkg.needX " + weakened + " (1 → 0)"}, // the weakest declaration's count
		"a caller loses, a callee gains":                       {map[string]string{"pkg/helpers_test.go": strings.NewReplacer("\tif Add(1, 0) != 1 {\n\t\tt.Fatal(\"mid\")\n\t}\n", "", "func leaf(t *testing.T) {\n", "func leaf(t *testing.T) {\n\tif Add(2, 0) != 2 {\n\t\tt.Fatal(\"more\")\n\t}\n").Replace(helpersBase)}, "pkg:pkg.mid " + weakened + " (1 → 0)"},
		"a helper inlined into its caller and deleted (clean)": {map[string]string{"pkg/helpers_test.go": strings.NewReplacer("\tinner(t)\n", "\tif Add(4, 0) != 4 {\n\t\tt.Fatal(\"inner\")\n\t}\n", "func inner(t *testing.T) {\n\tif Add(4, 0) != 4 {\n\t\tt.Fatal(\"inner\")\n\t}\n}\n", "").Replace(helpersBase)}, ""},
		"an unused helper deleted (clean)":                     {edit("func spare(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"spare\")\n\t}\n}\n", ""), ""},
		// Opus #369 r2 H1: either variant of a test made dead blocks.
		"a !race variant of a test made dead": {map[string]string{"pkg/norace_r_test.go": strings.Replace(raceTestR("!race"), "\tif Add(1, 2) != 3 {", "\tif false {", 1)}, "pkg:pkg.TestR a test's assertions made dead (1 → 0 live)"},
		"a race variant of a test made dead":  {map[string]string{"pkg/race_r_test.go": strings.Replace(raceTestR("race"), "\tif Add(1, 2) != 3 {", "\tif false {", 1)}, "pkg:pkg.TestR a test's assertions made dead (1 → 0 live)"},
		// Astra #369 H1: a helper reached through a var blocks at the helper.
		"Astra's alias input": {in("pkg/alias_test.go", aliasFile, "\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"second\")\n\t}\n", ""), "pkg:pkg.check " + weakened + " (2 → 1)"},
		// Astra #369 H2: b's own count falling blocks at b.
		"Astra's cycle input, b's own assertion removed": {in("pkg/cycle_test.go", cycleFile, "\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"b\")\n\t}\n", ""), "pkg:pkg.b " + weakened + " (1 → 0)"},
		// Astra #369 H4: a default another clause must match is dead.
		"Astra's default input": {edit("\tswitch false {\n", "\tswitch true {\n"), "pkg:pkg.checkDefault " + weakened + " (1 → 0)"},
		// Opus #376 H1: a helper renamed while weakened blocks, through a
		// var, a t.Run name or a direct call; renamed whole is clean.
		"a helper renamed weaker, through a var":                      {in("pkg/more_test.go", moreFile, "var renVar = renCase\n\nfunc renCase(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"ren\")\n\t}\n}\n", "var renVar = renCase2\n\nfunc renCase2(t *testing.T) {}\n"), "pkg:pkg.renCase a helper renamed or replaced, weaker (1 → 0)"},
		"a helper renamed whole, through a var (clean)":               {in("pkg/more_test.go", moreFile, "var renVar = renCase\n\nfunc renCase(t *testing.T) {\n", "var renVar = renCase2\n\nfunc renCase2(t *testing.T) {\n"), ""},
		"a helper renamed weaker, a t.Run name":                       {map[string]string{"pkg/more_test.go": strings.NewReplacer("func renRun(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"run\")\n\t}\n}\n", "func renRun2(t *testing.T) {}\n", "t.Run(\"a\", renRun)", "t.Run(\"a\", renRun2)", "\trenRun(t)\n", "\trenRun2(t)\n").Replace(moreFile)}, "pkg:pkg.renRun a helper renamed or replaced, weaker (1 → 0)"},
		"a helper renamed weaker, a direct call":                      {map[string]string{"pkg/more_test.go": strings.NewReplacer("func renTwo(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"two\")\n\t}\n", "func renOne(t *testing.T) {\n", "renTwo(t) }", "renOne(t) }").Replace(moreFile)}, "pkg:pkg.renTwo a helper renamed or replaced, weaker (2 → 1)"},
		"a helper inlined beside an existing one and deleted (clean)": {in("pkg/more_test.go", moreFile, inner2Base, inner2Head), ""},
		// Opus #376 M2: a var's whole value, a table of closures.
		"a table of closures weakened": {in("pkg/more_test.go", moreFile, "\t\tt.Fatal(\"one\")\n", "\t\t_ = t\n"), "pkg:pkg.table " + weakened + " (1 → 0)"},
		// Opus #376 M1: a testing value through a field or an embedding.
		"a suite field's assertion removed":                     {in("pkg/more_test.go", moreFile, "\t\ts.t.Fatal(\"suite\")\n", "\t\t_ = s\n"), "pkg:pkg.suite.check " + weakened + " (1 → 0)"},
		"a struct's own Fatal method, its call removed (clean)": {in("pkg/more_test.go", moreFile, "\t\to.Fatal(\"own\")\n", "\t\t_ = o\n"), ""},
		"an embedded value's assertion removed":                 {in("pkg/more_test.go", moreFile, "\t\te.Fatal(\"emb\")\n", "\t\t_ = e\n"), "pkg:pkg.emb.check " + weakened + " (1 → 0)"},
		// Stated false blocks (main accepts each).
		"a function replaced by a var naming another (stated false block)":     {edit("func checkCase(t *testing.T) {\n"+one+"}\n", "var checkCase = checkTwo\n"), "pkg:pkg.checkCase " + weakened + " (1 → 0)"},
		"a body extracted into a new helper (stated false block)":              {edit("func checkTwo(t *testing.T) {\n", "func checkTwo(t *testing.T) { checkTwoAt(t) }\n\nfunc checkTwoAt(t *testing.T) {\n"), "pkg:pkg.checkTwo " + weakened + " (2 → 0)"},
		"an unused helper weakened (stated false block)":                       {edit("\t\tt.Fatal(\"unused\")\n", "\t\t_ = t\n"), "pkg:pkg.unused " + weakened + " (1 → 0)"},
		"an assertion moved into a package file's helper (stated false block)": {edit("\tif Add(3, 0) != 3 {\n\t\tt.Fatal(\"via\")\n\t}\n", "\tlibCheck(t)\n"), "pkg:pkg.viaLib " + weakened + " (1 → 0)"},
		// Silent, stated (P14d-w2): calls are not followed.
		"Astra's cycle input, a's call to b removed (silent, stated)": {in("pkg/cycle_test.go", cycleFile, "\tif n > 0 {\n\t\tb(t, n-1)\n\t}\n", "\t_ = n\n"), ""},
	} {
		v := f.guarded(t, c.files)
		switch {
		case c.block != "" && !finds(v.Blocks, c.block):
			t.Errorf("%s: blocks %q, want %q", name, whats(v.Blocks), c.block)
		case c.block == "" && (!v.Accepted() || len(v.Warnings) > 0):
			t.Errorf("%s: blocks %q, warnings %q, want accepted clean", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// A test replacing a helper call with fewer inline assertions: accepted
	// here (P14d-w2), P14d-b1's print warns (stated).
	for name, files := range map[string]map[string]string{
		"TestFour": edit("func TestFour(t *testing.T) { four(t) }\n", "func TestFour(t *testing.T) {\n\tif Add(4, 1) != 5 {\n\t\tt.Fatal(\"4+1\")\n\t}\n}\n"),
		"TestFive": {"pkg/helpers_test.go": strings.NewReplacer("func TestFive(t *testing.T) { five(t) }\n", "func TestFive(t *testing.T) {\n"+fives[0]+"}\n", "func five(t *testing.T) {\n"+strings.Join(fives, "")+"}\n", "").Replace(helpersBase)},
	} {
		if v := f.guarded(t, files); !v.Accepted() || !finds(v.Warnings, "pkg:pkg."+name+" an assertion, its condition or a test case changed") {
			t.Errorf("%s's helper call inlined weaker: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// A test is main's to compare: its assertion removed (not made dead) is
	// main's block only (Opus #369 L2).
	v := f.guarded(t, map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n", "\t_ = Add(1, 2)\n", 1)})
	if !finds(v.Blocks, "pkg:pkg.TestAdd fewer assertions in a test") || finds(v.Blocks, "made dead") || finds(v.Blocks, weakened) {
		t.Errorf("a test's assertion removed: %q", whats(v.Blocks))
	}
	// A default fallen into from a clause that must match: kept live, fail
	// closed, and warned (Astra #369 H4's fallthrough).
	v = f.guarded(t, edit("\tswitch false {\n\tcase true:\n\tdefault:\n", "\tswitch true {\n\tcase true:\n\t\tfallthrough\n\tdefault:\n"))
	if !v.Accepted() || !finds(v.Warnings, "pkg:pkg.checkDefault a switch clause never selected but fallen into, kept live") {
		t.Errorf("a default fallen into: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// So is any clause never selected that the one before falls into.
	v = f.guarded(t, edit("\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"add2\")\n\t}\n", "\tswitch {\n\tcase true:\n\t\tfallthrough\n\tcase false:\n\t\tt.Fatal(\"add2\")\n\t}\n"))
	if !v.Accepted() || !finds(v.Warnings, "pkg:pkg.checkTwo a switch clause never selected but fallen into, kept live") {
		t.Errorf("a clause fallen into: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
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

// raceTestR is TestR's variant for build tag tag, with one assertion.
func raceTestR(tag string) string {
	return "//go:build " + tag + "\n\npackage pkg\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"r\")\n\t}\n}\n"
}

// TestIntegrityHelpersDeterministic (Opus #369 H2, Astra #369 H3): an
// unchanged chain of 200 helpers, in a test file and in a package file
// (a named callback calling each), under a comment-only change, is
// accepted, every run alike: main's map-order fixed point is never read.
func TestIntegrityHelpersDeterministic(t *testing.T) {
	var b, lib strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc e200(t *testing.T) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"e\")\n\t}\n}\n")
	lib.WriteString("package pkg\n\nimport \"testing\"\n\nfunc pe200(t testing.TB) {\n\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"e\")\n\t}\n}\n")
	for i := 199; i >= 0; i-- {
		fmt.Fprintf(&b, "\nfunc e%d(t *testing.T) { e%d(t) }\n", i, i+1)
		fmt.Fprintf(&lib, "\nfunc pe%d(t testing.TB) { pe%d(t) }\n", i, i+1)
	}
	b.WriteString("\nfunc TestE(t *testing.T) { e0(t) }\n\nfunc hp(t *testing.T) {\n")
	for i := range 201 {
		fmt.Fprintf(&b, "\tpe%d(t)\n", i)
	}
	b.WriteString("}\n\nfunc TestChain(t *testing.T) { t.Run(\"chain\", hp) }\n")
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/chain_test.go": b.String(), "pkg/chain.go": lib.String()})
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
