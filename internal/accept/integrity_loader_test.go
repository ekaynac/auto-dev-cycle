// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// TestMain: the test binary is Integrity's child when asked (RunChildIfAsked).
func TestMain(m *testing.M) {
	RunChildIfAsked()
	os.Exit(m.Run())
}

// loaded accepts a patch of the base through Integrity with list: the
// guard in its own process.
func (f fixture) loaded(t *testing.T, list Lister, files map[string]string, gone ...string) Verdict {
	t.Helper()
	v, err := f.loadedErr(t, list, files, gone...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// loadedErr is loaded, its error returned.
func (f fixture) loadedErr(t *testing.T, list Lister, files map[string]string, gone ...string) (Verdict, error) {
	t.Helper()
	return Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, files, gone...), Policy: f.policy,
		Guard: Integrity(f.repo, f.base, list, "")})
}

// didNotRunErr reports whether err is ErrGuardRun saying why.
func didNotRunErr(err error, why string) bool {
	return errors.Is(err, ErrGuardRun) && strings.Contains(err.Error(), why)
}

// children records each run's child, and kills any left at the end.
func children(t *testing.T) func() []*os.Process {
	var mu sync.Mutex
	var procs []*os.Process
	procStarted = func(p *os.Process) { mu.Lock(); procs = append(procs, p); mu.Unlock() }
	t.Cleanup(func() {
		procStarted = nil
		for _, p := range procs {
			_ = p.Kill()
		}
	})
	return func() []*os.Process { mu.Lock(); defer mu.Unlock(); return slices.Clone(procs) }
}

// exited: p has exited (and been waited for).
func exited(p *os.Process) bool { return !alive(p.Pid) }

// TestIntegrityProcess (P14e-2c): the guard runs in a child the controller
// kills: its verdicts are the in-process guard's, a type check that never
// ends is killed at the whole-run deadline, an abandoned one ends with its
// process (the cap on abandoned checks is gone: no later run sees it), and
// every way the child fails blocks.
func TestIntegrityProcess(t *testing.T) {
	base := map[string]string{
		"tool/mv.go":  "package tool\n\nimport \"flag\"\n\nvar safe = flag.NewFlagSet(\"s\", flag.ContinueOnError)\n\nvar fatal = flag.NewFlagSet(\"f\", flag.ExitOnError)\n\nvar parse4 = safe.Parse\n",
		"tool/val.go": "package tool\n\nimport \"flag\"\n\nvar w = flag.NewFlagSet(\"w\", flag.ContinueOnError)\n\nfunc v() {}\n",
		"tool/mx.go":  "package tool\n\nimport \"flag\"\n\nfunc x1() { flag.Parse() }\n\nfunc x2() { _ = w.Parse }\n",
	}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, base)
		plain(t, src, "add", "--all")
	})
	procs := children(t)
	edit := func(file, from, to string) string { return strings.Replace(base[file], from, to, 1) }
	plainGo := map[string]string{"pkg/x.go": "package pkg\n\nfunc G() int { return 1 }\n"}
	// The same verdicts as in this process: blocks, warnings (Additive and
	// Typed too), in order; a reader's error is the guard's.
	for name, files := range map[string]map[string]string{
		"a plain change": plainGo,
		"a forgery":      {"pkg/forge.go": forgeGo},
		"typed and text warnings": {"tool/mv.go": edit("tool/mv.go", "= safe.Parse", "= fatal.Parse"), "tool/val.go": edit("tool/val.go", "func v() {}", "func v() { _ = w.Parse(nil) }"),
			"tool/mx.go": strings.Replace(edit("tool/mx.go", "flag.Parse() }", "flag.Parse(); _ = 1 }"), "w.Parse }", "w.Parse; _ = 2 }", 1)},
		"an unloaded import": {"pkg/x.go": "package pkg\n\nimport \"example.org/none\"\n\nvar _ = none.X\n"},
		// H1 (Opus #287): decision A's Python classes are the child's too.
		"python os._exit":         {"tests/test_money.py": baseFiles["tests/test_money.py"] + "\ndef test_exit():\n    import os\n    os._exit(0)\n"},
		"python module-level run": {"money.py": baseFiles["money.py"] + "\nimport subprocess\nsubprocess.run(['true'])\n"},
		"python print('PASSED')":  {"tests/test_money.py": baseFiles["tests/test_money.py"] + "\nprint('PASSED')\n"},
	} {
		in, out := f.guarded(t, files), f.loaded(t, testLister(t), files)
		if !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || len(out.Blocks)+len(out.Warnings) == 0 && name != "a plain change" ||
			strings.HasPrefix(name, "python") && !slices.ContainsFunc(out.Blocks, func(b Block) bool {
				return strings.Contains(b.What, "decision A") || strings.Contains(b.What, "allowlist")
			}) {
			t.Errorf("%s: in this process %+v %+v, in its own %+v %+v", name, in.Blocks, in.Warnings, out.Blocks, out.Warnings)
		}
	}
	if ps := procs(); len(ps) == 0 || !slices.ContainsFunc(ps, exited) {
		t.Fatalf("no child ran, or none ended: %d", len(ps))
	}
	// The result comes back whole (#226 entry 105): a Path past 4,096
	// bytes; more warnings than the report keeps (maxReported; Opus's probe
	// had 70,000 warnings, 142 s here, and any count past it takes the same
	// path); 1,001 warnings of 1,000-byte paths that exhaust the report's
	// bytes before its count (Astra #293 r2 #3), after a warning the verdict
	// holds before the guard's, and after an invalid UTF-8 one (Astra #311
	// #1). The same verdict, report and detail, byte for byte.
	var many strings.Builder
	many.WriteString("package tool8\n\nimport \"fmt\"\n")
	for i := range maxReported + 500 {
		fmt.Fprintf(&many, "\nfunc f%d() { fmt.Println() }\n", i)
	}
	var wide strings.Builder
	wide.WriteString("package tool7\n\nimport \"fmt\"\n")
	for i := range 1001 {
		fmt.Fprintf(&wide, "\nfunc f%s%04d() { fmt.Println() }\n", strings.Repeat("a", 966), i)
	}
	for name, c := range map[string]struct {
		files   map[string]string
		ignored []string // warnings the verdict holds before the guard's
	}{
		"a path past 4,096 bytes":            {map[string]string{"tool9/long.go": "package tool9\n\nimport \"fmt\"\n\nfunc f" + strings.Repeat("a", 5000) + "() { fmt.Println(1) }\n"}, nil},
		"warnings past a rank's maxReported": {map[string]string{"tool8/many.go": many.String()}, nil},
		"after the verdict's own warning":    {map[string]string{"tool7/many.go": wide.String()}, []string{"out.bin", "\xff" + strings.Repeat("o", 1000) + strings.Repeat("\xff", 30)}},
	} {
		in, out := f.withIgnored(t, integrity(f.repo, f.base, testLister(t)), c.files, c.ignored), f.withIgnored(t, Integrity(f.repo, f.base, testLister(t), ""), c.files, c.ignored)
		in.Elapsed, out.Elapsed = 0, 0
		if len(in.Warnings) == 0 || !reflect.DeepEqual(in.Warnings, out.Warnings) || !sameVerdict(t, in, out) {
			t.Errorf("%s: %d warnings in this process, %d in its own; %q", name, len(in.Warnings), len(out.Warnings), whats(out.Blocks))
		}
	}
	// A Lister's or a Reader's long panic (Astra #311 r2 #1): one block,
	// its text whole, as in this process: the controller's latch finds it in
	// the child's result, and adds none. Multi-byte text, cut by nothing
	// wherever a bound of 4,096 bytes would fall.
	change := map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"}
	g := newGuardFixture(t)
	for _, lead := range []int{0, 1, 2} {
		text := strings.Repeat("x", lead) + strings.Repeat("€", 1800) // 5,400 bytes: a rune across 4,096 at each lead
		listPanic := func(context.Context, string, [2]string, []byte, []byte, []string) (map[string]Listed, error) {
			panic(text)
		}
		readPanic := func(context.Context, string, int64) ([]byte, bool, error) { panic(text) }
		for name, guards := range map[string][2]Guard{
			"a Lister's": {integrity(g.repo, g.base, listPanic), Integrity(g.repo, g.base, listPanic, "")},
			"a Reader's": {wrapRead(integrity(g.repo, g.base, testLister(t)), readPanic), wrapRead(Integrity(g.repo, g.base, testLister(t), ""), readPanic)},
		} {
			in, out := g.withIgnored(t, guards[0], change, nil), g.withIgnored(t, guards[1], change, nil)
			in.Elapsed, out.Elapsed = 0, 0
			if len(out.Blocks) != 1 || !reflect.DeepEqual(in.Blocks, out.Blocks) || !sameVerdict(t, in, out) || !strings.HasSuffix(out.Blocks[0].What, text+")") {
				t.Errorf("%s long panic (lead %d): %d blocks in this process, %d in its own", name, lead, len(in.Blocks), len(out.Blocks))
			}
		}
	}
	// Past the result's bound: one unverifiable block, a false block for an
	// extreme result only (maxResultEntries lowered here; the controller
	// sends its bound to the child).
	oldEntries := maxResultEntries
	maxResultEntries = maxReported
	over := f.withIgnored(t, Integrity(f.repo, f.base, testLister(t), ""), map[string]string{"tool8/many.go": many.String()}, nil)
	maxResultEntries = oldEntries
	if len(over.Blocks) != 1 || len(over.Warnings) != 0 || over.Blocks[0].What != fmt.Sprintf("unverifiable: the guard's result is over its bound (0 blocks, %d warnings)", maxReported+500) {
		t.Errorf("a result past its bound: %q, %d warnings", whats(over.Blocks), len(over.Warnings))
	}
	// The bytes branch (Opus #339 L1): one 3.4 MB function name and 20
	// references into S make a result of about 71 MB, past maxFrame: the
	// stated block, a false block (main accepts with 21 warnings).
	var huge strings.Builder
	huge.WriteString("package tool6\n\nimport (\n\t\"fmt\"\n\t\"log\"\n\t\"os\"\n)\n\nfunc f" + strings.Repeat("a", 3_400_000) + "() {\n")
	for _, ref := range []string{"fmt.Println()", "fmt.Print()", "fmt.Printf(\"\")", "fmt.Fprintln(os.Stdout)", "fmt.Fprint(os.Stdout)", "fmt.Fprintf(os.Stdout, \"\")",
		"os.Exit(0)", "log.Fatal()", "log.Fatalf(\"\")", "log.Fatalln()", "log.Panic()", "log.Panicf(\"\")", "log.Panicln()", "log.Print()", "log.Printf(\"\")",
		"log.Println()", "_ = os.Stdout", "_ = os.Stderr", "_ = os.Args", "_ = os.Getpid()"} {
		huge.WriteString("\t" + ref + "\n")
	}
	huge.WriteString("}\n")
	bytesOver := f.withIgnored(t, Integrity(f.repo, f.base, testLister(t), ""), map[string]string{"tool6/x.go": huge.String()}, nil)
	if len(bytesOver.Blocks) != 1 || len(bytesOver.Warnings) != 0 || !strings.HasPrefix(bytesOver.Blocks[0].What, "unverifiable: the guard's result is over its bound (") ||
		!strings.HasSuffix(bytesOver.Blocks[0].What, " bytes)") {
		t.Errorf("a result past maxFrame: %q, %d warnings", whats(bytesOver.Blocks), len(bytesOver.Warnings))
	}
	// Listing requests as main makes them (Opus #339 M2): an import path of
	// 4,097 bytes, and 2^16+1 distinct imports, each listed whole. A
	// Lister that loads none: the same "not loaded" verdicts either way.
	none := func(_ context.Context, _ string, _ [2]string, _, _ []byte, paths []string) (map[string]Listed, error) {
		l := map[string]Listed{}
		for _, q := range paths {
			l[q] = Listed{ImportPath: q, Error: &struct{ Err string }{fmt.Sprintf("not loaded (%d bytes)", len(q))}}
		}
		return l, nil
	}
	var imports strings.Builder
	imports.WriteString("package pkg\n\nimport (\n")
	for i := range 1<<16 + 1 {
		fmt.Fprintf(&imports, "\t_ \"a%06d\"\n", i)
	}
	imports.WriteString(")\n")
	for name, files := range map[string]map[string]string{
		"an import path of 4,097 bytes": {"pkg/x.go": "package pkg\n\nimport _ \"example.org/" + strings.Repeat("a", 4097) + "\"\n"},
		"2^16+1 imports":                {"pkg/x.go": imports.String()},
	} {
		in, out := f.withIgnored(t, integrity(f.repo, f.base, none), files, nil), f.withIgnored(t, Integrity(f.repo, f.base, none, ""), files, nil)
		in.Elapsed, out.Elapsed = 0, 0
		if len(in.Blocks) == 0 || !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || !sameVerdict(t, in, out) {
			t.Errorf("%s: in this process %.300q, in its own %.300q", name, whats(in.Blocks), whats(out.Blocks))
		}
	}
	// Every recover of the guard's that turns a service's panic into a block
	// (Opus #339 M1): the guard's own ("the guard failed") and decision A's
	// ("…outside-initialization check failed", reading vendor/modules.txt
	// first, Opus's input). The child's block acknowledges the panic; the
	// latch adds none. Enumerated: a Reader panicking on each blob a change
	// reads, in turn, and a Lister on each of its calls, so every recover a
	// service call reaches is met; the others (pyLex's, the Python scan's,
	// the types checks' workers) run no service call (the PR lists them).
	vb := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"go.work": "go 1.26.0\n\nuse .\n", "vendor/modules.txt": "## workspace\n"})
		plain(t, src, "add", "--all")
	})
	goChange := map[string]string{"pkg/calc.go": "package pkg\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc Add(a, b int) int {\n\tfmt.Println(\"x\")\n\t_ = os.Getpid()\n\treturn a + b\n}\n"}
	changes := map[string]struct {
		at    fixture
		files map[string]string
	}{
		"a Go change, vendored (Opus's)": {vb, goChange},
		"a Go change, listed":            {g, goChange},
		"a Python change":                {vb, map[string]string{"money.py": baseFiles["money.py"] + "\nimport os\nos._exit(0)\n"}},
	}
	modules := []byte("## workspace\n")
	decisionA, emptyA, listings := false, false, 0
	for name, c := range changes {
		vb, change := c.at, c.files
		var blobs, calls []string // what the change reads and lists, in this process
		record := func(guard func(Lister) Guard) Guard {
			lister := func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
				calls = append(calls, pl[0]+"/"+pl[1]+" "+strings.Join(paths, ","))
				return testLister(t)(ctx, dir, pl, gomod, gosum, paths)
			}
			g := guard(lister)
			return func(ctx context.Context, c []Change, read Reader) ([]Block, []Block, error) {
				return g(ctx, c, func(ctx context.Context, blob string, max int64) ([]byte, bool, error) {
					blobs = append(blobs, blob)
					return read(ctx, blob, max)
				})
			}
		}
		vb.withIgnored(t, record(func(l Lister) Guard { return integrity(vb.repo, vb.base, l) }), change, nil)
		blobs, calls = slices.Compact(slices.Sorted(slices.Values(blobs))), slices.Compact(slices.Sorted(slices.Values(calls)))
		for i := range 2*len(blobs) + 2*len(calls) {
			at, text := slices.Concat(blobs, calls)[i/2], "" // an empty panic (Opus #339 r2 M1′), then a multi-byte one, whole
			if i%2 == 1 {
				text = "boom " + at[:min(len(at), 40)] + " é€"
			}
			panicking := func(guard func(Lister) Guard) Guard {
				lister := func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
					if pl[0]+"/"+pl[1]+" "+strings.Join(paths, ",") == at {
						panic(text)
					}
					return testLister(t)(ctx, dir, pl, gomod, gosum, paths)
				}
				g := guard(lister)
				return func(ctx context.Context, c []Change, read Reader) ([]Block, []Block, error) {
					return g(ctx, c, func(ctx context.Context, blob string, max int64) ([]byte, bool, error) {
						b, ok, err := read(ctx, blob, max)
						if blob == at {
							decisionA = decisionA || bytes.Equal(b, modules)
							panic(text)
						}
						return b, ok, err
					})
				}
			}
			in := vb.withIgnored(t, panicking(func(l Lister) Guard { return integrity(vb.repo, vb.base, l) }), change, nil)
			out := vb.withIgnored(t, panicking(func(l Lister) Guard { return Integrity(vb.repo, vb.base, l, "") }), change, nil)
			in.Elapsed, out.Elapsed = 0, 0
			if !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || !sameVerdict(t, in, out) {
				t.Errorf("%s, a panic %q at %.60s: in this process %q, in its own %q", name, text, at, whats(in.Blocks), whats(out.Blocks))
			}
			if bytes.Contains([]byte(strings.Join(whats(in.Blocks), "\n")), []byte("decision A's outside-initialization check failed ("+text+")")) {
				decisionA, emptyA = decisionA || text != "", emptyA || text == ""
			}
		}
		t.Logf("%s: two panics (empty, and not) at each of %d blobs and %d listings", name, len(blobs), len(calls))
		listings += len(calls)
	}
	if !decisionA || !emptyA || listings == 0 {
		t.Errorf("decision A's recover met: %v, by an empty panic: %v; %d listings: the enumeration no longer meets them", decisionA, emptyA, listings)
	}
	// #1 (Astra #293 r2): an empty root go.mod added, or deleted: the
	// in-process guard's verdicts (no absent blob read).
	readme := map[string]string{"README.md": "x\n"}
	for name, c := range map[string]struct {
		base, files map[string]string
		gone        []string
	}{
		"an empty go.mod added":   {readme, map[string]string{"go.mod": ""}, nil},
		"an empty go.mod deleted": {map[string]string{"README.md": "x\n", "go.mod": ""}, nil, []string{"go.mod"}},
	} {
		m := newFixtureWith(t, func(src string) {
			write(t, src, c.base)
			plain(t, src, "add", "--all")
		})
		in, out := m.guarded(t, c.files, c.gone...), m.loaded(t, testLister(t), c.files, c.gone...)
		if !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || !out.Accepted() {
			t.Errorf("%s: in this process %q %q, in its own %q %q", name, whats(in.Blocks), whats(in.Warnings), whats(out.Blocks), whats(out.Warnings))
		}
	}
	// The base's links cross to the child (main's Opus #279 M1, Astra #281
	// r2 H5): a judged base symlink is unread there too.
	lm := newFixtureWith(t, func(src string) {
		flags := "import pytest\nimport feature.flags\n\n@pytest.mark.skipif(feature.flags.SKIP, reason='r')\ndef test_z():\n    assert False\n"
		write(t, src, map[string]string{"feature/__init__.py": "", "feature/flags.py": "SKIP = False\n", "cfg.py": "SKIP = False\n", "tests/test_z.py": flags})
		if err := os.Symlink("X=1", filepath.Join(src, "tests", "test_y.py")); err != nil {
			t.Fatal(err)
		}
		plain(t, src, "add", "--all")
	})
	cfg := map[string]string{"cfg.py": "SKIP = 'True'\n"}
	if in, out := lm.guarded(t, cfg), lm.loaded(t, testLister(t), cfg); !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) ||
		!slices.ContainsFunc(out.Blocks, func(b Block) bool { return strings.Contains(b.What, "tests/test_y.py is unread") }) {
		t.Errorf("a base symlink: in this process %q, in its own %q", whats(in.Blocks), whats(out.Blocks))
	}
	// #2 (Astra #293 r2): nine nested modules of identical module files
	// (each listing's payload sent once, then referred to), within a lowered
	// output bound that the files resent on every listing would pass.
	nested := map[string]string{}
	for i := range 9 {
		d := fmt.Sprintf("n%d/", i)
		nested[d+"go.mod"] = "module example.com/n\n\ngo 1.24\n\n// " + strings.Repeat("p", 100<<10) + "\n"
		nested[d+"go.sum"] = strings.Repeat("\n", 100<<10)
		nested[d+"x.go"] = "package n\n\nimport \"strings\"\n\nfunc F() string { return strings.ToUpper(\"a\") }\n"
	}
	nm := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, nested)
		plain(t, src, "add", "--all")
	})
	changed := map[string]string{}
	for i := range 9 {
		changed[fmt.Sprintf("n%d/x.go", i)] = "package n\n\nimport \"strings\"\n\nfunc F() string { return strings.ToUpper(\"b\") }\n"
	}
	oldOut := maxProcOut
	maxProcOut = 2 << 20 // 36 listings resending both files: about 7 MiB
	in, out := nm.guarded(t, changed), nm.loaded(t, testLister(t), changed)
	maxProcOut = oldOut
	if !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || !out.Accepted() {
		t.Errorf("nine nested modules: in this process %q %q, in its own %q %q", whats(in.Blocks), whats(in.Warnings), whats(out.Blocks), whats(out.Warnings))
	}
	// #2 (Astra #287 r2): an absent go.mod (the loader's fallback module)
	// and a present, empty one cross the frame as they are: the same
	// verdicts as in this process, and the absent one typed.
	helper := map[string]string{"pkg/help.go": "package pkg\n\nimport \"fmt\"\n\nfunc H() string { return fmt.Sprint(1) }\n"}
	for name, mod := range map[string]map[string]string{"no go.mod": nil, "an empty go.mod": {"go.mod": ""}} {
		m := newFixtureWith(t, func(src string) {
			b := maps.Clone(guardBase)
			delete(b, "go.mod")
			delete(b, "go.sum")
			write(t, src, b)
			write(t, src, mod)
			plain(t, src, "add", "--all")
		})
		in, out := m.guarded(t, helper), m.loaded(t, testLister(t), helper)
		if !reflect.DeepEqual(in.Blocks, out.Blocks) || !reflect.DeepEqual(in.Warnings, out.Warnings) || mod == nil && !out.Accepted() {
			t.Errorf("%s: in this process %q %q, in its own %q %q", name, whats(in.Blocks), whats(in.Warnings), whats(out.Blocks), whats(out.Warnings))
		}
	}
	// The baseline: an ordinary run in its own process.
	begin := time.Now()
	if v := f.loaded(t, testLister(t), plainGo); !v.Accepted() {
		t.Fatalf("the baseline: %q", whats(v.Blocks))
	}
	baseline := time.Since(begin)
	// F: a type check that never ends (validType on a chain of 40), no type
	// check deadline before the whole-run deadline: the child is killed at
	// it, the change unverifiable; a later ordinary run is accepted.
	var chain strings.Builder
	chain.WriteString("package pkg\n")
	for i := range 40 {
		fmt.Fprintf(&chain, "\ntype S%d struct{ A, B, C, D, E, F, G, H S%d }\n", i, i+1)
	}
	chain.WriteString("\ntype S40 int\n")
	oldList, oldRun := maxListTime, maxRunTime
	t.Cleanup(func() { maxListTime, maxRunTime = oldList, oldRun })
	maxListTime, maxRunTime = time.Hour, 2*baseline+time.Second
	n, begin := len(procs()), time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := f.loadedErr(t, testLister(t), map[string]string{"pkg/chain.go": chain.String()})
		done <- err
	}()
	select {
	case err := <-done: // Astra chunk 1 H2: past its whole-run deadline, the guard did not run, no finding
		took := time.Since(begin)
		if !didNotRunErr(err, "the guard did not finish by its whole-run deadline") || took > maxRunTime+2*baseline+5*time.Second {
			t.Errorf("a type check that never ends: %v (deadline %v), %v", took, maxRunTime, err)
		}
	case <-time.After(maxRunTime + 10*baseline + 30*time.Second):
		t.Fatalf("a type check that never ends: no verdict in %v", time.Since(begin))
	}
	if ps := procs(); len(ps) != n+1 || !exited(ps[n]) {
		t.Errorf("the killed child: %d children, the last alive", len(ps)-n)
	}
	maxListTime, maxRunTime = oldList, oldRun
	if v := f.loaded(t, testLister(t), plainGo); !v.Accepted() {
		t.Errorf("a run after the kill: %q", whats(v.Blocks))
	}
	// Past the type check deadline in the child: abandoned there (main's
	// block), the child ended with its result, and a later run accepted
	// (main blocked it while the check ran: a reviewed relaxation). The
	// deadline is the baseline's twice: listing is over by then (Astra L7).
	maxListTime = 2*baseline + time.Second
	n = len(procs())
	if v := f.loaded(t, testLister(t), map[string]string{"pkg/chain.go": chain.String()}); !finds(v.Blocks, "unverifiable: the guard failed (a type check over the guard's deadline (") {
		t.Errorf("past the type check deadline: %q", whats(v.Blocks))
	}
	maxListTime = oldList
	if ps := procs(); len(ps) != n+1 || !exited(ps[n]) {
		t.Errorf("an abandoned check's child: %d children, the last alive", len(ps)-n)
	}
	if v := f.loaded(t, testLister(t), plainGo); !v.Accepted() {
		t.Errorf("a run after an abandoned check: %q", whats(v.Blocks))
	}
	// A child that ends without its result, or writes past its bound: the
	// guard did not run (Astra chunk 1 H2).
	maxProcOut = 64
	_, err := f.loadedErr(t, testLister(t), plainGo)
	if maxProcOut = oldOut; !didNotRunErr(err, "the guard's process ended without its result") {
		t.Errorf("a child over its output bound: %v", err)
	}
	// H3 (Astra #287): one deadline for the whole run. A reader that waits
	// for its context, and one that does not: the run ends at the deadline,
	// its child reaped; its directory is removed once no call runs in it,
	// the stuck one's only when it returns (Opus #293 L2).
	stuck := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(stuck) })
	for name, read := range map[string]Reader{
		"a reader waiting for its context": func(ctx context.Context, _ string, _ int64) ([]byte, bool, error) {
			<-ctx.Done()
			return nil, false, ctx.Err()
		},
		"a reader that never returns": func(context.Context, string, int64) ([]byte, bool, error) { <-stuck; return nil, false, nil },
	} {
		maxRunTime = baseline + 250*time.Millisecond // the run reaches its first read well within a whole run's time
		scratch, n, begin := t.TempDir(), len(procs()), time.Now()
		g := Integrity(f.repo, f.base, testLister(t), scratch)
		done := make(chan error, 1)
		go func() {
			_, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, plainGo), Policy: f.policy,
				Guard: func(ctx context.Context, c []Change, _ Reader) ([]Block, []Block, error) {
					return g(ctx, c, read)
				}})
			done <- err
		}()
		select {
		case err := <-done:
			left, _ := os.ReadDir(scratch)
			stuckCall := name == "a reader that never returns" // its call still runs: its directory stays
			if ps := procs(); !didNotRunErr(err, "did not finish by its whole-run deadline") || len(ps) != n+1 || !exited(ps[n]) || stuckCall && len(left) != 1 {
				t.Errorf("%s: %v, %d children, left %d", name, err, len(ps)-n, len(left))
			}
			if stuckCall {
				once.Do(func() { close(stuck) })
			}
			// The directory goes once no call runs in it: a reader that
			// returns as its context ends can outlive the run by a moment.
			for end := time.Now().Add(10 * time.Second); len(left) != 0 && time.Now().Before(end); left, _ = os.ReadDir(scratch) {
				time.Sleep(10 * time.Millisecond)
			}
			if len(left) != 0 {
				t.Errorf("%s: its directory kept once no call runs in it", name)
			}
		case <-time.After(maxRunTime + 10*baseline + 30*time.Second):
			t.Errorf("%s: no verdict in %v", name, time.Since(begin))
		}
		maxRunTime = oldRun
	}
}

// localBox runs a sandbox run's argv here, as the image would: the mounts
// are the attempt's directories, the image's layout this host's go.
type localBox struct {
	t       *testing.T
	gobin   string
	caches  [2]string // GOCACHE, GOMODCACHE
	ws      *[]string // the workspace's entries at each start
	mutate  func(results string)
	started atomic.Int32
	copied  []int // export data files each run copied out
}

func newLocalBox(t *testing.T) *localBox {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	out, err := exec.Command(gobin, "env", "GOCACHE", "GOMODCACHE").Output()
	c := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || len(c) != 2 {
		t.Fatalf("go env: %v %q", err, out)
	}
	return &localBox{t: t, gobin: gobin, caches: [2]string{c[0], c[1]}, ws: &[]string{}}
}

func (b *localBox) NewAttempt() (*sandbox.Attempt, error) {
	a := &sandbox.Attempt{Dir: b.t.TempDir()}
	return a, errors.Join(os.Mkdir(a.Workspace(), 0o700), os.Mkdir(a.Results(), 0o700))
}

func (b *localBox) Start(ctx context.Context, r sandbox.Run) ([]byte, error) {
	b.started.Add(1)
	entries, _ := os.ReadDir(r.Attempt.Workspace())
	for _, e := range entries {
		*b.ws = append(*b.ws, e.Name())
	}
	if mod, _ := os.ReadFile(filepath.Join(r.Attempt.Workspace(), "go.mod")); strings.Contains(string(mod), "=> ../") {
		b.t.Errorf("a loader run with a directory replacement: %q", mod)
	}
	if r.Mode != sandbox.Gate || r.Argv[0] != "env" || r.Argv[1] != "-i" {
		b.t.Errorf("a loader run: %s %q", r.Mode, r.Argv[:2])
	}
	argv := slices.Clone(r.Argv)
	for i, a := range argv {
		switch k, _, _ := strings.Cut(a, "="); {
		case a == sandbox.ResultsDir:
			argv[i] = r.Attempt.Results()
		case k == "PATH":
			argv[i] = "PATH=" + filepath.Dir(b.gobin) + ":/usr/bin:/bin"
		case k == "GOCACHE":
			argv[i] = "GOCACHE=" + b.caches[0]
		case k == "GOPATH":
			argv[i] = "GOMODCACHE=" + b.caches[1]
		case a == listScript:
			argv[i] = strings.ReplaceAll(a, "/tmp/gocache", filepath.Join(r.Attempt.Dir, "cold"))
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.Attempt.Workspace()
	out, err := cmd.Output()
	xs, _ := os.ReadDir(filepath.Join(r.Attempt.Results(), "x"))
	b.copied = append(b.copied, len(xs))
	if err == nil && b.mutate != nil {
		b.mutate(r.Attempt.Results())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) { // as sandbox.Host.Start reports the workload's own status
		err = &sandbox.WorkloadExit{Code: exit.ExitCode(), Err: err}
	}
	return out, err
}

// TestSandboxLister (P14e-2c): the production loader's go list, in a stand-in
// sandbox running its argv with this host's go: only go.mod and go.sum in
// the workspace, the export data copied out into the run's directory, the
// gate's Go environment pinned (a candidate's raised toolchain line never
// switches go; a raised go line is unverifiable), a base go.work never
// read, and what comes out checked.
func TestSandboxLister(t *testing.T) {
	work := "go 1.26.0\n\nuse ./missing\n" // read, it fails: ./missing has no go.mod
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, map[string]string{"go.work": work})
		plain(t, src, "add", "--all")
	})
	box := newLocalBox(t)
	list := boxLister(box, sandbox.Image{}, sandbox.Limits{})
	change := map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\nfunc Sum(xs ...int) int { return len(fmt.Sprint(xs)) }\n"}
	change["pkg/calc.go"] = strings.Replace(change["pkg/calc.go"], "package pkg\n", "package pkg\n\nimport \"fmt\"\n", 1)
	typed := func(v Verdict) bool { return v.Accepted() && !finds(v.Blocks, "not loaded") }
	if v := f.loaded(t, list, change); !typed(v) || box.started.Load() == 0 {
		t.Errorf("a change typed through the loader (a base go.work beside it): %d runs, %q", box.started.Load(), whats(v.Blocks))
	}
	for _, e := range *box.ws {
		if e != "go.mod" && e != "go.sum" {
			t.Errorf("the loader's workspace holds %q", e)
		}
	}
	// F: a candidate raising the toolchain line: typed by the image's own go
	// (GOTOOLCHAIN=local), as the gate builds it under GoEnv.
	raised := maps2(change, "go.mod", guardBase["go.mod"]+"\ntoolchain go1.99.0\n")
	if v := f.loaded(t, list, raised); !typed(v) {
		t.Errorf("a raised toolchain line: %q", whats(v.Blocks))
	}
	if v := f.loaded(t, list, maps2(change, "go.mod", strings.Replace(guardBase["go.mod"], "go 1.26.0", "go 1.99", 1))); !finds(v.Blocks, "go list in the prepared image") {
		t.Errorf("a raised go line: %q", whats(v.Blocks))
	}
	// What comes out: another release's go, export data not copied out, an
	// unreadable list: each import not loaded, blocked. Export data is read
	// from the copy only, never from the path go listed ("": typed).
	relist := func(re, to string) func(string) {
		return func(r string) {
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			_ = os.WriteFile(filepath.Join(r, "list"), regexp.MustCompile(re).ReplaceAll(b, []byte(to)), 0o600)
		}
	}
	// A listing whose output is wrong with no bound broken is no evidence
	// against the candidate: the guard did not run (ErrGuardRun; #226 entry
	// 147). A bound its run broke is the candidate's: a block.
	for name, c := range map[string]struct {
		mutate func(results string)
		why    string
		noRun  bool
	}{
		"another release": {func(r string) { _ = os.WriteFile(filepath.Join(r, "version"), []byte("go1.0\n"), 0o600) },
			"the toolchain is \"go1.0\", not the guard's " + runtime.Version(), true},
		"export data not copied out": {relist(`\t/[^\t]*-d\t`, "\t/etc/passwd\t"), "its export data not copied out", false},
		"a listed path elsewhere":    {relist(`\t/[^\t]*/([^/\t]*-d)\t`, "\t/nonexistent/$1\t"), "", false},
		"an unreadable list":         {func(r string) { _ = os.WriteFile(filepath.Join(r, "list"), []byte("fmt\n"), 0o600) }, "an unreadable list", true},
		// Astra #287 H2 (A): an error beside a valid archive; a package
		// listed twice; (B) an archive not named by its content.
		"an error beside an archive": {relist(`(?m)^fmt\t([^\t]+)\t$`, "fmt\t$1\t\"cache failure\""), "(fmt: cache failure)", false},
		"a package listed twice":     {relist(`(?m)^(fmt\t[^\n]*\n)`, "$1$1"), "(fmt: listed twice)", false},
		"a corrupted archive": {func(r string) {
			xs, _ := os.ReadDir(filepath.Join(r, "x"))
			f, _ := os.OpenFile(filepath.Join(r, "x", xs[0].Name()), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.Write([]byte{0})
			_ = f.Close()
		}, "an archive not named by its content", true},
		// M6: an unexpected output; a list past maxList (Astra's 500 MiB
		// list, just past the bound: long lines, fewer than maxListEntries),
		// refused before it is read; one past maxListEntries; and archives
		// the run already holds, delivered again.
		"an unexpected output": {func(r string) { _ = os.WriteFile(filepath.Join(r, "extra"), nil, 0o600) }, "an unexpected output", true},
		"a list past maxList": {func(r string) {
			pad := bytes.Repeat([]byte("example.org/"+strings.Repeat("p", 2000)+"\t\t\n"), int(maxList)/2000)
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			_ = os.WriteFile(filepath.Join(r, "list"), append(b, pad...), 0o600)
		}, "a list over its bound", false},
		"a list past maxListEntries": {func(r string) {
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			_ = os.WriteFile(filepath.Join(r, "list"), append(b, bytes.Repeat([]byte("example.org/p\t\t\n"), maxListEntries)...), 0o600)
		}, "a list over its bound", false},
		"archives delivered again": {func(r string) {
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			for line := range strings.Lines(string(b)) {
				if e := strings.Split(line, "\t")[1]; e != "" {
					src, _ := os.ReadFile(e)
					_ = os.WriteFile(filepath.Join(r, "x", filepath.Base(e)), src, 0o600)
				}
			}
		}, "an archive the run holds", true},
		// Astra #287 r2: #4 an error column present and empty; #7 one
		// record past maxListEntries, the last with no final newline.
		"an empty error listed": {relist(`(?m)^fmt\t([^\t]+)\t$`, "fmt\t$1\t\"\""), "(fmt: an empty error listed)", false},
		"maxListEntries+1 records, no final newline": {func(r string) {
			var b strings.Builder
			for i := range maxListEntries + 1 {
				fmt.Fprintf(&b, "%sexample.org/p%d\t\t", map[bool]string{true: "\n"}[i > 0], i)
			}
			_ = os.WriteFile(filepath.Join(r, "list"), []byte(b.String()), 0o600)
		}, "a list over its bound", false},
	} {
		box.mutate = c.mutate
		if c.noRun {
			if _, err := f.loadedErr(t, list, change); !didNotRunErr(err, c.why) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if v := f.loaded(t, list, change); c.why == "" && !typed(v) || c.why != "" && !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q", name, whats(v.Blocks))
		}
	}
	box.mutate = nil
	// #2 (Astra #287 r2): an absent go.mod (the fallback module) and a
	// present, empty one, each as main's loader has them.
	for name, mod := range map[string]map[string]string{"no go.mod": nil, "an empty go.mod": {"go.mod": ""}} {
		m := newFixtureWith(t, func(src string) {
			b := maps.Clone(guardBase)
			delete(b, "go.mod")
			delete(b, "go.sum")
			write(t, src, b)
			write(t, src, mod)
			plain(t, src, "add", "--all")
		})
		fresh := LocalLister(box.gobin, []string{"GOCACHE=" + box.caches[0], "GOMODCACHE=" + box.caches[1]}, 5*time.Minute, 8<<30) // main's, its cache its own
		got, main := m.loaded(t, list, change), m.loaded(t, fresh, change)
		cause := regexp.MustCompile(`(?s)\(.*\)`) // the lister's own wording of its error
		norm := func(bs []Block) (out []string) {
			for _, w := range whats(bs) {
				out = append(out, cause.ReplaceAllString(w, "(…)"))
			}
			return out
		}
		if !slices.Equal(norm(got.Blocks), norm(main.Blocks)) || !slices.Equal(norm(got.Warnings), norm(main.Warnings)) {
			t.Errorf("%s: %q %q, main's %q %q", name, whats(got.Blocks), whats(got.Warnings), whats(main.Blocks), whats(main.Warnings))
		}
	}
	// H2 (A), each layer alone: the loader drops an errored entry's
	// archive; the typer loads no entry with an error, whatever it holds.
	box.mutate = relist(`(?m)^fmt\t([^\t]+)\t$`, "fmt\t$1\t\"cache failure\"")
	if got, err := list(context.Background(), t.TempDir(), [2]string{"linux", "amd64"}, []byte("module example.com/m\n\ngo 1.26.0\n"), nil, []string{"fmt"}); err != nil || got["fmt"].Export != "" || got["fmt"].Error == nil {
		t.Errorf("the loader, an error beside an archive: %v %+v", err, got["fmt"])
	}
	box.mutate = nil
	both := func(ctx context.Context, dir string, pl [2]string, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		got, err := testLister(t)(ctx, dir, pl, gomod, gosum, paths)
		out := map[string]Listed{}
		for p, l := range got {
			if p == "fmt" {
				l.Error = &struct{ Err string }{"cache failure"}
			}
			out[p] = l
		}
		return out, err
	}
	if v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, change), Policy: f.policy, Guard: Integrity(f.repo, f.base, both, "")}); err != nil || !finds(v.Blocks, "(fmt: cache failure)") {
		t.Errorf("the typer, an error beside an archive: %v %q", err, whats(v.Blocks))
	}
	// A directory replacement is refused before any run of it.
	if v := f.loaded(t, list, maps2(change, "go.mod", guardBase["go.mod"]+"\nreplace golang.org/x/sys => ../sys\n")); !finds(v.Blocks, "go.mod replaces a module by a directory") {
		t.Errorf("a directory replacement: %q", whats(v.Blocks))
	}
	// M1 (Opus #287): five modules listing the same heavy import copy its
	// export data once in the run, all within one budget; past it, a
	// listing fails (not loaded).
	dir, amd64 := t.TempDir(), [2]string{"linux", "amd64"}
	box.copied = nil
	for i := range 5 {
		got, err := list(context.Background(), dir, amd64, fmt.Appendf(nil, "module example.com/m%d\n\ngo 1.26.0\n", i), nil, []string{"net/http"})
		if err != nil || got["net/http"].Export == "" {
			t.Fatalf("listing %d: %v %+v", i, err, got["net/http"])
		}
	}
	var size int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, _ error) error {
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			size += fi.Size()
		}
		return nil
	})
	if len(box.copied) != 5 || box.copied[0] == 0 || slices.Max(box.copied[1:]) != 0 || size > maxRunCopy() {
		t.Errorf("five listings of net/http: copied %v, %d bytes (bound %d)", box.copied, size, maxRunCopy())
	}
	// L7 (Astra #287): two distinct archive sets (fmt on each platform)
	// that each fit the run's allowance and together do not: the
	// allowance is what is left of the run's, not each listing's.
	fmtOn := func(dir, arch string) error {
		_, err := list(context.Background(), dir, [2]string{"linux", arch}, []byte("module example.com/m\n\ngo 1.26.0\n"), nil, []string{"fmt"})
		return err
	}
	d1 := t.TempDir()
	if err := fmtOn(d1, "amd64"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d1, "copied"))
	one, _ := strconv.ParseInt(string(b), 10, 64)
	oldExport, oldList, oldRuns := maxExport, maxList, maxListRuns
	defer func() { maxExport, maxList, maxListRuns = oldExport, oldList, oldRuns }()
	maxExport, maxList, maxListRuns = (one-1<<20)*3/4, 1<<20, 1 // 1.5 of one listing, less 0.5 MiB
	d2 := t.TempDir()
	alone, first := fmtOn(t.TempDir(), "arm64"), fmtOn(d2, "amd64")
	if second := fmtOn(d2, "arm64"); alone != nil || first != nil || second == nil || !strings.Contains(second.Error(), "more than") {
		t.Errorf("two archive sets past the run's allowance (%d each): alone %v, first %v, second %v", one, alone, first, second)
	}
}

// TestReadListing (#3, Astra #287 r2): a version past its bound (Astra's
// 768 MiB of spaces, scaled to 1 MiB), and a list of one line of tabs
// (16 MiB, scaled with maxList to 1 MiB), are refused with bounded allocation.
func TestReadListing(t *testing.T) {
	oldList := maxList
	defer func() { maxList = oldList }()
	maxList = 1 << 20
	for name, c := range map[string]struct {
		version, list []byte
		why           string
		alloc         uint64
	}{
		"a version past its bound": {append([]byte(runtime.Version()), bytes.Repeat([]byte(" "), 1<<20)...), nil, "a version over its bound", 64 << 10},
		"a line of tabs":           {[]byte(runtime.Version()), bytes.Repeat([]byte("\t"), 1<<20), "an unreadable list", 3<<20 + 1<<20},
	} {
		out := t.TempDir()
		write(t, out, map[string]string{"version": string(c.version), "list": string(c.list)})
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := readListing(out, t.TempDir())
		runtime.ReadMemStats(&after)
		if err == nil || !strings.Contains(err.Error(), c.why) || after.TotalAlloc-before.TotalAlloc > c.alloc {
			t.Errorf("%s: %v, %d bytes allocated", name, err, after.TotalAlloc-before.TotalAlloc)
		}
	}
}

// TestGoEnv (L5, Opus #287): the gate's Go environment, whole.
func TestGoEnv(t *testing.T) {
	want := []string{"GOFLAGS=-mod=readonly -buildvcs=false", "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off",
		"GOENV=off", "GOTELEMETRY=off", "GODEBUG=goindex=0", "CGO_ENABLED=0", "GO111MODULE=on", "GOEXPERIMENT=", "GOAMD64=v1", "GOARM64=v8.0"}
	if !slices.Equal(GoEnv, want) {
		t.Errorf("GoEnv: %q", GoEnv)
	}
}

// TestNoInit (H1, Opus #287): no file of the package has an init (Go runs
// them in file-name order: a child dispatched from one would run the guard
// before a later one), and adcd's main asks first.
func TestNoInit(t *testing.T) {
	paths, _ := filepath.Glob("*.go")
	for _, p := range append(paths, "../../cmd/adcd/main.go") {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
				t.Errorf("%s has an init", p)
			} else if ok && p == "../../cmd/adcd/main.go" && fn.Name.Name == "main" && !firstCall(fn, "RunChildIfAsked") {
				t.Errorf("adcd's main does not ask first")
			}
		}
	}
	if len(paths) < 20 {
		t.Fatalf("only %d files", len(paths))
	}
}

// firstCall: fn's first statement calls a function named name.
func firstCall(fn *ast.FuncDecl, name string) bool {
	s, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	c, ok := s.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

// maps2 is m with k set to v.
func maps2(m map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for mk, mv := range m {
		if mk != k {
			out[mk] = mv
		}
	}
	return out
}

// vendorTyping turns vendorBlock off for t: its fixtures test the guard's
// typing of vendor/, kept for the item that pins -mod=vendor.
func vendorTyping(t *testing.T) {
	vendorBlock = false
	t.Cleanup(func() { vendorBlock = true })
}

// TestIntegrityVendored (the operator's decision on #287): a vendor/
// directory beside a go.mod, in the base or the candidate, at the root or in
// a nested module, blocks with its reason (the gate builds from the module
// cache under GoEnv, the guard would type vendor/); a vendor/ directory
// beside no go.mod is not Go's.
func TestIntegrityVendored(t *testing.T) {
	const why = "unverifiable: a vendored module: the gate builds from the module cache (GOFLAGS=-mod=readonly), not vendor/"
	lib := map[string]string{"go.mod": guardBase["go.mod"] + "\nrequire example.org/lib v1.0.0\n",
		"vendor/modules.txt":            "# golang.org/x/sys v0.48.0\n## explicit\n# example.org/lib v1.0.0\n## explicit\nexample.org/lib\n",
		"vendor/example.org/lib/lib.go": "package lib\n\nfunc F() int { return 1 }\n",
		"sub/go.mod":                    "module example.com/sub\n\ngo 1.26.0\n", "sub/vendor/modules.txt": "", "tools/vendor/notes.txt": "x\n"}
	f := newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		write(t, src, lib)
		plain(t, src, "add", "--all")
	})
	plainGo := map[string]string{"pkg/x.go": "package pkg\n\nimport \"example.org/lib\"\n\nfunc G() int { return lib.F() }\n"}
	v := f.guarded(t, plainGo)
	if !slices.Contains(whats(v.Blocks), "vendor/: "+why) || !slices.Contains(whats(v.Blocks), "sub/vendor/: "+why) || finds(v.Blocks, "tools/vendor/") {
		t.Errorf("a vendored base: %q", whats(v.Blocks))
	}
	g := newGuardFixture(t)
	if v := g.guarded(t, map[string]string{"vendor/example.org/lib/lib.go": lib["vendor/example.org/lib/lib.go"]}); !slices.Contains(whats(v.Blocks), "vendor/: "+why) {
		t.Errorf("a candidate adding vendor/: %q", whats(v.Blocks))
	}
	if v := g.guarded(t, map[string]string{"tools/vendor/notes.txt": "x\n"}); !v.Accepted() {
		t.Errorf("a vendor/ beside no go.mod: %q", whats(v.Blocks))
	}
}

// withIgnored is guarded's Accept with guard, and ignored paths' warnings
// before the guard's.
func (f fixture) withIgnored(t *testing.T, guard Guard, files map[string]string, ignored []string) Verdict {
	t.Helper()
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, files), Policy: f.policy, Ignored: ignored, Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// wrapRead is guard reading through read, whatever Accept's reader is.
func wrapRead(guard Guard, read Reader) Guard {
	return func(ctx context.Context, c []Change, _ Reader) ([]Block, []Block, error) { return guard(ctx, c, read) }
}
