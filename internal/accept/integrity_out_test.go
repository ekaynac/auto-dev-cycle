// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// outGuarded runs the guard with the layer and without it: the base guard's
// findings must be byte-equal (the layer's isolation; Opus #276 r2 L5).
func outGuarded(t *testing.T, f fixture, files map[string]string, gone ...string) Verdict {
	t.Helper()
	v := f.guarded(t, files, gone...)
	outHook = false
	w := f.guarded(t, files, gone...)
	outHook = true
	base := func(bs []Block) []Block {
		return slices.DeleteFunc(slices.Clone(bs), func(b Block) bool { return b.Additive })
	}
	if !slices.Equal(base(v.Blocks), w.Blocks) || !slices.Equal(base(v.Warnings), w.Warnings) {
		t.Errorf("the base guard's findings differ with the layer on: blocks %q / %q, warnings %d / %d", whats(base(v.Blocks)), whats(w.Blocks), len(base(v.Warnings)), len(w.Warnings))
	}
	return v
}

// outRefs is the layer's aggregated warning for file, package and functions.
func outRefs(file, pkg, fns string) string {
	return file + " references into " + pkg + " added, removed, moved or changed (" + fns + ")"
}

// libcFixture vendors modernc.org/libc (and its sub package), example.org/lib,
// example.com/mx and example.com/m/evil (the gate is offline).
func libcFixture(t *testing.T, extra ...map[string]string) (fixture, string) {
	gomod := guardBase["go.mod"] + "\nrequire (\n\tmodernc.org/libc v1.0.0\n\texample.org/lib v1.0.0\n\texample.com/mx v1.0.0\n\texample.com/m/evil v1.0.0\n)\n"
	return newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{
			"go.mod": gomod,
			"vendor/modules.txt": "# golang.org/x/sys v0.48.0\n## explicit\n# modernc.org/libc v1.0.0\n## explicit\nmodernc.org/libc\nmodernc.org/libc/sub\n" +
				"# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n# example.com/mx v1.0.0\n## explicit\nexample.com/mx\n" +
				"# example.com/m/evil v1.0.0\n## explicit\nexample.com/m/evil\n",
			"vendor/modernc.org/libc/libc.go":    "package libc\n\ntype TLS struct{}\n\nfunc NewTLS() *TLS { return nil }\n\nfunc X_Exit(t *TLS, s int32) {}\n",
			"vendor/modernc.org/libc/sub/sub.go": "package sub\n\nfunc X_exit(s int32) {}\n",
			"vendor/example.org/lib/lib.go":      "package lib\n\nfunc F() int { return 1 }\n",
			"vendor/example.com/mx/mx.go":        "package mx\n\nfunc F() int { return 1 }\n",
			"vendor/example.com/m/evil/evil.go":  "package evil\n\nfunc Exit() {}\n",
		})
		for _, e := range extra {
			write(t, src, e)
		}
		plain(t, src, "add", "--all")
	}), gomod
}

// F (P14b-out-1, #226 entries 75 and 76; operator decision A outside
// initialization and the hook, an additive layer): each reproduced first on
// main (a639412).
func TestDecisionAOut(t *testing.T) {
	f, gomod := libcFixture(t)
	libc := "import \"modernc.org/libc\"\n\nfunc quit() { libc.X_Exit(libc.NewTLS(), 0) }\n"
	for name, c := range map[string]struct {
		file, src, block, warn string
	}{
		"libc.X_Exit in a tested package": {"pkg/out.go", "package pkg\n\n" + libc, "pkg/out.go: modernc.org/libc.X_Exit in quit", ""},
		"libc.X_Exit untested":            {"tool/out.go", "package tool\n\n" + libc, "", outRefs("tool/out.go", "modernc.org/libc", "1: quit")},
		"modernc.org/libc/sub.X_exit":     {"pkg/sub.go", "package pkg\n\nimport \"modernc.org/libc/sub\"\n\nfunc quit() { sub.X_exit(0) }\n", "pkg/sub.go: modernc.org/libc/sub.X_exit in quit", ""},
		"import . \"flag\"":               {"pkg/dot.go", "package pkg\n\nimport . \"flag\"\n\nfunc p() { Parse() }\n", "a dot import of flag", ""},
		"a new import outside std and the module": {"tool/x.go", "package tool\n\nimport \"example.org/lib\"\n\nfunc G() int { return lib.F() }\n", "",
			"a new import of example.org/lib"},
		"a vendored module under the module's path": {"pkg/probe.go", "package pkg\n\nimport \"example.com/m/evil\"\n\nfunc p() { evil.Exit() }\n", "",
			"a new import of example.com/m/evil"},
		"example.com/mx beside example.com/m": {"tool/x.go", "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n", "", "a new import of example.com/mx"},
		"a renamed flag":                      {"tool/r.go", "package tool\n\nimport fl \"flag\"\n\nfunc p() { fl.Parse() }\n", "", outRefs("tool/r.go", "flag", "1: p")},
		"a local shadowing flag": {"tool/l.go", "package tool\n\nimport \"flag\"\n\nvar _ = flag.Parse\n\nfunc p() {\n\tflag := struct{ Parse func() }{func() {}}\n\tflag.Parse()\n}\n", "",
			outRefs("tool/l.go", "flag", "1: package level")},
	} {
		v := outGuarded(t, f, map[string]string{c.file: c.src})
		if c.block != "" && !finds(v.Blocks, c.block) || c.block == "" && !v.Accepted() || c.warn != "" && !finds(v.Warnings, c.warn) || c.warn == "" && c.block == "" && len(v.Warnings) > 0 {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// The module line changed: which imports are own changes with it.
	if v := outGuarded(t, f, map[string]string{"go.mod": strings.Replace(gomod, "module example.com/m", "module example.com", 1),
		"tool/x.go": "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n"}); !finds(v.Blocks, "the module path changed") {
		t.Errorf("a module rename: %q", whats(v.Blocks))
	}
	// Own only if the loader reads it as own in both versions (a nested module merged in).
	nested := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"api/go.mod": "module example.com/m/api\n\ngo 1.26.0\n", "api/a.go": "package api\n\nfunc F() int { return 1 }\n"})
		plain(t, src, "add", "--all")
	})
	if v := outGuarded(t, nested, map[string]string{"tool/n.go": "package tool\n\nimport \"example.com/m/api\"\n\nfunc G() int { return api.F() }\n"}, "api/go.mod"); !finds(v.Warnings, "a new import of example.com/m/api") {
		t.Errorf("a nested module merged in: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// No edge for the standard library (go list's Standard) or the module's own packages.
	if v := outGuarded(t, f, map[string]string{"tool/y.go": "package tool\n\nimport (\n\t\"strings\"\n\n\t\"example.com/m/other\"\n)\n\nvar _ = strings.ToUpper\n\nvar _ = other.Value\n"}); finds(v.Warnings, "outside the standard library") {
		t.Errorf("std and own imports: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// TestDecisionAOutIsolated: the layer never changes the base guard's
// findings (Astra #276 r3 #1-#3), and its findings take only the report's
// capacity main's leave (r2 H2, H3): each against main's on the same input.
func TestDecisionAOutIsolated(t *testing.T) {
	report := func(v Verdict) (blocks, warnings []Block) {
		b, err := v.Report()
		var r struct{ Blocks, Warnings []Block }
		if err != nil || json.Unmarshal(b, &r) != nil {
			t.Fatal(err)
		}
		return r.Blocks, r.Warnings
	}
	out := "package zzz\n\nimport \"fmt\"\n\nfunc out() { fmt.Println(\"x\") }\n"
	g := newGuardFixture(t)

	// r3 #1: x "os" to x "syscall" in 334 functions: main's 668 warnings,
	// no more, and main's zzz/out.go warning reported.
	var os, sys strings.Builder
	os.WriteString("package aaa\n\nimport x \"os\"\n")
	for i := range 334 {
		fmt.Fprintf(&os, "\nfunc f%d() { x.Exit(0) }\n", i)
	}
	sys.WriteString(strings.Replace(os.String(), "import x \"os\"", "import x \"syscall\"", 1))
	base := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"aaa/a.go": os.String()})
		plain(t, src, "add", "--all")
	})
	v := outGuarded(t, base, map[string]string{"aaa/a.go": sys.String(), "zzz/out.go": out})
	if _, w := report(v); len(v.Warnings) != 669 || !finds(w, "zzz/out.go: fmt.Println in out") {
		t.Errorf("r3 #1: %d warnings (main: 669), zzz/out.go reported %t", len(v.Warnings), finds(w, "zzz/out.go: fmt.Println in out"))
	}

	// r3 #2: a ~p directory's type error, and 1000 Additive libc blocks.
	f, _ := libcFixture(t)
	var exits strings.Builder
	exits.WriteString("package pkg\n\nimport \"modernc.org/libc\"\n")
	for i := range 1000 {
		fmt.Fprintf(&exits, "\nfunc f%d() { libc.X_Exit(nil, 0) }\n", i)
	}
	v = outGuarded(t, f, map[string]string{"pkg/a.go": exits.String(), "~p/p.go": "package p\n\nvar x int = \"bad\"\n"})
	if b, _ := report(v); !finds(b, "~p:p unverifiable") || slices.ContainsFunc(v.Blocks, func(b Block) bool { return b.Path == "~p:p" && b.Additive }) {
		t.Errorf("r3 #2: main's ~p block reported %t", finds(b, "~p:p unverifiable"))
	}

	// r3 #3: 250000 method selections and main's zzz/out.go: it is in the
	// verdict and the report.
	pad := "package aaa\n\nimport \"os\"\n\nfunc padding(f *os.File) {\n" + strings.Repeat("\t_ = f.Close\n", 250000) + "}\n"
	v = outGuarded(t, g, map[string]string{"aaa/a.go": pad, "zzz/out.go": out})
	if _, w := report(v); !finds(v.Warnings, "zzz/out.go: fmt.Println in out") || !finds(w, "zzz/out.go: fmt.Println in out") {
		t.Errorf("r3 #3: zzz/out.go in the verdict %t, in the report %t", finds(v.Warnings, "zzz/out.go: fmt.Println in out"), finds(w, "zzz/out.go: fmt.Println in out"))
	}

	// The layer's own budget: past it, one Additive block; main's findings stay.
	old := maxOutNodes
	maxOutNodes = 5 // the file alone has more nodes
	v = outGuarded(t, g, map[string]string{"aaa/a.go": "package aaa\n\nimport \"flag\"\n\nfunc p() { flag.Parse() }\n", "zzz/out.go": out})
	maxOutNodes = old
	over := slices.DeleteFunc(slices.Clone(v.Blocks), func(b Block) bool { return !b.Additive || !strings.Contains(b.What, "over its work bound") })
	if len(over) != 1 || !finds(v.Warnings, "zzz/out.go: fmt.Println in out") || finds(v.Warnings, "references into") { // and the layer stopped
		t.Errorf("the layer's budget: %d Additive over-bound blocks, warnings %q", len(over), whats(v.Warnings))
	}

	// r2 H2: 1000 dot imports of flag, untested, and zzz/out.go.
	files := map[string]string{"zzz/out.go": out}
	for i := range 1000 {
		files[fmt.Sprintf("aaa/a%04d.go", i)] = "package aaa\n\nimport . \"flag\"\n\nvar _ = ContinueOnError\n"
	}
	if _, w := report(outGuarded(t, g, files)); !finds(w, "zzz/out.go: fmt.Println in out") {
		t.Errorf("r2 H2 1000 dot imports: main's warning displaced")
	}
	// r2 H2: 1000 functions calling flag.Parse: one Additive warning.
	var parse strings.Builder
	parse.WriteString("package pkg\n\nimport \"flag\"\n")
	for i := range 1000 {
		fmt.Fprintf(&parse, "\nfunc f%d() { flag.Parse() }\n", i)
	}
	v = outGuarded(t, g, map[string]string{"pkg/a.go": parse.String(), "pkg/z.go": strings.Replace(out, "package zzz", "package pkg", 1)})
	if _, w := report(v); !finds(w, "pkg/z.go: fmt.Println in out") || len(slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return b.Path != "pkg/a.go" })) != 1 {
		t.Errorf("r2 H2 1000 functions: %d warnings", len(v.Warnings))
	}
	// r2 H3: 1000 libc blocks and a main block (the count cap); the verdict blocks.
	v = outGuarded(t, f, map[string]string{"pkg/a.go": exits.String(), "pkg/z.go": "package pkg\n\nimport \"os\"\n\nfunc quit() { os.Exit(0) }\n"})
	if b, _ := report(v); v.Accepted() || !finds(b, "pkg/z.go: os.Exit in quit") {
		t.Errorf("r2 H3 1000 libc blocks: main's block displaced")
	}
	// The report itself: Additive blocks of the widest path take neither main's
	// block's place (the count) nor main's warning's (the byte budget).
	short := slices.Repeat([]Block{{vocab.ReasonTestIntegrityViolation, "pkg/a.go: modernc.org/libc.X_Exit in f", "x", true}}, maxReported)
	if b, _ := report(Verdict{Blocks: append(short, Block{vocab.ReasonTestIntegrityViolation, "pkg/z.go: os.Exit in quit", "main's", false})}); !finds(b, "pkg/z.go: os.Exit in quit") {
		t.Errorf("the report's count: main's block displaced")
	}
	wide := slices.Repeat([]Block{{vocab.ReasonTestIntegrityViolation, strings.Repeat("p", maxField), "x", true}}, maxReported)
	mainW := Block{vocab.ReasonTestIntegrityViolation, "pkg/z.go: fmt.Println in out" + strings.Repeat("x", maxField), strings.Repeat("main's ", 20), false}
	if _, w := report(Verdict{Blocks: wide, Warnings: []Block{mainW}}); !finds(w, "pkg/z.go: fmt.Println in out") {
		t.Errorf("the report's bytes: main's warning displaced")
	}
}

// TestDecisionAOutRound6: Opus #276 r2 on a9d8f28, each probe a row.
func TestDecisionAOutRound6(t *testing.T) {
	f, _ := libcFixture(t)
	libc := "import \"modernc.org/libc\"\n\nfunc quit() { libc.X_Exit(nil, 0) }\n"
	probe := "fs := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = fs.Parse([]string{\"-h\"})"
	for name, c := range map[string]struct {
		files       map[string]string
		block, warn string
	}{
		// H1: a negated tag the guard does not model: some assignment builds it.
		"A1 !race libc":   {map[string]string{"pkg/a.go": "//go:build !race\n\npackage pkg\n\n" + libc}, "modernc.org/libc.X_Exit in quit", ""},
		"A2 !arm64 libc":  {map[string]string{"pkg/a.go": "//go:build !arm64\n\npackage pkg\n\n" + libc}, "modernc.org/libc.X_Exit in quit", ""},
		"A3 !cgo libc":    {map[string]string{"pkg/a.go": "//go:build !cgo\n\npackage pkg\n\n" + libc}, "modernc.org/libc.X_Exit in quit", ""},
		"A9 !amd64 dot":   {map[string]string{"pkg/d.go": "//go:build !amd64\n\npackage pkg\n\nimport . \"flag\"\n\nvar _ = ContinueOnError\n"}, "a dot import of flag", ""},
		"A10 !race probe": {map[string]string{"pkg/x_test.go": "//go:build !race\n\npackage pkg\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\nfunc TestProbe(t *testing.T) {\n\t" + probe + "\n}\n"}, "", outRefs("pkg/x_test.go", "flag", "1: TestProbe")},
		// M1: a literal assigned in init runs later, from code the tests call.
		"S1 libc in an init literal": {map[string]string{"pkg/i.go": "package pkg\n\nimport \"modernc.org/libc\"\n\nvar parse func()\n\nfunc init() { parse = func() { libc.X_Exit(nil, 0) } }\n"}, "modernc.org/libc.X_Exit in init literal", ""},
		"S2 flag in an init literal": {map[string]string{"pkg/i.go": "package pkg\n\nimport \"flag\"\n\nvar parse func()\n\nfunc init() {\n\tparse = func() {\n\t\t" + probe + "\n\t}\n}\n"}, "", outRefs("pkg/i.go", "flag", "1: init literal")},
		// L1: go list's Standard on any gate platform.
		"N1 strings in an arm64 file": {map[string]string{"tool/x_arm64.go": "package tool\n\nimport \"strings\"\n\nvar _ = strings.ToUpper\n"}, "", ""},
	} {
		v := outGuarded(t, f, c.files)
		if c.block != "" && !finds(v.Blocks, c.block) || c.block == "" && !v.Accepted() || c.warn != "" && !finds(v.Warnings, c.warn) || c.warn == "" && c.block == "" && len(v.Warnings) > 0 {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}

	// M4: a new use through an existing dot import of flag.
	dot := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/hold.go": "package pkg\n\nimport . \"flag\"\n\nvar _ = ContinueOnError\n"})
		plain(t, src, "add", "--all")
	})
	if v := outGuarded(t, dot, map[string]string{"pkg/hold.go": "package pkg\n\nimport . \"flag\"\n\nvar _ = ContinueOnError\n\nfunc P() {\n\tfs := NewFlagSet(\"x\", ExitOnError)\n\t_ = fs.Parse([]string{\"-h\"})\n}\n"}); !finds(v.Warnings, "references through a dot import of flag may have changed") {
		t.Errorf("S3 a use through a dot import: %q", whats(v.Warnings))
	}
	// L4: a reference moved between methods of one name keeps no key.
	recv := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"tool/m.go": "package tool\n\nimport \"flag\"\n\ntype A struct{}\n\ntype B struct{}\n\nfunc (A) M() { flag.Parse() }\n\nfunc (B) M() {}\n"})
		plain(t, src, "add", "--all")
	})
	if v := outGuarded(t, recv, map[string]string{"tool/m.go": "package tool\n\nimport \"flag\"\n\ntype A struct{}\n\ntype B struct{}\n\nfunc (A) M() {}\n\nfunc (B) M() { flag.Parse() }\n"}); !finds(v.Warnings, outRefs("tool/m.go", "flag", "2: A.M, B.M")) {
		t.Errorf("L4 a moved reference: %q", whats(v.Warnings))
	}
	// H2 (and Astra #276 r4 H2): 18 MiB of generated Go beside a one-line
	// change: accepted.
	gen := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		for _, n := range []string{"gen1", "gen2", "gen3", "gen4", "gen5", "gen6"} {
			write(t, src, map[string]string{"tool/" + n + ".go": "package tool\n\nvar " + n + " = []int{" + strings.Repeat("0,", 3<<19) + "}\n"})
		}
		plain(t, src, "add", "--all")
	})
	if v := outGuarded(t, gen, map[string]string{"tool/x.go": "package tool\n\nvar x = 1\n"}); !v.Accepted() {
		t.Errorf("C1 a change beside 18 MiB: %q", whats(v.Blocks))
	}
	// L5 probe E: 1101 layer warnings on long paths and two libc blocks:
	// the blocks are reported.
	long := strings.Repeat("d", 250) + "/" + strings.Repeat("e", 250) + "/" + strings.Repeat("f", 250)
	files := map[string]string{"pkg/" + strings.Repeat("l", 200) + ".go": "package pkg\n\nimport \"modernc.org/libc\"\n\nfunc a() { libc.X_Exit(nil, 0) }\n\nfunc b() { libc.X_Exit(nil, 0) }\n"}
	for i := range 1101 {
		files[fmt.Sprintf("%s/%s%04d.go", long, strings.Repeat("g", 200), i)] = fmt.Sprintf("package ff\n\nimport \"flag\"\n\nfunc p%d() { flag.Parse() }\n", i)
	}
	v := f.guarded(t, files)
	b, err := v.Report()
	var rep struct{ Blocks []Block }
	if err != nil || json.Unmarshal(b, &rep) != nil {
		t.Fatal(err)
	}
	if n := len(slices.DeleteFunc(rep.Blocks, func(b Block) bool { return !strings.Contains(b.Path, "modernc.org/libc.X_Exit") })); n != 2 {
		t.Errorf("E: %d of 2 libc blocks reported; verdict blocks %d", n, len(v.Blocks))
	}
}

// TestDecisionAOutRound7: Astra #276 r4 on 6d271a1, each input a row.
func TestDecisionAOutRound7(t *testing.T) {
	f, _ := libcFixture(t)
	// H1: 1,001 nested blocks, which object resolution rejects and the base
	// guard's parser mode accepts.
	nest := strings.Repeat("{", 1001) + strings.Repeat("}", 1001)
	if v := outGuarded(t, f, map[string]string{"pkg/out.go": "package pkg\n\nimport \"modernc.org/libc\"\n\nfunc quit() { libc.X_Exit(nil, 0) }\n\nfunc padding() " + nest + "\n"}); !finds(v.Blocks, "pkg/out.go: modernc.org/libc.X_Exit in quit") {
		t.Errorf("H1 1,001 nested blocks: %q", whats(v.Blocks))
	}
	// H2: 5,000 identical contradictory +build lines: never built, accepted.
	off := strings.Repeat("// +build a,!a b,!b c,!c d,!d e,!e f,!f g,!g h,!h i,!i j,!j k,!k l,!l\n", 5000) + "\npackage tool\n"
	if v := outGuarded(t, f, map[string]string{"tool/off.go": off}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("H2 5,000 +build lines: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// H3: a default import name is the package's declared name.
	shim, _ := libcFixture(t, map[string]string{"vendor/modernc.org/libc/sub/sub.go": "package shim\n\nfunc X_exit(int32) {}\n",
		"pkg/keep.go": "package pkg\n\nimport \"modernc.org/libc/sub\"\n\nfunc keep() { _ = shim.X_exit }\n"})
	if v := outGuarded(t, shim, map[string]string{"pkg/keep.go": "package pkg\n\nimport \"modernc.org/libc/sub\"\n\nfunc keep() { _ = shim.X_exit }\n\nfunc quit() { shim.X_exit(0) }\n"}); !finds(v.Blocks, "pkg/keep.go: modernc.org/libc/sub.X_exit in quit") {
		t.Errorf("H3 package shim: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// H1: a parameter, a range variable and a := shadow the import name in
	// their scopes only.
	if v := outGuarded(t, f, map[string]string{"tool/s.go": "package tool\n\nimport \"flag\"\n\ntype fs struct{ Parse func() }\n\nfunc a(flag fs) { flag.Parse() }\n\n" +
		"func b(xs []fs) {\n\tfor _, flag := range xs {\n\t\tflag.Parse()\n\t}\n\tif flag := (fs{}); flag.Parse != nil {\n\t\tflag.Parse()\n\t}\n}\n\nfunc c() { flag.Parse() }\n"}); !finds(v.Warnings, outRefs("tool/s.go", "flag", "1: c")) {
		t.Errorf("H1 scopes: %q", whats(v.Warnings))
	}
}

// TestOutReadAndCancel: the layer reads no non-Go blob (Opus #276 r2 M3,
// probe B1), and a done context stops it with no block (M2, probe B2).
func TestOutReadAndCancel(t *testing.T) {
	var read []string
	r := &run{ctx: context.Background(), budget: maxRead, blobs: map[string][]byte{}, out: additive{files: map[string]*outFile{}, built: map[string]bool{}},
		read: func(_ context.Context, blob string, _ int64) ([]byte, bool, error) {
			read = append(read, blob)
			return []byte("package tool\n"), true, nil
		}}
	tr := &tree{files: map[string]string{"tool/README.md": "readme", "tool/x.go": "x"}, dirs: map[string][]string{"tool": {"tool/README.md", "tool/x.go"}}}
	r.outRead("tool", tr)
	if !slices.Equal(read, []string{"x"}) {
		t.Errorf("B1: blobs read %q", read)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = &run{ctx: ctx, touched: map[string]bool{"tool": true}, trees: [2]*tree{tr, tr}, budget: maxRead, blobs: map[string][]byte{}, read: r.read}
	if err := r.outRun(); err == nil || len(r.out.blocks) > 0 {
		t.Errorf("B2: %v, blocks %q", err, whats(r.out.blocks))
	}
}
