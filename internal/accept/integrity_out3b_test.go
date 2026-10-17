// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestOutTypedBodies (P14b-out-3b, Opus #280 M4): an init or TestMain body's
// S uses by types outside its function literals, fingerprinted apart, give
// their own Typed entries after the text warnings; the text layer's
// findings, counts and counters stay main's (outGuarded's outSame, outDone).
// Each row is accepted, and silent by types with the types check off (main).
func TestOutTypedBodies(t *testing.T) {
	fs := "import \"flag\"\n\nvar safe = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse func([]string) error\n\nvar hook func()\n"
	base := map[string]string{
		"iv/iv.go":      "package iv\n\n" + fs + "\nfunc init() { parse = safe.Parse }\n",
		"iv2/iv.go":     "package iv2\n\n" + fs,
		"tm/tm.go":      "package tm\n\n" + fs,
		"tm/tm_test.go": "package tm\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tparse = safe.Parse\n\tos.Exit(m.Run())\n}\n\nfunc TestT(t *testing.T) {}\n",
		"mx/mx.go":      "package mx\n\n" + fs + "\nfunc init() {\n\tparse = safe.Parse\n\thook = func() { flag.Parse() }\n}\n",
		"mx/mz.go":      "package mx\n\nvar n int\n\nvar hook3 func()\n\nfunc init() {\n\tn = 1\n\thook3 = func() { _ = safe.Parsed() }\n}\n",
		"mx/my.go":      "package mx\n\nimport \"flag\"\n\nvar parse2 func([]string) error\n\nvar hook2 func()\n\nfunc init() {\n\tparse2 = safe.Parse\n\thook2 = func() { flag.Parse() }\n}\n",
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	edit := func(file, from, to string) map[string]string {
		return map[string]string{file: strings.Replace(base[file], from, to, 1)}
	}
	is := func(v Verdict, what string, typed bool) bool {
		return slices.ContainsFunc(v.Warnings, func(b Block) bool { return b.Path+" "+b.What == what+": review it" && b.Typed == typed })
	}
	body := func(file, bind string) string { return file + " changed bodies that use flag (1: " + bind + ")" }
	for name, c := range map[string]struct {
		files       map[string]string
		typed, text []string
	}{
		"an init body's method value":            {edit("iv/iv.go", "= safe.Parse", "= fatal.Parse"), []string{body("iv/iv.go", "init body")}, nil},
		"an init added":                          {map[string]string{"iv2/iv.go": base["iv2/iv.go"] + "\nfunc init() { parse = fatal.Parse }\n"}, []string{body("iv2/iv.go", "init body")}, nil},
		"a TestMain hook body":                   {edit("tm/tm_test.go", "= safe.Parse", "= fatal.Parse"), []string{body("tm/tm_test.go", "TestMain body")}, nil},
		"a mix: the literal and the body":        {edit("mx/mx.go", "= safe.Parse\n\thook = func() { flag.Parse() }", "= fatal.Parse\n\thook = func() { flag.Parse(); _ = 1 }"), []string{body("mx/mx.go", "init body")}, []string{body("mx/mx.go", "init literal")}},
		"a mix: the body alone":                  {edit("mx/my.go", "= safe.Parse", "= fatal.Parse"), []string{body("mx/my.go", "init body")}, nil},
		"a body without S, its literal by types": {edit("mx/mz.go", "n = 1", "n = 2"), nil, nil},
		"a literal alone":                        {edit("mx/my.go", "flag.Parse() }", "flag.Parse(); _ = 1 }"), nil, []string{body("mx/my.go", "init literal")}},
	} {
		v := outGuarded(t, f, c.files)
		n := len(slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return !b.Typed }))
		if !v.Accepted() || n != len(c.typed) || slices.ContainsFunc(c.typed, func(w string) bool { return !is(v, w, true) }) || slices.ContainsFunc(c.text, func(w string) bool { return !is(v, w, false) }) {
			t.Errorf("%s: blocks %q, warnings %q (%d typed)", name, whats(v.Blocks), whats(v.Warnings), n)
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
