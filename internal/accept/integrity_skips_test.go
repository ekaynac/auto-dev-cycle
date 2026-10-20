// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"fmt"
	"strings"
	"testing"
)

// skipsBase is P14d-a's test file: skipping helpers (a function, a method,
// package variables holding one, a literal and an alias of each), a runner,
// an interface, and helpers that are no skipping helpers.
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

// TestIntegritySkips (P14d-a; Astra #255 H6, Opus #255 M5, #292's reviews,
// #226 entry 95's H2): a skip added through an existing helper, by any use,
// blocks; propagated skips never offset a direct one; a helper calling
// another type's Skip is none; aliases of package variables are followed,
// failing closed past the fixed point's bound.
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
		"through a package variable":          {in("\tpkgSkip(t)\n"), skipped},
		"a method called":                     {in("\th{}.require(t)\n"), skipped},
		"a method value":                      {in("\tg := h{}.require\n\tg(t)\n"), skipped},
		"SkipNow by reflection":               {map[string]string{"pkg/skips_test.go": strings.Replace(strings.Replace(skipsBase, plainTest, "\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n"+plainTest, 1), "\"encoding/xml\"", "\"encoding/xml\"\n\t\"reflect\"", 1)}, "unverifiable: reflection on a testing value"},
		"an asserting helper (control)":       {in("\tcheck(t)\n"), ""},
		"another file only (control)":         {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		"skipRebound":                         {in("\tx := t\n\trequireDocker(x)\n\tx = nil\n\t_ = x\n"), skipped},
		"skipSlice":                           {in("\t[]func(*testing.T){requireDocker}[0](t)\n"), skipped},
		"skipField":                           {in("\tstruct{ f func(*testing.T) }{requireDocker}.f(t)\n"), skipped},
		"skipWith":                            {in("\twith(t, requireDocker)\n"), skipped},
		"skipIface":                           {in("\tvar r req = h{}\n\tr.require(t)\n"), skipped},
		"skipClosureVar":                      {in("\tskipIt(t)\n"), skipped},
		"skipRaceVariant":                     {in("\trequireX(t)\n"), skipped},
		"an alias of an alias (H2)":           {in("\tsecond(t)\n"), skipped},
		"an alias of a literal variable":      {in("\tlitAlias(t)\n"), skipped},
		"a local alias of an alias":           {in("\tg := second\n\tg(t)\n"), skipped},
		"an alias set in init":                {chainInit(skipsBase, plainTest), skipped},
		"a chain of 4 aliases":                {skipChain(4, skipsBase, plainTest), skipped},
		"a chain of 40 aliases":               {skipChain(40, skipsBase, plainTest), skipped}, // a worklist: no depth limit (Opus #315 H1)
		"Skipf with an identifier (C1)":       {in("\tneedEnv(t)\n"), skipped},
		"Skip of an error (C1)":               {in("\tneedErr(t, nil)\n"), skipped},
		"a helper calling a helper (C1)":      {in("\tneedAll(t)\n"), skipped},
		"a skip, then another type's Skip":    {in("\tneedThenDecode(t, nil)\n"), skipped}, // never reset (C1)
		"a literal variable calling a helper": {in("\tneedVarLit(t)\n"), skipped},
		"an asserting helper made skipping":   {map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, "func check(t *testing.T) {\n", "func check(t *testing.T) {\n\trequireDocker(t)\n", 1)}, "TestChecked a test skipped"},
		"a slice in a package variable (H2)":  {in("\tneeds[0](t)\n"), skipped},
		"a map in a package variable (H2)":    {in("\tbyName[\"docker\"](t)\n"), skipped},
		"a struct in a package variable":      {in("\tcfg.need(t)\n"), skipped},
		"a field set in init (H2)":            {in("\tholder.f(t)\n"), skipped},
		"a held result, used (r2 M1)":         {in("\t_ = url()\n"), ""},
		"a held result, read (r2 M1)":         {in("\tif shared != nil {\n\t\tt.Log(shared.URL)\n\t}\n"), ""},
		"a held result, cleared (r2 M1)":      {in("\tdefer closeClient()\n"), ""},
		"a plain result of a helper argument": {in("\tif !registered {\n\t\tt.Fatal(\"r\")\n\t}\n"), ""},
		"a plain result of a helper slice":    {in("\tt.Log(count)\n"), ""},
		"a returned helper called":            {in("\tpick()(t)\n"), skipped},
		"a returned helper held":              {in("\tpicked(t)\n"), skipped},
		"a copy of a rebound copy":            {in("\trebound(t)\n"), ""},
		"a cycle of copies (L1)":              {in("\tneedCyc(t)\n"), skipped},
		"a longer cycle of copies (L1)":       {in("\tneedCyc2(t)\n"), skipped},
		"a propagated use for a direct skip":  {map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, "\t_ = requireDocker\n", "\tt.Skip(\"unavailable\")\n", 1)}, skipped}, // Astra #292 r2 1
		"another type's Skip, from t":         {in("\t_ = skipElement(t)\n"), ""},                                                                                                       // Astra #292 r2 4
		"another type's Skip":                 {in("\t_ = skipDecoder(nil)\n"), ""},                                                                                                     // Astra #292 M7
		"a helper skipping through a copy":    {in("\trequireCopy(t)\n"), skipped},
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

// skipChain is base with c0 = requireDocker, c1 = c0, … cn = c(n-1) in a
// new file, and cn(t) before TestPlain's assertion.
func skipChain(n int, base, plainTest string) map[string]string {
	var b strings.Builder
	b.WriteString("package pkg\n\nvar c0 = requireDocker\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "var c%d = c%d\n", i, i-1)
	}
	return map[string]string{"pkg/chain_test.go": b.String(), "pkg/skips_test.go": strings.Replace(base, plainTest, fmt.Sprintf("\tc%d(t)\n", n)+plainTest, 1)}
}

// chainInit is an alias set in init: var late func(*testing.T); late = second.
func chainInit(base, plainTest string) map[string]string {
	return map[string]string{"pkg/chain_test.go": "package pkg\n\nimport \"testing\"\n\nvar late func(*testing.T)\n\nfunc init() { late = second }\n",
		"pkg/skips_test.go": strings.Replace(base, plainTest, "\tlate(t)\n"+plainTest, 1)}
}

// TestIntegritySkipsBound (Astra #292 r2 6, Opus #315 H1): the skip
// layer reads each body once a scan, charged to its own bound and checked
// before each reading: twenty 20 KB helpers under a 128 KiB bound are read
// at most seven times in all (with the base's bodies, under 30 readings),
// never twenty a scan (80); the package is then
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
	b.WriteString("package pkg\n")
	for i := range 20 {
		fmt.Fprintf(&b, "\nfunc helper%d() {\n\tx := 0\n%s\t_ = x\n}\n", i, strings.Repeat("\tx++\n", 4000))
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
		fmt.Fprintf(&body, "\nfunc helper%d() {\n\tx := 0\n%s\t_ = x\n}\n", i, strings.Repeat("\tx++\n", 400))
	}
	files, change := map[string]string{}, map[string]string{}
	for i := range 10 {
		dir := fmt.Sprintf("p%d/", i)
		files[dir+"big.go"] = fmt.Sprintf("package p%d\n", i) + body.String()
		files[dir+"big_test.go"] = fmt.Sprintf("package p%d\n\nimport \"testing\"\n\nfunc TestBig(t *testing.T) {\n\thelper0()\n\tif 1+1 != 2 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n", i)
		change[dir+"big.go"] = files[dir+"big.go"] + "\n// x\n"
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

// charged is the skip layer's work (skipCharged) in one guarded run of
// files, which it must accept clean.
func charged(t *testing.T, f fixture, files map[string]string) int64 {
	t.Helper()
	before := skipCharged.Load()
	if v := f.guarded(t, files); !v.Accepted() || len(v.Warnings) > 0 {
		t.Fatalf("blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	return skipCharged.Load() - before
}

// linear: the skip layer's work for 4n is under 5 × n's (4 ×, and n's
// again as slack: longer names, fixed costs); a quadratic shape is about
// 16 times, by a count, never a clock (Opus #315 r2 L1). The counter sees
// only charged work: a loop that never calls charge passes these rows
// whatever its shape (Opus #315 r3 L1). That every loop over references,
// values and copies charges is review's to keep.
func linear(t *testing.T, small, large int64) {
	t.Helper()
	t.Logf("work: n %d, 4n %d", small, large)
	if large > 5*small {
		t.Errorf("4n's work %d against n's %d: not linear", large, small)
	}
}

// TestIntegritySkipsVarLinear (Opus #315 H3): a package variable assigned n
// distinct functions in init and called n times by a test, beside a
// skipping helper, costs linear work.
func TestIntegritySkipsVarLinear(t *testing.T) {
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/req_test.go": "package pkg\n\nimport \"testing\"\n\nfunc requireDocker(t *testing.T) { t.Skip(\"x\") }\n"})
		plain(t, src, "add", "--all")
	})
	file := func(n int) map[string]string {
		var b strings.Builder
		b.WriteString("package pkg\n\nimport \"testing\"\n\nvar v func(*testing.T)\n")
		for i := range n {
			fmt.Fprintf(&b, "\nfunc f%d(*testing.T) {}\n", i)
		}
		b.WriteString("\nfunc init() {\n")
		for i := range n {
			fmt.Fprintf(&b, "\tv = f%d\n", i)
		}
		b.WriteString("}\n\nfunc TestMany(t *testing.T) {\n")
		b.WriteString(strings.Repeat("\tv(t)\n", n))
		b.WriteString("\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n")
		return map[string]string{"pkg/many_test.go": b.String()}
	}
	linear(t, charged(t, f, file(2000)), charged(t, f, file(8000)))
}

// TestIntegritySkipsCopyLinear (Opus #315 r2 H1): a base helper copying t
// down a chain of n variables (u0 = nil at its head) and calling Skip on
// each, under a comment-only change, costs linear work: the copy search
// runs once a scan.
func TestIntegritySkipsCopyLinear(t *testing.T) {
	work := func(n int) int64 {
		var b strings.Builder
		b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc chain(t *testing.T) {\n\tu0 := t\n\tu0 = nil\n")
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "\tu%d := u%d\n", i, i-1)
		}
		for i := n; i >= 1; i-- {
			fmt.Fprintf(&b, "\tu%d.Skip()\n", i)
		}
		b.WriteString("}\n")
		f := newFixtureWith(t, func(src string) {
			write(t, src, guardBase)
			write(t, src, map[string]string{"pkg/chain_test.go": b.String()})
			plain(t, src, "add", "--all")
		})
		return charged(t, f, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"})
	}
	linear(t, work(2000), work(8000))
}

// TestIntegritySkipsCopyBound: the copy search stops at the skip layer's
// bound, checked at each step: a chain of 4000 copies (about 8000 to index,
// 8000 to search) under a 12000 bound charges no more than the bound and a
// step's overshoot, and the package is unverifiable.
func TestIntegritySkipsCopyBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nimport \"testing\"\n\nfunc chain(t *testing.T) {\n\tu0 := t\n\tu0 = nil\n")
	for i := 1; i <= 4000; i++ {
		fmt.Fprintf(&b, "\tu%d := u%d\n", i, i-1)
	}
	b.WriteString("\tu4000.Skip()\n}\n")
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"pkg/chain_test.go": b.String()})
		plain(t, src, "add", "--all")
	})
	old := maxSkipWork
	t.Cleanup(func() { maxSkipWork = old })
	maxSkipWork = 12000
	before := skipCharged.Load()
	v := f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"})
	n := skipCharged.Load() - before
	t.Logf("charged %d", n)
	if !finds(v.Blocks, "skipping helpers past the guard's bound") || n > 12000+64 {
		t.Errorf("charged %d, blocks %q", n, whats(v.Blocks))
	}
}
