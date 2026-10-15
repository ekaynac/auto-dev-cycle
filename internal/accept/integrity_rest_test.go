// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"go/build"
	"io"
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
	vendored := func(modules string, more ...map[string]string) fixture {
		return newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			for _, m := range more {
				write(t, src, m)
			}
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

	// The background flag on 69b7710 "parser-differential / integrity-guard
	// bypass": vendor/ is read only where go would read it, and only as the
	// base has it (the gate may build -mod=vendor or, as the prepared image's
	// GOFLAGS=-mod=readonly does, from the module cache).
	for name, c := range map[string]struct {
		modules string
		files   map[string]string
		why     string
	}{
		"a vendored file changed": {consistent, map[string]string{"vendor/example.org/lib/lib.go": "package lib\n\nfunc F() int { return 2 }\n"}, "vendor/"},
		"modules.txt lists an own package": {consistent + "example.com/m/other\n", map[string]string{"vendor/example.com/m/other/o.go": "package other\n\nvar Value string\n",
			"pkg/x.go": "package pkg\n\nimport \"example.com/m/other\"\n\nvar n int = other.Value\n"}, "vendor/"},
		"modules.txt lists a standard package": {consistent + "fmt\n", map[string]string{"vendor/fmt/f.go": "package fmt\n\nfunc Sprint(...any) int { return 0 }\n",
			"pkg/x.go": "package pkg\n\nimport \"fmt\"\n\nvar n int = fmt.Sprint()\n"}, "vendor/"},
		"a package outside its module": {consistent + "evil.org/x\n", map[string]string{"vendor/evil.org/x/x.go": "package x\n\nfunc F() int { return 1 }\n",
			"pkg/x.go": "package pkg\n\nimport \"evil.org/x\"\n\nvar n = x.F()\n"}, "vendor/modules.txt does not agree"},
	} {
		base, cand := map[string]string{}, map[string]string{} // vendor/ in the base; the candidate's own change
		for p, src := range c.files {
			if strings.HasPrefix(p, "vendor/") && name != "a vendored file changed" {
				base[p] = src
			} else {
				cand[p] = src
			}
		}
		if v := vendored(c.modules, base).guarded(t, cand); !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
	noTxt := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"vendor/example.org/lib/lib.go": "package lib\n\nfunc F() int { return 1 }\n"})
		plain(t, src, "add", "--all")
	})
	if v := noTxt.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nfunc G() int { return 1 }\n"}); !finds(v.Blocks, "vendor/modules.txt") {
		t.Errorf("vendor/ without modules.txt: %q", whats(v.Blocks))
	}
	// The build-tag flag on 1ac24de: a file is left out only by the
	// constraint lines go reads (go/build's header rule), so a line go does
	// not read, or an ignore another tag can override, keeps it in.
	dec := "package pkg\n\nimport (\n\t\"encoding/json\"\n\t\"testing\"\n)\n\n" +
		"func dec() *testing.T {\n\tvar h struct{ T *testing.T }\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &h)\n\treturn h.T\n}\n"
	for name, c := range map[string]struct{ file, why string }{
		"ignore || linux":                      {"//go:build ignore || linux\n\n" + dec, forgedWhy},
		"!ignore":                              {"//go:build !ignore\n\n" + dec, forgedWhy},
		"+build ignore, no blank line":         {"// +build ignore\n" + dec, forgedWhy},
		"+build ignore in the doc comment":     {"// +build cgo\n\n// +build ignore\n" + dec, forgedWhy},
		"//go:build after /* */":               {"/* x */ //go:build ignore\n\n" + dec, forgedWhy},
		"//go:build windows after /* */":       {"/* x */ //go:build windows\n\n" + forgeGo, "cannot prove safe (syscall.Exit)"},
		"+build windows, no blank line":        {"// +build windows\n" + forgeGo, "cannot prove safe (syscall.Exit)"},
		"//go:build windows after a /* */ run": {"/* x\n*/\n//go:build windows\n\n" + forgeGo, ""}, // go reads it: never built here
	} {
		if v := u.guarded(t, map[string]string{"pkg/dec.go": c.file}); c.why == "" && !v.Accepted() || c.why != "" && !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
	if v := u.guarded(t, map[string]string{"pkg/gen.go": "// +build ignore\n\npackage main\n\nimport \"example.org/gen\"\n\nfunc main() { gen.Run() }\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("a generator under +build ignore: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
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

// TestHeaderAsGo: the constraint lines goOn reads decide as go/build does,
// over every header of up to three of these lines (the build-tag flag).
func TestHeaderAsGo(t *testing.T) {
	parts := []string{"", "// doc", "/* x */", "/*\n// +build windows\n*/", "/*\n//go:build windows\n*/", "// +build windows", "// +build linux", "//go:build windows",
		"//go:build linux", "/* x */ //go:build windows", "//go:build\twindows", "//go:buildwindows", "/* x */ // +build windows"}
	r, ctx := &run{ctx: context.Background()}, build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: "gc", ReleaseTags: release}
	n := 0
	var walk func(string, int)
	walk = func(head string, depth int) {
		for _, src := range []string{head + "\npackage p\n", head + "\npackage p\n\n//go:build windows\n"} { // and a line after the code
			ctx.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(src)), nil }
			if want, err := ctx.MatchFile("/x", "x.go"); err == nil {
				n++
				if on, _, _, err := r.goOn("x.go", []byte(src)); err != nil || (on[0] == 2) != want || on[0] == 1 {
					t.Errorf("%q: go builds %v, goOn %v", src, want, on)
				}
			}
		}
		for _, p := range parts[:len(parts)*b2i(depth < 3)] {
			walk(head+"\n"+p, depth+1)
		}
	}
	walk("", 0)
	if n < 1000 {
		t.Errorf("only %d headers", n)
	}
}
