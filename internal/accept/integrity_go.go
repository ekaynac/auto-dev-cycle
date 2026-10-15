// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
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
	// initPkgs are the inert packages initialization may call, with the
	// unicode, math and encoding trees; fmt and time only in part (inert).
	initPkgs = []string{"strings", "strconv", "bytes", "errors", "sort", "slices", "maps", "regexp", "embed", "path", "path/filepath"}
)

// inert: initialization may call or pass on q ("importpath.Name"), a
// function or value of an inert package (never by its name's case).
func inert(q string) bool {
	i := strings.LastIndex(q, ".")
	pkg, name := q[:max(i, 0)], q[i+1:]
	root, _, _ := strings.Cut(pkg, "/")
	switch {
	case pkg == "fmt":
		return strings.HasPrefix(name, "Sprint") || name == "Errorf"
	case pkg == "time":
		return !slices.Contains([]string{"Sleep", "After", "AfterFunc", "Tick", "NewTicker", "NewTimer"}, name)
	case pkg == "sync": // its types, and Once* wrapping a function for later
		return strings.HasPrefix(name, "Once")
	}
	return slices.Contains(initPkgs, pkg) || slices.Contains([]string{"unicode", "math", "encoding"}, root)
}

func allCaps(n string) bool {
	return len(n) >= 3 && n == strings.ToUpper(n) && strings.ContainsFunc(n, unicode.IsUpper) // not V, T, ID
}

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
	case pkg == "syscall" || strings.HasPrefix(pkg, "golang.org/x/sys/"): // ALL_CAPS: an inert constant
		return b2i(!allCaps(name)) * (1 + is("Exit", "Exec*", "Syscall*", "RawSyscall*", "AllThreadsSyscall*", "Dup*", "Fcntl*", "ForkExec", "StartProcess"))
	case pkg == "os": // its reads are inert
		return b2i(!slices.Contains(osRead, name) && !strings.HasPrefix(name, "Err")) * (1 + is("Exit", "StartProcess"))
	case pkg == "os/exec":
		return 1 + is("Command*", "Cmd") // a process start
	case pkg == "plugin":
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

// goOn is, per gate platform, whether go builds p: 2 surely, 1 by unmodelled
// tags (tool tags, cgo, race, custom), 0 never; nil past the work bound.
func (r *run) goOn(p string, f *ast.File) (on []int, exprs []constraint.Expr, cons string, err error) {
	on = make([]int, len(platforms))
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
	for i, pl := range platforms {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release, // its name: go/build's rules
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
		if ok, err := ctx.MatchFile(path.Dir(p), path.Base(p)); ok || err != nil {
			if on[i], err = r.holds(exprs, pl); err != nil || on[i] < 0 {
				return nil, nil, "", err
			}
		}
	}
	return on, exprs, text.String(), nil
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

// analyse reads one version of dir: each package (pkg and pkg_test apart),
// per gate platform, type-checked from the files that platform may build,
// imports unresolved.
func (r *run) analyse(dir string, srcs map[string][]byte, fa *pkgFacts) error {
	fset, groups, meta := token.NewFileSet(), map[string][]*ast.File{}, map[*ast.File]fileMeta{}
	for _, p := range slices.Sorted(maps.Keys(srcs)) {
		f, err := parser.ParseFile(fset, p, srcs[p], parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			fa.bad[p] = "not parsed: " + err.Error()
			continue
		}
		on, exprs, cons, err := r.goOn(p, f)
		if err != nil {
			return err
		} else if on == nil {
			fa.bad[p] = "its build constraints are over the guard's work bound"
			continue
		} else if slices.Max(on) == 0 {
			continue // no gate platform builds it, whatever the tags: it cannot shadow what one does
		}
		coll := on // collected as tests
		if !goByName(p) || f.Name.Name == "documentation" {
			coll = make([]int, len(platforms))
		}
		meta[f] = fileMeta{p, cons, coll, srcs[p], exprs, on}
		groups[f.Name.Name] = append(groups[f.Name.Name], f)
	}
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		for i := range platforms { // each platform's files apart: a name split by platform (x_amd64.go) is that platform's
			if on := slices.DeleteFunc(slices.Clone(groups[name]), func(f *ast.File) bool { return meta[f].built[i] == 0 }); len(on) > 0 {
				if err := r.group(i, dir, name, fset, on, meta, fa); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type fileMeta struct {
	path, cons string
	on         []int // collected as tests, per platform
	src        []byte
	exprs      []constraint.Expr
	built      []int // built, per platform (goOn's)
}

// pkgScan is one type-checked package: its testing parameters, functions,
// methods by receiver, asserting helpers, and testing types behind a pointer.
type pkgScan struct {
	r         *run
	pkg       *types.Package
	info      *types.Info
	tvals     map[types.Object]string
	decls     map[types.Object]*ast.FuncDecl
	same      map[string][]*ast.FuncDecl // every declaration of a name (race, !race): each counts
	asserting map[*ast.FuncDecl]bool
	mentioned map[ast.Expr]bool // the aliases mentions is in
	starred   map[ast.Expr]bool // directly behind *
	pkgVars   map[*ast.ValueSpec]bool
	aliases   map[types.Object]ast.Expr // an alias's right-hand side
}

// qual is what n names as "importpath.Name", if a package's: by type
// information, or, where unrecorded, by its own scope (file imports, locals).
func (s *pkgScan) qual(n ast.Node) string {
	if x, ok := n.(*ast.SelectorExpr); ok && x != nil {
		if id, ok := x.X.(*ast.Ident); ok {
			o := s.info.Uses[id]
			if sc := s.pkg.Scope().Innermost(id.Pos()); o == nil && sc != nil {
				_, o = sc.LookupParent(id.Name, id.Pos())
			}
			if pn, ok := o.(*types.PkgName); ok {
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
	if o := s.info.Uses[ident(e)]; o != nil { // not tvals[nil], which _ = t sets
		return s.tvals[o]
	}
	return ""
}

// callee is the function call reaches in this package, if any.
func (s *pkgScan) callee(call *ast.CallExpr) *ast.FuncDecl {
	f, _ := fn(call.Fun)
	id := ident(f)
	if sel, ok := f.(*ast.SelectorExpr); ok {
		id = sel.Sel // a method's identifier uses the method
	}
	if o, ok := s.info.Uses[id].(*types.Func); ok {
		return s.decls[o.Origin()]
	}
	return nil
}

// fn is a call's function and type arguments, through an explicit
// instantiation (f[int]).
func fn(e ast.Expr) (ast.Expr, []ast.Expr) {
	switch x := ast.Unparen(e).(type) {
	case *ast.IndexExpr:
		return ast.Unparen(x.X), []ast.Expr{x.Index}
	case *ast.IndexListExpr:
		return ast.Unparen(x.X), x.Indices
	}
	return ast.Unparen(e), nil
}

// alias is what e names through its own aliases (type TP = *testing.T), if one.
func (s *pkgScan) alias(e ast.Expr) (rhs ast.Expr) {
	for i := 0; i < 8; i++ {
		if f, _ := fn(e); s.aliases[s.info.Uses[ident(f)]] != nil { // TG[int] too
			e, rhs = s.aliases[s.info.Uses[ident(f)]], s.aliases[s.info.Uses[ident(f)]]
		}
	}
	return rhs
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

// ptrTo: e is *testing.T, B, F or M; or *X.T, B, F, M or TB that no
// scope resolves (fail closed).
func (s *pkgScan) ptrTo(e ast.Expr) bool {
	st, ok := ast.Unparen(cmp.Or(s.alias(e), e)).(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, _ := ast.Unparen(st.X).(*ast.SelectorExpr)
	return sel != nil && (slices.Contains(valueTypes, s.qual(sel)) || s.qual(sel) == "" && slices.Contains(strings.Fields("T B F M TB"), sel.Sel.Name))
}

// mentions: n names a testing type or a testing value.
func (s *pkgScan) mentions(n ast.Node) (found bool) {
	ast.Inspect(n, func(n ast.Node) bool {
		e, _ := n.(ast.Expr)
		if a := s.alias(e); e != nil && a != nil && !found && !slices.Contains(testTypes, s.qual(n)) && s.tval(e) == "" {
			if found = s.mentioned[a]; !found { // an alias cycle (A[P] = B[P], B[P] = A[P]) mentions, fail closed
				s.mentioned[a] = true
				found = s.mentions(a)
				delete(s.mentioned, a)
			}
		}
		found = found || slices.Contains(testTypes, s.qual(n)) || e != nil && s.tval(e) != ""
		return !found
	})
	return found
}

// counts are a declaration's references; refs, its references into S.
type counts struct {
	asserts, skips, forged, reflect, generic int // generic: an own generic function not called
	refs                                     []string
}

func (s *pkgScan) count(n ast.Node) (c counts) {
	called := map[*ast.Ident]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.UnaryExpr:
			if x.Op == token.AND && s.qual(s.root(x.X)) == "os.Args" {
				c.refs = append(c.refs, "os.Args assigned")
			}
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
			} else if f, ok := s.info.Uses[x].(*types.Func); ok && !called[x] && s.decls[f.Origin()] != nil && s.decls[f.Origin()].Type.TypeParams != nil {
				c.generic++ // assigned, passed or returned: inferred where C3 does not look
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
		case *ast.TypeSpec: // an alias of a testing type: plain, generic or exported
			c.forged += b2i(x.Assign.IsValid() && s.mentions(x.Type))
		case *ast.TypeAssertExpr: // to *testing.T: laundered through any
			c.forged += b2i(s.ptrTo(x.Type))
		case *ast.CaseClause:
			for _, e := range x.List {
				c.forged += b2i(s.ptrTo(e))
			}
		case *ast.CallExpr:
			if s.mRun(x) {
				return false
			}
			c.asserts += b2i(s.assertion(x))
			fun, args := ast.Unparen(x.Fun), x.Args
			f, targs := fn(fun)
			called[ident(f)] = true
			if b, ok := s.info.Uses[ident(fun)].(*types.Builtin); ok && (b.Name() == "copy" || b.Name() == "append") {
				args = args[:min(1, len(args))] // the destination
			} else if _, lit := fun.(*ast.FuncLit); !lit && s.callee(x) == nil {
				args = nil // own code only
			}
			for _, a := range args { // os.Args's array, aliased (an element is a string)
				if _, el := ast.Unparen(a).(*ast.IndexExpr); !el && s.qual(s.root(a)) == "os.Args" {
					c.refs = append(c.refs, "os.Args assigned")
				}
			}
			c.forged += b2i(s.ptrTo(fun)) // a conversion to *testing.T
			// A testing value made by inference (an explicit testing type argument is unstarred).
			if d := s.callee(x); d != nil && d.Type.TypeParams != nil && len(targs) < d.Type.TypeParams.NumFields() {
				var params []ast.Expr
				for _, f := range d.Type.Params.List {
					params = append(params, slices.Repeat([]ast.Expr{f.Type}, max(1, len(f.Names)))...)
				}
				c.forged += b2i(len(params) > 0 && slices.ContainsFunc(x.Args, func(a ast.Expr) bool {
					return s.mentions(a) && !s.mentions(params[min(slices.Index(x.Args, a), len(params)-1)])
				}))
			}
			if sensitive(s.qual(fun)) == 2 { // its arguments too: a changed exit code warns
				c.refs = append(c.refs, types.ExprString(x))
			}
			if strings.HasPrefix(s.qual(fun), "reflect.") && slices.ContainsFunc(x.Args, func(a ast.Expr) bool { return s.mentions(a) }) {
				c.reflect++
			}
			if sel, ok := fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MethodByName" && len(x.Args) == 1 {
				lit, _ := ast.Unparen(x.Args[0]).(*ast.BasicLit)
				name, _ := strconv.Unquote(cmp.Or(lit, &ast.BasicLit{}).Value) // not a literal: any method
				c.reflect += b2i(name == "" || slices.Contains(control, name))
			}
		}
		return true
	})
	return c
}

func ident(e ast.Expr) *ast.Ident {
	id, _ := ast.Unparen(e).(*ast.Ident)
	return id
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// declKey is a function's name, or a method's receiver and name.
func declKey(fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		return recv(fn) + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// recv is a method's receiver type name, without * or type parameters.
func recv(fn *ast.FuncDecl) string {
	return strings.TrimLeft(strings.SplitN(types.ExprString(fn.Recv.List[0].Type), "[", 2)[0], "*")
}

// group reads one type-checked package of dir.
func (r *run) group(pi int, dir, name string, fset *token.FileSet, files []*ast.File, meta map[*ast.File]fileMeta, fa *pkgFacts) error {
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	exprs, built, first := map[string][]constraint.Expr{}, map[string][]int{}, ""
	for _, f := range files {
		exprs[meta[f].path], built[meta[f].path] = meta[f].exprs, meta[f].built
	}
	twice := func(a, b string) { // a name declared in files a and b: unless no gate platform builds both (by tags or by name, x_amd64.go)
		if slices.ContainsFunc(platforms, func(pl [2]string) bool {
			i := slices.Index(platforms, pl)
			n, _ := r.holds(append(slices.Clone(exprs[a]), exprs[b]...), pl)
			return n != 0 && (built[a] == nil || built[a][i] > 0) && (built[b] == nil || built[b][i] > 0)
		}) {
			fa.bad[a] = "a name declared twice, in files the gate may build together"
		}
	}
	pkg, _ := (&types.Config{FakeImportC: true, Error: func(err error) {
		te, _ := err.(types.Error)
		switch at := fset.Position(te.Pos).Filename; {
		case strings.Contains(te.Msg, "redeclared"):
			first = at
		case strings.HasPrefix(te.Msg, "\tother declaration of"):
			twice(first, at)
		case strings.Contains(te.Msg, " already declared at "):
			other := te.Msg[strings.LastIndex(te.Msg, " at ")+4:]
			twice(at, other[:strings.LastIndex(other[:strings.LastIndex(other, ":")], ":")])
		}
	}}).Check(dir, fset, files, info)
	for _, n := range pkg.Scope().Names() { // the predeclared names the rules rely on
		if o := pkg.Scope().Lookup(n); slices.Contains([]string{"print", "println", "new", "make", "rune", "byte", "string"}, n) {
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
		same: map[string][]*ast.FuncDecl{}, mentioned: map[ast.Expr]bool{}, asserting: map[*ast.FuncDecl]bool{}, starred: map[ast.Expr]bool{}, pkgVars: map[*ast.ValueSpec]bool{}, aliases: map[types.Object]ast.Expr{}}
	for _, f := range slices.Concat(files, files) { // twice: an alias or a copy declared after its use
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				if o := info.Defs[x.Name]; x.Assign.IsValid() && o != nil { // a redeclared alias has none: never a nil key, every name's
					s.aliases[o] = x.Type
				}
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
				if s.decls[info.Defs[x.Name]] = x; !slices.Contains(s.same[declKey(x)], x) {
					s.same[declKey(x)] = append(s.same[declKey(x)], x)
				}
			case *ast.AssignStmt: // a copy of a testing value is one
				for i, v := range x.Rhs {
					if id := ident(x.Lhs[min(i, len(x.Lhs)-1)]); id != nil && s.mentions(v) {
						s.tvals[cmp.Or(info.Defs[id], info.Uses[id])] = "testing.T"
					}
				}
			case *ast.ValueSpec: // as is a variable of a testing type
				for i, id := range x.Names {
					if x.Type != nil && s.mentions(x.Type) || i < len(x.Values) && s.mentions(x.Values[i]) {
						s.tvals[info.Defs[id]] = "testing.T"
					}
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
		for _, ds := range s.same { // asserting only when every declaration asserts
			if !s.asserting[ds[0]] && !slices.ContainsFunc(ds, func(d *ast.FuncDecl) bool { return d.Body == nil || s.count(d.Body).asserts == 0 }) {
				for _, d := range ds {
					s.asserting[d], changed = true, true
				}
			}
		}
	}
	var inits, hooks []ast.Node
	for _, f := range files {
		m := meta[f]
		test, flags, tfile := strings.HasSuffix(m.path, "_test.go"), []string{}, s.mentions(f)
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
			fa.forged += c.forged + c.generic*b2i(tfile) // a file with testing values
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
				fa.refs[m.path+"\x00"+q+"\x00"+where]++
			}
			hook := test && fname == "TestMain" && slices.ContainsFunc(fn.Type.Params.List, func(f *ast.Field) bool {
				st, ok := f.Type.(*ast.StarExpr)
				return ok && s.qual(st.X) == "testing.M"
			})
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, sp := range g.Specs {
					s.pkgVars[sp.(*ast.ValueSpec)] = true
				}
				inits = append(inits, d)
			} else if fname == "init" {
				inits = append(inits, fn.Body) // a function: its body, not its signature
			} else if hook {
				hooks = append(hooks, fn.Body)
			}
			switch {
			case hook:
				fa.warn["TestMain(*testing.M) of "+dir+":"+name] = string(m.src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset])
			case test && goTest(fname):
				if m.on[pi] > 0 {
					k, st := platforms[pi][0]+"/"+platforms[pi][1]+" "+dir+":"+name+"."+fname, &stat{c.asserts + output(f, fn), c.skips, m.on[pi], m.cons}
					if old := fa.tests[k]; old != nil { // declared again (race, !race): the weakest
						st = &stat{min(old.asserts, st.asserts), max(old.skips, st.skips), min(old.cert, st.cert), old.cons + st.cons}
					}
					fa.tests[k] = st
				}
				continue
			}
			fa.ambient += c.skips
		}
		if len(flags) > 0 {
			fa.flags[m.path] = flags
		}
	}
	if err := r.typedOn(platforms[pi], dir, name, fset, files, meta, fa); err != nil {
		return err
	} else if err := s.reach(dir, inits, false, fa); err != nil {
		return err
	}
	return s.reach(dir, hooks, true, fa)
}

// reach follows initialization (inits and package variables, test files
// included; or the TestMain hook) through the own functions and literals it
// calls, ranges over or passes to own functions, to a bound. Unverifiable:
// a call of another package's name not inert; a value passed outside the
// package (a receiver, panic's argument) not plain data; another package's
// non-plain value read into a local; a write to another package; a range
// or call it cannot follow. The hook may flag.Parse() and os.Exit(m.Run()).
func (s *pkgScan) reach(dir string, starts []ast.Node, hook bool, fa *pkgFacts) error {
	seen, run, exited, calls := map[*ast.FuncDecl]bool{}, map[*ast.FuncLit]bool{}, !hook, map[ast.Expr]bool{}
	own := func(e ast.Expr) bool { // e is the package's own function or literal: followed
		id := ident(e)
		if f, ok := ast.Unparen(e).(*ast.FuncLit); ok {
			run[f] = true
			return true
		} else if sel, ok := ast.Unparen(e).(*ast.SelectorExpr); ok {
			id = sel.Sel
		}
		if o, _ := s.info.Uses[id].(*types.Func); o != nil && s.decls[o.Origin()] != nil && s.decls[o.Origin()].Body != nil {
			for _, d := range s.same[declKey(s.decls[o.Origin()])] { // every declaration of it
				if !seen[d] && d.Body != nil {
					seen[d], starts = true, append(starts, d.Body)
				}
			}
			return true
		}
		return false
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
			add := func(k string) { fa.init[k]++ }
			switch x := n.(type) {
			case *ast.FuncLit: // run only when called, ranged over or passed to an own function
				return run[x]
			case *ast.RangeStmt:
				t, fn := s.info.TypeOf(x.X), false
				if t != nil {
					_, fn = t.Underlying().(*types.Signature)
				}
				if (t == nil || t == types.Typ[types.Invalid] || fn) && !own(x.X) {
					add("a range it cannot follow (" + types.ExprString(x.X) + ")")
				}
			case *ast.AssignStmt:
				s.store(x.Lhs, x.Rhs, add)
			case *ast.IncDecStmt:
				s.store([]ast.Expr{x.X}, nil, add)
			case *ast.UnaryExpr: // a pointer into another package: a write it cannot follow
				if q := s.qual(s.root(x.X)); x.Op == token.AND && q != "" {
					add("another package's variable addressed (" + types.ExprString(x) + ")")
				}
			case *ast.ReturnStmt: // a value own code returns is read where it is used
				for _, r := range x.Results {
					if !s.read(r) {
						add("another package's value it cannot prove plain data returned (" + types.ExprString(r) + ")")
					}
				}
			case *ast.SendStmt:
				if s.qual(s.root(x.Chan)) != "" && !s.shared(x.Value) {
					add("a value it cannot prove plain data sent to " + types.ExprString(x.Chan))
				}
			case *ast.ValueSpec: // a local's (a package variable's values flow, and are checked, where they are used)
				if !s.pkgVars[x] {
					s.store(nil, x.Values, add)
				}
			case *ast.SelectorExpr:
				if q := s.qual(x); q != "" && calls[x] && !inert(q) {
					add("a reference it cannot prove safe (" + q + ")")
				}
			case *ast.Ident:
				if o, ok := s.info.Uses[x].(*types.Builtin); ok && sensitive(o.Name()) > 0 {
					add("a reference it cannot prove safe (" + o.Name() + ")")
				}
			case *ast.CallExpr:
				fun := ast.Unparen(x.Fun)
				if calls[fun] = true; hook && s.qual(fun) == "flag.Parse" && len(x.Args) == 0 {
					hook = false // once
					return false
				} else if s.mRun(x) && !exited {
					exited = true
					return false
				} else if k := s.call(x, own); k != "" {
					add(k)
				}
			}
			return true
		})
	}
	return nil
}

// call is what a call reached from initialization is, if not safe: an own
// callee is followed (with own functions passed to it); another package's
// callee, and panic, may take only plain data, a receiver included.
func (s *pkgScan) call(x *ast.CallExpr, own func(ast.Expr) bool) string {
	fun, args, inside := ast.Unparen(x.Fun), x.Args, false
	unresolved := "a call it cannot resolve (" + types.ExprString(fun) + ")"
	if s.conversion(x) && !s.read(x) {
		return "another package's value it cannot prove plain data converted (" + types.ExprString(x) + ")"
	} else if s.conversion(x) {
		return ""
	}
	switch f := fun.(type) {
	case *ast.FuncLit, *ast.Ident:
		if b, ok := s.info.Uses[ident(fun)].(*types.Builtin); ok && slices.Contains([]string{"copy", "append", "clear", "delete"}, b.Name()) &&
			len(args) > 0 && s.qual(s.root(args[0])) != "" {
			return "a write it cannot prove safe (" + s.qual(s.root(args[0])) + ")" // into another package's slice or map
		} else if ok && b.Name() != "panic" {
			return ""
		} else if inside = own(f); !inside && !ok {
			return unresolved // a function value, or no body (assembly, linkname)
		}
	case *ast.SelectorExpr:
		if q := s.qual(f); strings.HasPrefix(q, "sync.Once") {
			return "" // wraps a function for later: a call of what it returns is unresolved
		} else if q == "" {
			sel := s.info.Selections[f]
			if sel == nil || sel.Kind() != types.MethodVal || types.IsInterface(sel.Recv()) { // the universe error too
				return unresolved // a method of an unloaded type, a func field, an interface
			} else if inside = own(f); !inside && sel.Obj().Pkg() == s.pkg {
				return unresolved // no body
			}
			args = append([]ast.Expr{f.X}, args...) // a method's receiver is an argument
		}
	default:
		return unresolved
	}
	for _, a := range args {
		if q := s.qual(s.root(a)); inside && q != "" && !inert(q) { // a slice, map or chan aliases
			return "another package's variable passed to " + types.ExprString(fun)
		} else if inside {
			own(a)
		} else if !s.shared(a) {
			return "a value it cannot prove plain data passed to " + types.ExprString(fun)
		}
	}
	return ""
}

// shared: a may leave the package: plain data, or an inert package's
// function, value or result (never by its name's case), not one of its
// error variables (an interface a write could fill; by name, types unloaded).
func (s *pkgScan) shared(a ast.Expr) bool {
	r, q := s.root(a), s.qual(s.root(a))
	name := q[strings.LastIndex(q, ".")+1:]
	errVar := strings.HasPrefix(name, "Skip") || strings.HasPrefix(name, "Err") && len(name) > 3 && unicode.IsUpper(rune(name[3]))
	c, call := r.(*ast.CallExpr)
	return inert(q) && !errVar || call && inert(s.qual(ast.Unparen(c.Fun))) || plainData(s.info.TypeOf(a), map[types.Type]bool{})
}

// store checks a write (none to another package) and a read into a local
// (another package's value only as plain data), from initialization.
func (s *pkgScan) store(lhs, rhs []ast.Expr, add func(string)) {
	for _, l := range lhs {
		if q := s.qual(s.root(l)); q != "" {
			add("a write it cannot prove safe (" + q + ")")
		}
	}
	for _, r := range rhs {
		if !s.read(r) {
			add("another package's value it cannot prove plain data read (" + types.ExprString(r) + ")")
		}
	}
}

// read: r is not another package's, or is an inert package's, or a copy
// of a basic value (a slice, map, chan, pointer or struct may alias it).
func (s *pkgScan) read(r ast.Expr) bool {
	t, q := s.info.TypeOf(r), s.qual(s.root(r))
	if c, ok := ast.Unparen(r).(*ast.CallExpr); ok && s.conversion(c) {
		t = s.info.TypeOf(c.Fun) // what it converts to (an unloaded operand leaves the result invalid)
	}
	_, basic := types.Unalias(cmp.Or(t, types.Type(types.Typ[types.Invalid]))).Underlying().(*types.Basic)
	return q == "" || inert(q) || basic && plainData(t, map[types.Type]bool{})
}

func (s *pkgScan) conversion(x *ast.CallExpr) bool {
	_, star := ast.Unparen(x.Fun).(*ast.StarExpr)
	return len(x.Args) == 1 && (star || s.info.Types[ast.Unparen(x.Fun)].IsType())
}

// root is what a value is read from: through &, *, indexes, slices, fields,
// conversions and assertions, to another package's name, or to whatever else
// it starts at.
func (s *pkgScan) root(e ast.Expr) ast.Expr {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.UnaryExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.TypeAssertExpr:
			e = x.X
		case *ast.CallExpr:
			if !s.conversion(x) {
				return x
			}
			e = x.Args[0]
		case *ast.SelectorExpr:
			if s.qual(x) != "" {
				return x
			}
			e = x.X
		default:
			return x
		}
	}
}

// plainData: t holds no func, interface or invalid type and no named type
// with methods, through pointers, slices, arrays, chans, maps and fields.
func plainData(t types.Type, seen map[types.Type]bool) bool {
	if t = types.Unalias(t); t == nil || seen[t] {
		return t != nil
	}
	seen[t] = true
	if n, ok := t.(*types.Named); ok && n.NumMethods() > 0 {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Kind() != types.Invalid && u.Kind() != types.UnsafePointer
	case *types.Map:
		return plainData(u.Key(), seen) && plainData(u.Elem(), seen)
	case interface{ Elem() types.Type }: // pointer, slice, array, chan
		return plainData(u.Elem(), seen)
	case *types.Struct:
		for i := range u.NumFields() {
			if !plainData(u.Field(i).Type(), seen) {
				return false
			}
		}
		return true
	}
	return false
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

// goIgnores is a go.mod's ignore directives, comments dropped.
func goIgnores(b []byte) (ignores string) {
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if l, _, _ = strings.Cut(l, "//"); strings.HasPrefix(strings.TrimSpace(l), "ignore") || in {
			l = strings.TrimSpace(l)
			in, ignores = strings.HasSuffix(l, "(") || in && l != ")", ignores+l+"\n"
		}
	}
	return ignores
}
