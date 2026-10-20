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
		"a skipping helper called":           {in("\trequireDocker(t)\n"), skipped},
		"through a local":                    {in("\tf := requireDocker\n\tf(t)\n"), skipped},
		"through a package variable":         {in("\tpkgSkip(t)\n"), skipped},
		"a method called":                    {in("\th{}.require(t)\n"), skipped},
		"a method value":                     {in("\tg := h{}.require\n\tg(t)\n"), skipped},
		"SkipNow by reflection":              {map[string]string{"pkg/skips_test.go": strings.Replace(strings.Replace(skipsBase, plainTest, "\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n"+plainTest, 1), "\"encoding/xml\"", "\"encoding/xml\"\n\t\"reflect\"", 1)}, "unverifiable: reflection on a testing value"},
		"an asserting helper (control)":      {in("\tcheck(t)\n"), ""},
		"another file only (control)":        {map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n// x\n"}, ""},
		"skipRebound":                        {in("\tx := t\n\trequireDocker(x)\n\tx = nil\n\t_ = x\n"), skipped},
		"skipSlice":                          {in("\t[]func(*testing.T){requireDocker}[0](t)\n"), skipped},
		"skipField":                          {in("\tstruct{ f func(*testing.T) }{requireDocker}.f(t)\n"), skipped},
		"skipWith":                           {in("\twith(t, requireDocker)\n"), skipped},
		"skipIface":                          {in("\tvar r req = h{}\n\tr.require(t)\n"), skipped},
		"skipClosureVar":                     {in("\tskipIt(t)\n"), skipped},
		"skipRaceVariant":                    {in("\trequireX(t)\n"), skipped},
		"an alias of an alias (H2)":          {in("\tsecond(t)\n"), skipped},
		"an alias of a literal variable":     {in("\tlitAlias(t)\n"), skipped},
		"a local alias of an alias":          {in("\tg := second\n\tg(t)\n"), skipped},
		"an alias set in init":               {chainInit(skipsBase, plainTest), skipped},
		"a chain of 4 aliases":               {skipChain(4, skipsBase, plainTest), skipped},
		"a chain of 40 aliases":              {skipChain(40, skipsBase, plainTest), "unverifiable: skipping helpers past the guard's bound"},
		"a propagated use for a direct skip": {map[string]string{"pkg/skips_test.go": strings.Replace(skipsBase, "\t_ = requireDocker\n", "\tt.Skip(\"unavailable\")\n", 1)}, skipped}, // Astra #292 r2 1
		"another type's Skip, from t":        {in("\t_ = skipElement(t)\n"), ""},                                                                                                       // Astra #292 r2 4
		"another type's Skip":                {in("\t_ = skipDecoder(nil)\n"), ""},                                                                                                     // Astra #292 M7
		"a helper skipping through a copy":   {in("\trequireCopy(t)\n"), skipped},
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

// TestIntegritySkipsBound (Astra #292 r2 6): the skipping facts' reading
// stops at the work bound, checked after each charge and between helpers:
// twenty 20 KB helpers under a 128 KiB bound are read about seven times a
// scan, never twenty; the package is then unverifiable. Readings are
// counted, never timed.
func TestIntegritySkipsBound(t *testing.T) {
	f := newGuardFixture(t)
	old := maxWork
	t.Cleanup(func() { maxWork = old })
	maxWork = 1 << 17
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
