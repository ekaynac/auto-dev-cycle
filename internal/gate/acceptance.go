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
	"go/parser"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	errModule     = fmt.Errorf("%w: it holds a go.mod or a go.work", ErrAcceptance)
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
// identifier, a dot-import's name its package declares) fails every
// candidate (found/gate_test_failed), bounded by the rails; the acceptance
// is immutable, so the recovery is a new task.

// AcceptanceTest is a top-level test an acceptance's Go test file
// declares: its directory (the repository path; "." the root) and name.
type AcceptanceTest struct{ Dir, Name string }

// goFile is an active Go file: one go builds on the platform.
type goFile struct {
	path, pkg string
	test      bool
	f         *ast.File
}

// active is overlay's file at p when go builds it on pl (its path, its
// build constraints, its package not documentation, no import of "C":
// cgo is off, gosel.Cgo). strict: a Go test
// file it never runs, or a Go file that does not parse, is refused; a Go
// file go never builds is data.
func active(pl [2]string, p string, src []byte, strict bool) (goFile, bool, error) {
	test := strings.HasSuffix(p, "_test.go")
	switch {
	case strict && test && !gosel.TestFile(p):
		return goFile{}, false, errNeverRun
	case !gosel.Active(p):
		return goFile{}, false, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
	switch {
	case err != nil && strict:
		return goFile{}, false, errParse
	case err != nil:
		return goFile{}, false, nil
	case !gosel.Builds(pl, p, src) || f.Name.Name == gosel.Documentation || gosel.Cgo(f):
		if strict && test {
			return goFile{}, false, errNeverRun
		}
		return goFile{}, false, nil
	}
	return goFile{p, f.Name.Name, test, f}, true, nil
}

// AcceptanceTests are the top-level tests (TestXxx and FuzzXxx, by
// gosel.RunName's rule; TestMain(*testing.M) none) the Go test files of
// overlay declare, sorted by file, which go test ./... runs on pl
// ({GOOS, GOARCH}). It refuses (ErrAcceptance) what go test ./... could
// never run, so it is never expected: files over MaxOverlay in all; a
// go.mod or go.work among them (a module of its own); a Go test file it
// never collects (testdata, vendor, a _ or . directory or name), that
// go/build excludes on pl, or of package documentation; a Go file that
// does not parse; a test, benchmark, fuzz target or example whose
// signature go test refuses; and no test at all (Opus #429 M1). Files
// that are not Go files, and Go files go never builds, are data.
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
		if b := path.Base(p); b == "go.mod" || b == "go.work" {
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
			if malformed(fn) {
				return nil, errSignature // go test's vet refuses its name (Opus #429 r3 N1)
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
	}
	if len(out) == 0 {
		return nil, errNoTest
	}
	return out, nil
}

// malformed: go test's vet (its tests analyzer) refuses fn, a top-level
// function of a test file, and the package never runs: Test, Benchmark or
// Fuzz then a lower-case letter, on that kind's signature; Example then a
// lower-case letter (an unknown identifier, or a malformed name).
func malformed(fn *ast.FuncDecl) bool {
	for pre, kind := range map[string]string{"Test": "T", "Benchmark": "B", "Fuzz": "F", "Example": ""} {
		rest, ok := strings.CutPrefix(fn.Name.Name, pre)
		r, _ := utf8.DecodeRuneInString(rest)
		if ok && rest != "" && unicode.IsLower(r) && (kind == "" || fn.Type.Params != nil && len(fn.Type.Params.List) == 1 && param(fn) == kind) {
			return true
		}
	}
	return false
}

// testKind is what go test makes of a top-level function named name in a
// test file: its parameter's type name (T a test, F a fuzz target, B a
// benchmark, M TestMain's), "" an example; false none.
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
	return "", true // an example
}

// signature is the go command's check of a test function (load/test.go's
// isTestFunc): one parameter *X or *pkg.X, X kind's, no result, no type
// parameter; an example takes and returns nothing. TestMain may take a
// *testing.T instead, and is a test then.
func signature(fn *ast.FuncDecl, kind string) bool {
	t := fn.Type
	if t.TypeParams != nil && len(t.TypeParams.List) > 0 || t.Results != nil && len(t.Results.List) > 0 {
		return false
	}
	if kind == "" {
		return t.Params == nil || len(t.Params.List) == 0
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
// refused. base holds the base's Go files in those directories,
// repository path → bytes; only the files go builds on pl count. A
// directory's package is its non-test files', else its internal test
// files', else its external test files' without _test. The internal test
// package shares its scope with the package's files; the external one
// with the other external test files.
func AcceptanceFits(pl [2]string, overlay, base map[string][]byte) error {
	baseOf, ownOf := map[string][]goFile{}, map[string][]goFile{}
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
		// name may share (Opus #429 r3 M1).
		for _, g := range all {
			for _, name := range importNames(g.f) {
				if other, ok := declared[g.pkg][name]; ok && (own(g.path) || own(other)) {
					return errRedeclared
				}
			}
		}
	}
	return nil
}

// importNames are the names f's imports declare in its file block: each
// one's explicit name (not _ or .), else its path's last element when that
// is an identifier (a vN element is a version: the one before it). An
// import whose package name is not its path's is not seen, nor is a
// dot-import's names (stated residuals).
func importNames(f *ast.File) []string {
	var out []string
	for _, is := range f.Imports {
		if is.Name != nil { // _ and . match no package-scope name (topNames has no _)
			out = append(out, is.Name.Name)
			continue
		}
		p, err := strconv.Unquote(is.Path.Value)
		if err != nil {
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
			} else if t := recvType(x); x.Recv != nil && t != "" && x.Name.Name != "_" {
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

// recvType is the name of fn's receiver's type (T of T, *T, T[P]), "" for
// none.
func recvType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}
