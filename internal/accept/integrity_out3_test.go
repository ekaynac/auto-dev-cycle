// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path"
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
		"tool/mx.go":   "package tool\n\nimport \"flag\"\n\nfunc x1() { flag.Parse() }\n\nfunc x2() { _ = w.Parse }\n",
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
		"r3 #7 a method-value initializer":      {edit("tool/mv.go", "= safe.Parse", "= fatal.Parse"), "tool/mv.go changed bodies that use flag (1: var parse4)", ""},
		"an S method through a value":           {edit("tool/val.go", "func v() {}", "func v() { _ = w.Parse(nil) }"), "tool/val.go references into flag added, removed, moved or changed (1: v)", ""},
		"an S type argument":                    {edit("tool/targ.go", "func h() {}", "func h() { g[fsAlias]() }"), "tool/targ.go references into flag added, removed, moved or changed (1: h)", ""},
		"L3 a pointer type argument":            {edit("tool/targ.go", "func h2() {}", "func h2() { g[*pa]() }"), "tool/targ.go references into flag added, removed, moved or changed (1: h2)", ""},
		"M1 an arm64-only file":                 {edit("arm/x_arm64.go", "func v() {}", "func v() { _ = w.Parse(nil) }"), "arm/x_arm64.go references into flag added, removed, moved or changed (1: v)", ""},
		"M2 export_test.go's value":             {edit("xt/a_test.go", "func helper() {}", "func helper() { _ = xt.FS.Parse(nil) }"), "xt/a_test.go references into flag added, removed, moved or changed (1: helper)", ""},
		"L1 an os read by types (Opus #282 M2)": {edit("osr/r.go", "b := make([]byte, 1)\n\tn, _ := F.Read", "b := make([]byte, 2)\n\tn, _ := F.Read"), "", "osr/r.go changed bodies that use os"},
		"L1 an os write by types still warns":   {edit("osr/r.go", "b := make([]byte, 1)\n\tn, _ := F.Write", "b := make([]byte, 2)\n\tn, _ := F.Write"), "osr/r.go changed bodies that use os (1: func wr)", ""},
		"Astra H2 one coherent graph":           {edit("p/use.go", "Parse(nil)", "Parse([]string{\"-h\"})"), "p/use.go changed bodies that use flag (1: func f)", "incomplete"},
		"one graph across directories": {map[string]string{"a2/a_test.go": strings.Replace(base["a2/a_test.go"], "func helper() {}", "func helper() { _ = fs.Parse(nil) }", 1),
			"zr/r.go": strings.Replace(base["zr/r.go"], "func g() {}", "func g() { _ = fs.Parse(nil) }", 1)}, "zr/r.go references into flag added, removed, moved or changed (1: g)", "could not finish"},
		"Astra H3 each platform its own": {edit("c3/c.go", "Parse(nil)", "Parse([]string{\"-h\"})"), "c3/c.go changed bodies that use flag (1: func f)", "incomplete"},
	} {
		v := outGuarded(t, f, c.files)
		if c.warn != "" && !finds(v.Warnings, c.warn) || c.never != "" && finds(v.Warnings, c.never) || stopped(v) {
			t.Errorf("%s: blocks %q, warnings %q", name, whats(v.Blocks), whats(v.Warnings))
		}
	}
	// Astra #282 H1: the types check charges its own budget alone: the same
	// change with it on and off (off: main's layer), with method values, type
	// arguments, an arm64-only file, an external test and a build
	// constraint, leaves the text layer's counters (outDone) as main's.
	// Astra #282 r3 H1: typed signals are Typed entries apart: mx.go's text
	// body (x1: the text read's use) is main's, x2's is a typed entry.
	all, counters := map[string]string{"tool/tag.go": "//go:build !windows\n\npackage tool\n\nimport \"flag\"\n\nfunc tg() { flag.Parse() }\n"}, []string{}
	for _, files := range []map[string]string{edit("tool/mv.go", "= safe.Parse", "= fatal.Parse"), edit("tool/targ.go", "func h() {}", "func h() { g[fsAlias]() }"),
		edit("tool/val.go", "func v() {}", "func v() { _ = w.Parse(nil) }"), edit("arm/x_arm64.go", "func v() {}", "func v() { _ = w.Parse(nil) }"),
		edit("xt/a_test.go", "func helper() {}", "func helper() { _ = xt.FS.Parse(nil) }")} {
		maps.Copy(all, files)
	}
	all["tool/mx.go"] = strings.Replace(strings.Replace(base["tool/mx.go"], "flag.Parse() }", "flag.Parse(); _ = 1 }", 1), "w.Parse }", "w.Parse; _ = 2 }", 1)
	outDone = func(nodes, tags int, over bool) { counters = append(counters, fmt.Sprint(nodes, tags, over)) }
	on := f.guarded(t, all)
	outTypesHook = false
	off := f.guarded(t, all)
	outTypesHook, outDone = true, nil
	typedAs := func(v Verdict, what string, typed bool) bool {
		return slices.ContainsFunc(v.Warnings, func(b Block) bool { return strings.Contains(b.Path+" "+b.What, what) && b.Typed == typed })
	}
	if len(counters) != 2 || counters[0] != counters[1] || strings.HasSuffix(counters[0], "true") || !typedAs(on, "tool/mv.go changed bodies that use flag (1: var parse4)", true) ||
		!typedAs(on, "tool/targ.go references into flag added, removed, moved or changed (1: h)", true) || !typedAs(on, "tool/val.go references into flag added, removed, moved or changed (1: v)", true) ||
		!typedAs(on, "tool/mx.go changed bodies that use flag (1: func x1)", false) || !typedAs(on, "tool/mx.go changed bodies that use flag (1: func x2)", true) ||
		!typedAs(on, outRefs("tool/tag.go", "flag", "1: tg"), false) {
		t.Errorf("the text layer's counters, types on and off: %q; warnings %q", counters, whats(on.Warnings))
	}
	outSame(t, on, off)
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
	unit := f.ordinary(t, unitGo)
	for _, bad := range []string{"bad1", "bad2"} {
		begin := time.Now()
		v := outGuarded(t, f, map[string]string{"use/u.go": "package use\n\nimport (\n\t\"flag\"\n\n\t\"example.com/m/" + bad + "\"\n)\n\nvar _ = " + bad + ".X\n\nfunc f() { flag.Parse() }\n"})
		if took := time.Since(begin); !finds(v.Warnings, "use decision A's check by types could not finish") || stopped(v) || finds(v.Warnings, "check by types stopped") || outWorkers.Load() != 0 || !within(t, took, unit, 40) {
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
// worker slot (waited for within a test-shortened outSlotWait); a dropped
// worker still running stops the types check at once; its stop warning
// feeds no libc block; past the export bound, an incomplete warning. A
// dropped worker's lifecycle is TestOutCheckHandOff's.
func TestOutTypedBounded(t *testing.T) {
	defer func(w time.Duration) { outSlotWait = w }(outSlotWait)
	outSlotWait = 100 * time.Millisecond
	f, _ := libcFixture(t)
	change := map[string]string{"tool/x.go": "package tool\n\nimport \"flag\"\n\nfunc v() {\n\tw := flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\t_ = w.Parse(nil)\n}\n"}
	typesStopped := func(v Verdict) bool { return finds(v.Warnings, "decision A's check by types stopped") }
	// Opus #282 M1 (probe B): a live peer holds the slot: the types check
	// stops within outSlotWait of the run's start, not at its deadline.
	unit := f.ordinary(t, unitGo)
	outWorkers.Add(1)
	begin := time.Now()
	v := outGuarded(t, f, change)
	took := time.Since(begin)
	outWorkers.Add(-1)
	if !v.Accepted() || !typesStopped(v) || finds(v.Warnings, "stopped at its work bound") || !within(t, took-outSlotWait, unit, 20) {
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

// gate is an importer that holds each check in its import until the row
// releases it (out closed); in tells the row a check is held.
type gate struct{ in, out chan struct{} }

func newGate() gate { return gate{make(chan struct{}, 1), make(chan struct{})} }

func (g gate) Import(p string) (*types.Package, error) {
	select {
	case g.in <- struct{}{}:
	default:
	}
	<-g.out
	return types.NewPackage(p, path.Base(p)), nil
}

// TestOutCheckHandOff (Astra #282 M2 and L3): a worker's end and its drop,
// under the row's control (a gate importer, outCheckHook; no sleeps): one
// decision a worker, its slot freed before its result is published,
// outDropped never below zero, and both counts 0 once both sides are done.
func TestOutCheckHandOff(t *testing.T) {
	waitWorkers(t)
	defer func() { outCheckHook, maxOutCheck = nil, 0 }()
	check := func(ctx context.Context, start time.Time, im types.Importer) bool {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "p.go", "package p\n\nimport _ \"flag\"\n", 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _, ok := outCheck(ctx, start, time.Now().Add(time.Hour), im, "p", fset, []*ast.File{f}, &types.Info{})
		return ok
	}
	ended := make(chan int32, 4) // outDropped at a worker's very end
	// M2: a completed worker whose tail is held after its result: its
	// caller's next check, in a run past outSlotWait (no wait for a slot),
	// finds the slot free.
	free, tail := newGate(), make(chan struct{})
	var n atomic.Int32
	close(free.out)
	outCheckHook = func(e string) {
		if e == "ended" && n.Add(1) == 1 {
			<-tail
		}
		if e == "ended" {
			ended <- outDropped.Load()
		}
	}
	long := time.Now().Add(-time.Hour)
	first := check(context.Background(), long, free)
	second := check(context.Background(), long, free)
	close(tail)
	for range 1 + b2i(second) {
		<-ended
	}
	if !first || !second || outWorkers.Load() != 0 || outDropped.Load() != 0 {
		t.Errorf("a held tail: checks %v %v, %d workers, %d dropped", first, second, outWorkers.Load(), outDropped.Load())
	}
	// M2: the drop races the worker's end, the worker first (released in
	// the drop's hook): the caller's CAS fails, it takes the published
	// result, and outDropped is 0 throughout.
	g, atEnd := newGate(), int32(-9)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-g.in; cancel() }()
	outCheckHook = func(e string) {
		if e == "ended" {
			ended <- outDropped.Load()
		} else {
			close(g.out)
			atEnd = <-ended
		}
	}
	if ok := check(ctx, time.Now(), g); !ok || atEnd != 0 || outWorkers.Load() != 0 || outDropped.Load() != 0 {
		t.Errorf("the worker first: check %v, %d at its end, %d workers, %d dropped", ok, atEnd, outWorkers.Load(), outDropped.Load())
	}
	// L3: a dropped worker (maxOutCheck): held in its import until the row
	// releases it, it keeps its slot and is counted (the immediate-stop gate
	// sees it), abandoned untouched; at its real end both counts are 0.
	g = newGate()
	outCheckHook = func(e string) {
		if e == "ended" {
			ended <- outDropped.Load()
		}
	}
	maxOutCheck = 20 * time.Millisecond
	ok := check(context.Background(), time.Now(), g)
	maxOutCheck = 0
	if ok || outWorkers.Load() != 1 || outDropped.Load() != 1 || abandoned.Load() != 0 {
		t.Errorf("dropped: check %v, %d workers, %d dropped, %d abandoned", ok, outWorkers.Load(), outDropped.Load(), abandoned.Load())
	}
	close(g.out)
	if d := <-ended; d != 0 || outWorkers.Load() != 0 || outDropped.Load() != 0 {
		t.Errorf("dropped, then ended: %d at its end, %d workers, %d dropped", d, outWorkers.Load(), outDropped.Load())
	}
	// M2: the drop and the worker's end race, released together, 100 times:
	// outDropped is 0 or 1 after the drop, never -1, and 0 at the worker's
	// end, as outWorkers.
	for i := range 100 {
		g := newGate()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-g.in; cancel() }()
		outCheckHook = func(e string) {
			if e == "ended" {
				ended <- outDropped.Load()
			} else {
				close(g.out)
			}
		}
		check(ctx, time.Now(), g)
		d := outDropped.Load()
		if e := <-ended; d < 0 || d > 1 || e != 0 || outWorkers.Load() != 0 || outDropped.Load() != 0 {
			t.Fatalf("race %d: %d after the drop, %d at its end, %d workers, %d dropped", i, d, e, outWorkers.Load(), outDropped.Load())
		}
	}
}

// TestOutTypesApart (Astra #282 H1): the types check's work has its own
// budget (maxOutTypeWork) and stops through typesOff; the text layer's
// counter keeps main's charges. Astra's input, scaled by test-only bounds:
// a padded file importing flag in four directories before tested pkg, each
// directory touched, a text signal in tool, and pkg's libc-importing file
// changed only from return 1 to return 2 (or not). With the types check on,
// a text bound just above the text layer's work with it off (main's layer)
// and a types bound under the padding: the same text counters, the same
// blocks (none: as on main), and the same warnings plus the types stop
// alone, before pkg.
func TestOutTypesApart(t *testing.T) {
	tl := "package pkg\n\nimport \"modernc.org/libc\"\n\nvar _ = libc.NewTLS\n\nfunc k() int { return 1 }\n"
	pad := "package pad\n\nimport _ \"flag\"\n\n/* " + strings.Repeat("x", 16<<10) + " */\n"
	base, change := map[string]string{"pkg/tl.go": tl}, map[string]string{"tool/q.go": "package tool\n\nimport \"flag\"\n\nfunc q() { flag.Parse() }\n"}
	for i := range 4 {
		base[fmt.Sprintf("a%03d/pad.go", i)], change[fmt.Sprintf("a%03d/x.go", i)] = pad, "package pad\n"
	}
	lf, _ := libcFixture(t, base)
	type counters struct {
		nodes, tags int
		over        bool
	}
	var got counters
	oldNodes, oldTypes := maxOutNodes, maxOutTypeWork
	outDone = func(nodes, tags int, over bool) { got = counters{nodes, tags, over} }
	defer func() { outDone, outTypesHook, maxOutNodes, maxOutTypeWork = nil, true, oldNodes, oldTypes }()
	for _, libcEdit := range []bool{true, false} {
		files := maps.Clone(change)
		if libcEdit {
			files["pkg/tl.go"] = strings.Replace(tl, "return 1", "return 2", 1)
		}
		outTypesHook = false
		off := lf.guarded(t, files)
		text := got
		maxOutNodes, maxOutTypeWork, outTypesHook = text.nodes+len(pad)/2, 5*len(pad)/2, true // a typed file's bytes would pass the text bound; the types check stops in a000's second version
		on := lf.guarded(t, files)
		maxOutNodes, maxOutTypeWork = oldNodes, oldTypes
		added := slices.DeleteFunc(whats(on.Warnings), func(w string) bool { return slices.Contains(whats(off.Warnings), w) })
		if got != text || text.over || !on.Accepted() || !slices.Equal(whats(on.Blocks), whats(off.Blocks)) || len(on.Warnings) != len(off.Warnings)+1 ||
			len(added) != 1 || !strings.HasPrefix(added[0], "a000: decision A's check by types stopped") || !finds(off.Warnings, outRefs("tool/q.go", "flag", "1: q")) {
			t.Errorf("libc edit %v: text counters %v, off %v; blocks %q; warnings %q, off %q", libcEdit, got, text, whats(on.Blocks), whats(on.Warnings), whats(off.Warnings))
		}
		outSame(t, on, off)
	}
}

// TestOutReportPriority (Astra #282 r2 H1): typed-only warnings never take
// a text-level warning's place in the report. Astra's input, scaled by a
// test-only report cap of 20 entries (main's 1,000): tool's state.go holds
// w, a *flag.FlagSet; 20 files change fNNN() {} to fNNN() { _ = w.Parse(nil) }
// (a typed-only warning each), and z.go adds flag.Parse(). With them, a
// mixed reference warning (y.go: a text and a typed key) and a text body
// whose function's references change by types alone (v.go). The report
// starts with main's report (the types check off: main's layer), the same
// entries in the same order; typed ones follow.
func TestOutReportPriority(t *testing.T) {
	defer func() { reported = maxReported }()
	reported = 20
	base := map[string]string{
		"tool/state.go": "package tool\n\nimport \"flag\"\n\nvar w *flag.FlagSet\n",
		"tool/v.go":     "package tool\n\nimport \"flag\"\n\nfunc v() { flag.Parse() }\n",
		"tool/y.go":     "package tool\n\nimport \"flag\"\n\nfunc y() { _ = flag.CommandLine }\n",
		"tool/z.go":     "package tool\n\nimport \"flag\"\n\nfunc z() { _ = flag.CommandLine }\n",
	}
	change := map[string]string{
		"tool/v.go": strings.Replace(base["tool/v.go"], "flag.Parse() }", "flag.Parse(); _ = w.Parse(nil) }", 1),
		"tool/y.go": strings.Replace(base["tool/y.go"], "flag.CommandLine }", "flag.CommandLine; flag.Parse(); _ = w.Parse(nil) }", 1),
		"tool/z.go": strings.Replace(base["tool/z.go"], "flag.CommandLine }", "flag.CommandLine; flag.Parse() }", 1),
	}
	for i := range reported {
		base[fmt.Sprintf("tool/a%03d.go", i)] = fmt.Sprintf("package tool\n\nfunc f%03d() {}\n", i)
		change[fmt.Sprintf("tool/a%03d.go", i)] = fmt.Sprintf("package tool\n\nfunc f%03d() { _ = w.Parse(nil) }\n", i)
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	on := f.guarded(t, change)
	outTypesHook = false
	off := f.guarded(t, change)
	outTypesHook = true
	outSame(t, on, off)
	b, err := off.Report()
	if err != nil || !strings.Contains(string(b), `"truncated":0`) || len(on.Warnings) <= reported || !finds(off.Warnings, outRefs("tool/z.go", "flag", "1: z")) ||
		!finds(off.Warnings, outRefs("tool/y.go", "flag", "1: y")) || !finds(off.Warnings, "tool/v.go changed bodies that use flag (1: func v)") {
		t.Errorf("main's warnings %q, %d warnings with the types check", whats(off.Warnings), len(on.Warnings))
	}
}

// TestOutTypedEntries (Astra #282 r3 H1): typed signals are Typed entries
// apart; main's entries never change. Astra's three inputs, in tool with
// w a *flag.FlagSet: mixed.go (a, b and c call w.Parse, z adds flag.Parse);
// body.go (each of ba, bb, bc calling w.Parse and bz calling flag.Parse
// gets _ = 1); a byte-budget case scaled by test-only report bounds (long
// paths and names). And both preview limits (wide.go: four typed and four
// text functions).
func TestOutTypedEntries(t *testing.T) {
	long := "tool/" + strings.Repeat("p", 200)
	base := map[string]string{
		"tool/state.go": "package tool\n\nimport \"flag\"\n\nvar w *flag.FlagSet\n",
		"tool/mixed.go": "package tool\n\nimport \"flag\"\n\nfunc a() {}\n\nfunc b() {}\n\nfunc c() {}\n\nfunc z() { _ = flag.CommandLine }\n",
		"tool/body.go":  "package tool\n\nimport \"flag\"\n\nfunc ba() { _ = w.Parse(nil) }\n\nfunc bb() { _ = w.Parse(nil) }\n\nfunc bc() { _ = w.Parse(nil) }\n\nfunc bz() { flag.Parse() }\n",
		"tool/wide.go":  "package tool\n\nimport \"flag\"\n\nvar _ = flag.CommandLine\n",
		long + "/s.go":  "package long\n\nimport \"flag\"\n\nvar w *flag.FlagSet\n",
	}
	change := map[string]string{
		"tool/mixed.go": strings.NewReplacer("{}", "{ _ = w.Parse(nil) }", "CommandLine }", "CommandLine; flag.Parse() }").Replace(base["tool/mixed.go"]),
		"tool/body.go":  strings.ReplaceAll(base["tool/body.go"], ") }", "); _ = 1 }"),
		"tool/wide.go":  base["tool/wide.go"],
	}
	for _, fn := range []string{"wa", "wb", "wc", "wd", "wz1", "wz2", "wz3", "wz4"} {
		base["tool/wide.go"] += "\nfunc " + fn + "() {}\n"
		change["tool/wide.go"] += "\nfunc " + fn + "() { " + map[bool]string{true: "flag.Parse()", false: "_ = w.Parse(nil)"}[strings.HasPrefix(fn, "wz")] + " }\n"
	}
	for i := range 10 {
		fns := []string{fmt.Sprintf("z%03d", i), fmt.Sprintf("%s%03da", strings.Repeat("t", 60), i), fmt.Sprintf("%s%03db", strings.Repeat("t", 60), i), fmt.Sprintf("%s%03dc", strings.Repeat("t", 60), i)}
		src := "package long\n\nimport \"flag\"\n\nvar _ = flag.CommandLine\n"
		base[fmt.Sprintf("%s/f%03d.go", long, i)] = src + "\nfunc " + strings.Join(fns, "() {}\n\nfunc ") + "() {}\n"
		change[fmt.Sprintf("%s/f%03d.go", long, i)] = src + "\nfunc " + fns[0] + "() { flag.Parse() }\n\nfunc " + strings.Join(fns[1:], "() { _ = w.Parse(nil) }\n\nfunc ") + "() { _ = w.Parse(nil) }\n"
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	on := f.guarded(t, change)
	outTypesHook = false
	off := f.guarded(t, change)
	outTypesHook = true
	outSame(t, on, off)
	is := func(what string, typed bool) bool {
		return slices.ContainsFunc(on.Warnings, func(b Block) bool { return b.Path+" "+b.What == what+": review it" && b.Typed == typed })
	}
	if !is(outRefs("tool/mixed.go", "flag", "1: z"), false) || !is(outRefs("tool/mixed.go", "flag", "3: a, b, c"), true) ||
		!is("tool/body.go changed bodies that use flag (1: func bz)", false) || !is("tool/body.go changed bodies that use flag (3: func ba, func bb, func bc)", true) ||
		!is(outRefs("tool/wide.go", "flag", "4: wz1, wz2, wz3, …"), false) || !is(outRefs("tool/wide.go", "flag", "4: wa, wb, wc, …"), true) ||
		!is(outRefs(long+"/f009.go", "flag", "1: z009"), false) || !is(outRefs(long+"/f009.go", "flag", "3: "+strings.Repeat("t", 60)+"009a, "+strings.Repeat("t", 60)+"009b, "+strings.Repeat("t", 60)+"009c"), true) {
		t.Errorf("warnings %q", whats(on.Warnings))
	}
	// The byte boundary: a report budget main's entries just fill; the typed
	// entries are left out, main's all kept, byte for byte.
	defer func() { reportedBytes = maxEntries }()
	b, err := off.Report()
	var r struct{ Blocks, Warnings []json.RawMessage }
	if err != nil || json.Unmarshal(b, &r) != nil {
		t.Fatal(err)
	}
	reportedBytes = 8
	for _, e := range append(r.Blocks, r.Warnings...) {
		reportedBytes += len(e) + 1
	}
	outSame(t, on, off)
	if b, err := on.Report(); err != nil || !strings.Contains(string(b), fmt.Sprintf(`"truncated":%d,`, len(on.Warnings)-len(off.Warnings))) {
		t.Errorf("the byte boundary: %d typed entries, not all left out", len(on.Warnings)-len(off.Warnings))
	}
	// Below it (Astra #282 r4 L2): main's report already leaves text entries
	// out (its last one; half of them); with the types check, the same
	// report, then typed candidates alone in what is left.
	full := reportedBytes
	for _, bound := range []int{full - 8 - len(r.Warnings[len(r.Warnings)-1]), full / 2} {
		reportedBytes = bound
		outSame(t, on, off)
		if b, err := off.Report(); err != nil || strings.Contains(string(b), `"truncated":0,`) {
			t.Errorf("below the boundary (%d bytes): main's report leaves no text entry out", bound)
		}
	}
}
