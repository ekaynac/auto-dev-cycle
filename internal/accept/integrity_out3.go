// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"context"
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
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// P14b-out-3a: S by real types, warnings only: S methods through values
// ((*flag.FlagSet).Parse), S type arguments (g[flag.FlagSet]), S uses by
// types in changed bodies. A shared object is the base guard's alone, read
// only, or the layer's alone; its findings are Typed entries apart, and the
// text layer's are main's, byte for byte (Astra #282 r3 H1).

// outView is a read-only snapshot of a base guard typer: all a worker reads
// of the base guard (never the run, its tree or the typer).
type outView struct {
	fset   *token.FileSet         // the base guard's positions of its parsed syntax
	own    map[string][]*ast.File // an own or vendored package it parsed, by import path
	export map[string]string      // the export data file it listed, by import path
}

// outImporter is one coherent package graph a typer, all its own (Astra #280
// H2): own and vendored packages checked from the base guard's syntax,
// bodies skipped; others from the export data it listed (maxOutExport); for
// an external test, the layer's own check of the package under test (Opus
// #280 M2). An import it cannot serve fails: the directory is incomplete.
type outImporter struct {
	v      outView
	gc     types.Importer
	pkgs   map[string]*types.Package // checked from the base guard's syntax (nil: being checked, or failed)
	errs   map[string]error          // why one failed
	self   *types.Package            // for an external test (forTest): its package under test
	budget *int64                    // the export data bytes it may still read: its own counter
	failed string
}

// maxOutExport bounds the export data the layer's importer of a typer reads,
// as the base guard's typer is bounded (Opus #282 L2).
var maxOutExport = maxExport

func (r *run) outImporterOf(ty *typer) *outImporter {
	if im := r.out.importers[ty]; im != nil {
		return im
	} else if r.out.importers == nil { // a run made without outRun (a test)
		r.out.importers = map[*typer]*outImporter{}
	}
	im := &outImporter{v: outView{fset: ty.fset, own: map[string][]*ast.File{}, export: map[string]string{}}, pkgs: map[string]*types.Package{}, errs: map[string]error{}, budget: new(int64)}
	*im.budget = maxOutExport
	for d, fs := range ty.parsed { // only what the base guard could check (Opus #282 H1): its other syntax is never checked
		p := ty.pathOf(d)
		if rel, ok := strings.CutPrefix(d, path.Join(ty.root, "vendor")+"/"); ok {
			p = rel
		}
		if fs != nil && p != "" && ty.why == "" && ty.failed["dir "+d] == "" && ty.failed[p] == "" {
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

// forTest is im's view for self's external test (Opus #280 M2): a package
// reaching self's path is checked again against self; any other is im's.
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

// outWorkers counts the layer's checks running, any run's (Astra #280 M5):
// a dropped worker keeps its slot until it ends. outDropped counts the
// dropped ones still running (Opus #282 M1).
var outWorkers, outDropped atomic.Int32

const maxOutWorkers = 1

var (
	outSlotWait  = 2 * time.Second  // a run's wait for a peer's slot, from its start: past it, or while a dropped worker runs, the types check stops (tests shorten it)
	outCheckHook func(event string) // a test's: a caller's drop ("drop"), a worker's very end ("ended": its slot freed, accounting done, result published)
	maxOutCheck  time.Duration      // one check's bound (0: the run's deadline alone; tests set it)
)

// outCheck type-checks files in a worker holding only them, the importer and
// info, until the deadline (or maxOutCheck) or ctx's end; no slot, a dropped
// worker running, a drop or a panic: false. One CAS decides a worker's end or
// drop (Astra #282 M2): an ended one frees its slot before its result; a
// dropped one is uncounted at its real end, after its caller counted it.
func outCheck(ctx context.Context, start, deadline time.Time, im types.Importer, path string, fset *token.FileSet, files []*ast.File, info *types.Info) (*types.Package, []error, bool) {
	if maxOutCheck > 0 && time.Now().Add(maxOutCheck).Before(deadline) {
		deadline = time.Now().Add(maxOutCheck)
	}
	waitEnd := start.Add(outSlotWait)
	if deadline.Before(waitEnd) {
		waitEnd = deadline
	}
	for outWorkers.Add(1) > maxOutWorkers { // a live peer's slot, waited for briefly
		outWorkers.Add(-1)
		if outDropped.Load() > 0 || !time.Now().Before(waitEnd) {
			return nil, nil, false
		}
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-time.After(min(10*time.Millisecond, time.Until(waitEnd))):
		}
	}
	if outDropped.Load() > 0 || !time.Now().Before(deadline) || ctx.Err() != nil { // never a worker past the deadline
		outWorkers.Add(-1)
		return nil, nil, false
	}
	type result struct {
		pkg  *types.Package
		errs []error
		ok   bool
	}
	done, state, counted := make(chan result, 1), new(atomic.Int32), make(chan struct{}) // state: 0 running, 1 ended, 2 dropped
	go func() {
		var res result
		defer func() {
			if recover() != nil {
				res.ok = false
			}
			ended := state.CompareAndSwap(0, 1)
			if !ended { // dropped: its caller counted it first
				<-counted
				outDropped.Add(-1)
			}
			outWorkers.Add(-1) // its slot, before its result
			if ended {
				done <- res
			}
			if outCheckHook != nil {
				outCheckHook("ended")
			}
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
	if outCheckHook != nil {
		outCheckHook("drop")
	}
	if !state.CompareAndSwap(0, 2) { // it ended first: its result is published
		res := <-done
		return res.pkg, res.errs, res.ok
	}
	outDropped.Add(1)
	close(counted)
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

// outTyped checks dir's packages in t by types: each platform's own files
// with its base guard typer (Astra #280 H3, Opus #280 H1: no union), the
// package before its external test; a key counts its largest on any
// platform. An import it cannot serve, or an error the base guard does not
// tolerate, makes the directory incomplete: one warning, nothing added.
func (r *run) outTyped(dir string, t *tree, ofs []*outFile) {
	var todo []*outFile
	for _, of := range ofs {
		if !of.typed {
			of.typed, todo = true, append(todo, of)
		}
	}
	if r.out.over || r.out.typedOff || !slices.ContainsFunc(todo, func(of *outFile) bool { return len(of.f.Imports) > 0 }) {
		return
	} else if !r.chargeTypes(0) {
		r.typesOff(dir)
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
				if files = append(files, of.f); !r.chargeTypes(int(of.f.FileEnd - of.f.FileStart)) {
					r.typesOff(dir)
					return
				}
			}
			info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Instances: map[*ast.Ident]types.Instance{}}
			check := im
			if strings.HasSuffix(name, "_test") && self != nil {
				check = im.forTest(self)
			}
			if outCheckSeen != nil {
				outCheckSeen(files)
			}
			pkg, errs, ok := outCheck(r.ctx, r.started, r.listEnd, check, cmp.Or(ty.pathOf(dir), dir)+map[bool]string{true: "_test"}[strings.HasSuffix(name, "_test")], r.out.fset, files, info)
			if !ok {
				r.typesOff(dir)
				return
			} else if e := cmp.Or(check.failed, unexpected(errs)); e != "" {
				why, check.failed = cmp.Or(why, pl[0]+"/"+pl[1]+": "+e), ""
				continue
			} else if !strings.HasSuffix(name, "_test") {
				self = pkg
			}
			if r.outTypedRefs(groups[name], info); !r.chargeTypes(0) {
				r.typesOff(dir)
				return
			}
		}
	}
	if why != "" {
		r.outTypedWarn(dir, "decision A's check by types could not finish ("+bound(why)+"): its signals by types for this directory are incomplete: review it")
	}
}

// maxOutTypeWork bounds all the types check's work (0: 16 × maxRead): file
// bytes, visits, names, keys, platform masks past the first. The text
// layer's counter keeps main's charges (Astra #282 H1); past it, typesOff.
var maxOutTypeWork int

func (r *run) chargeTypes(n int) bool {
	r.out.tnodes += n
	return r.out.tnodes <= cmp.Or(maxOutTypeWork, int(16*maxRead)) && r.ctx.Err() == nil
}

// outTypedWarn is a typed warning: the report gives it what the base
// guard's findings and the text layer's leave (Astra #282 r2 H1).
func (r *run) outTypedWarn(p, what string) {
	r.out.warnings = append(r.out.warnings, Block{vocab.ReasonTestIntegrityViolation, p, what, true, true})
}

// outTypedSignals warns for what only the types check found in dir, from
// typed facts alone, less what the text facts report (a function the
// references warning names, a binding using the package by text): no text
// entry ever changes (Astra #282 r3 H1). After a types stop, none.
func (r *run) outTypedSignals(dir string, b, a outFacts, changed map[string][]string) {
	if !r.chargeTypes(len(b.trefs) + len(a.trefs) + len(b.tprints) + len(a.tprints)) {
		r.typesOff(dir)
	}
	if r.out.typedOff {
		return
	}
	refs, bodies := map[string][]string{}, map[string][]string{} // "file\x00package": functions, bindings
	for _, k := range keys(b.trefs, a.trefs) {
		if f := strings.Split(k, "\x00"); a.trefs[k] != b.trefs[k] {
			refs[f[0]+"\x00"+f[1]] = append(refs[f[0]+"\x00"+f[1]], f[2])
		}
	}
	for _, k := range keys(b.tuses, a.tuses) { // in a binding outBodies reads as changed
		f := strings.Split(k, "\x00") // file, binding, package
		kb, fp, fn := f[0]+"\x00"+f[1], f[0]+"\x00"+f[2], strings.TrimPrefix(f[1], "func ")
		print := func(fa outFacts) (string, bool) { // a text binding's, or an init or hook body's by types (P14b-out-3b)
			if h, ok := fa.prints[kb]; ok {
				return h, ok
			}
			h, ok := fa.tprints[kb]
			return h, ok
		}
		bp, inB := print(b)
		if ap, inA := print(a); !inB && !inA || inB && inA && bp == ap || (!inB || !inA) && strings.HasPrefix(f[1], "func ") {
			continue
		} else if !b.uses[kb][f[2]] && !a.uses[kb][f[2]] && !slices.Contains(changed[fp], fn) && !slices.Contains(refs[fp], fn) {
			bodies[fp] = append(bodies[fp], f[1])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(refs)) {
		fns := slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(refs[k]))), func(fn string) bool { return slices.Contains(changed[k], fn) })
		if file, pkg, _ := strings.Cut(k, "\x00"); len(fns) > 0 {
			r.outTypedWarn(file, "references into "+pkg+" added, removed, moved or changed ("+preview(fns)+"): review it")
		}
	}
	for _, k := range slices.Sorted(maps.Keys(bodies)) {
		file, pkg, _ := strings.Cut(k, "\x00")
		r.outTypedWarn(file, "changed bodies that use "+pkg+" ("+preview(bodies[k])+"): review it")
	}
}

// preview is a count and the first three names, as the text warnings show them.
func preview(ns []string) string {
	return fmt.Sprintf("%d: %s%s", len(ns), strings.Join(ns[:min(3, len(ns))], ", "), map[bool]string{true: ", …"}[len(ns) > 3])
}

// outTypesHook runs all the types check's work (off: main's layer); outDone
// and outCheckSeen (tests') see the text layer's counters at a run's end and
// each file set the layer checks.
var (
	outTypesHook = true
	outDone      func(nodes, tags int, over bool)
	outCheckSeen func(files []*ast.File)
)

// typesOff stops the types check for the run, with one warning, never
// P14b-out-1's work bound (no libc exhaustion block: Opus #282 probe D);
// its signals are then left out (outTypedSignals).
func (r *run) typesOff(dir string) {
	if !r.out.typedOff {
		r.out.typedOff = true
		r.outTypedWarn(dir, "decision A's check by types stopped (a worker still busy, its deadline or its own work bound): its signals by types are incomplete from this directory on: review it")
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

// outTypedRefs adds one check's references by types to its files' trefs (a
// key's largest count on any platform) and S uses to tuses, never to refs;
// an init or hook body's S uses outside its literals go to its own binding,
// "init body" or "TestMain body", fingerprinted by typedPrint (P14b-out-3b).
func (r *run) outTypedRefs(ofs []*outFile, info *types.Info) {
	names := map[types.Object]string{}
	for _, of := range ofs {
		counts, printed := map[string]int{}, of.tprints != nil
		if !printed {
			of.tprints = map[string]string{}
		}
		for _, u := range of.units {
			r.typedIn(u, false, info, names, func(pkg, ref string) {
				if r.chargeTypes(len(u.where)+len(ref)) && u.where != "" {
					counts[pkg+"\x00"+u.where+"\x00"+ref]++
				}
				of.tuses[u.bind+"\x00"+pkg] = true
			})
			if d, ok := u.n.(*ast.FuncDecl); ok && u.bind == "init literal" { // its body outside its literals (P14b-out-3b)
				bind := d.Name.Name + " body"
				if !printed {
					r.typedPrint(of, d, bind)
				}
				r.typedIn(u, true, info, names, func(pkg, ref string) {
					if r.chargeTypes(len(ref)) {
						of.tuses[bind+"\x00"+pkg] = true
					}
				})
			}
		}
		for k, n := range counts {
			of.trefs[k] = max(of.trefs[k], n)
		}
	}
}

// typedPrint combines d's spans outside its function literals (the text
// binding "init literal" holds those) into of's typed fingerprint for
// bind, sha256(prev||span) in source order, on the types budget.
func (r *run) typedPrint(of *outFile, d *ast.FuncDecl, bind string) {
	tf, at := r.out.fset.File(of.f.Pos()), d.Pos()
	var spans [][2]token.Pos
	ast.Inspect(d, func(n ast.Node) bool {
		if l, ok := n.(*ast.FuncLit); ok {
			spans, at = append(spans, [2]token.Pos{at, l.Pos()}), l.End()
			return false
		}
		return n == nil || r.chargeTypes(1)
	})
	for _, sp := range append(spans, [2]token.Pos{at, d.End()}) {
		s := of.src[tf.Offset(sp[0]):tf.Offset(sp[1])]
		if !r.chargeTypes(len(s)) {
			return
		}
		h := sha256.Sum256(append([]byte(of.tprints[bind]), s...))
		of.tprints[bind] = string(h[:])
	}
}

// sObj: o, named name, is in S: the layer's, or the base guard's as its
// sensitive classifies it (Opus #280 L1); testing, reflect and unsafe excepted.
func sObj(o types.Object, name string) bool {
	if o == nil || o.Pkg() == nil || slices.Contains([]string{"testing", "reflect", "unsafe"}, o.Pkg().Path()) {
		return false
	}
	return outS(o.Pkg().Path()) || sPkg(o.Pkg().Path()) && sensitive(o.Pkg().Path()+"."+name) > 0
}

// osReads are an *os.File's plain reads (Opus #282 M2): inert, as the base
// guard's osRead for os's functions (os.FileInfo's are io/fs's, outside S).
var osReads = []string{"Read", "ReadAt", "ReadDir", "Readdir", "Readdirnames", "Stat", "Name", "Seek"}

// sMethod: o is a method in S, classified by its receiver: every method of
// an S package but os.File's plain reads (testing, reflect, unsafe excepted).
func sMethod(o *types.Func) bool {
	p := ""
	if o.Pkg() != nil {
		p = o.Pkg().Path()
	}
	recv := ""
	if sig, _ := o.Type().(*types.Signature); sig != nil && sig.Recv() != nil {
		t := sig.Recv().Type()
		if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
			t = ptr.Elem()
		}
		if n, ok := types.Unalias(t).(*types.Named); ok {
			recv = n.Obj().Name()
		}
	}
	return (outS(p) || sPkg(p)) && !slices.Contains([]string{"testing", "reflect", "unsafe"}, p) &&
		!(p == "os" && recv == "File" && slices.Contains(osReads, o.Name()))
}

// typedIn calls add for each S method through a value and each S type
// argument in u (inside its function literals only, with lits; outside
// them, with body).
func (r *run) typedIn(u unit, body bool, info *types.Info, names map[types.Object]string, add func(pkg, ref string)) {
	visit := func(n ast.Node) bool {
		if n == nil || !r.chargeTypes(1) {
			return n == nil
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if s := info.Selections[x]; s != nil && s.Kind() != types.FieldVal {
				if o, _ := s.Obj().(*types.Func); o != nil && sMethod(o) {
					add(o.Pkg().Path(), r.methodName(o.Origin(), names))
				}
			}
		case *ast.Ident:
			if in, ok := info.Instances[x]; ok && in.TypeArgs != nil && r.chargeTypes(len(x.Name)) {
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
	if !u.lits && !body {
		ast.Inspect(u.n, visit)
		return
	}
	ast.Inspect(u.n, func(n ast.Node) bool { // its function literals alone, or all but them (body)
		if l, ok := n.(*ast.FuncLit); ok {
			if !body {
				ast.Inspect(l, visit)
			}
			return false
		} else if body {
			return visit(n)
		}
		return n == nil || r.chargeTypes(1)
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
	r.chargeTypes(len(o.Name()))
	names[o] = "(" + on + ")." + bound(o.Name())
	return names[o]
}

func (r *run) typeName(o types.Object, names map[types.Object]string) string {
	if q, ok := names[o]; ok {
		return q
	}
	r.chargeTypes(len(o.Pkg().Path()) + len(o.Name()))
	names[o] = bound(o.Pkg().Path()) + "." + bound(o.Name())
	return names[o]
}
