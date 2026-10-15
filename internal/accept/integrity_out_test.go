// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// outRefs is the layer's aggregated warning for file, package and functions.
func outRefs(file, pkg, fns string) string {
	return file + " references into " + pkg + " added, removed, moved or changed (" + fns + ")"
}

// libcFixture vendors modernc.org/libc (and its sub package), example.org/lib,
// example.com/mx and example.com/m/evil (the gate is offline).
func libcFixture(t *testing.T) (fixture, string) {
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
		v := f.guarded(t, map[string]string{c.file: c.src})
		if c.block != "" && !finds(v.Blocks, c.block) || c.block == "" && !v.Accepted() || c.warn != "" && !finds(v.Warnings, c.warn) || c.warn == "" && c.block == "" && len(v.Warnings) > 0 {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// The module line changed: which imports are own changes with it.
	if v := f.guarded(t, map[string]string{"go.mod": strings.Replace(gomod, "module example.com/m", "module example.com", 1),
		"tool/x.go": "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n"}); !finds(v.Blocks, "the module path changed") {
		t.Errorf("a module rename: %q", whats(v.Blocks))
	}
	// Own only if the loader reads it as own in both versions (a nested module merged in).
	nested := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"api/go.mod": "module example.com/m/api\n\ngo 1.26.0\n", "api/a.go": "package api\n\nfunc F() int { return 1 }\n"})
		plain(t, src, "add", "--all")
	})
	if v := nested.guarded(t, map[string]string{"tool/n.go": "package tool\n\nimport \"example.com/m/api\"\n\nfunc G() int { return api.F() }\n"}, "api/go.mod"); !finds(v.Warnings, "a new import of example.com/m/api") {
		t.Errorf("a nested module merged in: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// No edge for the standard library (go list's Standard) or the module's own packages.
	if v := f.guarded(t, map[string]string{"tool/y.go": "package tool\n\nimport (\n\t\"strings\"\n\n\t\"example.com/m/other\"\n)\n\nvar _ = strings.ToUpper\n\nvar _ = other.Value\n"}); finds(v.Warnings, "outside the standard library") {
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
	v := base.guarded(t, map[string]string{"aaa/a.go": sys.String(), "zzz/out.go": out})
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
	v = f.guarded(t, map[string]string{"pkg/a.go": exits.String(), "~p/p.go": "package p\n\nvar x int = \"bad\"\n"})
	if b, _ := report(v); !finds(b, "~p:p unverifiable") || slices.ContainsFunc(v.Blocks, func(b Block) bool { return b.Path == "~p:p" && b.Additive }) {
		t.Errorf("r3 #2: main's ~p block reported %t", finds(b, "~p:p unverifiable"))
	}

	// r3 #3: 250000 method selections and main's zzz/out.go: it is in the
	// verdict and the report.
	pad := "package aaa\n\nimport \"os\"\n\nfunc padding(f *os.File) {\n" + strings.Repeat("\t_ = f.Close\n", 250000) + "}\n"
	v = g.guarded(t, map[string]string{"aaa/a.go": pad, "zzz/out.go": out})
	if _, w := report(v); !finds(v.Warnings, "zzz/out.go: fmt.Println in out") || !finds(w, "zzz/out.go: fmt.Println in out") {
		t.Errorf("r3 #3: zzz/out.go in the verdict %t, in the report %t", finds(v.Warnings, "zzz/out.go: fmt.Println in out"), finds(w, "zzz/out.go: fmt.Println in out"))
	}

	// The layer's own budget: past it, one Additive block; main's findings stay.
	old := maxOutWork
	maxOutWork = 100
	v = g.guarded(t, map[string]string{"aaa/a.go": "package aaa\n\nimport \"flag\"\n\nfunc p() { flag.Parse() }\n", "zzz/out.go": out})
	maxOutWork = old
	over := slices.DeleteFunc(slices.Clone(v.Blocks), func(b Block) bool { return !b.Additive || !strings.Contains(b.What, "over its work bound") })
	if len(over) != 1 || !finds(v.Warnings, "zzz/out.go: fmt.Println in out") || finds(v.Warnings, "references into") { // and the layer stopped
		t.Errorf("the layer's budget: %d Additive over-bound blocks, warnings %q", len(over), whats(v.Warnings))
	}

	// r2 H2: 1000 dot imports of flag, untested, and zzz/out.go.
	files := map[string]string{"zzz/out.go": out}
	for i := range 1000 {
		files[fmt.Sprintf("aaa/a%04d.go", i)] = "package aaa\n\nimport . \"flag\"\n\nvar _ = ContinueOnError\n"
	}
	if _, w := report(g.guarded(t, files)); !finds(w, "zzz/out.go: fmt.Println in out") {
		t.Errorf("r2 H2 1000 dot imports: main's warning displaced")
	}
	// r2 H2: 1000 functions calling flag.Parse: one Additive warning.
	var parse strings.Builder
	parse.WriteString("package pkg\n\nimport \"flag\"\n")
	for i := range 1000 {
		fmt.Fprintf(&parse, "\nfunc f%d() { flag.Parse() }\n", i)
	}
	v = g.guarded(t, map[string]string{"pkg/a.go": parse.String(), "pkg/z.go": strings.Replace(out, "package zzz", "package pkg", 1)})
	if _, w := report(v); !finds(w, "pkg/z.go: fmt.Println in out") || len(slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return b.Path != "pkg/a.go" })) != 1 {
		t.Errorf("r2 H2 1000 functions: %d warnings", len(v.Warnings))
	}
	// r2 H3: 1000 libc blocks and a main block (the count cap); the verdict blocks.
	v = f.guarded(t, map[string]string{"pkg/a.go": exits.String(), "pkg/z.go": "package pkg\n\nimport \"os\"\n\nfunc quit() { os.Exit(0) }\n"})
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
