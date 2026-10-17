// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// TestOutTypedBodies (P14b-out-3b, Opus #280 M4): an init or TestMain body's
// S uses by types outside its function literals, fingerprinted apart, and
// its references by types (Opus #283 H1) give their own Typed entries after
// the text warnings; the text layer's findings, counts and counters stay
// main's (outGuarded's outSame, outDone). Each row is accepted; its lists of
// the layer's Typed and text entries are exact (Opus #283 L3).
func TestOutTypedBodies(t *testing.T) {
	fs := "import \"flag\"\n\nvar safe = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse func([]string) error\n\nvar hook func()\n"
	// Opus #283 H1: x's type changes, the body's bytes do not.
	st := func(pkg string) string {
		return "package " + pkg + "\n\nimport \"flag\"\n\ntype T struct{}\n\nfunc (*T) Parse([]string) error { return nil }\n\ntype FS = flag.FlagSet\n\nvar safe = &T{}\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse func([]string) error\n"
	}
	tmain := "import (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tparse = x.Parse\n\tos.Exit(m.Run())\n}\n"
	base := map[string]string{
		"iv/iv.go":      "package iv\n\n" + fs + "\nfunc init() { parse = safe.Parse }\n",
		"iv2/iv.go":     "package iv2\n\n" + fs,
		"tm/tm.go":      "package tm\n\n" + fs,
		"tm/tm_test.go": "package tm\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tparse = safe.Parse\n\tos.Exit(m.Run())\n}\n\nfunc TestT(t *testing.T) {}\n",
		"mx/mx.go":      "package mx\n\n" + fs + "\nfunc init() {\n\tparse = safe.Parse\n\thook = func() { flag.Parse() }\n}\n",
		"mx/mz.go":      "package mx\n\nvar n int\n\nvar hook3 func()\n\nfunc init() {\n\tn = 1\n\thook3 = func() { _ = safe.Parsed() }\n}\n",
		"mx/my.go":      "package mx\n\nimport \"flag\"\n\nvar parse2 func([]string) error\n\nvar hook2 func()\n\nfunc init() {\n\tparse2 = safe.Parse\n\thook2 = func() { flag.Parse() }\n}\n",
		"ua/st.go":      st("ua"),
		"ua/v.go":       "package ua\n\nvar x = safe\n",
		"ua/a.go":       "package ua\n\nfunc init() { parse = x.Parse }\n",
		"ub/st.go":      st("ub"),
		"ub/a.go":       "package ub\n\nvar x = safe\n\nfunc init() { parse = x.Parse }\n",
		"um/st.go":      st("um"),
		"um/v.go":       "package um\n\nvar x = safe\n",
		"um/m_test.go":  "package um\n\n" + tmain,
		"un/st.go":      st("un"),
		"un/m_test.go":  "package un\n\n" + tmain + "\nvar x = safe\n",
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	edit := func(file, from, to string) map[string]string {
		return map[string]string{file: strings.Replace(base[file], from, to, 1)}
	}
	layer := func(v Verdict, typed bool) []string { // the layer's entries, Typed or text, sorted
		var out []string
		for _, b := range v.Warnings {
			if b.Additive && b.Typed == typed {
				out = append(out, b.Path+" "+strings.TrimSuffix(b.What, ": review it"))
			}
		}
		return slices.Sorted(slices.Values(out))
	}
	body := func(file, bind string) string { return file + " changed bodies that use flag (1: " + bind + ")" }
	refs := func(file, bind string) string { return outRefs(file, "flag", "1: "+bind) }
	same := map[string]string{"var x = safe": "var y *FS\n\nvar x = y"} // the type changed in the body's own file
	for name, c := range map[string]struct {
		files       map[string]string
		typed, text []string
		base        string // a warning of the base guard's
	}{
		"an init body's method value":            {edit("iv/iv.go", "= safe.Parse", "= fatal.Parse"), []string{body("iv/iv.go", "init body")}, nil, ""},
		"an init added":                          {map[string]string{"iv2/iv.go": base["iv2/iv.go"] + "\nfunc init() { parse = fatal.Parse }\n"}, []string{refs("iv2/iv.go", "init body")}, nil, ""},
		"a TestMain hook body":                   {edit("tm/tm_test.go", "= safe.Parse", "= fatal.Parse"), []string{body("tm/tm_test.go", "TestMain body")}, nil, "tm: TestMain(*testing.M) of tm:tm changed or deleted"},
		"a mix: the literal and the body":        {edit("mx/mx.go", "= safe.Parse\n\thook = func() { flag.Parse() }", "= fatal.Parse\n\thook = func() { flag.Parse(); _ = 1 }"), []string{body("mx/mx.go", "init body")}, []string{body("mx/mx.go", "init literal")}, ""},
		"a mix: the body alone":                  {edit("mx/my.go", "= safe.Parse", "= fatal.Parse"), []string{body("mx/my.go", "init body")}, nil, ""},
		"a body without S, its literal by types": {edit("mx/mz.go", "n = 1", "n = 2"), nil, nil, ""},
		"a literal alone":                        {edit("mx/my.go", "flag.Parse() }", "flag.Parse(); _ = 1 }"), nil, []string{body("mx/my.go", "init literal")}, ""},
		"H1 init, the type in another file":      {edit("ua/v.go", "= safe", "= fatal"), []string{refs("ua/a.go", "init body")}, nil, ""},
		"H1 init, the type in its own file":      {edit("ub/a.go", "var x = safe", same["var x = safe"]), []string{refs("ub/a.go", "init body")}, nil, ""},
		"H1 TestMain, the type in another file":  {edit("um/v.go", "= safe", "= fatal"), []string{refs("um/m_test.go", "TestMain body")}, nil, ""},
		"H1 TestMain, the type in its own file":  {edit("un/m_test.go", "var x = safe", same["var x = safe"]), []string{refs("un/m_test.go", "TestMain body")}, nil, ""},
	} {
		v := outGuarded(t, f, c.files)
		if !v.Accepted() || !slices.Equal(layer(v, true), slices.Sorted(slices.Values(c.typed))) || !slices.Equal(layer(v, false), slices.Sorted(slices.Values(c.text))) ||
			c.base != "" && !slices.ContainsFunc(v.Warnings, func(b Block) bool { return !b.Additive && strings.Contains(b.Path+": "+b.What, c.base) }) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// The text layer's counters with the types check on and off (its
	// fingerprints are on the types budget), all rows at once.
	all, counters := map[string]string{}, []string{}
	for _, k := range []string{"iv/iv.go", "tm/tm_test.go", "mx/mx.go", "mx/my.go"} {
		all[k] = strings.NewReplacer("= safe.Parse", "= fatal.Parse", "flag.Parse() }", "flag.Parse(); _ = 1 }").Replace(base[k])
	}
	outDone = func(nodes, tags int, over bool) { counters = append(counters, fmt.Sprint(nodes, tags, over)) }
	on := f.guarded(t, all)
	outTypesHook = false
	off := f.guarded(t, all)
	outTypesHook, outDone = true, nil
	outSame(t, on, off)
	if len(counters) != 2 || counters[0] != counters[1] || len(on.Warnings)-len(off.Warnings) != 4 {
		t.Errorf("the text layer's counters, types on and off: %q; warnings %q", counters, whats(on.Warnings))
	}
	// The budget: past maxOutTypeWork, the types stop warning alone.
	defer func(w int) { maxOutTypeWork = w }(maxOutTypeWork)
	maxOutTypeWork = 1
	v := outGuarded(t, f, edit("iv/iv.go", "= safe.Parse", "= fatal.Parse"))
	if typed := slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return !b.Typed }); !v.Accepted() || len(typed) != 1 || !strings.Contains(typed[0].What, "check by types stopped") {
		t.Errorf("the budget: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// TestOutTypedBodyCharge (Opus #283 L2): a body pass charges the types
// budget, deterministically: its fingerprint's nodes and bytes, its visit's
// nodes, and each reference by types (the bind's and the name's bytes), as
// outBodyHook sees it on the first platform, where the fingerprint is made.
func TestOutTypedBodyCharge(t *testing.T) {
	const refs = 200
	src := "package bb\n\nimport \"flag\"\n\nvar w *flag.FlagSet\n\nfunc init() {\n" + strings.Repeat("\t_ = w.Parse\n", refs) + "}\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "b.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	d := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	nodes := 0
	ast.Inspect(d, func(n ast.Node) bool { nodes += b2i(n != nil); return true })
	bytes := fset.Position(d.End()).Offset - fset.Position(d.Pos()).Offset
	least := 2*nodes + bytes + refs*len("init body"+"(*flag.FlagSet).Parse") // and the names, once: len("Parse"+"flag"+"FlagSet")
	var charged []int
	outBodyHook = func(n int) { charged = append(charged, n) }
	defer func() { outBodyHook = nil }()
	v := outGuarded(t, newGuardFixture(t), map[string]string{"bb/b.go": src})
	if most := slices.Max(append(charged, 0)); !v.Accepted() || most < least || most > least+len("Parse"+"flag"+"FlagSet") {
		t.Errorf("the body pass charged %v; at least %d (%d nodes twice, %d bytes, %d references); blocks %q", charged, least, nodes, bytes, refs, whats(v.Blocks))
	}
}
