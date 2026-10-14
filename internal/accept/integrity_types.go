// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The guard's real types (P14e, #226 entries 79 and 82). The module's own
// packages are type-checked here, from the version's blobs, with the files
// the gate builds on typePlatform. Every other import (the standard library,
// the module cache) is read from the export data go list writes in a
// directory holding only go.mod and go.sum: the go command never reads the
// tree's Go source. A package whose imports cannot all be loaded, in the
// base or the candidate, is unverifiable.
var (
	maxPkgs         = 2000      // own packages one version type-checks
	maxExport int64 = 256 << 20 // export data bytes one version reads
	maxList   int64 = 16 << 20  // go list's output
	maxListed       = 64        // go list results a LocalLister keeps
	// typePlatform is the gate platform the types are read for: the host's,
	// whose standard library the toolchain's cache holds.
	typePlatform = platforms[max(0, slices.IndexFunc(platforms, func(p [2]string) bool { return p[1] == runtime.GOARCH }))]
	// failSigs are the failure-setting methods: an interface holding one
	// turns a value asserted to it into an assertion the guard counts.
	failSigs = func() map[string]*types.Signature {
		sig := func(variadic bool, ps ...types.Type) *types.Signature {
			var vs []*types.Var
			for _, p := range ps {
				vs = append(vs, types.NewParam(token.NoPos, nil, "", p))
			}
			return types.NewSignatureType(nil, nil, nil, types.NewTuple(vs...), nil, variadic)
		}
		args := types.NewSlice(types.Universe.Lookup("any").Type())
		return map[string]*types.Signature{"Error": sig(true, args), "Fatal": sig(true, args), "Fail": sig(false), "FailNow": sig(false),
			"Errorf": sig(true, types.Typ[types.String], args), "Fatalf": sig(true, types.Typ[types.String], args)}
	}()
)

// Listed is one package go list reads: its export data file, or its error.
type Listed struct {
	ImportPath, Export string
	Error              *struct{ Err string }
}

// Lister lists paths and their dependencies with their export data, for
// typePlatform, in the module whose go.mod and go.sum are given (nil: none).
type Lister func(ctx context.Context, gomod, gosum []byte, paths []string) (map[string]Listed, error)

// LocalLister is a Lister running the go command gobin with env added (its
// caches: GOCACHE, GOMODCACHE, HOME), offline: no proxy, no checksum
// database, no workspace, no other toolchain, no cgo, go.mod read-only. A
// run is bounded by timeout, its output by maxList and each process's
// address space by mem bytes (0: unbounded); a cancelled run kills go. A
// go.mod replacing a module by a directory is refused: only the module
// cache's source is read. Results are kept by go.mod, go.sum and paths.
func LocalLister(gobin string, env []string, timeout time.Duration, mem int64) Lister {
	var mu sync.Mutex
	kept := map[[32]byte]map[string]Listed{}
	return func(ctx context.Context, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
		h := sha256.New()
		for _, b := range append([][]byte{gomod, gosum}, []byte(strings.Join(paths, "\n"))) {
			_ = binary.Write(h, binary.BigEndian, int64(len(b)))
			h.Write(b)
		}
		key := [32]byte(h.Sum(nil))
		mu.Lock()
		got, ok := kept[key]
		mu.Unlock()
		if ok {
			return got, nil
		}
		got, err := goList(ctx, gobin, env, timeout, mem, gomod, gosum, paths)
		if err == nil {
			mu.Lock()
			if len(kept) >= maxListed {
				clear(kept)
			}
			kept[key] = got
			mu.Unlock()
		}
		return got, err
	}
}

func goList(ctx context.Context, gobin string, env []string, timeout time.Duration, mem int64, gomod, gosum []byte, paths []string) (map[string]Listed, error) {
	if dirReplace(gomod) {
		return nil, errors.New("go.mod replaces a module by a directory")
	}
	for _, p := range paths {
		if p == "" || strings.ContainsAny(p[:1], "-./") || strings.ContainsFunc(p, func(r rune) bool {
			return r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-._~+/", r)
		}) {
			return nil, fmt.Errorf("an import path go list cannot take (%q)", p)
		}
	}
	dir, err := os.MkdirTemp("", "adc-golist-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if gomod == nil {
		gomod = []byte("module adc.invalid/guard\n")
	}
	err = os.WriteFile(filepath.Join(dir, "go.mod"), gomod, 0o600)
	if err == nil && gosum != nil {
		err = os.WriteFile(filepath.Join(dir, "go.sum"), gosum, 0o600)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limit := "unlimited"
	if mem > 0 {
		limit = strconv.FormatInt(mem>>10, 10)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `ulimit -v "$1" && shift && exec "$@"`, "sh", limit,
		gobin, "list", "-e", "-export", "-deps", "-json=ImportPath,Export,Error", "--"}, paths...)...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), "PATH="+filepath.Dir(gobin)+":/usr/bin:/bin", "GOENV=off", "GOFLAGS=-mod=readonly",
		"GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "CGO_ENABLED=0", "GO111MODULE=on",
		"GOOS="+typePlatform[0], "GOARCH="+typePlatform[1]) // last: they win over env's
	out, stderr, err := drain(cmd, maxList, maxStderr)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("go list: %w", context.Cause(ctx))
	} else if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, bytes.TrimSpace(stderr[:min(len(stderr), 512)]))
	}
	got := map[string]Listed{}
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var l Listed
		if err := dec.Decode(&l); errors.Is(err, io.EOF) {
			return got, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list: %w", err)
		}
		got[l.ImportPath] = l
	}
}

// dirReplace: gomod replaces a module by a directory (./x, ../x, /x).
func dirReplace(gomod []byte) bool {
	for _, l := range strings.Split(string(gomod), "\n") {
		l, _, _ = strings.Cut(l, "//")
		if _, rhs, ok := strings.Cut(l, "=>"); ok && len(strings.Fields(rhs)) > 0 {
			if to := strings.Trim(strings.Fields(rhs)[0], "\"`"); to == "." || to == ".." || strings.HasPrefix(to, "./") ||
				strings.HasPrefix(to, "../") || strings.HasPrefix(to, "/") {
				return true
			}
		}
	}
	return false
}

// tree is one version of the repository: paths to blobs, by directory, and
// a typer per module root.
type tree struct {
	files  map[string]string
	dirs   map[string][]string
	typers map[string]*typer
}

// typer is one module's packages in one version: its own type-checked here,
// the rest from export data.
type typer struct {
	r            *run
	t            *tree
	root, mod    string // the module's directory and path ("": no module)
	gomod, gosum []byte
	why          string // why nothing loads (the module unread)
	fset         *token.FileSet
	gc           types.Importer
	export       int64
	listed       map[string]Listed
	parsed       map[string][]*ast.File    // own packages' files, by directory
	pkgs         map[string]*types.Package // own packages, by import path (nil: being checked)
	failed       map[string]string         // import paths not loaded, and why
	self         map[string]*types.Package // analysed packages with their tests, for their external tests
}

// typer is the typer of dir's module (the nearest go.mod above it).
func (t *tree) typer(r *run, dir string) *typer {
	root := ""
	for d := dir; root == ""; d = path.Dir(d) {
		if t.files[path.Join(d, "go.mod")] != "" {
			root = d
		} else if d == "." {
			break
		}
	}
	if ty := t.typers[root]; ty != nil {
		return ty
	}
	ty := &typer{r: r, t: t, root: root, fset: token.NewFileSet(), export: maxExport, listed: map[string]Listed{},
		parsed: map[string][]*ast.File{}, pkgs: map[string]*types.Package{}, failed: map[string]string{}, self: map[string]*types.Package{}}
	ty.gc = importer.ForCompiler(ty.fset, "gc", ty.lookup)
	t.typers[root] = ty
	read := func(name string) []byte {
		blob := t.files[path.Join(root, name)]
		if blob == "" || ty.why != "" {
			return nil
		}
		b, why, err := r.file(blob)
		ty.why = cmp.Or(why, errText(err))
		return b
	}
	ty.gomod, ty.gosum = read("go.mod"), read("go.sum")
	for _, l := range strings.Split(string(ty.gomod), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "module" && ty.mod == "" {
			ty.mod = strings.Trim(f[1], "\"`")
		}
	}
	return ty
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// local is the directory of an own package's import path, if one.
func (ty *typer) local(p string) (string, bool) {
	rel, ok := strings.CutPrefix(p, ty.mod)
	if ty.mod == "" || !ok || rel != "" && (rel[0] != '/' || path.Clean(rel) != rel) {
		return "", false
	}
	dir := path.Join(ty.root, strings.TrimPrefix(rel, "/"))
	for d := dir; d != ty.root; d = path.Dir(d) {
		if ty.t.files[path.Join(d, "go.mod")] != "" { // a nested module's
			return "", false
		}
	}
	return dir, true
}

// pathOf is the import path of the own package in dir.
func (ty *typer) pathOf(dir string) string {
	if ty.mod == "" || dir == ty.root {
		return ty.mod
	} else if ty.root == "." {
		return ty.mod + "/" + dir
	}
	return ty.mod + "/" + strings.TrimPrefix(dir, ty.root+"/")
}

// parse reads the files the gate builds of the own package in dir (no
// tests, no cgo), once; why says why not.
func (ty *typer) parse(dir string) ([]*ast.File, string) {
	if fs, ok := ty.parsed[dir]; ok {
		return fs, ty.failed["dir "+dir]
	}
	ty.parsed[dir] = nil
	why := ""
	switch {
	case len(ty.parsed) > maxPkgs:
		why = "over the guard's package bound"
	case ty.r.ctx.Err() != nil:
		why = ty.r.ctx.Err().Error()
	}
	bctx := build.Context{GOOS: typePlatform[0], GOARCH: typePlatform[1], Compiler: "gc", ReleaseTags: release}
	if typePlatform[1] == runtime.GOARCH {
		bctx.ToolTags = build.Default.ToolTags
	}
	name := ""
	for _, p := range ty.t.dirs[dir] {
		if why != "" || path.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, unread, err := ty.r.file(ty.t.files[p])
		if why = cmp.Or(unread, errText(err)); why != "" {
			break
		}
		bctx.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		ok, err := bctx.MatchFile(dir, path.Base(p))
		if !ok || err != nil {
			continue
		}
		f, err := parser.ParseFile(ty.fset, p, b, parser.SkipObjectResolution)
		switch {
		case err != nil:
			why = err.Error()
		case f.Name.Name == "documentation" || slices.ContainsFunc(f.Imports, func(is *ast.ImportSpec) bool { return is.Path.Value == `"C"` }):
		case name != "" && name != f.Name.Name:
			why = "files of packages " + name + " and " + f.Name.Name
		default:
			name, ty.parsed[dir] = f.Name.Name, append(ty.parsed[dir], f)
		}
	}
	if why == "" && name == "" {
		why = "no Go files the gate builds"
	}
	if why != "" {
		ty.failed["dir "+dir] = why
	}
	return ty.parsed[dir], why
}

// need lists, once, what files import outside the module, through the own
// packages they import.
func (ty *typer) need(files []*ast.File) error {
	seen, ext := map[string]bool{}, []string{}
	var walk func([]*ast.File)
	walk = func(fs []*ast.File) {
		for _, f := range fs {
			for _, is := range f.Imports {
				p, _ := strconv.Unquote(is.Path.Value)
				if dir, own := ty.local(p); !seen[p] && own {
					seen[p] = true
					fs, _ := ty.parse(dir)
					walk(fs)
				} else if _, ok := ty.listed[p]; !seen[p] && !ok && p != "C" && p != "unsafe" {
					seen[p], ext = true, append(ext, p)
				}
			}
		}
	}
	walk(files)
	if len(ext) == 0 || ty.why != "" {
		return nil
	}
	slices.Sort(ext)
	got, err := map[string]Listed{}, errors.New("no Go type loader")
	if ty.r.lister != nil {
		got, err = ty.r.lister(ty.r.ctx, ty.gomod, ty.gosum, ext)
	}
	if ctxErr := ty.r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	for p, l := range got {
		ty.listed[p] = l
	}
	for _, p := range ext {
		if l := ty.listed[p]; l.Export == "" && l.Error == nil {
			ty.listed[p] = Listed{ImportPath: p, Error: &struct{ Err string }{cmp.Or(errText(err), "not listed")}}
		}
	}
	return nil
}

// lookup opens a listed package's export data, within the version's bound.
func (ty *typer) lookup(p string) (io.ReadCloser, error) {
	l := ty.listed[p]
	if l.Export == "" && l.Error != nil {
		return nil, errors.New(l.Error.Err)
	} else if l.Export == "" {
		return nil, errors.New("no export data")
	}
	fi, err := os.Stat(l.Export)
	if err != nil {
		return nil, err
	} else if ty.export -= fi.Size(); ty.export < 0 {
		return nil, errors.New("over the guard's export data bound")
	}
	return os.Open(l.Export)
}

// Import is an import's package: an analysed one with its tests, an own one
// type-checked (bodies skipped; any error fails it), or export data.
func (ty *typer) Import(p string) (*types.Package, error) {
	if ty.why != "" {
		return nil, errors.New(ty.why)
	} else if pkg := ty.self[p]; pkg != nil {
		return pkg, nil
	}
	dir, own := ty.local(p)
	if !own {
		return ty.gc.Import(p)
	} else if why := ty.failed[p]; why != "" {
		return nil, errors.New(why)
	} else if pkg, ok := ty.pkgs[p]; ok && pkg == nil {
		return nil, errors.New("an import cycle")
	} else if ok {
		return pkg, nil
	}
	files, why := ty.parse(dir)
	var first error
	if why == "" {
		ty.pkgs[p] = nil
		conf := types.Config{Importer: ty, FakeImportC: true, IgnoreFuncBodies: true, Error: func(err error) { first = cmp.Or(first, err) }}
		ty.pkgs[p], _ = conf.Check(p, ty.fset, files, nil)
	}
	if first != nil {
		why = "it does not type-check: " + first.Error()
	}
	if why != "" {
		delete(ty.pkgs, p)
		ty.failed[p] = why
		return nil, errors.New(why)
	}
	return ty.pkgs[p], nil
}

type importFunc func(string) (*types.Package, error)

func (f importFunc) Import(p string) (*types.Package, error) { return f(p) }

// typed type-checks one package of dir with real imports and adds what the
// types show to fa: testing values made or laundered (forged), reflection
// reaching a testing type (a flag). Imports not loaded: unverifiable.
func (r *run) typed(dir, name string, fset *token.FileSet, files []*ast.File, meta map[*ast.File]fileMeta, fa *pkgFacts) error {
	ty := fa.tree.typer(r, dir)
	if err := ty.need(files); err != nil {
		return err
	}
	failed, xtest, suffix := "", strings.HasSuffix(name, "_test"), ""
	if xtest {
		suffix = "_test"
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Instances: map[*ast.Ident]types.Instance{}}
	conf := types.Config{FakeImportC: true, Error: func(error) {}, Importer: importFunc(func(p string) (*types.Package, error) {
		pkg, err := ty.Import(p)
		if err != nil && failed == "" {
			failed = p + ": " + err.Error()
		}
		return pkg, err
	})}
	pkg, _ := conf.Check(cmp.Or(ty.pathOf(dir), dir)+suffix, fset, files, info)
	if err := r.ctx.Err(); err != nil {
		return err
	} else if failed != "" {
		fa.untyped[dir+":"+name] = "the types of its imports not loaded (" + failed[:min(len(failed), 300)] + ")"
		return nil
	} else if !xtest && ty.pathOf(dir) != "" {
		ty.self[ty.pathOf(dir)] = pkg
	}
	fa.forged += made(info, files)
	for _, f := range files {
		if p := meta[f].path; reflects(info, f) && !slices.Contains(fa.flags[p], "reflection on a testing value") {
			fa.flags[p] = append(fa.flags[p], "reflection on a testing value")
		}
	}
	return nil
}

// made counts the testing values the real types show made: an expression,
// variable, type or type argument holding a T, B, F or M by value (a type
// directly behind * excepted); a value asserted, switched or converted to a
// pointer to one, or to an interface with a failure-setting method; and a
// type argument a generic could assert a value to (as[*testing.T](v)).
// Types are unaliased: an alias in another, unchanged package is seen.
func made(info *types.Info, files []*ast.File) (n int) {
	starred := map[ast.Expr]bool{}
	for _, f := range files {
		ast.Inspect(f, func(x ast.Node) bool {
			switch x := x.(type) {
			case *ast.StarExpr:
				for e := x.X; ; {
					starred[e] = true
					p, ok := e.(*ast.ParenExpr)
					if !ok {
						break
					}
					e = p.X
				}
			case *ast.TypeAssertExpr:
				n += b2i(x.Type != nil && launders(info.TypeOf(x.Type)))
			case *ast.TypeSwitchStmt:
				for _, c := range x.Body.List {
					for _, e := range c.(*ast.CaseClause).List {
						n += b2i(info.Types[e].IsType() && launders(info.Types[e].Type))
					}
				}
			case *ast.CallExpr: // a conversion to *testing.T, of anything else
				if tv := info.Types[ast.Unparen(x.Fun)]; tv.IsType() && len(x.Args) == 1 {
					p, ok := types.Unalias(tv.Type).Underlying().(*types.Pointer)
					from := info.TypeOf(x.Args[0])
					n += b2i(ok && holds(p.Elem(), false, nil) && (from == nil || !types.Identical(from, tv.Type)))
				}
			}
			return true
		})
	}
	for e, tv := range info.Types {
		n += b2i((tv.IsValue() || tv.IsType() && !starred[e]) && holds(tv.Type, false, nil))
	}
	for _, o := range info.Defs {
		switch o.(type) {
		case *types.Var, *types.TypeName:
			n += b2i(holds(o.Type(), false, nil))
		}
	}
	for _, in := range info.Instances {
		n += b2i(slices.ContainsFunc(slices.Collect(in.TypeArgs.Types()), func(t types.Type) bool { return holds(t, false, nil) || launders(t) }))
	}
	return n
}

// launders: a value asserted or switched to t is a testing value to the
// guard: a pointer to one, or an interface with a failure-setting method
// (testing.TB, interface{ Error(...any) }, a type parameter so constrained).
func launders(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.TypeParam); ok {
		t = p.Constraint()
	}
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Pointer:
		return holds(u.Elem(), false, nil)
	case *types.Interface:
		for i := range u.NumMethods() {
			if s := failSigs[u.Method(i).Name()]; s != nil && types.Identical(u.Method(i).Type(), s) {
				return true
			}
		}
	}
	return false
}

// holds: t holds a testing.T, B, F or M (or their common part) by value,
// through arrays, slices, maps, chans, fields and signatures; ptr, also
// behind pointers, and testing.TB.
func holds(t types.Type, ptr bool, seen map[types.Type]bool) bool {
	if t = types.Unalias(t); t == nil || seen[t] {
		return false
	} else if seen == nil {
		seen = map[types.Type]bool{}
	}
	seen[t] = true
	if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "testing" &&
		(slices.Contains([]string{"T", "B", "F", "M", "common"}, n.Obj().Name()) || ptr && n.Obj().Name() == "TB") {
		return true
	}
	var in []types.Type
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := range u.NumFields() {
			in = append(in, u.Field(i).Type())
		}
	case *types.Map:
		in = []types.Type{u.Key(), u.Elem()}
	case *types.Signature:
		in = []types.Type{u.Params(), u.Results()}
	case *types.Tuple:
		for i := range u.Len() {
			in = append(in, u.At(i).Type())
		}
	case *types.Pointer:
		if ptr {
			in = []types.Type{u.Elem()}
		}
	case interface{ Elem() types.Type }: // array, slice, chan
		in = []types.Type{u.Elem()}
	}
	return slices.ContainsFunc(in, func(t types.Type) bool { return t != nil && holds(t, ptr, seen) })
}

// reflects: f calls into reflect with an argument whose type holds a
// testing type at any depth, behind pointers too (a struct's *testing.T).
func reflects(info *types.Info, f *ast.File) (found bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && !found {
			fun, _ := fn(c.Fun)
			id := ident(fun)
			if sel, ok := fun.(*ast.SelectorExpr); ok {
				id = sel.Sel
			}
			o, _ := info.Uses[id].(*types.Func)
			found = o != nil && o.Pkg() != nil && o.Pkg().Path() == "reflect" &&
				slices.ContainsFunc(c.Args, func(a ast.Expr) bool { return holds(info.TypeOf(a), true, nil) })
		}
		return !found
	})
	return found
}
