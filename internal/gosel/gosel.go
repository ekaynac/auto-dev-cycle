// SPDX-License-Identifier: Apache-2.0

// Package gosel holds the go command's selection rules that more than one
// controller check reads: which directories go test ./... walks, which
// files it collects as tests, which functions it runs, and which files the
// gate's platform builds (Astra #429 M6: one copy, accept's, shared with
// the gate's acceptance overlay). Each is pure: a path, a name or a file's
// bytes in, a verdict out.
package gosel

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/types"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// Documentation is the package name go/build ignores a file of.
const Documentation = "documentation"

// SkipDir: go test ./... never walks a directory named d.
func SkipDir(d string) bool {
	return d == "vendor" || d == "testdata" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".")
}

// Walked: no directory of p is one skip names (the runner never walks it;
// SkipDir go's, accept's Python guard its own: Opus #429 L1).
func Walked(p string, skip func(dir string) bool) bool {
	d := path.Dir(p)
	return d == "." || !slices.ContainsFunc(strings.Split(d, "/"), skip)
}

// Active: go may build the file at p, by its path alone: a .go file not
// starting with _ or ., in a directory go test ./... walks.
func Active(p string) bool {
	b := path.Base(p)
	return strings.HasSuffix(b, ".go") && b[0] != '_' && b[0] != '.' && Walked(p, SkipDir)
}

// Cgo: f imports "C". The gate builds with cgo off (the image's
// CGO_ENABLED=0): go ignores such a file, and fails the package of such a
// test file ("use of cgo in test not supported"; Opus #429 r3 N1).
func Cgo(f *ast.File) bool {
	return slices.ContainsFunc(f.Imports, func(is *ast.ImportSpec) bool {
		p, err := strconv.Unquote(is.Path.Value) // "C" or `C` (Opus #429 r4 B1)
		return err == nil && p == "C"
	})
}

// TestFile: p may be a collected Go test file, by its name alone.
func TestFile(p string) bool { return strings.HasSuffix(path.Base(p), "_test.go") && Active(p) }

// RunName: name is a function go test runs (TestXxx, FuzzXxx, ExampleXxx;
// Xxx not starting with a lower-case letter).
func RunName(name string) bool {
	for _, pre := range []string{"Test", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, pre); ok {
			r, _ := utf8.DecodeRuneInString(rest)
			return rest == "" || !unicode.IsLower(r)
		}
	}
	return false
}

// ReleaseTags are the pinned Go's release tags, which the gate runs too:
// this toolchain's (they agree while both are the same Go release).
var ReleaseTags = build.Default.ReleaseTags

// Builds: the gate builds p (src) on pl ({GOOS, GOARCH}), by go/build's
// rules: its name's _GOOS/_GOARCH and its build constraints, positive and
// negated, against pl, this toolchain's experiments and pl's default
// architecture level (amd64.v1, arm64.v8.0); no cgo, no other tag.
func Builds(pl [2]string, p string, src []byte) bool {
	c := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: ReleaseTags,
		OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
	c.ToolTags = append(slices.DeleteFunc(slices.Clone(build.Default.ToolTags), func(t string) bool { return !strings.HasPrefix(t, "goexperiment.") }),
		map[string][]string{"amd64": {"amd64.v1"}, "arm64": {"arm64.v8.0"}}[pl[1]]...) // the toolchain's experiments, pl's default level
	ok, err := c.MatchFile(path.Dir(p), path.Base(p))
	return ok && err == nil
}

// Modules are the module paths a go.mod names (its module, its
// requirements', its replacements' old paths), read strictly
// (modfile.Parse: ParseLax drops replace), and its own module path; ok is
// false when it does not parse. Each caller keeps its own failure policy.
func Modules(gomod []byte) (named map[string]bool, module string, ok bool) {
	f, err := modfile.Parse("go.mod", gomod, nil)
	if err != nil {
		return nil, "", false
	}
	named = map[string]bool{}
	if f.Module != nil {
		module, named[f.Module.Mod.Path] = f.Module.Mod.Path, true
	}
	for _, q := range f.Require {
		named[q.Mod.Path] = true
	}
	for _, q := range f.Replace {
		named[q.Old.Path] = true
	}
	return named, module, true
}

// Std: p is the standard library's by module metadata alone (accept's
// rule, Astra #276 r6): its first element has no dot, and no module path
// named names is p or a prefix of it (named[""] names every path). The go
// command resolves such a path from its GOROOT only: it is std or it does
// not build. A path that is no valid import path (module.CheckImportPath:
// "", "/x", "a//b", "a/") is none (Opus #429 r7 B1).
func Std(p string, named map[string]bool) bool {
	if first, _, _ := strings.Cut(p, "/"); module.CheckImportPath(p) != nil || strings.Contains(first, ".") || named[""] {
		return false
	}
	// Ends: each step drops p's last element, and a valid import path is
	// relative and clean; "." and "/" end it too (belt and braces).
	for q := p; q != "." && q != "/"; q = path.Dir(q) {
		if named[q] {
			return false
		} else if !strings.Contains(q, "/") {
			return true
		}
	}
	return false
}

// Recv is a method's receiver type name, without parentheses, * or type
// parameters ((*S), *(S), S[P]: S; Opus #429 r7 M1); "" for a function.
func Recv(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := ast.Unparen(fn.Recv.List[0].Type)
	if star, ok := t.(*ast.StarExpr); ok {
		t = ast.Unparen(star.X)
	}
	return strings.TrimLeft(strings.SplitN(types.ExprString(t), "[", 2)[0], "*")
}
