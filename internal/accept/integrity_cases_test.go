// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// casesBase is P14d-b's test file: tables, a map of subtests, nested
// subtests, closures, helpers and named callbacks with subtests, and
// conditions.
const casesBase = `package pkg

import "testing"

func TestTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		a, want int
	}{
		{"one", 1, 1},
		{"zero", 0, 0},
		{"neg", -1, -1},
	} {
		if Add(tc.a, 0) != tc.want {
			t.Errorf("%s", tc.name)
		}
	}
}

func TestMap(t *testing.T) {
	cases := map[string]int{"one": 1, "two": 2}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			if Add(n, 0) != n {
				t.Fatal(name)
			}
		})
	}
}

func TestSub(t *testing.T) {
	t.Run("outer", func(t *testing.T) {
		t.Run("inner", func(t *testing.T) {
			if Add(1, 1) != 2 {
				t.Fatal("inner")
			}
		})
		t.Run("other", func(t *testing.T) {
			if Add(2, 2) != 4 {
				t.Fatal("other")
			}
		})
	})
}

func TestPlain(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("add")
	}
}

func TestArgs(t *testing.T) {
	id := []string{"-c", "x"}
	for _, args := range [][]string{{"status"}, append(id, "commit", "-m")} {
		if len(args) == 0 {
			t.Fatal(args)
		}
	}
}

func TestLocal(t *testing.T) {
	tests := []struct {
		name string
		a    int
	}{{"x", 1}, {"y", 2}}
	for _, tc := range tests {
		if Add(tc.a, 0) != tc.a {
			t.Error(tc.name)
		}
	}
}

func TestClosure(t *testing.T) {
	check := func(a, want int) {
		if Add(a, 0) != want {
			t.Errorf("%d", a)
		}
	}
	for _, tc := range []struct{ a, want int }{{1,1}, {2, 2}} {
		check(tc.a, tc.want)
	}
}

func TestCond(t *testing.T) {
	enabled := Add(1, 0) == 1
	if enabled {
		if Add(1, 1) != 2 {
			t.Error("bad")
		}
	}
	switch Add(1, 0) {
	case 1:
		if Add(2, 2) != 4 {
			t.Error("four")
		}
	}
}

func TestElse(t *testing.T) {
	outer, enabled := Add(1, 0) == 1, Add(0, 0) == 0
	if outer {
		t.Log("outer")
	} else if enabled {
		t.Error("bad")
	}
}

func TestRuns(t *testing.T) {
	for _, name := range []string{"a"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestNames(t *testing.T) {
	t.Run("a runs", func(t *testing.T) {
		if Add(1, 1) != 2 {
			t.Fatal("a")
		}
	})
}

func TestCleanup(t *testing.T) {
	check := func() {
		if Add(1, 0) != 1 {
			t.Error("one")
		}
	}
	for range []string{"a", "b"} {
		check()
	}
}

func TestLits(t *testing.T) {
	if len([]int{1}) != 1 {
		t.Fatal("len")
	}
	if !func() bool { return Add(1, 1) == 2 }() {
		t.Fatal("func")
	}
	if n := Add(1, 1); n != 2 {
		t.Fatal("init")
	}
}

func runCases(t *testing.T) {
	t.Run("a", func(t *testing.T) {
		if Add(1, 1) != 2 {
			t.Fatal("a")
		}
	})
	t.Run("b", func(t *testing.T) {
		if Add(2, 2) != 4 {
			t.Fatal("b")
		}
	})
}

func TestHelper(t *testing.T) {
	runCases(t)
}

func namedCase(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		if Add(1, 1) != 2 {
			t.Fatal("inner")
		}
	})
}

func tableHelper(t *testing.T) {
	for _, n := range []string{"x", "y"} {
		if n == "" {
			t.Fatal(n)
		}
	}
}

func TestNamed(t *testing.T) {
	t.Run("named", namedCase)
	t.Run("rows", tableHelper)
}

func recurse(t *testing.T, n int) {
	if n > 0 {
		recurse(t, n-1)
	}
	t.Run("r", func(t *testing.T) {
		if Add(n, 0) != n {
			t.Fatal(n)
		}
	})
}

func TestRecurse(t *testing.T) {
	recurse(t, 2)
}

const strict = true

func TestStrict(t *testing.T) {
	if strict {
		t.Error("strict")
	}
}

func TestInline(t *testing.T) {
	sub := func() {
		t.Run("c", func(t *testing.T) {
			if Add(1, 1) != 2 {
				t.Fatal("c")
			}
		})
	}
	sub()
}
`

// raceCases is a test declared once per race variant, with its table's
// rows.
func raceCases(tag, rows string) string {
	return "//go:build " + tag + "\n\npackage pkg\n\nimport \"testing\"\n\n" +
		"func TestRace(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{" + rows + "} {\n\t\tif tc.n == \"\" {\n\t\t\tt.Fatal(tc.v)\n\t\t}\n\t}\n}\n"
}

// TestIntegrityCases (P14d-b; #226 entries 74, 85, 99; Astra #255 H5, M9;
// Astra #292 H3, H4 and round 2's 2, 3, 5, 7, 8): a row per bypass, each
// with its exact block or warnings, and the clean controls with none.
func TestIntegrityCases(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/cases_test.go": casesBase, "pkg/race_cases_test.go": raceCases("race", `{"a", 1}, {"b", 2}`),
			"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}, {"c", 3}`)})
		plain(t, src, "add", "--all")
	})
	edit := func(from, to string) map[string]string {
		if !strings.Contains(casesBase, from) {
			t.Fatalf("no %q in the base", from)
		}
		return map[string]string{"pkg/cases_test.go": strings.Replace(casesBase, from, to, 1)}
	}
	both := func(what string) []string { // a warning on each gate platform
		return []string{"linux/amd64 pkg:pkg." + what, "linux/arm64 pkg:pkg." + what}
	}
	changed := "an assertion, its condition or a test case changed: review it"
	for name, c := range map[string]struct {
		files map[string]string
		block string   // in a block ("": accepted)
		warns []string // exactly, when accepted
	}{
		// Identities.
		"a table row removed":              {edit("\t\t{\"zero\", 0, 0},\n", ""), `1 test cases removed (table rows or subtests: row "zero" @`, nil},
		"a table row added":                {edit("\t\t{\"neg\", -1, -1},\n", "\t\t{\"neg\", -1, -1},\n\t\t{\"two\", 2, 2},\n"), "", nil},
		"a row edited under its name":      {edit(`{"zero", 0, 0}`, `{"zero", 0, 1}`), "", both("TestTable: " + changed)},
		"a call row edited under its name": {edit(`append(id, "commit", "-m")`, `{id[0], "commit", "-m"}`), "", both("TestArgs: " + changed)}, // a real commit's edit (c09ac49)
		"a call row removed":               {edit(`, append(id, "commit", "-m")`, `, id`), `1 test cases removed (table rows or subtests: row "commit" @`, nil},
		"a map case removed":               {edit(`"one": 1, "two": 2`, `"one": 1`), `1 test cases removed (table rows or subtests: row "two" @`, nil},
		"a nested subtest renamed":         {edit(`t.Run("other"`, `t.Run("another"`), "1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest removed":                {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", ""), "test cases removed (table rows or subtests: run outer/other)", nil},
		"a computed subtest name":          {imported("strings", edit(`t.Run("outer"`, `t.Run(strings.ToLower("OUTER")`)), "unverifiable: 3 test cases no longer where the guard can follow them", nil},
		"the table moved to a package var": {movedTable(t), "unverifiable: 3 test cases no longer where the guard can follow them", nil},
		"another file only (the control)":  {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, "", nil},
		// M9: equal counts, a changed condition.
		"a condition inverted":   {edit("Add(1, 2) != 3", "Add(1, 2) == 3"), "", both("TestPlain: " + changed)},
		"if false":               {edit("\tif Add(1, 2) != 3 {", "\tif false {"), "", both("TestPlain: " + changed)}, // main counts it; a branch never taken prints none
		"a dead else":            {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "\tif true {\n\t} else {\n\t\tt.Fatal(\"add\")\n\t}"), "", both("TestPlain: " + changed)},
		"a constant flipped":     {edit("const strict = true", "const strict = false"), "", both("TestStrict: " + changed)}, // main counts it; a branch never taken prints none
		"an if and else swapped": {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "\tif Add(1, 2) != 3 {\n\t} else {\n\t\tt.Fatal(\"add\")\n\t}"), "", both("TestPlain: " + changed)},
		// A local table changed before its loop; a row moved under another loop.
		"truncLocal":               {edit("\tfor _, tc := range tests {", "\ttests = tests[:1]\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"nilLocal":                 {edit("\tfor _, tc := range tests {", "\ttests = nil\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a table passed to a call": {edit("\tfor _, tc := range tests {", "\tclear(tests)\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a table's address taken":  {edit("\tfor _, tc := range tests {", "\tp := &tests\n\t*p = nil\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a map case deleted":       {edit("\tfor name, n := range cases {", "\tdelete(cases, \"two\")\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"an alias deleted from":    {edit("\tfor name, n := range cases {", "\talias := cases\n\tdelete(alias, \"two\")\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"an alias cleared":         {edit("\tfor name, n := range cases {", "\talias := cases\n\tclear(alias)\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a map value incremented":  {edit("\tfor name, n := range cases {", "\tcases[\"one\"]++\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a loop's check moved out": {map[string]string{"pkg/cases_test.go": strings.NewReplacer("\t\tif Add(tc.a, 0) != tc.want {\n\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t}\n", "\t\tt.Run(tc.name, func(t *testing.T) {})\n", "func TestTable(t *testing.T) {\n", "func TestTable(t *testing.T) {\n\tif Add(9, 0) != 9 {\n\t\tt.Error(\"nine\")\n\t}\n").Replace(casesBase)}, "TestTable 3 test cases removed", nil},
		"moveRow":                  {moveRow(), `TestTable 1 test cases removed (table rows or subtests: row "zero" @`, nil},
		// A loop that ran subtests now checks too (Astra #292 M6): accepted;
		// a subtest's name is compared exactly (Astra r2 2).
		"runs to checks":           {edit("\t\tt.Run(name, func(t *testing.T) {})\n", "\t\tif name == \"\" {\n\t\t\tt.Fatal(name)\n\t\t}\n\t\tt.Run(name, func(t *testing.T) {})\n"), "", nil},
		"a subtest runs to checks": {edit(`t.Run("a runs"`, `t.Run("a checks"`), "TestNames 1 test cases removed (table rows or subtests: run a runs)", nil},
		// Every enclosing condition and case (Astra #292 M8), an else if's
		// own (r2 7).
		"an outer condition inverted": {edit("\tif enabled {", "\tif !enabled {"), "", both("TestCond: " + changed)},
		"a case changed":              {edit("\tcase 1:\n", "\tcase 2:\n"), "", both("TestCond: " + changed)},
		"an else if inverted":         {edit("} else if enabled {", "} else if !enabled {"), "", both("TestElse: " + changed)},
		// A table driven through a local closure (M4), and the closure also a
		// Cleanup (r2 8).
		"closureRowRemoved": {edit("{{1,1}, {2, 2}}", "{{1,1}}"), "TestClosure 1 test cases removed (table rows or subtests: row {2, 2} @", nil},
		"a closure cleanup": {edit("\tfor range []string{\"a\", \"b\"} {", "\tt.Cleanup(check)\n\tfor range []string{\"a\", \"b\"} {"), "", nil},
		"a closure's row":   {edit("\tfor range []string{\"a\", \"b\"} {", "\tfor range []string{\"a\"} {"), `TestCleanup 1 test cases removed (table rows or subtests: row "b" @1 checks)`, nil},
		// As on main, accepted clean.
		"namelessFmt":     {edit("{{1,1}, {2, 2}}", "{{1, 1}, {2, 2}}"), "", nil},
		"namelessComment": {edit("{{1,1}, {2, 2}}", "{\n\t\t{1, // one\n\t\t\t1},\n\t\t{2, 2},\n\t}"), "", nil},
		"subConstName":    {edit("\t\tt.Run(\"other\", func", "\t\tconst otherName = \"other\"\n\t\tt.Run(otherName, func"), "", nil},
		// Literals and an if's Init in the print; a row whatever the counts.
		"sliceLit":          {edit("[]int{1}) != 1", "[]int{2}) != 1"), "", both("TestLits: " + changed)},
		"funcLit":           {edit("return Add(1, 1) == 2", "return true"), "", both("TestLits: " + changed)},
		"ifInit":            {edit("n := Add(1, 1); n != 2", "n := Add(2, 0); n != 2"), "", both("TestLits: " + changed)},
		"rowEditPlusAssert": {map[string]string{"pkg/cases_test.go": strings.NewReplacer(`{"zero", 0, 0}`, `{"zero", 0, 1}`, "func TestTable(t *testing.T) {\n", "func TestTable(t *testing.T) {\n\tif Add(9, 0) != 9 {\n\t\tt.Error(\"nine\")\n\t}\n").Replace(casesBase)}, "", both("TestTable: " + changed)},
		"subDeadKeep":       {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tif false {\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		// Helpers and named callbacks (Astra #292 H3).
		"a helper's subtest deleted":      {edit("\tt.Run(\"b\", func(t *testing.T) {\n\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"b\")\n\t\t}\n\t})\n", ""), "TestHelper 1 test cases removed (table rows or subtests: run b)", nil},
		"a helper's subtest renamed":      {edit(`t.Run("b"`, `t.Run("c"`), "TestHelper 1 test cases removed (table rows or subtests: run b)", nil},
		"a named callback's subtest":      {edit(`t.Run("inner", func(t *testing.T) {`+"\n\t\tif Add(1, 1) != 2 {\n\t\t\tt.Fatal(\"inner\")", `t.Run("inner2", func(t *testing.T) {`+"\n\t\tif Add(1, 1) != 2 {\n\t\t\tt.Fatal(\"inner\")"), "TestNamed 1 test cases removed (table rows or subtests: run named/inner)", nil},
		"a named callback's row":          {edit(`[]string{"x", "y"}`, `[]string{"x"}`), `TestNamed 1 test cases removed (table rows or subtests: row rows/"y" @tableHelper#1 checks)`, nil},
		"a helper's call removed":         {edit("\trunCases(t)\n", "\tif Add(1, 1) != 2 {\n\t\tt.Fatal(\"x\")\n\t}\n"), "TestHelper 2 test cases removed (table rows or subtests: run a)", nil},
		"a helper called in a subtest":    {edit("\trunCases(t)\n", "\tt.Run(\"in\", runCases)\n"), "TestHelper 2 test cases removed (table rows or subtests: run a)", nil},
		"a helper inlined (the control)":  {edit("\trunCases(t)\n", "\trunCases(t)\n\trunCases(t)\n"), "", nil},
		"a closure inlined (the control)": {edit("\tsub := func() {\n\t\tt.Run(\"c\", func(t *testing.T) {\n\t\t\tif Add(1, 1) != 2 {\n\t\t\t\tt.Fatal(\"c\")\n\t\t\t}\n\t\t})\n\t}\n\tsub()\n", "\tt.Run(\"c\", func(t *testing.T) {\n\t\tif Add(1, 1) != 2 {\n\t\t\tt.Fatal(\"c\")\n\t\t}\n\t})\n"), "", nil},
		"a recursive helper (control)":    {edit("\trecurse(t, 2)\n", "\tt.Log(\"r\")\n\trecurse(t, 2)\n"), "", nil},
		// Build alternatives (Astra #292 H4): a row of one variant only.
		"a race variant's row":          {map[string]string{"pkg/race_cases_test.go": raceCases("race", `{"a", 1}`)}, `TestRace 1 test cases removed (table rows or subtests: row "b" @`, nil},
		"a !race variant's row":         {map[string]string{"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}`)}, `TestRace 1 test cases removed (table rows or subtests: row "c" @`, nil},
		"a race variant's row edited":   {map[string]string{"pkg/race_cases_test.go": raceCases("race", `{"a", 1}, {"b", 3}`)}, "", both("TestRace: " + changed)},
		"a !race variant's row added":   {map[string]string{"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}, {"c", 3}, {"b", 2}`)}, "", nil},
		"the variants merged, b kept":   {map[string]string{"pkg/race_cases_test.go": "package pkg\n", "pkg/norace_cases_test.go": raceCases("race || !race", `{"a", 1}, {"b", 2}`)}, `TestRace 1 test cases removed (table rows or subtests: row "c" @`, nil},
		"the variants merged, all kept": {map[string]string{"pkg/race_cases_test.go": "package pkg\n", "pkg/norace_cases_test.go": raceCases("race || !race", `{"a", 1}, {"b", 2}, {"c", 3}`)}, "", nil},
	} {
		v := f.guarded(t, c.files)
		switch {
		case c.block != "" && !finds(v.Blocks, c.block):
			t.Errorf("%s: blocks %q, want %q", name, whats(v.Blocks), c.block)
		case c.block == "" && (!v.Accepted() || !slices.Equal(whats(v.Warnings), c.warns) && !(c.warns == nil && len(v.Warnings) == 0)):
			t.Errorf("%s: blocks %q, warnings %q, want %q", name, whats(v.Blocks), whats(v.Warnings), c.warns)
		}
	}
}

// moveRow is casesBase with TestTable's "zero" row moved to a second loop
// of the test that only runs a subtest per row.
func moveRow() map[string]string {
	s := strings.Replace(casesBase, "\t\t{\"zero\", 0, 0},\n", "", 1)
	s = strings.Replace(s, "\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t}\n\t}\n}\n", "\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t}\n\t}\n"+
		"\tfor _, tc := range []struct {\n\t\tname    string\n\t\ta, want int\n\t}{{\"zero\", 0, 0}} {\n\t\tt.Run(tc.name, func(t *testing.T) {})\n\t}\n}\n", 1)
	return map[string]string{"pkg/cases_test.go": s}
}

// imported is files with the test file importing p too.
func imported(p string, files map[string]string) map[string]string {
	files["pkg/cases_test.go"] = strings.Replace(files["pkg/cases_test.go"], "import \"testing\"", "import (\n\t\""+p+"\"\n\t\"testing\"\n)", 1)
	return files
}

// movedTable is casesBase with TestTable's table moved to a package variable.
func movedTable(t *testing.T) map[string]string {
	i, j := strings.Index(casesBase, "\tfor _, tc := range []struct {"), strings.Index(casesBase, "\t} {\n\t\tif Add(tc.a")
	if i < 0 || j < 0 {
		t.Fatal("no table in the base")
	}
	lit := strings.TrimPrefix(casesBase[i:j+3], "\tfor _, tc := range ")
	return map[string]string{"pkg/cases_test.go": casesBase[:i] + "\tfor _, tc := range table {" + casesBase[j+4:] + "\nvar table = " + strings.TrimSuffix(lit, " {") + "\n"}
}

// readings is the guard on files, with its drives readings and charged
// case work.
func readings(t *testing.T, f fixture, files map[string]string) (Verdict, int64, int64) {
	v0, w0 := caseVisits.Load(), caseWork.Load()
	v := f.guarded(t, files)
	return v, caseVisits.Load() - v0, caseWork.Load() - w0
}

// lowered is the guard's work bound lowered to 1<<17 for the test.
func lowered(t *testing.T) {
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 17
}

// TestIntegrityCasesBound: a test's cases and print, and the loops read
// for them, are charged to the guard's work bound; past it, the test's
// file is unverifiable, never followed in part.
func TestIntegrityCasesBound(t *testing.T) {
	f := newGuardFixture(t)
	lowered(t)
	if v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Fatalf("the control under the lowered bound: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	var rows strings.Builder
	for i := range 1 << 12 {
		fmt.Fprintf(&rows, "\t\t{%d, %d},\n", i, i)
	}
	big := "package pkg\n\nimport \"testing\"\n\nfunc TestBig(t *testing.T) {\n\tfor _, tc := range []struct{ a, b int }{\n" + rows.String() +
		"\t} {\n\t\tif Add(tc.a, 0) != tc.b {\n\t\t\tt.Fatal(tc.a)\n\t\t}\n\t}\n}\n"
	nested := "package pkg\n\nimport \"testing\"\n\nfunc TestNested(t *testing.T) {\n" + strings.Repeat("for range []int{1} {\n", 300) + strings.Repeat("}\n", 300) + "}\n"
	var asserts strings.Builder
	for i := range 1 << 13 {
		fmt.Fprintf(&asserts, "\tt.Error(%d)\n", i)
	}
	many := "package pkg\n\nimport \"testing\"\n\nfunc TestMany(t *testing.T) {\n" + asserts.String() + "}\n"
	for name, file := range map[string]string{"a table": big, "300 nested loops": nested, "8,192 assertions": many, "a 100,000-value case": caseClause(100000)} {
		if v := f.guarded(t, map[string]string{"pkg/big_test.go": file}); !finds(v.Blocks, "past the guard's work bound") {
			t.Errorf("%s past the bound: blocks %q", name, whats(v.Blocks))
		}
	}
}

// caseClause is a test with one case clause listing n integers around an
// assertion (Astra #292 r2 3).
func caseClause(n int) string {
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc TestClause(t *testing.T) {\n\tswitch Add(1, 0) {\n\tcase 0")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, ", %d", i)
	}
	b.WriteString(":\n\t\tt.Error(\"in\")\n\t}\n}\n")
	return b.String()
}

// TestIntegrityCasesClauseStops (Astra #292 r2 3): the print stops within
// a case clause's values once past the bound, after the value that crossed
// it, never reading the rest.
func TestIntegrityCasesClauseStops(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "clause_test.go", caseClause(20000), 0)
	if err != nil {
		t.Fatal(err)
	}
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1000
	r := &run{}
	s := &pkgScan{r: r, pkg: types.NewPackage("example.com/m/pkg", "pkg"), info: &types.Info{}, cs: caseScan{dead: map[ast.Node]bool{}}}
	s.testPrint(file.Decls[1].(*ast.FuncDecl).Body)
	if r.work > maxWork+16 {
		t.Errorf("the print read %d past a bound of %d", r.work, maxWork)
	}
}

// TestIntegrityCasesLinear (Opus #292 H3; Astra #292 r2 3): the print's
// work is linear: n nested asserting ifs, and a case clause of n values,
// each charge twice the work at 2n, never four times.
func TestIntegrityCasesLinear(t *testing.T) {
	f := newGuardFixture(t)
	nested := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "if Add(%d, 0) != %d {\nt.Error(%d)\n", i, i+1, i)
		}
		return "package pkg\n\nimport \"testing\"\n\nfunc TestDeep(t *testing.T) {\n" + b.String() + strings.Repeat("}\n", n) + "}\n"
	}
	for name, file := range map[string]func(int) string{"nested ifs": nested, "a case clause": caseClause} {
		work := func(n int) int64 {
			v, _, w := readings(t, f, map[string]string{"pkg/deep_test.go": file(n)})
			if !v.Accepted() {
				t.Fatalf("%s of %d: blocks %q", name, n, whats(v.Blocks))
			}
			return w
		}
		if a, b := work(1000), work(2000); b > 5*a/2 {
			t.Errorf("%s: 1,000 charge %d, 2,000 %d: not linear", name, a, b)
		}
	}
}

// TestIntegrityCasesStops (Astra #292 H5; r2 5, 6): the guard stops at
// once past its work bound, and reads each loop body and closure once per
// depth: thousands of nested empty ranges are unverifiable after a few
// readings, a loop body past the bound is never read, a fan-out of
// closures (c0 … c8, each calling its predecessor ten times) is read in a
// few dozen readings, and a deep call before a shallow one keeps the
// shallow one's answer. Readings are counted, never timed.
func TestIntegrityCasesStops(t *testing.T) {
	f := newGuardFixture(t)
	lowered(t)
	nested := "package pkg\n\nimport \"testing\"\n\nfunc TestNested(t *testing.T) {\n" + strings.Repeat("for range []int{1} {\n", 3000) + strings.Repeat("}\n", 3000) + "}\n"
	if v, n, _ := readings(t, f, map[string]string{"pkg/nested_test.go": nested}); !finds(v.Blocks, "past the guard's work bound") || n > 100 {
		t.Errorf("3,000 nested ranges: %d readings, blocks %q", n, whats(v.Blocks))
	}
	body := "package pkg\n\nimport \"testing\"\n\nfunc TestLong(t *testing.T) {\n\tfor range []int{1} {\n\t\tx := 0\n" + strings.Repeat("\t\tx++\n", 14000) + "\t\t_ = x\n\t}\n}\n"
	if v, n, _ := readings(t, f, map[string]string{"pkg/long_test.go": body}); !finds(v.Blocks, "past the guard's work bound") || n != 0 {
		t.Errorf("a loop body past the bound: %d readings, blocks %q", n, whats(v.Blocks))
	}
	var fan strings.Builder
	fan.WriteString("package pkg\n\nimport \"testing\"\n\nfunc TestFan(t *testing.T) {\n\tc0 := func() {}\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&fan, "\tc%d := func() {%s }\n", i, strings.Repeat(fmt.Sprintf(" c%d();", i-1), 10))
	}
	fan.WriteString("\tfor range []int{1} {\n\t\tc8()\n\t}\n}\n")
	if v, n, _ := readings(t, f, map[string]string{"pkg/fan_test.go": fan.String()}); !v.Accepted() || n > 100 {
		t.Errorf("a fan-out of closures: %d readings, blocks %q", n, whats(v.Blocks))
	}
}

// TestIntegrityCasesDepth (Astra #292 r2 5): c1 … c9 each call their
// predecessor, c0 asserts; a loop calling c2 keeps its rows when c9 is
// called before c2 in it (c2 is then read at depth 8 first, its answer cut
// by the depth). Likewise k0 runs a subtest, k1 … k9 call their
// predecessor: calling k9 before k2 keeps k2's subtest.
func TestIntegrityCasesDepth(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc TestChain(t *testing.T) {\n\tc0 := func() {\n\t\tif Add(1, 0) != 1 {\n\t\t\tt.Fatal(\"c0\")\n\t\t}\n\t}\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "\tc%d := func() { c%d() }\n", i, i-1)
	}
	b.WriteString("\tk0 := func() {\n\t\tt.Run(\"deep\", func(t *testing.T) {\n\t\t\tif Add(1, 0) != 1 {\n\t\t\t\tt.Fatal(\"k0\")\n\t\t\t}\n\t\t})\n\t}\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, "\tk%d := func() { k%d() }\n", i, i-1)
	}
	b.WriteString("\tfor range []string{\"a\"} {\n\t\tc2()\n\t}\n\tc9()\n\tk2()\n\tk9()\n}\n")
	base := b.String()
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/chain_test.go": base})
		plain(t, src, "add", "--all")
	})
	if v := f.guarded(t, map[string]string{"pkg/chain_test.go": strings.Replace(base, "\t\tc2()\n", "\t\tc9()\n\t\tc2()\n", 1)}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("c9 called before c2: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	if v := f.guarded(t, map[string]string{"pkg/chain_test.go": strings.Replace(base, "\tk2()\n\tk9()\n", "\tk9()\n\tk2()\n", 1)}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("k9 called before k2: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	if v := f.guarded(t, map[string]string{"pkg/chain_test.go": strings.Replace(base, "[]string{\"a\"}", "[]string{}", 1)}); !finds(v.Blocks, `TestChain unverifiable: 1 test cases no longer where the guard can follow them (row "a" @1 checks)`) {
		t.Errorf("the row removed: blocks %q", whats(v.Blocks))
	}
}
