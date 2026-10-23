// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const forgedWhy = "a testing value not the runner's"

// e2eCalc is the round-8 and round-9 end-to-end test files: a failing
// TestAdd (Add returns a-b) whose check runs on x, with decls added.
func e2eCalc(imports, x, decls string) string {
	return "package pkg\n\nimport (\n" + imports + ")\n\nfunc TestAdd(t *testing.T) {\n\tfunc(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n" +
		"\t\t\tt.Error(\"add\")\n\t\t}\n\t}(" + x + ")\n}\n" + decls
}

// F (P14e, #226 entries 79 and 82): read with real import types, a testing
// value made through an own function, method, field or container, a
// generic used as a value through a signature in another file, reflection
// through any, or an alias of a testing type in another, unchanged package
// forges; each end-to-end source of the round-8 and round-9 checks is a row.
func TestIntegrityRealTypes(t *testing.T) {
	f := newGuardFixture(t)
	holder := "\ntype holder struct{ t *testing.T }\n"
	// decode is TestAdd checking y, which body sets, with decls added (P14e-2a, #226 entry 85).
	decode := func(imports, body, decls string) map[string]string {
		return map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n" + imports + "\t\"testing\"\n)\n\nfunc TestAdd(t *testing.T) {\n" + body +
			"\tif Add(1, 2) != 3 {\n\t\ty.Error(\"add\")\n\t}\n}\n" + decls}
	}
	js, hy := "\t\"encoding/json\"\n", "\tvar h struct{ T *testing.T }\n"
	for name, c := range map[string]guardCase{
		// Entry 79: own functions and methods returning *testing.T, fields, containers.
		"fresh(zt())":         {fake("fresh(zt())", freshGo+"\nfunc zt() *testing.T { return nil }\n"), nil, forgedWhy},
		"fresh(H{}.T())":      {fake("fresh(H{}.T())", freshGo+"\ntype H struct{}\n\nfunc (H) T() *testing.T { return nil }\n"), nil, forgedWhy},
		"fresh(holder{}.t)":   {fake("fresh(holder{}.t)", freshGo+holder), nil, forgedWhy},
		"h.t = t; fresh(h.t)": {fake("func() *testing.T { var h holder; h.t = t; return fresh(h.t) }()", freshGo+holder), nil, forgedWhy},
		"fresh(ts[0])":        {fake("func(ts []*testing.T) *testing.T { return fresh(ts[0]) }([]*testing.T{t})", freshGo), nil, forgedWhy},
		"fresh(<-c)":          {fake("func(c chan *testing.T) *testing.T { c <- t; return fresh(<-c) }(make(chan *testing.T, 1))", freshGo), nil, forgedWhy},
		"fresh(m[0])":         {fake("fresh(m[0])", freshGo+"\ntype TM map[int]*testing.T\n\nvar m TM\n"), nil, forgedWhy},
		"fresh(TP(nil))":      {fake("fresh(TP(nil))", freshGo+"\ntype TP = *testing.T\n"), nil, forgedWhy},
		"*t copied":           {fake("func() *testing.T { c := *t; return &c }()", ""), nil, forgedWhy},
		"reflection on a field": {map[string]string{"pkg/calc_test.go": strings.Replace(fake("t", holder+"\nfunc peek(h *holder) { _ = reflect.ValueOf(h).Elem().Field(0) }\n")["pkg/calc_test.go"],
			"func TestAdd(t *testing.T) {", "func TestAdd(t *testing.T) {\n\tpeek(&holder{t})", 1)}, nil, "reflection on a testing value"},
		// Entry 82 (a): r8 M2's end-to-end source, and its two other forms.
		"Reg(fresh), E2E": {map[string]string{"pkg/g.go": "package pkg\n\nfunc init() { Reg(fresh) }\n\n" + freshGo,
			"pkg/calc_test.go": e2eCalc("\t\"testing\"\n", "G(t)", "\nvar G func(*testing.T) *testing.T\n\nfunc Reg(f func(*testing.T) *testing.T) { G = f }\n")}, nil, forgedWhy},
		"var G FT = fresh": {map[string]string{"pkg/g.go": "package pkg\n\nvar G FT = fresh\n\n" + freshGo,
			"pkg/calc_test.go": e2eCalc("\t\"testing\"\n", "G(t)", "\ntype FT func(*testing.T) *testing.T\n")}, nil, forgedWhy},
		"HT{F: fresh}": {map[string]string{"pkg/g.go": "package pkg\n\nvar H = HT{F: fresh}\n\n" + freshGo,
			"pkg/calc_test.go": e2eCalc("\t\"testing\"\n", "H.F(t)", "\ntype HT struct{ F func(*testing.T) *testing.T }\n")}, nil, forgedWhy},
		// (b): reflection launders a testing value through any (r8, end to end).
		"mk(t) asserted to an interface, E2E": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
			"func TestAdd(t *testing.T) {\n\ty := mk(t).(interface{ Error(...any) })\n\tif Add(1, 2) != 3 {\n\t\ty.Error(\"add\")\n\t}\n}\n\n" + mkGo}, nil, forgedWhy},
		"mk(t) asserted to testing.TB": {fake("func() *testing.T { y := mk(t).(testing.TB); y.Fail(); return t }()", mkGo), nil, forgedWhy},
		"a constrained type parameter": {fake("func() *testing.T { y := as[interface{ Fatalf(string, ...any) }](mk(t)); y.Fatalf(\"\"); return t }()",
			mkGo+"\nfunc as[I any](v any) I { return v.(I) }\n"), nil, forgedWhy},
		// A new import whose types cannot be read: unverifiable (fail closed).
		"an import not loaded":          {map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.com/m/nowhere\"\n\nvar _ = nowhere.X\n"}, nil, "the types of its imports not loaded (example.com/m/nowhere"},
		"a module not required":         {map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.org/none\"\n\nvar _ = none.X\n"}, nil, "the types of its imports not loaded (example.org/none: cannot find module"},
		"an import go list cannot take": {map[string]string{"pkg/x.go": "package pkg\n\nimport \"-modfile/x\"\n"}, nil, "an import path go list cannot take"},
		"an own import not type-checked": {map[string]string{"util/u.go": "package util\n\nvar X int = \"x\"\n", "pkg/x.go": "package pkg\n\nimport \"example.com/m/util\"\n\nvar _ = util.X\n"},
			nil, "the types of its imports not loaded (example.com/m/util: it does not type-check"},
		"a directory replace": {map[string]string{"go.mod": guardBase["go.mod"] + "\nreplace golang.org/x/sys => ../sys\n", "pkg/x.go": "package pkg\n\nimport \"golang.org/x/sys/unix\"\n\nvar _ = unix.S_IFMT\n"},
			nil, "replaces a module by a directory"},
		// Round 2 (Opus #265). H1, M1: every pointer level.
		"H1 .(**testing.T), E2E": {map[string]string{"pkg/calc_test.go": e2eCalc("\t\"reflect\"\n\t\"testing\"\n", "*mk2(t).(**testing.T)", mk2Go)}, nil, forgedWhy},
		"H1 case **testing.T": {map[string]string{"pkg/calc_test.go": e2eCalc("\t\"reflect\"\n\t\"testing\"\n",
			"func() *testing.T {\n\t\tswitch y := mk2(t).(type) {\n\t\tcase **testing.T:\n\t\t\treturn *y\n\t\t}\n\t\treturn nil\n\t}()", mk2Go)}, nil, forgedWhy},
		"H1 as[**testing.T]": {map[string]string{"pkg/calc_test.go": e2eCalc("\t\"reflect\"\n\t\"testing\"\n", "*as[**testing.T](mk2(t))",
			mk2Go+"\nfunc as[I any](v any) I { return v.(I) }\n")}, nil, forgedWhy},
		"M1 an unsafe **testing.T, untested": {map[string]string{"util/u.go": "package util\n\nimport (\n\t\"testing\"\n\t\"unsafe\"\n)\n\n" +
			"func Fresh() *testing.T {\n\tvar p *int\n\treturn *(**testing.T)(unsafe.Pointer(&p))\n}\n"}, nil, "util/u.go: " + forgedWhy},
		// H2: reflection writes a fresh T into a typed field (inputs A2 and C2).
		"H2 A2": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\ntype holder struct{ T *testing.T }\n\n" +
			"func TestAdd(t *testing.T) {\n\th := &holder{}\n\treflect.ValueOf(any(h)).Elem().Field(0).Set(reflect.New(reflect.TypeFor[holder]().Field(0).Type.Elem()))\n" +
			"\tfunc(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n\t}(h.T)\n}\n"}, nil, "reflection on a testing value"},
		"H2 C2": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\ntype holder struct{ T *testing.T }\n\n" +
			"func TestAdd(t *testing.T) {\n\tc := make(chan any, 1)\n\tc <- t\n\tx := <-c\n\th := &holder{}\n" +
			"\treflect.ValueOf(any(h)).Elem().Field(0).Set(reflect.New(reflect.ValueOf(&x).Elem().Elem().Type().Elem()))\n" +
			"\tfunc(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n\t}(h.T)\n}\n"}, nil, "reflection that makes, writes or calls values"},
		// H4: a directory replace however go.mod spells it; L1: patterns and reserved names.
		"H4 an escaped directory": {map[string]string{"go.mod": guardBase["go.mod"] + "\nreplace golang.org/x/sys => \"\\x2fabs/dir\"\n",
			"pkg/x.go": "package pkg\n\nimport \"golang.org/x/sys/unix\"\n\nvar _ = unix.S_IFMT\n"}, nil, "replaces a module by a directory"},
		"H4 a //-directory": {map[string]string{"go.mod": guardBase["go.mod"] + "\nreplace golang.org/x/sys => \"//abs/dir\"\n",
			"pkg/x.go": "package pkg\n\nimport \"golang.org/x/sys/unix\"\n\nvar _ = unix.S_IFMT\n"}, nil, "replaces a module by a directory"},
		"L1 std":    {map[string]string{"pkg/x.go": "package pkg\n\nimport \"std\"\n"}, nil, "an import path go list cannot take (\"std\")"},
		"L1 cmd/go": {map[string]string{"pkg/x.go": "package pkg\n\nimport \"cmd/go\"\n"}, nil, "an import path go list cannot take (\"cmd/go\")"},
		// Round 3 (Mythos #265). H1: a container or field of *testing.T, and the value-making reflect API.
		"H1 MakeSlice and Append into []*testing.T": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
			"func TestAdd(t *testing.T) {\n\tc := make(chan any, 1)\n\tc <- t\n\tv := <-c\n" +
			"\tp := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(v).Elem()), 1, 1).Index(0).Addr()\n" +
			"\ts := reflect.Append(reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(v)), 0, 1), p).Interface().([]*testing.T)\n" +
			"\tfunc(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n\t}(s[0])\n}\n"}, nil, forgedWhy},
		"H1 .(struct{ T *testing.T })": {fake("mk(t).(struct{ T *testing.T }).T", mkGo), nil, forgedWhy},
		"H1 as[[]*testing.T]":          {fake("as[[]*testing.T](mk(t))[0]", mkGo+"\nfunc as[I any](v any) I { return v.(I) }\n"), nil, forgedWhy},
		"H1 MakeSlice alone": {map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "import \"testing\"", "import (\n\t\"reflect\"\n\t\"testing\"\n)", 1) +
			"\nvar _ = reflect.MakeSlice(reflect.TypeOf([]int{}), 1, 1)\n"}, nil, "reflection that makes, writes or calls values (MakeSlice)"},
		// Found in round 3 (end to end: TestAdd passes): a decoder outside the module allocates a T into a field.
		"json.Unmarshal into a *testing.T field": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"encoding/json\"\n\t\"testing\"\n)\n\n" +
			"func TestAdd(t *testing.T) {\n\tvar h struct{ T *testing.T }\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &h)\n\ty := h.T\n" +
			"\tif Add(1, 2) != 3 {\n\t\ty.Error(\"add\")\n\t}\n}\n"}, nil, forgedWhy},
		// P14e-2a, #226 entry 85 (each accepted at 94bff81): a decoder's target is a fact on the value
		// (fillable), however it reaches the decoder.
		"H1 through an any variable": {decode(js, hy+"\tvar a any = &h\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), a)\n\ty := h.T\n", ""), nil, forgedWhy},
		"H1 a []any spread":          {decode(js, hy+"\targs := []any{&h}\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), args[0])\n\ty := h.T\n", ""), nil, forgedWhy},
		"H1 **testing.T behind any":  {decode(js, "\tvar y *testing.T\n\tvar a any = &y\n\t_ = json.Unmarshal([]byte(\"{}\"), a)\n", ""), nil, forgedWhy},
		"H1 a decoder as a value":    {decode(js, hy+"\tdec := json.Unmarshal\n\t_ = dec([]byte(`{\"T\":{}}`), &h)\n\ty := h.T\n", ""), nil, forgedWhy},
		"H1 gob": {decode("\t\"bytes\"\n\t\"encoding/gob\"\n", hy+"\tvar a any = &h\n\t_ = gob.NewDecoder(bytes.NewReader(nil)).Decode(a)\n\ty := h.T\n", ""),
			nil, forgedWhy},
		"H1 xml": {decode("\t\"encoding/xml\"\n", hy+"\tvar a any = &h\n\t_ = xml.Unmarshal([]byte(\"<x><T></T></x>\"), a)\n\ty := h.T\n", ""), nil, forgedWhy},
		"H1 an own func returning any": {decode(js, "\tvar a any = target()\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), a)\n\tvar y *testing.T\n",
			"\ntype Holder struct{ T *testing.T }\n\nfunc target() any { return &Holder{} }\n"), nil, forgedWhy},
		// The fdec10c flag "incomplete fix" (generic Call accepted at 94bff81): reflect.Value's makers through a constraint or an interface.
		"a reflect maker through a constraint": {decode("\t\"reflect\"\n", "\ty := t\n\t_ = call(reflect.ValueOf(func() {}))\n",
			"\nfunc call[V interface{ Call([]reflect.Value) []reflect.Value }](v V) []reflect.Value { return v.Call(nil) }\n"), nil, "reflection that makes, writes or calls values (Call) in linux/amd64 call"},
		"a reflect maker through an interface": {decode("\t\"reflect\"\n", "\ty := t\n\tvar s interface{ Set(reflect.Value) } = reflect.ValueOf(new(int)).Elem()\n\ts.Set(reflect.Zero(reflect.TypeFor[int]()))\n", ""),
			nil, "reflection that makes, writes or calls values (Set) in linux/amd64 TestAdd"},
		// The fdec10c flag "fail-open on redeclaration": one name in files under exclusive
		// tags the guard does not model, with other types (the race pair, same types, is accepted).
		"a name declared again with other types": {map[string]string{"pkg/r_test.go": "//go:build race\n\npackage pkg\n\nconst limit = 1\n",
			"pkg/n_test.go": "//go:build !race\n\npackage pkg\n\nconst limit = \"one\"\n"}, nil, "declared again with other types"},
		// The background flag on P14e-2a "shape check incomplete": a type's methods (a const's value changes no type: the race pair).
		"a type declared again with other methods": {map[string]string{"pkg/r_test.go": "//go:build race\n\npackage pkg\n\ntype probe struct{}\n\nfunc (probe) Error(...any) {}\n",
			"pkg/n_test.go": "//go:build !race\n\npackage pkg\n\ntype probe struct{}\n\nfunc (probe) Log(...any) {}\n"}, nil, "declared again with other types"},
		// H2: an assertion on a value not an interface; the gate cannot compile it.
		"H2 t.(*testing.T)": {map[string]string{"pkg/x_test.go": "package pkg\n\nimport \"testing\"\n\nfunc keep(t *testing.T) { _ = t.(*testing.T) }\n"}, nil, "it does not type-check"},
		// Round 5 (the background detection-bypass flag): reflect's functions as values (end to end: TestAdd passes).
		"reflect functions as values": {map[string]string{"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\n" +
			"type holder struct{ T *testing.T }\n\nfunc TestAdd(t *testing.T) {\n\tvar v any = t\n\tvar h holder\n" +
			"\tvo, nw, set := reflect.ValueOf, reflect.New, reflect.Value.Set\n\tset(vo(&h).Elem().Field(0), nw(vo(v).Type().Elem()))\n" +
			"\tvar y *testing.T = h.T\n\tif Add(1, 2) != 3 {\n\t\ty.Error(\"add\")\n\t}\n}\n"}, nil, "reflection that makes, writes or calls values (ValueOf) in linux/amd64 TestAdd"},
		// Round 5: a file for one platform hides a forgery from the other's types (accepted
		// at 57fb44b). Each platform's files are typed with its own export data: its fact.
		"a forgery typed away on the other platform": {map[string]string{
			"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "t.Fatal(\"add\")", "var y *testing.T = forge(t)\n\t\ty.Fatal(\"add\")", 1),
			"pkg/x_amd64_test.go": "package pkg\n\nimport (\n\t\"encoding/json\"\n\t\"syscall\"\n\t\"testing\"\n)\n\n" +
				"type holder struct{ T *testing.T }\n\nfunc pickH[N, H any](N, H) H { var h H; return h }\n\nfunc forge(t *testing.T) *testing.T {\n" +
				"\th := pickH(syscall.SYS_ARCH_PRCTL, holder{})\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &h)\n\treturn h.T\n}\n"}, nil, forgedWhy},
		// Round 4 (the background fail-open flag): an error whose text holds "redeclared" is still an error.
		"an error naming redeclared": {map[string]string{"pkg/x_test.go": "package pkg\n\nvar _ = redeclared\n"}, nil, "it does not type-check (pkg/x_test.go:3:9: undefined: redeclared)"},
		// M2: value-making reflection in an untested helper of a repository with tests.
		"M2 util.Run": {map[string]string{"util/u.go": "package util\n\nimport \"reflect\"\n\nfunc Run(fn, v any) { reflect.ValueOf(fn).Call([]reflect.Value{reflect.New(reflect.TypeOf(v).Elem())}) }\n",
			"pkg/calc_test.go": strings.Replace(strings.Replace(baseFiles["pkg/calc_test.go"], "import \"testing\"", "import (\n\t\"testing\"\n\n\t\"example.com/m/util\"\n)", 1),
				"if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "util.Run(func(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Fatal(\"add\")\n\t\t}\n\t}, t)", 1)},
			nil, "util/u.go: reflection that makes, writes or calls values"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), c.why)
		}
	}

	// Entry 82 (e): aliases of a testing type already in the base, in
	// another, unchanged package (the round-9 check 2b, end to end).
	b := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/u.go": "package util\n\nimport \"testing\"\n\ntype TP = *testing.T\n\ntype TG[P any] = *testing.T\n\ntype TV = testing.T\n\ntype MV = testing.M\n"})
		plain(t, src, "add", "--all")
	})
	imports := "\t\"reflect\"\n\t\"testing\"\n\n\t\"example.com/m/util\"\n"
	for name, x := range map[string][2]string{
		"b1 .(util.TP)":        {"reflect.New(reflect.TypeOf(util.TP(nil)).Elem()).Interface().(util.TP)"},
		"b2 case util.TP":      {"func() *testing.T {\n\t\tswitch y := mk(t).(type) {\n\t\tcase util.TP:\n\t\t\treturn y\n\t\t}\n\t\treturn nil\n\t}()"},
		"b3 .(util.TG[int])":   {"mk(t).(util.TG[int])"},
		"b4 case util.TG[int]": {"func() *testing.T {\n\t\tswitch y := mk(t).(type) {\n\t\tcase util.TG[int]:\n\t\t\treturn y\n\t\t}\n\t\treturn nil\n\t}()"},
		"new(util.TV)":         {"new(util.TV)"},
		"new(util.MV)":         {"t", "\nvar _ = new(util.MV)\n"}, // only holds' Unalias sees M (no common part)
	} {
		if v := b.guarded(t, map[string]string{"pkg/calc_test.go": e2eCalc(imports, x[0], "\n"+mkGo+x[1])}); !finds(v.Blocks, forgedWhy) {
			t.Errorf("a base alias in util, %s: %q", name, whats(v.Blocks))
		}
	}

	// H3 (Opus #265): facts compare by identity. A base whose helpers hold
	// counted forms (cmp.Or of *testing.T, an assertion to it): the
	// candidate rewrites them and adds a forgery; it is still blocked.
	for name, helper := range map[string]string{
		"a cmp.Or traded":     "func pick(a, b *testing.T) *testing.T { return cmp.Or(a, b) }\n",
		"an assertion traded": "func pick(tb testing.TB) *testing.T { return tb.(*testing.T) }\n",
	} {
		h := newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"pkg/helper_test.go": "package pkg\n\nimport (\n\t\"cmp\"\n\t\"testing\"\n)\n\nvar _ = cmp.Or[int]\n\n" + helper})
			plain(t, src, "add", "--all")
		})
		v := h.guarded(t, map[string]string{"pkg/helper_test.go": "package pkg\n\nimport \"testing\"\n\nfunc pick(a *testing.T) *testing.T { return a }\n",
			"pkg/calc_test.go": fake("fresh(zt())", freshGo+"\nfunc zt() *testing.T { return nil }\n")["pkg/calc_test.go"]})
		if !finds(v.Blocks, forgedWhy) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// Round 4 (the background detection-bypass flag in integrity.go): a base
	// with no Go test; the candidate adds one, and an untested helper that
	// makes values by reflection.
	none := newFixtureWith(t, func(src string) {
		plain(t, src, "rm", "-q", "--cached", "pkg/calc_test.go")
		write(t, src, map[string]string{"go.mod": guardBase["go.mod"], "go.sum": guardBase["go.sum"]})
		plain(t, src, "add", "go.mod", "go.sum")
	})
	if v := none.guarded(t, map[string]string{"pkg/calc_test.go": baseFiles["pkg/calc_test.go"], "util/u.go": "package util\n\nimport \"reflect\"\n\n" +
		"func Run(fn, v any) { reflect.ValueOf(fn).Call([]reflect.Value{reflect.New(reflect.TypeOf(v).Elem())}) }\n"}); !finds(v.Blocks, "util/u.go: reflection that makes") {
		t.Errorf("tests added to a base with none: %q", whats(v.Blocks))
	}

	// Opus #269 round 2. H1 (a regression) and M1 (on main): a helper or a
	// const declared once per platform, or per race tag; the text layer read
	// the first declaration. Each platform's files are now read apart, and a
	// name declared again counts as asserting only when every declaration
	// asserts; a redeclared const's value must match (but a bool's).
	check := "func check(t *testing.T, got, want int) {\n\tif got != want {\n\t\tt.Errorf(\"got %d\", got)\n\t}\n}\n"
	split := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/helper_test.go": "package pkg\n\nimport \"testing\"\n\n" + check,
			"pkg/calc_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tcheck(t, Add(1, 2), 3)\n}\n"})
		plain(t, src, "add", "--all")
	})
	empty := "package pkg\n\nimport \"testing\"\n\nfunc check(t *testing.T, got, want int) {}\n"
	for name, c := range map[string]struct {
		files map[string]string
		why   string
	}{
		"H1 an asserting helper on amd64 only, E2E": {map[string]string{"pkg/a_amd64_test.go": "package pkg\n\nimport \"testing\"\n\n" + check, "pkg/b_arm64_test.go": empty}, "fewer assertions"},
		"H1 the reverse file names":                 {map[string]string{"pkg/a_arm64_test.go": empty, "pkg/b_amd64_test.go": "package pkg\n\nimport \"testing\"\n\n" + check}, "fewer assertions"},
		"M1 a race pair helper": {map[string]string{"pkg/a_test.go": "//go:build race\n\npackage pkg\n\nimport \"testing\"\n\n" + check,
			"pkg/b_test.go": "//go:build !race\n\npackage pkg\n\nimport \"testing\"\n\nfunc check(t *testing.T, got, want int) {}\n"}, "fewer assertions"},
		"H1 a const 22 on amd64 only": {map[string]string{"pkg/a_arm64.go": "package pkg\n\nconst mark = 0\n", "pkg/b_amd64.go": "package pkg\n\nconst mark = 22\n",
			"pkg/m.go": "package pkg\n\nvar sep = string(rune(mark))\n"}, "can forge"},
		"M1 a race pair const": {map[string]string{"pkg/a.go": "//go:build race\n\npackage pkg\n\nconst mark = 0\n", "pkg/b.go": "//go:build !race\n\npackage pkg\n\nconst mark = 22\n",
			"pkg/m.go": "package pkg\n\nvar sep = string(rune(mark))\n"}, "declared again with other types"},
		// The same audit: a test, and initialization's callee, declared once per race tag.
		"M1 init's callee per race tag": {map[string]string{"pkg/a.go": "//go:build race\n\npackage pkg\n\nfunc quit() {}\n",
			"pkg/b.go": "//go:build !race\n\npackage pkg\n\nimport \"fmt\"\n\nfunc quit() { fmt.Println(\"PASS\") }\n", "pkg/c.go": "package pkg\n\nfunc init() { quit() }\n"},
			"reaches a reference it cannot prove safe (fmt.Println)"},
	} {
		var gone []string // the helper moves into the split files
		if !strings.Contains(name, "const") && !strings.Contains(name, "M1 a test") && !strings.Contains(name, "init") {
			gone = []string{"pkg/helper_test.go"}
		}
		if v := split.guarded(t, c.files, gone...); !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}

	// The same audit: a test declared once per race tag counts as its weakest declaration.
	race := func(body string) string {
		return "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n" + body + "}\n"
	}
	rp := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		plain(t, src, "rm", "-q", "-f", "pkg/calc_test.go")
		write(t, src, map[string]string{"pkg/a_test.go": "//go:build race\n\n" + race("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n"),
			"pkg/b_test.go": "//go:build !race\n\n" + race("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n")})
		plain(t, src, "add", "--all")
	})
	if v := rp.guarded(t, map[string]string{"pkg/a_test.go": "//go:build race\n\n" + race("")}); !finds(v.Blocks, "fewer assertions") {
		t.Errorf("M1 a test per race tag: %q", whats(v.Blocks))
	}

	// H2 (Opus #269): an embedded unexported field promotes its exported ones
	// to the decoders; the holder types are already in the base.
	emb := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/suite_test.go": "package pkg\n\nimport \"testing\"\n\ntype env struct{ T *testing.T }\n\ntype suite struct{ env }\n"})
		plain(t, src, "add", "--all")
	})
	for name, dec := range map[string][2]string{
		"json": {"\t\"encoding/json\"\n", "\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &s)\n"},
		"xml":  {"\t\"encoding/xml\"\n", "\t_ = xml.Unmarshal([]byte(\"<s><T></T></s>\"), &s)\n"},
		"gob":  {"\t\"bytes\"\n\t\"encoding/gob\"\n", "\t_ = gob.NewDecoder(bytes.NewReader(nil)).Decode(&s)\n"},
	} {
		v := emb.guarded(t, decode(dec[0], "\tvar s suite\n"+dec[1]+"\tvar y *testing.T = s.T\n", ""))
		if !finds(v.Blocks, forgedWhy) {
			t.Errorf("H2 an embedded holder, %s: %q", name, whats(v.Blocks))
		}
	}

	// H3 (Opus #269, on main): a generic alias cycle crashed the text layer
	// (a fatal stack overflow, not recoverable); it blocks in-process.
	for name, src := range map[string]string{
		"A = B, B = A": "package pkg\n\ntype A[P any] = B[P]\n\ntype B[P any] = A[P]\n",
		"A = A":        "package pkg\n\ntype A[P any] = A[P]\n",
	} {
		if v := f.guarded(t, map[string]string{"pkg/cycle.go": src}); v.Accepted() {
			t.Errorf("H3 an alias cycle, %s: accepted", name)
		}
	}

	// The background flag on 52087d3 "count trading across platforms" (each
	// accepted there): the text layer's counts are per platform too. A skip
	// outside tests, initialization's reach and a 0x16 move from one
	// platform's file to the other's; an assertion moves from an amd64-only
	// helper to an arm64-only one (per platform since 52087d3).
	tt := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/s_amd64.go": "package pkg\n\nfunc skipper(t interface{ Skip(...any) }) { t.Skip() }\n",
			"pkg/s_arm64.go": "package pkg\n\nfunc skipper(t interface{ Skip(...any) }) {}\n",
			"pkg/i_amd64.go": "package pkg\n\nimport \"fmt\"\n\nfunc init() { fmt.Println(\"x\") }\n", "pkg/i_arm64.go": "package pkg\n",
			"pkg/m_amd64.go": "package pkg\n\nvar mark = string(rune(21 + 1))\n", "pkg/m_arm64.go": "package pkg\n\nvar mark = \"\"\n",
			"pkg/h_amd64_test.go": "package pkg\n\nimport \"testing\"\n\n" + check, "pkg/h_arm64_test.go": empty,
			"pkg/calc_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tcheck(t, Add(1, 2), 3)\n}\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]struct {
		files map[string]string
		why   string
	}{
		"a skip traded":       {map[string]string{"pkg/s_amd64.go": "package pkg\n\nfunc skipper(t interface{ Skip(...any) }) {}\n", "pkg/s_arm64.go": "package pkg\n\nfunc skipper(t interface{ Skip(...any) }) { t.Skip() }\n"}, "a skip added outside a test"},
		"init traded":         {map[string]string{"pkg/i_amd64.go": "package pkg\n", "pkg/i_arm64.go": "package pkg\n\nimport \"fmt\"\n\nfunc init() { fmt.Println(\"x\") }\n"}, "reaches a reference it cannot prove safe (fmt.Println)"},
		"a 0x16 traded":       {map[string]string{"pkg/m_amd64.go": "package pkg\n\nvar mark = \"\"\n", "pkg/m_arm64.go": "package pkg\n\nvar mark = string(rune(21 + 1))\n"}, "can forge"},
		"an assertion traded": {map[string]string{"pkg/h_amd64_test.go": empty, "pkg/h_arm64_test.go": "package pkg\n\nimport \"testing\"\n\n" + check}, "linux/amd64 pkg:pkg.TestAdd fewer assertions"},
	} {
		if v := tt.guarded(t, c.files); !finds(v.Blocks, c.why) {
			t.Errorf("traded across platforms, %s: %q", name, whats(v.Blocks))
		}
	}
	// Mythos #269 H1 (a), a regression, and its race twin (on main): a
	// TestMain declared per platform or per race tag; every declaration of
	// the hook is compared, per platform.
	hook := "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n"
	for name, files := range map[string]map[string]string{
		"a TestMain per platform": {"pkg/main_arm64_test.go": hook, "pkg/main_amd64_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestMain(m *testing.M) {}\n"},
		"a TestMain per race tag": {"pkg/main_race_test.go": "//go:build race\n\n" + hook, "pkg/main_norace_test.go": "//go:build !race\n\npackage pkg\n\nimport \"testing\"\n\nfunc TestMain(m *testing.M) {}\n"},
	} {
		if v := f.guarded(t, files, "pkg/main_test.go"); !finds(v.Warnings, "TestMain(*testing.M) of pkg:pkg changed or deleted") {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// H1 (b): a both-platform .(*testing.T) traded for a one-platform "\x16"
	// literal; a second identical exec.Command in a file now built on amd64 only.
	ex := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/helper_test.go": "package pkg\n\nimport \"testing\"\n\nfunc asT(tb testing.TB) *testing.T { return tb.(*testing.T) }\n",
			"pkg/x.go": "package pkg\n\nimport \"os/exec\"\n\nfunc run() { _ = exec.Command(\"true\").Run() }\n"})
		plain(t, src, "add", "--all")
	})
	for name, c := range map[string]struct {
		files map[string]string
		why   string
	}{
		"a 0x16 literal for an assertion": {map[string]string{"pkg/helper_test.go": "package pkg\n\nimport \"testing\"\n\nfunc asT(tb testing.TB) *testing.T { return nil }\n",
			"pkg/mark_amd64_test.go": "package pkg\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\nfunc TestMark(t *testing.T) { fmt.Print(\"\\x16--- PASS: TestAdd (0.00s)\\n\") }\n"}, "can forge"},
		"a second exec.Command on amd64 only": {map[string]string{"pkg/x.go": "//go:build amd64\n\npackage pkg\n\nimport \"os/exec\"\n\nfunc run() {\n\t_ = exec.Command(\"true\").Run()\n\t_ = exec.Command(\"true\").Run()\n}\n"},
			"a new exit, exec"},
	} {
		if v := ex.guarded(t, c.files); !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
	// Mythos #269 M2: a redeclared func's variadic-ness is part of its shape.
	if v := f.guarded(t, map[string]string{"pkg/a_test.go": "//go:build race\n\npackage pkg\n\nfunc sum(a ...int) int { return 0 }\n",
		"pkg/b_test.go": "//go:build !race\n\npackage pkg\n\nfunc sum(a []int) int { return 0 }\n"}); !finds(v.Blocks, "declared again with other types") {
		t.Errorf("a variadic redeclared as a slice: %q", whats(v.Blocks))
	}
	// Mythos #269 L2: gob.Register of a testing value lets a stream allocate one.
	if v := f.guarded(t, decode("\t\"encoding/gob\"\n", "\tvar zt *testing.T\n\tgob.Register(zt)\n\ty := t\n", "")); !finds(v.Blocks, forgedWhy+" (gob.Register(zt))") {
		t.Errorf("gob.Register of a *testing.T: %q", whats(v.Blocks))
	}
	// Round 4 (the lead; the flag "fail-open-state-drift"): the mentions memo
	// is read only once every alias is known. In the base, A names B, which a
	// later file declares, and A is used before B is known; a new alias C
	// naming A is still an alias of a testing type.
	late := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/a_test.go": "package pkg\n\ntype A = struct{ x B }\n\nvar v A\n",
			"pkg/z_test.go": "package pkg\n\nimport \"testing\"\n\ntype B = *testing.T\n"})
		plain(t, src, "add", "--all")
	})
	if v := late.guarded(t, map[string]string{"pkg/c_test.go": "package pkg\n\ntype C = struct{ y A }\n"}); !finds(v.Blocks, "can forge") {
		t.Errorf("an alias of a later alias: %q", whats(v.Blocks))
	}
	// Round 4 (the lead, on main): go/types' validType is exponential on a
	// chain of struct types; a check past the guard's deadline is abandoned
	// and the change unverifiable, in-process. P14e-2c replaced the cap on
	// abandoned checks by the guard's own process (TestIntegrityProcess).
	var chain strings.Builder
	chain.WriteString("package pkg\n")
	for i := range 8 {
		fmt.Fprintf(&chain, "\ntype S%d struct{ A, B, C, D, E, F, G, H S%d }\n", i, i+1)
	}
	chain.WriteString("\ntype S8 int\n")
	unit := f.ordinary(t, unitGo) // before the chain: no check abandoned, the guard's own deadline (Opus #317 M1)
	const chainTime = 2 * time.Second
	oldTime := maxListTime
	maxListTime = chainTime
	begin := time.Now()
	cv := f.guarded(t, map[string]string{"pkg/chain.go": chain.String()})
	if took := time.Since(begin); !finds(cv.Blocks, "over the guard's deadline") || !within(t, took-chainTime, unit, 20) { // its deadline, then the rest of a run
		t.Errorf("a struct chain: %v %q", took, whats(cv.Blocks))
	}
	maxListTime = oldTime

	// Mythos #269 M1 (on main): an alias DAG made mentions exponential (8^n
	// visits at depth n). Counted, not timed (#226 entry 89; the timed bound
	// flaked beside another test binary on #423's CI): each alias's
	// right-hand side is read once per scan and platform (2 per alias,
	// measured), so reads grow linearly with the DAG. At most 4 per alias, at
	// depth 12 and 24; an exponential walk reads thousands at depth 12.
	const maxAliasReads = 4
	reads := func(depth int) int64 {
		var dag strings.Builder
		dag.WriteString("package pkg\n")
		for i := range depth {
			a := fmt.Sprintf("A%d", i+1)
			fmt.Fprintf(&dag, "\ntype A%d = func(%s) %s\n", i, strings.Repeat(a+", ", 7)+a, a)
		}
		fmt.Fprintf(&dag, "\ntype A%d = int\n", depth)
		before := aliasReads.Load()
		if v := f.guarded(t, map[string]string{"pkg/dag.go": dag.String()}); len(v.Blocks) > 0 {
			t.Errorf("an alias DAG of depth %d: %q", depth, whats(v.Blocks))
		}
		return aliasReads.Load() - before
	}
	r12, r24 := reads(12), reads(24)
	if t.Logf("alias reads: %d at depth 12, %d at depth 24", r12, r24); r12 == 0 || r12 > 12*maxAliasReads || r24 > 24*maxAliasReads {
		t.Errorf("an alias DAG: %d reads at depth 12, %d at depth 24; want at most %d per alias", r12, r24, maxAliasReads)
	}

	// The background flag on 52087d3 "parser differential (build tags)": a
	// //go:build line makes +build lines ignored, as go reads them (go vet's
	// buildtag check fails such a package in a default go test).
	if v := f.guarded(t, map[string]string{"pkg/forge.go": strings.Replace(forgeGo, "package pkg", "//go:build linux\n// +build windows\n\npackage pkg", 1)}); !finds(v.Blocks, "cannot prove safe (syscall.Exit)") {
		t.Errorf("a +build line beside //go:build: %q", whats(v.Blocks))
	}

	// The background flag on P14e-2a "fact count trading across platforms":
	// facts are compared per platform. (a) A file built on both platforms
	// keeps its count while one platform loses the fact and the other gains
	// one; (b) a fact moved from x_amd64_test.go to x_arm64_test.go.
	hold := "package pkg\n\nimport \"testing\"\n\ntype V = struct{ T *testing.T }\n"
	trade := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/x_amd64_test.go": hold, "pkg/x_arm64_test.go": hold, "pkg/use_test.go": "package pkg\n\nfunc use() {\n\tvar h V\n\t_ = h\n}\n",
			"pkg/f_amd64_test.go": "package pkg\n\nimport \"testing\"\n\nfunc forge(t *testing.T) *testing.T {\n\tvar h struct{ T *testing.T }\n\treturn h.T\n}\n",
			"pkg/f_arm64_test.go": "package pkg\n\nimport \"testing\"\n\nfunc forge(t *testing.T) *testing.T { return t }\n"})
		plain(t, src, "add", "--all")
	})
	for name, files := range map[string]map[string]string{
		"(a) one platform's fact traded for the other's": {"pkg/x_arm64_test.go": "package pkg\n\ntype V = int\n",
			"pkg/use_test.go": "package pkg\n\nfunc use() {\n\t{\n\t\tvar h V\n\t\t_ = h\n\t}\n\t{\n\t\tvar h V\n\t\t_ = h\n\t}\n}\n"},
		"(b) a fact moved to the other platform's file": {"pkg/f_amd64_test.go": "package pkg\n\nimport \"testing\"\n\nfunc forge(t *testing.T) *testing.T { return t }\n",
			"pkg/f_arm64_test.go": "package pkg\n\nimport (\n\t\"encoding/json\"\n\t\"testing\"\n)\n\nfunc forge(t *testing.T) *testing.T {\n" +
				"\tvar h struct{ T *testing.T }\n\t_ = json.Unmarshal([]byte(`{\"T\":{}}`), &h)\n\treturn h.T\n}\n"},
	} {
		if v := trade.guarded(t, files); !finds(v.Blocks, forgedWhy) {
			t.Errorf("trading across platforms, %s: %q", name, whats(v.Blocks))
		}
	}

	// M1 (Mythos #265): a shadowed operand is another fact (its type is in
	// the key), though the expression reads the same.
	pick := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/helper_test.go": "package pkg\n\nimport \"testing\"\n\nfunc pick(tb testing.TB) *testing.T { return tb.(*testing.T) }\n"})
		plain(t, src, "add", "--all")
	})
	v := pick.guarded(t, map[string]string{"util/u.go": "package util\n\nfunc Fresh(v any) any { return v }\n",
		"pkg/helper_test.go": "package pkg\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/util\"\n)\n\n" +
			"func pick(tb testing.TB) *testing.T {\n\t{\n\t\ttb := util.Fresh(tb)\n\t\treturn tb.(*testing.T)\n\t}\n}\n"})
	if !finds(v.Blocks, "pkg/helper_test.go: "+forgedWhy+" (tb.(*testing.T) of any) in linux/amd64 pick") {
		t.Errorf("M1 the shadowed pick: %q", whats(v.Blocks))
	}

	// H3 (Mythos #265): a module path the standard library may shadow.
	std := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"go.mod": "module testing\n\ngo 1.26.0\n",
			"testing.go": "package testing\n\ntype T = tee\n\ntype tee struct{}\n\nfunc (*tee) Error(...any) {}\nfunc (*tee) Fatal(...any) {}\nfunc (*tee) Helper()      {}\n"})
		plain(t, src, "add", "--all")
	})
	v = std.guarded(t, map[string]string{"pkg/calc_test.go": strings.Replace(baseFiles["pkg/calc_test.go"], "t.Fatal(\"add\")", "c := *t\n\t\tuse(&c)\n\t\tt.Fatal(\"add\")", 1) +
		"\nfunc use(*testing.T) {}\n"})
	if !finds(v.Blocks, "module path testing has no dot in its first element") {
		t.Errorf("H3 module testing: %q", whats(v.Blocks))
	}
}

// alive: pid runs (a zombie is not).
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return err == nil && !strings.Contains(string(b), ") Z ")
}

// mk2Go is H1's (Opus #265): a fresh T behind two pointers.
const mk2Go = "\nfunc mk2(v any) any {\n\tp := reflect.New(reflect.TypeOf(v).Elem())\n\tpp := reflect.New(p.Type())\n\tpp.Elem().Set(p)\n\treturn pp.Interface()\n}\n"

// The controls: testing values the real types show are the runner's (a
// fixture holding t, an interface holding t), a generic of plain data in a
// test, and own and module-cache imports are accepted clean.
func TestIntegrityRealTypesAccepts(t *testing.T) {
	f := newGuardFixture(t)
	goT := baseFiles["pkg/calc_test.go"]
	for name, files := range map[string]map[string]string{
		"a generic of plain data": {"pkg/calc_test.go": strings.Replace(goT, "\tif Add(1, 2)", "\t_ = Add(first(1, 0), 2)\n\tif Add(1, 2)", 1) + // beside the assertion: a changed condition warns (M9, TestIntegrityCases)
			"\nfunc first[T any](v ...T) T { return v[0] }\n"},
		"a fixture holding t": {"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "fx := fixture{t}\n\t\tfx.t.Helper()\n\t\tt.Fatal(\"add\")", 1) +
			"\ntype fixture struct{ t *testing.T }\n"},
		"an interface holding t": {"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "check(t)\n\t\tt.Fatal(\"add\")", 1) +
			"\nfunc check(tb testing.TB) { tb.Helper() }\n"},
		// Round 5's false block, now typed per platform (P14e-2a).
		// One name in each platform's file (P14b's "declared twice" read tags only, and a
		// redeclared alias crashed its text rules with a stack overflow, at 94bff81).
		"one alias in each platform's file": {"pkg/v_amd64.go": "package pkg\n\ntype V = struct{ X int }\n", "pkg/v_arm64.go": "package pkg\n\ntype V = struct{ X int }\n"},
		"files for each platform": {"pkg/x_amd64.go": "package pkg\n\nimport \"syscall\"\n\nconst archAMD = syscall.SYS_ARCH_PRCTL\n",
			"pkg/x_arm64.go": "package pkg\n\nimport \"syscall\"\n\nconst archARM = syscall.SYS_ARCH_SPECIFIC_SYSCALL\n"},
		// M2 (Mythos #265 r2; each blocked at 94bff81): idioms holding t where no decoder can fill it.
		"maps.Keys of test funcs": {"pkg/calc_test.go": strings.Replace(goT, "import \"testing\"", "import (\n\t\"maps\"\n\t\"slices\"\n\t\"testing\"\n)", 1) +
			"\nfunc TestKeys(t *testing.T) {\n\tfor _, k := range slices.Sorted(maps.Keys(map[string]func(*testing.T){\"a\": nil})) {\n\t\tt.Log(k)\n\t}\n}\n"},
		"sync.OnceValue of a fixture": {"pkg/calc_test.go": strings.Replace(goT, "import \"testing\"", "import (\n\t\"sync\"\n\t\"testing\"\n)", 1) +
			"\ntype fixture struct{ t *testing.T }\n\nfunc TestOnce(t *testing.T) {\n\tget := sync.OnceValue(func() *fixture { return &fixture{t: t} })\n\t_ = get()\n}\n"},
		"httptest with a handler holding t": {"pkg/calc_test.go": strings.Replace(goT, "import \"testing\"", "import (\n\t\"net/http\"\n\t\"net/http/httptest\"\n\t\"testing\"\n)", 1) +
			"\ntype handler struct{ t *testing.T }\n\nfunc (h *handler) ServeHTTP(http.ResponseWriter, *http.Request) {}\n\n" +
			"func TestServe(t *testing.T) {\n\ts := httptest.NewServer(&handler{t: t})\n\ts.Close()\n}\n"},
		"a fixture logged":                 {"pkg/calc_test.go": goT + "\ntype fixture struct{ t *testing.T }\n\nfunc TestLog(t *testing.T) {\n\tfx := &fixture{t: t}\n\tt.Logf(\"%+v\", fx)\n}\n"},
		"an own and a module-cache import": {"pkg/x.go": "package pkg\n\nimport (\n\t\"example.com/m/other\"\n\t\"golang.org/x/sys/unix\"\n)\n\nvar mode, v = unix.S_IFMT, other.Value\n"},
	} {
		edge := name == "an own and a module-cache import" // P14b-out: its new x/sys edge warns, nothing else
		if v := f.guarded(t, files); !v.Accepted() || len(v.Warnings) > b2i(edge) || edge && !finds(v.Warnings, "a new import of golang.org/x/sys/unix") {
			t.Errorf("%s: blocks %q, warnings %q; want accepted clean", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// L1 (Mythos #265), found by the replay: an external test sees the
	// package with its tests, and so does an own import that imports it (as
	// go test builds them); nothing else does. One type, not two.
	x := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/box.go": "package pkg\n\nimport \"example.com/m/other\"\n\ntype Box struct{}\n\nfunc Cfg() *other.Config { return nil }\n",
			"pkg/export_test.go": "package pkg\n\nfunc Internal() int { return 1 }\n",
			"util/u.go":          "package util\n\nimport \"example.com/m/pkg\"\n\nfunc Get() pkg.Box { return pkg.Box{} }\n"})
		plain(t, src, "add", "--all")
	})
	ext := strings.Replace(guardBase["pkg/ext_test.go"], "import \"testing\"", "import (\n\t\"testing\"\n\n\t\"example.com/m/other\"\n\t\"example.com/m/pkg\"\n\t\"example.com/m/util\"\n)", 1) +
		"\nfunc TestBox(t *testing.T) {\n\tvar _ pkg.Box = util.Get()\n\tvar _ *other.Config = pkg.Cfg()\n\t_ = pkg.Internal()\n}\n"
	if v := x.guarded(t, map[string]string{"pkg/ext_test.go": ext}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("an external test through an own import: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// The background flag on 52087d3 "parser differential (build tags)": an
	// own import's file under one of the toolchain's default experiments is read.
	if i := slices.IndexFunc(build.Default.ToolTags, func(t string) bool { return strings.HasPrefix(t, "goexperiment.") }); i >= 0 {
		ex := newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"util/e.go": "//go:build " + build.Default.ToolTags[i] + "\n\npackage util\n\nfunc E() int { return 1 }\n"})
			plain(t, src, "add", "--all")
		})
		if v := ex.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.com/m/util\"\n\nfunc Ex() int { return util.E() }\n"}); !v.Accepted() || len(v.Warnings) > 0 {
			t.Errorf("an own file under a default experiment: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
		}
	}
	// L2 (Opus #269): an own import's files under each platform's default
	// level tag (amd64.v1, arm64.v8.0) are read on that platform, whatever the host.
	lv := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"util/v.go": "//go:build amd64.v1\n\npackage util\n\nfunc V() int { return 1 }\n",
			"util/w.go": "//go:build arm64.v8.0\n\npackage util\n\nfunc V() int { return 2 }\n"})
		plain(t, src, "add", "--all")
	})
	if v := lv.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.com/m/util\"\n\nfunc Level() int { return util.V() }\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("level-tagged own files: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// Fail closed: without a loader, or when it fails, a changed Go package is
// unverifiable; a cancelled load stops the guard; go list's time and
// memory bounds and the package and export data bounds hold.
func TestIntegrityTypesFailClosed(t *testing.T) {
	f := newGuardFixture(t)
	change := map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"}
	accept := func(ctx context.Context, list Lister) (Verdict, error) {
		return Accept(ctx, In{Repo: f.repo, Base: f.base, Patch: f.patch(t, change), Policy: f.policy, Guard: Integrity(f.repo, f.base, list, "")})
	}
	// A go that sleeps, one that reports its address-space limit, and one
	// that starts a child (M2, Opus #265: the group is killed, not go alone).
	// Each in a fake GOROOT whose VERSION is the guard's (L2, Mythos #265),
	// and one of another release.
	root, old := t.TempDir(), t.TempDir()
	bin := filepath.Join(root, "bin")
	write(t, root, map[string]string{"VERSION": runtime.Version() + "\ntime x\n", "bin/slow": "#!/bin/sh\nexec sleep 30\n",
		"bin/limit": "#!/bin/sh\nulimit -v >&2\nexit 1\n", "bin/child": "#!/bin/sh\nsleep 30 &\necho $! > " + filepath.Join(bin, "child.pid") + "\nwait\n"})
	write(t, old, map[string]string{"VERSION": "go1.0\n", "bin/go": "#!/bin/sh\nexit 0\n"})
	for _, p := range []string{"slow", "limit", "child"} {
		if err := os.Chmod(filepath.Join(bin, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	failing := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		return nil, errors.New("no toolchain")
	}
	unit := f.ordinary(t, unitGo)
	for name, c := range map[string]struct {
		list Lister
		why  string
		wait time.Duration // each go call's fixed time limit, waited out
	}{
		"no loader":      {nil, "(testing: no Go type loader)", 0},
		"a loader fails": {failing, "(testing: no toolchain)", 0},
		"a missing go":   {LocalLister("/nonexistent/go", nil, time.Minute, 0), "(testing: go list: lstat /nonexistent", 0},
		"a go of another release": {LocalLister(filepath.Join(old, "bin/go"), nil, time.Minute, 0),
			"(testing: go list: the toolchain is go1.0, not the guard's " + runtime.Version() + ")", 0},
		"a go over time":   {LocalLister(filepath.Join(bin, "slow"), nil, 200*time.Millisecond, 0), "(testing: go list: context deadline exceeded)", 200 * time.Millisecond},
		"a go over memory": {LocalLister(filepath.Join(bin, "limit"), nil, time.Minute, 1<<30), "(testing: go list: exit status 1: 1048576)", 0},
	} {
		var calls atomic.Int64 // the go calls the guard makes, each waiting out c.wait at most (Opus #317 M2): at most one per version and platform of its one module (r2 L2)
		list := c.list
		if list != nil {
			list = func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
				calls.Add(1)
				return c.list(ctx, dir, pl, gomod, gosum, paths)
			}
		}
		start := time.Now()
		v, err := accept(context.Background(), list)
		if took := time.Since(start); err != nil || !finds(v.Blocks, "pkg:pkg unverifiable: linux/amd64: the types of its imports not loaded "+c.why) || calls.Load() > int64(2*len(platforms)) || !within(t, took-time.Duration(calls.Load())*c.wait, unit, 20) {
			t.Errorf("%s (%v, %d go calls): %v %q", name, time.Since(start), calls.Load(), err, whats(v.Blocks))
		}
	}
	if v, err := accept(context.Background(), LocalLister(filepath.Join(bin, "child"), nil, 300*time.Millisecond, 0)); err != nil || v.Accepted() {
		t.Errorf("a go with a child: %v %q", err, whats(v.Blocks))
	}
	b, err := os.ReadFile(filepath.Join(bin, "child.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for end := time.Now().Add(5 * time.Second); err == nil && pid > 0 && alive(pid) && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || pid <= 0 || alive(pid) {
		t.Errorf("go's child after a timeout: pid %d, %v, alive %v", pid, err, pid > 0 && alive(pid))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		cancel()
		return testLister(t)(ctx, dir, pl, gomod, gosum, paths)
	}
	if _, err := accept(ctx, cancelling); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled while listing: %v", err)
	}
	// H2 (Mythos #265): a guard fault blocks, it does not crash.
	panicking := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		panic("a lister fault")
	}
	if v, err := accept(context.Background(), panicking); err != nil || !finds(v.Blocks, "unverifiable: the guard failed (a lister fault)") {
		t.Errorf("a panic in the guard: %v %q", err, whats(v.Blocks))
	}
	empty := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
		panic("")
	} // L2, Opus #287: a panic whatever its text
	if v, err := accept(context.Background(), empty); err != nil || !finds(v.Blocks, "unverifiable: the guard failed ()") {
		t.Errorf("an empty panic in the guard: %v %q", err, whats(v.Blocks))
	}
	oldPkgs, oldExport, oldRuns := maxPkgs, maxExport, maxListRuns
	t.Cleanup(func() { maxPkgs, maxExport, maxListRuns = oldPkgs, oldExport, oldRuns })
	maxListRuns = 1 // L3: the base's run, then none for the candidate
	if v := f.guarded(t, change); !finds(v.Blocks, "over the guard's go list bound") {
		t.Errorf("past the go list run bound: %q", whats(v.Blocks))
	}
	maxListRuns, maxPkgs = oldRuns, 0
	if v := f.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.com/m/other\"\n\nvar _ = other.Value\n"}); !finds(v.Blocks, "over the guard's package bound") {
		t.Errorf("past the package bound: %q", whats(v.Blocks))
	}
	maxPkgs, maxExport = oldPkgs, 1<<10
	if v := f.guarded(t, change); !finds(v.Blocks, "over the guard's export data bound") {
		t.Errorf("past the export data bound: %q", whats(v.Blocks))
	}
}
