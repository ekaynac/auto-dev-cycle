// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"strings"
	"testing"
)

// helpClear is Mythos #271 H1 input 2's untested helper: a raw-pointer maker
// through an own interface, and a conversion from a type parameter
// constrained by unsafe.Pointer.
const helpClear = `package help

import "unsafe"

func deref[P ~unsafe.Pointer, T any](p P) *T { return (*T)(p) }

func Clear(a interface{ UnsafeAddr() uintptr }) { *deref[unsafe.Pointer, bool](unsafe.Pointer(a.UnsafeAddr())) = false }
`

// unfailAny walks t's failed flags behind an any parameter (no mention of t
// reaches reflect in the test).
const unfailAny = `

func Unfail(x any) {
	v := reflect.ValueOf(x).Elem()
	for v.IsValid() {
		Clear(v.FieldByName("failed"))
		p := v.FieldByName("parent")
		if p.IsNil() {
			break
		}
		v = p.Elem()
	}
}
`

// testUsing is a failing TestAdd that defers body, importing help.
func testUsing(imports, body string) string {
	return "package pkg\n\nimport (\n" + imports + "\t\"testing\"\n\n\t\"example.com/m/help\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tdefer func() {\n" + body +
		"\t}()\n\tif Add(1, 2) != 3 {\n\t\tt.Error(\"add\")\n\t}\n}\n"
}

// F (#271 round 3, Mythos): each row reproduced first on 050a6cc.
func TestIntegrityRound3(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"help/help.go": "package help\n\nfunc One() int { return 1 }\n"})
		plain(t, src, "add", "--all")
	})
	walk := "\t\tvar x any = t\n\t\tv := reflect.ValueOf(x).Elem()\n\t\tfor v.IsValid() {\n\t\t\thelp.Clear(v.FieldByName(\"failed\"))\n" +
		"\t\t\tp := v.FieldByName(\"parent\")\n\t\t\tif p.IsNil() {\n\t\t\t\tbreak\n\t\t\t}\n\t\t\tv = p.Elem()\n\t\t}\n"
	input1 := strings.Replace(h1Addr, `int64(v.FieldByName("failed").UnsafeAddr())`, `int64(addr(v.FieldByName("failed")))`, 1) +
		"\nfunc addr(v reflect.Value) uintptr {\n\tvar a interface{ UnsafeAddr() uintptr } = v\n\treturn a.UnsafeAddr()\n}\n"
	ptrForm := strings.Replace(input1, "interface{ UnsafeAddr() uintptr } = v\n\treturn a.UnsafeAddr()", "interface{ Pointer() uintptr } = v.FieldByName(\"parent\")\n\treturn a.Pointer() + 73", 1)
	q := strings.Replace(strings.Replace(helpClear, "*deref[unsafe.Pointer, bool](unsafe.Pointer(", "*deref[Q, bool](Q(unsafe.Pointer(", 1), "UnsafeAddr())) = false", "UnsafeAddr()))) = false", 1) +
		"\ntype Q unsafe.Pointer\n"
	for name, c := range map[string]map[string]string{
		"H1 input 1, UnsafeAddr through an own interface": {"pkg/calc_test.go": input1},
		"H1 the Pointer() interface form":                 {"pkg/calc_test.go": ptrForm},
		"H1 the UnsafePointer() interface form": {"pkg/calc_test.go": testUsing("\t\"reflect\"\n", "\t\tvar x any = t\n\t\tvar a help.UP = reflect.ValueOf(x).Elem().FieldByName(\"parent\")\n\t\thelp.Zero(a.UnsafePointer())\n"),
			"help/up.go": "package help\n\nimport \"unsafe\"\n\ntype UP = interface{ UnsafePointer() unsafe.Pointer }\n\nfunc Zero(p unsafe.Pointer) { *deref[unsafe.Pointer, bool](p) = false }\n\nfunc deref[P ~unsafe.Pointer, T any](p P) *T { return (*T)(p) }\n"},
		"H1 input 2":               {"help/clear.go": helpClear, "pkg/calc_test.go": testUsing("\t\"reflect\"\n", walk)},
		"H1 input 2 behind an any": {"help/clear.go": strings.Replace(helpClear, "import \"unsafe\"", "import (\n\t\"reflect\"\n\t\"unsafe\"\n)", 1) + unfailAny, "pkg/calc_test.go": testUsing("", "\t\thelp.Unfail(t)\n")},
		"H1 deref with a type Q":   {"help/clear.go": strings.Replace(q, "import \"unsafe\"", "import (\n\t\"reflect\"\n\t\"unsafe\"\n)", 1) + unfailAny, "pkg/calc_test.go": testUsing("", "\t\thelp.Unfail(t)\n")},
	} {
		if v := f.guarded(t, c); !finds(v.Blocks, "can forge") && !finds(v.Blocks, "reflection on a testing value") {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}

	// M2: os.Args rewritten in place by an inert package (flag.Parse reads it
	// after every init); L1: a function value that is a call's result or a
	// dereference.
	for name, c := range map[string]struct{ file, src, why string }{
		"M2 slices.Replace(os.Args) from init": {"pkg/args.go", argsGo + "import \"slices\"\n\nfunc init() { slices.Replace(os.Args, 1, 2, \"-test.run=Nope\") }\n",
			"os.Args assigned in init"},
		"M2 sort.Strings(os.Args) from init": {"pkg/args.go", argsGo + "import \"sort\"\n\nfunc init() { sort.Strings(os.Args) }\n", "os.Args assigned in init"},
		"M2 sort.Strings(os.Args) in a test": {"pkg/args_test.go", "package pkg\n\nimport (\n\t\"os\"\n\t\"sort\"\n\t\"testing\"\n)\n\nfunc TestArgs(t *testing.T) { sort.Strings(os.Args) }\n",
			"os.Args assigned in TestArgs"},
		"L1 getter()(os.Args)": {"pkg/args.go", argsGo + "func getter() func([]string) { return func(a []string) { a[0] = \"-h\" } }\n\nfunc reset() { getter()(os.Args) }\n",
			"os.Args assigned in reset"},
		"L1 (*fp)(os.Args)": {"pkg/args.go", argsGo + "var fp = new(func([]string))\n\nfunc reset() { (*fp)(os.Args) }\n", "os.Args assigned in reset"},
	} {
		if v := f.guarded(t, map[string]string{c.file: c.src}); !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// The flag on 9b72e12 (decision A): os.Args passed to any callee but a
	// short read-only allowlist is written, a reflect.Copy or an unchanged
	// base helper included.
	fill := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/fill.go": "package util\n\nfunc Fill(s []string) { s[0] = \"-h\" }\n"})
		plain(t, src, "add", "--all")
	})
	refl := "import \"reflect\"\n\nfunc %s { reflect.Copy(reflect.ValueOf(os.Args), reflect.ValueOf([]string{\"-h\"})) }\n"
	util := "import \"example.com/m/util\"\n\nfunc %s { util.Fill(os.Args) }\n"
	for name, c := range map[string]struct{ src, why string }{
		"reflect.Copy(os.Args) from init":     {argsGo + strings.Replace(refl, "%s", "init()", 1), "os.Args assigned in init"},
		"reflect.Copy(os.Args) in reset":      {argsGo + strings.Replace(refl, "%s", "reset()", 1), "os.Args assigned in reset"},
		"util.Fill(os.Args) from init":        {argsGo + strings.Replace(util, "%s", "init()", 1), "os.Args assigned in init"},
		"util.Fill(os.Args) in reset":         {argsGo + strings.Replace(util, "%s", "reset()", 1), "os.Args assigned in reset"},
		"flag.CommandLine.Parse(os.Args[1:])": {argsGo + "import \"flag\"\n\nfunc parse() { _ = flag.CommandLine.Parse(os.Args[1:]) }\n", ""},
	} {
		if v := fill.guarded(t, map[string]string{"pkg/args.go": c.src}); c.why == "" && !v.Accepted() || c.why != "" && !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// L1's limit: another package's function (unresolved in the text layer)
	// is not a value; only the in-place writers count.
	if v := f.guarded(t, map[string]string{"pkg/args.go": argsGo + "import \"strings\"\n\nfunc line() string { return strings.Join(os.Args, \" \") }\n"}); !v.Accepted() {
		t.Errorf("L1 strings.Join(os.Args): %q", whats(v.Blocks))
	}

	// L2: go reads a go.mod with no go directive as go 1.16, so vendor/ is on.
	nogo := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"go.mod": "module example.com/m\n", "vendor/example.org/lib/lib.go": "package lib\n"})
		plain(t, src, "add", "--all")
	})
	if v := nogo.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nfunc G() int { return 1 }\n"}); !finds(v.Blocks, "vendor/ directory without vendor/modules.txt") {
		t.Errorf("L2 no go directive: %q", whats(v.Blocks))
	}

	// Round 4 (Mythos r2 on 9b72e12). M1: a raw-pointer maker's signature
	// by type, an alias or a type-parameter result too (behind an any: the
	// text rule does not reach them).
	withReflect := func(src string) string {
		return strings.Replace(src, "import \"unsafe\"", "import (\n\t\"reflect\"\n\t\"unsafe\"\n)", 1) + unfailAny
	}
	generic := "package help\n\nimport \"unsafe\"\n\ntype Addr[R any] interface{ UnsafeAddr() R }\n\nfunc Get[R any](a Addr[R]) R { return a.UnsafeAddr() }\n\n" +
		"func deref[P ~unsafe.Pointer, T any](p P) *T { return (*T)(p) }\n\nfunc Clear(v reflect.Value) { *deref[unsafe.Pointer, bool](unsafe.Pointer(Get[uintptr](v))) = false }\n"
	for name, help := range map[string]string{
		"M1 interface{ UnsafeAddr() U }, U = uintptr": withReflect(strings.Replace(helpClear, "interface{ UnsafeAddr() uintptr }", "interface{ UnsafeAddr() U }", 1) + "\ntype U = uintptr\n"),
		"M1 Addr[R] interface{ UnsafeAddr() R }":      withReflect(generic),
		// The flag on a8a9ffd: a named result; a named type over uintptr (no
		// reflect.Value satisfies it: a false block, by its underlying type).
		"M1 interface{ UnsafeAddr() (r uintptr) }": withReflect(strings.Replace(helpClear, "interface{ UnsafeAddr() uintptr }", "interface{ UnsafeAddr() (r uintptr) }", 1)),
	} {
		if v := f.guarded(t, map[string]string{"help/clear.go": help, "pkg/calc_test.go": testUsing("", "\t\thelp.Unfail(t)\n")}); !finds(v.Blocks, "can forge") {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	if v := f.guarded(t, map[string]string{"help/u.go": "package help\n\ntype U uintptr\n\ntype A interface{ UnsafeAddr() U }\n\nfunc Get(a A) U { return a.UnsafeAddr() }\n"}); !finds(v.Blocks, "can forge") {
		t.Errorf("M1 interface{ UnsafeAddr() U }, U uintptr (a false block): %q", whats(v.Blocks))
	}
	// M2: a method of another package's (unresolved) type keeps main's
	// reading: its arguments are not counted.
	if v := f.guarded(t, map[string]string{"pkg/args.go": argsGo + "import \"flag\"\n\nfunc parse() {\n\tfs := flag.NewFlagSet(\"x\", flag.ContinueOnError)\n\t_ = fs.Parse(os.Args[1:])\n}\n"}); !v.Accepted() {
		t.Errorf("M2 fs.Parse(os.Args[1:]): %q", whats(v.Blocks))
	}
	// H1: SWIG turns cgo on by itself: the .swig file is a non-Go file in a repository that uses cgo.
	if v := f.guarded(t, map[string]string{"help/x.swig": "%module x\n%{\n__attribute__((constructor)) static void forge(void) {}\n%}\n"}); !finds(v.Blocks, "uses cgo") {
		t.Errorf("H1 a .swig file without cgo: %q", whats(v.Blocks))
	}
}
