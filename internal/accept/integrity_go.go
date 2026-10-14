// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// platforms are the gate's: a Go test file must stay collected on each.
	platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}}
	// release is the gate toolchain's release tags: the Go the repository
	// pins, which the controller runs too.
	release = build.Default.ReleaseTags
	// otherPlatforms are the GOOS and GOARCH names no gate platform sets.
	otherPlatforms = strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js nacl netbsd openbsd plan9 " +
		"solaris wasip1 windows zos 386 amd64p32 arm armbe arm64be loong64 mips mipsle mips64 mips64le mips64p32 " +
		"mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm gccgo")
	goSkips  = []string{"Skip", "Skipf", "SkipNow"}
	goFails  = []string{"Error", "Fatal", "Fail"}
	goHelper = []string{"assert", "Assert", "require", "Require", "check", "Check", "expect", "Expect"}
	testify  = []string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require"}
	exits    = []string{"os.Exit", "syscall.Exit", "runtime.Goexit"}
	// forged are the spellings of 0x16 (test2json's marker) in Go source.
	forged = []string{"\x16", `\x16`, `\026`, `\u0016`, `\U00000016`}
)

// goSkipDir: go test ./... never walks dir (vendor, testdata, a leading _
// or .).
func goSkipDir(d string) bool {
	return d == "vendor" || d == "testdata" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".")
}

// goByName: p may be a collected Go test file, by its name alone.
func goByName(p string) bool {
	b := path.Base(p)
	return strings.HasSuffix(b, "_test.go") && b[0] != '_' && b[0] != '.' && walked(p, goSkipDir)
}

// goOn is, per gate platform, whether go test collects the test file p
// (see side.on), and its constraint lines: Go's file name rules (go/build:
// a leading _ or ., GOOS and GOARCH suffixes), a directory ./... walks,
// not package documentation, and every //go:build and // +build line
// before the package clause holding for every assignment of the tags the
// guard does not model (cgo, race, goexperiment.*, amd64.v1, custom tags).
func goOn(p string, f *ast.File) (on []int, cons string) {
	on = make([]int, len(platforms))
	var exprs []constraint.Expr
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if c.Pos() < f.Package && (constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text)) {
				x, err := constraint.Parse(c.Text)
				exprs, cons = append(exprs, unparsed(x, err)), cons+c.Text+"\n"
			}
		}
	}
	if !goByName(p) || f.Name.Name == "documentation" {
		return on, cons
	}
	for i, pl := range platforms {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc",
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
		if ok, _ := ctx.MatchFile(path.Dir(p), path.Base(p)); ok {
			on[i] = holds(exprs, pl)
		}
	}
	return on, cons
}

// unparsed: a constraint Go cannot parse is a tag the guard does not model.
func unparsed(x constraint.Expr, err error) constraint.Expr {
	if err != nil {
		return &constraint.TagExpr{Tag: "unparsed"}
	}
	return x
}

// holds is 2 when exprs hold on pl for every assignment of the tags the
// guard does not model, 0 when for none, 1 otherwise (or past 12 tags).
func holds(exprs []constraint.Expr, pl [2]string) int {
	known := func(tag string) (bool, bool) {
		switch {
		case tag == pl[0] || tag == pl[1] || tag == "unix" || tag == "gc" || slices.Contains(release, tag):
			return true, true
		case slices.Contains(otherPlatforms, tag) || strings.HasPrefix(tag, "go1.") ||
			slices.ContainsFunc(platforms, func(q [2]string) bool { return q[0] == tag || q[1] == tag }):
			return false, true // another platform, or a Go after the gate's
		}
		return false, false
	}
	var free []string
	for _, x := range exprs { // Eval visits every tag
		x.Eval(func(tag string) bool {
			if _, ok := known(tag); !ok && !slices.Contains(free, tag) {
				free = append(free, tag)
			}
			return false
		})
	}
	if len(free) > 12 {
		return 1
	}
	all, some := true, false
	for m := 0; m < 1<<len(free); m++ {
		ok := true
		for _, x := range exprs {
			ok = x.Eval(func(tag string) bool {
				v, k := known(tag)
				return k && v || !k && m>>slices.Index(free, tag)&1 == 1
			}) && ok
		}
		all, some = all && ok, some || ok
	}
	switch {
	case all:
		return 2
	case some:
		return 1
	}
	return 0
}

// goTest: name is a function go test runs (TestXxx, FuzzXxx, ExampleXxx).
func goTest(name string) bool {
	for _, pre := range []string{"Test", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, pre); ok {
			r, _ := utf8.DecodeRuneInString(rest)
			return rest == "" || !unicode.IsLower(r)
		}
	}
	return false
}

// goFile resolves a file's imports: local name to path, and dot imports.
type goFile struct {
	imp  map[string]string
	dots []string
}

// quals are what n may name as "importpath.Name": a selector on an import,
// or an identifier a dot import provides.
func (g goFile) quals(n ast.Node) []string {
	switch x := n.(type) {
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && g.imp[id.Name] != "" {
			return []string{g.imp[id.Name] + "." + x.Sel.Name}
		}
	case *ast.Ident:
		var out []string
		for _, d := range g.dots {
			out = append(out, d+"."+x.Name)
		}
		return out
	}
	return nil
}

func (g goFile) is(n ast.Node, names ...string) bool {
	return slices.ContainsFunc(g.quals(n), func(q string) bool { return slices.Contains(names, q) })
}

// params updates in, the names bound to a *testing.T, B or F, or a
// testing.TB, by a function's parameters.
func (g goFile) params(ft *ast.FuncType, in map[string]bool) {
	for _, f := range ft.Params.List {
		t := f.Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		for _, n := range f.Names {
			in[n.Name] = g.is(t, "testing.T", "testing.B", "testing.F", "testing.TB")
		}
	}
}

// goCounts are a declaration's references.
type goCounts struct{ asserts, skips, exits, forged, twentyTwos, detect int }

// count tallies n's references. An assertion is Error*, Fatal* or Fail*
// on a test parameter in scope, a testify call (by import path), or a
// helper named for one (assert*, check*, …) that is passed one; a skip,
// any Skip, Skipf or SkipNow selector, called or not.
func (g goFile) count(n ast.Node, params map[string]bool, c *goCounts) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			inner := maps.Clone(params)
			g.params(x.Type, inner)
			g.count(x.Body, inner, c)
			return false
		case *ast.SelectorExpr:
			switch {
			case slices.Contains(goSkips, x.Sel.Name):
				c.skips++
			case g.is(x, "testing.Testing"):
				c.forged++
			case g.is(x, "os.Args", "flag.Lookup"):
				c.detect++
			}
		case *ast.Ident:
			if g.is(x, "testing.Testing") {
				c.forged++
			}
		case *ast.CallExpr:
			c.asserts += g.assertion(x, params)
			if id, ok := x.Fun.(*ast.Ident); ok && len(x.Args) == 1 && slices.Contains([]string{"byte", "rune", "uint8", "int32"}, id.Name) {
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT {
					if v, err := strconv.ParseInt(lit.Value, 0, 64); err == nil && v == 22 {
						c.twentyTwos++
					}
				}
			}
		}
		if g.is(n, exits...) {
			c.exits++
		}
		return true
	})
}

func (g goFile) assertion(call *ast.CallExpr, params map[string]bool) int {
	name, passes := "", slices.ContainsFunc(call.Args, func(a ast.Expr) bool { id, ok := a.(*ast.Ident); return ok && params[id.Name] })
	switch f := call.Fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && params[id.Name] && slices.ContainsFunc(goFails, func(p string) bool { return strings.HasPrefix(f.Sel.Name, p) }) ||
			slices.ContainsFunc(g.quals(f), func(q string) bool { return slices.Contains(testify, q[:strings.LastIndex(q, ".")]) }) {
			return 1
		}
		name = f.Sel.Name
	}
	if passes && slices.ContainsFunc(goHelper, func(p string) bool { return strings.HasPrefix(name, p) }) {
		return 1
	}
	return 0
}

// goFacts reads a Go file: its collection, its tests (in a _test.go file),
// the skips outside them, its TestMain and exiting inits, its exits and
// its ways to forge test2json results. A non-test file that can detect
// test mode (imports testing, reads os.Args or flag.Lookup, or uses
// //go:linkname) and exits at init (in init, a package initializer, or a
// function of the file they reference) can forge them.
func goFacts(p string, src []byte, s *side) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	s.on, s.cons = goOn(p, f)
	fa, test := &s.facts, strings.HasSuffix(p, "_test.go")
	g, detect := goFile{imp: map[string]string{}}, bytes.Count(src, []byte("//go:linkname"))
	for _, is := range f.Imports {
		ip, _ := strconv.Unquote(is.Path.Value)
		switch name := path.Base(ip); {
		case is.Name != nil && is.Name.Name == ".":
			g.dots = append(g.dots, ip)
		case is.Name != nil:
			g.imp[is.Name.Name] = ip
		default:
			g.imp[name] = ip
		}
		if ip == "testing" && !test {
			detect++
		}
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	funcs, inits := map[string]ast.Node{}, []ast.Node(nil)
	for _, d := range f.Decls {
		var c goCounts
		params, name := map[string]bool{}, ""
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			funcs[fn.Name.Name] = fn // methods too: a selector's name is an identifier
			if g.params(fn.Type, params); fn.Recv == nil {
				name = fn.Name.Name
			}
		}
		g.count(d, params, &c)
		fa.forged, fa.exits, fa.twentyTwos, detect = fa.forged+c.forged, fa.exits+c.exits, fa.twentyTwos+c.twentyTwos, detect+c.detect
		if _, gen := d.(*ast.GenDecl); gen || name == "init" {
			inits = append(inits, d)
		}
		switch {
		case test && name == "TestMain":
			fa.warn["TestMain"] = text(d)
		case test && goTest(name):
			fa.tests[path.Dir(p)+":"+name] = &stat{asserts: c.asserts + output(f, d.(*ast.FuncDecl)), skips: c.skips}
			continue
		case name == "init" && c.exits > 0:
			fa.warn["an init calling an exit"] += text(d)
		}
		fa.ambient += c.skips
	}
	for _, m := range forged {
		fa.forged += bytes.Count(src, []byte(m))
	}
	atInit, seen := 0, map[ast.Node]bool{}
	for len(inits) > 0 { // what init and the package initializers reach in this file
		n := inits[0]
		inits = inits[1:]
		var c goCounts
		g.count(n, nil, &c)
		atInit += c.exits
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && funcs[id.Name] != nil && !seen[funcs[id.Name]] {
				seen[funcs[id.Name]], inits = true, append(inits, funcs[id.Name])
			}
			return true
		})
	}
	fa.forge = !test && detect > 0 && atInit > 0
	return nil
}

// output: an example's output comment (its body's last comment), its
// assertion; the comments are sorted, so one search finds it.
func output(f *ast.File, fn *ast.FuncDecl) int {
	i := sort.Search(len(f.Comments), func(i int) bool { return f.Comments[i].Pos() > fn.Body.Rbrace })
	if !strings.HasPrefix(fn.Name.Name, "Example") || i == 0 || f.Comments[i-1].Pos() < fn.Body.Lbrace {
		return 0
	}
	t := strings.ToLower(strings.TrimSpace(f.Comments[i-1].Text()))
	if strings.HasPrefix(t, "output:") || strings.HasPrefix(t, "unordered output:") {
		return 1
	}
	return 0
}

// goIgnores is a root go.mod's ignore directives (Go 1.25: packages
// ./... leaves out), comments dropped.
func goIgnores(b []byte) string {
	var out []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l, _, _ = strings.Cut(l, "//")
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "ignore") || in {
			in, out = strings.HasSuffix(l, "(") || in && l != ")", append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
