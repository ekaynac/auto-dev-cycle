// SPDX-License-Identifier: Apache-2.0

package accept

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// platforms are the gate's (linux, amd64 and arm64): a Go test file must
// stay collected on each.
var platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}}

// goSkipDir: go test ./... never walks dir (vendor, testdata, a leading _
// or .).
func goSkipDir(d string) bool {
	return d == "vendor" || d == "testdata" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".")
}

// goByName: p may be a collected Go test file, by its name alone.
func goByName(p string) bool {
	b := path.Base(p)
	return strings.HasSuffix(b, "_test.go") && b[0] != '_' && b[0] != '.' && walked(p, goSkipDir)
}

// goOn is the gate platforms whose go test collects p, holding src: Go's
// own file rules (go/build: a leading _ or ., GOOS and GOARCH suffixes,
// //go:build and // +build constraints with no tags set), in a directory
// ./... walks. A header Go cannot read fails the build: collected.
func goOn(p string, src []byte) []string {
	if !goByName(p) {
		return nil
	}
	var on []string
	for _, pl := range platforms {
		ctx := build.Context{GOOS: pl[0], GOARCH: pl[1], Compiler: "gc", ReleaseTags: build.Default.ReleaseTags,
			OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }}
		if ok, err := ctx.MatchFile(path.Dir(p), path.Base(p)); ok || err != nil {
			on = append(on, pl[0]+"/"+pl[1])
		}
	}
	return on
}

// goTest: name is a function go test runs (TestXxx, FuzzXxx, ExampleXxx).
func goTest(name string) bool {
	for _, pre := range []string{"Test", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, pre); ok {
			r, _ := utf8.DecodeRuneInString(rest)
			return rest == "" || !unicode.IsLower(r)
		}
	}
	return false
}

var (
	goSkips  = map[string]bool{"Skip": true, "Skipf": true, "SkipNow": true}
	goFails  = map[string]bool{"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true, "Fail": true, "FailNow": true}
	goAssert = []string{"assert", "Assert", "require", "Require", "check", "Check", "expect", "Expect"}
	// forged are the spellings of 0x16 (test2json's marker) in Go source.
	forged = []string{"\x16", `\x16`, `\026`, `\u0016`, `\U00000016`}
)

// goCount tallies a Go declaration's calls.
func goCount(n ast.Node) (c struct{ asserts, skips, exits, forged int }) {
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		recv, name, sel := "", "", false
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name, sel = f.Sel.Name, true
			if x, ok := f.X.(*ast.Ident); ok {
				recv = x.Name
			}
		}
		switch {
		case recv == "os" && name == "Exit":
			c.exits++
		case sel && name == "Testing":
			c.forged++
		case sel && goSkips[name]:
			c.skips++
		case sel && goFails[name], recv == "assert", recv == "require", slices.ContainsFunc(goAssert, func(p string) bool { return strings.HasPrefix(name, p) }):
			c.asserts++
		}
		return true
	})
	return c
}

// goFacts reads a Go file's tests (in a _test.go file), the skips outside
// them, its TestMain, its exiting init functions and its exits.
func goFacts(p string, src []byte, fa *facts) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	test, exits := strings.HasSuffix(p, "_test.go"), 0
	for _, d := range f.Decls {
		c := goCount(d)
		fa.forged, exits = fa.forged+c.forged, exits+c.exits
		name := ""
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
			name = fn.Name.Name
		}
		switch {
		case test && name == "TestMain":
			fa.warn["TestMain"] = text(d)
		case test && goTest(name):
			fa.tests[path.Dir(p)+":"+name] = &stat{asserts: c.asserts + output(f, d.(*ast.FuncDecl)), skips: c.skips}
			continue
		case name == "init" && c.exits > 0:
			fa.warn["an init calling os.Exit"] += text(d)
		}
		fa.ambient += c.skips
	}
	for _, m := range forged {
		fa.forged += bytes.Count(src, []byte(m))
	}
	if exits > 0 {
		fa.warn["os.Exit calls"] = strconv.Itoa(exits)
	}
	return nil
}

// output: an example's output comment, its assertion.
func output(f *ast.File, fn *ast.FuncDecl) int {
	for _, cg := range f.Comments {
		t := strings.ToLower(strings.TrimSpace(cg.Text()))
		if cg.Pos() > fn.Body.Lbrace && cg.End() < fn.Body.Rbrace && (strings.HasPrefix(t, "output:") || strings.HasPrefix(t, "unordered output:")) {
			return 1
		}
	}
	return 0
}
