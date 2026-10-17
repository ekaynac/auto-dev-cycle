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
	"go/types"
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
// touched packages again on its own, with its own work bounds, and never
// writes the base guard's maps, keys, counters or listing. Its findings are
// Additive: the report gives them only what the base guard's leave.

// additive is the layer's state.
type additive struct {
	blocks, warnings []Block
	nodes, tags      int // its work: AST nodes and items handled; tag visits
	over             bool
	files            map[string]*outFile        // by blob, name, directory and version
	built            map[string]int             // by blob and name: its gate platforms (outBuilt)
	named            map[string]map[string]bool // by module root and version
	fset             *token.FileSet             // every file it parses
	importers        map[*typer]*outImporter    // P14b-out-3: one a typer of main's, read only
	typedOff         bool                       // P14b-out-3: its types check stopped (typesOff)
	tnodes           int                        // P14b-out-3: its types check's own work (chargeTypes)
}

// outFile is what one Go file holds for the layer.
type outFile struct {
	refs    map[string]int // "package\x00function\x00name": references into outS
	dots    []string       // packages in outS it dot-imports
	unknown []string       // packages in outS imported under a name the layer cannot learn
	imports []string
	f       *ast.File
	units   []unit
	prints  map[string]string          // by binding: its spans' fingerprint, combined in source order
	uses    map[string]map[string]bool // by binding: the S packages it uses
	on      int                        // its gate platforms (outBuilt)
	typed   bool                       // its package's checks by types are done
	trefs   map[string]int             // its references by types alone, as refs (outTypedRefs)
	tuses   map[string]bool            // "binding\x00package": its S uses by types alone
}

// outHook runs the layer (a test turns it off to compare the base guard's
// findings with and without it).
var outHook = true

// The layer's own work bounds: AST nodes, items and name bytes, and
// constraint tag visits. Reaching one stops the layer with one Additive
// warning, never a block by itself (outStopped; the lead's decision on
// Astra #276 r5). They are read when charged, so a test that scales maxRead
// or maxWork scales them; a test sets one to reach it.
var maxOutNodes, maxOutTags int // 0: 16 × maxRead; maxWork

// outS is S beyond the base guard's sPkg: flag, io/ioutil, net/http/cgi
// and the os/, log/, runtime/, testing/ and modernc.org/libc trees.
func outS(p string) bool {
	return !sPkg(p) && (slices.Contains([]string{"flag", "io/ioutil", "net/http/cgi"}, p) ||
		slices.ContainsFunc([]string{"os", "log", "runtime", "testing", "modernc.org/libc"}, func(t string) bool { return p == t || strings.HasPrefix(p, t+"/") }))
}

// libc: C's exit, exec and system calls in Go: class 2, every name.
func libc(p string) bool { return p == "modernc.org/libc" || strings.HasPrefix(p, "modernc.org/libc/") }

func (r *run) outBlock(p, what string) {
	r.out.blocks = append(r.out.blocks, Block{vocab.ReasonTestIntegrityViolation, p, what, true, false})
}

func (r *run) outWarn(p, what string) {
	r.out.warnings = append(r.out.warnings, Block{vocab.ReasonTestIntegrityViolation, p, what, true, false})
}

// charge counts the layer's work: past a bound, or on a done context, no
// more of it (outStopped says so).
func (r *run) charge(nodes, tags int) bool {
	r.out.nodes, r.out.tags = r.out.nodes+nodes, r.out.tags+tags
	past := r.out.nodes > cmp.Or(maxOutNodes, int(16*maxRead)) || r.out.tags > cmp.Or(maxOutTags, maxWork)
	r.out.over = r.out.over || past || r.ctx.Err() != nil
	return !r.out.over
}

// bound is a name cut once, where it is declared: 64 bytes and a hash.
func bound(name string) string {
	if len(name) <= 64 {
		return name
	}
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s…%x", name[:64], h[:boundHash])
}

// boundHash is how many bytes of its hash a bound name keeps (a test forces
// a collision with 0; fingerprints combine, so one never hides another).
var boundHash = 16

// std: p is the standard library's by module metadata alone (Astra #276
// r6; the lead's rule): its first element has no dot, and no module path
// that dir's go.mod or vendor/modules.txt names, in either version, is p or
// a prefix of it. The go command resolves such a path from its GOROOT
// only: it is std or it does not build. Otherwise a new edge warns.
func (r *run) std(dir, p string) bool {
	if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
		return false
	}
	for _, t := range r.trees {
		named := r.named(t, t.typer(r, dir, platforms[0]).root)
		for q := p; ; q = path.Dir(q) {
			if named[q] || named[""] {
				return false
			} else if !strings.Contains(q, "/") {
				break
			}
		}
	}
	return true
}

// named: the module paths root's go.mod (module, require, replace) and
// vendor/modules.txt name in t, once; an unreadable one names every path ("").
func (r *run) named(t *tree, root string) map[string]bool {
	key := root + "\x00" + fmt.Sprintf("%p", t)
	if m, ok := r.out.named[key]; ok {
		return m
	}
	m := map[string]bool{}
	for _, name := range []string{"go.mod", "vendor/modules.txt"} {
		blob := t.files[path.Join(root, name)]
		if blob == "" {
			continue
		}
		b, why, err := r.file(blob)
		f, perr := modfile.Parse(name, b, nil) // strict: ParseLax drops replace
		switch {
		case why != "" || err != nil || name == "go.mod" && perr != nil:
			m[""] = true
		case name == "go.mod":
			if f.Module != nil {
				m[f.Module.Mod.Path] = true
			}
			for _, q := range f.Require {
				m[q.Mod.Path] = true
			}
			for _, q := range f.Replace {
				m[q.Old.Path] = true
			}
		default:
			for _, l := range strings.Split(string(b), "\n") {
				if fs := strings.Fields(l); len(fs) > 1 && fs[0] == "#" {
					m[fs[1]] = true
				} else if len(fs) == 1 {
					m[fs[0]] = true
				}
			}
		}
	}
	r.out.named[key] = m
	return m
}

// outBuilt: the gate platforms that may build p (bit i: platforms[i]), as the base guard's goOn and holds
// read it: its name by go/build; its constraint lines deduplicated, true for
// some assignment of the tags the guard does not model (up to 12; more:
// built); ignore false.
func (r *run) outBuilt(p string, src []byte) (on int) {
	var exprs []constraint.Expr
	seen := map[string]bool{}
	for _, l := range header(src) {
		if x, err := constraint.Parse(l); err == nil && !seen[l] {
			seen[l], exprs = true, append(exprs, x)
		}
	}
	builds := func(pl [2]string, charge func(), stop func() bool) bool {
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
				charge()
				return false
			})
		}
		if len(free) > 12 {
			return true
		}
		for m := 0; m < 1<<len(free) && !stop(); m++ {
			if !slices.ContainsFunc(exprs, func(x constraint.Expr) bool {
				return !x.Eval(func(tag string) bool {
					charge()
					v, k := known(tag)
					return k && v || !k && m>>slices.Index(free, tag)&1 == 1
				})
			}) {
				return true
			}
		}
		return false
	}
	for i, pl := range platforms { // the platforms after the first that builds it: the types check's work (Astra #282 H1)
		charge, stop := func() { r.charge(0, 1) }, func() bool { return r.out.over }
		if on != 0 && !outTypesHook { // the types check off: main's layer
			break
		} else if on != 0 {
			charge, stop = func() { r.chargeTypes(1) }, func() bool { return !r.chargeTypes(0) }
		}
		if builds(pl, charge, stop) {
			on |= 1 << i
		}
	}
	return on
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
		r.out.built[bk] = 0
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
	if built == 0 {
		return nil
	}
	if r.out.fset == nil {
		r.out.fset = token.NewFileSet()
	}
	f, err := parser.ParseFile(r.out.fset, p, b, parser.ParseComments|parser.SkipObjectResolution) // the base guard's mode
	if err != nil {
		delete(r.out.built, bk)
		return fail("parse")
	}
	of := &outFile{refs: map[string]int{}, f: f, prints: map[string]string{}, uses: map[string]map[string]bool{}, on: built, trefs: map[string]int{}, tuses: map[string]bool{}}
	names, testing := map[string]string{}, ""
	for _, is := range f.Imports {
		ip, _ := strconv.Unquote(is.Path.Value)
		name := ""
		switch {
		case is.Name != nil:
			name = is.Name.Name
		case r.std(dir, ip):
			name = path.Base(ip) // a standard package is named by its last element
		default:
			name = r.loadedName(t, dir, ip)
		}
		if ip == "testing" {
			testing = cmp.Or(name, "testing")
		}
		switch {
		case name == "." && outS(ip):
			of.dots = append(of.dots, ip)
		case name == "" && outS(ip):
			of.unknown = append(of.unknown, ip)
		case (outS(ip) || sPkg(ip)) && name != "" && name != "." && name != "_": // the base guard's S too: what a body uses
			names[name] = ip
		}
		of.imports = append(of.imports, ip)
	}
	for _, d := range f.Decls {
		for _, u := range r.units(d, p, testing) {
			of.units = append(of.units, u)
			r.refsIn(u.n, names, u.lits, func(pkg, name string) {
				if r.charge(len(name)+len(u.bind), 0) && outS(pkg) { // the bytes it keys (Astra #276 r3 #6, #280 H4)
					of.refs[pkg+"\x00"+u.where+"\x00"+bound(name)]++
				}
				if outS(pkg) || sensitive(pkg+"."+name) > 0 {
					of.use(u.bind, pkg)
				}
			})
			r.print(of, b, u)
		}
	}
	r.out.files[key] = of
	return of
}

// loadedName is the declared name of ip as the base guard's loader read it
// for t and dir: its own and vendored packages, and the imports of dir's
// analysed package (export data); "" if it did not.
func (r *run) loadedName(t *tree, dir, ip string) string {
	for _, pl := range platforms {
		ty := t.typer(r, dir, pl)
		if p := ty.pkgs[ip]; p != nil {
			return p.Name()
		} else if self := ty.self[ty.pathOf(dir)]; self != nil {
			if i := slices.IndexFunc(self.Imports(), func(q *types.Package) bool { return q.Path() == ip }); i >= 0 {
				return self.Imports()[i].Name()
			}
		}
	}
	return ""
}

// refsIn calls add for each reference in n to a package names maps it to
// (only inside function literals with lits), a local that shadows the
// import name, in a scope open there, being no reference. Scopes are
// tracked lexically, as the language declares them: a := , var or const
// name from the end of its statement, parameters and range variables in
// the body. Only these shadow: any other name (a type parameter, a type
// switch's variable, a local type) is taken as not shadowing, so a doubt
// is a false warning, never a silent reference (decision A).
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
			case *ast.AssignStmt: // a type switch's v is declared in each clause, after its types: left out (not shadowed)
				if ts, ok := stack[len(stack)-1].(*ast.TypeSwitchStmt); top.Tok == token.DEFINE && (!ok || ts.Assign != top) {
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
	prints  map[string]string          // "file\x00binding"
	trefs   map[string]int             // P14b-out-3: the references by types alone, apart from refs (Astra #282 r3 H1)
	tuses   map[string]bool            // P14b-out-3: "file\x00binding\x00package", the S uses by types alone
	uses    map[string]map[string]bool // "file\x00binding": packages
}

func (r *run) outRead(dir string, t *tree) outFacts {
	fa := outFacts{refs: map[string]int{}, dots: map[string]bool{}, unknown: map[string]bool{}, imports: map[string]bool{},
		prints: map[string]string{}, uses: map[string]map[string]bool{}, trefs: map[string]int{}, tuses: map[string]bool{}}
	var ofs []*outFile
	var ps []string
	for _, p := range t.dirs[dir] {
		if path.Ext(p) != ".go" || r.out.over {
			continue // never a non-Go blob
		}
		if f := r.outFileOf(p, dir, t.files[p], t); f != nil {
			ofs, ps = append(ofs, f), append(ps, p)
		}
	}
	if outTypesHook {
		r.outTyped(dir, t, ofs)
	}
	for i, f := range ofs {
		p := ps[i]
		for k, h := range f.prints {
			fa.prints[p+"\x00"+k], fa.uses[p+"\x00"+k] = h, f.uses[k]
		}
		for k, n := range f.refs {
			fa.refs[p+"\x00"+k] += n
		}
		for k, n := range f.trefs {
			fa.trefs[p+"\x00"+k] += n
		}
		for k := range f.tuses {
			fa.tuses[p+"\x00"+k] = true
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
	r.out.files, r.out.built, r.out.named, r.out.importers = map[string]*outFile{}, map[string]int{}, map[string]map[string]bool{}, map[*typer]*outImporter{}
	r.outPkgs()
	r.outModule()
	if outDone != nil {
		outDone(r.out.nodes, r.out.tags, r.out.over)
	}
	return r.ctx.Err()
}

func (r *run) outPkgs() {
	dirs := slices.Sorted(maps.Keys(r.touched))
	for i, dir := range dirs {
		b, a := r.outRead(dir, r.trees[0]), r.outRead(dir, r.trees[1])
		if r.out.over || !r.charge(len(b.refs)+len(a.refs)+len(a.dots)+len(a.unknown)+len(a.imports)+len(b.prints)+len(a.prints), 0) {
			r.outStopped(dirs[i:])
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
		r.outBodies(b, a, changed)
		r.outTypedSignals(dir, b, a, changed)
		for _, k := range slices.Sorted(maps.Keys(a.unknown)) { // fail closed: its references cannot be read
			if file, pkg, _ := strings.Cut(k, "\x00"); r.files[file] != r.cand[file] && libc(pkg) && tested {
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
			if !b.imports[p] && p != "C" && p != "unsafe" && !r.std(dir, p) && !own(p) {
				r.outWarn(dir, "a new import of "+p+", outside the standard library and the module: review it")
			}
		}
		if r.ctx.Err() != nil {
			return
		}
	}
}

// outStopped: the layer stopped at its own work bound (the lead's decision
// on Astra #276 r5): one Additive warning naming the directories it did
// not finish, never a block by itself. Class 2 stays closed: a libc
// reference needs a libc import in its file, so a changed Go file of an
// unfinished tested package that imports modernc.org/libc blocks. The
// import scan reads each blob once (the base guard read it), its imports
// only, charged by its bytes to maxRead; a scan that cannot finish blocks.
func (r *run) outStopped(dirs []string) {
	if r.ctx.Err() != nil {
		return
	}
	shown := strings.Join(dirs[:min(3, len(dirs))], ", ") + map[bool]string{true: ", …"}[len(dirs) > 3]
	r.outWarn("", fmt.Sprintf("decision A's outside-initialization check stopped at its work bound; its signals for %d directories (%s) are incomplete", len(dirs), bound(shown)))
	scanned, bytes := map[string]bool{}, int64(0)
	for _, dir := range slices.DeleteFunc(slices.Clone(dirs), func(d string) bool { return !r.tested(d) }) {
		for _, p := range r.trees[1].dirs[dir] {
			blob := r.trees[1].files[p]
			if path.Ext(p) != ".go" || r.files[p] == blob || scanned[blob] {
				continue
			}
			b, why, err := r.file(blob)
			scanned[blob], bytes = true, bytes+int64(len(b))
			f, perr := parser.ParseFile(token.NewFileSet(), p, b, parser.ImportsOnly|parser.SkipObjectResolution)
			if why != "" || err != nil || perr != nil || bytes > maxRead || r.ctx.Err() != nil {
				r.outBlock(p, "unverifiable: decision A's outside-initialization check stopped at its work bound and could not read this file's imports")
				return
			}
			for _, is := range f.Imports {
				if ip, _ := strconv.Unquote(is.Path.Value); libc(ip) {
					r.outBlock(p, "unverifiable: decision A's outside-initialization check stopped at its work bound, and this file imports "+ip+" in a package with tests")
				}
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
