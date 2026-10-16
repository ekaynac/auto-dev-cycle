// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/ast"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// aliasChain is n nested struct aliases of name (P0 = struct{ v int }, Pi =
// struct{ a, b P(i-1) }), whose identity with another such chain costs 2^n.
func aliasChain(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type %s0 = struct{ v int }\n\n", name)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "type %s%d = struct{ a, b %s%d }\n\n", name, i, name, i-1)
	}
	return b.String()
}

// waitWorkers waits until no check of the layer's runs (Opus #282 L1: a
// stuck one fails here).
func waitWorkers(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(time.Minute); outWorkers.Load() > 0 || outDropped.Load() > 0; {
		if !time.Now().Before(end) {
			t.Fatalf("a worker still runs: %d workers, %d dropped", outWorkers.Load(), outDropped.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDecisionAOut3 (P14b-out-3): S by real types. Each row is silent on
// main (a03c3af) or, for the bounds, stalls or blocks there in #280's
// reviews; each runs with the layer on and off (outGuarded).
func TestDecisionAOut3(t *testing.T) {
	probe := "\tfs := flag.NewFlagSet(\"x\", flag.ExitOnError)\n\t_ = fs.Parse(nil)\n"
	base := map[string]string{
		// r3 #7, values and type arguments.
		"tool/mv.go":   "package tool\n\nimport \"flag\"\n\nvar safe = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse4 = safe.Parse\n",
		"tool/val.go":  "package tool\n\nimport \"flag\"\n\nvar w = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc v() {}\n",
		"tool/targ.go": "package tool\n\nimport \"flag\"\n\ntype fsAlias = flag.FlagSet\n\ntype pa = flag.FlagSet\n\nfunc g[T any]() {}\n\nfunc h() {}\n\nfunc h2() {}\n",
		// Opus #280 H1: each platform's files alone are cheap; their union costs 2^28.
		"slow/a.go":       "package slow\n\n" + aliasChain("A", 28),
		"slow/b_amd64.go": "package slow\n\n" + aliasChain("B", 28) + "type Q = B28\n",
		// Opus #280 H2: only/'s amd64 file costs 2^30; the base guard loads only/ on arm64 alone.
		"only/a.go":       "package only\n\nvar X = 1\n\n" + aliasChain("A", 30) + aliasChain("B", 30),
		"only/s_amd64.go": "package only\n\nvar _ = A30(B30{})\n",
		// Opus #280 M1, M2, M4 and L1.
		"arm/x_arm64.go":    "package arm\n\nimport \"flag\"\n\nvar w = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc v() {}\n",
		"xt/x.go":           "package xt\n\nimport \"flag\"\n\nvar fs = flag.NewFlagSet(\"x\", flag.ContinueOnError)\n",
		"xt/export_test.go": "package xt\n\nvar FS = fs\n",
		"xt/a_test.go":      "package xt_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/xt\"\n)\n\nfunc helper() {}\n\nfunc TestX(t *testing.T) { helper(); _ = xt.FS }\n",
		"iv/iv.go":          "package iv\n\nimport \"flag\"\n\nvar safe2 = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal2 = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse5 func([]string) error\n\nfunc init() { parse5 = safe2.Parse }\n",
		"iv2/iv.go":         "package iv2\n\nimport \"flag\"\n\nvar safe3, fatal3 = flag.NewFlagSet(\"s\", flag.ContinueOnError), flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse6 func([]string) error\n",
		"osr/f.go":          "package osr\n\nimport \"os\"\n\nvar F *os.File\n",
		"osr/r.go":          "package osr\n\nfunc rd() int {\n\tb := make([]byte, 1)\n\tn, _ := F.Read(b)\n\treturn n\n}\n\nfunc wr() int {\n\tb := make([]byte, 1)\n\tn, _ := F.Write(b)\n\treturn n\n}\n",
		// Opus #282 H1 (probe A): own syntax the base guard refused.
		"bad1/a.go": "package bad1\n\nvar X = 1\n\n" + aliasChain("A", 26) + aliasChain("B", 26) + "var _ = A26(B26{})\n",
		"bad1/z.go": "package other\n",
		"bad2/a.go": "package bad2\n\nvar X = 1\n\n" + aliasChain("A", 26) + aliasChain("B", 26) + "var _ = A26(B26{})\n",
		"bad2/z.go": "package bad2\n\nfunc (\n",
		// Astra #280 H2: one coherent graph (util's flag is the layer's flag).
		"util/u.go":  "package util\n\nimport \"flag\"\n\nvar FS *flag.FlagSet\n\nfunc Choose[T any](a, b T) T { return a }\n",
		"p/state.go": "package p\n\nimport \"flag\"\n\nvar FS *flag.FlagSet\n",
		"p/use.go":   "package p\n\nimport \"example.com/m/util\"\n\nfunc f() { _ = util.Choose(FS, util.FS).Parse(nil) }\n",
		// Astra #280 H3: util2.FS is flag.FlagSet on arm64 alone.
		"util2/type_amd64.go": "package util2\n\ntype FS struct{}\n\nfunc (*FS) Parse([]string) error { return nil }\n",
		"util2/type_arm64.go": "package util2\n\nimport \"flag\"\n\ntype FS = flag.FlagSet\n",
		"c3/c.go":             "package c3\n\nimport \"example.com/m/util2\"\n\nfunc f(fs *util2.FS) { _ = fs.Parse(nil) }\n",
		// One graph across directories: a2's external test imports q2, which
		// imports a2; then zr imports both (the replay's vocab and types).
		"a2/x.go":      "package a2\n\ntype T int\n",
		"q2/q.go":      "package q2\n\nimport \"example.com/m/a2\"\n\nvar V a2.T\n",
		"a2/a_test.go": "package a2_test\n\nimport (\n\t\"flag\"\n\t\"testing\"\n\n\t\"example.com/m/a2\"\n\t\"example.com/m/q2\"\n)\n\nvar fs = flag.NewFlagSet(\"x\", flag.ContinueOnError)\n\nvar _ a2.T = q2.V\n\nfunc helper() {}\n\nfunc TestX(t *testing.T) { helper() }\n",
		"zr/r.go":      "package zr\n\nimport (\n\t\"flag\"\n\n\t\"example.com/m/a2\"\n\t\"example.com/m/q2\"\n)\n\nvar _ a2.T = q2.V\n\nvar fs = flag.NewFlagSet(\"r\", flag.ContinueOnError)\n\nfunc g() {}\n",
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	stopped := func(v Verdict) bool { return finds(v.Warnings, "stopped at its work bound") }
	edit := func(file, from, to string) map[string]string {
		return map[string]string{file: strings.Replace(base[file], from, to, 1)}
	}
	for name, c := range map[string]struct {
		files       map[string]string
		warn, never string
	}{
		"r3 #7 a method-value initializer":       {edit("tool/mv.go", "= safe.Parse", "= fatal.Parse"), "tool/mv.go changed bodies that use flag (1: var parse4)", ""},
		"an S method through a value":            {edit("tool/val.go", "func v() {}", "func v() { _ = w.Parse(nil) }"), "tool/val.go references into flag added, removed, moved or changed (1: v)", ""},
		"an S type argument":                     {edit("tool/targ.go", "func h() {}", "func h() { g[fsAlias]() }"), "tool/targ.go references into flag added, removed, moved or changed (1: h)", ""},
		"L3 a pointer type argument":             {edit("tool/targ.go", "func h2() {}", "func h2() { g[*pa]() }"), "tool/targ.go references into flag added, removed, moved or changed (1: h2)", ""},
		"M1 an arm64-only file":                  {edit("arm/x_arm64.go", "func v() {}", "func v() { _ = w.Parse(nil) }"), "arm/x_arm64.go references into flag added, removed, moved or changed (1: v)", ""},
		"M2 export_test.go's value":              {edit("xt/a_test.go", "func helper() {}", "func helper() { _ = xt.FS.Parse(nil) }"), "xt/a_test.go references into flag added, removed, moved or changed (1: helper)", ""},
		"M4 a method value rebound in init":      {edit("iv/iv.go", "= safe2.Parse", "= fatal2.Parse"), "iv/iv.go changed bodies that use flag (1: init body)", ""},
		"an init added rebinding a method value": {map[string]string{"iv2/iv.go": base["iv2/iv.go"] + "\nfunc init() { parse6 = fatal3.Parse }\n"}, "iv2/iv.go changed bodies that use flag (1: init body)", ""},
		"L1 an os read by types (Opus #282 M2)":  {edit("osr/r.go", "b := make([]byte, 1)\n\tn, _ := F.Read", "b := make([]byte, 2)\n\tn, _ := F.Read"), "", "osr/r.go changed bodies that use os"},
		"L1 an os write by types still warns":    {edit("osr/r.go", "b := make([]byte, 1)\n\tn, _ := F.Write", "b := make([]byte, 2)\n\tn, _ := F.Write"), "osr/r.go changed bodies that use os (1: func wr)", ""},
		"Astra H2 one coherent graph":            {edit("p/use.go", "Parse(nil)", "Parse([]string{\"-h\"})"), "p/use.go changed bodies that use flag (1: func f)", "incomplete"},
		"one graph across directories": {map[string]string{"a2/a_test.go": strings.Replace(base["a2/a_test.go"], "func helper() {}", "func helper() { _ = fs.Parse(nil) }", 1),
			"zr/r.go": strings.Replace(base["zr/r.go"], "func g() {}", "func g() { _ = fs.Parse(nil) }", 1)}, "zr/r.go references into flag added, removed, moved or changed (1: g)", "could not finish"},
		"Astra H3 each platform its own": {edit("c3/c.go", "Parse(nil)", "Parse([]string{\"-h\"})"), "c3/c.go changed bodies that use flag (1: func f)", "incomplete"},
	} {
		v := outGuarded(t, f, c.files)
		if c.warn != "" && !finds(v.Warnings, c.warn) || c.never != "" && finds(v.Warnings, c.never) || stopped(v) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// Opus #280 H1: the union is never checked: no file set given to a
	// check holds both platforms' files (Opus #282 L1: by structure).
	var union bool
	outCheckSeen = func(files []*ast.File) { // b_amd64.go declares B0; g_arm64.go declares Q alone
		union = union || slices.ContainsFunc(files, func(f *ast.File) bool { return hasDecl(f, "B0") }) && slices.ContainsFunc(files, func(f *ast.File) bool { return hasDecl(f, "Q") && !hasDecl(f, "B0") })
	}
	v := outGuarded(t, f, map[string]string{"slow/g_arm64.go": "package slow\n\nimport _ \"flag\"\n\ntype Q = A28\n\nvar _ = A28(Q{})\n"})
	outCheckSeen = nil
	if !v.Accepted() || stopped(v) || union {
		t.Errorf("H1 the union: blocks %q, warnings %q, a union checked %v", whats(v.Blocks), whats(v.Warnings), union)
	}
	waitWorkers(t)
	// Opus #282 H1 (probe A): a package the base guard refused (a second
	// package name; a later parse error) is never checked by the layer: the
	// directory is incomplete, at once, and no worker is left.
	for _, bad := range []string{"bad1", "bad2"} {
		begin := time.Now()
		v := outGuarded(t, f, map[string]string{"use/u.go": "package use\n\nimport (\n\t\"flag\"\n\n\t\"example.com/m/" + bad + "\"\n)\n\nvar _ = " + bad + ".X\n\nfunc f() { flag.Parse() }\n"})
		if !finds(v.Warnings, "use decision A's check by types could not finish") || stopped(v) || finds(v.Warnings, "check by types stopped") || outWorkers.Load() != 0 || time.Since(begin) > time.Minute {
			t.Errorf("H1 %s: warnings %q, %d workers, %v", bad, whats(v.Warnings), outWorkers.Load(), time.Since(begin))
		}
	}
	_ = v
	// Opus #280 H2: run 1, then an ordinary run 2: both as the base guard's, nothing left running.
	if v := outGuarded(t, f, map[string]string{"zzz/z_arm64.go": "package zzz\n\nimport \"example.com/m/only\"\n\nvar _ = only.X\n"}); !v.Accepted() || stopped(v) || abandoned.Load() != 0 || outWorkers.Load() != 0 {
		t.Errorf("H2 run 1: blocks %q, warnings %q, %d abandoned, %d workers", whats(v.Blocks), whats(v.Warnings), abandoned.Load(), outWorkers.Load())
	}
	if v := outGuarded(t, f, map[string]string{"tool2/x.go": "package tool2\n\nfunc F() {}\n"}); !v.Accepted() || len(v.Blocks) > 0 {
		t.Errorf("H2 run 2: blocks %q", whats(v.Blocks))
	}
	// libc by types is a warning, never a block (the blocking decisions keep P14b-out-1's identity).
	tl := "package pkg\n\nimport \"modernc.org/libc\"\n\ntype tlsAlias = libc.TLS\n\nfunc g[T any]() {}\n\nfunc h() {}\n"
	lf, _ := libcFixture(t, map[string]string{"pkg/tl.go": tl})
	if v := outGuarded(t, lf, map[string]string{"pkg/tl.go": strings.Replace(tl, "func h() {}", "func h() { g[tlsAlias]() }", 1)}); !v.Accepted() ||
		!finds(v.Warnings, "pkg/tl.go references into modernc.org/libc added, removed, moved or changed (1: h)") {
		t.Errorf("a libc type argument: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// L3, r3 #6 by types: a 64 KiB name with 5,000 references through a value, under a node bound.
	oldNodes := maxOutNodes
	maxOutNodes = 8 << 20
	v = outGuarded(t, f, map[string]string{"tool/huge.go": "package tool\n\nfunc " + strings.Repeat("n", 64<<10) + "() {\n" + strings.Repeat("\t_ = w.Parse\n", 5000) + "}\n"})
	maxOutNodes = oldNodes
	if !finds(v.Warnings, "tool/huge.go references into flag added, removed, moved or changed (1: "+strings.Repeat("n", 64)) || stopped(v) {
		t.Errorf("r3 #6 by types: %q", whats(v.Warnings))
	}
	_ = probe
}

// hasDecl: f declares name at package level.
func hasDecl(f *ast.File, name string) bool {
	return slices.ContainsFunc(f.Decls, func(d ast.Decl) bool {
		g, ok := d.(*ast.GenDecl)
		return ok && slices.ContainsFunc(g.Specs, func(sp ast.Spec) bool { ts, ok := sp.(*ast.TypeSpec); return ok && ts.Name.Name == name })
	})
}

// TestOutTypedBounded: Astra #280 M5 and the bounds. A check needs a free
// worker slot; a dropped worker keeps its slot until it ends; past the
// deadline, the stop warning; past the export bound, an incomplete warning;
// the base guard's accounting (abandoned) is never touched.
func TestOutTypedBounded(t *testing.T) {
	f, _ := libcFixture(t)
	change := map[string]string{"tool/x.go": "package tool\n\nimport \"flag\"\n\nfunc v() {\n\tw := flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\t_ = w.Parse(nil)\n}\n"}
	typesStopped := func(v Verdict) bool { return finds(v.Warnings, "decision A's check by types stopped") }
	// Opus #282 M1 (probe B): a live peer holds the slot: the types check
	// stops within outSlotWait of the run's start, not at its deadline.
	outWorkers.Add(1)
	begin := time.Now()
	v := outGuarded(t, f, change)
	took := time.Since(begin)
	outWorkers.Add(-1)
	if !v.Accepted() || !typesStopped(v) || finds(v.Warnings, "stopped at its work bound") || took > time.Minute {
		t.Errorf("a held slot: blocks %q, warnings %q, %v", whats(v.Blocks), whats(v.Warnings), took)
	}
	// A dropped worker still running: the types check stops at once.
	outDropped.Add(1)
	v = outGuarded(t, f, change)
	outDropped.Add(-1)
	if !v.Accepted() || !typesStopped(v) {
		t.Errorf("a dropped worker: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
	// No worker starts past the deadline.
	if _, _, ok := outCheck(context.Background(), time.Now(), time.Now().Add(-time.Second), nil, "p", nil, nil, nil); ok || outWorkers.Load() != 0 {
		t.Errorf("past the deadline: started, %d workers", outWorkers.Load())
	}
	// Past the deadline (a 2^25 check, maxOutCheck 50 ms): the types check
	// stops; the dropped worker keeps its slot; abandoned is untouched.
	maxOutCheck = 50 * time.Millisecond
	v = f.guarded(t, map[string]string{"tool/c.go": "package tool\n\nimport _ \"flag\"\n\n" + aliasChain("A", 25) + aliasChain("B", 25) + "var _ = A25(B25{})\n"}) // read at once: the dropped worker still runs
	held, dropped := outWorkers.Load(), outDropped.Load()
	maxOutCheck = 0
	if !v.Accepted() || !typesStopped(v) || abandoned.Load() != 0 || held != 1 || dropped != 1 {
		t.Errorf("past the deadline: blocks %q, warnings %q, %d abandoned, %d workers, %d dropped", whats(v.Blocks), whats(v.Warnings), abandoned.Load(), held, dropped)
	}
	waitWorkers(t)
	// Opus #282 probe D (the lead's decision): the types check's stop never
	// feeds the libc exhaustion block: a later run that changes only a
	// libc-importing file of a tested package is accepted, as on main.
	tl := "package pkg\n\nimport \"modernc.org/libc\"\n\nvar _ = libc.NewTLS\n\nfunc k() int { return 1 }\n"
	lf, _ := libcFixture(t, map[string]string{"pkg/tl.go": tl})
	for name, hold := range map[string]*atomic.Int32{"held": &outWorkers, "dropped": &outDropped} {
		hold.Add(1)
		v := outGuarded(t, lf, map[string]string{"pkg/tl.go": strings.Replace(tl, "return 1", "return 2", 1)})
		hold.Add(-1)
		if !v.Accepted() || len(v.Blocks) > 0 || !typesStopped(v) {
			t.Errorf("probe D, a %s worker: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// The export bound (each typer's, Opus #282 L2): incomplete.
	old := maxOutExport
	maxOutExport = 1
	v = outGuarded(t, f, change)
	maxOutExport = old
	if !v.Accepted() || !finds(v.Warnings, "tool decision A's check by types could not finish (linux/amd64: flag: over the layer's export data bound)") {
		t.Errorf("the export bound: blocks %q, warnings %q", whats(v.Blocks), whats(v.Warnings))
	}
}
