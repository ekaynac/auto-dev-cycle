// SPDX-License-Identifier: Apache-2.0

package gate

// The task's acceptance, as the gate overlays it (#226 entry 190): what go
// test ./... runs of it, by the go command's selection rules (gosel, one
// copy, accept's: Astra #429 M6), and whether it can compile in the base's
// packages it joins. Every refusal is the task's (ErrAcceptance) and one
// of fixed texts (Astra #429 H3): none names a path, a package, an
// identifier or a digest of the acceptance, so adcd's log and a recorded
// detail carry none.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/gosel"
)

// ErrAcceptance: a task's acceptance the gate cannot run as its hidden
// tests: the task's, no retry or profile change fixes it.
var ErrAcceptance = errors.New("gate: the task's acceptance cannot be run")

// MaxOverlay bounds an acceptance's files, in all.
const MaxOverlay = 16 << 20

// The refusals, each a fixed text.
var (
	errTooLarge   = fmt.Errorf("%w: its files are over the overlay's bound", ErrAcceptance)
	errPath       = fmt.Errorf("%w: a file of it is not at a clean path inside the workspace", ErrAcceptance)
	errModule     = fmt.Errorf("%w: it holds a go.mod", ErrAcceptance)
	errNeverRun   = fmt.Errorf("%w: a Go test file of it is one go test ./... never runs (its directory, its name, its build constraints or its package)", ErrAcceptance)
	errParse      = fmt.Errorf("%w: a Go file of it does not parse", ErrAcceptance)
	errSignature  = fmt.Errorf("%w: a test of it has a signature go test refuses", ErrAcceptance)
	errNoTest     = fmt.Errorf("%w: no Go test file of it declares a test go test runs", ErrAcceptance)
	errPackage    = fmt.Errorf("%w: a Go file of it is in another package than its directory's", ErrAcceptance)
	errRedeclared = fmt.Errorf("%w: a Go file of it declares a name its package declares already", ErrAcceptance)
)

// The build context the gate reads acceptance files with drifts from the
// go command's in two ways, each stated (Opus #429 N2):
//   - its release tags and its experiments are adcd's own toolchain's
//     (gosel.Builds), not the pinned image's: they agree while both are the
//     same Go release (go1.27 today);
//   - it assumes the profile's gate command is go test ./... with no -tags
//     or -run: a profile adding -tags x has a //go:build x hidden test
//     refused though it runs, and one with -run may never run what it
//     expects (the candidate's, then).
// The platform's default architecture level is modelled (arm64.v8.0,
// amd64.v1), in positive and negated constraints alike (Astra #429 H1).
//
// The boundary (Opus #429 r3 M2; #226 entry 197): the gate refuses an
// acceptance that the shared rules (gosel) prove go test never runs. Any
// other acceptance that does not compile or vet on its own (an unused
// import, a type error, a printf vet error, an example naming an unknown
// identifier, a malformed Test/Benchmark/Fuzz name (vet's tests analyzer;
// Astra #429 r2 H1), a dot-import's name its package declares, an import name
// the gate cannot be certain of, a method named like a field of its type)
// fails every candidate (found/gate_test_failed), bounded by the rails;
// the acceptance is immutable, so the recovery is a new task. A check that
// guesses is worse than a stated residual (Opus #429 r4).

// AcceptanceTest is a top-level test an acceptance's Go test file
// declares: its directory (the repository path; "." the root) and name.
type AcceptanceTest struct{ Dir, Name string }

// goFile is an active Go file: one go builds on the platform.
type goFile struct {
	path, pkg string
	test      bool
	f         *ast.File
}

// active is overlay's file at p when go builds it on pl, by go's own
// order (Astra #429 r2 H3): its path (gosel.Active), then its name's and
// its constraints' platform (gosel.Builds), then its header (the package
// clause and imports: documentation, or an import of "C" with cgo off,
// gosel.Cgo), and only then its body. strict: a Go test file go never runs
// is refused (errNeverRun), and a file go builds that does not parse is
// refused (errParse); a Go file go never builds, test or helper, is data
// and never parsed beyond what decides it, except that a test file is
// refused instead.
func active(pl [2]string, p string, src []byte, strict bool) (goFile, bool, error) {
	test := strings.HasSuffix(p, "_test.go")
	excluded := func() (goFile, bool, error) {
		if strict && test {
			return goFile{}, false, errNeverRun
		}
		return goFile{}, false, nil
	}
	switch {
	case strict && test && !gosel.TestFile(p):
		return goFile{}, false, errNeverRun
	case !gosel.Active(p):
		return goFile{}, false, nil
	case !gosel.Builds(pl, p, src):
		return excluded()
	}
	head, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
	switch {
	case err != nil && strict:
		return goFile{}, false, errParse
	case err != nil:
		return goFile{}, false, nil
	case head.Name.Name == gosel.Documentation || gosel.Cgo(head):
		return excluded()
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ParseComments|parser.SkipObjectResolution)
	switch {
	case err != nil && strict:
		return goFile{}, false, errParse
	case err != nil:
		return goFile{}, false, nil
	}
	return goFile{p, f.Name.Name, test, f}, true, nil
}

// AcceptanceTests are the top-level tests the Go test files of overlay
// declare, sorted by file, which go test ./... runs on pl ({GOOS,
// GOARCH}): TestXxx and FuzzXxx (gosel.RunName's rule; TestMain(*testing.T)
// a test, TestMain(*testing.M) the hook), and the executable examples the
// go command runs, those with an output comment, empty included, by the
// standard library's go/doc.Examples (Astra #429 r2 H2). It refuses
// (ErrAcceptance) what go test ./... could never run, so it is never
// expected: files over MaxOverlay in all; a go.mod among them (a module of
// its own; a go.work is data, the image's GOWORK=off); a Go test file it
// never collects (testdata, vendor, a _ or . directory or name), that
// go/build excludes on pl, of package documentation or importing "C"; a Go file go builds that does not parse;
// a test, benchmark or fuzz target whose signature the go command refuses
// (load/test.go); and no test at all (Opus #429 M1). Files that are not Go
// files, and Go files go never builds, are data. A name go test's vet
// refuses (Testfoo) is the stated general class, not refused (#226 entry
// 197; Astra #429 r2 H1).
func AcceptanceTests(pl [2]string, overlay map[string][]byte) ([]AcceptanceTest, error) {
	total := 0
	for _, b := range overlay {
		total += len(b)
	}
	if total > MaxOverlay {
		return nil, errTooLarge
	}
	var out []AcceptanceTest
	for _, p := range slices.Sorted(maps.Keys(overlay)) {
		if path.Base(p) == "go.mod" { // a go.work is data: the image's GOWORK=off (Astra #429 r2 differential)
			return nil, errModule
		}
		g, ok, err := active(pl, p, overlay[p], true)
		if err != nil {
			return nil, err
		}
		if !ok || !g.test {
			continue
		}
		for _, d := range g.f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			kind, ok := testKind(fn.Name.Name)
			if !ok {
				continue
			}
			if !signature(fn, kind) {
				return nil, errSignature
			}
			if kind == "M" && param(fn) == "T" {
				kind = "T" // TestMain(*testing.T): a test
			}
			if kind == "T" || kind == "F" {
				out = append(out, AcceptanceTest{Dir: path.Dir(p), Name: fn.Name.Name})
			}
		}
		for _, e := range doc.Examples(g.f) { // the go command's own collection: run only with an output comment
			if e.Output != "" || e.EmptyOutput {
				out = append(out, AcceptanceTest{Dir: path.Dir(p), Name: "Example" + e.Name})
			}
		}
	}
	if len(out) == 0 {
		return nil, errNoTest
	}
	return out, nil
}

// testKind is what the go command makes of a top-level function named name
// in a test file (load/test.go): its parameter's type name (T a test, F a
// fuzz target, B a benchmark, M TestMain's); false none. Examples are
// go/doc's (AcceptanceTests).
func testKind(name string) (string, bool) {
	switch {
	case name == "TestMain":
		return "M", true // or a test, when it takes a *testing.T (signature)
	case strings.HasPrefix(name, "Benchmark"): // the same rule for its suffix
		return "B", gosel.RunName("Test" + strings.TrimPrefix(name, "Benchmark"))
	case !gosel.RunName(name):
		return "", false
	case strings.HasPrefix(name, "Test"):
		return "T", true
	case strings.HasPrefix(name, "Fuzz"):
		return "F", true
	}
	return "", false // an example: go/doc's
}

// signature is the go command's check of a test function (load/test.go's
// isTestFunc and checkTestFunc): one parameter *X or *pkg.X, X kind's, no
// result, no type parameter. TestMain may take a *testing.T instead, and is
// a test then.
func signature(fn *ast.FuncDecl, kind string) bool {
	t := fn.Type
	if t.TypeParams != nil && len(t.TypeParams.List) > 0 || t.Results != nil && len(t.Results.List) > 0 {
		return false
	}
	if t.Params == nil || len(t.Params.List) != 1 || len(t.Params.List[0].Names) > 1 {
		return false
	}
	name := param(fn)
	return name == kind || kind == "M" && name == "T"
}

// param is the type name of fn's one pointer parameter (X of *X or of
// *pkg.X), "" for another.
func param(fn *ast.FuncDecl) string {
	if star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr); ok {
		switch x := star.X.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		}
	}
	return ""
}

// AcceptanceFits refuses (ErrAcceptance) an acceptance with a Go file that
// can never compile against the base, whatever the candidate does (Opus
// #429 N1, Astra H2): every Go file of it go builds on pl, tests and
// helpers alike, combined with the base's in each directory it joins. A
// file whose package clause is not its directory's (a Go test file: nor
// that package's _test form), and a top-level name an acceptance file
// declares that another file of the same package declares too, are
// refused. base holds the base's Go files in those directories and its
// root go.mod, repository path → bytes; only the files go builds on pl count. A
// directory's package is its non-test files', else its internal test
// files', else its external test files' without _test. The internal test
// package shares its scope with the package's files; the external one
// with the other external test files.
func AcceptanceFits(pl [2]string, overlay, base map[string][]byte) error {
	baseOf, ownOf, named := map[string][]goFile{}, map[string][]goFile{}, baseModules(base["go.mod"])
	for _, p := range slices.Sorted(maps.Keys(overlay)) {
		g, ok, err := active(pl, p, overlay[p], true)
		if err != nil {
			return err
		}
		if ok {
			ownOf[path.Dir(p)] = append(ownOf[path.Dir(p)], g)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(base)) {
		if g, ok, _ := active(pl, p, base[p], false); ok { // a base file that does not parse is the base's own failure
			baseOf[path.Dir(p)] = append(baseOf[path.Dir(p)], g)
		}
	}
	for _, d := range slices.Sorted(maps.Keys(ownOf)) {
		all := append(slices.Clone(baseOf[d]), ownOf[d]...)
		pkg := ""
		for _, pick := range []func(goFile) (string, bool){
			func(g goFile) (string, bool) { return g.pkg, !g.test },
			func(g goFile) (string, bool) { return g.pkg, g.test && !strings.HasSuffix(g.pkg, "_test") },
			func(g goFile) (string, bool) { return strings.TrimSuffix(g.pkg, "_test"), g.test },
		} {
			for _, g := range all {
				if name, ok := pick(g); ok && pkg == "" {
					pkg = name
				}
			}
		}
		own := func(p string) bool { return slices.ContainsFunc(ownOf[d], func(g goFile) bool { return g.path == p }) }
		declared := map[string]map[string]string{} // package → name (a method T.M) → its file
		for _, g := range all {
			if own(g.path) && g.pkg != pkg && (!g.test || g.pkg != pkg+"_test") {
				return errPackage
			}
			if declared[g.pkg] == nil {
				declared[g.pkg] = map[string]string{}
			}
			for _, name := range topNames(g.f) {
				if other, ok := declared[g.pkg][name]; ok && (own(g.path) || own(other)) {
					return errRedeclared
				}
				declared[g.pkg][name] = g.path
			}
		}
		// A file's import name is in its file block, which no package-scope
		// name may share (Opus #429 r3 M1); only a certain one counts (B3).
		for _, g := range all {
			for _, name := range importNames(g.f, named) {
				if other, ok := declared[g.pkg][name]; ok && (own(g.path) || own(other)) {
					return errRedeclared
				}
			}
		}
	}
	return nil
}

// importNames are the names f's imports certainly declare in its file
// block (Opus #429 r4 B3: a check that guesses is worse than a stated
// residual): each one's explicit name (not _ or .), and a standard-library
// path's last element (a vN element is a version: the one before it,
// math/rand/v2's rand), which is every importable std package's name. A
// path is the standard library's by module metadata alone, accept's rule:
// its first element has no dot and no module the base's go.mod names
// (named, gosel.Std) is it or a prefix of it, so the go command resolves it from
// its GOROOT only. Every other import is unseen, as is a dot-import's
// names (stated residuals).
func importNames(f *ast.File, named map[string]bool) []string {
	var out []string
	for _, is := range f.Imports {
		if is.Name != nil { // _ and . match no package-scope name (topNames has no _)
			out = append(out, is.Name.Name)
			continue
		}
		p, err := strconv.Unquote(is.Path.Value)
		if err != nil || !gosel.Std(p, named) {
			continue
		}
		elems := strings.Split(p, "/")
		name := elems[len(elems)-1]
		if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
			name = elems[len(elems)-2]
		}
		if token.IsIdentifier(name) {
			out = append(out, name)
		}
	}
	return out
}

// topNames are the package-scope names f declares: its functions (no
// init, no _), types, variables and constants, and its methods as T.M
// (their receiver's type: Opus #429 r3 M1).
func topNames(f *ast.File) []string {
	var out []string
	add := func(id *ast.Ident) {
		if id.Name != "_" {
			out = append(out, id.Name)
		}
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			if x.Recv == nil && x.Name.Name != "init" {
				add(x.Name)
			} else if t := gosel.Recv(x); t != "" && x.Name.Name != "_" {
				out = append(out, t+"."+x.Name.Name)
			}
		case *ast.GenDecl:
			for _, sp := range x.Specs {
				switch y := sp.(type) {
				case *ast.TypeSpec:
					add(y.Name)
				case *ast.ValueSpec:
					for _, id := range y.Names {
						add(id)
					}
				}
			}
		}
	}
	return out
}

// baseModules are the module paths the base's go.mod (gomod) names
// (gosel.Modules). The failure direction is explicit (Opus #429 r5 C1): no
// go.mod, one that does not parse, or one with no module line names every
// path ({"": true}), so no path reads as the standard library's and
// importNames counts explicit names only.
func baseModules(gomod []byte) map[string]bool {
	named, module, ok := gosel.Modules(gomod)
	if gomod == nil || !ok || module == "" {
		return map[string]bool{"": true}
	}
	return named
}
