// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
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
// candidate and the base again on its own, with its own work budget, and
// never writes the base guard's maps, keys or counters. Its findings are
// Additive: the report gives them only what the base guard's leave.

// additive is the layer's state: its findings and its work budget.
type additive struct {
	blocks, warnings []Block
	work             int
	over             bool
}

// outS is S beyond the base guard's sPkg: flag, io/ioutil, net/http/cgi
// and the os/, log/, runtime/, testing/ and modernc.org/libc trees.
func outS(p string) bool {
	return !sPkg(p) && (slices.Contains([]string{"flag", "io/ioutil", "net/http/cgi"}, p) ||
		slices.ContainsFunc([]string{"os", "log", "runtime", "testing", "modernc.org/libc"}, func(t string) bool { return p == t || strings.HasPrefix(p, t+"/") }))
}

// libc: C's exit, exec and system calls in Go: class 2.
func libc(p string) bool { return p == "modernc.org/libc" || strings.HasPrefix(p, "modernc.org/libc/") }

func (r *run) outBlock(p, what string) {
	r.out.blocks = append(r.out.blocks, Block{vocab.ReasonTestIntegrityViolation, p, what, true})
}

func (r *run) outWarn(p, what string) {
	r.out.warnings = append(r.out.warnings, Block{vocab.ReasonTestIntegrityViolation, p, what, true})
}

// maxOutWork bounds the layer's own work (bytes read and nodes visited).
var maxOutWork = maxWork

// charge counts the layer's work: past its bound, one Additive block and
// no more of its work; the base guard's analysis is done already.
func (r *run) charge(n int) bool {
	if r.out.work += n; !r.out.over && (r.out.work > maxOutWork || r.ctx.Err() != nil) {
		r.out.over = true
		r.outBlock("", "unverifiable: decision A's outside-initialization check is over its work bound")
	}
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

// outFacts is one version of a package directory, as the layer reads it.
type outFacts struct {
	refs    map[string]int  // "file\x00package\x00function\x00name": references into outS
	dots    map[string]bool // "file\x00package": a dot import of an outS package
	imports map[string]bool // what the files a gate platform may build import
}

// outBuilt: a gate platform may build p, a tag the guard does not model
// (custom, race, cgo) taken as set.
func outBuilt(p string, src []byte) bool {
	var tags []string
	for _, l := range header(src) {
		if x, err := constraint.Parse(l); err == nil {
			x.Eval(func(tag string) bool {
				if !slices.Contains(otherPlatforms, tag) && tag != "ignore" && !strings.HasPrefix(tag, "go1.") {
					tags = append(tags, tag)
				}
				return false
			})
		}
	}
	return slices.ContainsFunc(platforms, func(pl [2]string) bool {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: release, BuildTags: tags,
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
		ok, err := ctx.MatchFile(path.Dir(p), path.Base(p))
		return ok && err == nil
	})
}

func (r *run) outRead(dir string, t *tree) outFacts {
	fa := outFacts{refs: map[string]int{}, dots: map[string]bool{}, imports: map[string]bool{}}
	for _, p := range t.dirs[dir] {
		b, why, err := r.file(t.files[p]) // the base guard read it: cached
		if path.Ext(p) != ".go" || why != "" || err != nil || !r.charge(len(b)) {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, b, 0) // objects resolved: a local shadowing an import is no reference
		if err != nil || !outBuilt(p, b) {
			continue
		}
		names, testing := map[string]string{}, ""
		for _, is := range f.Imports {
			ip, _ := strconv.Unquote(is.Path.Value)
			name := path.Base(ip)
			if is.Name != nil {
				name = is.Name.Name
			}
			switch {
			case name == "." && outS(ip):
				fa.dots[p+"\x00"+ip] = true
			case outS(ip):
				names[name] = ip
			case ip == "testing":
				testing = name
			}
			fa.imports[ip] = true
		}
		for _, d := range f.Decls {
			fn, _ := d.(*ast.FuncDecl)
			where := "package level"
			if fn != nil && (fn.Name.Name == "init" && fn.Recv == nil || strings.HasSuffix(p, "_test.go") && fn.Name.Name == "TestMain" && fn.Recv == nil && hook(fn, testing)) {
				continue // initialization and the hook: the base guard's
			} else if fn != nil && r.charge(len(fn.Name.Name)) {
				where = bound(fn.Name.Name)
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if !r.charge(1) {
					return false
				} else if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil && names[id.Name] != "" && r.charge(len(sel.Sel.Name)) {
						fa.refs[p+"\x00"+names[id.Name]+"\x00"+where+"\x00"+bound(sel.Sel.Name)]++
					}
				}
				return true
			})
		}
	}
	return fa
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

// outPkgs runs the layer over every touched package directory, once the
// base guard is done with all of them.
func (r *run) outPkgs() {
	for _, dir := range slices.Sorted(maps.Keys(r.touched)) {
		if r.out.over {
			return
		}
		b, a := r.outRead(dir, r.trees[0]), r.outRead(dir, r.trees[1])
		changed := map[string][]string{} // "file\x00package" to the functions whose references changed
		for _, k := range keys(b.refs, a.refs) {
			f := strings.Split(k, "\x00") // file, package, function, name
			if a.refs[k] > b.refs[k] && libc(f[1]) && r.tested(dir) {
				r.outBlock(f[0]+": "+f[1]+"."+f[3]+" in "+f[2], "unverifiable: a new exit, exec, system call or call into testing's runner in a package with tests")
			} else if a.refs[k] != b.refs[k] && r.charge(len(k)) {
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
			if file, pkg, _ := strings.Cut(k, "\x00"); !b.dots[k] && r.tested(dir) {
				r.outBlock(file, "unverifiable: a dot import of "+pkg)
			} else if !b.dots[k] {
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
		for _, p := range slices.Sorted(maps.Keys(a.imports)) { // a new edge outside the standard library (go list's) and the module
			if !b.imports[p] && p != "C" && p != "unsafe" && !r.trees[1].typer(r, dir, platforms[0]).listed[p].Standard && !own(p) {
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
