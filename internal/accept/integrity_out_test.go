// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// F (P14b-out, #226 entries 75 and 76; operator decision A, outside
// initialization and the hook): modernc.org/libc is class 2; a new import
// edge outside the standard library and the module warns; a method of an S
// type reached through a value warns by its real type. Each reproduced first
// on main (a639412).
func TestDecisionAOut(t *testing.T) {
	gomod := guardBase["go.mod"] + "\nrequire (\n\tmodernc.org/libc v1.0.0\n\texample.org/lib v1.0.0\n\texample.com/mx v1.0.0\n\texample.com/m/evil v1.0.0\n)\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{
			"go.mod": gomod,
			"vendor/modules.txt": "# golang.org/x/sys v0.48.0\n## explicit\n# modernc.org/libc v1.0.0\n## explicit\nmodernc.org/libc\nmodernc.org/libc/sub\n" +
				"# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n# example.com/mx v1.0.0\n## explicit\nexample.com/mx\n" +
				"# example.com/m/evil v1.0.0\n## explicit\nexample.com/m/evil\n",
			"vendor/modernc.org/libc/sub/sub.go": "package sub\n\nfunc X_exit(s int32) {}\n",
			"vendor/example.com/mx/mx.go":        "package mx\n\nfunc F() int { return 1 }\n",
			"vendor/example.com/m/evil/evil.go":  "package evil\n\nfunc Exit() {}\n",
			"util/embed.go":                      "package util\n\nimport \"flag\"\n\ntype Embed struct{ *flag.FlagSet }\n\nfunc NewEmbed() Embed { return Embed{flag.CommandLine} }\n",
			"vendor/modernc.org/libc/libc.go":    "package libc\n\ntype TLS struct{}\n\nfunc NewTLS() *TLS { return nil }\n\nfunc X_Exit(t *TLS, s int32) {}\n",
			"vendor/example.org/lib/lib.go":      "package lib\n\nfunc F() int { return 1 }\n",
			"util/flags.go":                      "package util\n\nimport \"flag\"\n\nfunc Flags() *flag.FlagSet { return flag.CommandLine }\n",
		})
		plain(t, src, "add", "--all")
	})
	libc := "import \"modernc.org/libc\"\n\nfunc quit() { libc.X_Exit(libc.NewTLS(), 0) }\n"
	for name, c := range map[string]struct {
		file, src, block, warn string
	}{
		"libc.X_Exit in a tested package": {"pkg/out.go", "package pkg\n\n" + libc, "pkg/out.go: modernc.org/libc.X_Exit in quit", ""},
		"libc.X_Exit untested":            {"tool/out.go", "package tool\n\n" + libc, "", "tool/out.go references into modernc.org/libc added, removed, moved or changed (1: quit)"},
		"a new import outside std and the module": {"tool/x.go", "package tool\n\nimport \"example.org/lib\"\n\nfunc G() int { return lib.F() }\n", "",
			"a new import of example.org/lib"},
		// Opus #276. H1: an import resolved from vendor/ under the module's
		// path is no own package (probe 20); L5: a module beside it (example.com/mx).
		"a vendored module under the module's path": {"pkg/probe.go", "package pkg\n\nimport \"example.com/m/evil\"\n\nfunc p() { evil.Exit() }\n", "",
			"a new import of example.com/m/evil"},
		"example.com/mx beside example.com/m": {"tool/x.go", "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n", "", "a new import of example.com/mx"},
		// L5: a libc subpackage is class 2; a dot import of a package newly in S.
		"modernc.org/libc/sub.X_exit": {"pkg/sub.go", "package pkg\n\nimport \"modernc.org/libc/sub\"\n\nfunc quit() { sub.X_exit(0) }\n", "pkg/sub.go: modernc.org/libc/sub.X_exit in quit", ""},
		"import . \"flag\"":           {"pkg/dot.go", "package pkg\n\nimport . \"flag\"\n\nfunc p() { Parse() }\n", "a dot import of flag", ""},
		// M1: a generic over an S type (probe 06); L3: a promoted method names
		// flag (probes 03 and 04).
		"parse[*flag.FlagSet] through util.Flags()": {"pkg/g.go", "package pkg\n\nimport \"example.com/m/util\"\n\nfunc parse[T interface{ Parse([]string) error }](t T) { _ = t.Parse([]string{\"-h\"}) }\n\nfunc p() { parse(util.Flags()) }\n",
			"", "pkg/g.go references into flag added, removed, moved or changed (1: p)"}, // parse[flag.FlagSet], the pointer removed
		"a base struct embedding *flag.FlagSet": {"tool/e.go", "package tool\n\nimport \"example.com/m/util\"\n\nfunc p() { _ = util.NewEmbed().Parse(nil) }\n", "",
			"tool/e.go references into flag added, removed, moved or changed (1: p)"}, // (*flag.FlagSet).Parse via example.com/m/util.Embed
		"a local struct embedding *flag.FlagSet": {"tool/w.go", "package tool\n\nimport \"flag\"\n\ntype wrap struct{ *flag.FlagSet }\n\nfunc p(w wrap) { _ = w.Parse(nil) }\n", "",
			"tool/w.go references into flag added, removed, moved or changed (2: p, package level)"}, // (*flag.FlagSet).Parse via example.com/m/tool.wrap
		"a FlagSet through a base helper": {"tool/p.go", "package tool\n\nimport \"example.com/m/util\"\n\nfunc parse() { _ = util.Flags().Parse([]string{\"-h\"}) }\n", "",
			"tool/p.go references into flag added, removed, moved or changed (1: parse)"},
	} {
		v := f.guarded(t, map[string]string{c.file: c.src})
		if c.block != "" && !finds(v.Blocks, c.block) || c.block == "" && !v.Accepted() || c.warn != "" && !finds(v.Warnings, c.warn) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// H1: own only if the loader reads it as own in both versions: a nested
	// module's go.mod removed makes example.com/m/api own in the candidate only.
	nested := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"api/go.mod": "module example.com/m/api\n\ngo 1.26.0\n", "api/a.go": "package api\n\nfunc F() int { return 1 }\n"})
		plain(t, src, "add", "--all")
	})
	if v := nested.guarded(t, map[string]string{"tool/n.go": "package tool\n\nimport \"example.com/m/api\"\n\nfunc G() int { return api.F() }\n"}, "api/go.mod"); !finds(v.Warnings, "a new import of example.com/m/api") {
		t.Errorf("a nested module merged in: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// L3: a key is cut to 200 bytes and a hash (an anonymous struct prints whole).
	long := "package tool\n\nimport \"flag\"\n\nfunc p() {\n\tvar w struct {\n\t\t*flag.FlagSet\n\t\t" + strings.Repeat("Field", 30) + " int\n\t\t" + strings.Repeat("Other", 30) + " int\n\t}\n\t_ = w.Parse(nil)\n}\n"
	if v := f.guarded(t, map[string]string{"tool/long.go": long}); finds(v.Warnings, "struct{") || !finds(v.Warnings, "tool/long.go references into flag added, removed, moved or changed (1: p)") { // an anonymous receiver: never rendered
		t.Errorf("a long key: %q", whats(v.Warnings))
	}
	// H1: a change to the module line changes which imports are own (probe 42).
	if v := f.guarded(t, map[string]string{"go.mod": strings.Replace(gomod, "module example.com/m", "module example.com", 1),
		"tool/x.go": "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n"}); !finds(v.Blocks, "the module path changed") {
		t.Errorf("a module rename: %q", whats(v.Blocks))
	}
	// A field of an S type is data: no reference (the type's name warns).
	if v := f.guarded(t, map[string]string{"tool/h.go": "package tool\n\nimport \"os/user\"\n\nfunc home(u *user.User) string { return u.HomeDir }\n"}); !v.Accepted() || finds(v.Warnings, "HomeDir") || !finds(v.Warnings, "tool/h.go references into os/user added, removed, moved or changed (1: home)") {
		t.Errorf("a field of an S type: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// A method by real types is P14b-out's even on a type main had in S (os):
	// aggregated, after main's, never one by one in main's stream.
	if v := f.guarded(t, map[string]string{"tool/c.go": "package tool\n\nimport \"os\"\n\nfunc p(f *os.File) { _ = f.Close() }\n"}); finds(v.Warnings, "(*os.File).Close") ||
		!finds(v.Warnings, "tool/c.go references into os added, removed, moved or changed (1: p)") || !finds(v.Warnings, "tool/c.go: os.File in p") {
		t.Errorf("a method on an os type: %q", whats(v.Warnings))
	}
	// A renamed import of a package main had in S stays in main's stream, in main's form.
	if v := f.guarded(t, map[string]string{"tool/r.go": "package tool\n\nimport x \"os\"\n\nfunc f() { x.Exit(2) }\n"}); !finds(v.Warnings, "tool/r.go: x.Exit(2) in f") || !finds(v.Warnings, "tool/r.go: os.Exit in f") {
		t.Errorf("a renamed os: %q", whats(v.Warnings))
	}
	// No edge warning for the standard library or the module's own packages.
	if v := f.guarded(t, map[string]string{"tool/y.go": "package tool\n\nimport (\n\t\"strings\"\n\n\t\"example.com/m/util\"\n)\n\nvar _ = strings.ToUpper\n\nvar _ = util.Flags\n"}); !v.Accepted() || finds(v.Warnings, "outside the standard library") {
		t.Errorf("std and own imports: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// TestDecisionAOutRound3: Astra #276. H1: an existing body changed to the
// failure probe, with the same references, warns. H2: P14b-out's findings
// never displace main's in the report (its 1000 entries, its byte budget).
// H3: a method's key never renders an anonymous receiver.
func TestDecisionAOutRound3(t *testing.T) {
	parse := "package pkg\n\nimport \"flag\"\n\nfunc parse() {\n\tfs := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = fs.Parse(nil)\n}\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/p.go": parse})
		plain(t, src, "add", "--all")
	})
	if v := f.guarded(t, map[string]string{"pkg/p.go": strings.Replace(parse, "fs.Parse(nil)", "fs.Parse([]string{\"-h\"})", 1)}); !finds(v.Warnings, "pkg/p.go changed bodies that use flag (1: parse)") {
		t.Errorf("H1 fs.Parse(nil) to fs.Parse(-h): blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}

	report := func(v Verdict) []Block {
		b, err := v.Report()
		var r struct{ Warnings []Block }
		if err != nil || json.Unmarshal(b, &r) != nil {
			t.Fatal(err)
		}
		return r.Warnings
	}
	for name, fn := range map[string]string{"1000 functions": "f%d", "long names": "f%d_" + strings.Repeat("x", 1500)} {
		var a strings.Builder
		a.WriteString("package pkg\n\nimport \"flag\"\n")
		for i := range 1000 {
			fmt.Fprintf(&a, "\nfunc "+fn+"() { flag.Parse() }\n", i)
		}
		v := f.guarded(t, map[string]string{"pkg/a.go": a.String(), "pkg/z.go": "package pkg\n\nimport \"fmt\"\n\nfunc output() { fmt.Println(\"x\") }\n"})
		if !finds(report(v), "pkg/z.go: fmt.Println in output") {
			t.Errorf("H2 %s: main's warning displaced in the report (%d warnings)", name, len(v.Warnings))
		} else if n := len(slices.DeleteFunc(slices.Clone(v.Warnings), func(b Block) bool { return b.Path != "pkg/a.go" })); n != 1 {
			t.Errorf("H2 %s: %d warnings for pkg/a.go, want one (per file and package)", name, n)
		}
	}
	// 1000 files, one P14b-out warning each even when aggregated: main's
	// comes first in the report.
	files := map[string]string{"pkg/z.go": "package pkg\n\nimport \"fmt\"\n\nfunc output() { fmt.Println(\"x\") }\n"}
	for i := range 1000 {
		files[fmt.Sprintf("pkg/a%04d.go", i)] = fmt.Sprintf("package pkg\n\nimport \"flag\"\n\nfunc f%d() { flag.Parse() }\n", i)
	}
	if v := f.guarded(t, files); !finds(report(v), "pkg/z.go: fmt.Println in output") {
		t.Errorf("H2 1000 files: main's warning displaced in the report (%d warnings)", len(v.Warnings))
	}

	// H3: an anonymous receiver with a long tag, selected many times: the
	// key is the method's full name, never the rendered receiver (the cost
	// that remains grows with the tag in go/types' own error text in the
	// text layer's check, as on main, under the type-check deadline).
	anon := "package tool\n\nimport \"flag\"\n\nfunc p() {\n\tvar w struct {\n\t\t*flag.FlagSet\n\t\tX int `x:\"" + strings.Repeat("t", 1<<12) + "\"`\n\t}\n" +
		strings.Repeat("\t_ = w.Parse\n", 200) + "}\n"
	if v := f.guarded(t, map[string]string{"tool/w.go": anon}); finds(v.Warnings, "struct{") || !finds(v.Warnings, "tool/w.go references into flag") {
		t.Errorf("H3 an anonymous receiver: %q", whats(v.Warnings))
	}
}

// TestMethodRef: Astra #276 H3: a method's key is its full name and a named
// type at most, never an anonymous receiver rendered (a 1 MiB tag here).
func TestMethodRef(t *testing.T) {
	src := "package p\n\nimport \"flag\"\n\nvar w struct {\n\t*flag.FlagSet\n\tX int `x:\"" + strings.Repeat("t", 1<<20) + "\"`\n}\n\nvar _ = w.Parse\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := (&types.Config{Importer: importer.ForCompiler(fset, "source", nil)}).Check("p", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	for _, sel := range info.Selections {
		if q := methodRef(sel.Obj().(*types.Func), sel.Recv()); q != "(*flag.FlagSet).Parse" {
			t.Errorf("the key: %d bytes, %.80q", len(q), q)
		}
	}
}
