// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// P14b-out-3 (split from #280 at its round 3): S by real types, in the
// additive layer, warnings only. The base guard keeps no types.Info, so the
// layer checks each touched package again, with one rule from #280's
// reviews: a shared object is the base guard's alone, read only, or the
// layer's alone. The layer reads a method of an S type through a value
// (fs.Parse on a *flag.FlagSet, keyed by its full name), an S type argument
// (g[T] with T flag.FlagSet), and S uses by types in changed bodies,
// variable initializers (var parse = safe.Parse) and init and hook bodies.

// outView is a narrow, read-only snapshot of one of the base guard's
// typers: all a worker reads of the base guard (never the run, its tree or
// the typer).
type outView struct {
	fset   *token.FileSet         // the base guard's positions of its parsed syntax
	own    map[string][]*ast.File // an own or vendored package it parsed, by import path
	export map[string]string      // the export data file it listed, by import path
}

// outImporter is one coherent package graph for the layer's checks on one
// typer (Astra #280 H2; choice a): every package it returns is its own. An
// own or vendored package is checked from the base guard's syntax (bodies
// skipped) against this importer; any other is read by its own gc importer
// from the export data the base guard listed, within maxOutExport bytes a
// run; the package under test, for its external test, is the layer's own
// check of it with its test files (Opus #280 M2). It loads nothing the base
// guard did not list or parse and writes none of its state; any import it
// cannot serve is failed (the directory is incomplete).
type outImporter struct {
	v      outView
	gc     types.Importer
	pkgs   map[string]*types.Package // checked from the base guard's syntax (nil: being checked, or failed)
	errs   map[string]error          // why one failed
	self   *types.Package            // for an external test (forTest): its package under test
	budget *int64                    // the run's export data bytes left: the layer's own counter
	failed string
}

// maxOutExport bounds the export data the layer's importers read in a run:
// the base guard reads at most maxExport a version.
var maxOutExport = 2 * maxExport

func (r *run) outImporterOf(ty *typer) *outImporter {
	if im := r.out.importers[ty]; im != nil {
		return im
	} else if r.out.importers == nil { // a run made without outRun (a test)
		r.out.importers, r.out.export = map[*typer]*outImporter{}, new(int64)
		*r.out.export = maxOutExport
	}
	im := &outImporter{v: outView{fset: ty.fset, own: map[string][]*ast.File{}, export: map[string]string{}}, pkgs: map[string]*types.Package{}, errs: map[string]error{}, budget: r.out.export}
	for d, fs := range ty.parsed {
		if rel, ok := strings.CutPrefix(d, path.Join(ty.root, "vendor")+"/"); ok && fs != nil {
			im.v.own[rel] = fs
		} else if p := ty.pathOf(d); p != "" && fs != nil {
			im.v.own[p] = fs
		}
	}
	for p, l := range ty.listed {
		if l.Export != "" {
			im.v.export[p] = l.Export
		}
	}
	im.gc = importer.ForCompiler(token.NewFileSet(), "gc", func(p string) (io.ReadCloser, error) {
		fi, err := os.Stat(im.v.export[p])
		if im.v.export[p] == "" || err != nil {
			return nil, errors.New("not listed by the base guard")
		} else if *im.budget -= fi.Size(); *im.budget < 0 {
			return nil, errors.New("over the layer's export data bound")
		}
		return os.Open(im.v.export[p])
	})
	r.out.importers[ty] = im
	return im
}

func (im *outImporter) Import(p string) (*types.Package, error) {
	if im.self != nil && p == im.self.Path() {
		return im.self, nil
	} else if pkg, seen := im.pkgs[p]; seen && pkg != nil {
		return pkg, nil
	} else if seen {
		return nil, im.fail(p, cmp.Or(im.errs[p], errors.New("an import cycle")))
	} else if files := im.v.own[p]; files != nil {
		im.pkgs[p] = nil
		var first error
		conf := types.Config{Importer: im, FakeImportC: true, IgnoreFuncBodies: true, Error: func(err error) { first = cmp.Or(first, err) }}
		pkg, _ := conf.Check(p, im.v.fset, files, nil)
		if first != nil { // failed for every check that imports it
			im.errs[p] = first
			return nil, im.fail(p, first)
		}
		im.pkgs[p] = pkg
		return pkg, nil
	}
	pkg, err := im.gc.Import(p)
	if err != nil {
		return nil, im.fail(p, err)
	}
	return pkg, nil
}

// forTest is im's view for the external test of self, the layer's own check
// of its package with its test files (Opus #280 M2), coherent as the base
// guard's: a package that reaches self's path is checked again against
// self; any other is im's own.
func (im *outImporter) forTest(self *types.Package) *outImporter {
	c := &outImporter{v: im.v, gc: im.gc, pkgs: map[string]*types.Package{}, errs: im.errs, self: self, budget: im.budget}
	memo := map[*types.Package]bool{}
	var reaches func(p *types.Package) bool
	reaches = func(p *types.Package) bool {
		if v, ok := memo[p]; ok {
			return v
		}
		memo[p] = false
		memo[p] = p.Path() == self.Path() || slices.ContainsFunc(p.Imports(), reaches)
		return memo[p]
	}
	for path, p := range im.pkgs {
		if p != nil && !reaches(p) {
			c.pkgs[path] = p
		}
	}
	return c
}

func (im *outImporter) fail(p string, err error) error {
	if im.failed == "" {
		im.failed = p + ": " + err.Error()
	}
	return err
}

// outWorkers counts the layer's checks that run, any run's (Astra #280 M5):
// a slot is taken before a check starts and freed only when its worker
// ends; go/types cannot be stopped, so a dropped worker keeps its slot.
var outWorkers atomic.Int32

const maxOutWorkers = 1

// maxOutCheck bounds one of the layer's checks (0: the run's deadline
// alone; a test sets it).
var maxOutCheck time.Duration

// outCheck type-checks files as path in a worker that holds only them, the
// importer (its snapshot and budget) and info, never the run: it waits until
// the run's deadline (or maxOutCheck) or until ctx ends, then drops it. No
// slot free by the deadline, a drop or a panic: false (the layer stops,
// with its warning).
func outCheck(ctx context.Context, deadline time.Time, im *outImporter, path string, fset *token.FileSet, files []*ast.File, info *types.Info) (*types.Package, []error, bool) {
	if maxOutCheck > 0 && time.Now().Add(maxOutCheck).Before(deadline) {
		deadline = time.Now().Add(maxOutCheck)
	}
	for outWorkers.Add(1) > maxOutWorkers { // a slot, waited for until the deadline
		outWorkers.Add(-1)
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-time.After(min(10*time.Millisecond, time.Until(deadline))):
			if !time.Now().Before(deadline) {
				return nil, nil, false
			}
		}
	}
	type result struct {
		pkg  *types.Package
		errs []error
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		defer outWorkers.Add(-1) // when it ends
		var res result
		defer func() {
			if recover() != nil {
				res.ok = false
			}
			done <- res
		}()
		conf := types.Config{Importer: im, FakeImportC: true, Error: func(err error) { res.errs = append(res.errs, err) }}
		res.pkg, _ = conf.Check(path, fset, files, info)
		res.ok = true
	}()
	select {
	case res := <-done:
		return res.pkg, res.errs, res.ok
	case <-ctx.Done():
	case <-time.After(time.Until(deadline)):
	}
	return nil, nil, false
}

// outTyperOf is the base guard's typer of dir's module on pl, nil if it
// made none: the layer makes none (Opus #280 H2).
func outTyperOf(t *tree, dir string, pl [2]string) *typer {
	root := ""
	for d := dir; root == ""; d = path.Dir(d) {
		if t.files[path.Join(d, "go.mod")] != "" {
			root = d
		} else if d == "." {
			break
		}
	}
	return t.typers[root+"\x00"+pl[0]+"/"+pl[1]]
}

// outTyped checks each package of dir in t by types: each gate platform's
// own files with the base guard's typer of that platform (Astra #280 H3,
// Opus #280 H1: no union, no file set shared), the package before its
// external test; a key's count is its largest on any platform. A package
// none of whose files imports anything reaches no S type. An import it
// cannot serve, or a type error the base guard tolerates not (its
// redeclarations under tags it does not model), makes the directory
// incomplete: one warning, and that check adds nothing.
func (r *run) outTyped(dir string, t *tree, ofs []*outFile) {
	var todo []*outFile
	for _, of := range ofs {
		if !of.typed {
			of.typed, todo = true, append(todo, of)
		}
	}
	if r.out.over || !slices.ContainsFunc(todo, func(of *outFile) bool { return len(of.f.Imports) > 0 }) {
		return
	}
	why := ""
	for i, pl := range platforms {
		groups := map[string][]*outFile{}
		for _, of := range todo {
			if of.on>>i&1 == 1 {
				groups[of.f.Name.Name] = append(groups[of.f.Name.Name], of)
			}
		}
		ty := outTyperOf(t, dir, pl)
		if len(groups) == 0 {
			continue
		} else if ty == nil {
			why = cmp.Or(why, pl[0]+"/"+pl[1]+": no typer of the base guard's")
			continue
		}
		im, self := r.outImporterOf(ty), (*types.Package)(nil)
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
			check := im
			if strings.HasSuffix(name, "_test") && self != nil {
				check = im.forTest(self)
			}
			pkg, errs, ok := outCheck(r.ctx, r.listEnd, check, cmp.Or(ty.pathOf(dir), dir)+map[bool]string{true: "_test"}[strings.HasSuffix(name, "_test")], r.out.fset, files, info)
			if !ok {
				r.out.over = true
				return
			} else if e := cmp.Or(check.failed, unexpected(errs)); e != "" {
				why, check.failed = cmp.Or(why, pl[0]+"/"+pl[1]+": "+e), ""
				continue
			} else if !strings.HasSuffix(name, "_test") {
				self = pkg
			}
			r.outTypedRefs(groups[name], info)
		}
	}
	if why != "" {
		r.outWarn(dir, "decision A's check by types could not finish ("+bound(why)+"): its signals by types for this directory are incomplete: review it")
	}
}

// unexpected is the first error the base guard does not tolerate: it
// tolerates a name declared again in a file under a tag it does not model.
func unexpected(errs []error) string {
	for _, err := range errs {
		if te, ok := err.(types.Error); !ok || !redeclared.MatchString(te.Msg) && !otherDecl.MatchString(te.Msg) && !methodDecl.MatchString(te.Msg) {
			return err.Error()
		}
	}
	return ""
}

// outTypedRefs adds one check's references by types to its files, a key's
// largest count on any platform, and its S uses to their bindings.
func (r *run) outTypedRefs(ofs []*outFile, info *types.Info) {
	names := map[types.Object]string{}
	for _, of := range ofs {
		counts := map[string]int{}
		for _, u := range of.units {
			r.typedIn(u, info, names, func(pkg, ref string) {
				if r.charge(len(u.where)+len(ref), 0) && u.where != "" {
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

// sObj: o, named name, is in S: the layer's, or the base guard's as its
// sensitive classifies it (Opus #280 L1); testing's, reflect's and unsafe's
// values have the base guard's own rules.
func sObj(o types.Object, name string) bool {
	if o == nil || o.Pkg() == nil || slices.Contains([]string{"testing", "reflect", "unsafe"}, o.Pkg().Path()) {
		return false
	}
	return outS(o.Pkg().Path()) || sPkg(o.Pkg().Path()) && sensitive(o.Pkg().Path()+"."+name) > 0
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
