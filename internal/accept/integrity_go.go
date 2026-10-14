// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
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
	platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}} // the gate's
	release   = build.Default.ReleaseTags                           // the pinned Go's, which the gate runs too
	// otherPlatforms are the GOOS and GOARCH names no gate platform sets.
	otherPlatforms = strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js nacl netbsd openbsd plan9 " +
		"solaris wasip1 windows zos 386 amd64p32 arm armbe arm64be loong64 mips mipsle mips64 mips64le mips64p32 " +
		"mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm gccgo")
	goSkips    = []string{"Skip", "Skipf", "SkipNow"}
	goFails    = []string{"Error", "Errorf", "Fatal", "Fatalf", "Fail", "FailNow"} // exactly the failure-setting methods
	control    = append(slices.Clone(goSkips), append(slices.Clone(goFails), "Run", "Helper", "Cleanup")...)
	testify    = []string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require"}
	testTypes  = []string{"testing.T", "testing.B", "testing.F", "testing.TB", "testing.M"}
	valueTypes = []string{"testing.T", "testing.B", "testing.F", "testing.M"} // only ever behind a pointer
	osRead     = []string{"Getenv", "LookupEnv", "Environ", "Getwd", "Stat", "Open", "ReadFile", "ReadDir", "PathSeparator"}
	// sPkgs (and golang.org/x/sys/...) reach the process, its output or testing: S.
	sPkgs   = []string{"syscall", "os", "os/exec", "os/signal", "log", "log/slog", "fmt", "testing", "reflect", "unsafe", "runtime", "plugin"}
	spelled = []string{"\x16", `\x16`, `\026`, `\u0016`, `\U00000016`} // test2json's marker
)

func sPkg(p string) bool {
	return slices.Contains(sPkgs, p) || strings.HasPrefix(p, "golang.org/x/sys/")
}

// sensitive classifies q ("importpath.Name", a builtin, "os.Args assigned"):
// 2 if it can exit, exec, start a process, make a system call, dup a
// descriptor or drive testing's runner; 1 for any other reference into S
// (os's reads excepted); 0 otherwise. Names match exactly, or as families.
func sensitive(q string) int {
	i := strings.LastIndex(q, ".")
	pkg, name := q[:max(i, 0)], q[i+1:]
	is := func(names ...string) int {
		return b2i(slices.ContainsFunc(names, func(n string) bool {
			return name == n || strings.HasSuffix(n, "*") && strings.HasPrefix(name, n[:len(n)-1])
		}))
	}
	switch {
	case q == "print" || q == "println":
		return 1
	case q == "os.Args assigned":
		return 2
	case pkg == "syscall" || strings.HasPrefix(pkg, "golang.org/x/sys/"):
		return 1 + is("Exit", "Exec*", "Syscall*", "RawSyscall*", "AllThreadsSyscall*", "Dup*", "ForkExec", "StartProcess")
	case pkg == "os" && (slices.Contains(osRead, name) || strings.HasPrefix(name, "Err")):
		return 0
	case pkg == "os":
		return 1 + is("Exit", "StartProcess")
	case pkg == "os/exec" || pkg == "plugin":
		return 2
	case pkg == "log" || pkg == "log/slog":
		return 1 + is("Fatal*", "Panic*")
	case pkg == "fmt":
		return is("Print*", "Fprint*")
	case pkg == "runtime":
		return 1 + is("Goexit")
	case pkg == "testing" && slices.Contains(testTypes, q):
		return 0 // the testing-value rule
	case pkg == "testing":
		return 1 + is("Main*", "Run*", "Init", "Benchmark*")
	}
	return b2i(sPkg(pkg))
}

// goSkipDir: go test ./... never walks dir.
func goSkipDir(d string) bool {
	return d == "vendor" || d == "testdata" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".")
}

// goByName: p may be a collected Go test file, by its name alone.
func goByName(p string) bool {
	b := path.Base(p)
	return strings.HasSuffix(b, "_test.go") && b[0] != '_' && b[0] != '.' && walked(p, goSkipDir)
}

// goOn is, per gate platform, whether go test collects the test file p: 2
// surely, 1 depending on unmodelled tags (cgo, race, custom ones), 0 never;
// and its constraint lines. on is nil past the work bound.
func (r *run) goOn(p string, f *ast.File) (on []int, cons string, err error) {
	on = make([]int, len(platforms))
	var exprs []constraint.Expr
	var text strings.Builder
	seen := map[string]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if c.Pos() < f.Package && (constraint.IsGoBuild(c.Text) || constraint.IsPlusBuild(c.Text)) && !seen[c.Text] {
				x, err := constraint.Parse(c.Text)
				if err != nil {
					x = &constraint.TagExpr{Tag: "unparsed"} // a tag the guard does not model
				}
				seen[c.Text], exprs = true, append(exprs, x)
				text.WriteString(c.Text + "\n")
			}
		}
	}
	if !goByName(p) || f.Name.Name == "documentation" {
		return on, text.String(), nil
	}
	for i, pl := range platforms {
		if built(p, pl, nil) {
			if on[i], err = r.holds(exprs, pl); err != nil || on[i] < 0 {
				return nil, "", err
			}
		}
	}
	return on, text.String(), nil
}

// built: go/build includes the file p (src, or only its name when nil) on pl.
func built(p string, pl [2]string, src []byte) bool {
	if src == nil {
		src = []byte("package p\n")
	}
	ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release,
		OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
	ok, err := ctx.MatchFile(path.Dir(p), path.Base(p))
	return ok || err != nil // a header Go cannot read fails the build: built
}

// holds is 2 when exprs hold on pl for every assignment of the unmodelled
// tags, 0 for none, 1 otherwise or past 12 tags, -1 past the work bound.
func (r *run) holds(exprs []constraint.Expr, pl [2]string) (int, error) {
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
			r.work++
			return false
		})
	}
	if len(free) > 12 {
		return 1, nil
	}
	all, some := true, false
	for m := 0; m < 1<<len(free); m++ {
		if err := r.ctx.Err(); err != nil || r.work > maxWork {
			return -1, err
		}
		ok := true
		for _, x := range exprs {
			ok = x.Eval(func(tag string) bool {
				r.work++
				v, k := known(tag)
				return k && v || !k && m>>slices.Index(free, tag)&1 == 1
			}) && ok
		}
		all, some = all && ok, some || ok
	}
	if all {
		return 2, nil
	}
	return b2i(some), nil
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

// analyse reads one version of dir: each package (pkg and pkg_test apart)
// type-checked from the files a gate platform builds, imports unresolved.
func (r *run) analyse(dir string, srcs map[string][]byte, fa *pkgFacts) error {
	fset, groups, meta := token.NewFileSet(), map[string][]*ast.File{}, map[*ast.File]fileMeta{}
	for _, p := range slices.Sorted(maps.Keys(srcs)) {
		f, err := parser.ParseFile(fset, p, srcs[p], parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			fa.bad[p] = "not parsed: " + err.Error()
			continue
		}
		on, cons, err := r.goOn(p, f)
		if err != nil {
			return err
		} else if on == nil {
			fa.bad[p] = "its build constraints are over the guard's work bound"
			continue
		}
		test := strings.HasSuffix(p, "_test.go")
		if test && slices.Max(on) == 0 || !test && !slices.ContainsFunc(platforms, func(pl [2]string) bool { return built(p, pl, srcs[p]) }) {
			continue // no gate platform builds it: it cannot shadow what one does
		}
		meta[f] = fileMeta{p, cons, on, srcs[p]}
		groups[f.Name.Name] = append(groups[f.Name.Name], f)
	}
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		if err := r.group(dir, name, fset, groups[name], meta, fa); err != nil {
			return err
		}
	}
	return nil
}

type fileMeta struct {
	path, cons string
	on         []int
	src        []byte
}

// pkgScan is one type-checked package: its testing parameters, functions,
// methods by receiver, asserting helpers, and testing types behind a pointer.
type pkgScan struct {
	r         *run
	pkg       *types.Package
	info      *types.Info
	tvals     map[types.Object]string
	decls     map[types.Object]*ast.FuncDecl
	methodsOf map[string][]*ast.FuncDecl
	asserting map[*ast.FuncDecl]bool
	starred   map[ast.Expr]bool
}

func (s *pkgScan) qual(n ast.Node) string {
	if x, ok := n.(*ast.SelectorExpr); ok {
		if id, ok := x.X.(*ast.Ident); ok {
			if pn, ok := s.info.Uses[id].(*types.PkgName); ok {
				return pn.Imported().Path() + "." + x.Sel.Name
			}
		}
	}
	return ""
}

// tval is the testing type of e, an identifier bound to a testing value.
func (s *pkgScan) tval(e ast.Expr) string {
	if u, ok := ast.Unparen(e).(*ast.UnaryExpr); ok {
		e = u.X
	}
	id, _ := ast.Unparen(e).(*ast.Ident)
	return s.tvals[s.info.Uses[id]] // Uses[nil] is nil
}

// callee is the function call reaches in this package, if any.
func (s *pkgScan) callee(call *ast.CallExpr) *ast.FuncDecl {
	id, _ := ast.Unparen(call.Fun).(*ast.Ident)
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		id = sel.Sel // a method's identifier uses the method
	}
	if o, ok := s.info.Uses[id].(*types.Func); ok {
		return s.decls[o.Origin()]
	}
	return nil
}

func (s *pkgScan) assertion(call *ast.CallExpr) bool {
	passes := slices.ContainsFunc(call.Args, func(a ast.Expr) bool { t := s.tval(a); return t != "" && t != "testing.M" })
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	q := s.qual(ast.Unparen(call.Fun))
	return ok && slices.Contains(goFails, sel.Sel.Name) && s.tval(sel.X) != "" && s.tval(sel.X) != "testing.M" ||
		q != "" && slices.Contains(testify, q[:strings.LastIndex(q, ".")]) ||
		passes && s.asserting[s.callee(call)]
}

// mRun: call is os.Exit(m.Run()) on a testing.M, TestMain's one allowed exit.
func (s *pkgScan) mRun(call *ast.CallExpr) bool {
	if s.qual(ast.Unparen(call.Fun)) != "os.Exit" || len(call.Args) != 1 {
		return false
	}
	run, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := ast.Unparen(run.Fun).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && s.tval(sel.X) == "testing.M"
}

// mentions: n names a testing type or a testing value.
func (s *pkgScan) mentions(n ast.Node) (found bool) {
	ast.Inspect(n, func(n ast.Node) bool {
		e, _ := n.(ast.Expr)
		found = found || slices.Contains(testTypes, s.qual(n)) || e != nil && s.tval(e) != ""
		return !found
	})
	return found
}

// counts are a declaration's references; refs, its references into S.
type counts struct {
	asserts, skips, forged, reflect int
	refs                            []string
}

func (s *pkgScan) count(n ast.Node) (c counts) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			q := s.qual(x)
			c.skips += b2i(slices.Contains(goSkips, x.Sel.Name)) // called or not
			c.forged += b2i(q == "testing.Testing" || slices.Contains(valueTypes, q) && !s.starred[x])
			if sensitive(q) > 0 {
				c.refs = append(c.refs, q)
			}
		case *ast.Ident:
			if b, ok := s.info.Uses[x].(*types.Builtin); ok && sensitive(b.Name()) > 0 {
				c.refs = append(c.refs, b.Name())
			}
		case *ast.BasicLit: // a 0x16 spelled in a string or rune literal
			c.forged += b2i(x.Kind != token.INT && slices.ContainsFunc(spelled, func(m string) bool { return strings.Contains(x.Value, m) }))
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if ix, ok := ast.Unparen(l).(*ast.IndexExpr); ok {
					l = ix.X
				}
				if s.qual(ast.Unparen(l)) == "os.Args" {
					c.refs = append(c.refs, "os.Args assigned")
				}
			}
		case *ast.CallExpr:
			if s.mRun(x) {
				return false
			}
			c.asserts += b2i(s.assertion(x))
			fun := ast.Unparen(x.Fun)
			if st, ok := fun.(*ast.StarExpr); ok { // a conversion to *testing.T
				c.forged += b2i(slices.Contains(valueTypes, s.qual(ast.Unparen(st.X))))
			}
			if strings.HasPrefix(s.qual(fun), "reflect.") && slices.ContainsFunc(x.Args, func(a ast.Expr) bool { return s.mentions(a) }) {
				c.reflect++
			}
			if sel, ok := fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MethodByName" && len(x.Args) == 1 {
				name := "" // not a literal: any method
				if lit, ok := ast.Unparen(x.Args[0]).(*ast.BasicLit); ok {
					name, _ = strconv.Unquote(lit.Value)
				}
				c.reflect += b2i(name == "" || slices.Contains(control, name))
			}
		}
		return true
	})
	return c
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// recv is a method's receiver type name, without * or type parameters.
func recv(fn *ast.FuncDecl) string {
	return strings.TrimLeft(strings.SplitN(types.ExprString(fn.Recv.List[0].Type), "[", 2)[0], "*")
}

// group reads one type-checked package of dir.
func (r *run) group(dir, name string, fset *token.FileSet, files []*ast.File, meta map[*ast.File]fileMeta, fa *pkgFacts) error {
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	pkg, _ := (&types.Config{FakeImportC: true, Error: func(err error) {
		if te, ok := err.(types.Error); ok && (strings.Contains(te.Msg, "redeclared") || strings.Contains(te.Msg, "already declared")) {
			fa.bad[fset.Position(te.Pos).Filename] = "a name declared twice (in files the gate never builds together?)"
		}
	}}).Check(dir, fset, files, info)
	for _, n := range pkg.Scope().Names() {
		if o := pkg.Scope().Lookup(n); types.Universe.Lookup(n) != nil {
			fa.bad[fset.Position(o.Pos()).Filename] = "a package-level " + n + " shadows Go's predeclared one"
		}
	}
	for _, tv := range info.Types { // constants: a 0x16 in a string forges; a byte or rune 22 warns
		b, _ := tv.Type.Underlying().(*types.Basic)
		switch {
		case tv.Value != nil && tv.Value.Kind() == constant.String:
			fa.forged += b2i(strings.Contains(constant.StringVal(tv.Value), "\x16"))
		case tv.Value != nil && b != nil && slices.Contains([]types.BasicKind{types.Int32, types.Uint8, types.UntypedRune}, b.Kind()):
			fa.twentyTwos += b2i(constant.Compare(tv.Value, token.EQL, constant.MakeInt64(22)))
		}
	}
	s := &pkgScan{r: r, pkg: pkg, info: info, tvals: map[types.Object]string{}, decls: map[types.Object]*ast.FuncDecl{},
		methodsOf: map[string][]*ast.FuncDecl{}, asserting: map[*ast.FuncDecl]bool{}, starred: map[ast.Expr]bool{}}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.StarExpr:
				s.starred[x.X] = true
			case *ast.FuncType:
				for _, fl := range x.Params.List {
					t := fl.Type
					if st, ok := t.(*ast.StarExpr); ok {
						t = st.X
					}
					if q := s.qual(t); slices.Contains(testTypes, q) {
						for _, id := range fl.Names {
							s.tvals[info.Defs[id]] = q
						}
					}
				}
			case *ast.FuncDecl:
				if s.decls[info.Defs[x.Name]] = x; x.Recv != nil && len(x.Recv.List) > 0 {
					s.methodsOf[recv(x)] = append(s.methodsOf[recv(x)], x)
				}
			}
			return true
		})
	}
	for changed, i := true, 0; changed && i < 32; i++ { // the asserting helpers, to a bounded depth
		if err := r.ctx.Err(); err != nil {
			return err
		}
		changed = false
		for _, d := range s.decls {
			if !s.asserting[d] && d.Body != nil && s.count(d.Body).asserts > 0 {
				s.asserting[d], changed = true, true
			}
		}
	}
	var inits []ast.Node
	for _, f := range files {
		m := meta[f]
		test, flags := strings.HasSuffix(m.path, "_test.go"), []string{}
		if slices.ContainsFunc(f.Comments, func(cg *ast.CommentGroup) bool {
			return slices.ContainsFunc(cg.List, func(c *ast.Comment) bool { return strings.HasPrefix(c.Text, "//go:linkname") })
		}) {
			flags = append(flags, "a //go:linkname directive")
		}
		for _, is := range f.Imports {
			switch ip, _ := strconv.Unquote(is.Path.Value); {
			case ip == "unsafe" || ip == "C":
				flags = append(flags, "an import of "+ip)
			case is.Name != nil && is.Name.Name == "." && sPkg(ip):
				flags = append(flags, "a dot import of "+ip)
			}
		}
		for _, d := range f.Decls {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			c := s.count(d)
			fa.forged += c.forged
			if c.reflect > 0 && !slices.Contains(flags, "reflection on a testing value") {
				flags = append(flags, "reflection on a testing value")
			}
			fn, _ := d.(*ast.FuncDecl)
			fname, where := "", "package level"
			if fn != nil && fn.Body != nil && fn.Recv == nil {
				fname, where = fn.Name.Name, fn.Name.Name
			} else if fn != nil && fn.Body != nil {
				where = recv(fn) + "." + fn.Name.Name
			}
			for _, q := range c.refs {
				fa.refs[m.path+": "+q+" in "+where]++
			}
			hook := test && fname == "TestMain" && slices.ContainsFunc(fn.Type.Params.List, func(f *ast.Field) bool {
				st, ok := f.Type.(*ast.StarExpr)
				return ok && s.qual(st.X) == "testing.M"
			})
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR || fname == "init" || hook {
				if inits = append(inits, d); fn != nil {
					inits[len(inits)-1] = fn.Body // a function: its body, not its signature
				}
			}
			switch {
			case hook:
				fa.warn["TestMain(*testing.M) of "+dir+":"+name] = string(m.src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset])
			case test && goTest(fname):
				for i, pl := range platforms {
					if m.on[i] > 0 {
						fa.tests[pl[0]+"/"+pl[1]+" "+dir+":"+name+"."+fname] = &stat{c.asserts + output(f, fn), c.skips, m.on[i], m.cons}
					}
				}
				continue
			}
			fa.ambient += c.skips
		}
		if len(flags) > 0 {
			fa.flags[m.path] = flags
		}
	}
	return s.reach(dir, inits, fa)
}

// reach follows initialization (inits, package variables, the TestMain hook,
// test files included) through the functions it names and the methods of
// package types whose values it handles, to a bound, counting references
// into S and calls it cannot resolve; TestMain may os.Exit(m.Run()) once.
func (s *pkgScan) reach(dir string, starts []ast.Node, fa *pkgFacts) error {
	seen, exited := map[*ast.FuncDecl]bool{}, false
	follow := func(d *ast.FuncDecl) {
		if d != nil && d.Body != nil && !seen[d] {
			seen[d], starts = true, append(starts, d.Body)
		}
	}
	for len(starts) > 0 {
		if err := s.r.ctx.Err(); err != nil {
			return err
		} else if len(seen) > maxReach {
			fa.bad[dir+" (initialization)"] = "initialization past the guard's bound"
			return nil
		}
		n := starts[0]
		starts = starts[1:]
		ast.Inspect(n, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok { // a value of a package type: its methods may run
				t := s.info.TypeOf(e)
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if nt, ok := t.(*types.Named); ok && nt.Obj().Pkg() == s.pkg {
					for _, d := range s.methodsOf[nt.Obj().Name()] {
						follow(d)
					}
				}
			}
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if q := s.qual(x); sensitive(q) > 0 {
					fa.init["a reference into S ("+q+")"]++
				}
			case *ast.Ident:
				switch o := s.info.Uses[x].(type) {
				case *types.Func:
					follow(s.decls[o.Origin()])
				case *types.Builtin:
					if sensitive(o.Name()) > 0 {
						fa.init["a reference into S ("+o.Name()+")"]++
					}
				}
			case *ast.CallExpr:
				if s.mRun(x) && !exited {
					exited = true
					return false
				} else if s.unresolved(x) {
					fa.init["a call it cannot resolve ("+types.ExprString(x.Fun)+")"]++
				}
			}
			return true
		})
	}
	return nil
}

// unresolved: a call reached from initialization the guard cannot follow.
func (s *pkgScan) unresolved(call *ast.CallExpr) bool {
	fun := ast.Unparen(call.Fun) // an instantiated generic is not resolved: its body is followed as an identifier
	if tv, ok := s.info.Types[fun]; ok && tv.IsType() {
		return false // a conversion
	}
	var fn *types.Func
	switch f := fun.(type) {
	case *ast.FuncLit:
		return false // inspected in place
	case *ast.Ident:
		switch o := s.info.Uses[f].(type) {
		case *types.Builtin:
			return false
		case *types.Func:
			fn = o
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if pn, ok := s.info.Uses[id].(*types.PkgName); ok {
				return s.r.changed[pn.Imported().Path()]
			}
		}
		sel := s.info.Selections[f]
		if sel == nil && !slices.ContainsFunc(slices.Concat(slices.Collect(maps.Values(s.methodsOf))...), func(d *ast.FuncDecl) bool { return d.Name.Name == f.Sel.Name }) {
			return false // a method of a type from an import the guard does not load
		} else if sel == nil || sel.Kind() != types.MethodVal || types.IsInterface(sel.Recv()) { // the universe error too
			return true // perhaps this package's method, a func field, an interface
		}
		fn = sel.Obj().(*types.Func)
	}
	if fn == nil {
		return true
	}
	d := s.decls[fn.Origin()]
	return d != nil && d.Body == nil || d == nil && fn.Pkg() == s.pkg // no body: assembly, linkname
}

// output: an example's output comment (its body's last), its assertion.
func output(f *ast.File, fn *ast.FuncDecl) int {
	i := sort.Search(len(f.Comments), func(i int) bool { return f.Comments[i].Pos() > fn.Body.Rbrace })
	if !strings.HasPrefix(fn.Name.Name, "Example") || i == 0 || f.Comments[i-1].Pos() < fn.Body.Lbrace {
		return 0
	}
	t := strings.ToLower(strings.TrimSpace(f.Comments[i-1].Text()))
	return b2i(strings.HasPrefix(t, "output:") || strings.HasPrefix(t, "unordered output:"))
}

// goModFacts is a go.mod's module path and ignore directives, comments dropped.
func goModFacts(b []byte) (mod, ignores string) {
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l, _, _ = strings.Cut(l, "//")
		if f := strings.Fields(l); len(f) == 2 && f[0] == "module" {
			mod = strings.Trim(f[1], `"`)
		}
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "ignore") || in {
			in, ignores = strings.HasSuffix(l, "(") || in && l != ")", ignores+l+"\n"
		}
	}
	return mod, ignores
}
