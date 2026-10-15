// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"testing"
)

// forgeU is a package using util with a Stringer that does nothing.
const forgeU = "package pkg\n\nimport \"example.com/m/util\"\n\ntype forger struct{}\n\nfunc (forger) String() string { return \"\" }\n\n"

// F (P14e-2b, #226 entries 82 and 85): os.Args handed to a function value,
// another package's variable in a composite literal from initialization,
// vendoring, ignore files and an abandoned type check, each reproduced
// first on main (326b55e).
func TestIntegrityRest(t *testing.T) {
	u := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/u.go": utilGo})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]guardCase{
		// 82 (c): os.Args to a function value, outside initialization and the hook.
		"82c os.Args to a function value": {map[string]string{"pkg/args.go": argsGo + "var set = func(a []string) { a[0] = \"-h\" }\n\nfunc reset() { set(os.Args) }\n"},
			nil, "pkg/args.go: os.Args assigned in reset"},
		// 82 (d): another package's container in a composite literal, read from initialization.
		"82d a composite literal of util.S": {map[string]string{"pkg/forge.go": forgeU + "type holder struct{ s []any }\n\nfunc init() {\n\th := holder{util.S}\n\th.s[0] = forger{}\n}\n"},
			nil, "plain data read"},
		"82d a keyed, addressed literal": {map[string]string{"pkg/forge.go": forgeU + "type holder struct{ s []any }\n\nfunc init() {\n\th := &holder{s: util.S}\n\th.s[0] = forger{}\n}\n"},
			nil, "plain data read"},
	} {
		v := u.guarded(t, c.files, c.gone...)
		if !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// Vendoring: with vendor/modules.txt consistent with go.mod, the gate
	// builds -mod=vendor, so a vendored package's types are read from vendor/
	// (as own source); an inconsistent vendor/modules.txt, or an import it
	// does not list, is unverifiable.
	vendored := func(modules string) fixture {
		return newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"go.mod": guardBase["go.mod"] + "\nrequire example.org/lib v1.0.0\n", "vendor/modules.txt": modules,
				"vendor/example.org/lib/lib.go": "package lib\n\nimport \"testing\"\n\ntype Holder struct{ T *testing.T }\n\nfunc F() int { return 1 }\n"})
			plain(t, src, "add", "--all")
		})
	}
	consistent := "# golang.org/x/sys v0.48.0\n## explicit\n# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n"
	vf := vendored(consistent)
	if v := vf.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.org/lib\"\n\nfunc G() int { return lib.F() }\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("a vendored import: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	holder := "package pkg\n\nimport (\n\t\"encoding/json\"\n\t\"testing\"\n\n\t\"example.org/lib\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tvar h lib.Holder\n" +
		"\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &h)\n\tvar y *testing.T = h.T\n\tif Add(1, 2) != 3 {\n\t\ty.Error(\"add\")\n\t}\n}\n"
	if v := vf.guarded(t, map[string]string{"pkg/calc_test.go": holder}); !finds(v.Blocks, forgedWhy) {
		t.Errorf("a vendored holder decoded: %q", whats(v.Blocks))
	}
	if v := vendored(strings.Replace(consistent, "lib v1.0.0", "lib v1.0.1", 1)).guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.org/lib\"\n\nfunc G() int { return lib.F() }\n"}); !finds(v.Blocks, "vendor/modules.txt") {
		t.Errorf("an inconsistent vendor/modules.txt: %q", whats(v.Blocks))
	}
	if v := vf.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.org/other\"\n\nvar _ = other.X\n"}); !finds(v.Blocks, "not in vendor/modules.txt") {
		t.Errorf("an import vendor/modules.txt does not list: %q", whats(v.Blocks))
	}

	// Mythos #269 L4: go never builds a //go:build ignore file, so the types
	// layer leaves it out (a generator's imports need not list); the text
	// layer still reads it, so a forgery there still blocks.
	gen := "//go:build ignore\n\npackage main\n\nimport \"example.org/gen\"\n\nfunc main() { gen.Run() }\n"
	if v := u.guarded(t, map[string]string{"pkg/gen.go": gen}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("a generator under ignore: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	if v := u.guarded(t, map[string]string{"pkg/gen.go": strings.Replace(gen, "package main", "package pkg", 1)}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("an ignored file of the package: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	if v := u.guarded(t, map[string]string{"pkg/forge.go": "//go:build ignore\n\n" + forgeGo}); !finds(v.Blocks, "cannot prove safe (syscall.Exit)") {
		t.Errorf("a forgery under ignore: %q", whats(v.Blocks))
	}
}
