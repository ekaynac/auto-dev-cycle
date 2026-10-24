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
	"io"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
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
	return slices.ContainsFunc(f.Imports, func(is *ast.ImportSpec) bool { return is.Path.Value == `"C"` })
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
