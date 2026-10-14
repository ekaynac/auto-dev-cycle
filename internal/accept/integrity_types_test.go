// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		"a generic of plain data": {"pkg/calc_test.go": strings.Replace(goT, "Add(1, 2)", "Add(first(1, 0), 2)", 1) +
			"\nfunc first[T any](v ...T) T { return v[0] }\n"},
		"a fixture holding t": {"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "fx := fixture{t}\n\t\tfx.t.Helper()\n\t\tt.Fatal(\"add\")", 1) +
			"\ntype fixture struct{ t *testing.T }\n"},
		"an interface holding t": {"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "check(t)\n\t\tt.Fatal(\"add\")", 1) +
			"\nfunc check(tb testing.TB) { tb.Helper() }\n"},
		"an own and a module-cache import": {"pkg/x.go": "package pkg\n\nimport (\n\t\"example.com/m/other\"\n\t\"golang.org/x/sys/unix\"\n)\n\nvar mode, v = unix.S_IFMT, other.Value\n"},
	} {
		if v := f.guarded(t, files); !v.Accepted() || len(v.Warnings) > 0 {
			t.Errorf("%s: blocks %q, warnings %q; want accepted clean", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
}

// Fail closed: without a loader, or when it fails, a changed Go package is
// unverifiable; a cancelled load stops the guard; go list's time and
// memory bounds and the package and export data bounds hold.
func TestIntegrityTypesFailClosed(t *testing.T) {
	f := newGuardFixture(t)
	change := map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"}
	accept := func(ctx context.Context, list Lister) (Verdict, error) {
		return Accept(ctx, In{Repo: f.repo, Base: f.base, Patch: f.patch(t, change), Policy: f.policy, Guard: Integrity(f.repo, f.base, list)})
	}
	// A go that sleeps, one that reports its address-space limit, and one
	// that starts a child (M2, Opus #265: the group is killed, not go alone).
	bin := t.TempDir()
	write(t, bin, map[string]string{"slow": "#!/bin/sh\nexec sleep 30\n", "limit": "#!/bin/sh\nulimit -v >&2\nexit 1\n",
		"child": "#!/bin/sh\nsleep 30 &\necho $! > " + filepath.Join(bin, "child.pid") + "\nwait\n"})
	for _, p := range []string{"slow", "limit", "child"} {
		if err := os.Chmod(filepath.Join(bin, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	failing := func(context.Context, []byte, []byte, []string) (map[string]Listed, error) {
		return nil, errors.New("no toolchain")
	}
	for name, c := range map[string]struct {
		list Lister
		why  string
	}{
		"no loader":        {nil, "(testing: no Go type loader)"},
		"a loader fails":   {failing, "(testing: no toolchain)"},
		"a missing go":     {LocalLister("/nonexistent/go", nil, time.Minute, 0), "(testing: go list: exit status 127"},
		"a go over time":   {LocalLister(filepath.Join(bin, "slow"), nil, 200*time.Millisecond, 0), "(testing: go list: context deadline exceeded)"},
		"a go over memory": {LocalLister(filepath.Join(bin, "limit"), nil, time.Minute, 1<<30), "(testing: go list: exit status 1: 1048576)"},
	} {
		start := time.Now()
		v, err := accept(context.Background(), c.list)
		if err != nil || !finds(v.Blocks, "pkg:pkg unverifiable: the types of its imports not loaded "+c.why) || time.Since(start) > 20*time.Second {
			t.Errorf("%s (%v): %v %q", name, time.Since(start), err, whats(v.Blocks))
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
	cancelling := func(ctx context.Context, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		cancel()
		return testLister(t)(ctx, gomod, gosum, paths)
	}
	if _, err := accept(ctx, cancelling); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled while listing: %v", err)
	}
	oldPkgs, oldExport := maxPkgs, maxExport
	t.Cleanup(func() { maxPkgs, maxExport = oldPkgs, oldExport })
	maxPkgs = 0
	if v := f.guarded(t, map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.com/m/other\"\n\nvar _ = other.Value\n"}); !finds(v.Blocks, "over the guard's package bound") {
		t.Errorf("past the package bound: %q", whats(v.Blocks))
	}
	maxPkgs, maxExport = oldPkgs, 1<<10
	if v := f.guarded(t, change); !finds(v.Blocks, "over the guard's export data bound") {
		t.Errorf("past the export data bound: %q", whats(v.Blocks))
	}
}
