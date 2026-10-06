// SPDX-License-Identifier: Apache-2.0

package adapterhost

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const inprocPath = "github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session/inproc"

// inprocImporters are the only packages whose non-test files may import
// package inproc: the session that runs a peer and the fixture harness.
var inprocImporters = []string{"internal/adapterhost/session", "internal/fixtures"}

// An in-process adapter bypasses the PID-namespace lifetime boundary, so no
// production path may make one: only the fixture harness (and tests) import
// package inproc. Every .go file of the module is parsed for its imports.
func TestInprocIsFixtureHarnessOnly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no module root at %s: %v", root, err)
	}
	var importers []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == inprocPath {
				rel, err := filepath.Rel(root, filepath.Dir(path))
				if err != nil {
					return err
				}
				importers = append(importers, filepath.ToSlash(rel))
				if !slices.Contains(inprocImporters, filepath.ToSlash(rel)) {
					t.Errorf("%s imports %s: only the fixture harness may run an in-process adapter", path, inprocPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk saw the importers it allows: it is not vacuous.
	for _, want := range inprocImporters {
		if !slices.Contains(importers, want) {
			t.Errorf("%s does not import %s: the walk missed it", want, inprocPath)
		}
	}
}
