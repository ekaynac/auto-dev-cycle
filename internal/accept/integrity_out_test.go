// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path"
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

	// The layer's own budget: past it, one Additive warning (round 8);
	// main's findings stay.
	old := maxOutNodes
	maxOutNodes = 5 // the file alone has more nodes
	v = outGuarded(t, g, map[string]string{"aaa/a.go": "package aaa\n\nimport \"flag\"\n\nfunc p() { flag.Parse() }\n", "zzz/out.go": out})
	maxOutNodes = old
	stopped := slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return !b.Additive || !strings.Contains(b.What, "stopped at its work bound") })
	if !v.Accepted() || len(stopped) != 1 || !finds(v.Warnings, "zzz/out.go: fmt.Println in out") || finds(v.Warnings, "references into") { // and the layer stopped
		t.Errorf("the layer's budget: blocks %q, %d stop warnings, warnings %q", whats(v.Blocks), len(stopped), whats(v.Warnings))
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
	short := slices.Repeat([]Block{{vocab.ReasonTestIntegrityViolation, "pkg/a.go: modernc.org/libc.X_Exit in f", "x", true, false}}, maxReported)
	if b, _ := report(Verdict{Blocks: append(short, Block{vocab.ReasonTestIntegrityViolation, "pkg/z.go: os.Exit in quit", "main's", false, false})}); !finds(b, "pkg/z.go: os.Exit in quit") {
		t.Errorf("the report's count: main's block displaced")
	}
	wide := slices.Repeat([]Block{{vocab.ReasonTestIntegrityViolation, strings.Repeat("p", maxField), "x", true, false}}, maxReported)
	mainW := Block{vocab.ReasonTestIntegrityViolation, "pkg/z.go: fmt.Println in out" + strings.Repeat("x", maxField), strings.Repeat("main's ", 20), false, false}
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
	// H2 (and Astra #276 r4 H2): six generated files that fill most of the
	// base guard's read bound, beside a one-line change: accepted. The
	// guard's envelope is scaled by 1/256 (maxRead 256 KiB, maxWork 64 Ki),
	// so the layer's bounds scale with it; at full scale this is six 3 MiB
	// files (about 280 s).
	oldRead, oldWork := maxRead, maxWork
	t.Cleanup(func() { maxRead, maxWork = oldRead, oldWork })
	maxRead, maxWork = 256<<10, 1<<16
	gen := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		for _, n := range []string{"gen1", "gen2", "gen3", "gen4", "gen5", "gen6"} {
			write(t, src, map[string]string{"tool/" + n + ".go": "package tool\n\nvar " + n + " = []int{" + strings.Repeat("0,", 18000) + "}\n"})
		}
		plain(t, src, "add", "--all")
	})
	if v := outGuarded(t, gen, map[string]string{"tool/x.go": "package tool\n\nvar x = 1\n"}); !v.Accepted() || len(v.Warnings) > 0 { // not even the layer's stop warning
		t.Errorf("C1 a change beside 216 KB at a 256 KiB read bound: %q, %q", whats(v.Blocks), whats(v.Warnings))
	}
	maxRead, maxWork = oldRead, oldWork
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

// TestDecisionAOutRound8: Astra #276 r5 on 8830861, scaled by the
// test-only envelope, and the lead's rule: the layer's own bound warns,
// and blocks only with a libc import in a changed file of a tested package.
func TestDecisionAOutRound8(t *testing.T) {
	oldRead, oldWork, oldNodes := maxRead, maxWork, maxOutNodes
	t.Cleanup(func() { maxRead, maxWork, maxOutNodes = oldRead, oldWork, oldNodes })
	onlyStopped := func(name string, v Verdict) {
		t.Helper()
		if !v.Accepted() || len(v.Warnings) > 1 || len(v.Warnings) == 1 && !strings.Contains(v.Warnings[0].What, "stopped at its work bound") {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// #1: 70 directories share one blob whose function name is 32 KiB;
	// x.go is added to each. Main reads the blob once (maxRead 256 KiB).
	maxRead = 256 << 10
	many := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		for i := range 70 {
			write(t, src, map[string]string{fmt.Sprintf("d%03d/a.go", i): "package p\n\nfunc " + strings.Repeat("a", 32<<10) + "() {}\n"})
		}
		plain(t, src, "add", "--all")
	})
	added := map[string]string{}
	for i := range 70 {
		added[fmt.Sprintf("d%03d/x.go", i)] = "package p\n"
	}
	onlyStopped("#1 70 directories sharing a 32 KiB name", outGuarded(t, many, added))
	maxRead = oldRead
	// #2: 34 files of one blob, 169 distinct +build lines of 13 free tags
	// each (main's holds returns before its bound); one file's comment
	// changes. maxWork 64 Ki.
	maxWork = 1 << 16
	tags := strings.Fields("a b c d e f g h i j k l m")
	var hdr strings.Builder
	for i := range 169 {
		rot := func(k int) string { return strings.Join(append(slices.Clone(tags[k:]), tags[:k]...), ",") }
		hdr.WriteString("// +build " + rot(i%13) + " " + rot(i/13) + "\n")
	}
	blob := hdr.String() + "\npackage p\n"
	tagged := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		for i := range 34 {
			write(t, src, map[string]string{fmt.Sprintf("zzz/f%02d.go", i): blob})
		}
		plain(t, src, "add", "--all")
	})
	onlyStopped("#2 34 files of 13 free tags", outGuarded(t, tagged, map[string]string{"zzz/f00.go": blob + "\n// changed\n"}))
	maxWork = oldWork
	// The rule: exhausted, a libc import in a changed file of a tested
	// package blocks; elsewhere, or with no libc import, a warning.
	f, _ := libcFixture(t)
	maxOutNodes = 5
	v := outGuarded(t, f, map[string]string{"pkg/x.go": "package pkg\n\nimport \"modernc.org/libc\"\n\nvar _ = libc.NewTLS\n"})
	if !finds(v.Blocks, "pkg/x.go unverifiable: decision A's outside-initialization check stopped at its work bound, and this file imports modernc.org/libc") {
		t.Errorf("exhausted with a libc import: blocks %q", whats(v.Blocks))
	}
	onlyStopped("exhausted, flag only", outGuarded(t, f, map[string]string{"pkg/y.go": "package pkg\n\nimport \"flag\"\n\nvar _ = flag.Parse\n"}))
	onlyStopped("exhausted, libc untested", outGuarded(t, f, map[string]string{"tool/x.go": "package tool\n\nimport \"modernc.org/libc\"\n\nvar _ = libc.NewTLS\n"}))
	maxOutNodes = oldNodes
}

// TestDecisionAOutRound9 (final, scope freeze): Astra #276 r6 on f9ab6e6.
func TestDecisionAOutRound9(t *testing.T) {
	// Fix 1: std by module metadata. A dotless path a module of go.mod or
	// vendor/modules.txt names is not std: its new edge warns (main blocks
	// this vendored form itself: a replacement).
	for _, mod := range []string{"archive", "archive/tar"} {
		f := newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"go.mod": guardBase["go.mod"] + "\nrequire " + mod + " v1.0.0\n\nreplace " + mod + " => example.org/x v1.0.0\n",
				"vendor/modules.txt":      "# golang.org/x/sys v0.48.0\n## explicit\n# " + mod + " v1.0.0 => example.org/x v1.0.0\n## explicit\n" + mod + "\n",
				"vendor/" + mod + "/a.go": "package " + path.Base(mod) + "\n\nfunc F() {}\n"})
			plain(t, src, "add", "--all")
		})
		if v := outGuarded(t, f, map[string]string{"tool/x.go": "package tool\n\nimport dep \"" + mod + "\"\n\nfunc f() { dep.F() }\n"}); !finds(v.Warnings, "a new import of "+mod+",") {
			t.Errorf("replace %s: blocks %q, warnings %q", mod, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// flag and os/user, which no module names: std, no edge.
	f, _ := libcFixture(t)
	if v := outGuarded(t, f, map[string]string{"tool/s.go": "package tool\n\nimport (\n\t\"flag\"\n\t\"os/user\"\n)\n\nvar _, _ = flag.Parse, user.Current\n"}); finds(v.Warnings, "a new import of") || !v.Accepted() {
		t.Errorf("flag, os/user: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// Fix 2: a libc import whose name the layer cannot learn (package p_test
	// in an ordinary file) blocks only in a tested package.
	quiet := "package p_test\n\nimport \"modernc.org/libc\"\n\nfunc f() { _ = libc.NewTLS }\n"
	f, _ = libcFixture(t, map[string]string{"t2/x_test.go": "package p_test\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"})
	if v := outGuarded(t, f, map[string]string{"untested/a.go": quiet}); !v.Accepted() || !finds(v.Warnings, "untested/a.go an import of modernc.org/libc with an unknown name") {
		t.Errorf("unknown name, untested: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	if v := outGuarded(t, f, map[string]string{"t2/a.go": quiet}); !finds(v.Blocks, "t2/a.go unverifiable: an import of modernc.org/libc with an unknown name") {
		t.Errorf("unknown name, tested: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// TestOutNodesPerByte: the shadow walk charges real nodes only, at most two
// a byte: dense forms of Go's syntax, about 300 KiB, charge at most one (nil
// callbacks charged would be two). All of outRead (P14b-out-2's fingerprints
// too) charges a few units a byte.
func TestOutNodesPerByte(t *testing.T) {
	src := "package p\n\nfunc f() {\n" + strings.Repeat("\tx\n\t^^^^^^^^x\n\ta.b.c.d.e.f\n\tf()()()()\n\t{{{{}}}}\n\tx[0][0][0]\n\t-(-(-x))\n\tx, y := 0, 0\n\t_ = []int{0,0,0,0}\n", 4000) + "}\n"
	r := &run{ctx: context.Background(), budget: maxRead, blobs: map[string][]byte{}, out: additive{files: map[string]*outFile{}, built: map[string]int{}, named: map[string]map[string]bool{}},
		read: func(context.Context, string, int64) ([]byte, bool, error) { return []byte(src), true, nil }}
	tr := &tree{files: map[string]string{"p/d.go": "d"}, dirs: map[string][]string{"p": {"p/d.go"}}}
	if r.outRead("p", tr); r.out.nodes > 4*len(src) || r.out.over {
		t.Errorf("outRead: %d units for %d bytes", r.out.nodes, len(src))
	}
	f, err := parser.ParseFile(token.NewFileSet(), "p/d.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	r = &run{ctx: context.Background()}
	if r.refsIn(f, nil, false, func(string, string) {}); r.out.nodes > len(src) || r.out.nodes < len(src)/4 || r.out.over {
		t.Errorf("%d nodes for %d bytes", r.out.nodes, len(src))
	}
}

// TestOutShadowOracle: the layer's shadow tracking against go/types' scope
// rules. Each row's files type-check; a flag.X that go/types resolves to
// the package must be one of the layer's references (never fewer: decision
// A); more is a false warning (logged).
func TestOutShadowOracle(t *testing.T) {
	h := "package p\n\nimport \"flag\"\n\n"
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil)
	for name, files := range map[string][]string{
		"x := x":                    {h + "func a() {\n\tflag := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = flag.Parse(nil)\n}\n"},
		"init scopes end":           {h + "func b() {\n\tif flag := 1; flag > 0 {\n\t}\n\tfor flag := 0; flag < 1; flag++ {\n\t}\n\tswitch flag := 2; flag {\n\t}\n\tflag.Parse()\n}\n"},
		"a type switch":             {h + "func c(v any) {\n\tswitch flag := v.(type) {\n\tcase *flag.FlagSet:\n\t\t_ = flag.Name()\n\tcase int:\n\t\t_ = flag\n\t}\n\tflag.Parse()\n}\n"},
		"labels and goto":           {h + "func d() {\nflag:\n\tfor {\n\t\tbreak flag\n\t}\n\tgoto done\ndone:\n\tflag.Parse()\n}\n"},
		"fields and keys":           {h + "type T struct{ flag int }\n\nfunc e() {\n\tt := T{flag: 1}\n\t_ = t.flag\n\tflag.Parse()\n}\n"},
		"selectors, methods":        {h + "type U struct{}\n\nfunc (U) flag() {}\n\nfunc f(u U) {\n\tu.flag()\n\tflag.Parse()\n}\n"},
		"signatures without a body": {h + "func g(flag int) int\n\nvar fn func(flag int)\n\ntype I interface{ M(flag int) }\n\nfunc g2() { flag.Parse() }\n"},
		"type parameters":           {h + "func k[flag any](v flag) flag { return v }\n\ntype G[flag any] struct{ v flag }\n\nfunc l() { flag.Parse() }\n"},
		"a var block":               {h + "func m() {\n\tvar x = flag.NArg()\n\tvar (\n\t\ty, flag = x, 2\n\t)\n\t_, _ = y, flag\n}\n\nfunc n() {\n\t{\n\t\tconst flag = 1\n\t\t_ = flag\n\t}\n\tflag.Parse()\n}\n"},
		"closures":                  {h + "func o() {\n\tf := func() { flag.Parse() }\n\tf()\n\t_ = func(flag int) int { return flag }\n\t{\n\t\tflag := 1\n\t\t_ = func() int { return flag }\n\t}\n\tflag.Parse()\n}\n"},
		"select with :=":            {h + "func q(ch chan int) {\n\tselect {\n\tcase flag := <-ch:\n\t\t_ = flag\n\tdefault:\n\t\tflag.Parse()\n\t}\n}\n"},
		"ranges":                    {h + "func u(xs []int) {\n\tfor _, flag := range xs {\n\t\t_ = flag\n\t}\n\tfor flag := range flag.Args() {\n\t\t_ = flag\n\t}\n}\n"},
		"receivers, results":        {h + "type V struct{}\n\nfunc (flag V) w() (n int) { _ = flag; return }\n\nfunc x() (flag int) { return }\n\nfunc y() { flag.Parse() }\n"},
		"another file":              {"package p\n\nfunc r() {\n\tflag := 1\n\t_ = flag\n}\n", h + "func s() { flag.Parse() }\n"},
	} {
		fset, want, got := token.NewFileSet(), map[string]int{}, map[string]int{}
		var fs []*ast.File
		for i, src := range files {
			f, err := parser.ParseFile(fset, fmt.Sprintf("p/f%d.go", i), src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(name, err)
			}
			fs = append(fs, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
		if _, err := (&types.Config{Importer: imp}).Check("p", fset, fs, info); err != nil {
			t.Fatal(name, err)
		}
		for i, f := range fs {
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok {
						if pn, ok := info.Uses[x].(*types.PkgName); ok && pn.Imported().Path() == "flag" {
							want[fmt.Sprintf("p/f%d.go.%s", i, sel.Sel.Name)]++
						}
					}
				}
				return true
			})
		}
		r := &run{ctx: context.Background(), budget: maxRead, blobs: map[string][]byte{}, out: additive{files: map[string]*outFile{}, built: map[string]int{}, named: map[string]map[string]bool{}},
			read: func(_ context.Context, blob string, _ int64) ([]byte, bool, error) {
				return []byte(files[blob[0]-'0']), true, nil
			}}
		tr := &tree{files: map[string]string{}, dirs: map[string][]string{}, typers: map[string]*typer{}}
		r.trees = [2]*tree{tr, tr}
		for i := range files {
			p := fmt.Sprintf("p/f%d.go", i)
			tr.files[p], tr.dirs["p"] = fmt.Sprint(i), append(tr.dirs["p"], p)
			for k, n := range r.outFileOf(p, "p", fmt.Sprint(i), tr).refs {
				got[p+"."+k[strings.LastIndexByte(k, 0)+1:]] += n
			}
		}
		for k, n := range want {
			if got[k] < n {
				t.Errorf("%s: %s: %d of go/types' %d references", name, k, got[k], n)
			}
		}
		for k, n := range got {
			if n > want[k] {
				t.Logf("%s: %s: %d references, go/types %d (a false warning)", name, k, n, want[k])
			}
		}
	}
}

// TestOutReadAndCancel: the layer reads no non-Go blob (Opus #276 r2 M3,
// probe B1), and a done context stops it with no block (M2, probe B2).
func TestOutReadAndCancel(t *testing.T) {
	var read []string
	r := &run{ctx: context.Background(), budget: maxRead, blobs: map[string][]byte{}, out: additive{files: map[string]*outFile{}, built: map[string]int{}, named: map[string]map[string]bool{}},
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
