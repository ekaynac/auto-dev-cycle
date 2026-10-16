// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
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
}

// units are d's parts: a function, bound by its bounded name; each var spec,
// bound by all its names (blank ones too: Astra #276 r3 #5); initialization
// and the real TestMain(*testing.M) hook of a _test.go file, whose function
// literals alone are the layer's (r3 #4), bound as one; another declaration,
// unbound. Names are bounded once here, their original bytes charged (r3 #6).
func (r *run) units(d ast.Decl, p, testing string) []unit {
	switch d := d.(type) {
	case *ast.FuncDecl:
		switch {
		case d.Recv == nil && (d.Name.Name == "init" || strings.HasSuffix(p, "_test.go") && d.Name.Name == "TestMain" && hook(d, testing)):
			return []unit{{d, "init literal", "init literal", true}}
		case d.Recv != nil && r.charge(len(d.Name.Name)+len(recv(d)), 0):
			w := bound(recv(d) + "." + d.Name.Name)
			return []unit{{d, w, "func " + w, false}}
		case d.Recv == nil && r.charge(len(d.Name.Name), 0):
			return []unit{{d, bound(d.Name.Name), "func " + bound(d.Name.Name), false}}
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
				us = append(us, unit{sp, "package level", "var " + strings.Join(ns, ", "), false})
			}
			return us
		}
	}
	return []unit{{d, "package level", "", false}}
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

// sObj: o is in S, the base guard's or the layer's (testing's, reflect's
// and unsafe's values have the base guard's own rules).
func sObj(o types.Object) bool {
	return o != nil && o.Pkg() != nil && (sPkg(o.Pkg().Path()) || outS(o.Pkg().Path())) &&
		!slices.Contains([]string{"testing", "reflect", "unsafe"}, o.Pkg().Path())
}

// outTyped type-checks each package of dir in t once, after the base guard
// and with its importer (the gate's first platform), and adds what real
// types show: a method of an S type through a value, keyed by its full name
// ((*flag.FlagSet).Parse), and an S type argument (parse[flag.FlagSet]).
// A type error is the base guard's; a panic stops the layer (a warning).
func (r *run) outTyped(dir string, t *tree, ofs []*outFile) {
	groups := map[string][]*outFile{}
	for _, of := range ofs {
		if !of.typed {
			of.typed, groups[of.f.Name.Name] = true, append(groups[of.f.Name.Name], of)
		}
	}
	if len(groups) == 0 || r.out.over {
		return
	}
	ty := t.typer(r, dir, platforms[0])
	defer func() {
		if p := recover(); p != nil {
			r.out.over = true
		}
	}()
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		var files []*ast.File
		for _, of := range groups[name] {
			files = append(files, of.f)
			if !r.charge(int(of.f.FileEnd-of.f.FileStart), 0) {
				return
			}
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Instances: map[*ast.Ident]types.Instance{}}
		conf := types.Config{Importer: ty, FakeImportC: true, Error: func(error) {}}
		_, _ = conf.Check(cmp.Or(ty.pathOf(dir), dir)+map[bool]string{true: "_test"}[strings.HasSuffix(name, "_test")], r.out.fset, files, info)
		names := map[types.Object]string{}
		for _, of := range groups[name] {
			for _, u := range of.units {
				r.typedIn(u, info, names, func(pkg, ref string) {
					of.refs[pkg+"\x00"+u.where+"\x00"+ref]++
					of.use(u.bind, pkg)
				})
			}
		}
	}
}

// typedIn calls add for each S method through a value and each S type
// argument in u (inside its function literals only, with lits).
func (r *run) typedIn(u unit, info *types.Info, names map[types.Object]string, add func(pkg, ref string)) {
	visit := func(n ast.Node) bool {
		if n == nil || !r.charge(1, 0) {
			return n == nil
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if s := info.Selections[x]; s != nil && s.Kind() != types.FieldVal {
				if o, _ := s.Obj().(*types.Func); sObj(o) {
					add(o.Pkg().Path(), r.methodName(o.Origin(), names))
				}
			}
		case *ast.Ident:
			if in, ok := info.Instances[x]; ok && in.TypeArgs != nil && r.charge(len(x.Name), 0) {
				for t := range in.TypeArgs.Types() {
					if p, ok := types.Unalias(t).(*types.Pointer); ok {
						t = p.Elem()
					}
					if nt, ok := types.Unalias(t).(*types.Named); ok && sObj(nt.Obj()) {
						add(nt.Obj().Pkg().Path(), bound(x.Name)+"["+r.typeName(nt.Obj(), names)+"]")
					}
				}
			}
		}
		return true
	}
	if !u.lits {
		ast.Inspect(u.n, visit)
		return
	}
	ast.Inspect(u.n, func(n ast.Node) bool {
		if l, ok := n.(*ast.FuncLit); ok {
			ast.Inspect(l, visit)
			return false
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
// fingerprint changed and that use S in either version, unless the file's
// references into that package changed (their warning says it already).
func (r *run) outBodies(b, a outFacts, changed map[string][]string) {
	bodies := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(a.prints)) {
		if bp, ok := b.prints[k]; !ok || bp == a.prints[k] {
			continue
		}
		file, bind, _ := strings.Cut(k, "\x00")
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
