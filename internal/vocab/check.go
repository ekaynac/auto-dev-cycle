// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"fmt"
	"go/ast"
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
// dependency trees. They are still loaded and type-checked.
var skipDirs = map[string]bool{".git": true, "gen": true, "fixtures": true, "node_modules": true, "testdata": true, ".claude": true}

// CheckSource is `adc dev vocab check` (docs/design/m0/02 "Reason codes").
//
// The reason type does the main work. A reason is the generated
// types.ReasonCode, an opaque struct (vocab.Reason is an alias): no string
// literal, conversion or type-parameter literal can become one. A value is
// produced only by the generated vocab.Reason* values, by ParseReason (which
// refuses unregistered codes), or by decoding (which does the same); and it
// is written only through MarshalText, which refuses unregistered and
// deprecated codes however it is serialised.
//
// What remains for source to get wrong, CheckSource reports. The vocabulary
// package is identified by its import path (ending in /internal/vocab), never
// by its package name:
//   - any reference to the unchecked constructor types.ReasonCodeOf outside
//     the vocabulary package (a call, or a function value);
//   - any reference to a SetReasonCheck (generated types or contract runtime)
//     outside the vocabulary package; at run time they also refuse any other
//     caller and a second installation;
//   - ParseReason with a constant argument (use the generated value), and
//     ParseReason used as a function value (its argument cannot be checked);
//   - writing to a generated vocab.Reason* value: an assignment, taking its
//     address, or calling a pointer method on it (which takes the address
//     implicitly). The values are immutable.
//
// It type-checks the module's packages (go list ./..., non-test files); a
// package that does not load or type-check, wherever it lives (skipped
// directories and imported packages included), fails the check. Test files
// build malformed results on purpose and are not scanned; there is no product
// TypeScript yet; holds get their type and check with the first emitter.
func (reg *Registry) CheckSource(root string) ([]Finding, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule,
		Dir: absRoot,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages under %s: %w", root, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages under %s", root)
	}
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
	mod := ""
	for _, pkg := range pkgs {
		if pkg.Module != nil && pkg.Module.Main {
			mod = pkg.Module.Path
			break
		}
	}
	if mod == "" {
		return nil, fmt.Errorf("no main module under %s", root)
	}
	mp := modPaths{vocab: mod + "/internal/vocab", gen: mod + "/gen/go/types", contract: mod + "/internal/contract"}
	var found []located
	for _, pkg := range pkgs {
		if skipped(absRoot, pkg) {
			continue
		}
		for _, file := range pkg.Syntax {
			for _, f := range scan(pkg.PkgPath, mp, pkg.TypesInfo, file) {
				found = append(found, locate(absRoot, pkg, f.pos, f.problem))
			}
		}
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
	out := []Finding{}
	for _, f := range found {
		out = append(out, f.Finding)
	}
	return out, nil
}

type raw struct {
	pos     token.Pos
	problem string
}

// modPaths are the exact import paths the rules trust or guard, derived from
// the scanned module: a package elsewhere with the same name or suffix is
// neither trusted nor guarded.
type modPaths struct{ vocab, gen, contract string }

func (m modPaths) isVocab(p *types.Package) bool { return p != nil && p.Path() == m.vocab }
func (m modPaths) isGen(p *types.Package) bool   { return p != nil && p.Path() == m.gen }
func (m modPaths) isContract(p *types.Package) bool {
	return p != nil && p.Path() == m.contract
}

// scan applies the rules to one file of the package at pkgPath.
func scan(pkgPath string, mp modPaths, info *types.Info, file *ast.File) []raw {
	var out []raw
	trusted := pkgPath == mp.vocab
	// Identifiers in call position, to tell a call from a function value.
	called := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				called[f] = true
			case *ast.SelectorExpr:
				called[f.Sel] = true
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			f, ok := info.Uses[x].(*types.Func)
			if !ok || f.Pkg() == nil {
				return true
			}
			switch {
			case trusted:
			case f.Name() == "ReasonCodeOf" && mp.isGen(f.Pkg()):
				out = append(out, raw{x.Pos(), "types.ReasonCodeOf builds an unchecked reason; use a generated vocab.Reason* value or vocab.ParseReason"})
			case f.Name() == "SetReasonCheck" && (mp.isGen(f.Pkg()) || mp.isContract(f.Pkg())):
				out = append(out, raw{x.Pos(), "only the vocabulary package installs the reason check"})
			case f.Name() == "ParseReason" && mp.isVocab(f.Pkg()) && !called[x]:
				out = append(out, raw{x.Pos(), "ParseReason used as a function value; call it directly"})
			}
		case *ast.CallExpr:
			f := callee(info, x)
			if f == nil || f.Pkg() == nil {
				return true
			}
			if f.Name() == "ParseReason" && mp.isVocab(f.Pkg()) && len(x.Args) == 1 {
				if tv, ok := info.Types[x.Args[0]]; ok && tv.Value != nil {
					out = append(out, raw{x.Pos(), fmt.Sprintf("ParseReason(%s) with a constant; use the generated vocab.Reason%s value", tv.Value, GoName(strings.Trim(tv.Value.String(), `"`)))})
				}
			}
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if v := generatedValue(mp, info, l); v != nil {
					out = append(out, raw{l.Pos(), fmt.Sprintf("assignment to the generated reason %s, which is immutable", v.Name())})
				}
			}
		case *ast.RangeStmt:
			// for k, v = range …: both targets are assigned.
			if x.Tok == token.ASSIGN {
				for _, l := range []ast.Expr{x.Key, x.Value} {
					if l == nil {
						continue
					}
					if v := generatedValue(mp, info, l); v != nil {
						out = append(out, raw{l.Pos(), fmt.Sprintf("assignment to the generated reason %s, which is immutable", v.Name())})
					}
				}
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				if v := generatedValue(mp, info, x.X); v != nil {
					out = append(out, raw{x.Pos(), fmt.Sprintf("the address of the generated reason %s, which is immutable", v.Name())})
				}
			}
		case *ast.SelectorExpr:
			// v.M with a pointer receiver takes &v implicitly (a call or a
			// method value alike).
			if sel, ok := info.Selections[x]; ok && sel.Kind() == types.MethodVal {
				if v := generatedValue(mp, info, x.X); v != nil {
					if sig, ok := sel.Obj().Type().(*types.Signature); ok && sig.Recv() != nil {
						if _, ptr := sig.Recv().Type().(*types.Pointer); ptr {
							out = append(out, raw{x.Pos(), fmt.Sprintf("pointer method %s on the generated reason %s, which is immutable", x.Sel.Name, v.Name())})
						}
					}
				}
			}
		}
		return true
	})
	return out
}

// callee returns the function a call invokes directly (a package function or
// a method), or nil.
func callee(info *types.Info, call *ast.CallExpr) *types.Func {
	var id *ast.Ident
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	case *ast.IndexExpr: // an explicitly instantiated generic function
		return nil
	default:
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

// generatedValue returns the generated vocab.Reason* variable e names, or nil.
func generatedValue(mp modPaths, info *types.Info, e ast.Expr) *types.Var {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	default:
		return nil
	}
	v, ok := info.Uses[id].(*types.Var)
	if !ok || v.IsField() || !mp.isVocab(v.Pkg()) || !strings.HasPrefix(v.Name(), "Reason") {
		return nil
	}
	if v.Parent() != v.Pkg().Scope() { // package-level only
		return nil
	}
	return v
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

// located is a finding with its sort key.
type located struct {
	Finding
	file      string
	line, col int
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
