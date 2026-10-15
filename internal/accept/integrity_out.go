// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// P14b-out-1: operator decision A outside initialization and the TestMain
// hook, as an additive layer (#226 entries 75 and 76; #276, split under
// 04 §2). It runs once the base guard has analysed every package, reads the
// touched packages again on its own, with its own work bound, and never
// writes the base guard's maps, keys or counters. Its findings are Additive:
// the report gives them only what the base guard's leave.

// additive is the layer's state: its findings, work and files read.
type additive struct {
	blocks, warnings []Block
	work             int
	over             bool
	files            map[string]*outFile // by blob and base name: each read and parsed once
}

// outFile is what one Go file holds for the layer.
type outFile struct {
	refs    map[string]int // "package\x00function\x00name": references into outS
	dots    []string       // packages in outS it dot-imports
	imports []string
}

// outHook runs the layer (a test turns it off to compare the base guard's
// findings with and without it).
var outHook = true

// maxOutWork bounds the layer's own work: AST nodes and tag evaluations (the
// bytes are the base guard's read bound's).
var maxOutWork = maxWork

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

// charge counts the layer's work: past its bound, one Additive block and no
// more of its work; a done context stops it with no block.
func (r *run) charge(n int) bool {
	if r.out.work += n; !r.out.over && r.ctx.Err() == nil && r.out.work > maxOutWork {
		r.outBlock("", "unverifiable: decision A's outside-initialization check is over its work bound")
	}
	r.out.over = r.out.over || r.out.work > maxOutWork || r.ctx.Err() != nil
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

// outBuilt: a gate platform may build p, as the base guard's holds reads
// it: its name by go/build, and a constraint true for some assignment of
// the tags it does not model (up to 12; more: built). ignore is false.
func (r *run) outBuilt(p string, src []byte) bool {
	var exprs []constraint.Expr
	for _, l := range header(src) {
		if x, err := constraint.Parse(l); err == nil {
			exprs = append(exprs, x)
		}
	}
	return slices.ContainsFunc(platforms, func(pl [2]string) bool {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release, // its name: go/build's rules
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("package p\n")), nil }}
		if ok, err := ctx.MatchFile(path.Dir(p), path.Base(p)); !ok || err != nil {
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
				return false
			})
		}
		if len(free) > 12 {
			return true
		}
		for m := 0; m < 1<<len(free) && r.charge(len(exprs)); m++ {
			if !slices.ContainsFunc(exprs, func(x constraint.Expr) bool {
				return !x.Eval(func(tag string) bool {
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

// outFileOf reads and parses p (blob) once; nil if not read, not parsed or
// no gate platform builds it.
func (r *run) outFileOf(p, blob string) *outFile {
	key := blob + "\x00" + path.Base(p)
	if f, ok := r.out.files[key]; ok {
		return f
	}
	r.out.files[key] = nil
	b, why, err := r.file(blob) // the base guard read it: cached
	if why != "" || err != nil || !r.outBuilt(p, b) {
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, b, 0) // objects resolved: a local shadowing an import is no reference
	if err != nil {
		return nil
	}
	of := &outFile{refs: map[string]int{}}
	names, testing := map[string]string{}, ""
	for _, is := range f.Imports {
		ip, _ := strconv.Unquote(is.Path.Value)
		name := path.Base(ip)
		if is.Name != nil {
			name = is.Name.Name
		}
		switch {
		case name == "." && outS(ip):
			of.dots = append(of.dots, ip)
		case outS(ip):
			names[name] = ip
		case ip == "testing":
			testing = name
		}
		of.imports = append(of.imports, ip)
	}
	for _, d := range f.Decls {
		fn, _ := d.(*ast.FuncDecl)
		where, lits := "package level", false
		switch {
		case fn != nil && fn.Recv == nil && (fn.Name.Name == "init" || strings.HasSuffix(p, "_test.go") && fn.Name.Name == "TestMain" && hook(fn, testing)):
			where, lits = "init literal", true // initialization and the hook are the base guard's; the literals in them run later
		case fn != nil && fn.Recv != nil && r.charge(len(fn.Name.Name)+len(recv(fn))):
			where = bound(recv(fn) + "." + fn.Name.Name)
		case fn != nil && r.charge(len(fn.Name.Name)):
			where = bound(fn.Name.Name)
		}
		var walk func(n ast.Node, in bool)
		walk = func(n ast.Node, in bool) {
			ast.Inspect(n, func(n ast.Node) bool {
				if !r.charge(1) {
					return false
				} else if lit, ok := n.(*ast.FuncLit); ok && lits && !in {
					walk(lit.Body, true)
					return false
				} else if sel, ok := n.(*ast.SelectorExpr); ok && (in || !lits) {
					if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil && names[id.Name] != "" && r.charge(len(sel.Sel.Name)/64) {
						of.refs[names[id.Name]+"\x00"+where+"\x00"+bound(sel.Sel.Name)]++
					}
				}
				return true
			})
		}
		walk(d, false)
	}
	r.out.files[key] = of
	return of
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
	imports map[string]bool
}

func (r *run) outRead(dir string, t *tree) outFacts {
	fa := outFacts{refs: map[string]int{}, dots: map[string]bool{}, imports: map[string]bool{}}
	for _, p := range t.dirs[dir] {
		if path.Ext(p) != ".go" || r.out.over {
			continue // never a non-Go blob
		}
		f := r.outFileOf(p, t.files[p])
		if f == nil {
			continue
		}
		for k, n := range f.refs {
			fa.refs[p+"\x00"+k] += n
		}
		for _, d := range f.dots {
			fa.dots[p+"\x00"+d] = true
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
	r.out.files = map[string]*outFile{}
	r.outPkgs()
	r.outModule()
	return r.ctx.Err()
}

func (r *run) outPkgs() {
	for _, dir := range slices.Sorted(maps.Keys(r.touched)) {
		b, a := r.outRead(dir, r.trees[0]), r.outRead(dir, r.trees[1])
		if r.out.over {
			return
		}
		changed := map[string][]string{} // "file\x00package" to the functions whose references changed
		for _, k := range keys(b.refs, a.refs) {
			f := strings.Split(k, "\x00") // file, package, function, name
			if a.refs[k] > b.refs[k] && libc(f[1]) && r.tested(dir) {
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
		for _, k := range slices.Sorted(maps.Keys(a.dots)) {
			switch file, pkg, _ := strings.Cut(k, "\x00"); {
			case b.dots[k] && r.files[file] != r.cand[file]: // its uses are unqualified: any change may add one
				r.outWarn(file, "references through a dot import of "+pkg+" may have changed: review it")
			case !b.dots[k] && r.tested(dir):
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
		std := func(p string) bool { // go list's, on any gate platform
			return slices.ContainsFunc(platforms, func(pl [2]string) bool { return r.trees[1].typer(r, dir, pl).listed[p].Standard })
		}
		for _, p := range slices.Sorted(maps.Keys(a.imports)) { // a new edge outside the standard library and the module
			if !b.imports[p] && p != "C" && p != "unsafe" && !std(p) && !own(p) {
				r.outWarn(dir, "a new import of "+p+", outside the standard library and the module: review it")
			}
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
