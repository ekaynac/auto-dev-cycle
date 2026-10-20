// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// casesBase is P14d's test file: a table, a map of subtests, nested
// subtests, a plain test, and skipping helpers (a function, a method, a
// package variable holding one).
const casesBase = `package pkg

import "testing"

type E interface{ Errorf(string, ...any) }

type fake struct{}

func (fake) Errorf(string, ...any) {}

func requireDocker(t *testing.T) { t.Skip("unavailable") }

type h struct{}

func (h) require(t *testing.T) { t.Skip("unavailable") }

var pkgSkip = requireDocker

func check(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("check")
	}
}

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
`

// TestIntegrityCases (P14d; #226 entries 74, 85; Astra #255 H5, H6, M9 and
// finding 10): a row per bypass, each with its exact block or warnings,
// and the clean controls with none.
func TestIntegrityCases(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/cases_test.go": casesBase})
		plain(t, src, "add", "--all")
	})
	edit := func(from, to string) map[string]string {
		if !strings.Contains(casesBase, from) {
			t.Fatalf("no %q in the base", from)
		}
		return map[string]string{"pkg/cases_test.go": strings.Replace(casesBase, from, to, 1)}
	}
	plainTest := "\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"
	inPlain := func(code string) map[string]string { return edit(plainTest, code+plainTest) }
	both := func(what string) []string { // a warning or block on each gate platform
		return []string{"linux/amd64 pkg:pkg." + what, "linux/arm64 pkg:pkg." + what}
	}
	changed := "an assertion, its condition or a test case changed, with equal counts: review it"
	for name, c := range map[string]struct {
		files map[string]string
		block string   // in a block ("": accepted)
		warns []string // exactly, when accepted
	}{
		// H5: identities.
		"a table row removed":              {edit("\t\t{\"zero\", 0, 0},\n", ""), `1 test cases removed (table rows or subtests: row "zero")`, nil},
		"a table row added":                {edit("\t\t{\"neg\", -1, -1},\n", "\t\t{\"neg\", -1, -1},\n\t\t{\"two\", 2, 2},\n"), "", nil},
		"a row edited under its name":      {edit(`{"zero", 0, 0}`, `{"zero", 0, 1}`), "", both("TestTable: " + changed)},
		"a map case removed":               {edit(`"one": 1, "two": 2`, `"one": 1`), `1 test cases removed (table rows or subtests: row "two")`, nil},
		"a nested subtest renamed":         {edit(`t.Run("other"`, `t.Run("another"`), "1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest removed":                {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", ""), "test cases removed (table rows or subtests: run outer/other)", nil},
		"a computed subtest name":          {imported("strings", edit(`t.Run("outer"`, `t.Run(strings.ToLower("OUTER")`)), "unverifiable: 3 test cases no longer where the guard can follow them", nil},
		"the table moved to a package var": {movedTable(t), "unverifiable: 3 test cases no longer where the guard can follow them", nil},
		// H6: a skip through an existing helper, its aliases, reflection.
		"a skipping helper called":                   {inPlain("\trequireDocker(t)\n"), "TestPlain a test skipped", nil},
		"a skipping helper through a local":          {inPlain("\tf := requireDocker\n\tf(t)\n"), "TestPlain a test skipped", nil},
		"a skipping helper through a package var":    {inPlain("\tpkgSkip(t)\n"), "TestPlain a test skipped", nil},
		"a skipping method called":                   {inPlain("\th{}.require(t)\n"), "TestPlain a test skipped", nil},
		"a skipping method value":                    {inPlain("\tg := h{}.require\n\tg(t)\n"), "TestPlain a test skipped", nil},
		"SkipNow by reflection":                      {imported("reflect", inPlain("\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n")), "unverifiable: reflection on a testing value", nil},
		"an asserting helper called (the control)":   {inPlain("\tcheck(t)\n"), "", nil},
		"another file of the package only (control)": {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, "", nil},
		// M9: equal counts, a changed condition.
		"a condition inverted": {edit("Add(1, 2) != 3", "Add(1, 2) == 3"), "", both("TestPlain: " + changed)},
		// #226 entry 85.
		"a constant-false conjunction": {edit("\tif Add(1, 2) != 3 {", "\tconst strict = false\n\tif strict && Add(1, 2) != 3 {"), "TestPlain fewer assertions in a test (1 → 0)", nil},
		"if false":                     {edit("\tif Add(1, 2) != 3 {", "\tif false {"), "TestPlain fewer assertions in a test (1 → 0)", nil},
		"a testing value rebound":      {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")", "\tvar x E = t\n\tx = fake{}\n\tif Add(1, 2) != 3 {\n\t\tx.Errorf(\"add\")"), "TestPlain fewer assertions in a test (1 → 0)", nil},
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

// TestIntegrityCasesBound: a test's cases are charged to the guard's work
// bound; past it, the test's file is unverifiable, never followed in part.
func TestIntegrityCasesBound(t *testing.T) {
	f := newGuardFixture(t)
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 12
	if v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Fatalf("the control under the lowered bound: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	var rows strings.Builder
	for i := range 1 << 12 {
		fmt.Fprintf(&rows, "\t\t{%d, %d},\n", i, i)
	}
	big := "package pkg\n\nimport \"testing\"\n\nfunc TestBig(t *testing.T) {\n\tfor _, tc := range []struct{ a, b int }{\n" + rows.String() +
		"\t} {\n\t\tif Add(tc.a, 0) != tc.b {\n\t\t\tt.Fatal(tc.a)\n\t\t}\n\t}\n}\n"
	if v := f.guarded(t, map[string]string{"pkg/big_test.go": big}); !finds(v.Blocks, "test cases past the guard's work bound") {
		t.Errorf("a table past the bound: blocks %q", whats(v.Blocks))
	}
}
