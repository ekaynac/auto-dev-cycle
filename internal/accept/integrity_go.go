// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/build/constraint"
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
	// platforms are the gate's: a Go test must stay collected on each.
	platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}}
	// release is the gate toolchain's release tags: the Go the repository
	// pins, which the controller runs too.
	release = build.Default.ReleaseTags
	// otherPlatforms are the GOOS and GOARCH names no gate platform sets.
	otherPlatforms = strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js nacl netbsd openbsd plan9 " +
		"solaris wasip1 windows zos 386 amd64p32 arm armbe arm64be loong64 mips mipsle mips64 mips64le mips64p32 " +
		"mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm gccgo")
	goSkips   = []string{"Skip", "Skipf", "SkipNow"}
	goFails   = []string{"Error", "Errorf", "Fatal", "Fatalf", "Fail", "FailNow"} // exactly the failure-setting methods
	control   = append(slices.Clone(goSkips), append(slices.Clone(goFails), "Run", "Helper", "Cleanup")...)
	testify   = []string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require"}
	testTypes = []string{"testing.T", "testing.B", "testing.F", "testing.TB", "testing.M"}
	exitFns   = []string{"os.Exit", "syscall.Exit", "runtime.Goexit", "log.Fatal", "log.Fatalf", "log.Fatalln", "log.Panic",
		"log.Panicf", "log.Panicln"}
	outputs = []string{"os.Stdout", "os.Stderr", "fmt.Print", "fmt.Printf", "fmt.Println", "log.Print", "log.Printf",
		"log.Println", "syscall.Write"}
	// sensitive packages must not be dot-imported: their names would not resolve.
	sensitive = []string{"testing", "os", "syscall", "runtime", "log", "reflect", "unsafe", "fmt"}
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

// goOn is, per gate platform, whether go test collects the test file p (2
// surely, 1 depending on unmodelled tags such as cgo, race, goexperiment.*
// or custom ones, 0 never: go/build's name rules, a directory ./... walks,
// not package documentation, its constraints), and its distinct constraint
// lines. on is nil past the work bound.
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
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc",
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
		if ok, _ := ctx.MatchFile(path.Dir(p), path.Base(p)); ok {
			if on[i], err = r.holds(exprs, pl); err != nil || on[i] < 0 {
				return nil, "", err
			}
		}
	}
	return on, text.String(), nil
}

// holds is 2 when exprs hold on pl for every assignment of the tags the
// guard does not model, 0 when for none, 1 otherwise (or past 12 tags),
// -1 past the work bound. It checks ctx per assignment.
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
	switch {
	case all:
		return 2, nil
	case some:
		return 1, nil
	}
	return 0, nil
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

// analyse reads one version of the package directory dir: per file its
// collection, then each package (dir's internal and external test packages
// apart) type-checked from its own files, imports left unresolved.
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
		for _, m := range forged {
			fa.forged += bytes.Count(srcs[p], []byte(m))
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

// pkgScan is one type-checked package: its testing values (parameters of
// a testing type), its functions by object, and the helpers that assert
// (call a failure method on a testing value, or such a helper with one).
type pkgScan struct {
	r         *run
	pkg       *types.Package
	info      *types.Info
	tvals     map[types.Object]string
	decls     map[types.Object]*ast.FuncDecl
	asserting map[*ast.FuncDecl]bool
	methods   map[string]bool // the names of the package's methods
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
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return ""
	}
	return s.tvals[s.info.Uses[id]]
}

// callee is the function call reaches in this package, if any.
func (s *pkgScan) callee(call *ast.CallExpr) *ast.FuncDecl {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		if o, ok := s.info.Uses[f].(*types.Func); ok {
			return s.decls[o.Origin()]
		}
	case *ast.SelectorExpr:
		if sel := s.info.Selections[f]; sel != nil && sel.Kind() == types.MethodVal {
			return s.decls[sel.Obj().(*types.Func).Origin()]
		}
	}
	return nil
}

func (s *pkgScan) assertion(call *ast.CallExpr) bool {
	passes := slices.ContainsFunc(call.Args, func(a ast.Expr) bool { t := s.tval(a); return t != "" && t != "testing.M" })
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	q := s.qual(call.Fun)
	return ok && slices.Contains(goFails, sel.Sel.Name) && s.tval(sel.X) != "" && s.tval(sel.X) != "testing.M" ||
		q != "" && slices.Contains(testify, q[:strings.LastIndex(q, ".")]) ||
		passes && s.asserting[s.callee(call)]
}

// counts are a declaration's references.
type counts struct{ asserts, skips, exits, forged, twentyTwos, reflect int }

func (s *pkgScan) count(n ast.Node) (c counts) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			q := s.qual(x)
			c.skips += b2i(slices.Contains(goSkips, x.Sel.Name)) // called or not
			c.exits += b2i(slices.Contains(exitFns, q))
			c.forged += b2i(q == "testing.Testing")
		case *ast.CallExpr:
			c.asserts += b2i(s.assertion(x))
			q := s.qual(x.Fun)
			if strings.HasPrefix(q, "reflect.") && slices.ContainsFunc(x.Args, func(a ast.Expr) bool { return s.tval(a) != "" }) {
				c.reflect++
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MethodByName" && len(x.Args) == 1 {
				name := "" // not a literal: any method
				if lit, ok := x.Args[0].(*ast.BasicLit); ok {
					name, _ = strconv.Unquote(lit.Value)
				}
				c.reflect += b2i(name == "" || slices.Contains(control, name))
			}
			if id, ok := x.Fun.(*ast.Ident); ok && len(x.Args) == 1 && slices.Contains([]string{"byte", "rune", "uint8", "int32"}, id.Name) {
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT {
					v, err := strconv.ParseInt(lit.Value, 0, 64)
					c.twentyTwos += b2i(err == nil && v == 22)
				}
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

// group reads one type-checked package of dir.
func (r *run) group(dir, name string, fset *token.FileSet, files []*ast.File, meta map[*ast.File]fileMeta, fa *pkgFacts) error {
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	pkg, _ := (&types.Config{Error: func(error) {}, FakeImportC: true}).Check(dir, fset, files, info)
	s := &pkgScan{r: r, pkg: pkg, info: info, tvals: map[types.Object]string{}, decls: map[types.Object]*ast.FuncDecl{}, asserting: map[*ast.FuncDecl]bool{}, methods: map[string]bool{}}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
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
				s.decls[info.Defs[x.Name]], s.methods[x.Name.Name] = x, s.methods[x.Name.Name] || x.Recv != nil
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
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "//go:linkname") && !slices.Contains(flags, "a //go:linkname directive") {
					flags = append(flags, "a //go:linkname directive")
				}
			}
		}
		for _, is := range f.Imports {
			switch ip, _ := strconv.Unquote(is.Path.Value); {
			case ip == "unsafe" || ip == "C":
				flags = append(flags, "an import of "+ip)
			case is.Name != nil && is.Name.Name == "." && slices.Contains(sensitive, ip):
				flags = append(flags, "a dot import of "+ip)
			}
		}
		for _, d := range f.Decls {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			c := s.count(d)
			fa.exits, fa.forged, fa.twentyTwos = fa.exits+c.exits, fa.forged+c.forged, fa.twentyTwos+c.twentyTwos
			if c.reflect > 0 && !slices.Contains(flags, "reflection on a testing value") {
				flags = append(flags, "reflection on a testing value")
			}
			fn, _ := d.(*ast.FuncDecl)
			fname := ""
			if fn != nil && fn.Recv == nil && fn.Body != nil {
				fname = fn.Name.Name
			}
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR || fname == "init" {
				inits = append(inits, d)
			}
			switch {
			case test && fname == "TestMain" && slices.ContainsFunc(fn.Type.Params.List, func(f *ast.Field) bool {
				st, ok := f.Type.(*ast.StarExpr)
				return ok && s.qual(st.X) == "testing.M"
			}):
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
	return s.reach(inits, fa)
}

// reach follows initialization (inits and package variables, test files
// included) through the package's functions, to a bound, counting exits,
// output and calls it cannot resolve (interfaces, function values, no body,
// a changed in-module package). Other packages' calls are taken not to
// exit, except the known exits.
func (s *pkgScan) reach(starts []ast.Node, fa *pkgFacts) error {
	seen := map[*ast.FuncDecl]bool{}
	for len(starts) > 0 {
		if err := s.r.ctx.Err(); err != nil {
			return err
		} else if len(seen) > maxReach {
			fa.init["a call it cannot resolve (past the guard's bound)"]++
			return nil
		}
		n := starts[0]
		starts = starts[1:]
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if q := s.qual(x); slices.Contains(exitFns, q) || slices.Contains(outputs, q) {
					fa.init[map[bool]string{true: "an exit", false: "output"}[slices.Contains(exitFns, q)]+" ("+q+")"]++
				}
			case *ast.CallExpr:
				switch k, d := s.resolve(x); k {
				case "follow":
					if !seen[d] {
						seen[d], starts = true, append(starts, d.Body)
					}
				case "output", "a call it cannot resolve":
					fa.init[k+" ("+types.ExprString(x.Fun)+")"]++
				}
			}
			return true
		})
	}
	return nil
}

func (s *pkgScan) resolve(call *ast.CallExpr) (string, *ast.FuncDecl) {
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}
	if tv, ok := s.info.Types[fun]; ok && tv.IsType() {
		return "", nil // a conversion
	}
	var fn *types.Func
	switch f := fun.(type) {
	case *ast.FuncLit:
		return "", nil // inspected in place
	case *ast.Ident:
		switch o := s.info.Uses[f].(type) {
		case *types.Builtin:
			return map[bool]string{true: "output"}[o.Name() == "print" || o.Name() == "println"], nil
		case *types.Func:
			fn = o
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			if pn, ok := s.info.Uses[id].(*types.PkgName); ok {
				p := pn.Imported().Path()
				if dir, ok := strings.CutPrefix(p, s.r.mod+"/"); s.r.mod != "" && (ok && s.r.touched[dir] || p == s.r.mod && s.r.touched["."]) {
					return "a call it cannot resolve", nil
				}
				return "", nil
			}
		}
		sel := s.info.Selections[f]
		if sel == nil && !s.methods[f.Sel.Name] {
			return "", nil // a method of a type from an import the guard does not load
		} else if sel == nil || sel.Kind() != types.MethodVal { // perhaps this package's method, or a func field
			return "a call it cannot resolve", nil
		}
		fn = sel.Obj().(*types.Func)
	}
	if fn == nil {
		return "a call it cannot resolve", nil
	}
	if d := s.decls[fn.Origin()]; d != nil && d.Body != nil {
		return "follow", d
	} else if d != nil || fn.Pkg() == s.pkg {
		return "a call it cannot resolve", nil // no body (assembly, linkname), or an interface method
	}
	return "", nil
}

// output: an example's output comment (its body's last comment), its
// assertion; the comments are sorted, so one search finds it.
func output(f *ast.File, fn *ast.FuncDecl) int {
	i := sort.Search(len(f.Comments), func(i int) bool { return f.Comments[i].Pos() > fn.Body.Rbrace })
	if !strings.HasPrefix(fn.Name.Name, "Example") || i == 0 || f.Comments[i-1].Pos() < fn.Body.Lbrace {
		return 0
	}
	t := strings.ToLower(strings.TrimSpace(f.Comments[i-1].Text()))
	return b2i(strings.HasPrefix(t, "output:") || strings.HasPrefix(t, "unordered output:"))
}

// goIgnores is a root go.mod's ignore directives, comments dropped.
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
