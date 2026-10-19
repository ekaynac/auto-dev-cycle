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
	gst := func(pkg string) string {
		return "package " + pkg + "\n\nimport \"flag\"\n\ntype Safe struct{}\n\ntype FS = flag.FlagSet\n\nfunc g[A, B any]() {}\n\nfunc h[A, B any](func(A), func(B)) {}\n\nvar fa func(X)\n\nvar fb func(Y)\n"
	}
	xy, yx := "type X = Safe\n\ntype Y = FS\n", "type X = FS\n\ntype Y = Safe\n"
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
		// Astra #283 H1: two uses swap their targets; counts and bytes stay.
		"sw/st.go":      st("sw") + "\nvar spare func([]string) error\n",
		"sw/v.go":       "package sw\n\nvar x = safe\n\nvar y = fatal\n",
		"sw/a.go":       "package sw\n\nfunc init() {\n\tparse = x.Parse\n\tspare = y.Parse\n}\n",
		"sm/st.go":      st("sm") + "\nvar spare func([]string) error\n",
		"sm/v.go":       "package sm\n\nvar x = safe\n\nvar y = fatal\n",
		"sm/m_test.go":  "package sm\n\n" + strings.Replace(tmain, "\tos.Exit", "\tspare = y.Parse\n\tos.Exit", 1),
		"sp/st.go":      st("sp") + "\nvar spare func([]string) error\n",
		"sp/v_amd64.go": "package sp\n\nvar x = safe\n\nvar y = fatal\n",
		"sp/v_arm64.go": "package sp\n\nvar x = safe\n\nvar y = fatal\n",
		"sp/a.go":       "package sp\n\nfunc init() {\n\tparse = x.Parse\n\tspare = y.Parse\n}\n",
		"sg/st.go":      "package sg\n\nimport (\n\t\"flag\"\n\t\"io\"\n\t\"log\"\n)\n\nvar fs *flag.FlagSet\n\nvar lg *log.Logger\n\nvar setA, setB func(io.Writer)\n",
		"sg/v.go":       "package sg\n\nvar x = fs\n\nvar y = lg\n",
		"sg/a.go":       "package sg\n\nfunc init() {\n\tsetA = x.SetOutput\n\tsetB = y.SetOutput\n}\n",
		// Astra #283 r2 H1: two inits, swapped on arm64 alone.
		"tw/st.go":      st("tw") + "\nvar spare func([]string) error\n",
		"tw/v_amd64.go": "package tw\n\nvar x = safe\n\nvar y = fatal\n",
		"tw/v_arm64.go": "package tw\n\nvar x = safe\n\nvar y = fatal\n",
		"tw/a.go":       "package tw\n\nfunc init() { parse = x.Parse }\n\nfunc init() { spare = y.Parse }\n",
		// Astra #283 r2 H2: type-argument slots swap, explicit (g) and
		// inferred through composite parameters (h), in init and TestMain.
		"ga/st.go":     gst("ga"),
		"ga/v.go":      "package ga\n\n" + xy,
		"ga/a.go":      "package ga\n\nfunc init() { g[X, Y]() }\n",
		"gi/st.go":     gst("gi"),
		"gi/v.go":      "package gi\n\n" + xy,
		"gi/a.go":      "package gi\n\nfunc init() { h(fa, fb) }\n",
		"gm/st.go":     gst("gm"),
		"gm/v.go":      "package gm\n\n" + xy,
		"gm/m_test.go": "package gm\n\n" + strings.Replace(tmain, "parse = x.Parse", "g[X, Y]()", 1),
		"gn/st.go":     gst("gn"),
		"gn/v.go":      "package gn\n\n" + xy,
		"gn/m_test.go": "package gn\n\n" + strings.Replace(tmain, "parse = x.Parse", "h(fa, fb)", 1),
		// The declarations' boundary: the first of two inits alone changes.
		"t2/i.go": "package t2\n\n" + fs + "\nfunc init() { parse = safe.Parse }\n\nfunc init() { hook = nil }\n",
		// Astra #283 r2 L3: an S use after a literal whose contents alone change.
		"lt/l.go": "package lt\n\n" + fs + "\nfunc init() {\n\thook = func() { flag.Parse() }\n\tparse = fatal.Parse\n}\n",
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
		"Astra H1 an init's uses swap":           {edit("sw/v.go", "x = safe\n\nvar y = fatal", "x = fatal\n\nvar y = safe"), []string{body("sw/a.go", "init body")}, nil, ""},
		"Astra H1 a TestMain's uses swap":        {edit("sm/v.go", "x = safe\n\nvar y = fatal", "x = fatal\n\nvar y = safe"), []string{body("sm/m_test.go", "TestMain body")}, nil, ""},
		"Astra H1 a swap on arm64 alone":         {edit("sp/v_arm64.go", "x = safe\n\nvar y = fatal", "x = fatal\n\nvar y = safe"), []string{body("sp/a.go", "init body")}, nil, ""},
		"r2 H1 two inits swap on arm64 alone":    {edit("tw/v_arm64.go", "x = safe\n\nvar y = fatal", "x = fatal\n\nvar y = safe"), []string{body("tw/a.go", "init body")}, nil, ""},
		"the first of two inits changes":         {edit("t2/i.go", "= safe.Parse", "= fatal.Parse"), []string{body("t2/i.go", "init body")}, nil, ""},
		"a statement moves to the next init":     {edit("t2/i.go", "{ parse = safe.Parse }\n\nfunc init() { hook = nil }", "{}\n\nfunc init() { parse = safe.Parse; hook = nil }"), []string{body("t2/i.go", "init body")}, nil, ""},
		"r2 H2 an init's explicit slots swap":    {map[string]string{"ga/v.go": "package ga\n\n" + yx}, []string{body("ga/a.go", "init body")}, nil, ""},
		"r2 H2 an init's inferred slots swap":    {map[string]string{"gi/v.go": "package gi\n\n" + yx}, []string{body("gi/a.go", "init body")}, nil, ""},
		"r2 H2 a TestMain's explicit slots swap": {map[string]string{"gm/v.go": "package gm\n\n" + yx}, []string{body("gm/m_test.go", "TestMain body")}, nil, ""},
		"r2 H2 a TestMain's inferred slots swap": {map[string]string{"gn/v.go": "package gn\n\n" + yx}, []string{body("gn/m_test.go", "TestMain body")}, nil, ""},
		"r2 L3 an S use after a changed literal": {edit("lt/l.go", "flag.Parse() }", "flag.Parse(); _ = 1 }"), nil, []string{body("lt/l.go", "init literal")}, ""},
		"Astra H1 two S targets swap":            {edit("sg/v.go", "x = fs\n\nvar y = lg", "x = lg\n\nvar y = fs"), []string{body("sg/a.go", "init body"), "sg/a.go changed bodies that use log (1: init body)"}, nil, ""},
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
// budget, deterministically, on every platform (#283's split), as
// outBodyHook sees it once a platform: its typed print at least its node
// kinds and its source bytes; its visit exactly its nodes and each reference
// by types (the bind's and the name's bytes), and the names once.
func TestOutTypedBodyCharge(t *testing.T) {
	const refs = 200
	src := "package bb\n\nimport \"flag\"\n\nvar w *flag.FlagSet\n\nfunc init() {\n" + strings.Repeat("\t_ = w.Parse\n", refs) + "}\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "b.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	d := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	nodes, kinds := 0, 0
	ast.Inspect(d, func(n ast.Node) bool {
		if n != nil {
			nodes, kinds = nodes+1, kinds+len(fmt.Sprintf("\x00%T", n))
		}
		return true
	})
	bytes := fset.Position(d.End()).Offset - fset.Position(d.Pos()).Offset
	visit := nodes + refs*len("init body"+"(*flag.FlagSet).Parse") + len("Parse"+"flag"+"FlagSet")
	var prints, visits []int
	outBodyHook = func(p, v int) { prints, visits = append(prints, p), append(visits, v) }
	defer func() { outBodyHook = nil }()
	v := outGuarded(t, newGuardFixture(t), map[string]string{"bb/b.go": src})
	if !v.Accepted() || len(prints) != len(platforms) || slices.Min(prints) < kinds+bytes || !slices.Equal(visits, []int{visit, visit}) {
		t.Errorf("the body pass charged %v (print), %v (visit); at least %d (%d kinds' bytes, %d bytes), exactly %d (%d nodes, %d references); blocks %q",
			prints, visits, kinds+bytes, kinds, bytes, visit, nodes, refs, whats(v.Blocks))
	}
}
