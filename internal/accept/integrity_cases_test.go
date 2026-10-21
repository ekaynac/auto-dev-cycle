// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"runtime"
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

func TestNameless(t *testing.T) {
	for _, tc := range []struct{ a, want int }{{3,3}, {4, 4}} {
		if Add(tc.a, 0) != tc.want {
			t.Errorf("%d", tc.a)
		}
	}
}

const keyOne = "one"

func TestConstKeys(t *testing.T) {
	for k, v := range map[string]int{keyOne: 1, "two": 2} {
		if len(k) != v+2 {
			t.Fatal(k)
		}
	}
}

func TestHexKeys(t *testing.T) {
	for k, v := range map[int]int{0X1F: 31, 2: 2} {
		if k != v && k != 0x1f {
			t.Fatal(k)
		}
	}
}

func TestNestedRows(t *testing.T) {
	for _, a := range []string{"p", "q"} {
		for _, b := range []string{"x"} {
			if a+b == "" {
				t.Fatal(a)
			}
		}
	}
}

func one(t *testing.T) int {
	if Add(1, 0) != 1 {
		t.Fatal("one")
	}
	return 1
}

func TestRangeExpr(t *testing.T) {
	for _, n := range []int{one(t), 2} {
		_ = n
	}
}

type skipper struct{}

func (skipper) Skip() {}

func TestSkipper(t *testing.T) {
	for range []string{"s1", "s2"} {
		skipper{}.Skip()
	}
	if Add(1, 1) != 2 {
		t.Fatal("skipper")
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

func withFn(t *testing.T, f func()) {
	t.Log("with")
	f()
}

func TestArgLit(t *testing.T) {
	withFn(t, func() {
		t.Run("x", func(t *testing.T) {
			if Add(1, 1) != 2 {
				t.Fatal("x")
			}
		})
	})
	t.Run("y", func(t *testing.T) {
		if Add(2, 2) != 4 {
			t.Fatal("y")
		}
	})
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

func TestSpot(t *testing.T) {
	func() {
		t.Run("spot", func(t *testing.T) {
			if Add(1, 1) != 2 {
				t.Fatal("spot")
			}
		})
	}()
}

func checkOne(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("one")
	}
}

func TestRunsFn(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T)
	}{{"a", checkOne}, {"b", checkOne}} {
		t.Run(tc.name, tc.fn)
	}
}

func runNamed(t *testing.T, n string) {
	t.Run(n, checkOne)
}

func TestNamedLoop(t *testing.T) {
	for _, n := range []string{"a", "b"} {
		runNamed(t, n)
	}
}

func TestH(t *testing.T) {
	raceHelper(t)
}

func TestControls(t *testing.T) {
	for i := 0; i < 2; i++ {
		if Add(i, 0) != i {
			t.Error("for")
		}
	}
	switch n := Add(1, 0); n {
	default:
		if n != 1 {
			t.Error("switch")
		}
	}
	var x any = Add(1, 0)
	switch v := x.(type) {
	case int:
		if v != 1 {
			t.Error("type")
		}
	}
	ch := make(chan int, 1)
	ch <- 1
	select {
	case v := <-ch:
		if v != 1 {
			t.Error("select")
		}
	}
}

const lax = true

func TestLax(t *testing.T) {
	if lax {
		t.Log("lax")
	} else {
		t.Error("strict")
	}
}

func ping(t *testing.T, n int) {
	t.Run("ping", func(t *testing.T) {
		if Add(n, 0) != n {
			t.Fatal(n)
		}
	})
	if n > 0 {
		pong(t, n-1)
	}
}

func pong(t *testing.T, n int) {
	t.Run("pong", func(t *testing.T) {
		if Add(n, 0) != n {
			t.Fatal(n)
		}
	})
	if n > 0 {
		ping(t, n-1)
	}
}

func TestPing(t *testing.T) {
	ping(t, 2)
	pong(t, 2)
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

// raceHelper is raceHelper declared under tag, running a subtest per name
// in subs (each asserting).
func raceHelper(tag string, subs ...string) string {
	var b strings.Builder
	b.WriteString("//go:build " + tag + "\n\npackage pkg\n\nimport \"testing\"\n\nfunc raceHelper(t *testing.T) {\n")
	for _, n := range subs {
		fmt.Fprintf(&b, "\tt.Run(%q, func(t *testing.T) {\n\t\tif Add(1, 1) != 2 {\n\t\t\tt.Fatal(%q)\n\t\t}\n\t})\n", n, n)
	}
	return b.String() + "}\n"
}

// splitCases is TestSplit under tag, with its table's rows.
func splitCases(tag, rows string) string {
	return strings.Replace(strings.Replace(raceCases(tag, rows), "TestRace", "TestSplit", 1), "//go:build \n\n", "", 1)
}

// TestIntegrityCases (P14d-b; #226 entries 74, 85, 99; Astra #255 H5, M9;
// Astra #292 H3, H4 and round 2's 2, 3, 5, 7, 8): a row per bypass, each
// with its exact block or warnings, and the clean controls with none.
func TestIntegrityCases(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/cases_test.go": casesBase, "pkg/race_cases_test.go": raceCases("race", `{"a", 1}, {"b", 2}`),
			"pkg/norace_cases_test.go": raceCases("!race", `{"a", 1}, {"c", 3}`), "pkg/race_helper_test.go": raceHelper("race", "a", "b"),
			"pkg/norace_helper_test.go": raceHelper("!race", "a", "b"), "pkg/split_test.go": splitCases("", `{"a", 1}, {"b", 2}`)})
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
		"if false":               {edit("\tif Add(1, 2) != 3 {", "\tif false {"), "TestPlain a test's assertions made dead (1 → 0 live)", nil}, // main counts it; P14d-w: a dead assertion is removed
		"a dead else":            {edit("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "\tif true {\n\t} else {\n\t\tt.Fatal(\"add\")\n\t}"), "TestPlain a test's assertions made dead (1 → 0 live)", nil},
		"a constant flipped":     {edit("const strict = true", "const strict = false"), "TestStrict a test's assertions made dead (1 → 0 live)", nil}, // main counts it; P14d-w: a dead assertion is removed
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
		"a subtest runs to checks": {edit(`t.Run("a runs"`, `t.Run("a runs checks"`), "TestNames 1 test cases removed (table rows or subtests: run a runs)", nil},
		// Every enclosing condition and case (Astra #292 M8), an else if's
		// own (r2 7).
		"an outer condition inverted": {edit("\tif enabled {", "\tif !enabled {"), "", both("TestCond: " + changed)},
		"a case changed":              {edit("\tcase 1:\n", "\tcase 2:\n"), "", both("TestCond: " + changed)},
		"an else if inverted":         {edit("} else if enabled {", "} else if !enabled {"), "", both("TestElse: " + changed)},
		// A table driven through a local closure (M4), and the closure also a
		// Cleanup (r2 8).
		// Astra #318 1: a loop's subtest wrapped in a literal never run; a
		// loop's check moved into a literal (a stated false block).
		"a loop's t.Run in a literal never run": {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\t_ = func() { t.Run(tc.name, tc.fn) }\n"), `TestRunsFn unverifiable: 2 test cases no longer where the guard can follow them (row "a" @1 runs)`, nil},
		"a loop's check moved into a literal":   {edit("\t\tif Add(tc.a, 0) != tc.want {\n\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t}\n", "\t\tfunc() {\n\t\t\tif Add(tc.a, 0) != tc.want {\n\t\t\t\tt.Errorf(\"%s\", tc.name)\n\t\t\t}\n\t\t}()\n"), `TestTable unverifiable: 3 test cases no longer where the guard can follow them (row "neg" @1 checks)`, nil},
		"another type's Skip drives no loop":    {edit(`[]string{"s1", "s2"}`, `[]string{"s1"}`), "", nil},
		// Opus #344 L1: an outer loop holds what its inner loop does; a
		// range's own expression drives nothing.
		"an outer loop's row":        {edit(`[]string{"p", "q"}`, `[]string{"p"}`), `TestNestedRows 1 test cases removed (table rows or subtests: row "q" @1 checks)`, nil},
		"a range expression's check": {edit("[]int{one(t), 2}", "[]int{one(t)}"), "", nil},
		// Mythos #344 H1: a subtest in a branch a constant switch or for
		// never takes is removed.
		"a loop's t.Run under switch { case false }":  {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tswitch {\n\t\tcase false:\n\t\t\tt.Run(tc.name, tc.fn)\n\t\t}\n"), `TestRunsFn 2 test cases removed (table rows or subtests: row "a" @1 runs)`, nil},
		"a loop's t.Run under for false":              {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tfor false {\n\t\t\tt.Run(tc.name, tc.fn)\n\t\t}\n"), `TestRunsFn 2 test cases removed (table rows or subtests: row "a" @1 runs)`, nil},
		"a subtest under switch { case false }":       {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tswitch {\n\t\tcase false:\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest after a switch's true clause":      {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tswitch {\n\t\tcase true:\n\t\tcase Add(1, 1) == 2:\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest under a constant tag's other case": {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tswitch 1 {\n\t\tcase 2:\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest after a constant tag's own case":   {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tswitch 1 {\n\t\tcase 1:\n\t\tcase Add(0, 1):\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		"a subtest under a live switch (the control)": {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\ton := Add(1, 1) == 2\n\t\tswitch on {\n\t\tcase false:\n\t\tcase true:\n\t\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\t\tif Add(2, 2) != 4 {\n\t\t\t\t\tt.Fatal(\"other\")\n\t\t\t\t}\n\t\t\t})\n\t\t}\n"), "", both("TestSub: " + changed)},
		// Mythos #344 M1: a t.Run's callback swapped for an empty literal.
		"a loop's callback emptied":              {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tt.Run(tc.name, func(t *testing.T) {})\n"), "", both("TestRunsFn: " + changed)},
		"a callback emptied, an assertion added": {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tif Add(1, 1) != 2 {\n\t\t\tt.Error(\"x\")\n\t\t}\n\t\tt.Run(tc.name, func(t *testing.T) {})\n"), "", both("TestRunsFn: " + changed)},
		"a named callback emptied":               {edit(`t.Run("named", namedCase)`, `t.Run("named", func(t *testing.T) {})`), "", both("TestNamed: " + changed)},
		// Mythos #344 L2: a constant key by its value.
		"a constant key's value changed": {edit(`const keyOne = "one"`, `const keyOne = "uno"`), `TestConstKeys 1 test cases removed (table rows or subtests: row "one" @`, nil},
		"a constant key's name changed":  {map[string]string{"pkg/cases_test.go": strings.NewReplacer(`const keyOne = "one"`, `const keyUno = "one"`, "keyOne: 1", "keyUno: 1").Replace(casesBase)}, "", both("TestConstKeys: " + changed)}, // its row's text
		// Opus #344 r2 M1: gofmt's changes change no identity.
		"gofmt on a hex map key":   {edit("0X1F: 31", "0x1F: 31"), "", nil},
		"a trailing comma dropped": {edit(`{"zero", 0, 0}`, `{"zero", 0, 0,}`), "", nil},
		// Opus #344 r2 L2: an early exit added with an assertion warns.
		"an exit and an assertion added": {edit("\tt.Run(\"outer\", func(t *testing.T) {\n", "\tif Add(1, 1) != 2 {\n\t\tt.Error(\"more\")\n\t}\n\tif Add(1, 1) == 2 {\n\t\treturn\n\t}\n\tt.Run(\"outer\", func(t *testing.T) {\n"), "", both("TestSub: " + changed)},
		// Opus #344 M1: an early exit added, the counts equal, warns.
		"a return before a subtest":        {edit("\tt.Run(\"outer\", func(t *testing.T) {\n", "\tif Add(1, 1) == 2 {\n\t\treturn\n\t}\n\tt.Run(\"outer\", func(t *testing.T) {\n"), "", both("TestSub: " + changed)},
		"a return in a subtest's callback": {edit("\t\tt.Run(\"inner\", func(t *testing.T) {\n", "\t\tif Add(1, 1) == 2 {\n\t\t\treturn\n\t\t}\n\t\tt.Run(\"inner\", func(t *testing.T) {\n"), "", both("TestSub: " + changed)},
		"a break after a loop's subtest":   {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tt.Run(tc.name, tc.fn)\n\t\tif tc.name == \"a\" {\n\t\t\tbreak\n\t\t}\n"), "", both("TestRunsFn: " + changed)},
		"a return in a checking loop":      {edit("\t\tif Add(tc.a, 0) != tc.want {\n\t\t\tt.Errorf(\"%s\", tc.name)", "\t\tif tc.name == \"zero\" {\n\t\t\treturn\n\t\t}\n\t\tif Add(tc.a, 0) != tc.want {\n\t\t\tt.Errorf(\"%s\", tc.name)"), "", both("TestTable: " + changed)},
		// As on main, accepted clean.
		"namelessFmt":     {edit("{{3,3}, {4, 4}}", "{{3, 3}, {4, 4}}"), "", nil},
		"namelessComment": {edit("{{3,3}, {4, 4}}", "{\n\t\t{3, // three\n\t\t\t3},\n\t\t{4, 4},\n\t}"), "", nil},
		"subConstName":    {edit("\t\tt.Run(\"other\", func", "\t\tconst otherName = \"other\"\n\t\tt.Run(otherName, func"), "", nil},
		// Literals and an if's Init in the print; a row whatever the counts.
		"sliceLit":          {edit("[]int{1}) != 1", "[]int{2}) != 1"), "", both("TestLits: " + changed)},
		"funcLit":           {edit("return Add(1, 1) == 2", "return true"), "", both("TestLits: " + changed)},
		"ifInit":            {edit("n := Add(1, 1); n != 2", "n := Add(2, 0); n != 2"), "", both("TestLits: " + changed)},
		"rowEditPlusAssert": {map[string]string{"pkg/cases_test.go": strings.NewReplacer(`{"zero", 0, 0}`, `{"zero", 0, 1}`, "func TestTable(t *testing.T) {\n", "func TestTable(t *testing.T) {\n\tif Add(9, 0) != 9 {\n\t\tt.Error(\"nine\")\n\t}\n").Replace(casesBase)}, "", both("TestTable: " + changed)},
		"subDeadKeep":       {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\tif Add(2, 2) != 4 {\n\t\t\tt.Fatal(\"other\")\n\t\t}\n\t\tif false {\n\t\t\tt.Run(\"other\", func(t *testing.T) {})\n\t\t}\n"), "TestSub 1 test cases removed (table rows or subtests: run outer/other)", nil},
		// Helpers and named callbacks (Astra #292 H3).
		// Opus #318 H1: a loop that ran a subtest per row and now only checks.
		"a loop's subtests replaced by a check": {edit("\t\tt.Run(tc.name, tc.fn)\n", "\t\tif tc.name == \"\" {\n\t\t\tt.Fatal(tc.name)\n\t\t}\n"), `TestRunsFn 2 test cases removed (table rows or subtests: row "a" @1 runs)`, nil},
		// H2: a literal that never runs, and a closure never called.
		"a subtest in a literal never run": {edit("\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\tif Add(2, 2) != 4 {\n\t\t\t\tt.Fatal(\"other\")\n\t\t\t}\n\t\t})\n", "\t\t_ = func() {\n\t\t\tt.Run(\"other\", func(t *testing.T) {\n\t\t\t\tif Add(2, 2) != 4 {\n\t\t\t\t\tt.Fatal(\"other\")\n\t\t\t\t}\n\t\t\t})\n\t\t}\n"), "TestSub unverifiable: 1 test cases no longer where the guard can follow them (run outer/other)", nil},
		// Opus #318 r2 H1 (the lead's decision): a literal passed to a helper
		// is opaque and names no case: the helper no longer calling it
		// removes none (as on main); a subtest moved into one is an
		// unverifiable removal (a stated false block).
		"a nameless row's value edited (M1)": {edit("{{3,3}, {4, 4}}", "{{3,3}, {4, 5}}"), "", both("TestNameless: " + changed)},
		// H3: a loop driven through a helper's named-callback subtest.
		// H4 (the lead's decision): a helper of several declarations is one identity.
		"a test split into variants": {map[string]string{"pkg/split_test.go": "package pkg\n", "pkg/split_race_test.go": splitCases("race", `{"a", 1}`), "pkg/split_norace_test.go": splitCases("!race", `{"a", 1}, {"b", 2}`)}, `TestSplit 1 test cases removed (table rows or subtests: row "b" @`, nil},
		// L2: every control's part in the print; a dead else; a cycle's order.
		"a for's condition":        {edit("i < 2; i++", "i < 3; i++"), "", both("TestControls: " + changed)},
		"a switch's tag":           {edit("Add(1, 0); n {", "Add(1, 0); n + 0 {"), "", both("TestControls: " + changed)},
		"a type switch's guard":    {edit("switch v := x.(type)", "switch v := (x).(type)"), "", both("TestControls: " + changed)},
		"a comm clause's receive":  {edit("case v := <-ch:", "case v := <-(ch):"), "", both("TestControls: " + changed)},
		"a constant flipped, else": {edit("const lax = true", "const lax = false"), "", both("TestLax: " + changed)},
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
	v0, w0 := caseVisits.Load(), caseCharged.Load()
	v := f.guarded(t, files)
	return v, caseVisits.Load() - v0, caseCharged.Load() - w0
}

// lowered is the case layer's work bound lowered to 1<<17 for the test.
func lowered(t *testing.T) {
	old := maxCaseWork
	t.Cleanup(func() { maxCaseWork = old })
	maxCaseWork = 1 << 17
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
	var asserts strings.Builder
	for i := range 1 << 13 {
		fmt.Fprintf(&asserts, "\tt.Error(%d)\n", i)
	}
	many := "package pkg\n\nimport \"testing\"\n\nfunc TestMany(t *testing.T) {\n" + asserts.String() + "}\n"
	plainBody := "package pkg\n\nimport \"testing\"\n\nfunc TestPlainLong(t *testing.T) {\n\tx := 0\n" + strings.Repeat("\tx++\n", 40000) + "\t_ = x\n}\n"
	for name, file := range map[string]string{"a table": big, "8,192 assertions": many, "a 100,000-value case": caseClause(100000),
		"a long body": plainBody} {
		if v := f.guarded(t, map[string]string{"pkg/big_test.go": file}); !finds(v.Blocks, "past the guard's work bound") {
			t.Errorf("%s past the bound: blocks %q", name, whats(v.Blocks))
		}
	}
}

// TestIntegrityCasesRowsStop (Opus #318 L2): a table's rows past the case
// layer's bound are not read on: each platform's scan charges at most the
// bound and the row that crossed it.
func TestIntegrityCasesRowsStop(t *testing.T) {
	f := newGuardFixture(t)
	lowered(t)
	var rows strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&rows, "\t\t{\"r%d\", %d},\n", i, i)
	}
	file := "package pkg\n\nimport \"testing\"\n\nfunc TestRows(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{\n" + rows.String() +
		"\t} {\n\t\tif tc.v < 0 {\n\t\t\tt.Fatal(tc.n)\n\t\t}\n\t}\n}\n"
	if v, _, w := readings(t, f, map[string]string{"pkg/rows_test.go": file}); !finds(v.Blocks, "past the guard's work bound") || w > 2*int64(maxCaseWork)+4096 {
		t.Errorf("4,000 rows past the bound: work %d, blocks %q", w, whats(v.Blocks))
	}
}

// TestIntegrityCasesPerRun (Opus #318 H6; r2 M1): the case layer's work
// is its own, one counter per run: ten packages reach no bound when the
// run's r.work is lowered below their case work (the layer never spends
// it), and are past the case layer's bound when it is lowered below their
// total, though each package is within it.
func TestIntegrityCasesPerRun(t *testing.T) {
	f := newGuardFixture(t)
	var rows strings.Builder
	for i := range 300 {
		fmt.Fprintf(&rows, "\t\t{\"r%d\", %d},\n", i, i)
	}
	pkg := func(i int) (string, string) {
		return fmt.Sprintf("p%d/p_test.go", i), fmt.Sprintf("package p%d\n\nimport \"testing\"\n\nfunc TestRows(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{\n", i) + rows.String() +
			"\t} {\n\t\tif tc.v < 0 {\n\t\t\tt.Fatal(tc.n)\n\t\t}\n\t}\n}\n"
	}
	p, src := pkg(0)
	v, _, one := readings(t, f, map[string]string{p: src})
	if !v.Accepted() {
		t.Fatalf("one package: blocks %q", whats(v.Blocks))
	}
	files := map[string]string{}
	for i := range 10 {
		p, src := pkg(i)
		files[p] = src
	}
	oldWork, oldCase := maxWork, maxCaseWork
	t.Cleanup(func() { maxWork, maxCaseWork = oldWork, oldCase })
	maxWork = int(one) // the run's bound, below the ten packages' case work
	if v, _, all := readings(t, f, files); !v.Accepted() || all < 5*one {
		t.Errorf("ten packages, the run's bound lowered: work %d, blocks %q", all, whats(v.Blocks))
	}
	maxWork, maxCaseWork = oldWork, int(5*one) // each package within it, the ten past it
	if v := f.guarded(t, files); !finds(v.Blocks, "past the guard's work bound") {
		t.Errorf("ten packages past the case layer's bound: blocks %q", whats(v.Blocks))
	}
}

// TestIntegrityCasesCharged (Opus #318 H5): each reading and identity is
// charged as it is read or built: a called closure's long body twice per
// scan (its test's body, then its own reading), and a table's long row
// names three times (the test's body, each identity, each row's text), on
// each platform.
func TestIntegrityCasesCharged(t *testing.T) {
	f := newGuardFixture(t)
	name := strings.Repeat("n", 200)
	var rows strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&rows, "\t\t{\"%s%d\", %d},\n", name, i, i)
	}
	table := "package pkg\n\nimport \"testing\"\n\nfunc TestNames(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{\n" + rows.String() +
		"\t} {\n\t\tif tc.v < 0 {\n\t\t\tt.Fatal(tc.n)\n\t\t}\n\t}\n}\n"
	if _, _, w := readings(t, f, map[string]string{"pkg/names_test.go": table}); w < int64(7*1000*len(name)) { // the bodies, each identity and each row's text, on two platforms
		t.Errorf("1,000 rows of %d-byte names: charged %d", len(name), w)
	}
}

// TestIntegrityCasesAllocs (Opus #318 r2 L1): rows sharing one name
// allocate linearly in the guard as a whole, not only in what is charged:
// 8,000 rows allocate under three times what 4,000 do (round 1's
// concatenated row text measured 3.6 times per doubling).
func TestIntegrityCasesAllocs(t *testing.T) {
	f := newGuardFixture(t)
	alloc := func(n int) uint64 {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "\t\t{\"same\", %d},\n", i)
		}
		file := "package pkg\n\nimport \"testing\"\n\nfunc TestSame(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{\n" + b.String() +
			"\t} {\n\t\tif tc.v < 0 {\n\t\t\tt.Fatal(tc.n)\n\t\t}\n\t}\n}\n"
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		if v := f.guarded(t, map[string]string{"pkg/same_test.go": file}); !v.Accepted() {
			t.Fatalf("%d rows: blocks %q", n, whats(v.Blocks))
		}
		runtime.ReadMemStats(&m1)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	if a, b := alloc(4000), alloc(8000); b > 3*a {
		t.Errorf("rows sharing a name: 4,000 allocate %d bytes, 8,000 %d: not linear", a, b)
	}
}

// TestIntegrityCasesNestedLiterals (Opus #344 H1): an assertion whose
// argument nests n asserting function literals: the guard allocates no
// faster than the case work it charges grows (go/printer's canonical text
// grew with the nesting, uncharged: 365 MB at 200, 51 GB at 400).
func TestIntegrityCasesNestedLiterals(t *testing.T) {
	f := newGuardFixture(t)
	run := func(n int) (alloc uint64, charged int64) {
		file := "package pkg\n\nimport \"testing\"\n\nfunc TestNest(t *testing.T) {\n\tt.Error(" +
			strings.Repeat("func() int {\n\tt.Error(", n) + "0" + strings.Repeat(")\n\treturn 0\n}()", n) + ")\n}\n"
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		v, _, w := readings(t, f, map[string]string{"pkg/nest_test.go": file})
		runtime.ReadMemStats(&m1)
		if !v.Accepted() {
			t.Fatalf("%d nested literals: blocks %q", n, whats(v.Blocks))
		}
		return m1.TotalAlloc - m0.TotalAlloc, w
	}
	a1, c1 := run(100)
	a2, c2 := run(200)
	t.Logf("nested literals: 100 allocate %d bytes, charge %d; 200 allocate %d, charge %d", a1, c1, a2, c2)
	if float64(a2)/float64(a1) > float64(c2)/float64(c1) || c2 > 5*c1/2 {
		t.Errorf("not linear, or allocating past the charged work: %d → %d bytes, %d → %d charged", a1, a2, c1, c2)
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
	src := []byte(caseClause(20000))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "clause_test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	read := caseScan{dead: map[ast.Node]bool{}, file: fset.File(file.Pos()), src: src}
	old := maxCaseWork
	t.Cleanup(func() { maxCaseWork = old })
	maxCaseWork = 1000
	r := &run{ctx: context.Background()}
	s := &pkgScan{r: r, pkg: types.NewPackage("example.com/m/pkg", "pkg"), info: &types.Info{}, cs: read}
	s.testPrint(file.Decls[1].(*ast.FuncDecl).Body)
	if s.r.caseWork > maxCaseWork+16 {
		t.Errorf("the print read %d past a bound of %d", s.r.caseWork, maxCaseWork)
	}
	maxCaseWork = old // a cancelled run reads no more (Opus #318 H5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = &pkgScan{r: &run{ctx: ctx}, pkg: s.pkg, info: s.info, cs: read}
	if s.testPrint(file.Decls[1].(*ast.FuncDecl).Body); s.r.caseWork > 16 {
		t.Errorf("a cancelled run's print read %d", s.r.caseWork)
	}
}

// TestIntegrityCasesCanon (Opus #344 H1): a node's canonical text is its
// tokens, one space apart, whatever gofmt or a comment changes, never a
// newline's semicolon; its span is charged.
func TestIntegrityCasesCanon(t *testing.T) {
	src := []byte("package p\n\nvar a = f(x,y)\nvar b = f(x, // why\n\ty)\nvar c = func() {\n\tg(); h()\n}\nvar d = f(x, z)\n" +
		"var e = []float64{0X1F, 1E3, 0x1.8P3, 0xAE, 0B1, 0O7,}\nvar g = []float64{0x1F, 1e3, 0x1.8p3, 0xAE, 0b1, 0o7}\n" +
		"var h = []any{0765i, ((x)), (((y))), f((z)), (a)+(b), ((a)+(b)), 0i}\nvar k = []any{765i, (x), (y), f(z), (a)+(b), ((a)+(b)), 0i}\n")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	s := &pkgScan{r: &run{ctx: context.Background()}, cs: caseScan{dead: map[ast.Node]bool{}, file: fset.File(file.Pos()), src: src}}
	value := func(i int) ast.Expr { return file.Decls[i].(*ast.GenDecl).Specs[0].(*ast.ValueSpec).Values[0] }
	before := s.r.caseWork
	a := s.canon(value(0))
	if span := int(value(0).End() - value(0).Pos()); s.r.caseWork-before != span {
		t.Errorf("charged %d for a span of %d", s.r.caseWork-before, span)
	}
	if b, c, d := s.canon(value(1)), s.canon(value(2)), s.canon(value(3)); a != "f ( x , y )" || b != a || c != "func ( ) { g ( ) h ( ) }" || d == a {
		t.Errorf("canonical texts %q %q %q %q", a, b, c, d)
	}
	// Opus #344 r2 M1: a number's prefix and exponent letters, and a comma
	// before a closing bracket, as gofmt writes them; a hex digit is kept.
	if e, g := s.canon(value(4)), s.canon(value(5)); e != g || e != "[ ] float64 { 0x1F , 1e3 , 0x1.8p3 , 0xAE , 0b1 , 0o7 }" {
		t.Errorf("numbers and a trailing comma: %q, gofmt's %q", e, g)
	}
	// Mythos #344 L1: an integer imaginary literal's leading zeros, and
	// doubled parentheses, as gofmt writes them; (a)+(b) is no doubling.
	if h, k := s.canon(value(6)), s.canon(value(7)); h != k || k != "[ ] any { 765i , ( x ) , ( y ) , f ( z ) , ( a ) + ( b ) , ( ( a ) + ( b ) ) , 0i }" {
		t.Errorf("imaginary zeros and doubled parentheses: %q, gofmt's %q", h, k)
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
	same := func(n int) string { // rows sharing one name (Opus #318 H5)
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "\t\t{\"same\", %d},\n", i)
		}
		return "package pkg\n\nimport \"testing\"\n\nfunc TestSame(t *testing.T) {\n\tfor _, tc := range []struct {\n\t\tn string\n\t\tv int\n\t}{\n" + b.String() +
			"\t} {\n\t\tif tc.v < 0 {\n\t\t\tt.Fatal(tc.n)\n\t\t}\n\t}\n}\n"
	}
	long := func(n int) string { // subtests nested n deep, each named by a 4 KiB constant (H5)
		return "package pkg\n\nimport \"testing\"\n\nconst long = \"" + strings.Repeat("x", 4096) + "\"\n\nfunc TestLong(t *testing.T) {\n" +
			strings.Repeat("t.Run(long, func(t *testing.T) {\n", n) + "t.Error(long)\n" + strings.Repeat("})\n", n) + "}\n"
	}
	conds := func(n int) string { // a condition nesting n asserting literals, each with its own condition (Mythos #344 L3)
		return "package pkg\n\nimport \"testing\"\n\nfunc TestConds(t *testing.T) {\n\tif " +
			strings.Repeat("func() bool {\n\tif ", n) + "true" + strings.Repeat(" {\n\t\tt.Error(1)\n\t}\n\treturn true\n}()", n) + " {\n\t\tt.Error(0)\n\t}\n}\n"
	}
	ranges := func(n int) string { // nested ranges, each asserting (Opus #344 L1)
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "for range []int{%d} {\nt.Error(%d)\n", i, i)
		}
		return "package pkg\n\nimport \"testing\"\n\nfunc TestRanges(t *testing.T) {\n" + b.String() + strings.Repeat("}\n", n) + "}\n"
	}
	for name, file := range map[string]func(int) string{"nested ifs": nested, "a case clause": caseClause, "rows sharing a name": same, "4 KiB names nested": func(n int) string { return long(n / 10) },
		"nested asserting ranges": ranges, "nested asserting conditions": func(n int) string { return conds(n / 10) }} {
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
