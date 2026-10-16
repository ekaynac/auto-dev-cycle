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
		"tool/mv.go":    "package tool\n\nimport \"flag\"\n\nvar safe = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse4 = safe.Parse\n",
		"tool/val.go":   "package tool\n\nimport \"flag\"\n\nvar w = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc v() {}\n",
		"tool/targ.go":  "package tool\n\nimport \"flag\"\n\ntype fsAlias = flag.FlagSet\n\nfunc g[T any]() {}\n\nfunc h() {}\n",
		"tool/tm.go":    "package tool\n\nimport \"flag\"\n\nfunc TestMain() {\n" + probe + "}\n",
	}
	base["other/a_test.go"] = "package other\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\nfunc TestMain(t *testing.T) {\n" + probe + "}\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"
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
		files map[string]string
		warn  string
	}{
		"a changed body (flag)":              {first("tool/body.go"), "tool/body.go changed bodies that use flag (1: func p)"},
		"a changed body (fmt, main's S)":     {map[string]string{"tool/body.go": strings.Replace(base["tool/body.go"], "\"a\"", "\"b\"", 1)}, "tool/body.go changed bodies that use fmt (1: func q)"},
		"a package-level var literal":        {first("tool/lit.go"), "tool/lit.go changed bodies that use flag (1: var parse)"},
		"a deferred literal in init (r3 #4)": {first("tool/init.go"), "tool/init.go changed bodies that use flag (1: init literal)"},
		"a multi-spec var block (r3 #5)":     {first("tool/multi.go"), "tool/multi.go changed bodies that use flag (1: var _, parse3)"},
		"two func _":                         {first("tool/blank.go"), "tool/blank.go changed bodies that use flag (1: func _)"},
		"two var _ = func":                   {first("tool/bvar.go"), "tool/bvar.go changed bodies that use flag (1: var _)"},
		"a method-value initializer (r3 #7)": {map[string]string{"tool/mv.go": strings.Replace(base["tool/mv.go"], "= safe.Parse", "= fatal.Parse", 1)}, "tool/mv.go changed bodies that use flag (1: var parse4)"},
		"an S method through a value":        {map[string]string{"tool/val.go": strings.Replace(base["tool/val.go"], "func v() {}", "func v() { _ = w.Parse(nil) }", 1)}, "tool/val.go references into flag added, removed, moved or changed (1: v)"},
		"an S type argument":                 {map[string]string{"tool/targ.go": strings.Replace(base["tool/targ.go"], "func h() {}", "func h() { g[fsAlias]() }", 1)}, "tool/targ.go references into flag added, removed, moved or changed (1: h)"},
		"a plain func TestMain()":            {first("tool/tm.go"), "tool/tm.go changed bodies that use flag (1: func TestMain)"},
		"TestMain(*testing.T) in a test":     {first("other/a_test.go"), "other/a_test.go changed bodies that use flag (1: func TestMain)"},
	} {
		v := outGuarded(t, f, c.files)
		if !finds(v.Warnings, c.warn) {
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
	// By types, libc is a warning, never a block (P14b-out-2 adds none).
	tl := "package pkg\n\nimport \"modernc.org/libc\"\n\ntype tlsAlias = libc.TLS\n\nfunc g[T any]() {}\n\nfunc h() {}\n"
	lf, _ := libcFixture(t, map[string]string{"pkg/tl.go": tl})
	if v := outGuarded(t, lf, map[string]string{"pkg/tl.go": strings.Replace(tl, "func h() {}", "func h() { g[tlsAlias]() }", 1)}); !v.Accepted() ||
		!finds(v.Warnings, "pkg/tl.go references into modernc.org/libc added, removed, moved or changed (1: h)") {
		t.Errorf("a libc type argument: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// r3 #6: a 64 KiB name with 5,000 references through a value: its name
	// is bounded once (the run ends, and warns).
	huge := "package tool\n\nimport \"flag\"\n\nvar w2 = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc " + strings.Repeat("n", 64<<10) + "() {\n" + strings.Repeat("\t_ = w2.Parse\n", 5000) + "}\n"
	if v := outGuarded(t, f, map[string]string{"tool/huge.go": huge}); !finds(v.Warnings, "tool/huge.go references into flag added, removed, moved or changed (2: "+strings.Repeat("n", 64)) {
		t.Errorf("r3 #6: %q", whats(v.Warnings))
	}
}
