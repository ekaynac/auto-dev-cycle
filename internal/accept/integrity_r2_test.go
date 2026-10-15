// SPDX-License-Identifier: Apache-2.0

package accept

import "testing"

// Round 2 of #271 (Opus): forgeries that pass end to end under go test -json
// with Add returning a-b, each reproduced first on 1ae3268.
const (
	// h1P2c clears failed through Addr().UnsafePointer(), with no unsafe
	// import (P2c).
	h1P2c = `package pkg

import (
	"reflect"
	"testing"
)

func unfail(v reflect.Value) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	*(*bool)(v.FieldByName("failed").Addr().UnsafePointer()) = false
	unfail(v.FieldByName("parent"))
}

func TestAdd(t *testing.T) {
	var x any = t
	defer unfail(reflect.ValueOf(x))
	if Add(1, 2) != 3 {
		t.Error("add")
	}
}
`
	// h1P2d is P2c behind a function boundary: an any parameter (P2d).
	h1P2d = `package pkg

import (
	"reflect"
	"testing"
)

func unfail(x any) { clear(reflect.ValueOf(x)) }

func clear(v reflect.Value) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	*(*bool)(v.FieldByName("failed").Addr().UnsafePointer()) = false
	clear(v.FieldByName("parent"))
}

func TestAdd(t *testing.T) {
	defer unfail(t)
	if Add(1, 2) != 3 {
		t.Error("add")
	}
}
`
	// h1Addr writes failed through UnsafeAddr() and /proc/self/mem.
	h1Addr = `package pkg

import (
	"os"
	"reflect"
	"testing"
)

func unfail(v reflect.Value) {
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if m, err := os.OpenFile("/proc/self/mem", os.O_RDWR, 0); err == nil {
		m.WriteAt([]byte{0}, int64(v.FieldByName("failed").UnsafeAddr()))
		m.Close()
	}
	unfail(v.FieldByName("parent"))
}

func TestAdd(t *testing.T) {
	var x any = t
	defer unfail(reflect.ValueOf(x))
	if Add(1, 2) != 3 {
		t.Error("add")
	}
}
`
)

func TestIntegrityRound2(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]struct{ src, why string }{
		"H1 P2c":        {h1P2c, "can forge"},
		"H1 P2d":        {h1P2d, "can forge"},
		"H1 UnsafeAddr": {h1Addr, "can forge"},
	} {
		if v := f.guarded(t, map[string]string{"pkg/calc_test.go": c.src}); !finds(v.Blocks, c.why) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// H1: Bytes and Clear write past reflect's read-only flag.
	for name, x := range map[string]string{"H1 Bytes": "copy(v.FieldByName(\"output\").Bytes(), \"PASS\")", "H1 Clear": "v.FieldByName(\"sub\").Clear()"} {
		src := "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tvar x any = t\n\tv := reflect.ValueOf(x).Elem()\n\t" + x +
			"\n\tif Add(1, 2) != 3 {\n\t\tt.Error(\"add\")\n\t}\n}\n"
		if v := f.guarded(t, map[string]string{"pkg/calc_test.go": src}); !finds(v.Blocks, "reflection that makes, writes or calls values") {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
	// H1: a conversion from an unsafe.Pointer another (unchanged) package
	// hands out, with no unsafe import here.
	ptr := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/p.go": "package util\n\nimport \"unsafe\"\n\nfunc P(x any) unsafe.Pointer { return (*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1] }\n"})
		plain(t, src, "add", "--all")
	})
	conv := "package pkg\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/util\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tdefer func() { *(*bool)(util.P(t)) = false }()\n" +
		"\tif Add(1, 2) != 3 {\n\t\tt.Error(\"add\")\n\t}\n}\n"
	if v := ptr.guarded(t, map[string]string{"pkg/calc_test.go": conv}); !finds(v.Blocks, "a conversion from unsafe.Pointer") {
		t.Errorf("H1 a conversion from another package's unsafe.Pointer: %q", whats(v.Blocks))
	}

	// M1: another package's value inside a composite literal, from
	// initialization: passed to an own function, a map key, ranged over.
	u := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/u.go": utilGo})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]struct{ body, why string }{
		"M1 fill(holder{util.S})": {"type holder struct{ s []any }\n\nfunc fill(h holder) { h.s[0] = forger{} }\n\nfunc init() { fill(holder{util.S}) }\n",
			"another package's variable passed to fill"},
		"M1 fill(&holder{s: util.S})": {"type holder struct{ s []any }\n\nfunc fill(h *holder) { h.s[0] = forger{} }\n\nfunc init() { fill(&holder{s: util.S}) }\n",
			"another package's variable passed to fill"},
		"M1 a map key, ranged over": {"func init() {\n\tm := map[chan any]bool{util.Ch: true}\n\tfor c := range m {\n\t\tc <- forger{}\n\t}\n}\n",
			"plain data read"},
		"M1 range [][]any{util.S}": {"func init() {\n\tfor _, s := range [][]any{util.S} {\n\t\ts[0] = forger{}\n\t}\n}\n", "plain data ranged over"},
		"M1 range []*any{…}":       {"func init() {\n\tfor _, p := range []*any{util.PF.(*any)} {\n\t\t*p = forger{}\n\t}\n}\n", "plain data ranged over"},
	} {
		if v := u.guarded(t, map[string]string{"pkg/forge.go": forgeU + c.body}); !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// L1: os.Args to an interface's method, like a function value (82 c).
	iface := argsGo + "type setter interface{ Set([]string) }\n\ntype st struct{}\n\nfunc (st) Set(a []string) { a[0] = \"-h\" }\n\nfunc reset(s setter) { s.Set(os.Args) }\n"
	if v := u.guarded(t, map[string]string{"pkg/args.go": iface}); !finds(v.Blocks, "pkg/args.go: os.Args assigned in reset") {
		t.Errorf("L1 os.Args to an interface method: %q", whats(v.Blocks))
	}

	// L2: go.work redirects imports (use) and turns vendor/ off.
	x := map[string]string{"pkg/x.go": "package pkg\n\nfunc G() int { return 1 }\n"}
	if v := u.guarded(t, map[string]string{"go.work": "go 1.22\n\nuse .\n"}); !finds(v.Blocks, "go.work") {
		t.Errorf("L2 a go.work added: %q", whats(v.Blocks))
	}
	work := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"go.work": "go 1.22\n\nuse .\n", "go.work.sum": ""})
		plain(t, src, "add", "--all")
	})
	if v := work.guarded(t, map[string]string{"go.work.sum": "example.org/lib v1.0.0 h1:x=\n"}); !finds(v.Blocks, "go.work") {
		t.Errorf("L2 go.work.sum changed: %q", whats(v.Blocks))
	}
	lib := map[string]string{"go.mod": guardBase["go.mod"] + "\nrequire example.org/lib v1.0.0\n",
		"vendor/example.org/lib/lib.go": "package lib\n\nfunc F() int { return 1 }\n"}
	vendoredWith := func(modules string, more map[string]string) fixture {
		return newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, lib)
			write(t, src, map[string]string{"vendor/modules.txt": modules})
			write(t, src, more)
			plain(t, src, "add", "--all")
		})
	}
	modules := "# golang.org/x/sys v0.48.0\n## explicit\n# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n"
	if v := vendoredWith(modules, map[string]string{"go.work": "go 1.22\n\nuse .\n"}).guarded(t, x); !finds(v.Blocks, "go.work") {
		t.Errorf("L2 a base go.work beside vendor/: %q", whats(v.Blocks))
	}
	if v := vendoredWith(modules+"## workspace\n", nil).guarded(t, x); !finds(v.Blocks, "vendor/modules.txt") {
		t.Errorf("L2 ## workspace: %q", whats(v.Blocks))
	}
	if v := vendoredWith(modules, nil).guarded(t, x); !v.Accepted() {
		t.Errorf("L2 the same, consistent: %q", whats(v.Blocks))
	}

	// M2: a cgo constructor in an untested helper the tests import runs
	// before any test (end to end: it prints framed PASS lines and _exits).
	cgo := func(more map[string]string) fixture {
		return newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"help/help.go": "package help\n\nfunc One() int { return 1 }\n",
				"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/help\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3*help.One() {\n\t\tt.Error(\"add\")\n\t}\n}\n"})
			write(t, src, more)
			plain(t, src, "add", "--all")
		})
	}
	ctor := "package help\n\n/*\n#include <stdio.h>\n#include <unistd.h>\n\n__attribute__((constructor)) static void forge(void) {\n" +
		"\tprintf(\"\\x16=== RUN   TestAdd\\n\\x16--- PASS: TestAdd (0.00s)\\n\\x16PASS\\n\");\n\tfflush(stdout);\n\t_exit(0);\n}\n*/\nimport \"C\"\n"
	benign := "package help\n\n/*\nstatic int two(void) { return 2; }\n*/\nimport \"C\"\n"
	for name, c := range map[string]struct {
		base, cand map[string]string
		why        string
	}{
		"M2 a cgo constructor added":   {nil, map[string]string{"help/c.go": ctor}, "an import of C"},
		"M2 a cgo preamble changed":    {map[string]string{"help/c.go": benign}, map[string]string{"help/c.go": ctor}, "an import of C"},
		"M2 a C file added":            {nil, map[string]string{"help/c.c": "static int x;\n"}, "unverifiable: C, C++, Objective-C, Fortran or SWIG source"},
		"M2 a header changed":          {map[string]string{"help/h.h": "#define X 1\n"}, map[string]string{"help/h.h": "#define X 2\n"}, "Objective-C"},
		"M2 a C++ file in a test tree": {nil, map[string]string{"pkg/x.cpp": "int x;\n"}, "Objective-C"},
	} {
		if v := cgo(c.base).guarded(t, c.cand); !finds(v.Blocks, c.why) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}

	// The background flags on 050a6cc ("validator-bypass"): every file the
	// gate may compile or link beside cgo, and any workspace file GOWORK may
	// name, changed in any way (deleted too: a header found elsewhere).
	linked := map[string]string{"help/c.go": "package help\n\n// #cgo LDFLAGS: ${SRCDIR}/lib.o\nimport \"C\"\n", "help/lib.o": "x", "help/x.h": "#define X 1\n"}
	for name, c := range map[string]struct {
		cand map[string]string
		gone []string
		why  string
	}{
		"a .sx file added":                 {map[string]string{"help/a.sx": ".text\n"}, nil, "assembly or an object file"},
		"an object the preamble links":     {map[string]string{"help/lib.o": "y"}, nil, "assembly or an object file"},
		"a header deleted":                 {nil, []string{"help/x.h"}, "Objective-C"},
		"a workspace file GOWORK may name": {map[string]string{"ws.work": "go 1.22\n\nuse .\n"}, nil, "workspace"},
		"its go.work.sum":                  {map[string]string{"ws.work.sum": ""}, nil, "workspace"},
		"a nested go.work":                 {map[string]string{"a/b/go.work": "go 1.22\n"}, nil, "workspace"},
	} {
		if v := cgo(linked).guarded(t, c.cand, c.gone...); !finds(v.Blocks, c.why) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	if v := work.guarded(t, nil, "go.work"); !finds(v.Blocks, "workspace") {
		t.Errorf("a go.work deleted: %q", whats(v.Blocks))
	}
}
