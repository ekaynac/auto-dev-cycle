// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// P14b-out-2 (split from #276 under 04 §2): in the additive layer, with its
// own work bounds, warnings only. A changed body, variable initializer or
// deferred literal that uses S warns once per file and package, though its
// references stay the same; by real types, the layer also reads references
// into S through a value (fs.Parse on a *flag.FlagSet) and S type arguments
// of generic instantiations (parse[T] with T flag.FlagSet).

// unit is one part of a declaration the layer reads: its references are
// counted under where, its source spans fingerprinted under bind ("": none).
type unit struct {
	n           ast.Node
	where, bind string
	lits        bool // only its function literals: they run later
	typedOnly   bool // its S uses by types only (its text is the base guard's); no reference counted
}

// units are d's parts: a function, bound by its bounded name; each var spec,
// bound by all its names (blank ones too: Astra #276 r3 #5), its references
// counted under that binding (Opus #280 M3); initialization and the real
// TestMain(*testing.M) hook of a _test.go file: their function literals
// (r3 #4), bound as one, and their whole body, whose S uses by types alone
// count (Opus #280 M4); another declaration, unbound. Names are bounded
// once here, their original bytes charged (r3 #6).
func (r *run) units(d ast.Decl, p, testing string) []unit {
	switch d := d.(type) {
	case *ast.FuncDecl:
		switch {
		case d.Recv == nil && (d.Name.Name == "init" || strings.HasSuffix(p, "_test.go") && d.Name.Name == "TestMain" && hook(d, testing)):
			return []unit{{d, "init literal", "init literal", true, false}, {d, "", "func " + d.Name.Name, false, true}}
		case d.Recv != nil && r.charge(len(d.Name.Name)+len(recv(d)), 0):
			w := bound(recv(d) + "." + d.Name.Name)
			return []unit{{d, w, "func " + w, false, false}}
		case d.Recv == nil && r.charge(len(d.Name.Name), 0):
			return []unit{{d, bound(d.Name.Name), "func " + bound(d.Name.Name), false, false}}
		}
	case *ast.GenDecl:
		if d.Tok == token.VAR {
			var us []unit
			for _, sp := range d.Specs {
				var ns []string
				for _, id := range sp.(*ast.ValueSpec).Names {
					if r.charge(len(id.Name), 0) {
						ns = append(ns, bound(id.Name))
					}
				}
				b := "var " + strings.Join(ns, ", ")
				us = append(us, unit{sp, b, b, false, false})
			}
			return us
		}
	}
	return []unit{{d, "package level", "", false, false}}
}

// print combines u's spans (its function literals only, with lits) into its
// binding's fingerprint, sha256(prev||span) in source order: declarations
// sharing a binding (two func _, a bound name's collision) never overwrite.
func (r *run) print(of *outFile, src []byte, u unit) {
	spans := []ast.Node{u.n}
	if u.bind == "" {
		return
	} else if u.lits {
		spans = nil
		ast.Inspect(u.n, func(n ast.Node) bool {
			if l, ok := n.(*ast.FuncLit); ok {
				spans = append(spans, l)
			}
			_, lit := n.(*ast.FuncLit)
			return n == nil || !lit && r.charge(1, 0)
		})
	}
	tf := r.out.fset.File(of.f.Pos())
	for _, n := range spans {
		s := src[tf.Offset(n.Pos()):tf.Offset(n.End())]
		if !r.charge(len(s), 0) {
			return
		}
		h := sha256.Sum256(append([]byte(of.prints[u.bind]), s...))
		of.prints[u.bind] = string(h[:])
	}
}

func (of *outFile) use(bind, pkg string) {
	if bind != "" {
		if of.uses[bind] == nil {
			of.uses[bind] = map[string]bool{}
		}
		of.uses[bind][pkg] = true
	}
}

// sObj: o, named name, is in S: the layer's, or the base guard's as its
// sensitive classifies it (Opus #280 L1); testing's, reflect's and unsafe's
// values have the base guard's own rules.
func sObj(o types.Object, name string) bool {
	if o == nil || o.Pkg() == nil || slices.Contains([]string{"testing", "reflect", "unsafe"}, o.Pkg().Path()) {
		return false
	}
	return outS(o.Pkg().Path()) || sPkg(o.Pkg().Path()) && sensitive(o.Pkg().Path()+"."+name) > 0
}

// outTyped type-checks each package of dir in t after the base guard, as
// the base guard does (Opus #280 H1): each gate platform's own files, with
// that platform's typer, the package before its external test (which
// imports it with its test files: M2); a file set that two platforms share
// is checked once. It adds what real types show: a method of an S type
// through a value, keyed by its full name ((*flag.FlagSet).Parse), and an S
// type argument (parse[flag.FlagSet]); a key's count is its largest on any
// platform. Its importer loads nothing the base guard did not (outImporter);
// the check runs within the run's deadline and context, apart from the base
// guard's type checks (outCheck). Past a bound, on an import the base guard
// had and the layer cannot serve, or on a panic, the layer stops (its
// warning): never a silent skip. A type error is the base guard's.
func (r *run) outTyped(dir string, t *tree, ofs []*outFile) {
	var todo []*outFile
	for _, of := range ofs {
		if !of.typed {
			of.typed, todo = true, append(todo, of)
		}
	}
	done := map[string]bool{}
	for i, pl := range platforms {
		groups, set := map[string][]*outFile{}, ""
		for j, of := range todo {
			if of.on>>i&1 == 1 {
				groups[of.f.Name.Name], set = append(groups[of.f.Name.Name], of), set+fmt.Sprint(j, ",")
			}
		}
		if len(groups) == 0 || done[set] || r.out.over {
			continue
		}
		done[set] = true
		ty := t.typer(r, dir, pl)
		im := r.outImporter(ty)
		for _, name := range slices.SortedFunc(maps.Keys(groups), func(a, b string) int { // the package first, then its external test
			return strings.Compare(fmt.Sprint(strings.HasSuffix(a, "_test"), a), fmt.Sprint(strings.HasSuffix(b, "_test"), b))
		}) {
			var files []*ast.File
			for _, of := range groups[name] {
				if files = append(files, of.f); !r.charge(int(of.f.FileEnd-of.f.FileStart), 0) {
					return
				}
			}
			info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Instances: map[*ast.Ident]types.Instance{}}
			pkg, ok := r.outCheck(im, ty.pathOf(dir), name, files, info)
			if !ok || im.failed != "" {
				r.out.over = true
				return
			} else if !strings.HasSuffix(name, "_test") {
				im.self = pkg
			}
			r.outTypedRefs(groups[name], info)
		}
		im.self = nil
	}
}

// outTypedRefs adds the references by types of one check to its files, a
// key's largest count on any platform.
func (r *run) outTypedRefs(ofs []*outFile, info *types.Info) {
	names := map[types.Object]string{}
	for _, of := range ofs {
		counts := map[string]int{}
		for _, u := range of.units {
			r.typedIn(u, info, names, func(pkg, ref string) {
				if r.charge(len(u.where), 0) && u.where != "" {
					counts[pkg+"\x00"+u.where+"\x00"+ref]++
				}
				of.use(u.bind, pkg)
			})
		}
		for k, n := range counts {
			of.refs[k] = max(of.refs[k], n)
		}
	}
}

// outRunning is the layer's checks given up on that still run (any run's):
// go/types cannot be stopped; while one runs, the layer type-checks nothing
// more (it stops, with its warning). The base guard's own accounting
// (abandoned) is never touched.
var outRunning atomic.Int64

// maxOutCheck bounds the time of one of the layer's checks (0: the run's
// deadline alone; a test sets it).
var maxOutCheck time.Duration

// outCheck type-checks files as package name of path, apart from the base
// guard: in a goroutine it drops at the run's deadline (or maxOutCheck) or
// when the run's context ends; what a dropped check holds is the layer's
// own (its importer, these files, info), never the base guard's state.
func (r *run) outCheck(im *outImporter, path, name string, files []*ast.File, info *types.Info) (*types.Package, bool) {
	if outRunning.Load() > 0 {
		return nil, false
	}
	deadline := r.listEnd
	if maxOutCheck > 0 && time.Now().Add(maxOutCheck).Before(deadline) {
		deadline = time.Now().Add(maxOutCheck)
	}
	if strings.HasSuffix(name, "_test") {
		path += "_test"
	}
	conf := types.Config{Importer: im, FakeImportC: true, Error: func(error) {}}
	type result struct {
		pkg *types.Package
		ok  bool
	}
	done, state := make(chan result, 1), new(atomic.Int32) // 0 running, 1 ended, 2 dropped
	go func() {
		res := result{}
		defer func() {
			if recover() != nil {
				res.ok = false
			}
			if !state.CompareAndSwap(0, 1) {
				outRunning.Add(-1)
			}
			done <- res
		}()
		res.pkg, _ = conf.Check(path, r.out.fset, files, info)
		res.ok = true
	}()
	select {
	case res := <-done:
		return res.pkg, res.ok
	case <-r.ctx.Done():
	case <-time.After(time.Until(deadline)):
	}
	if state.CompareAndSwap(0, 2) {
		outRunning.Add(1)
	}
	return nil, false
}

// outImporter is the layer's own importer view of a typer (Opus #280 H2;
// the lead's condition): it loads nothing the base guard did not and writes
// none of the typer's state. An own or vendored package is the one the base
// guard type-checked, or one it parsed, checked again from its syntax by
// this importer (bodies skipped); the package under test, for its external
// test, is the layer's own check of it (self); any other is export data the
// base guard listed, read by its own gc importer within maxOutExport bytes a
// run. Any other import fails: if the base guard had it, its error is the
// base guard's; if not, failed is set and the layer stops.
type outImporter struct {
	ty     *typer
	gc     types.Importer
	own    map[string]*types.Package // checked from the base guard's syntax (nil: being checked)
	self   *types.Package            // the package of dir, for its external test
	failed string                    // the first import the base guard had and the layer could not serve
}

// maxOutExport bounds the export data the layer's importers read in a run:
// one importer a typer reads each file once, as the base guard's typer did
// (maxExport a version), so two versions read at most twice that.
var maxOutExport = 2 * maxExport

func (r *run) outImporter(ty *typer) *outImporter {
	if im := r.out.importers[ty]; im != nil {
		return im
	}
	im := &outImporter{ty: ty, own: map[string]*types.Package{}}
	r.out.importers[ty] = im
	im.gc = importer.ForCompiler(r.out.fset, "gc", func(p string) (io.ReadCloser, error) {
		fi, err := os.Stat(ty.listed[p].Export)
		if ty.listed[p].Export == "" || err != nil {
			return nil, errors.New("not listed by the base guard")
		} else if r.out.export -= fi.Size(); r.out.export < 0 {
			return nil, errors.New("over the layer's export data bound")
		}
		return os.Open(ty.listed[p].Export)
	})
	return im
}

func (im *outImporter) Import(p string) (*types.Package, error) {
	if im.self != nil && p == im.self.Path() {
		return im.self, nil
	} else if pkg := im.ty.pkgs[p]; pkg != nil {
		return pkg, nil
	}
	pkg, err := (*types.Package)(nil), errors.New("not loaded by the base guard")
	if dir, own := im.ty.local(p); !own {
		pkg, err = im.gc.Import(p)
	} else if done, seen := im.own[p]; seen && done != nil {
		pkg, err = done, nil
	} else if files := im.ty.parsed[dir]; !seen && files != nil {
		im.own[p] = nil
		conf := types.Config{Importer: im, FakeImportC: true, IgnoreFuncBodies: true, Error: func(error) {}}
		pkg, _ = conf.Check(p, im.ty.fset, files, nil)
		im.own[p], err = pkg, nil
	}
	if had := im.ty.why != "" || im.ty.failed[p] != "" || im.ty.listed[p].Error != nil; err != nil && !had && im.failed == "" {
		im.failed = p + ": " + err.Error()
	}
	return pkg, err
}

// typedIn calls add for each S method through a value and each S type
// argument in u (inside its function literals only, with lits; outside
// them, with typedOnly).
func (r *run) typedIn(u unit, info *types.Info, names map[types.Object]string, add func(pkg, ref string)) {
	visit := func(n ast.Node) bool {
		if n == nil || !r.charge(1, 0) {
			return n == nil
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if s := info.Selections[x]; s != nil && s.Kind() != types.FieldVal {
				if o, _ := s.Obj().(*types.Func); o != nil && sObj(o, o.Name()) {
					add(o.Pkg().Path(), r.methodName(o.Origin(), names))
				}
			}
		case *ast.Ident:
			if in, ok := info.Instances[x]; ok && in.TypeArgs != nil && r.charge(len(x.Name), 0) {
				for t := range in.TypeArgs.Types() {
					if p, ok := types.Unalias(t).(*types.Pointer); ok {
						t = p.Elem()
					}
					if nt, ok := types.Unalias(t).(*types.Named); ok && sObj(nt.Obj(), nt.Obj().Name()) {
						add(nt.Obj().Pkg().Path(), bound(x.Name)+"["+r.typeName(nt.Obj(), names)+"]")
					}
				}
			}
		}
		return true
	}
	if !u.lits && !u.typedOnly {
		ast.Inspect(u.n, visit)
		return
	}
	ast.Inspect(u.n, func(n ast.Node) bool { // its literals alone (lits), or all but them (typedOnly)
		if l, ok := n.(*ast.FuncLit); ok {
			if u.lits {
				ast.Inspect(l, visit)
			}
			return false
		} else if u.typedOnly {
			return visit(n)
		}
		return n == nil || r.charge(1, 0)
	})
}

// methodName is o's full name from bounded parts, once an object:
// (*flag.FlagSet).Parse.
func (r *run) methodName(o *types.Func, names map[types.Object]string) string {
	if q, ok := names[o]; ok {
		return q
	}
	on := "?"
	if sig, _ := o.Type().(*types.Signature); sig != nil && sig.Recv() != nil {
		t, star := sig.Recv().Type(), ""
		if p, ok := types.Unalias(t).(*types.Pointer); ok {
			t, star = p.Elem(), "*"
		}
		if n, ok := types.Unalias(t).(*types.Named); ok {
			on = star + r.typeName(n.Obj(), names)
		}
	}
	r.charge(len(o.Name()), 0)
	names[o] = "(" + on + ")." + bound(o.Name())
	return names[o]
}

func (r *run) typeName(o types.Object, names map[types.Object]string) string {
	if q, ok := names[o]; ok {
		return q
	}
	r.charge(len(o.Pkg().Path())+len(o.Name()), 0)
	names[o] = bound(o.Pkg().Path()) + "." + bound(o.Name())
	return names[o]
}

// outBodies warns, once per file and package, for the bindings whose
// fingerprint changed, or var specs added or gone, that use S in either
// version, unless the file's references into that package changed (their
// warning says it already).
func (r *run) outBodies(b, a outFacts, changed map[string][]string) {
	bodies := map[string][]string{}
	for _, k := range keys(b.prints, a.prints) {
		file, bind, _ := strings.Cut(k, "\x00")
		bp, inB := b.prints[k]
		if ap, inA := a.prints[k]; inB && inA && bp == ap || (!inB || !inA) && !strings.HasPrefix(bind, "var ") {
			continue // unchanged; a function added or gone moves its references (a var spec's text ones do not: Opus #280 M3)
		}
		for _, pkg := range slices.Sorted(maps.Keys(maps.Collect(func(yield func(string, bool) bool) {
			for _, m := range []map[string]bool{b.uses[k], a.uses[k]} {
				for p := range m {
					yield(p, true)
				}
			}
		}))) {
			if changed[file+"\x00"+pkg] == nil {
				bodies[file+"\x00"+pkg] = append(bodies[file+"\x00"+pkg], bind)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(bodies)) {
		file, pkg, _ := strings.Cut(k, "\x00")
		bs := bodies[k]
		r.outWarn(file, fmt.Sprintf("changed bodies that use %s (%d: %s%s): review it", pkg, len(bs),
			strings.Join(bs[:min(3, len(bs))], ", "), map[bool]string{true: ", …"}[len(bs) > 3]))
	}
}
