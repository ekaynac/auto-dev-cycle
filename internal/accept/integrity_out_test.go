// SPDX-License-Identifier: Apache-2.0

package accept

import "testing"

// F (P14b-out, #226 entries 75 and 76; operator decision A, outside
// initialization and the hook): modernc.org/libc is class 2; a new import
// edge outside the standard library and the module warns; a method of an S
// type reached through a value warns by its real type. Each reproduced first
// on main (a639412).
func TestDecisionAOut(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{
			"go.mod": guardBase["go.mod"] + "\nrequire (\n\tmodernc.org/libc v1.0.0\n\texample.org/lib v1.0.0\n)\n",
			"vendor/modules.txt": "# golang.org/x/sys v0.48.0\n## explicit\n# modernc.org/libc v1.0.0\n## explicit\nmodernc.org/libc\n" +
				"# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n",
			"vendor/modernc.org/libc/libc.go": "package libc\n\ntype TLS struct{}\n\nfunc NewTLS() *TLS { return nil }\n\nfunc X_Exit(t *TLS, s int32) {}\n",
			"vendor/example.org/lib/lib.go":   "package lib\n\nfunc F() int { return 1 }\n",
			"util/flags.go":                   "package util\n\nimport \"flag\"\n\nfunc Flags() *flag.FlagSet { return flag.CommandLine }\n",
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
		"a FlagSet through a base helper": {"tool/p.go", "package tool\n\nimport \"example.com/m/util\"\n\nfunc parse() { _ = util.Flags().Parse([]string{\"-h\"}) }\n", "",
			"tool/p.go: (*flag.FlagSet).Parse in parse"},
	} {
		v := f.guarded(t, map[string]string{c.file: c.src})
		if c.block != "" && !finds(v.Blocks, c.block) || c.block == "" && !v.Accepted() || c.warn != "" && !finds(v.Warnings, c.warn) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
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
