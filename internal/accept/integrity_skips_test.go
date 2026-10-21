// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// skipsBase is P14d-a's test file: skipping helpers (a function, a method),
// P14d-a2's forms (package variables holding one, a literal and an alias of
// each, a copy, an interface), a runner, and helpers that are no skipping
// helpers.
const skipsBase = `package pkg

import (
	"encoding/xml"
	"strings"
	"testing"
)

func requireDocker(t *testing.T) { t.Skip("unavailable") }

type h struct{}

func (h) require(t *testing.T) { t.Skip("unavailable") }

type req interface{ require(*testing.T) }

var pkgSkip = requireDocker

var second = pkgSkip

var skipIt = func(t *testing.T) { t.Skip("unavailable") }

var litAlias = skipIt

func with(t *testing.T, f func(*testing.T)) { f(t) }

func requireCopy(t *testing.T) {
	u := t
	u.Skip("unavailable")
}

func check(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("check")
	}
}

func needEnv(t *testing.T) {
	name := "X"
	t.Skipf("no %s", name)
}

func needErr(t *testing.T, err error) {
	if err != nil {
		t.Skip(err)
	}
}

func needAll(t *testing.T) { requireDocker(t) }

func needThenDecode(t *testing.T, d *xml.Decoder) {
	t.Skip("x")
	_ = d.Skip()
}

var needVarLit = func(t *testing.T) { requireDocker(t) }

var needs = []func(*testing.T){requireDocker}

var byName = map[string]func(*testing.T){"docker": requireDocker}

var cfg = struct{ need func(*testing.T) }{requireDocker}

var holder struct{ f func(*testing.T) }

func init() { holder.f = requireDocker }

func needCyc(t *testing.T) {
	u := t
	u = u
	u.Skip("x")
}

func rebound(t *testing.T) {
	u := t
	u = nil
	w := u
	w.Skip()
}

func needCyc2(t *testing.T) {
	a := t
	b := a
	a = b
	a.Skip("x")
}

func TestChecked(t *testing.T) { check(t) }

func needThenNil(t *testing.T) {
	t.Skip("unavailable")
	t = nil
}

func needCopyThenNil(t *testing.T) {
	u := t
	u.Skip("unavailable")
	u = nil
}

func runSkipped(t *testing.T) {
	t.Run("x", func(t *testing.T) { t.Skip("x") })
}

func needDeref(t *testing.T) { (*t).Skip("x") }

type TT = testing.T

type TBA = testing.TB

type PTT = *TT

func needAlias(t *TT) { t.Skip("x") }

func needAliasTB(tb TBA) { tb.SkipNow() }

func needParen(t (*testing.T)) { t.Skip("x") }

func needStarParen(t *(testing.T)) { t.Skip("x") }

func needPtrAlias(t PTT) { t.Skip("x") }

func viaCopy(t *testing.T) {
	u := t
	u.Skip("x")
}

func TestViaCopy(t *testing.T) {
	viaCopy(t)
	if Add(1, 1) != 2 {
		t.Fatal("add")
	}
}

type S struct{ URL string }

func requireServer(t *testing.T) *S {
	t.Skip("no server")
	return &S{}
}

var shared *S

func setup(t *testing.T) { shared = requireServer(t) }

func url() string {
	if shared == nil {
		return ""
	}
	return shared.URL
}

var client *S

func connect(t *testing.T) { client = requireServer(t) }

func closeClient() { client = nil }

func pick() func(*testing.T) { return requireDocker }

var picked = pick()

func register(f func(*testing.T)) bool { return f != nil }

var registered = register(requireDocker)

var count = len([]func(*testing.T){requireDocker})

func skipElement(t *testing.T) error {
	d := xml.NewDecoder(strings.NewReader(t.Name()))
	return d.Skip()
}

func skipDecoder(d *xml.Decoder) error { return d.Skip() }

func TestPlain(t *testing.T) {
	_ = requireDocker
	if Add(1, 2) != 3 {
		t.Fatal("add")
	}
}
`

// raceSkip is requireX, skipping in the race variant only (Opus #292 M3).
func raceSkip(tag, body string) string {
	return "//go:build " + tag + "\n\npackage pkg\n\nimport \"testing\"\n\nfunc requireX(t *testing.T) {" + body + "}\n"
}

// TestIntegritySkips (P14d-a1; Astra #255 H6, Opus #255 M5, the reviews of
// #292 and #315): a skip added through an existing helper that skips its
// own testing parameter directly, by any use, blocks, a later assignment
// of the parameter notwithstanding; propagated skips never offset a direct
// one; a helper calling another type's Skip is none. P14d-a2's forms are
// accepted here, as stated.
func TestIntegritySkips(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/skips_test.go": skipsBase, "pkg/race_skip_test.go": raceSkip("race", ` t.Skip("race") `),
			"pkg/norace_skip_test.go": raceSkip("!race", "")})
		plain(t, src, "add", "--all")
	})
	plainTest := "\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"
	in := func(code string) map[string]string {
		return map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, plainTest, code+plainTest, 1)}
	}
	skipped := "TestPlain a test skipped"
	for name, c := range map[string]struct {
		files map[string]string
		block string // "": accepted with no warning
	}{
		"a skipping helper called":            {in("\trequireDocker(t)\n"), skipped},
		"through a local":                     {in("\tf := requireDocker\n\tf(t)\n"), skipped},
		"a method called":                     {in("\th{}.require(t)\n"), skipped},
		"a method value":                      {in("\tg := h{}.require\n\tg(t)\n"), skipped},
		"SkipNow by reflection":               {map[string]string{"pkg/skips_test.go": strings.Replace(strings.Replace(skipsBase, plainTest, "\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n"+plainTest, 1), "\"encoding/xml\"", "\"encoding/xml\"\n\t\"reflect\"", 1)}, "unverifiable: reflection on a testing value"},
		"an asserting helper (control)":       {in("\tcheck(t)\n"), ""},
		"another file only (control)":         {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		"skipRebound":                         {in("\tx := t\n\trequireDocker(x)\n\tx = nil\n\t_ = x\n"), skipped},
		"skipSlice":                           {in("\t[]func(*testing.T){requireDocker}[0](t)\n"), skipped},
		"skipField":                           {in("\tstruct{ f func(*testing.T) }{requireDocker}.f(t)\n"), skipped},
		"skipWith":                            {in("\twith(t, requireDocker)\n"), skipped},
		"skipRaceVariant":                     {in("\trequireX(t)\n"), skipped},
		"Skipf with an identifier (C1)":       {in("\tneedEnv(t)\n"), skipped},
		"Skip of an error (C1)":               {in("\tneedErr(t, nil)\n"), skipped},
		"a skip, then another type's Skip":    {in("\tneedThenDecode(t, nil)\n"), skipped}, // never reset (C1)
		"a skip, then the parameter nil (H1)": {in("\tneedThenNil(t)\n"), skipped},         // Astra #315 H1
		"through (*t)":                        {in("\tneedDeref(t)\n"), skipped},
		"a type alias of testing.T (M1)":      {in("\tneedAlias(t)\n"), skipped}, // Opus #350 M1
		"a type alias of testing.TB (M1)":     {in("\tneedAliasTB(t)\n"), skipped},
		"a parenthesized type (M1)":           {in("\tneedParen(t)\n"), skipped},
		"a pointer to a parenthesized type":   {in("\tneedStarParen(t)\n"), skipped},
		"an alias of a pointer alias":         {in("\tneedPtrAlias(t)\n"), skipped},
		// Opus #350 M2, stated (a false block): a helper skipping through a
		// copy, rewritten to skip its parameter directly, blocks its callers.
		"a2 helper rewritten to direct (M2)":  {map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, "\tu := t\n\tu.Skip(\"x\")\n", "\tt.Skip(\"x\")\n", 1)}, "TestViaCopy a test skipped"},
		"a held result, used (r2 M1)":         {in("\t_ = url()\n"), ""},
		"a held result, read (r2 M1)":         {in("\tif shared != nil {\n\t\tt.Log(shared.URL)\n\t}\n"), ""},
		"a held result, cleared (r2 M1)":      {in("\tdefer closeClient()\n"), ""},
		"a plain result of a helper argument": {in("\tif !registered {\n\t\tt.Fatal(\"r\")\n\t}\n"), ""},
		"a plain result of a helper slice":    {in("\tt.Log(count)\n"), ""},
		"a copy of a rebound copy":            {in("\trebound(t)\n"), ""},
		"a propagated use for a direct skip":  {map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, "\t_ = requireDocker\n", "\tt.Skip(\"unavailable\")\n", 1)}, skipped}, // Astra #292 r2 1
		"another type's Skip, from t":         {in("\t_ = skipElement(t)\n"), ""},                                                                                                       // Astra #292 r2 4
		"another type's Skip":                 {in("\t_ = skipDecoder(nil)\n"), ""},                                                                                                     // Astra #292 M7
		// P14d-a2's, accepted here (stated): a copy, a package variable,
		// an interface's method, a helper calling one, a nested literal's
		// own parameter.
		"a2: a copy skips, then nil":     {in("\tneedCopyThenNil(t)\n"), ""},
		"a2: through a package variable": {in("\tpkgSkip(t)\n"), ""},
		"a2: an interface's method":      {in("\tvar r req = h{}\n\tr.require(t)\n"), ""},
		"a2: a helper calling a helper":  {in("\tneedAll(t)\n"), ""},
		"a2: a subtest's own parameter":  {in("\trunSkipped(t)\n"), ""},
	} {
		v := f.guarded(t, c.files)
		switch {
		case c.block != "" && !finds(v.Blocks, c.block):
			t.Errorf("%s: blocks %q, want %q", name, whats(v.Blocks), c.block)
		case c.block == "" && (!v.Accepted() || len(v.Warnings) > 0):
			t.Errorf("%s: blocks %q, warnings %q, want accepted clean", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
}

// TestIntegritySkipsBound (Astra #292 r2 6, Opus #315 H1): the skip
// layer reads each body with a testing parameter once a scan, charged to
// its own bound and checked before each reading: twenty 20 KB helpers under
// a 128 KiB bound are read at most seven times in all (with the base's
// bodies, under 30 readings), never twenty a scan (80); the package is then
// unverifiable. Readings are counted, never timed.
func TestIntegritySkipsBound(t *testing.T) {
	f := newGuardFixture(t)
	old := maxSkipWork
	t.Cleanup(func() { maxSkipWork = old })
	maxSkipWork = 1 << 17
	if v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}); !v.Accepted() || len(v.Warnings) > 0 {
		t.Fatalf("the control under the lowered bound: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n")
	for i := range 20 {
		fmt.Fprintf(&b, "\nfunc helper%d(t *testing.T) {\n\tx := 0\n%s\t_ = x\n}\n", i, strings.Repeat("\tx++\n", 4000))
	}
	before := skipReads.Load()
	v := f.guarded(t, map[string]string{"pkg/helpers_test.go": b.String()})
	n := skipReads.Load() - before
	t.Logf("twenty 20 KB helpers: %d readings", n)
	if !finds(v.Blocks, "skipping helpers past the guard's bound") || n > 30 {
		t.Errorf("twenty 20 KB helpers: %d readings, blocks %q", n, whats(v.Blocks))
	}
}

// TestIntegritySkipsManyPackages (Opus #315 H1): a change touching ten
// packages of about 60 KB of bodies each (2.4 MB of skip reading over the
// four scans) under a 1 MiB maxWork reaches no bound: the skip layer
// charges its own, never the run's shared build-constraint work.
func TestIntegritySkipsManyPackages(t *testing.T) {
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 20
	var body strings.Builder
	for i := range 30 {
		fmt.Fprintf(&body, "\nfunc helper%d(t *testing.T) {\n\tx := 0\n%s\t_ = x\n}\n", i, strings.Repeat("\tx++\n", 400))
	}
	files, change := map[string]string{}, map[string]string{}
	for i := range 10 {
		dir := fmt.Sprintf("p%d/", i)
		files[dir+"big_helpers_test.go"] = fmt.Sprintf("package p%d\n\nimport \"testing\"\n", i) + body.String()
		files[dir+"big_test.go"] = fmt.Sprintf("package p%d\n\nimport \"testing\"\n\nfunc TestBig(t *testing.T) {\n\thelper0(t)\n\tif 1+1 != 2 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n", i)
		change[dir+"big_helpers_test.go"] = files[dir+"big_helpers_test.go"] + "\n// x\n"
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, files)
		plain(t, src, "add", "--all")
	})
	if v := f.guarded(t, change); !v.Accepted() || len(v.Warnings) > 0 {
		t.Errorf("ten packages: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}

// TestIntegritySkipsCancel: the skip layer's reading checks the run's
// context as it goes, in the visitor, not only between bodies: a cancelled
// one stops the first body read.
func TestIntegritySkipsCancel(t *testing.T) {
	fset := token.NewFileSet()
	var b strings.Builder
	b.WriteString("package p\n\nimport \"testing\"\n\nfunc big(t *testing.T) {\n")
	b.WriteString(strings.Repeat("\t_ = 1\n", 10_000))
	b.WriteString("\tt.Skip()\n}\n")
	f, err := parser.ParseFile(fset, "p.go", b.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	pkg, _ := (&types.Config{Error: func(error) {}}).Check("p", fset, []*ast.File{f}, info)
	ctx, cancel := context.WithCancel(context.Background())
	s := &pkgScan{r: &run{ctx: ctx}, pkg: pkg, info: info, tvals: map[types.Object]string{}, aliases: map[types.Object]ast.Expr{}}
	d := f.Decls[1].(*ast.FuncDecl)
	if found, err := s.skipsDirectly(d); !found || err != nil {
		t.Fatalf("a live context: %v %v", found, err)
	}
	cancel()
	if found, err := s.skipsDirectly(d); found || err != context.Canceled {
		t.Errorf("a cancelled context: %v %v", found, err)
	}
}
