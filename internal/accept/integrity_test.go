// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// guardBase adds to baseFiles a TestMain, an example, a root test, an
// external test package, an ordinary test named TestMain, a test file
// under a constraint the guard does not model, a root go.mod requiring
// x/sys (from this repository's module cache) and an in-module package.
var guardBase = map[string]string{
	"pkg/main_test.go":    "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
	"pkg/example_test.go": "package pkg\n\nimport \"fmt\"\n\nfunc ExampleAdd() {\n\tfmt.Println(Add(1, 2))\n\t// Output: 3\n}\n",
	"pkg/ext_test.go":     "package pkg_test\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif 1+2 != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n",
	"other/a_test.go":     "package other\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) { t.Fatal() }\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n",
	"race/r_test.go":      "//go:build !race\n\npackage race\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) { t.Fatal() }\n",
	"go.mod":              "module example.com/m\n\ngo 1.26.0\n\nrequire golang.org/x/sys v0.48.0\n",
	"go.sum": "golang.org/x/sys v0.48.0 h1:bbX/i/6MgT9BVLM9RT1thmxL04yeTAhbEz4SyadbXoo=\n" +
		"golang.org/x/sys v0.48.0/go.mod h1:hNLxWAXmnKAxqDtdwIYC4bM9oQPEecfsnNMuSxOs3og=\n",
	"other/other.go": "package other\n\nvar (\n\tValue int\n\tCount int32\n)\n\nconst Name = \"n\"\n\ntype Config struct{}\n\nfunc Run() {}\n",
	"root_test.go":   "package m\n\nimport \"testing\"\n\nfunc TestRoot(t *testing.T) { t.Fatal() }\n",
	"cmd/x/main.go":  "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(1) }\n",
}

// The forgeries of the #255 reviews: a failing TestAdd reads pass under
// go test -json. linkGo is Astra's (Critical): no recognised exit, no
// marker spelling.
const (
	forgeGo = "package pkg\n\nimport (\n\t\"os\"\n\t\"strings\"\n\t\"syscall\"\n)\n\nfunc init() {\n" +
		"\tif strings.HasSuffix(os.Args[0], \".test\") {\n\t\tm := string(rune(22))\n" +
		"\t\tos.Stdout.WriteString(m + \"=== RUN   TestAdd\\n\" + m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n" +
		"\t\tsyscall.Exit(0)\n\t}\n}\n"
	linkGo = "package pkg\n\nimport (\n\t\"os\"\n\t_ \"unsafe\"\n)\n\n//go:linkname terminate syscall.Exit\nfunc terminate(code int)\n\n" +
		"func init() {\n\tm := string([]byte{22})\n\t_, _ = os.Stdout.WriteString(m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n\tterminate(0)\n}\n"
)

// flagBody is the verification's FLAG: it prints the forged lines and exits
// 0 through flag's own -h handling, touching nothing in S.
const flagBody = "\tfs := flag.NewFlagSet(\"\", flag.ExitOnError)\n\tfs.Usage = func() {}\n\tfs.Output().Write([]byte(\"--- PASS: TestAdd (0.00s)\\nPASS\\n\"))\n\tfs.Parse([]string{\"-h\"})\n"

// perrGo and utilGo are the round-7 verification's F1 and M1 payloads: an
// error and a Stringer whose methods are the FLAG, and another package's
// containers. freshGo is C3's generic.
const (
	perrGo = "package pkg\n\nimport (\n\t\"errors\"\n\t\"flag\"\n\t\"fmt\"\n\t\"path/filepath\"\n\n\t\"example.com/m/util\"\n)\n\n" +
		"type perr struct{}\n\nfunc (perr) Error() string {\n" + flagBody + "\treturn \"\"\n}\n\ntype forger struct{}\n\nfunc (forger) String() string { return perr{}.Error() }\n\n"
	utilGo  = "package util\n\nvar (\n\tCh   = make(chan any, 1)\n\tS    = make([]any, 1)\n\tFOO  any\n\tPF   any = new(any)\n\tCAPS = []any{nil}\n)\n"
	freshGo = "func fresh[P any](*P) *P { return new(P) }\n"
	mkGo    = "func mk(v any) any { return reflect.New(reflect.TypeOf(v).Elem()).Interface() }\n"
	argsGo  = "package pkg\n\nimport \"os\"\n\n"
)

// testLister reads export data with the toolchain the tests run with, and
// its caches (the test sandbox's).
var testListerOnce = sync.OnceValues(func() (Lister, error) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(gobin, "env", "GOCACHE", "GOMODCACHE").Output() // as go resolves them (GOPATH too)
	caches := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || len(caches) != 2 {
		return nil, fmt.Errorf("go env: %v %q", err, out)
	}
	return LocalLister(gobin, []string{"GOCACHE=" + caches[0], "GOMODCACHE=" + caches[1]}, 5*time.Minute, 8<<30), nil
})

func testLister(t testing.TB) Lister {
	l, err := testListerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func newGuardFixture(t *testing.T) fixture {
	return newFixtureWith(t, func(src string) {
		write(t, src, guardBase)
		plain(t, src, "add", "--all")
	})
}

// guarded accepts a patch of the base through the integrity guard.
func (f fixture) guarded(t *testing.T, files map[string]string, gone ...string) Verdict {
	t.Helper()
	v, err := Accept(context.Background(), In{Repo: f.repo, Base: f.base, Patch: f.patch(t, files, gone...), Policy: f.policy,
		Guard: integrity(f.repo, f.base, testLister(t))})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// linear checks that a guard's work, counted by counter, is linear in its
// input, instead of timing it (#226 entry 89: a timed bound flaked beside
// another test binary). run runs the row's guard on its input at size n
// (and its own checks) and returns the input's units; the counter's growth
// across it must be positive, at most per units at size n and at 2n, and
// at 2n at most three times n's: superlinear work fails, whatever the
// host's load.
func linear(t *testing.T, name string, counter *atomic.Int64, per int64, n int, run func(n int) int64) {
	t.Helper()
	count := func(n int) (int64, int64) {
		before := counter.Load()
		units := run(n)
		return counter.Load() - before, units
	}
	c1, u1 := count(n)
	c2, u2 := count(2 * n)
	t.Logf("%s: %d steps for %d units, %d for %d (bound %d per unit)", name, c1, u1, c2, u2, per)
	if c1 <= 0 || c1 > per*u1 || c2 > per*u2 || c2 > 3*c1 {
		t.Errorf("%s: %d steps for %d units, %d for %d; want at most %d per unit, and linear", name, c1, u1, c2, u2, per)
	}
}

func whats(bs []Block) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Path+": "+b.What)
	}
	return out
}

func finds(bs []Block, what string) bool {
	return slices.ContainsFunc(bs, func(b Block) bool { return strings.Contains(b.Path+" "+b.What, what) })
}

type guardCase struct {
	files map[string]string
	gone  []string
	why   string // the finding the row must raise
}

// fake is calc_test.go with its check run on the testing value x, not
// the runner's, and decls added.
func fake(x, decls string) map[string]string {
	src := strings.Replace(baseFiles["pkg/calc_test.go"], "if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}",
		"func(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n\t}("+x+")", 1)
	return map[string]string{"pkg/calc_test.go": strings.Replace(src, "import \"testing\"", "import (\n\t\"reflect\"\n\t\"testing\"\n)", 1) + "\n" + decls}
}

// F (P14b, #226 entry 68; #255 rounds 2 and 3): each way to lose, skip or
// weaken a Go test, or to forge its result, that the reviews of #252 and
// #255 found is refused, failed/test_integrity_violation, with the rule
// that discriminates it. A Python change in a base holding Python is
// refused until P14c.
func TestIntegrityBlocks(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	goRep := func(old, new string) map[string]string {
		return map[string]string{"pkg/calc_test.go": strings.Replace(goT, old, new, 1)}
	}
	for name, c := range map[string]guardCase{
		// Collection (Astra #252 H1; Opus #255 H3; Astra #255 H3, H4).
		"_calc_test.go":         {map[string]string{"pkg/_calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_windows_test.go":  {map[string]string{"pkg/calc_windows_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"calc_amd64_test.go":    {map[string]string{"pkg/calc_amd64_test.go": goT}, []string{"pkg/calc_test.go"}, "linux/arm64 pkg:pkg.TestAdd"},
		"calc.test.go":          {map[string]string{"pkg/calc.test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"into testdata":         {map[string]string{"pkg/testdata/calc_test.go": goT}, []string{"pkg/calc_test.go"}, "no longer collected"},
		"a root test renamed":   {map[string]string{"_root_test.go": guardBase["root_test.go"]}, []string{"root_test.go"}, "no longer collected"},
		"go:build ignore":       {map[string]string{"pkg/calc_test.go": "//go:build ignore\n\n" + goT}, nil, "no longer surely collected"},
		"+build windows":        {map[string]string{"pkg/calc_test.go": "// +build windows\n\n" + goT}, nil, "no longer collected"},
		"go:build integration":  {map[string]string{"pkg/calc_test.go": "//go:build integration\n\n" + goT}, nil, "no longer surely collected"},
		"!goexperiment":         {map[string]string{"pkg/calc_test.go": "//go:build !goexperiment.greenteagc\n\n" + goT}, nil, "no longer surely collected"},
		"package documentation": {goRep("package pkg", "package documentation"), nil, "no longer collected"},
		"moved to amd64 only":   {map[string]string{"other/a_test.go": strings.SplitAfter(guardBase["other/a_test.go"], "t.Fatal() }\n")[0], "other/b_amd64_test.go": "package other\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"}, nil, "linux/arm64 other:other.TestOther"},
		"!race renamed amd64":   {map[string]string{"race/r_amd64_test.go": guardBase["race/r_test.go"]}, []string{"race/r_test.go"}, "linux/arm64 race:race.TestR"},
		"pkg and pkg_test":      {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "_ = 0", 1), "pkg/ext_test.go": strings.Replace(guardBase["pkg/ext_test.go"], "t.Fatal(\"add\")", "t.Fatal(\"add\")\n\t\tt.Error(\"again\")", 1)}, nil, "pkg:pkg.TestAdd"},
		"TestMain(*testing.T)":  {map[string]string{"other/a_test.go": "package other\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { t.Fatal() }\n"}, nil, "other:other.TestMain"},
		"a nested go.mod":       {map[string]string{"pkg/go.mod": "module example.com/pkg\n"}, nil, "go.mod added over tests"},
		"root go.mod ignore":    {map[string]string{"go.mod": guardBase["go.mod"] + "\nignore ./pkg // gone\n"}, nil, "ignore directives changed"},
		// Tests and assertions hidden in comments and strings (Astra #252 H2).
		"go test commented out": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "/*\nfunc TestAdd", 1) + "*/\n"}, nil, "a test deleted"},
		"go test in a string":   {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "func TestAdd", "var _ = `\nfunc TestAdd", 1) + "`\n"}, nil, "a test deleted"},
		"go assert commented":   {goRep("t.Fatal", "// t.Fatal"), nil, "fewer assertions"},
		// Assertion look-alikes (Opus #255 H7, Astra #255 H2).
		"err.Error()":      {goRep("t.Fatal(\"add\")", "_ = err.Error()"), nil, "fewer assertions"},
		"t.Failed()":       {goRep("t.Fatal(\"add\")", "_ = t.Failed()"), nil, "fewer assertions"},
		"a shadowed t":     {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "t := fake{}\n\t\tt.Fatal(\"add\")", 1) + "\ntype fake struct{}\n\nfunc (fake) Fatal(...any) {}\n"}, nil, "fewer assertions"},
		"a no-op check":    {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkNothing(false)", 1) + "\nfunc checkNothing(bool) {}\n"}, nil, "fewer assertions"},
		"a helper given t": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "checkAdd(t)", 1) + "\nfunc checkAdd(t *testing.T) { t.Helper() }\n"}, nil, "fewer assertions"},
		"output not last":  {map[string]string{"pkg/example_test.go": strings.Replace(guardBase["pkg/example_test.go"], "// Output: 3\n", "// Output: 3\n\t_ = 0\n\t// done\n", 1)}, nil, "fewer assertions"},
		// Skips (Opus #255 H2).
		"go skip added":       {goRep("{\n\tif", "{\n\tt.Skip()\n\tif"), nil, "a test skipped"},
		"go skip by value":    {goRep("{\n\tif", "{\n\tskip := t.Skip\n\tskip()\n\tif"), nil, "a test skipped"},
		"go skip in a helper": {map[string]string{"pkg/calc.go": calc + "\nfunc skip(t interface{ Skip(...any) }) { t.Skip() }\n"}, nil, "a skip added outside a test"},
		"MethodByName":        {goRep("{\n\tif", "{\n\treflect.ValueOf(t).MethodByName(\"SkipNow\").Call(nil)\n\tif"), nil, "reflection on a testing value"},
		// Forgery (#226 entry 32; Opus #255 C1; Astra #255 Critical).
		"testing.Testing":           {map[string]string{"pkg/calc.go": "package pkg\n\nimport tt \"testing\"\n\nvar _ = tt.Testing()\n"}, nil, "forge"},
		"testing.Testing a value":   {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"testing\"\n\nvar isT = testing.Testing\n"}, nil, "forge"},
		"0x16 written":              {map[string]string{"pkg/calc.go": calc + "\nconst x = \"\\026\"\n"}, nil, "forge"},
		"forge.go":                  {map[string]string{"pkg/forge.go": forgeGo}, nil, "cannot prove safe (syscall.Exit)"},
		"linkname forgery":          {map[string]string{"pkg/forge.go": linkGo}, nil, "a //go:linkname directive"},
		"an unsafe import":          {map[string]string{"pkg/u.go": "package pkg\n\nimport \"unsafe\"\n\nvar _ = unsafe.Sizeof(0)\n"}, nil, "an import of unsafe"},
		"linkname reached":          {map[string]string{"pkg/forge.go": linkGo}, nil, "a call it cannot resolve (terminate)"},
		"init in a _test.go":        {map[string]string{"pkg/zz_test.go": "package pkg\n\nimport \"os\"\n\nfunc init() { os.Stdout.WriteString(\"PASS\\n\"); os.Exit(0) }\n"}, nil, "cannot resolve (os.Stdout.WriteString)"},
		"split across files":        {map[string]string{"pkg/a.go": "package pkg\n\nfunc init() { quit() }\n", "pkg/b.go": "package pkg\n\nimport \"log\"\n\nfunc quit() { log.Fatal(\"PASS\") }\n"}, nil, "cannot prove safe (log.Fatal)"},
		"init through an interface": {map[string]string{"pkg/a.go": "package pkg\n\ntype q interface{ quit() }\n\nvar Q q\n\nfunc init() { Q.quit() }\n"}, nil, "a call it cannot resolve (Q.quit)"},
		"init through an imported interface": {map[string]string{"pkg/a.go": "package pkg\n\nimport (\n\t\"io\"\n\t\"os\"\n)\n\ntype exiter struct{}\n\n" +
			"func (exiter) Write(p []byte) (int, error) { os.Exit(0); return 0, nil }\n\nvar w io.Writer = exiter{}\n\nfunc init() { w.Write(nil) }\n"}, nil, "a call it cannot resolve (w.Write)"},
		"assembly": {map[string]string{"pkg/exit_amd64.s": "TEXT ·terminate(SB),$0\n\tRET\n"}, nil, "assembly"},
		"a forged init through a changed package": {map[string]string{"pkg/a.go": "package pkg\n\nimport \"example.com/m/util\"\n\nfunc init() { util.Prepare() }\n", "util/u.go": "package util\n\nfunc Prepare() {}\n"}, nil, "cannot prove safe (example.com/m/util.Prepare)"},
		// Mythos #255 round 2: C1's three probes, from init and from code the tests call.
		"os.NewFile and RawSyscall": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"syscall\"\n)\n\nfunc init() {\n" +
			"\tf := os.NewFile(1, \"\")\n\tf.WriteString(\"--- PASS: TestAdd (0.00s)\\nPASS\\n\")\n\tsyscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)\n}\n"}, nil, "cannot prove safe (os.NewFile)"},
		"testing.Main from init": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"testing\"\n\nfunc init() {\n" +
			"\ttesting.Main(func(string, string) (bool, error) { return true, nil }, []testing.InternalTest{{Name: \"TestAdd\", F: func(*testing.T) {}}}, nil, nil)\n}\n"}, nil, "cannot prove safe (testing.Main)"},
		"os.Args from init":   {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"os\"\n\nfunc init() { os.Args = append(os.Args, \"-test.run=^$\") }\n"}, nil, "cannot prove safe (os.Args)"},
		"forging from Add":    {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"syscall\"\n\nfunc Add(a, b int) int {\n\tsyscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)\n\treturn a + b\n}\n"}, nil, "pkg/calc.go: syscall.RawSyscall in Add"},
		"rune(21+1)":          {map[string]string{"pkg/calc.go": calc + "\nvar mark = string(rune(21 + 1))\n"}, nil, "can forge"},
		"a decoy declaration": {map[string]string{"pkg/a_ignore.go": "//go:build ignore\n\npackage pkg\n\nfunc quit() {}\n", "pkg/forge.go": "package pkg\n\nimport \"os\"\n\nfunc init() { quit() }\n\nfunc quit() { os.Exit(0) }\n"}, nil, "declared twice"}, // ignore is unmodelled: both may be built
		"new(testing.T)":      {goRep("if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "func(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n\t}(new(testing.T))"), nil, "a testing value not the runner's"},
		"a testing.T literal": {goRep("t.Fatal(\"add\")", "(&testing.T{}).Fatal(\"add\")"), nil, "a testing value not the runner's"},
		"a function value from init": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"slices\"\n)\n\n" +
			"func cmpForge(a, b int) int { os.Exit(0); return 0 }\n\nfunc init() { slices.SortFunc([]int{2, 1}, cmpForge) }\n"}, nil, "plain data passed to slices.SortFunc"},
		"the universe error from init": {map[string]string{"pkg/forge.go": "package pkg\n\ntype exiter struct{}\n\nfunc (exiter) Error() string { return \"\" }\n\n" +
			"var e error = exiter{}\n\nfunc init() { _ = e.Error() }\n"}, nil, "a call it cannot resolve (e.Error)"},
		"a forging TestMain": {map[string]string{"newpkg/main_test.go": "package newpkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestMain(m *testing.M) {\n\tos.Stdout.WriteString(\"--- PASS: TestOther (0.00s)\\nPASS\\n\")\n\tos.Exit(0)\n}\n"}, nil, "cannot resolve (os.Stdout.WriteString)"},
		"TestMain exits without m.Run": {map[string]string{"pkg/main_test.go": strings.Replace(guardBase["pkg/main_test.go"], "m.Run()", "0", 1)}, nil, "os.Exit in TestMain"},
		"an exit in a _test.go helper": {map[string]string{"pkg/bail_test.go": "package pkg\n\nimport \"os\"\n\nfunc bail() { os.Exit(1) }\n"}, nil, "pkg/bail_test.go: os.Exit in bail"},
		"os.Args assigned by a helper": {map[string]string{"pkg/args_test.go": "package pkg\n\nimport \"os\"\n\nfunc only() { os.Args = os.Args[:1] }\n"}, nil, "os.Args assigned in only"},
		"an exit added":                {map[string]string{"pkg/calc.go": "package pkg\n\nimport xos \"os\"\n\nfunc quit() { xos.Exit(1) }\n"}, nil, "pkg/calc.go: os.Exit in quit"},
		"assembly, untested package":   {map[string]string{"tool/x_amd64.s": "TEXT ·x(SB),$0\n\tRET\n"}, nil, "assembly"},
		"reflect Method(0)":            {map[string]string{"pkg/calc_test.go": strings.Replace(strings.Replace(goT, "import \"testing\"", "import (\n\t\"reflect\"\n\t\"testing\"\n)", 1), "{\n\tif", "{\n\t_ = reflect.ValueOf(t).Method(0)\n\tif", 1)}, nil, "reflection on a testing value"},
		// The lead's verification of round 4: C1 probes A and B.
		"probe A": {map[string]string{"pkg/calc.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"syscall\"\n)\n\nfunc Add(a, b int) int {\n" +
			"\tn := 21\n\tm := string(rune(n + 1))\n\tsyscall.Dup3(1, 0, 0)\n\tos.Stdin.WriteString(m + \"--- PASS: TestAdd (0.00s)\\n\" + m + \"PASS\\n\")\n" +
			"\tsyscall.AllThreadsSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)\n\treturn a - b\n}\n"}, nil, "pkg/calc.go: syscall.AllThreadsSyscall in Add"},
		"probe B": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"os\"\n\n\t\"golang.org/x/sys/unix\"\n)\n\nfunc init() {\n" +
			"\tunix.Dup3(1, 0, 0)\n\tos.Stdin.WriteString(\"--- PASS: TestAdd (0.00s)\\nPASS\\n\")\n\tunix.Exit(0)\n}\n"}, nil, "cannot prove safe (golang.org/x/sys/unix.Dup3)"},
		// H1: testing values not the runner's, in every spelling.
		"(new)(testing.T)": {fake("(new)(testing.T)", ""), nil, "a testing value not the runner's"},
		"new((testing.T))": {fake("new((testing.T))", ""), nil, "a testing value not the runner's"},
		"an alias":         {fake("&f", "type myA = testing.T\n\nvar f myA\n"), nil, "a testing value not the runner's"},
		"a type argument":  {fake("mk[testing.T]()", "func mk[X any]() *X { return new(X) }\n"), nil, "a testing value not the runner's"},
		"an array":         {fake("&a[0]", "var a [1]testing.T\n"), nil, "a testing value not the runner's"},
		"a defined type":   {fake("(*testing.T)(&b)", "type myD testing.T\n\nvar b myD\n"), nil, "a testing value not the runner's"},
		"make":             {fake("&make([]testing.T, 1)[0]", ""), nil, "a testing value not the runner's"},
		"a map":            {fake("func() *testing.T { v := map[int]testing.T{}[0]; return &v }()", ""), nil, "a testing value not the runner's"},
		"a conversion":     {fake("(*testing.T)(p)", "var p *int\n"), nil, "a testing value not the runner's"},
		"reflect.New":      {fake("reflect.New(reflect.TypeOf((*testing.T)(nil)).Elem()).Interface().(*testing.T)", ""), nil, "reflection on a testing value"},
		// H2: decoys only another platform builds.
		"a decoy new":     {map[string]string{"pkg/n_windows.go": "package pkg\n\nimport \"testing\"\n\nfunc new(any) *testing.T { return nil }\n", "pkg/calc_test.go": fake("new(testing.T)", "")["pkg/calc_test.go"]}, nil, "a testing value not the runner's"},
		"a decoy rune":    {map[string]string{"pkg/r_windows.go": "package pkg\n\nfunc rune(int) int32 { return 0 }\n", "pkg/calc.go": calc + "\nvar mark = string(rune(21 + 1))\n"}, nil, "can forge"},
		"a decoy println": {map[string]string{"pkg/p_windows.go": "package pkg\n\nfunc println(...any) {}\n", "pkg/forge.go": "package pkg\n\nfunc init() { println(\"--- PASS: TestAdd (0.00s)\") }\n"}, nil, "cannot prove safe (println)"},
		"a built shadow":  {map[string]string{"pkg/shadow.go": "package pkg\n\nfunc println(...any) {}\n"}, nil, "shadows Go's predeclared"},
		"declared twice":  {map[string]string{"pkg/a.go": "package pkg\n\nfunc quit() {}\n", "pkg/b.go": "package pkg\n\nfunc quit() {}\n"}, nil, "declared twice"},
		// M1, M2, M3: a nested module, a spelled 0x16, implicit dispatch.
		"a nested module": {map[string]string{"svc/go.mod": "module example.com/svc\n", "svc/a.go": "package svc\n\nimport \"example.com/svc/util\"\n\nfunc init() { util.Prepare() }\n",
			"svc/util/u.go": "package util\n\nfunc Prepare() {}\n"}, nil, "cannot prove safe (example.com/svc/util.Prepare)"},
		"a spelled 0x16": {map[string]string{"pkg/calc.go": calc + "\nvar mark, _ = strconv.Unquote(`\"\\x16\"`)\n"}, nil, "can forge"},
		"implicit dispatch": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\ntype forger struct{}\n\n" +
			"func (forger) String() string { os.Stdout.WriteString(\"PASS\\n\"); return \"\" }\n\nfunc init() { _ = fmt.Sprint(forger{}) }\n"}, nil, "plain data passed to fmt.Sprint"},
		// The lead's verification of round 5 (P14b-init): initialization is an allowlist.
		"flag init": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"flag\"\n\nfunc init() {\n\ts := \"--- PASS: TestAdd (0.00s)\\nPASS\\n\"\n\tfs := flag.NewFlagSet(\"\", flag.ExitOnError)\n\tfs.Usage = func() {}\n\tfs.Output().Write([]byte(s))\n\tfs.Parse([]string{\"-h\"})\n}\n"}, nil, "cannot prove safe (flag.NewFlagSet)"},
		"flag TestMain": {map[string]string{"newpkg/main_test.go": "package newpkg\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\n" +
			"func TestMain(m *testing.M) {\n\tfs := flag.NewFlagSet(\"\", flag.ExitOnError)\n\tfs.Parse([]string{\"-h\"})\n}\n"}, nil, "cannot prove safe (flag.NewFlagSet)"},
		"ioutil init": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"flag\"\n\t\"io/ioutil\"\n)\n\n" +
			"func init() {\n\tioutil.WriteFile(\"/dev/stdout\", []byte(\"PASS\\n\"), 0)\n\tflag.CommandLine.Parse([]string{\"-h\"})\n}\n"}, nil, "cannot prove safe (io/ioutil.WriteFile)"},
		"libc init": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"modernc.org/libc\"\n\n" +
			"func init() {\n\ttls := libc.NewTLS()\n\tp, _ := libc.CString(\"PASS\\n\")\n\tlibc.Xwrite(tls, 1, p, 5)\n\tlibc.X_Exit(tls, 0)\n}\n"}, nil, "cannot prove safe (modernc.org/libc.NewTLS)"},
		"runtime/debug init":           {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"runtime/debug\"\n\nfunc init() { debug.WriteHeapDump(1) }\n"}, nil, "cannot prove safe (runtime/debug.WriteHeapDump)"},
		"time.Sleep from init":         {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"time\"\n\nfunc init() { time.Sleep(time.Second) }\n"}, nil, "cannot prove safe (time.Sleep)"},
		"a write to another package":   {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"example.com/m/other\"\n\nfunc init() { other.Value = 1 }\n"}, nil, "cannot prove safe (example.com/m/other.Value)"},
		"a value passed on":            {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/m/other\"\n)\n\nvar s = fmt.Sprint(other.Value)\n"}, nil, "plain data passed to fmt.Sprint"},
		"an unchanged in-module call":  {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"example.com/m/other\"\n\nfunc init() { other.Run() }\n"}, nil, "cannot prove safe (example.com/m/other.Run)"},
		"a method of an unloaded type": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"regexp\"\n\nvar re = regexp.MustCompile(\"x\")\n\nfunc init() { re.MatchString(\"x\") }\n"}, nil, "a call it cannot resolve (re.MatchString)"},
		// C2: files a tool tag, cgo or an experiment may build are analysed.
		"tooltag drop":  {map[string]string{"pkg/forge.go": "//go:build arm64.v8.0 || amd64.v1\n\n" + forgeGo}, nil, "cannot prove safe (syscall.Exit)"},
		"goexperiment":  {map[string]string{"pkg/forge.go": "//go:build goexperiment.regabiargs\n\n" + forgeGo}, nil, "cannot prove safe (syscall.Exit)"},
		"cgo build tag": {map[string]string{"pkg/forge.go": "//go:build cgo\n\n" + forgeGo}, nil, "cannot prove safe (syscall.Exit)"},
		// C3, H2: testing values by inference, or laundered through any.
		"fresh T":          {fake("fresh(t)", "func fresh[P any](*P) *P { return new(P) }\n"), nil, "a testing value not the runner's"},
		"mk(t)":            {fake("mk(t)", "func mk[PP interface{ *P }, P any](PP) PP { return PP(new(P)) }\n"), nil, "a testing value not the runner's"},
		"zero(t)":          {fake("func() *testing.T { z := zero(t); return &z }()", "func zero[P any](*P) P { var p P; return p }\n"), nil, "a testing value not the runner's"},
		"laundered by any": {fake("func() *testing.T { x := any(t); return reflect.New(reflect.TypeOf(x).Elem()).Interface().(*testing.T) }()", ""), nil, "a testing value not the runner's"},
		// H1: a path holding ": " keeps its block.
		"a path with a colon": {map[string]string{"pkg/x: y.go": "package pkg\n\nimport \"syscall\"\n\nfunc quit() { syscall.Exit(0) }\n"}, nil, "pkg/x: y.go: syscall.Exit in quit"},
		// H3: promoted methods, from init in another package.
		"a promoted method": {map[string]string{"tool/f.go": "package tool\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\ntype inner struct{}\n\n" +
			"func (inner) String() string { os.Stdout.WriteString(\"PASS\\n\"); return \"\" }\n\ntype W struct{ inner }\n\nfunc init() { _ = fmt.Sprint(W{}) }\n"}, nil, "plain data passed to fmt.Sprint"},
		// The lead's verification of round 6 (C1): callbacks and methods an inert function runs.
		"P1 range over a literal": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"flag\"\n\nfunc init() {\n\tfor range func(func() bool) {\n" + flagBody + "\t} {\n\t}\n}\n"}, nil, "cannot prove safe (flag.NewFlagSet)"},
		"P3 strings.Map":          {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"flag\"\n\t\"strings\"\n)\n\nvar f = func(r rune) rune {\n" + flagBody + "\treturn r\n}\n\nfunc init() { _ = strings.Map(f, \"a\") }\n"}, nil, "plain data passed to strings.Map"},
		"P7 fmt.Sprint of any":    {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"flag\"\n\t\"fmt\"\n)\n\ntype forger struct{}\n\nfunc (forger) String() string {\n" + flagBody + "\treturn \"\"\n}\n\nvar v any = forger{}\n\nfunc init() { _ = fmt.Sprint(v) }\n"}, nil, "plain data passed to fmt.Sprint"},
		"P9 panic":                {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"flag\"\n\ntype perr struct{}\n\nfunc (perr) Error() string {\n" + flagBody + "\treturn \"\"\n}\n\nfunc init() { panic(perr{}) }\n"}, nil, "plain data passed to panic"},
		"P16 &util.V": {map[string]string{"util/u.go": "package util\n\nimport \"flag\"\n\ntype T struct{}\n\nfunc (*T) String() string {\n" + flagBody + "\treturn \"\"\n}\n\nvar V T\n",
			"pkg/forge.go": "package pkg\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/m/util\"\n)\n\nfunc init() { _ = fmt.Sprint(&util.V) }\n"}, nil, "plain data passed to fmt.Sprint"},
		"an ALL_CAPS variable": {map[string]string{"util/u.go": "package util\n\nimport \"flag\"\n\ntype T struct{}\n\nfunc (*T) String() string {\n" + flagBody + "\treturn \"\"\n}\n\nvar FORGER T\n",
			"pkg/forge.go": "package pkg\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/m/util\"\n)\n\nfunc init() { _ = fmt.Sprint(&util.FORGER) }\n"}, nil, "plain data passed to fmt.Sprint"},
		"P18 a TestMain callback": {map[string]string{"pkg/main_test.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"strings\"\n\t\"testing\"\n)\n\nvar cb = func(r rune) rune { return r }\n\n" +
			"func TestMain(m *testing.M) {\n\t_ = strings.Map(cb, \"a\")\n\tos.Exit(m.Run())\n}\n"}, nil, "plain data passed to strings.Map"},
		"a range over a stored function":  {map[string]string{"pkg/forge.go": "package pkg\n\nvar seq = func(yield func() bool) {}\n\nfunc init() {\n\tfor range seq {\n\t}\n}\n"}, nil, "a range it cannot follow (seq)"},
		"a local read of another package": {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"example.com/m/util\"\n\nfunc init() {\n\tv := util.V\n\t_ = v\n}\n"}, nil, "plain data read (util.V)"},
		"an own function passed on":       {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"flag\"\n\nfunc helper() {\n" + flagBody + "}\n\nfunc apply(f func()) {}\n\nfunc init() { apply(helper) }\n"}, nil, "cannot prove safe (flag.NewFlagSet)"},
		"an unresolvable testing type":    {fake("func() *testing.T { var x any = t; return x.(*nope.T) }()", ""), nil, "unverifiable"}, // with real types, a type error
		"a stored sort.Slice callback":    {map[string]string{"pkg/forge.go": "package pkg\n\nimport \"sort\"\n\nvar less = func(i, j int) bool { return false }\n\nfunc init() {\n\txs := []int{2, 1}\n\tsort.Slice(xs, less)\n}\n"}, nil, "plain data passed to sort.Slice"},
		"a stored WalkDir callback":       {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"io/fs\"\n\t\"path/filepath\"\n)\n\nvar walk = func(string, fs.DirEntry, error) error { return nil }\n\nfunc init() { filepath.WalkDir(\".\", walk) }\n"}, nil, "plain data passed to filepath.WalkDir"},
		// H1: an alias in another file cannot rename testing; H2: inference through a copy or a package var.
		"a poisoned import": {map[string]string{"pkg/calc_test.go": fake("func() *testing.T { x := any(t); return reflect.New(reflect.TypeOf(x).Elem()).Interface().(*testing.T) }()", "")["pkg/calc_test.go"],
			"pkg/zz.go": "package pkg\n\nimport testing \"example.com/m/util\"\n\nvar _ = testing.V\n"}, nil, "unverifiable"}, // with real types, imports not loaded
		"fresh(u), a local copy":  {fake("func() *testing.T { u := t; return fresh(u) }()", "func fresh[P any](*P) *P { return new(P) }\n"), nil, "a testing value not the runner's"},
		"fresh(u), a package var": {fake("fresh(u)", "var u *testing.T\n\nfunc fresh[P any](*P) *P { return new(P) }\n"), nil, "a testing value not the runner's"},
		// The lead's verification of round 7 (F1, M1): a pointer into, or a write through, another package's variable.
		"F1 &errors.ErrUnsupported":      {map[string]string{"pkg/forge.go": perrGo + "func init() {\n\tp := &errors.ErrUnsupported\n\t*p = perr{}\n\t_ = fmt.Sprint(errors.ErrUnsupported)\n}\n"}, nil, "variable addressed (&errors.ErrUnsupported)"},
		"F1 set(&errors.ErrUnsupported)": {map[string]string{"pkg/forge.go": perrGo + "func set(p *error) { *p = perr{} }\n\nfunc init() {\n\tset(&errors.ErrUnsupported)\n\t_ = fmt.Sprint(errors.ErrUnsupported)\n}\n"}, nil, "variable addressed (&errors.ErrUnsupported)"},
		"util.S to own code":             {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func fill(s []any) { s[0] = forger{} }\n\nfunc init() { fill(util.S) }\n"}, nil, "variable passed to fill"},
		"F1 flag.Usage and os.Args": {map[string]string{"pkg/forge.go": "package pkg\n\nimport (\n\t\"flag\"\n\t\"os\"\n)\n\nfunc setU(p *func()) {\n\t*p = func() {\n" + flagBody + "\t}\n}\n\n" +
			"func setA(p *[]string) { copy((*p)[1:], []string{\"-h\"}) }\n\nfunc init() {\n\tsetU(&flag.Usage)\n\tsetA(&os.Args)\n}\n"}, nil, "pkg/forge.go: os.Args assigned in init"},
		"F1 filepath.SkipDir":    {map[string]string{"pkg/forge.go": perrGo + "func init() {\n\tp := &filepath.SkipDir\n\t*p = perr{}\n\t_ = errors.Is(filepath.SkipDir, filepath.SkipAll)\n}\n"}, nil, "plain data passed to errors.Is"},
		"F1 in the hook":         {map[string]string{"pkg/main_test.go": "package pkg\n\nimport (\n\t\"errors\"\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tp := &errors.ErrUnsupported\n\t*p = nil\n\tos.Exit(m.Run())\n}\n"}, nil, "variable addressed (&errors.ErrUnsupported)"},
		"F1 untested, &util.FOO": {map[string]string{"util/u.go": utilGo, "tool/f.go": "package tool\n\nimport \"example.com/m/util\"\n\nfunc init() {\n\tp := &util.FOO\n\t*p = 1\n}\n"}, nil, "variable addressed (&util.FOO)"},
		"M1 a send":              {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() { util.Ch <- forger{} }\n"}, nil, "plain data sent to util.Ch"},
		"M1 append into util":    {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() { _ = append(util.S[:0], forger{}) }\n"}, nil, "a write it cannot prove safe (example.com/m/util.S)"},
		"M1 copy into util":      {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() { copy(util.S, []any{forger{}}) }\n"}, nil, "a write it cannot prove safe (example.com/m/util.S)"},
		"os.Args copied over":    {map[string]string{"pkg/args.go": "package pkg\n\nimport \"os\"\n\nfunc reset() { copy(os.Args[1:], []string{\"-h\"}) }\n"}, nil, "pkg/args.go: os.Args assigned in reset"},
		"&os.Args kept":          {map[string]string{"pkg/args.go": "package pkg\n\nimport \"os\"\n\nfunc keep() *[]string { return &os.Args }\n"}, nil, "pkg/args.go: os.Args assigned in keep"},
		"os.Args to own code":    {map[string]string{"pkg/args.go": "package pkg\n\nimport \"os\"\n\nfunc Main() { run(os.Args[1:]) }\n\nfunc run([]string) {}\n"}, nil, "pkg/args.go: os.Args assigned in Main"},
		// F2: an explicit instantiation, a generic not called, an alias of *testing.T.
		"fresh2[int](t)":         {fake("fresh2[int](t)", "func fresh2[Q any, P any](*P) *P { return new(P) }\n"), nil, "a testing value not the runner's"},
		"fresh[testing.T](t)":    {fake("fresh[testing.T](t)", freshGo), nil, "a testing value not the runner's"},
		"a generic assigned":     {fake("func() *testing.T { var f func(*testing.T) *testing.T = fresh; return f(t) }()", freshGo), nil, "a testing value not the runner's"},
		"a generic passed":       {fake("apply(fresh, t)", freshGo+"\nfunc apply(f func(*testing.T) *testing.T, t *testing.T) *testing.T { return f(t) }\n"), nil, "a testing value not the runner's"},
		"an alias asserted":      {fake("reflect.New(reflect.TypeOf(TP(nil)).Elem()).Interface().(TP)", "type TP = *testing.T\n"), nil, "a testing value not the runner's"},
		"an alias in a case":     {fake("func() *testing.T {\n\t\tvar x any = t\n\t\tswitch y := x.(type) {\n\t\tcase TP:\n\t\t\treturn y\n\t\t}\n\t\treturn nil\n\t}()", "type TP = *testing.T\n"), nil, "a testing value not the runner's"},
		"a variable of an alias": {fake("func() *testing.T {\n\t\tvar x TP\n\t\treturn fresh(x)\n\t}()", "type TP = *testing.T\n\n"+freshGo), nil, "a testing value not the runner's"},
		// The lead's verification of round 8 (C1, H1, M1): conversions, assertions and returns; aliases anywhere.
		"C1 a converted os.Args in init": {map[string]string{"pkg/forge.go": argsGo + "func init() { copy([]string(os.Args)[1:], []string{\"-h\"}) }\n"}, nil, "a write it cannot prove safe (os.Args)"},
		"C1 a converted os.Args in the hook": {map[string]string{"pkg/main_test.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestMain(m *testing.M) {\n\tcopy([]string(os.Args)[1:], []string{\"-h\"})\n\tos.Exit(m.Run())\n}\n"}, nil, "a write it cannot prove safe (os.Args)"},
		"C1 append([]string(os.Args)[:1])": {map[string]string{"pkg/forge.go": argsGo + "func init() { _ = append([]string(os.Args)[:1], \"-h\") }\n"}, nil, "a write it cannot prove safe (os.Args)"},
		"C1 &[]string(os.Args)[1]":         {map[string]string{"pkg/forge.go": argsGo + "func init() {\n\tp := &[]string(os.Args)[1]\n\t*p = \"-h\"\n}\n"}, nil, "variable addressed (&[]string(os.Args)[1])"},
		"C1 set([]string(os.Args))":        {map[string]string{"pkg/forge.go": argsGo + "func set(a []string) { a[1] = \"-h\" }\n\nfunc init() { set([]string(os.Args)) }\n"}, nil, "variable passed to set"},
		"C1 args() returns os.Args":        {map[string]string{"pkg/forge.go": argsGo + "func args() []string { return os.Args }\n\nfunc init() { copy(args()[1:], []string{\"-h\"}) }\n"}, nil, "plain data returned (os.Args)"},
		"C1 a converted os.Args local":     {map[string]string{"pkg/forge.go": argsGo + "func init() {\n\ta := []string(os.Args)\n\ta[1] = \"-h\"\n}\n"}, nil, "plain data read ([]string(os.Args))"},
		"C1 a converted os.Args anywhere":  {map[string]string{"pkg/args.go": argsGo + "func reset() { copy([]string(os.Args), []string{\"x\", \"-h\"}) }\n"}, nil, "pkg/args.go: os.Args assigned in reset"},
		"C1 a conversion read":             {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "type holder struct{ s []any }\n\nfunc init() {\n\th := holder{[]any(util.S)}\n\th.s[0] = forger{}\n}\n"}, nil, "plain data converted ([]any(util.S))"},
		"M1 copy([]any(util.S))":           {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() { copy([]any(util.S), []any{forger{}}) }\n"}, nil, "a write it cannot prove safe (example.com/m/util.S)"},
		"M1 *util.PF.(*any)":               {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() { *util.PF.(*any) = forger{} }\n"}, nil, "a write it cannot prove safe (example.com/m/util.PF)"},
		"M1 get()[0]":                      {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func get() []any { return util.S }\n\nfunc init() { get()[0] = forger{} }\n"}, nil, "plain data returned (util.S)"},
		"M1 v := util.CAPS":                {map[string]string{"util/u.go": utilGo, "pkg/forge.go": perrGo + "func init() {\n\tv := util.CAPS\n\tv[0] = forger{}\n}\n"}, nil, "plain data read (util.CAPS)"},
		"H1 a generic alias":               {fake("mk(t).(TG[int])", mkGo+"\ntype TG[P any] = *testing.T\n"), nil, "a testing value not the runner's"},
		"H1 another package's alias": {map[string]string{"util/u.go": "package util\n\nimport \"testing\"\n\ntype TP = *testing.T\n",
			"pkg/calc_test.go": "package pkg\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n\n\t\"example.com/m/util\"\n)\n\nfunc TestAdd(t *testing.T) {\n\tfunc(t *testing.T) {\n\t\tif Add(1, 2) != 3 {\n\t\t\tt.Error(\"add\")\n\t\t}\n" +
				"\t}(reflect.New(reflect.TypeOf(util.TP(nil)).Elem()).Interface().(util.TP))\n}\n"}, nil, "a testing value not the runner's"},
		// Read bounds fail closed for every Go file (Opus #255 H1).
		"go test not parsed":  {map[string]string{"pkg/calc_test.go": goT + "func {\n"}, nil, "unverifiable"},
		"a helper not parsed": {map[string]string{"pkg/calc.go": calc + "func {\n"}, nil, "unverifiable"},
		"a large helper":      {map[string]string{"pkg/big.go": "package pkg\n\n//" + strings.Repeat("x", 5<<20) + "\n"}, nil, "unverifiable"},
		// Deletions.
		"test removed":      {map[string]string{"pkg/calc_test.go": "package pkg\n"}, nil, "a test deleted"},
		"test file deleted": {nil, []string{"pkg/calc_test.go"}, "a test deleted"},
		// Python until P14c (Opus and Astra #255).
		"python helper":     {map[string]string{"money.py": "def add(a, b):\n    return 0\n"}, nil, "python integrity not verified (P14c)"},
		"python test gone":  {nil, []string{"tests/test_money.py"}, "P14c"},
		"a nested conftest": {map[string]string{"tests/conftest.py": "collect_ignore = ['test_money.py']\n"}, nil, "P14c"},
		"pytest.toml":       {map[string]string{"pytest.toml": "[pytest]\naddopts = ['--ignore=tests']\n"}, nil, "P14c"},
		"conda-meta":        {map[string]string{"tests/conda-meta/history": "x\n"}, nil, "P14c"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, c.why) {
			t.Errorf("%s: %q, want test_integrity_violation (%s)", name, whats(v.Blocks), c.why)
		}
	}
	// H1: aliases already in the base; only their use is new.
	a := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"pkg/zz_alias_test.go": "package pkg\n\nimport \"testing\"\n\ntype TP = *testing.T\n\ntype TG[P any] = *testing.T\n"})
		plain(t, src, "add", "--all")
	})
	for name, x := range map[string]string{"mk(t).(TG[int])": "mk(t).(TG[int])", "case TG[int]": "func() *testing.T {\n\t\tswitch y := mk(t).(type) {\n\t\tcase TG[int]:\n\t\t\treturn y\n\t\t}\n\t\treturn nil\n\t}()",
		"var x TP": "func() *testing.T {\n\t\tvar x TP\n\t\treturn fresh(x)\n\t}()"} {
		if v := a.guarded(t, fake(x, mkGo+freshGo)); !finds(v.Blocks, "a testing value not the runner's") {
			t.Errorf("a base alias, %s: %q", name, whats(v.Blocks))
		}
	}
	// Astra #255 H7: a base whose only Python tests are named by a custom
	// pattern (python_files = check_*.py) is a base holding Python.
	g := newFixtureWith(t, func(src string) {
		plain(t, src, "mv", "tests/test_money.py", "tests/check_money.py")
		write(t, src, map[string]string{"pytest.ini": "[pytest]\npython_files = check_*.py\n"})
		plain(t, src, "add", "--all")
	})
	if v := g.guarded(t, map[string]string{"tests/check_money.py": "def test_add():\n    pass\n"}); !finds(v.Blocks, "P14c") {
		t.Errorf("custom python_files: %q", whats(v.Blocks))
	}
}

// The controls: ordinary changes the guard accepts with no warning, and
// the heuristics it warns the reviewer of (Astra #252 M7, #255 H4).
func TestIntegrityAcceptsAndWarns(t *testing.T) {
	f := newGuardFixture(t)
	goT, calc := baseFiles["pkg/calc_test.go"], baseFiles["pkg/calc.go"]
	for name, c := range map[string]guardCase{
		"go test renamed":     {map[string]string{"pkg/add_test.go": goT}, []string{"pkg/calc_test.go"}, ""},
		"go test moved":       {map[string]string{"pkg/calc_test.go": "package pkg\n", "pkg/more_test.go": goT}, nil, ""},
		"go:build linux":      {map[string]string{"pkg/calc_test.go": "//go:build linux && !windows && go1.20\n\n" + goT}, nil, ""},
		"an assertion added":  {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "{\n\tif", "{\n\tif Add(0, 0) != 0 {\n\t\tt.Error(\"zero\")\n\t}\n\tif", 1)}, nil, ""},
		"a helper that fails": {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "if Add(1, 2) != 3 {\n\t\tt.Fatal(\"add\")\n\t}", "checkAdd(t, Add(1, 2))", 1) + "\nfunc checkAdd(t *testing.T, got int) {\n\tif got != 3 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"}, nil, "pkg:pkg.TestAdd an assertion, its condition or a test case changed: review it"}, // M9 (P14d-b): the assertion moved into a helper
		"a subtest":           {map[string]string{"pkg/calc_test.go": strings.Replace(goT, "t.Fatal(\"add\")", "t.Run(\"x\", func(t *testing.T) { t.Fatal(\"add\") })", 1)}, nil, "pkg:pkg.TestAdd an assertion, its condition or a test case changed: review it"},
		"a comment and a doc": {map[string]string{"pkg/calc.go": calc + "\n// t.Skip() in a comment\nvar doc = \"t.Skip()\"\n"}, nil, ""},
		"an ordinary init":    {map[string]string{"pkg/calc.go": calc + "\nvar table = map[string]int{}\n\nfunc init() { table[\"one\"] = Add(0, 1) }\n"}, nil, ""},
		"a new go file":       {map[string]string{"pkg/x.go": "package pkg\n"}, nil, ""},
		"a 0x16 in a comment": {map[string]string{"pkg/calc.go": calc + "\n// test2json's marker is \\x16 (\\026)\n"}, nil, ""},
		"a new TestMain hook": {map[string]string{"race/main_test.go": "package race\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n"}, nil, "TestMain(*testing.M) of race:race changed or deleted"},
		"TestMain deleted":    {nil, []string{"pkg/main_test.go"}, "TestMain(*testing.M) of pkg:pkg changed or deleted"},
		"rune(22)":            {map[string]string{"pkg/calc.go": calc + "\nvar m = rune(0x16)\n"}, nil, "converts 22"},
		// The round-5 verification's false blocks, now accepted clean.
		"x/sys constants":        {map[string]string{"pkg/consts.go": "package pkg\n\nimport \"golang.org/x/sys/unix\"\n\nvar mode = unix.S_IFMT\n"}, nil, "a new import of golang.org/x/sys/unix"}, // P14b-out: a new edge warns
		"a test helper clear":    {map[string]string{"pkg/clear_test.go": "package pkg\n\nfunc clear() {}\n"}, nil, ""},
		"a race pair":            {map[string]string{"pkg/race_test.go": "//go:build race\n\npackage pkg\n\nconst raceOn = true\n", "pkg/norace_test.go": "//go:build !race\n\npackage pkg\n\nconst raceOn = false\n"}, nil, ""},
		"an interface assertion": {map[string]string{"pkg/assert.go": "package pkg\n\nimport (\n\t\"bytes\"\n\t\"io\"\n)\n\nvar _ io.Writer = (*bytes.Buffer)(nil)\n"}, nil, ""},
		"a package variable of another package's value": {map[string]string{"pkg/v.go": "package pkg\n\nimport \"example.com/m/other\"\n\nvar v = other.Value\n"}, nil, ""},
		"a table of another package's names":            {map[string]string{"pkg/cols.go": "package pkg\n\nimport \"example.com/m/other\"\n\nvar cols = map[string]int{other.Name: 1}\n"}, nil, ""},
		"os.Args read":                                  {map[string]string{"pkg/args.go": "package pkg\n\nimport \"os\"\n\nfunc name() string { return base(os.Args[0]) }\n\nfunc base(s string) string { return s }\n\nfunc args() []string { return append([]string{}, os.Args...) }\n"}, nil, "pkg/args.go: os.Args in name"},
		"an inert value to own code":                    {map[string]string{"pkg/tz.go": "package pkg\n\nimport \"time\"\n\nfunc init() { at(time.UTC) }\n\nfunc at(*time.Location) {}\n"}, nil, ""},
		"an inert value passed on":                      {map[string]string{"pkg/tz.go": "package pkg\n\nimport (\n\t\"strings\"\n\t\"time\"\n\t\"unicode\"\n)\n\nvar epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)\n\nvar up = strings.Map(unicode.ToUpper, \"a\")\n\nfunc init() {\n\tloc := time.UTC\n\t_ = loc\n}\n"}, nil, ""},
		"a basic value converted":                       {map[string]string{"pkg/n.go": "package pkg\n\nimport \"example.com/m/other\"\n\nfunc init() {\n\tn := int(other.Count)\n\t_ = n\n}\n"}, nil, ""},
		"an explicit instantiation": {map[string]string{"pkg/view_test.go": "package pkg\n\nimport \"testing\"\n\ntype helper struct{ t *testing.T }\n\nfunc view[T any](*helper) T {\n\tvar v T\n\treturn v\n}\n\n" +
			"func TestView(t *testing.T) {\n\th := &helper{t}\n\tif view[int](h) != 0 {\n\t\tt.Fatal(\"view\")\n\t}\n}\n"}, nil, ""},
		"a blank assignment of t": {map[string]string{"pkg/ptr_test.go": "package pkg\n\nimport \"testing\"\n\nfunc ptr[T any](v T) *T { return &v }\n\n" +
			"func TestPtr(t *testing.T) {\n\t_ = t\n\tif *ptr(1) != 1 {\n\t\tt.Fatal(\"ptr\")\n\t}\n}\n"}, nil, ""},
		"a generic function value": {map[string]string{"tool/g.go": "package tool\n\nfunc id[T any](x T) T { return x }\n\nvar f = id[int]\n"}, nil, ""},
		"a lazy sync.OnceValue":    {map[string]string{"pkg/once.go": "package pkg\n\nimport (\n\t\"os\"\n\t\"sync\"\n)\n\nvar home = sync.OnceValue(func() string { return os.Getenv(\"HOME\") })\n"}, nil, ""},
		"inert initialization": {map[string]string{"pkg/table.go": "package pkg\n\nimport (\n\t\"errors\"\n\t\"path/filepath\"\n\t\"regexp\"\n\t\"strings\"\n\n\t\"example.com/m/other\"\n)\n\n" +
			"var (\n\tre  = regexp.MustCompile(\"x\")\n\terr = errors.New(strings.ToUpper(filepath.Join(\"a\", \"b\")))\n\tcfg = new(other.Config)\n)\n"}, nil, ""},
		"stored functions": {map[string]string{"tool/h.go": "package tool\n\nimport \"fmt\"\n\nfunc helper() { fmt.Println(\"x\") }\n\n" +
			"var handlers = map[string]func(){\"x\": helper, \"y\": func() { fmt.Println(\"y\") }}\n"}, nil, "tool/h.go: fmt.Println in helper"},
		"a TestMain parsing flags": {map[string]string{"race/main_test.go": "package race\n\nimport (\n\t\"flag\"\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tflag.Parse()\n\tos.Exit(m.Run())\n}\n"}, nil, "TestMain(*testing.M) of race:race changed or deleted"},
		"an exit code changed":     {map[string]string{"cmd/x/main.go": "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(0) }\n"}, nil, "cmd/x/main.go: os.Exit(0) in main"},
		"a named 22":               {map[string]string{"pkg/calc.go": calc + "\ntype R int32\n\nvar r = R(22)\n"}, nil, "converts 22"},
		"an exit moved":            {map[string]string{"cmd/x/main.go": "package main\n\nimport \"os\"\n\nfunc main() { run() }\n\nfunc run() { os.Exit(1) }\n"}, nil, "cmd/x/main.go: os.Exit in main"},
		"a dot import, untested":   {map[string]string{"tool/d.go": "package tool\n\nimport . \"os\"\n\nvar _ = Getpid\n"}, nil, "a dot import of os"},
		// cgo, untested: blocks since #271 Opus M2 (a constructor runs before any test), TestIntegrityRound2.
		"a method keyed by its receiver": {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"fmt\"\n\nfunc Add(a, b int) int { return a + b }\n\ntype A struct{}\n\nfunc (A) Print() { fmt.Println() }\n"}, nil, "pkg/calc.go: fmt.Println in A.Print"},
		// Operator decision A: every exit, output or testing-control
		// reference that does not block warns, naming its file and function.
		"an exit, untested package":          {map[string]string{"tool/main.go": "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(2) }\n"}, nil, "tool/main.go: os.Exit in main"},
		"testing control, untested":          {map[string]string{"tool/run.go": "package main\n\nimport \"testing\"\n\nfunc run() { testing.Init() }\n"}, nil, "tool/run.go: testing.Init in run"},
		"output from code tests call":        {map[string]string{"pkg/calc.go": "package pkg\n\nimport \"fmt\"\n\nfunc Add(a, b int) int {\n\tfmt.Println(\"--- PASS: TestAdd (0.00s)\")\n\treturn a + b\n}\n"}, nil, "pkg/calc.go: fmt.Println in Add"},
		"a file descriptor in a test helper": {map[string]string{"pkg/out_test.go": "package pkg\n\nimport \"os\"\n\nfunc out() { os.NewFile(1, \"\").WriteString(\"PASS\\n\") }\n"}, nil, "pkg/out_test.go: os.NewFile in out"},
		"log output redirected":              {map[string]string{"tool/log.go": "package main\n\nimport (\n\t\"log\"\n\t\"os\"\n)\n\nfunc quiet() { log.SetOutput(os.Stdout) }\n"}, nil, "tool/log.go: log.SetOutput in quiet"},
	} {
		v := f.guarded(t, c.files, c.gone...)
		if !v.Accepted() || c.why == "" && len(v.Warnings) > 0 || c.why != "" && !finds(v.Warnings, c.why) {
			t.Errorf("%s: blocks %q, warnings %q; want accepted (warning %q)", name, whats(v.Blocks), whats(v.Warnings), c.why)
		}
	}
	// A base holding no Python: a Python file is not P14c's to verify.
	g := newFixtureWith(t, func(src string) {
		plain(t, src, "rm", "-q", "--cached", "tests/test_money.py", "money.py", "conftest.py")
	})
	if v := g.guarded(t, map[string]string{"tool.py": "x = 1\n"}); !v.Accepted() {
		t.Errorf("python in a base without python: %q", whats(v.Blocks))
	}
}

// countCtx is cancelled at its n-th Err call: a cancellation mid-analysis.
type countCtx struct {
	context.Context
	calls, n atomic.Int64
}

func (c *countCtx) Err() error {
	if c.calls.Add(1) >= c.n.Load() {
		return context.Canceled
	}
	return nil
}

// Opus #255 H1, L2, L3 and Astra #255 H8: the read bounds fail closed for
// Go files, an unread file is reported once, a base list over its bound is
// the base's (not the candidate's unsafe output), constraint evaluation is
// bounded, and analysis stops when cancelled midway.
func TestIntegrityBounds(t *testing.T) {
	f := newGuardFixture(t)
	oldRead, oldFile, oldWork := maxRead, maxFile, maxWork
	t.Cleanup(func() { maxRead, maxFile, maxWork = oldRead, oldFile, oldWork })
	maxRead = 1 << 10 // the base's pkg files are read, then pkg/zz.go is past the budget
	v := f.guarded(t, map[string]string{"pkg/a.go": "package pkg\n\n//" + strings.Repeat("x", 600) + "\n", "pkg/zz.go": "package pkg\n\nvar _ = 1\n"})
	if !finds(v.Blocks, "unverifiable: over the guard's read bound") {
		t.Errorf("past the budget: %q", whats(v.Blocks))
	}
	maxRead, maxFile = oldRead, 40 // pkg/calc.go is over it in both versions
	v = f.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "//" + strings.Repeat("x", 100) + "\n"})
	if n := len(slices.DeleteFunc(slices.Clone(v.Blocks), func(b Block) bool { return b.Path != "pkg/calc.go" })); n != 1 {
		t.Errorf("an unread file: %q, want pkg/calc.go once", whats(v.Blocks))
	}
	maxFile = oldFile

	// Astra's H8 probe, scaled: tautologies over twelve unmodelled tags.
	heavy := strings.Repeat("// +build a !a b !b c !c d !d e !e f !f g !g h !h i !i j !j k !k l !l\n", 3) + "\npackage pkg\n"
	maxWork = 1 << 12
	if v := f.guarded(t, map[string]string{"pkg/h_test.go": heavy}); !finds(v.Blocks, "work bound") {
		t.Errorf("over the work bound: %q", whats(v.Blocks))
	}
	maxWork = oldWork
	oldReach := maxReach
	// L1: a base whose initialization is already past the bound; the
	// candidate's output at its end is unverifiable, not compared away.
	chain := "package pkg\n\nimport \"os\"\n\nfunc init() { g1() }\n\nfunc g1() { g2() }\n\nfunc g2() { g3() }\n\nfunc g3() { _ = os.Getenv(\"X\") }\n"
	deep := newFixtureWith(t, func(src string) {
		write(t, src, map[string]string{"pkg/chain.go": chain})
		plain(t, src, "add", "--all")
	})
	maxReach = 2
	v = deep.guarded(t, map[string]string{"pkg/chain.go": strings.Replace(chain, "_ = os.Getenv(\"X\")", "os.Stdout.WriteString(\"PASS\\n\")", 1)})
	if maxReach = oldReach; !finds(v.Blocks, "past the guard's bound") {
		t.Errorf("past the reach bound: %q", whats(v.Blocks))
	}
	ctx := &countCtx{Context: context.Background()}
	ctx.n.Store(1 << 40)
	in := In{Repo: f.repo, Base: f.base, Patch: f.patch(t, map[string]string{"pkg/h_test.go": heavy}), Policy: f.policy, Guard: integrity(f.repo, f.base, testLister(t))}
	start := time.Now()
	if _, err := Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	total := ctx.calls.Load()
	ctx.calls.Store(0)
	ctx.n.Store(total / 2)
	if _, err := Accept(ctx, in); err == nil || total < 1000 {
		t.Errorf("cancelled midway (%d checks, %v): %v", total, time.Since(start), err)
	}

	h := newFixtureWith(t, func(src string) {
		for i := range 300 {
			write(t, src, map[string]string{fmt.Sprintf("many/%s%d.txt", strings.Repeat("x", 60), i): "x\n"})
		}
		plain(t, src, "add", "--all")
	})
	old := maxPatch
	maxPatch = 8 << 10
	t.Cleanup(func() { maxPatch = old })
	v = h.guarded(t, map[string]string{"pkg/calc.go": baseFiles["pkg/calc.go"] + "\n"})
	if v.Reason() != vocab.ReasonTestIntegrityViolation || !finds(v.Blocks, "file list is over its bound") {
		t.Errorf("a base list over its bound: %s %q", v.Reason(), whats(v.Blocks))
	}
}

// Go's own build rules, per gate platform: 2 surely built, 1 depending on
// tags the guard does not model (tool tags, cgo, race, custom), 0 never
// (#255 H3, round-5 C2); and the collection rules by name.
func TestGoOn(t *testing.T) {
	r := &run{ctx: context.Background()}
	on := func(p, src string) []int {
		if _, err := parser.ParseFile(token.NewFileSet(), filepath.Base(p), src, parser.ParseComments); err != nil {
			t.Fatal(err)
		}
		o, _, _, err := r.goOn(p, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for p, want := range map[string][]int{
		"a_test.go": {2, 2}, "p/a_test.go": {2, 2}, "p/a_linux_test.go": {2, 2}, "p/a_arm64_test.go": {0, 2}, "p/a_darwin_test.go": {0, 0},
		"p/_a_test.go": {0, 0}, "p/.a_test.go": {0, 0}, "p/a.go": {2, 2}, "p/a_windows.go": {0, 0}, "p/_a.go": {0, 0},
	} {
		if got := on(p, "package p\n"); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	for c, want := range map[string][]int{"//go:build linux": {2, 2}, "//go:build !linux": {0, 0}, "//go:build go1.1": {2, 2},
		"//go:build go1.999": {0, 0}, "//go:build cgo": {1, 1}, "//go:build !integration": {1, 1}, "//go:build amd64 || ignore": {2, 1},
		"// +build linux,arm64": {0, 2}, "/*\n//go:build ignore\n*/": {2, 2}, "//go:build !arm64.v8.0": {1, 1}, "//go:build windows": {0, 0},
		"//go:build arm64.v8.0 || amd64.v1": {1, 1}, "//go:build goexperiment.regabiargs": {1, 1}} {
		if got := on("p/a_test.go", c+"\n\npackage p\n"); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", c, got, want)
		}
	}
	for p, want := range map[string]bool{"a_test.go": true, "p/a_test.go": true, "p/testdata/a_test.go": false, "vendor/x/a_test.go": false,
		"_x/a_test.go": false, "p/a.go": false} {
		if goByName(p) != want {
			t.Errorf("%s: collected by name %t", p, !want)
		}
	}
}

// Operator decision A: each reference into S gives a block or a warning
// naming its file and function. From initialization it blocks; in a
// package with tests an exit, exec, system call, descriptor dup, process
// start or runner call blocks and the rest warn; elsewhere all warn.
func TestDecisionASweep(t *testing.T) {
	if sensitive("syscall.ForkExecLock") != 1 { // a name, not the ForkExec family (L3)
		t.Error("syscall.ForkExecLock is not class 1")
	}
	f := newGuardFixture(t)
	for _, c := range []struct {
		imp, expr, q string
		class        int
	}{
		{"syscall", "syscall.Exit(0)", "syscall.Exit", 2},
		{"syscall", "syscall.AllThreadsSyscall(0, 0, 0, 0)", "syscall.AllThreadsSyscall", 2},
		{"syscall", "syscall.Dup3(1, 0, 0)", "syscall.Dup3", 2},
		{"syscall", "syscall.Kill(0, 0)", "syscall.Kill", 1},
		{"syscall", "syscall.ForkLock.Lock()", "syscall.ForkLock", 1}, // ForkExecLock (L3) does not exist on linux: below
		{"golang.org/x/sys/unix", "unix.Exit(0)", "golang.org/x/sys/unix.Exit", 2},
		{"golang.org/x/sys/unix", "unix.Getpid()", "golang.org/x/sys/unix.Getpid", 1},
		{"os", "os.Exit(0)", "os.Exit", 2},
		{"os", "os.StartProcess(\"x\", nil, nil)", "os.StartProcess", 2},
		{"os", "os.Stdin.WriteString(\"x\")", "os.Stdin", 1},
		{"os", "os.Stdout.WriteString(\"x\")", "os.Stdout", 1},
		{"os", "os.Executable()", "os.Executable", 1}, // not os.Exit (L3)
		{"os/exec", "exec.Command(\"true\").Run()", "os/exec.Command", 2},
		{"os/exec", "exec.LookPath(\"x\")", "os/exec.LookPath", 1}, // not a process start
		{"os/exec", "(&exec.Cmd{Path: \"x\"}).Run()", "os/exec.Cmd", 2},
		{"golang.org/x/sys/unix", "unix.FcntlInt(1, unix.F_DUPFD, 0)", "golang.org/x/sys/unix.FcntlInt", 2},
		{"os/signal", "signal.Ignore()", "os/signal.Ignore", 1},
		{"log", "log.Fatal(0)", "log.Fatal", 2},
		{"log", "log.SetPrefix(\"x\")", "log.SetPrefix", 1},
		{"log/slog", "slog.Info(\"x\")", "log/slog.Info", 1},
		{"fmt", "fmt.Println(0)", "fmt.Println", 1},
		{"fmt", "fmt.Fprintln(nil)", "fmt.Fprintln", 1},
		{"testing", "testing.Main(nil, nil, nil, nil)", "testing.Main", 2},
		{"testing", "testing.RunExamples(nil, nil)", "testing.RunExamples", 2},
		{"testing", "testing.Short()", "testing.Short", 1},
		{"runtime", "runtime.Goexit()", "runtime.Goexit", 2},
		{"runtime", "runtime.GC()", "runtime.GC", 1},
		{"reflect", "reflect.TypeOf(0)", "reflect.TypeOf", 1},
		{"", "println(0)", "println", 1},
		{"", "print(0)", "print", 1},
		// P14b-out: S prefix-matched (flag, io/ioutil, net/http/cgi; the os/,
		// log/, runtime/ and testing/ trees), from code the tests call.
		{"flag", "flag.Parse()", "flag.Parse", 1},
		{"flag", "flag.NewFlagSet(\"x\", flag.ExitOnError).Parse([]string{\"-h\"})", "flag.NewFlagSet", 1},
		{"io/ioutil", "ioutil.ReadFile(\"x\")", "io/ioutil.ReadFile", 1},
		{"net/http/cgi", "cgi.Serve(nil)", "net/http/cgi.Serve", 1},
		{"runtime/debug", "debug.SetTraceback(\"all\")", "runtime/debug.SetTraceback", 1},
		{"os/user", "user.Current()", "os/user.Current", 1},
		{"log/syslog", "syslog.New(syslog.LOG_INFO, \"x\")", "log/syslog.New", 1},
		{"testing/fstest", "fstest.TestFS(nil)", "testing/fstest.TestFS", 1},
	} {
		imp := ""
		if c.imp != "" {
			imp = "import \"" + c.imp + "\"\n\n"
		}
		for _, at := range []string{"pkg", "tool", "init"} {
			dir, fn := at, "sweep"
			if at == "init" {
				dir, fn = "pkg", "init"
			}
			p := dir + "/sweep.go"
			v := outGuarded(t, f, map[string]string{p: "package " + dir + "\n\n" + imp + "func " + fn + "() { " + c.expr + " }\n"})
			key := p + ": " + c.q + " in " + fn
			if pkg := c.q[:max(strings.LastIndex(c.q, "."), 0)]; outS(pkg) { // P14b-out-1's: per file and package, Additive
				key = outRefs(p, pkg, "1: "+fn)
			}
			switch {
			case at == "init" && !finds(v.Blocks, "initialization (tests included) reaches"):
				t.Errorf("%s from init: blocks %q", c.expr, whats(v.Blocks))
			case at == "pkg" && c.class == 2 && !finds(v.Blocks, key):
				t.Errorf("%s in a tested package: blocks %q, want %s", c.expr, whats(v.Blocks), key)
			case at != "init" && (at == "tool" || c.class == 1) && (!v.Accepted() || !finds(v.Warnings, key)):
				t.Errorf("%s in %s: blocks %q, warnings %q, want a warning %s", c.expr, at, whats(v.Blocks), whats(v.Warnings), key)
			}
		}
	}
}
