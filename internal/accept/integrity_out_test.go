// SPDX-License-Identifier: Apache-2.0

package accept

import (
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
		"libc.X_Exit untested":            {"tool/out.go", "package tool\n\n" + libc, "", "tool/out.go: modernc.org/libc.X_Exit in quit"},
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
			"", "parse[flag.FlagSet] in p"}, // the pointer removed
		"a base struct embedding *flag.FlagSet": {"tool/e.go", "package tool\n\nimport \"example.com/m/util\"\n\nfunc p() { _ = util.NewEmbed().Parse(nil) }\n", "",
			"(*flag.FlagSet).Parse via example.com/m/util.Embed in p"},
		"a local struct embedding *flag.FlagSet": {"tool/w.go", "package tool\n\nimport \"flag\"\n\ntype wrap struct{ *flag.FlagSet }\n\nfunc p(w wrap) { _ = w.Parse(nil) }\n", "",
			"(*flag.FlagSet).Parse via example.com/m/tool.wrap in p"},
		"a FlagSet through a base helper": {"tool/p.go", "package tool\n\nimport \"example.com/m/util\"\n\nfunc parse() { _ = util.Flags().Parse([]string{\"-h\"}) }\n", "",
			"tool/p.go: (*flag.FlagSet).Parse in parse"},
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
	if v := f.guarded(t, map[string]string{"tool/long.go": long}); !finds(v.Warnings, "(*flag.FlagSet).Parse via struct{") || !finds(v.Warnings, "… ") {
		t.Errorf("a long key: %q", whats(v.Warnings))
	}
	// H1: a change to the module line changes which imports are own (probe 42).
	if v := f.guarded(t, map[string]string{"go.mod": strings.Replace(gomod, "module example.com/m", "module example.com", 1),
		"tool/x.go": "package tool\n\nimport \"example.com/mx\"\n\nfunc G() int { return mx.F() }\n"}); !finds(v.Blocks, "the module path changed") {
		t.Errorf("a module rename: %q", whats(v.Blocks))
	}
	// A field of an S type is data: no reference (the type's name warns).
	if v := f.guarded(t, map[string]string{"tool/h.go": "package tool\n\nimport \"os/user\"\n\nfunc home(u *user.User) string { return u.HomeDir }\n"}); !v.Accepted() || finds(v.Warnings, "HomeDir") || !finds(v.Warnings, "tool/h.go: os/user.User in home") {
		t.Errorf("a field of an S type: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// No edge warning for the standard library or the module's own packages.
	if v := f.guarded(t, map[string]string{"tool/y.go": "package tool\n\nimport (\n\t\"strings\"\n\n\t\"example.com/m/util\"\n)\n\nvar _ = strings.ToUpper\n\nvar _ = util.Flags\n"}); !v.Accepted() || finds(v.Warnings, "outside the standard library") {
		t.Errorf("std and own imports: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}
