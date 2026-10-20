// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"runtime"
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

var skipIt = func(t *testing.T) { t.Skip("unavailable") }

type req interface{ require(*testing.T) }

func with(t *testing.T, f func(*testing.T)) { f(t) }

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

func TestRuns(t *testing.T) {
	for _, name := range []string{"a"} {
		t.Run(name, func(t *testing.T) {})
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
`

// raceCases is a test declared once per race variant, its table's rows,
// and requireX, skipping in the race variant only.
func raceCases(tag, rows, body string) string {
	return "//go:build " + tag + "\n\npackage pkg\n\nimport \"testing\"\n\nfunc requireX(t *testing.T) {" + body + "}\n\n" +
		"func TestRace(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{" + rows + "} {\n\t\tif tc.n == \"\" {\n\t\t\tt.Fatal(tc.v)\n\t\t}\n\t}\n}\n"
}

// TestIntegrityCases (P14d; #226 entries 74, 85; Astra #255 H5, H6, M9 and
// finding 10): a row per bypass, each with its exact block or warnings,
// and the clean controls with none.
func TestIntegrityCases(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/cases_test.go": casesBase, "pkg/xml_test.go": "package pkg\n\nimport \"encoding/xml\"\n\nfunc skipElement(d *xml.Decoder) error { return d.Skip() }\n", "pkg/race_cases_test.go": raceCases("race", `{"a", 1}, {"b", 2}`, ` t.Skip("race") `),
			"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}, {"b", 2}`, "")})
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
	changed := "an assertion, its condition or a test case changed: review it"
	for name, c := range map[string]struct {
		files map[string]string
		block string   // in a block ("": accepted)
		warns []string // exactly, when accepted
	}{
		// H5: identities.
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
		// Opus #292 H1: a local table changed before its loop; a row moved
		// under another loop.
		"truncLocal":               {edit("\tfor _, tc := range tests {", "\ttests = tests[:1]\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"nilLocal":                 {edit("\tfor _, tc := range tests {", "\ttests = nil\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a table passed to a call": {edit("\tfor _, tc := range tests {", "\tclear(tests)\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a table's address taken":  {edit("\tfor _, tc := range tests {", "\tp := &tests\n\t*p = nil\n\tfor _, tc := range tests {"), "TestLocal unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a map case deleted":       {edit("\tfor name, n := range cases {", "\tdelete(cases, \"two\")\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		// Astra #292 H1: any use of a table but its range's.
		"an alias deleted from": {edit("\tfor name, n := range cases {", "\talias := cases\n\tdelete(alias, \"two\")\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"an alias cleared":      {edit("\tfor name, n := range cases {", "\talias := cases\n\tclear(alias)\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		// Astra #292 M6: a loop that ran subtests now checks too (accepted).
		"runs to checks": {edit("\t\tt.Run(name, func(t *testing.T) {})\n", "\t\tif name == \"\" {\n\t\t\tt.Fatal(name)\n\t\t}\n\t\tt.Run(name, func(t *testing.T) {})\n"), "", nil},
		// Astra #292 M7: a helper calling another type's Skip is no skipping helper.
		"an xml.Decoder's Skip": {inPlain("\t_ = skipElement(nil)\n"), "", nil},
		// Astra #292 M8: every enclosing condition and case.
		"an outer condition inverted": {edit("\tif enabled {", "\tif !enabled {"), "", both("TestCond: " + changed)},
		"a case changed":              {edit("\tcase 1:\n", "\tcase 2:\n"), "", both("TestCond: " + changed)},
		"a map value incremented":     {edit("\tfor name, n := range cases {", "\tcases[\"one\"]++\n\tfor name, n := range cases {"), "TestMap unverifiable: 2 test cases no longer where the guard can follow them", nil},
		"a loop's check moved out":    {map[string]string{"pkg/cases_test.go": strings.NewReplacer("\t\tif Add(tc.a, 0) != tc.want {\n\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t}\n", "\t\tt.Run(tc.name, func(t *testing.T) {})\n", "func TestTable(t *testing.T) {\n", "func TestTable(t *testing.T) {\n\tif Add(9, 0) != 9 {\n\t\tt.Error(\"nine\")\n\t}\n").Replace(casesBase)}, "TestTable 3 test cases removed", nil},
		"moveRow":                     {moveRow(), `TestTable 1 test cases removed (table rows or subtests: row "zero" @`, nil},
		// H2: any use of a skipping function, whatever its arguments.
		"skipRebound":    {inPlain("\tx := t\n\trequireDocker(x)\n\tx = nil\n\t_ = x\n"), "TestPlain a test skipped", nil},
		"skipSlice":      {inPlain("\t[]func(*testing.T){requireDocker}[0](t)\n"), "TestPlain a test skipped", nil},
		"skipField":      {inPlain("\tstruct{ f func(*testing.T) }{requireDocker}.f(t)\n"), "TestPlain a test skipped", nil},
		"skipWith":       {inPlain("\twith(t, requireDocker)\n"), "TestPlain a test skipped", nil},
		"skipIface":      {inPlain("\tvar r req = h{}\n\tr.require(t)\n"), "TestPlain a test skipped", nil},
		"skipClosureVar": {inPlain("\tskipIt(t)\n"), "TestPlain a test skipped", nil},
		// M3: skipping in one variant (race) only.
		"skipRaceVariant": {inPlain("\trequireX(t)\n"), "TestPlain a test skipped", nil},
		// M4: a table driven through a local closure.
		"closureRowRemoved": {edit("{{1,1}, {2, 2}}", "{{1,1}}"), "TestClosure 1 test cases removed (table rows or subtests: row {2, 2} @", nil},
		// M2: as on main, accepted clean.
		"namelessFmt":     {edit("{{1,1}, {2, 2}}", "{{1, 1}, {2, 2}}"), "", nil},
		"namelessComment": {edit("{{1,1}, {2, 2}}", "{\n\t\t{1, // one\n\t\t\t1},\n\t\t{2, 2},\n\t}"), "", nil},
		"subConstName":    {edit("\t\tt.Run(\"other\", func", "\t\tconst otherName = \"other\"\n\t\tt.Run(otherName, func"), "", nil},
		// M1: literals and an if's Init in the print; a row whatever the counts.
		"sliceLit":          {edit("[]int{1}) != 1", "[]int{2}) != 1"), "", both("TestLits: " + changed)},
		"funcLit":           {edit("return Add(1, 1) == 2", "return true"), "", both("TestLits: " + changed)},
		"ifInit":            {edit("n := Add(1, 1); n != 2", "n := Add(2, 0); n != 2"), "", both("TestLits: " + changed)},
		"rowEditPlusAssert": {map[string]string{"pkg/cases_test.go": strings.NewReplacer(`{"zero", 0, 0}`, `{"zero", 0, 1}`, "func TestTable(t *testing.T) {\n", "func TestTable(t *testing.T) {\n\tif Add(9, 0) != 9 {\n\t\tt.Error(\"nine\")\n\t}\n").Replace(casesBase)}, "", both("TestTable: " + changed)},
		// Opus #292 L1, L4.
		"a dead else":                  {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "\tif true {\n\t} else {\n\t\tt.Fatal(\"add\")\n\t}"), "TestPlain fewer assertions in a test (1 → 0)", nil},
		"an if and else swapped":       {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "\tif Add(1, 2) != 3 {\n\t} else {\n\t\tt.Fatal(\"add\")\n\t}"), "", both("TestPlain: " + changed)},
		"a race variant's row":         {map[string]string{"pkg/race_cases_test.go": raceCases("race", `{"a", 1}`, ` t.Skip("race") `)}, `TestRace 1 test cases removed (table rows or subtests: row "b" @`, nil},
		"a !race variant's row":        {map[string]string{"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}`, "")}, `TestRace 1 test cases removed (table rows or subtests: row "b" @`, nil},
		"a race variant's row edited":  {map[string]string{"pkg/race_cases_test.go": raceCases("race", `{"a", 1}, {"b", 3}`, ` t.Skip("race") `)}, "", both("TestRace: " + changed)},
		"a !race variant's row edited": {map[string]string{"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}, {"b", 3}`, "")}, "", both("TestRace: " + changed)},
		"subDeadKeep":                  {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tif false {\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
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
// of the test that only runs a subtest per row (Opus #292 H1).
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

// TestIntegrityCasesBound: a test's cases and print, and the loops read
// for them (Opus #292 M5), are charged to the guard's work bound; past it,
// the test's file is unverifiable, never followed in part.
func TestIntegrityCasesBound(t *testing.T) {
	f := newGuardFixture(t)
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 17
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
	for name, file := range map[string]string{"a table": big, "300 nested loops": nested, "8,192 assertions": many} {
		if v := f.guarded(t, map[string]string{"pkg/big_test.go": file}); !finds(v.Blocks, "past the guard's work bound") {
			t.Errorf("%s past the bound: blocks %q", name, whats(v.Blocks))
		}
	}
}

// TestIntegrityCasesNested (Opus #292 H3): a test of n nested ifs, each
// asserting, costs the guard memory linear in n: 2,000 allocate less than
// three times what 1,000 do (the print of every enclosing condition per
// assertion, quadratic, measured 3.7 times).
func TestIntegrityCasesNested(t *testing.T) {
	f := newGuardFixture(t)
	alloc := func(n int) uint64 {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "if Add(%d, 0) != %d {\nt.Error(%d)\n", i, i+1, i)
		}
		file := "package pkg\n\nimport \"testing\"\n\nfunc TestDeep(t *testing.T) {\n" + b.String() + strings.Repeat("}\n", n) + "}\n"
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if v := f.guarded(t, map[string]string{"pkg/deep_test.go": file}); !v.Accepted() {
			t.Fatalf("%d nested ifs: blocks %q", n, whats(v.Blocks))
		}
		runtime.ReadMemStats(&m1)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	a, b := alloc(1000), alloc(2000)
	t.Logf("nested ifs: 1,000 allocate %d bytes, 2,000 %d", a, b)
	if b > 3*a {
		t.Errorf("not linear")
	}
}

// TestIntegrityCasesStops (Astra #292 H5): the guard stops at once past
// its work bound, and reads each loop body and closure once: thousands of
// nested empty ranges are unverifiable after a few readings, and a fan-out
// of closures (c0 … c8, each calling its predecessor ten times) is read in
// a few dozen, never 10^8. Readings are counted, never timed.
func TestIntegrityCasesStops(t *testing.T) {
	f := newGuardFixture(t)
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 17
	read := func(name, file string) (Verdict, int64) {
		before := caseVisits.Load()
		v := f.guarded(t, map[string]string{"pkg/" + name + "_test.go": file})
		return v, caseVisits.Load() - before
	}
	nested := "package pkg\n\nimport \"testing\"\n\nfunc TestNested(t *testing.T) {\n" + strings.Repeat("for range []int{1} {\n", 3000) + strings.Repeat("}\n", 3000) + "}\n"
	if v, n := read("nested", nested); !finds(v.Blocks, "past the guard's work bound") || n > 100 {
		t.Errorf("3,000 nested ranges: %d readings, blocks %q", n, whats(v.Blocks))
	}
	// The skipping helpers' summaries are charged: a helper of 200 KB.
	helper := "package pkg\n\nfunc helper() {\n\tx := 0\n" + strings.Repeat("\tx++\n", 40000) + "\t_ = x\n}\n"
	if v, _ := read("helper", helper); !finds(v.Blocks, "past the guard's work bound") {
		t.Errorf("a helper past the bound: blocks %q", whats(v.Blocks))
	}
	var fan strings.Builder
	fan.WriteString("package pkg\n\nimport \"testing\"\n\nfunc TestFan(t *testing.T) {\n\tc0 := func() {}\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&fan, "\tc%d := func() {%s }\n", i, strings.Repeat(fmt.Sprintf(" c%d();", i-1), 10))
	}
	fan.WriteString("\tfor range []int{1} {\n\t\tc8()\n\t}\n}\n")
	if v, n := read("fan", fan.String()); !v.Accepted() || n > 100 {
		t.Errorf("a fan-out of closures: %d readings, blocks %q", n, whats(v.Blocks))
	}
}
