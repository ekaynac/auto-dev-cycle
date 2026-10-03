// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// GoName turns a snake_case code into an exported Go identifier part.
func GoName(code string) string {
	var b strings.Builder
	for _, part := range strings.Split(code, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// GoNames maps each code to its GoName, refusing an empty name and two codes
// that map to the same name (a__b, a_b_ and a_b all give AB).
func GoNames(codes []string) (map[string]string, error) {
	out := make(map[string]string, len(codes))
	byName := map[string]string{}
	for _, c := range codes {
		n := GoName(c)
		if n == "" {
			return nil, fmt.Errorf("code %q gives an empty Go name", c)
		}
		if prev, dup := byName[n]; dup {
			return nil, fmt.Errorf("codes %q and %q both give the Go name %s", prev, c, n)
		}
		byName[n], out[c] = c, n
	}
	return out, nil
}

// Finding is one vocabulary violation in source code.
type Finding struct {
	Pos     string `json:"pos"` // file:line:column, relative to the scanned root
	Problem string `json:"problem"`
}

// skipDirs are never scanned: generated code, fixture repositories, VCS and
// dependency trees. (go list already skips testdata and dot directories.)
var skipDirs = map[string]bool{".git": true, "gen": true, "fixtures": true, "node_modules": true, "testdata": true, ".claude": true}

// genFile is the generated file whose constants are the only allowed source
// of a reason.
const genFile = "vocab.gen.go"

// CheckSource type-checks the Go packages of the module at root (go list
// ./..., non-test files) and reports:
//   - every constant expression of a reason type that is not a generated
//     constant from vocab.gen.go used by name (or converted): literals, named
//     constants, concatenations and conversions in any position, through
//     aliased and dot imports. A registered value is reported as a bypass, an
//     unregistered one as unregistered (docs/design/m0/02 `adc dev vocab check`);
//   - every constant flowing into a reason destination (a struct field, an
//     assignment, a typed declaration, a return, an argument, a channel
//     send) that is not a generated constant, so string-typed values such as
//     string("x") or a typed string constant are caught where they become
//     reasons;
//   - every generated constant whose code is deprecated, where it is written
//     into a result or record field (readers may still compare against it,
//     keep it in a local or a table, or return it);
//   - every generic function or type instantiated with a reason type, whose
//     reasons come from type-parameter literals the scan cannot see.
//
// The reason types are vocab.Reason and the generated types.ReasonCode, the
// type of every reason field of the contract records.
//
// Matching rule: the reason type is any named type called Reason declared in
// a package named vocab, and the generated constants are the constants
// declared in that package's file vocab.gen.go. Matching by package name, not
// import path, lets the tests use a synthetic module.
//
// The scan sees source constants only. A reason built at run time (from a
// non-constant string, a []byte, a pointer) is the emission boundary's job:
// results and records validated against the vocabulary when they are
// written. That boundary is a separate change; until it lands, the only
// result product code emits is the CLI's fixed check_unloadable diagnostic.
// Not scanned: test files (they build malformed results on purpose) and
// TypeScript (there is no product TS emitter yet).
//
// A package that does not load or type-check, wherever it lives (skipped
// directories and imported packages included), makes the check fail with an
// error, never pass.
func (reg *Registry) CheckSource(root string) ([]Finding, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Dir: absRoot,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages under %s: %w", root, err)
	}
	// Every package must load and type-check before any is excluded from the
	// scan: a failure in a skipped directory or in an imported package is a
	// failure of the check, never a pass.
	var loadErrs []string
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
		if len(pkg.Errors) == 0 && (pkg.IllTyped || pkg.TypesInfo == nil) {
			loadErrs = append(loadErrs, pkg.PkgPath+": no complete type information")
		}
	}
	if len(loadErrs) > 0 {
		sort.Strings(loadErrs)
		return nil, fmt.Errorf("packages under %s do not type-check:\n  %s", root, strings.Join(loadErrs, "\n  "))
	}
	var found []located
	for _, pkg := range pkgs {
		if skipped(absRoot, pkg) {
			continue
		}
		for _, file := range pkg.Syntax {
			name := pkg.Fset.Position(file.Pos()).Filename
			if pkg.Name == "vocab" && filepath.Base(name) == genFile {
				continue
			}
			seen := map[token.Pos]bool{}
			for _, e := range reasonConstants(pkg.Fset, pkg.TypesInfo, file) {
				seen[e.Pos()] = true
				found = append(found, reg.finding(absRoot, pkg, e))
			}
			for _, d := range reasonDestinations(pkg.TypesInfo, file) {
				tv, ok := pkg.TypesInfo.Types[d.e]
				if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || seen[d.e.Pos()] {
					continue
				}
				seen[d.e.Pos()] = true
				if !isGenerated(pkg.Fset, pkg.TypesInfo, d.e) {
					found = append(found, reg.finding(absRoot, pkg, d.e))
				} else if f, ok := reg.deprecated(absRoot, pkg, d.e); ok && d.write {
					found = append(found, f)
				}
			}

			for _, id := range reasonInstances(pkg.TypesInfo, file) {
				found = append(found, locate(absRoot, pkg, id.Pos(), fmt.Sprintf(
					"generic %s instantiated with a reason type: its reasons cannot be checked statically; use the generated constants", id.Name)))
			}
		}
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages under %s", root)
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.col < b.col
	})
	var out []Finding
	for _, f := range found {
		out = append(out, f.Finding)
	}
	return out, nil
}

// skipped reports a package that lives under one of skipDirs.
func skipped(absRoot string, pkg *packages.Package) bool {
	if len(pkg.GoFiles) == 0 {
		return false
	}
	rel, err := filepath.Rel(absRoot, filepath.Dir(pkg.GoFiles[0]))
	if err != nil {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if skipDirs[part] {
			return true
		}
	}
	return false
}

// reasonConstants returns the outermost constant expressions of a reason
// type in file that are not a generated constant used by name (or
// converted).
func reasonConstants(fset *token.FileSet, info *types.Info, file *ast.File) []ast.Expr {
	var all []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		tv, ok := info.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || !isReason(tv.Type) {
			return true
		}
		all = append(all, e)
		return false // the outermost expression is the finding
	})
	var cands []ast.Expr
	for _, e := range all {
		if !isGenerated(fset, info, e) {
			cands = append(cands, e)
		}
	}
	return cands
}

// reasonInstances returns the generic functions and types in file
// instantiated with a reason type (directly or as an element type): code
// inside them builds reasons the scan cannot see, from literals of a type
// parameter.
func reasonInstances(info *types.Info, file *ast.File) []*ast.Ident {
	var out []*ast.Ident
	for id, inst := range info.Instances {
		if id.Pos() < file.Pos() || id.Pos() > file.End() {
			continue
		}
		for i := 0; i < inst.TypeArgs.Len(); i++ {
			if containsReason(inst.TypeArgs.At(i)) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func containsReason(t types.Type) bool { return reasonInside(t, map[types.Type]bool{}) }

// reasonInside reports a reason type anywhere in t: through aliases, the
// underlying type of named types, element and key types, struct fields and
// function signatures. seen breaks cycles through named types.
func reasonInside(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if isReason(t) {
		return true
	}
	switch u := types.Unalias(t).(type) {
	case *types.Named:
		return reasonInside(u.Underlying(), seen)
	case *types.Pointer:
		return reasonInside(u.Elem(), seen)
	case *types.Slice:
		return reasonInside(u.Elem(), seen)
	case *types.Array:
		return reasonInside(u.Elem(), seen)
	case *types.Map:
		return reasonInside(u.Key(), seen) || reasonInside(u.Elem(), seen)
	case *types.Chan:
		return reasonInside(u.Elem(), seen)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if reasonInside(u.Field(i).Type(), seen) {
				return true
			}
		}
	case *types.Signature:
		for _, tup := range []*types.Tuple{u.Params(), u.Results()} {
			for i := 0; i < tup.Len(); i++ {
				if reasonInside(tup.At(i).Type(), seen) {
					return true
				}
			}
		}
	}
	return false
}

// isReason matches the named type Reason of a package named vocab, and the
// generated alias ReasonCode of a package named types (the reason fields of
// the contract records, e.g. types.Predicate.Reason).
func isReason(t types.Type) bool {
	for a, ok := t.(*types.Alias); ok; a, ok = t.(*types.Alias) {
		obj := a.Obj()
		if obj.Name() == "ReasonCode" && obj.Pkg() != nil && isGenPkg(obj.Pkg()) {
			return true
		}
		t = a.Rhs() // type Why = types.ReasonCode is a reason type too
	}
	n, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return obj.Name() == "Reason" && obj.Pkg() != nil && obj.Pkg().Name() == "vocab"
}

// isGenPkg: the generated contract types package (gen/go/types).
func isGenPkg(p *types.Package) bool {
	return p.Name() == "types" && strings.HasSuffix(p.Path(), "gen/go/types")
}

// dest is a value flowing into a reason destination. write marks the
// destinations that statically prove emission: a field of a vocab.Result or
// of a generated contract record (keyed, positional, assigned through a
// selector or *&). A local
// variable, a lookup table, a return value or an argument may belong to a
// reader, so a deprecated constant is allowed there.
type dest struct {
	e     ast.Expr
	write bool
}

// reasonDestinations returns the values flowing into a reason-typed
// destination: struct fields (keyed or positional), assignments, typed
// variable declarations, returns, call arguments, channel sends, and the
// elements and keys of reason-typed composite literals. The destination keeps the reason
// meaning that a string-typed value (string("x"), a typed string constant)
// does not carry itself.
func reasonDestinations(info *types.Info, file *ast.File) []dest {
	var out []dest
	add := func(t types.Type, e ast.Expr, write bool) {
		if t != nil && isReason(t) {
			out = append(out, dest{e, write})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			t := info.TypeOf(x)
			if t == nil {
				return true
			}
			emits := isEmitted(t)
			switch u := t.Underlying().(type) {
			case *types.Struct:
				for i, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							if v, ok := info.Uses[id].(*types.Var); ok {
								add(v.Type(), kv.Value, emits)
							}
						}
					} else if i < u.NumFields() {
						add(u.Field(i).Type(), el, emits)
					}
				}
			case *types.Slice, *types.Array, *types.Map:
				var key, elem types.Type
				switch c := u.(type) {
				case *types.Slice:
					elem = c.Elem()
				case *types.Array:
					elem = c.Elem()
				case *types.Map:
					key, elem = c.Key(), c.Elem()
				}
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if key != nil {
							add(key, kv.Key, false)
						}
						add(elem, kv.Value, false)
					} else {
						add(elem, el, false)
					}
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i := range x.Lhs {
					add(info.TypeOf(x.Lhs[i]), x.Rhs[i], writesEmittedField(info, x.Lhs[i]))
				}
			}
		case *ast.ValueSpec:
			if x.Type != nil {
				for _, v := range x.Values {
					add(info.TypeOf(x.Type), v, false)
				}
			}
		case *ast.SendStmt:
			if ch, ok := types.Unalias(info.TypeOf(x.Chan)).Underlying().(*types.Chan); ok {
				add(ch.Elem(), x.Value, false)
			}
		case *ast.CallExpr:
			if tv, ok := info.Types[x.Fun]; ok && tv.IsType() {
				return true // a conversion, not a call
			}
			sig, ok := types.Unalias(info.TypeOf(x.Fun)).(*types.Signature)
			if !ok {
				return true
			}
			params := sig.Params()
			for i, a := range x.Args {
				var pt types.Type
				switch {
				case sig.Variadic() && i >= params.Len()-1 && !x.Ellipsis.IsValid():
					if s, ok := params.At(params.Len() - 1).Type().(*types.Slice); ok {
						pt = s.Elem()
					}
				case i < params.Len():
					pt = params.At(i).Type()
				}
				add(pt, a, false)
			}
		case *ast.FuncDecl, *ast.FuncLit:
			var sig *types.Signature
			var body *ast.BlockStmt
			if f, ok := x.(*ast.FuncDecl); ok {
				if obj, ok := info.Defs[f.Name].(*types.Func); ok {
					sig, _ = obj.Type().(*types.Signature)
				}
				body = f.Body
			} else {
				f := x.(*ast.FuncLit)
				sig, _ = info.TypeOf(f).(*types.Signature)
				body = f.Body
			}
			if sig == nil || body == nil {
				return true
			}
			ast.Inspect(body, func(m ast.Node) bool {
				if _, ok := m.(*ast.FuncLit); ok {
					return false // its returns belong to its own signature
				}
				if r, ok := m.(*ast.ReturnStmt); ok && len(r.Results) == sig.Results().Len() {
					for i, e := range r.Results {
						add(sig.Results().At(i).Type(), e, false)
					}
				}
				return true
			})
		}
		return true
	})
	return out
}

// isEmitted: t (or what it points to) is a vocab.Result or a generated
// contract record, the types whose fields are written out.
func isEmitted(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	obj := n.Obj()
	return (obj.Name() == "Result" && obj.Pkg().Name() == "vocab") || isGenPkg(obj.Pkg())
}

// writesEmittedField: e, seen through parentheses and *&, is a field of a
// result or a contract record (res.Reason, *(&res.Reason)).
func writesEmittedField(info *types.Info, e ast.Expr) bool {
	for {
		e = ast.Unparen(e)
		star, ok := e.(*ast.StarExpr)
		if !ok {
			break
		}
		amp, ok := ast.Unparen(star.X).(*ast.UnaryExpr)
		if !ok || amp.Op != token.AND {
			break
		}
		e = amp.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	s, ok := info.Selections[sel]
	return ok && s.Kind() == types.FieldVal && isEmitted(s.Recv())
}

// isGenerated: e names (directly, or qualified) a constant declared in the
// vocab package's generated file.
func isGenerated(fset *token.FileSet, info *types.Info, e ast.Expr) bool {
	// A conversion of a generated constant (string(vocab.ReasonX) into a
	// record's ReasonCode field) is the constant.
	if call, ok := ast.Unparen(e).(*ast.CallExpr); ok && len(call.Args) == 1 {
		if tv, ok := info.Types[call.Fun]; ok && tv.IsType() {
			return isGenerated(fset, info, call.Args[0])
		}
	}
	c := namedConst(info, e)
	if c == nil || c.Pkg() == nil || c.Pkg().Name() != "vocab" || !c.Pos().IsValid() {
		return false
	}
	// Only the Reason* constants: the generated Hold* constants are holds,
	// never reasons.
	return strings.HasPrefix(c.Name(), "Reason") && filepath.Base(fset.Position(c.Pos()).Filename) == genFile
}

func namedConst(info *types.Info, e ast.Expr) *types.Const {
	e = ast.Unparen(e)
	var id *ast.Ident
	switch x := e.(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return nil
	}
	c, _ := info.Uses[id].(*types.Const)
	return c
}

// finding describes e: a bypass when its value is registered, otherwise an
// unregistered reason.
// located is a finding with its sort key.
type located struct {
	Finding
	file      string
	line, col int
}

// deprecated reports a generated constant whose code the registry has
// deprecated, where it is written (a field, an assignment, a declaration, a
// return); reading it (a comparison, a switch case, a lookup) stays allowed.
func (reg *Registry) deprecated(absRoot string, pkg *packages.Package, e ast.Expr) (located, bool) {
	code := constant.StringVal(pkg.TypesInfo.Types[e].Value)
	if x, ok := reg.Reasons[code]; ok && x.Deprecated != nil {
		return locate(absRoot, pkg, e.Pos(), fmt.Sprintf("deprecated reason %q (since %s); writers may not emit it", code, *x.Deprecated)), true
	}
	return located{}, false
}

// locate places a finding at pos, relative to the scanned root.
func locate(absRoot string, pkg *packages.Package, p token.Pos, problem string) located {
	pos := pkg.Fset.Position(p)
	rel, err := filepath.Rel(absRoot, pos.Filename)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = pos.Filename
	}
	rel = filepath.ToSlash(rel)
	return located{
		Finding: Finding{Pos: fmt.Sprintf("%s:%d:%d", rel, pos.Line, pos.Column), Problem: problem},
		file:    rel, line: pos.Line, col: pos.Column,
	}
}

func (reg *Registry) finding(absRoot string, pkg *packages.Package, e ast.Expr) located {
	tv := pkg.TypesInfo.Types[e]
	code := constant.StringVal(tv.Value)
	form := "constant expression"
	switch x := ast.Unparen(e).(type) {
	case *ast.BasicLit:
		form = "raw literal"
	case *ast.Ident, *ast.SelectorExpr:
		if c := namedConst(pkg.TypesInfo, x); c != nil {
			form = "non-generated constant " + c.Name()
		}
	case *ast.CallExpr:
		form = "conversion"
	}
	problem := fmt.Sprintf("reason %q as a %s; use vocab.Reason%s", code, form, GoName(code))
	if _, registered := reg.Reasons[code]; !registered {
		problem = fmt.Sprintf("unregistered reason %q (%s)", code, form)
	}
	return locate(absRoot, pkg, e.Pos(), problem)
}
