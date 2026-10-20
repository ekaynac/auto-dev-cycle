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
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, files, gone...), Policy: f.policy,
		Guard: Integrity(f.repo, f.base, list, "")})
	if err != nil {
		t.Fatal(err)
	}
	return v
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
	// M1 (Opus #293): results past the frame's bounds (a Path past 4,096
	// bytes; more warnings than a rank keeps, maxReported) fit to it: the
	// same Accepted() and the same report, byte for byte. Opus's probe had
	// 70,000 warnings (142 s here); past maxReported, any count takes the
	// same path, and 1,500 keeps the package within its time.
	var many strings.Builder
	many.WriteString("package tool8\n\nimport \"fmt\"\n")
	for i := range maxReported + 500 {
		fmt.Fprintf(&many, "\nfunc f%d() { fmt.Println() }\n", i)
	}
	for name, files := range map[string]map[string]string{
		"a path past the frame's bound":      {"tool9/long.go": "package tool9\n\nimport \"fmt\"\n\nfunc f" + strings.Repeat("a", 5000) + "() { fmt.Println(1) }\n"},
		"warnings past a rank's maxReported": {"tool8/many.go": many.String()},
	} {
		in, out := f.guarded(t, files), f.loaded(t, testLister(t), files)
		in.Elapsed, out.Elapsed = 0, 0
		if in.Accepted() != out.Accepted() || len(in.Warnings) == 0 || name != "a path past the frame's bound" && len(in.Warnings) <= maxReported ||
			len(out.Warnings) != len(in.Warnings) || !sameReport(t, in.Report, out.Report) {
			t.Errorf("%s: accepted %v, %d warnings; in its own %v, %d warnings; %q", name, in.Accepted(), len(in.Warnings), out.Accepted(), len(out.Warnings), whats(out.Blocks))
		}
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
	done := make(chan Verdict, 1)
	go func() { done <- f.loaded(t, testLister(t), map[string]string{"pkg/chain.go": chain.String()}) }()
	select {
	case v := <-done:
		took := time.Since(begin)
		if !finds(v.Blocks, "unverifiable: the guard did not finish by its whole-run deadline") || took > maxRunTime+2*baseline+5*time.Second {
			t.Errorf("a type check that never ends: %v (deadline %v), %q", took, maxRunTime, whats(v.Blocks))
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
	// A child that ends without its result, or writes past its bound, blocks.
	oldOut := maxProcOut
	maxProcOut = 64
	v := f.loaded(t, testLister(t), plainGo)
	if maxProcOut = oldOut; !finds(v.Blocks, "unverifiable: the guard's process ended without its result") {
		t.Errorf("a child over its output bound: %q", whats(v.Blocks))
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
		maxRunTime = 2*baseline + time.Second
		scratch, n, begin := t.TempDir(), len(procs()), time.Now()
		g := Integrity(f.repo, f.base, testLister(t), scratch)
		done := make(chan Verdict, 1)
		go func() {
			v, _ := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, plainGo), Policy: f.policy,
				Guard: func(ctx context.Context, c []Change, _ Reader) ([]Block, []Block, error) { return g(ctx, c, read) }})
			done <- v
		}()
		select {
		case v := <-done:
			left, _ := os.ReadDir(scratch)
			stuckCall := name == "a reader that never returns" // its call still runs: its directory stays
			if ps := procs(); !finds(v.Blocks, "did not finish by its whole-run deadline") || len(ps) != n+1 || !exited(ps[n]) || stuckCall && len(left) != 1 {
				t.Errorf("%s: %q, %d children, left %d", name, whats(v.Blocks), len(ps)-n, len(left))
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
	for name, c := range map[string]struct {
		mutate func(results string)
		why    string
	}{
		"another release": {func(r string) { _ = os.WriteFile(filepath.Join(r, "version"), []byte("go1.0\n"), 0o600) },
			"the toolchain is \"go1.0\", not the guard's " + runtime.Version()},
		"export data not copied out": {relist(`\t/[^\t]*-d\t`, "\t/etc/passwd\t"), "its export data not copied out"},
		"a listed path elsewhere":    {relist(`\t/[^\t]*/([^/\t]*-d)\t`, "\t/nonexistent/$1\t"), ""},
		"an unreadable list":         {func(r string) { _ = os.WriteFile(filepath.Join(r, "list"), []byte("fmt\n"), 0o600) }, "an unreadable list"},
		// Astra #287 H2 (A): an error beside a valid archive; a package
		// listed twice; (B) an archive not named by its content.
		"an error beside an archive": {relist(`(?m)^fmt\t([^\t]+)\t$`, "fmt\t$1\t\"cache failure\""), "(fmt: cache failure)"},
		"a package listed twice":     {relist(`(?m)^(fmt\t[^\n]*\n)`, "$1$1"), "(fmt: listed twice)"},
		"a corrupted archive": {func(r string) {
			xs, _ := os.ReadDir(filepath.Join(r, "x"))
			f, _ := os.OpenFile(filepath.Join(r, "x", xs[0].Name()), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.Write([]byte{0})
			_ = f.Close()
		}, "an archive not named by its content"},
		// M6: an unexpected output; a list past maxList (Astra's 500 MiB
		// list, just past the bound: long lines, fewer than maxListEntries),
		// refused before it is read; one past maxListEntries; and archives
		// the run already holds, delivered again.
		"an unexpected output": {func(r string) { _ = os.WriteFile(filepath.Join(r, "extra"), nil, 0o600) }, "an unexpected output"},
		"a list past maxList": {func(r string) {
			pad := bytes.Repeat([]byte("example.org/"+strings.Repeat("p", 2000)+"\t\t\n"), int(maxList)/2000)
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			_ = os.WriteFile(filepath.Join(r, "list"), append(b, pad...), 0o600)
		}, "a list over its bound"},
		"a list past maxListEntries": {func(r string) {
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			_ = os.WriteFile(filepath.Join(r, "list"), append(b, bytes.Repeat([]byte("example.org/p\t\t\n"), maxListEntries)...), 0o600)
		}, "a list over its bound"},
		"archives delivered again": {func(r string) {
			b, _ := os.ReadFile(filepath.Join(r, "list"))
			for line := range strings.Lines(string(b)) {
				if e := strings.Split(line, "\t")[1]; e != "" {
					src, _ := os.ReadFile(e)
					_ = os.WriteFile(filepath.Join(r, "x", filepath.Base(e)), src, 0o600)
				}
			}
		}, "an archive the run holds"},
		// Astra #287 r2: #4 an error column present and empty; #7 one
		// record past maxListEntries, the last with no final newline.
		"an empty error listed": {relist(`(?m)^fmt\t([^\t]+)\t$`, "fmt\t$1\t\"\""), "(fmt: an empty error listed)"},
		"maxListEntries+1 records, no final newline": {func(r string) {
			var b strings.Builder
			for i := range maxListEntries + 1 {
				fmt.Fprintf(&b, "%sexample.org/p%d\t\t", map[bool]string{true: "\n"}[i > 0], i)
			}
			_ = os.WriteFile(filepath.Join(r, "list"), []byte(b.String()), 0o600)
		}, "a list over its bound"},
	} {
		box.mutate = c.mutate
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
