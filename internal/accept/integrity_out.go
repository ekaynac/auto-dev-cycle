// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// P14b-out-1: operator decision A outside initialization and the TestMain
// hook, as an additive layer (#226 entries 75 and 76; #276, split under
// 04 §2). It runs once the base guard has analysed every package, reads the
// touched packages again on its own, with its own work bounds, and never
// writes the base guard's maps, keys, counters or listing. Its findings are
// Additive: the report gives them only what the base guard's leave.

// additive is the layer's state.
type additive struct {
	blocks, warnings []Block
	nodes, tags      int // its work: AST nodes and items handled; tag visits
	over             bool
	files            map[string]*outFile // by blob, name, directory and version
	std, built       map[string]bool     // by path; by blob and name
}

// outFile is what one Go file holds for the layer.
type outFile struct {
	refs    map[string]int // "package\x00function\x00name": references into outS
	dots    []string       // packages in outS it dot-imports
	unknown []string       // packages in outS imported under a name the layer cannot learn
	imports []string
}

// outHook runs the layer (a test turns it off to compare the base guard's
// findings with and without it).
var outHook = true

// The layer's bounds, sized so that no input the base guard accepts can
// exhaust them (Astra #276 r4 H2). Tags: the layer evaluates a file's
// constraints once a blob and name, on the lines the base guard's goOn
// keeps (deduplicated), platform by platform until one builds it, and an
// assignment until its first false line; the base guard evaluates them for
// each version, every platform and every line, charging maxWork. So the
// layer visits fewer tags than an accepted input cost the base guard.
// Nodes: a node of Go's syntax holds a token of its own (one byte at least)
// or wraps one node that does (ExprStmt, FuncType of a FuncDecl): two nodes
// a byte at most. The layer parses each version once and each blob was read
// once, within maxRead in all, so it visits at most 4 × maxRead nodes; the
// names it keys charge their bytes (2 × maxRead at most) and the items it
// handles are its keys (fewer than its nodes): 10 × maxRead at most, under
// 16 × maxRead.
var (
	maxOutTags  = maxWork
	maxOutNodes = int(8 * 2 * maxRead)
)

// outS is S beyond the base guard's sPkg: flag, io/ioutil, net/http/cgi
// and the os/, log/, runtime/, testing/ and modernc.org/libc trees.
func outS(p string) bool {
	return !sPkg(p) && (slices.Contains([]string{"flag", "io/ioutil", "net/http/cgi"}, p) ||
		slices.ContainsFunc([]string{"os", "log", "runtime", "testing", "modernc.org/libc"}, func(t string) bool { return p == t || strings.HasPrefix(p, t+"/") }))
}

// libc: C's exit, exec and system calls in Go: class 2, every name.
func libc(p string) bool { return p == "modernc.org/libc" || strings.HasPrefix(p, "modernc.org/libc/") }

func (r *run) outBlock(p, what string) {
	r.out.blocks = append(r.out.blocks, Block{vocab.ReasonTestIntegrityViolation, p, what, true})
}

func (r *run) outWarn(p, what string) {
	r.out.warnings = append(r.out.warnings, Block{vocab.ReasonTestIntegrityViolation, p, what, true})
}

// charge counts the layer's work: past a bound, one Additive block and no
// more of its work; a done context stops it with no block.
func (r *run) charge(nodes, tags int) bool {
	r.out.nodes, r.out.tags = r.out.nodes+nodes, r.out.tags+tags
	if past := r.out.nodes > maxOutNodes || r.out.tags > maxOutTags; past && !r.out.over && r.ctx.Err() == nil {
		r.outBlock("", "unverifiable: decision A's outside-initialization check is over its work bound")
	}
	r.out.over = r.out.over || r.out.nodes > maxOutNodes || r.out.tags > maxOutTags || r.ctx.Err() != nil
	return !r.out.over
}

// bound is a name cut once, where it is declared: 64 bytes and a hash.
func bound(name string) string {
	if len(name) <= 64 {
		return name
	}
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s…%x", name[:64], h[:16])
}

// std: p is the standard library's, by the GOROOT the guard runs with (its
// loader lists with the same toolchain); unknown is not.
func (r *run) std(p string) bool {
	if ok, seen := r.out.std[p]; seen {
		return ok
	}
	first, _, _ := strings.Cut(p, "/")
	fi, err := os.Stat(filepath.Join(build.Default.GOROOT, "src", filepath.FromSlash(p)))
	r.out.std[p] = build.Default.GOROOT != "" && !strings.Contains(first, ".") && err == nil && fi.IsDir()
	return r.out.std[p]
}

// outBuilt: a gate platform may build p, as the base guard's goOn and holds
// read it: its name by go/build; its constraint lines deduplicated, true for
// some assignment of the tags the guard does not model (up to 12; more:
// built); ignore false.
func (r *run) outBuilt(p string, src []byte) bool {
	var exprs []constraint.Expr
	seen := map[string]bool{}
	for _, l := range header(src) {
		if x, err := constraint.Parse(l); err == nil && !seen[l] {
			seen[l], exprs = true, append(exprs, x)
		}
	}
	return slices.ContainsFunc(platforms, func(pl [2]string) bool {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release, // its name: go/build's rules
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
		if ok, err := ctx.MatchFile(path.Dir(p), path.Base(p)); !ok && err == nil {
			return false
		}
		known := func(tag string) (bool, bool) {
			switch {
			case tag == pl[0] || tag == pl[1] || tag == "unix" || tag == "gc" || slices.Contains(release, tag):
				return true, true
			case tag == "ignore" || slices.Contains(otherPlatforms, tag) || strings.HasPrefix(tag, "go1.") ||
				slices.ContainsFunc(platforms, func(q [2]string) bool { return q[0] == tag || q[1] == tag }):
				return false, true
			}
			return false, false
		}
		var free []string
		for _, x := range exprs {
			x.Eval(func(tag string) bool {
				if _, ok := known(tag); !ok && !slices.Contains(free, tag) {
					free = append(free, tag)
				}
				r.charge(0, 1)
				return false
			})
		}
		if len(free) > 12 {
			return true
		}
		for m := 0; m < 1<<len(free) && !r.out.over; m++ {
			if !slices.ContainsFunc(exprs, func(x constraint.Expr) bool {
				return !x.Eval(func(tag string) bool {
					r.charge(0, 1)
					v, k := known(tag)
					return k && v || !k && m>>slices.Index(free, tag)&1 == 1
				})
			}) {
				return true
			}
		}
		return false
	})
}

// outFileOf reads and parses p (blob) of dir in t once, as the base guard
// parses it; a file the layer cannot read or parse is an Additive block (the
// base guard's own block covers it too); nil if no gate platform builds it.
func (r *run) outFileOf(p, dir, blob string, t *tree) *outFile {
	key := blob + "\x00" + p + "\x00" + fmt.Sprintf("%p", t)
	if f, ok := r.out.files[key]; ok {
		return f
	}
	r.out.files[key] = nil
	bk := blob + "\x00" + p
	fail := func(what string) *outFile { // once a blob and name, unless the base guard blocks it already
		if _, seen := r.out.built[bk]; !seen && !slices.ContainsFunc(r.blocks, func(x Block) bool { return x.Path == p }) {
			r.outBlock(p, "unverifiable: decision A's outside-initialization check could not "+what+" it")
		}
		r.out.built[bk] = false
		return nil
	}
	b, why, err := r.file(blob)
	if why != "" || err != nil {
		return fail("read")
	}
	built, ok := r.out.built[bk]
	if !ok { // once a blob and name, as the base guard's goOn
		built = r.outBuilt(p, b)
		r.out.built[bk] = built
	}
	if !built {
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, b, parser.ParseComments|parser.SkipObjectResolution) // the base guard's mode
	if err != nil {
		delete(r.out.built, bk)
		return fail("parse")
	}
	of := &outFile{refs: map[string]int{}}
	names, testing := map[string]string{}, ""
	for _, is := range f.Imports {
		ip, _ := strconv.Unquote(is.Path.Value)
		name := ""
		switch {
		case is.Name != nil:
			name = is.Name.Name
		case r.std(ip):
			name = path.Base(ip) // a standard package is named by its last element
		default:
			name = r.loadedName(t, dir, ip)
		}
		switch {
		case name == "." && outS(ip):
			of.dots = append(of.dots, ip)
		case name == "" && outS(ip):
			of.unknown = append(of.unknown, ip)
		case outS(ip):
			names[name] = ip
		case ip == "testing":
			testing = cmp.Or(name, "testing")
		}
		of.imports = append(of.imports, ip)
	}
	for _, d := range f.Decls {
		fn, _ := d.(*ast.FuncDecl)
		where, lits := "package level", false
		switch {
		case fn != nil && fn.Recv == nil && (fn.Name.Name == "init" || strings.HasSuffix(p, "_test.go") && fn.Name.Name == "TestMain" && hook(fn, testing)):
			where, lits = "init literal", true // initialization and the hook are the base guard's; the literals in them run later
		case fn != nil && fn.Recv != nil && r.charge(len(fn.Name.Name)+len(recv(fn)), 0):
			where = bound(recv(fn) + "." + fn.Name.Name)
		case fn != nil && r.charge(len(fn.Name.Name), 0):
			where = bound(fn.Name.Name)
		}
		r.refsIn(d, names, lits, func(pkg, name string) { of.refs[pkg+"\x00"+where+"\x00"+bound(name)]++ })
	}
	r.out.files[key] = of
	return of
}

// loadedName is the declared name of ip as the base guard's loader read it
// for t and dir (its own and vendored packages); "" if it did not.
func (r *run) loadedName(t *tree, dir, ip string) string {
	for _, pl := range platforms {
		if p := t.typer(r, dir, pl).pkgs[ip]; p != nil {
			return p.Name()
		}
	}
	return ""
}

// refsIn calls add for each reference in n to a package names maps it to
// (only inside function literals with lits), a local that shadows the
// import name, in a scope open there, being no reference. Scopes are
// tracked lexically, as the language declares them: a := or var name from
// the end of its statement, parameters and range variables in the body.
func (r *run) refsIn(n ast.Node, names map[string]string, lits bool, add func(pkg, name string)) {
	var stack []ast.Node
	var scopes [][]string // the import names each open scope declares
	shadow := map[string]int{}
	inLit := 0
	opens := func(x ast.Node) bool {
		switch x.(type) {
		case *ast.FuncDecl, *ast.FuncLit, *ast.BlockStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.CaseClause, *ast.CommClause:
			return true
		}
		return false
	}
	declare := func(es ...ast.Expr) {
		for _, e := range es {
			if id, ok := e.(*ast.Ident); ok && names[id.Name] != "" && len(scopes) > 0 {
				scopes[len(scopes)-1], shadow[id.Name] = append(scopes[len(scopes)-1], id.Name), shadow[id.Name]+1
			}
		}
	}
	fields := func(fls ...*ast.FieldList) (es []ast.Expr) {
		for _, fl := range slices.DeleteFunc(fls, func(fl *ast.FieldList) bool { return fl == nil }) {
			for _, f := range fl.List {
				for _, id := range f.Names {
					es = append(es, id)
				}
			}
		}
		return es
	}
	ast.Inspect(n, func(x ast.Node) bool {
		if x == nil {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			switch top := top.(type) {
			case *ast.AssignStmt:
				if top.Tok == token.DEFINE {
					declare(top.Lhs...)
				}
			case *ast.ValueSpec:
				for _, id := range top.Names {
					declare(id)
				}
			case *ast.FuncLit:
				inLit--
			}
			if opens(top) {
				for _, name := range scopes[len(scopes)-1] {
					shadow[name]--
				}
				scopes = scopes[:len(scopes)-1]
			}
			return true
		} else if !r.charge(1, 0) {
			return false
		}
		if b, ok := x.(*ast.BlockStmt); ok && len(stack) > 0 { // a body: its function's parameters, its range's variables
			defer func() {
				switch p := stack[len(stack)-2].(type) {
				case *ast.FuncDecl:
					declare(fields(p.Recv, p.Type.Params, p.Type.Results)...)
				case *ast.FuncLit:
					declare(fields(p.Type.Params, p.Type.Results)...)
				case *ast.RangeStmt:
					if p.Tok == token.DEFINE && p.Body == b {
						declare(p.Key, p.Value)
					}
				}
			}()
		}
		stack = append(stack, x)
		if opens(x) {
			scopes = append(scopes, nil)
		}
		switch x := x.(type) {
		case *ast.FuncLit:
			inLit++
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && (!lits || inLit > 0) && names[id.Name] != "" && shadow[id.Name] == 0 {
				add(names[id.Name], x.Sel.Name)
			}
		}
		return true
	})
}

// hook: fn's one parameter is *testing.M (testing imported as name).
func hook(fn *ast.FuncDecl, name string) bool {
	if fn.Type.Params.NumFields() != 1 {
		return false
	}
	st, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == name && sel.Sel.Name == "M"
}

// outFacts is one version of a package directory, as the layer reads it.
type outFacts struct {
	refs    map[string]int  // "file\x00package\x00function\x00name"
	dots    map[string]bool // "file\x00package"
	unknown map[string]bool // "file\x00package"
	imports map[string]bool
}

func (r *run) outRead(dir string, t *tree) outFacts {
	fa := outFacts{refs: map[string]int{}, dots: map[string]bool{}, unknown: map[string]bool{}, imports: map[string]bool{}}
	for _, p := range t.dirs[dir] {
		if path.Ext(p) != ".go" || r.out.over {
			continue // never a non-Go blob
		}
		f := r.outFileOf(p, dir, t.files[p], t)
		if f == nil {
			continue
		}
		for k, n := range f.refs {
			fa.refs[p+"\x00"+k] += n
		}
		for _, d := range f.dots {
			fa.dots[p+"\x00"+d] = true
		}
		for _, d := range f.unknown {
			fa.unknown[p+"\x00"+d] = true
		}
		for _, ip := range f.imports {
			fa.imports[ip] = true
		}
	}
	return fa
}

// outRun runs the layer once the base guard is done with every package:
// a panic is the layer's Additive block; a done context, the run's error.
func (r *run) outRun() (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.outBlock("", fmt.Sprintf("unverifiable: decision A's outside-initialization check failed (%v)", p))
		}
	}()
	r.out.files, r.out.std, r.out.built = map[string]*outFile{}, map[string]bool{}, map[string]bool{}
	r.outPkgs()
	r.outModule()
	return r.ctx.Err()
}

func (r *run) outPkgs() {
	for _, dir := range slices.Sorted(maps.Keys(r.touched)) {
		b, a := r.outRead(dir, r.trees[0]), r.outRead(dir, r.trees[1])
		if r.out.over || !r.charge(len(b.refs)+len(a.refs)+len(a.dots)+len(a.unknown)+len(a.imports), 0) {
			return
		}
		tested := r.tested(dir)          // once a directory
		changed := map[string][]string{} // "file\x00package" to the functions whose references changed
		for _, k := range keys(b.refs, a.refs) {
			f := strings.Split(k, "\x00") // file, package, function, name
			if r.ctx.Err() != nil {
				return
			} else if a.refs[k] > b.refs[k] && libc(f[1]) && tested {
				r.outBlock(f[0]+": "+f[1]+"."+f[3]+" in "+f[2], "unverifiable: a new exit, exec, system call or call into testing's runner in a package with tests")
			} else if a.refs[k] != b.refs[k] {
				changed[f[0]+"\x00"+f[1]] = append(changed[f[0]+"\x00"+f[1]], f[2])
			}
		}
		for _, k := range slices.Sorted(maps.Keys(changed)) {
			file, pkg, _ := strings.Cut(k, "\x00")
			fns := slices.Compact(slices.Sorted(slices.Values(changed[k])))
			r.outWarn(file, fmt.Sprintf("references into %s added, removed, moved or changed (%d: %s%s): review it", pkg, len(fns),
				strings.Join(fns[:min(3, len(fns))], ", "), map[bool]string{true: ", …"}[len(fns) > 3]))
		}
		for _, k := range slices.Sorted(maps.Keys(a.unknown)) { // fail closed: its references cannot be read
			if file, pkg, _ := strings.Cut(k, "\x00"); r.files[file] != r.cand[file] && libc(pkg) {
				r.outBlock(file, "unverifiable: an import of "+pkg+" with an unknown name")
			} else if r.files[file] != r.cand[file] {
				r.outWarn(file, "an import of "+pkg+" with an unknown name: review it")
			}
		}
		for _, k := range slices.Sorted(maps.Keys(a.dots)) {
			switch file, pkg, _ := strings.Cut(k, "\x00"); {
			case b.dots[k] && r.files[file] != r.cand[file]: // its uses are unqualified: any change may add one
				r.outWarn(file, "references through a dot import of "+pkg+" may have changed: review it")
			case !b.dots[k] && tested:
				r.outBlock(file, "unverifiable: a dot import of "+pkg)
			case !b.dots[k]:
				r.outWarn(file, "a dot import of "+pkg+": review it")
			}
		}
		own := func(p string) bool { // as the loader resolves it, in both versions: own, not vendored nor a nested module's
			return !slices.ContainsFunc(r.trees[:], func(t *tree) bool {
				ty := t.typer(r, dir, platforms[0])
				_, ok := ty.local(p)
				return !ok || ty.vendor[p]
			})
		}
		for _, p := range slices.Sorted(maps.Keys(a.imports)) { // a new edge outside the standard library and the module
			if !b.imports[p] && p != "C" && p != "unsafe" && !r.std(p) && !own(p) {
				r.outWarn(dir, "a new import of "+p+", outside the standard library and the module: review it")
			}
		}
		if r.ctx.Err() != nil {
			return
		}
	}
}

// outModule: the root go.mod's module line changed: which imports are own
// changes with it.
func (r *run) outModule() {
	if r.files["go.mod"] == r.cand["go.mod"] {
		return
	}
	var mods [2]string
	for i, blob := range []string{r.files["go.mod"], r.cand["go.mod"]} {
		if b, why, err := r.file(blob); blob != "" && why == "" && err == nil {
			mods[i] = modfile.ModulePath(b)
		}
	}
	if mods[0] != mods[1] {
		r.outBlock("go.mod", "unverifiable: the module path changed")
	}
}
