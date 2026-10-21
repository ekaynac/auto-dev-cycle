// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
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

func inspect(t *testing.T) { _ = t.Skip }

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

// aliasesBase is the base's aliases and parameter types (Astra #350 H1,
// #354; #226 entry 115): Ptr[testing.T], Ptr[TT], a generic alias of a
// generic alias, a chain of 64, another package's alias (bridge.T), a type
// parameter with a Skip method; and, never testing, Wrap[Wrap[int]] and a
// defined type with a Skip method.
func aliasesBase() string {
	var b strings.Builder
	b.WriteString("package pkg\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/bridge\"\n)\n\ntype Ptr[P any] = *P\n\ntype Box[Q any] = Ptr[Q]\n\ntype A0 = testing.T\n")
	b.WriteString("\ntype BT = bridge.T\n\nfunc needBridge(t *BT) { t.Skip(\"x\") }\n\nfunc needTypeParam[T interface{ Skip(...any) }](t T) { t.Skip(\"x\") }\n")
	b.WriteString("\ntype Id[P any] = []P\n\ntype Wrap[Q any] = Id[Q]\n\nfunc ordinary(x Wrap[Wrap[int]]) { _ = x }\n")
	b.WriteString("\ntype res struct{}\n\nfunc (res) Skip() {}\n\nfunc useRes(r res) { r.Skip() }\n")
	for i := 1; i <= 64; i++ {
		fmt.Fprintf(&b, "\ntype A%d = A%d\n", i, i-1)
	}
	b.WriteString("\nfunc needGeneric(t Ptr[testing.T]) { t.Skip(\"x\") }\n\nfunc needGenericTT(t Ptr[TT]) { t.Skip(\"x\") }\n\nfunc needNested(t Box[testing.T]) { t.Skip(\"x\") }\n\nfunc needChain(t *A64) { t.Skip(\"x\") }\n")
	return b.String()
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
		write(t, src, map[string]string{"pkg/skips_test.go": skipsBase, "pkg/aliases_test.go": aliasesBase(), "bridge/bridge.go": "package bridge\n\nimport \"testing\"\n\ntype T = testing.T\n", "pkg/race_skip_test.go": raceSkip("race", ` t.Skip("race") `),
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
		"a generic alias (Astra H1)":          {in("\tneedGeneric(t)\n"), skipped}, // Astra #350 H1
		"a generic alias of an alias":         {in("\tneedGenericTT(t)\n"), skipped},
		"a generic alias, nested":             {in("\tneedNested(t)\n"), skipped},
		"a chain of 64 aliases (Astra H1)":    {in("\tneedChain(t)\n"), skipped},
		"another package's alias (#354 H1)":   {in("\tneedBridge(t)\n"), skipped}, // Astra #354 H1
		"a type parameter's Skip":             {in("\tneedTypeParam(t)\n"), skipped},
		"Wrap[Wrap[int]] (#354 M3)":           {in("\tordinary(nil)\n"), ""}, // Astra #354 M3: clean
		"a defined type's Skip":               {in("\tuseRes(res{})\n"), ""},
		"an alias cycle: unverifiable":        {map[string]string{"pkg/cycle_test.go": "package pkg\n\nimport \"testing\"\n\ntype C1 = C2\n\ntype C2 = C1\n\nfunc needCycle(t *C1) { _ = t }\n\nvar _ = testing.Short\n"}, "unverifiable"}, // the types layer's; the skip layer's classes a cycle possibly testing (TestIntegritySkipsAliases)
		// Astra #350 M2, stated (a false block): selecting a skip method
		// without calling it makes a helper skipping.
		"a selected, uncalled Skip (M2)": {in("\tinspect(t)\n"), skipped},
		"an alias of a pointer alias":    {in("\tneedPtrAlias(t)\n"), skipped},
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
		"an imported type's Skip, stated":     {in("\t_ = skipDecoder(nil)\n"), skipped},                                                                                                // #226 entry 116: a false block, stated (Astra #292 M7's input)
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
	s := &pkgScan{r: &run{ctx: ctx}, pkg: pkg, info: info, tvals: map[types.Object]string{}, aliases: map[types.Object]ast.Expr{}, aliasMemo: map[types.Object]int8{}}
	d := f.Decls[1].(*ast.FuncDecl)
	params := s.testingParams(d)
	if found, err := s.skipsDirectly(d, params); !found || err != nil {
		t.Fatalf("a live context: %v %v", found, err)
	}
	cancel()
	if found, err := s.skipsDirectly(d, params); found || err != context.Canceled {
		t.Errorf("a cancelled context: %v %v", found, err)
	}
}

// TestIntegritySkipsAliases (#226 entry 115): a parameter's type
// classified conservatively: testing, provably not, or possibly testing,
// never resolved exactly; an alias chain followed once a scan, a cycle
// possibly testing; past the bound, the package's reason.
func TestIntegritySkipsAliases(t *testing.T) {
	scan := func(src string) (*pkgScan, map[string]*ast.FuncDecl) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", "package p\n\nimport (\n\t\"encoding/xml\"\n\t\"testing\"\n\ttt \"testing\"\n\n\t\"example.com/x\"\n\t\"m/own\"\n)\n\nvar _, _, _, _ = xml.Decoder{}, testing.Short, tt.Short, x.Y\n\nvar _ = own.Z\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
		pkg, _ := (&types.Config{Error: func(error) {}, Importer: importer.Default()}).Check("p", fset, []*ast.File{f}, info) // type errors kept: the scan reads on
		s := &pkgScan{r: &run{ctx: context.Background()}, pkg: pkg, info: info,
			tvals: map[types.Object]string{}, aliases: map[types.Object]ast.Expr{}, aliasMemo: map[types.Object]int8{}}
		decls := map[string]*ast.FuncDecl{}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				decls[d.Name.Name] = d
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok && ts.Assign.IsValid() && info.Defs[ts.Name] != nil {
						s.aliases[info.Defs[ts.Name]] = ts.Type
					}
				}
			}
		}
		return s, decls
	}
	for name, c := range map[string]struct {
		src     string
		testing bool
	}{
		"*testing.T":                          {"func f(t *testing.T) {}\n", true},
		"testing.TB":                          {"func f(t testing.TB) {}\n", true},
		"(*testing.T)":                        {"func f(t (*testing.T)) {}\n", true},
		"a renamed import":                    {"func f(t *tt.B) {}\n", true},
		"testing.M":                           {"func f(m *testing.M) {}\n", false},
		"a builtin":                           {"func f(t int) {}\n", false},
		"a parenthesized builtin":             {"func f(t (int)) {}\n", false},
		"a pointer to a parenthesized type":   {"type D struct{}\n\nfunc f(t *(D)) {}\n", false},
		"a slice":                             {"func f(t []int) {}\n", false},
		"a map":                               {"func f(t map[int]int) {}\n", false},
		"a func":                              {"func f(t func()) {}\n", false},
		"a struct":                            {"func f(t struct{}) {}\n", false},
		"an interface":                        {"func f(t interface{ Skip() }) {}\n", false},
		"a defined type":                      {"type D struct{}\n\nfunc f(t *D) {}\n", false},
		"a defined generic type instantiated": {"type G[P any] struct{}\n\nfunc f(t G[int]) {}\n", false},
		"a standard-library type, stated":     {"func f(d *xml.Decoder) {}\n", true}, // #226 entry 116
		"another module's type":               {"func f(t *x.T) {}\n", true},
		"a dotless import's type":             {"func f(t *own.T) {}\n", true},
		"an alias of testing.T":               {"type TT = testing.T\n\nfunc f(t *TT) {}\n", true},
		"an alias of int":                     {"type I = int\n\nfunc f(t I) {}\n", false},
		"an alias of another module's type":   {"type B = x.T\n\nfunc f(t *B) {}\n", true},
		"a generic alias":                     {"type Ptr[P any] = *P\n\nfunc f(t Ptr[int]) {}\n", true},
		"a type parameter":                    {"func f[T any](t T) {}\n", true},
		"a cycle":                             {"type C1 = C2\n\ntype C2 = C1\n\nfunc f(t *C1) {}\n", true},
		"unresolved":                          {"func f(t *Nowhere) {}\n", true},
	} {
		s, decls := scan(c.src)
		if params := s.testingParams(decls["f"]); (len(params) > 0) != c.testing || s.skipBad != "" {
			t.Errorf("%s: %d testing parameters, skipBad %q", name, len(params), s.skipBad)
		}
	}
	old := maxSkipWork
	t.Cleanup(func() { maxSkipWork = old })
	maxSkipWork = 10 // the bound, mid-chain
	s, decls := scan("type A0 = testing.T\n\ntype A1 = A0\n\ntype A2 = A1\n\ntype A3 = A2\n\ntype A4 = A3\n\ntype A5 = A4\n\ntype A6 = A5\n\ntype A7 = A6\n\ntype A8 = A7\n\ntype A9 = A8\n\ntype A10 = A9\n\ntype A11 = A10\n\nfunc f(t *A11) {}\n")
	s.same, s.skipping = map[string][]*ast.FuncDecl{"f": {decls["f"]}}, map[*ast.FuncDecl]bool{}
	if err := s.skipFacts(); err != nil || s.skipBad != "skipping helpers past the guard's bound" {
		t.Errorf("a chain past the bound: %v, skipBad %q", err, s.skipBad)
	}
}

// TestIntegritySkipsBridge (Astra #354 r2 H1): a dotless import a
// versioned replacement supplies (require bridge; replace bridge =>
// example.com/testbridge), its T an alias of testing.T: the base's helper
// skipping a parameter of a local alias of it is skipping, and a test's
// new call blocks (whatever else the unloadable module blocks).
func TestIntegritySkipsBridge(t *testing.T) {
	goT := "package pkg\n\nimport \"testing\"\n\nfunc TestBridged(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{
			"go.mod":              strings.Replace(guardBase["go.mod"], "require ", "require bridge v1.0.0\n\nreplace bridge => example.com/testbridge v1.0.0\n\nrequire ", 1),
			"pkg/bridge_test.go":  "package pkg\n\nimport \"bridge\"\n\ntype TT = bridge.T\n\nfunc need(t *TT) { t.Skip(\"unavailable\") }\n",
			"pkg/bridged_test.go": goT,
		})
		plain(t, src, "add", "--all")
	})
	v := f.guarded(t, map[string]string{"pkg/bridged_test.go": strings.Replace(goT, "{\n\tif", "{\n\tneed(t)\n\tif", 1)})
	if !finds(v.Blocks, "a test skipped (a skipping helper used)") {
		t.Errorf("a versioned replacement's bridge: blocks %q", whats(v.Blocks))
	}
}

// TestIntegritySkipsNoModuleReads (Astra #354 r2 M3): the skip layer reads
// no go.mod: an unrelated module file under testdata/ is never read for a
// change to a package with an imported parameter type.
func TestIntegritySkipsNoModuleReads(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"testdata/m1/go.mod": "module example.com/unrelated\n", "pkg/ord_test.go": "package pkg\n\nimport \"encoding/xml\"\n\nfunc ordinary(d *xml.Decoder) { _ = d }\n"})
		plain(t, src, "add", "--all")
	})
	unrelated := strings.TrimSpace(plain(t, f.src, "rev-parse", "HEAD:testdata/m1/go.mod"))
	var read []string
	inner := integrity(f.repo, f.base, testLister(t))
	g := func(ctx context.Context, c []Change, accepts Reader) ([]Block, []Block, error) {
		return inner(ctx, c, func(ctx context.Context, blob string, max int64) ([]byte, bool, error) {
			read = append(read, blob)
			return accepts(ctx, blob, max)
		})
	}
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, map[string]string{"pkg/calc_test.go": baseFiles["pkg/calc_test.go"] + "\n// x\n"}), Policy: f.policy, Guard: g})
	if err != nil || !v.Accepted() || len(read) == 0 || slices.Contains(read, unrelated) {
		t.Errorf("an unrelated go.mod: read %v of %d blobs, blocks %q, %v", slices.Contains(read, unrelated), len(read), whats(v.Blocks), err)
	}
}

// TestIntegritySkipsWide (Astra #354 H2): n parameters of one generic
// alias's instantiation over a wide alias, and a chain of n aliases each
// used by a parameter, cost work linear in n (counted, never timed): 4n
// under 5 × n's.
func TestIntegritySkipsWide(t *testing.T) {
	f := newGuardFixture(t)
	work := func(n int) int64 {
		var b strings.Builder
		b.WriteString("package pkg\n\ntype Wide[")
		for i := range n {
			fmt.Fprintf(&b, "P%d, ", i)
		}
		b.WriteString("_ any] = *P0\n\ntype Small[Q any] = Wide[Q")
		for range n {
			b.WriteString(", int")
		}
		b.WriteString("]\n\ntype C0 = int\n")
		for i := range n {
			fmt.Fprintf(&b, "\ntype C%d = C%d\n\nfunc f%d(x Small[int], t C%d) { _, _ = x, t }\n", i+1, i, i, i+1)
		}
		before := skipCharged.Load()
		if v := f.guarded(t, map[string]string{"pkg/wide_test.go": b.String()}); !v.Accepted() || len(v.Warnings) > 0 {
			t.Fatalf("%d: blocks %q, warnings %q", n, whats(v.Blocks), whats(v.Warnings))
		}
		return skipCharged.Load() - before
	}
	small, large := work(1000), work(4000)
	t.Logf("work: n %d, 4n %d", small, large)
	if large > 5*small {
		t.Errorf("4n's work %d against n's %d: not linear", large, small)
	}
}
