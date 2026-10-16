// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"testing"
)

// TestDecisionAOut2 (P14b-out-2): each row reproduced first on main
// (e948d00), where it is silent; each runs with the layer on and off, and
// the base guard's findings are byte-equal (outGuarded).
func TestDecisionAOut2(t *testing.T) {
	probe := "\tfs := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = fs.Parse(nil)\n"
	long := strings.Repeat("x", 80)
	base := map[string]string{
		"tool/body.go":  "package tool\n\nimport (\n\t\"flag\"\n\t\"fmt\"\n)\n\nfunc p() {\n" + probe + "}\n\nfunc q() { fmt.Println(\"a\") }\n",
		"tool/lit.go":   "package tool\n\nimport \"flag\"\n\nvar parse = func() {\n" + probe + "}\n",
		"tool/init.go":  "package tool\n\nimport \"flag\"\n\nvar parse2 func()\n\nfunc init() {\n\tparse2 = func() {\n" + probe + "\t}\n}\n",
		"tool/multi.go": "package tool\n\nimport \"flag\"\n\nvar (\n\t_, parse3 = 0, func() {\n" + probe + "\t}\n\t_, other = 0, func() {}\n)\n",
		"tool/blank.go": "package tool\n\nimport \"flag\"\n\nfunc _() {\n" + probe + "}\n\nfunc _() {\n" + probe + "}\n",
		"tool/bvar.go":  "package tool\n\nimport \"flag\"\n\nvar _ = func() {\n" + probe + "}\n\nvar _ = func() {\n" + probe + "}\n",
		"tool/long.go":  "package tool\n\nimport \"flag\"\n\nfunc f" + long + "A() {\n" + probe + "}\n\nfunc f" + long + "B() {\n" + probe + "}\n",
		"tool/tm.go":    "package tool\n\nimport \"flag\"\n\nfunc TestMain() {\n" + probe + "}\n",
		// Opus #280 M3: a var spec reshaped.
		"m3/a.go": "package m3\n\nimport (\n\t\"flag\"\n\t\"fmt\"\n)\n\nvar parse7 = func() {\n" + probe + "}\n\nvar hello = func() { fmt.Println(\"a\") }\n",
		// The background flag on d5041a9: a literal moved to a new init; a
		// body changed beside another function's references.
		"mv2/a.go": "package mv2\n\nimport \"fmt\"\n\nvar hello2 func()\n\nfunc init() { hello2 = func() { fmt.Println(\"a\") } }\n",
		"mv2/b.go": "package mv2\n",
		"l3/a.go":  "package l3\n\nimport \"flag\"\n\nfunc p() {\n" + probe + "}\n",
	}
	base["other/a_test.go"] = "package other\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\nfunc TestMain(t *testing.T) { fmt.Println(\"a\") }\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	h := strings.Replace(probe, "Parse(nil)", "Parse([]string{\"-h\"})", 1)
	first := func(file string) map[string]string {
		return map[string]string{file: strings.Replace(base[file], probe, h, 1)}
	}
	for name, c := range map[string]struct {
		files       map[string]string
		warn, never string
	}{
		"a changed body (flag)":              {first("tool/body.go"), "tool/body.go changed bodies that use flag (1: func p)", ""},
		"a changed body (fmt, main's S)":     {map[string]string{"tool/body.go": strings.Replace(base["tool/body.go"], "\"a\"", "\"b\"", 1)}, "tool/body.go changed bodies that use fmt (1: func q)", ""},
		"a package-level var literal":        {first("tool/lit.go"), "tool/lit.go changed bodies that use flag (1: var parse)", ""},
		"a deferred literal in init (r3 #4)": {first("tool/init.go"), "tool/init.go changed bodies that use flag (1: init literal)", ""},
		"a multi-spec var block (r3 #5)":     {first("tool/multi.go"), "tool/multi.go changed bodies that use flag (1: var _, parse3)", ""},
		"two func _":                         {first("tool/blank.go"), "tool/blank.go changed bodies that use flag (1: func _)", ""},
		"two var _ = func":                   {first("tool/bvar.go"), "tool/bvar.go changed bodies that use flag (1: var _)", ""},
		"a plain func TestMain()":            {first("tool/tm.go"), "tool/tm.go changed bodies that use flag (1: func TestMain)", ""},
		"TestMain(*testing.T) in a test":     {map[string]string{"other/a_test.go": strings.Replace(base["other/a_test.go"], "\"a\"", "\"b\"", 1)}, "other/a_test.go changed bodies that use fmt (1: func TestMain)", ""},
		"M3 a reshaped var spec (flag)": {map[string]string{"m3/a.go": strings.Replace(base["m3/a.go"], "var parse7 = func() {\n"+probe+"}", "var parse7, _ = func() {\n"+h+"}, 0", 1)},
			"m3/a.go changed bodies that use flag (2: var parse7, var parse7, _)", ""},
		"M3 a reshaped var spec (fmt)": {map[string]string{"m3/a.go": strings.Replace(base["m3/a.go"], "var hello = func() { fmt.Println(\"a\") }", "var hello, _ = func() { fmt.Println(\"b\") }, 0", 1)},
			"m3/a.go changed bodies that use fmt (2: var hello, var hello, _)", ""},
		"a literal moved to a new init": {map[string]string{"mv2/a.go": "package mv2\n\nvar hello2 func()\n", "mv2/b.go": "package mv2\n\nimport \"fmt\"\n\nfunc init() { hello2 = func() { fmt.Println(\"b\") } }\n"},
			"mv2/b.go changed bodies that use fmt (1: init literal)", ""},
		"a body changed beside another function's references": {map[string]string{"l3/a.go": strings.Replace(base["l3/a.go"], "func p() {\n"+probe+"}", "func p() {\n"+h+"}\n\nfunc q() { _ = flag.ContinueOnError }", 1)},
			"l3/a.go changed bodies that use flag (1: func p)", ""},
		"references changed: no body warning too": {map[string]string{"l3/a.go": strings.Replace(base["l3/a.go"], "flag.ExitOnError", "flag.ContinueOnError", 1)},
			"l3/a.go references into flag added, removed, moved or changed (1: p)", "l3/a.go changed bodies that use flag"},
	} {
		v := outGuarded(t, f, c.files)
		if !finds(v.Warnings, c.warn) || c.never != "" && finds(v.Warnings, c.never) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// A forced 64-byte collision: both long names bound to one binding; the
	// fingerprints combine, so the first's change shows.
	old := boundHash
	boundHash = 0
	if v := outGuarded(t, f, first("tool/long.go")); !finds(v.Warnings, "tool/long.go changed bodies that use flag (1: func f"+strings.Repeat("x", 63)+"…)") {
		t.Errorf("a bound collision: %q", whats(v.Warnings))
	}
	boundHash = old
	// The real hook is the base guard's: its changed body is no layer finding.
	if v := outGuarded(t, f, map[string]string{"pkg/main_test.go": strings.Replace(guardBase["pkg/main_test.go"], "os.Exit(m.Run())", "code := m.Run()\n\tos.Exit(code)", 1)}); finds(v.Warnings, "pkg/main_test.go changed bodies") {
		t.Errorf("the real hook: %q", whats(v.Warnings))
	}
	// r3 #6: a 64 KiB name with 5,000 references: its name is bounded once
	// (under a node bound that a name bounded for each reference passes).
	oldNodes := maxOutNodes
	maxOutNodes = 8 << 20
	v := outGuarded(t, f, map[string]string{"tool/huge.go": "package tool\n\nimport \"flag\"\n\nfunc " + strings.Repeat("n", 64<<10) + "() {\n" + strings.Repeat("\t_ = flag.Parse\n", 5000) + "}\n"})
	if !finds(v.Warnings, "tool/huge.go references into flag added, removed, moved or changed (1: "+strings.Repeat("n", 64)) || finds(v.Warnings, "stopped at its work bound") {
		t.Errorf("r3 #6: %q", whats(v.Warnings))
	}
	// Astra #280 H4: one var spec of 20,000 blank names and literals: its
	// names' identity is computed once, so its keys stay short.
	blanks := "package tool\n\nimport \"flag\"\n\nvar " + strings.Repeat("_, ", 19999) + "_ =\n" + strings.Repeat("\tfunc() { _ = flag.Parse },\n", 19999) + "\tfunc() { _ = flag.Parse }\n"
	maxOutNodes = 32 << 20
	v = outGuarded(t, f, map[string]string{"tool/blanks.go": blanks})
	maxOutNodes = oldNodes
	if !finds(v.Warnings, "tool/blanks.go references into flag added, removed, moved or changed (1: package level)") || finds(v.Warnings, "stopped at its work bound") {
		t.Errorf("H4: %q", whats(v.Warnings))
	}
	// Astra #280 H1: the blocking decisions keep P14b-out-1's identity: a
	// renamed var holding a libc reference in a tested package is accepted.
	lv := "package pkg\n\nimport \"modernc.org/libc\"\n\nvar before = func() { _ = libc.NewTLS }\n"
	lf, _ := libcFixture(t, map[string]string{"pkg/v.go": lv})
	if v := outGuarded(t, lf, map[string]string{"pkg/v.go": strings.Replace(lv, "before", "after", 1)}); !v.Accepted() || len(v.Blocks) > 0 {
		t.Errorf("H1 a renamed var: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}
