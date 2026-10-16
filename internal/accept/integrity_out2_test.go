// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"strings"
	"testing"
	"time"
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
	base["other/a_test.go"] = "package other\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\nfunc TestMain(t *testing.T) { fmt.Println(\"a\") }\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n" // its text alone uses S
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
		"TestMain(*testing.T) in a test":     {map[string]string{"other/a_test.go": strings.Replace(base["other/a_test.go"], "\"a\"", "\"b\"", 1)}, "other/a_test.go changed bodies that use fmt (1: func TestMain)"},
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
	oldNodes := maxOutNodes
	maxOutNodes = 8 << 20 // its name bounded once: far under; bounded for each reference, over
	v := outGuarded(t, f, map[string]string{"tool/huge.go": huge})
	maxOutNodes = oldNodes
	if !finds(v.Warnings, "tool/huge.go references into flag added, removed, moved or changed (2: "+strings.Repeat("n", 64)) || finds(v.Warnings, "stopped at its work bound") {
		t.Errorf("r3 #6: %q", whats(v.Warnings))
	}
}

// TestOutTypedBounded: the lead's conditions on #280. 1: the layer's type
// check is bounded as the base guard's (typeCheck); past it the layer stops
// (its warning), never a silent skip. 2: its importer loads nothing the base
// guard did not, within maxOutExport; an import it cannot serve stops it.
func TestOutTypedBounded(t *testing.T) {
	src := "package tool\n\nfunc v() {}\n" // it checks clean: only the gate stops it
	r := &run{ctx: context.Background(), budget: maxRead, blobs: map[string][]byte{}, listEnd: time.Now().Add(time.Minute),
		out:  additive{files: map[string]*outFile{}, built: map[string]int{}, named: map[string]map[string]bool{}, importers: map[*typer]*outImporter{}},
		read: func(context.Context, string, int64) ([]byte, bool, error) { return []byte(src), true, nil }}
	tr := &tree{files: map[string]string{"tool/x.go": "x"}, dirs: map[string][]string{"tool": {"tool/x.go"}}, typers: map[string]*typer{}}
	r.trees = [2]*tree{tr, tr}
	outRunning.Add(1) // a dropped check still runs: the layer checks nothing more
	r.outRead("tool", tr)
	outRunning.Add(-1)
	if !r.out.over {
		t.Error("1: a dropped check still running: the layer did not stop")
	}
	// 2: nothing listed, nothing loaded: an import fails, and is told.
	im := (&run{out: additive{fset: token.NewFileSet(), importers: map[*typer]*outImporter{}}}).outImporter(&typer{listed: map[string]Listed{}, pkgs: map[string]*types.Package{}, self: map[string]*types.Package{}})
	if _, err := im.Import("flag"); err == nil || !strings.Contains(im.failed, "not listed by the base guard") {
		t.Errorf("2: an unlisted import: %v, %q", err, im.failed)
	}
	f, _ := libcFixture(t)
	// 1: a check past its bound (maxOutCheck) is dropped: the stop warning;
	// the base guard's own accounting (abandoned) is not touched.
	oldCheck := maxOutCheck
	maxOutCheck = time.Nanosecond
	v := outGuarded(t, f, map[string]string{"tool/c.go": "package tool\n\n" + aliasChain("A", 20) + aliasChain("B", 20) + "var _ = A20(B20{})\n"})
	maxOutCheck = oldCheck
	if !v.Accepted() || !finds(v.Warnings, "stopped at its work bound") || abandoned.Load() != 0 {
		t.Errorf("1: past maxOutCheck: blocks %q, warnings %q, %d abandoned", whats(v.Blocks), whats(v.Warnings), abandoned.Load())
	}
	for end := time.Now().Add(time.Minute); outRunning.Load() > 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond) // the dropped check ends
	}
	old := maxOutExport
	maxOutExport = 1
	v = outGuarded(t, f, map[string]string{"tool/x.go": "package tool\n\nimport \"flag\"\n\nfunc v() {\n\tw := flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\t_ = w.Parse(nil)\n}\n"})
	maxOutExport = old
	if !v.Accepted() || !finds(v.Warnings, "decision A's outside-initialization check stopped at its work bound") {
		t.Errorf("2: the export bound: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// aliasChain is n nested struct aliases of name (P0 = struct{ v int }, Pi =
// struct{ a, b P(i-1) }), whose identity with another such chain costs 2^n.
func aliasChain(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type %s0 = struct{ v int }\n\n", name)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "type %s%d = struct{ a, b %s%d }\n\n", name, i, name, i-1)
	}
	return b.String()
}

// TestDecisionAOut2Round2: Opus's pre-review of #280 (640c111), each probe a
// row, silent or stalling on 640c111.
func TestDecisionAOut2Round2(t *testing.T) {
	probe := "\tfs := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = fs.Parse(nil)\n"
	h := strings.Replace(probe, "Parse(nil)", "Parse([]string{\"-h\"})", 1)
	base := map[string]string{
		// H1: each platform's files alone are cheap; their union costs 2^28.
		"slow/a.go":       "package slow\n\n" + aliasChain("A", 28),
		"slow/b_amd64.go": "package slow\n\n" + aliasChain("B", 28) + "type Q = B28\n",
		// H2: only/'s amd64 file costs 2^30 to check; main loads only/ on arm64 alone.
		"only/a.go":       "package only\n\nvar X = 1\n\n" + aliasChain("A", 30) + aliasChain("B", 30),
		"only/s_amd64.go": "package only\n\nvar _ = A30(B30{})\n",
		// M1: an arm64-only file.
		"arm/x_arm64.go": "package arm\n\nimport \"flag\"\n\nvar w = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc v() {}\n",
		// M2: a value from export_test.go, used by the external test.
		"xt/x.go":           "package xt\n\nimport \"flag\"\n\nvar fs = flag.NewFlagSet(\"x\", flag.ContinueOnError)\n",
		"xt/export_test.go": "package xt\n\nvar FS = fs\n",
		"xt/a_test.go":      "package xt_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/xt\"\n)\n\nfunc helper() {}\n\nfunc TestX(t *testing.T) { helper(); _ = xt.FS }\n",
		// M3: a var spec reshaped.
		"m3/a.go": "package m3\n\nimport (\n\t\"flag\"\n\t\"fmt\"\n)\n\nvar parse7 = func() {\n" + probe + "}\n\nvar hello = func() { fmt.Println(\"a\") }\n",
		// M4: a method value rebound in init.
		"iv/iv.go": "package iv\n\nimport \"flag\"\n\nvar safe2 = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal2 = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse5 func([]string) error\n\nfunc init() { parse5 = safe2.Parse }\n",
		// L1: an os read by types is inert, as the base guard's text rule has it.
		"osr/f.go": "package osr\n\nimport \"os\"\n\nvar F *os.File\n",
		"osr/r.go": "package osr\n\nfunc rd() int {\n\tfi, _ := F.Stat()\n\treturn int(fi.Size()) + 1\n}\n",
		// L3: the suppression, and a pointer type argument.
		"l3/a.go": "package l3\n\nimport \"flag\"\n\ntype pa = flag.FlagSet\n\nfunc gn[T any]() {}\n\nfunc h2() {}\n\nfunc p() {\n" + probe + "}\n",
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	stopped := func(v Verdict) bool { return finds(v.Warnings, "stopped at its work bound") }
	old := maxOutCheck
	maxOutCheck = 2 * time.Second // the union, if checked, is dropped: the stop warning
	if v := outGuarded(t, f, map[string]string{"slow/g_arm64.go": "package slow\n\ntype Q = A28\n\nvar _ = A28(Q{})\n"}); !v.Accepted() || stopped(v) {
		t.Errorf("H1 the union: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	maxOutCheck = old
	// H2: run 1, then an ordinary run 2: both as main's, nothing left running.
	if v := outGuarded(t, f, map[string]string{"zzz/z_arm64.go": "package zzz\n\nimport \"example.com/m/only\"\n\nvar _ = only.X\n"}); !v.Accepted() || stopped(v) || abandoned.Load() != 0 || outRunning.Load() != 0 {
		t.Errorf("H2 run 1: blocks %q, warnings %q, %d abandoned, %d dropped", whats(v.Blocks), whats(v.Warnings), abandoned.Load(), outRunning.Load())
	}
	if v := outGuarded(t, f, map[string]string{"tool2/x.go": "package tool2\n\nfunc F() {}\n"}); !v.Accepted() || len(v.Blocks) > 0 {
		t.Errorf("H2 run 2: blocks %q", whats(v.Blocks))
	}
	for name, c := range map[string]struct {
		files       map[string]string
		warn, never string
	}{
		"M1 an arm64-only file": {map[string]string{"arm/x_arm64.go": strings.Replace(base["arm/x_arm64.go"], "func v() {}", "func v() { _ = w.Parse(nil) }", 1)},
			"arm/x_arm64.go references into flag added, removed, moved or changed (1: v)", ""},
		"M2 export_test.go's value": {map[string]string{"xt/a_test.go": strings.Replace(base["xt/a_test.go"], "func helper() {}", "func helper() { _ = xt.FS.Parse(nil) }", 1)},
			"xt/a_test.go references into flag added, removed, moved or changed (1: helper)", ""},
		"M3 a reshaped var spec (flag)": {map[string]string{"m3/a.go": strings.Replace(strings.Replace(base["m3/a.go"], "var parse7 = func() {\n"+probe+"}", "var parse7, _ = func() {\n"+h+"}, 0", 1), "", "", 0)},
			"m3/a.go references into flag added, removed, moved or changed (2: var parse7, var parse7, _)", ""},
		"M3 a reshaped var spec (fmt)": {map[string]string{"m3/a.go": strings.Replace(base["m3/a.go"], "var hello = func() { fmt.Println(\"a\") }", "var hello, _ = func() { fmt.Println(\"b\") }, 0", 1)},
			"m3/a.go changed bodies that use fmt (2: var hello, var hello, _)", ""},
		"M4 a method value rebound in init": {map[string]string{"iv/iv.go": strings.Replace(base["iv/iv.go"], "= safe2.Parse", "= fatal2.Parse", 1)},
			"iv/iv.go changed bodies that use flag (1: func init)", ""},
		"L3 references changed: no body warning too": {map[string]string{"l3/a.go": strings.Replace(base["l3/a.go"], "flag.ExitOnError", "flag.ContinueOnError", 1)},
			"l3/a.go references into flag added, removed, moved or changed (1: p)", "l3/a.go changed bodies that use flag"},
		"L1 an os read by types": {map[string]string{"osr/r.go": strings.Replace(base["osr/r.go"], "+ 1", "+ 2", 1)}, "", "osr/r.go changed bodies that use os"},
		"L3 a pointer type argument": {map[string]string{"l3/a.go": strings.Replace(base["l3/a.go"], "func h2() {}", "func h2() { gn[*pa]() }", 1)},
			"l3/a.go references into flag added, removed, moved or changed (1: h2)", ""},
	} {
		v := outGuarded(t, f, c.files)
		if c.warn != "" && !finds(v.Warnings, c.warn) || c.never != "" && finds(v.Warnings, c.never) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
}
