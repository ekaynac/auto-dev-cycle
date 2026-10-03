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
// ./..., non-test files) and reports every constant expression whose type is
// the reason type, unless it is a generated constant from vocab.gen.go used by
// name. It catches literals, named constants, concatenations and conversions
// in any position: composite literals, assignments, call arguments,
// comparisons, switch cases, map keys; through aliased and dot imports alike.
// A registered value is reported as a bypass, an unregistered one as
// unregistered (docs/design/m0/02 `adc dev vocab check`).
//
// Matching rule: the reason type is any named type called Reason declared in
// a package named vocab, and the generated constants are the constants
// declared in that package's file vocab.gen.go. Matching by package name, not
// import path, lets the tests use a synthetic module.
//
// Not covered: test files (they build malformed results on purpose), values
// converted from non-constant strings (checked at runtime by Check), string
// fields of other types (such as the generated types.Receipt.Reason), and
// TypeScript (no TS code exists yet).
//
// A package that does not load or type-check makes the check fail with an
// error, never pass.
func (reg *Registry) CheckSource(root string) ([]Finding, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir: absRoot,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages under %s: %w", root, err)
	}
	var found []located
	var loadErrs []string
	for _, pkg := range pkgs {
		if skipped(absRoot, pkg) {
			continue
		}
		for _, e := range pkg.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
		if len(pkg.Errors) > 0 || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			name := pkg.Fset.Position(file.Pos()).Filename
			if pkg.Name == "vocab" && filepath.Base(name) == genFile {
				continue
			}
			for _, e := range reasonConstants(pkg.Fset, pkg.TypesInfo, file) {
				found = append(found, reg.finding(absRoot, pkg, e))
			}
		}
	}
	if len(loadErrs) > 0 {
		sort.Strings(loadErrs)
		return nil, fmt.Errorf("packages under %s do not type-check:\n  %s", root, strings.Join(loadErrs, "\n  "))
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

// reasonConstants returns the outermost constant expressions of the reason
// type in file that are not a generated constant used by name.
func reasonConstants(fset *token.FileSet, info *types.Info, file *ast.File) []ast.Expr {
	var cands []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		tv, ok := info.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String || !isReason(tv.Type) {
			return true
		}
		cands = append(cands, e)
		return false // the outermost expression is the finding
	})
	var out []ast.Expr
	for _, e := range cands {
		if !isGenerated(fset, info, e) {
			out = append(out, e)
		}
	}
	return out
}

// isReason matches the named type Reason of a package named vocab.
func isReason(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := n.Obj()
	return obj.Name() == "Reason" && obj.Pkg() != nil && obj.Pkg().Name() == "vocab"
}

// isGenerated: e names (directly, or qualified) a constant declared in the
// vocab package's generated file.
func isGenerated(fset *token.FileSet, info *types.Info, e ast.Expr) bool {
	c := namedConst(info, e)
	if c == nil || c.Pkg() == nil || c.Pkg().Name() != "vocab" || !c.Pos().IsValid() {
		return false
	}
	return filepath.Base(fset.Position(c.Pos()).Filename) == genFile
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

func (reg *Registry) finding(absRoot string, pkg *packages.Package, e ast.Expr) located {
	tv := pkg.TypesInfo.Types[e]
	code := constant.StringVal(tv.Value)
	pos := pkg.Fset.Position(e.Pos())
	rel, err := filepath.Rel(absRoot, pos.Filename)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = pos.Filename
	}
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
	rel = filepath.ToSlash(rel)
	return located{
		Finding: Finding{Pos: fmt.Sprintf("%s:%d:%d", rel, pos.Line, pos.Column), Problem: problem},
		file:    rel, line: pos.Line, col: pos.Column,
	}
}
