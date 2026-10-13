// SPDX-License-Identifier: Apache-2.0

package testguard

import (
	"go/ast"
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

const module = "github.com/ekaynac/auto-dev-cycle/"

// importAllowed reports whether a non-test file in dir (from the repository
// root) may import path: nothing under internal/testguard, except the fake
// clock, the owed-answer fake, the fake effect target, the fake sandbox,
// the fake executor registry and the fake forge from the fixture harness
// (whose installed run is a release binary).
func importAllowed(dir, path string) bool {
	if !inTestguard(strings.TrimPrefix(path, module)) || inTestguard(dir) {
		return true
	}
	allowed := []string{"clock", "owedanswers", "effecttarget", "sandbox", "executors", "forge"}
	return slices.Contains(allowed, strings.TrimPrefix(path, module+"internal/testguard/fakes/")) && dir == "internal/fixtures"
}

// inTestguard reports whether p (from the repository root) is
// internal/testguard or below it.
func inTestguard(p string) bool {
	return p == "internal/testguard" || strings.HasPrefix(p, "internal/testguard/")
}

// violations lists what one file breaks: an import importAllowed refuses
// (non-test files); outside the guard package itself, a Setenv or Unsetenv
// of HOME or an XDG_ directory, or of a key that is not a string literal,
// and a string literal starting "HOME=" or "XDG_" (a child's environment).
// Only the guard sets a home, after checking its directory.
func violations(rel string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	var out []string
	if !strings.HasSuffix(rel, "_test.go") {
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); !importAllowed(dir, p) {
				out = append(out, rel+" imports "+p)
			}
		}
	}
	if dir == "internal/testguard" { // the guard package itself, not the fakes below it
		return out, nil
	}
	homeKey := func(v string) bool { return v == "HOME" || strings.HasPrefix(v, "XDG_") }
	// The product sandbox (P9) gives Podman and the controller's git a home
	// of their own, inside its private root or none at all; its tests are
	// still held to the rule.
	productChild := dir == "internal/sandbox" && !strings.HasSuffix(rel, "_test.go")
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit: // a child's environment: "HOME=…", "XDG_…"
			if v, err := strconv.Unquote(n.Value); !productChild && n.Kind == token.STRING && err == nil && (strings.HasPrefix(v, "HOME=") || strings.HasPrefix(v, "XDG_")) {
				out = append(out, rel+" spells "+strings.SplitN(v, "=", 2)[0]+" for a child: use testguard.HomeEnv")
			}
		case *ast.CallExpr:
			var name string
			switch fun := n.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident: // a dot import
				name = fun.Name
			}
			if name != "Setenv" && name != "Unsetenv" || len(n.Args) == 0 {
				return true
			}
			lit, ok := n.Args[0].(*ast.BasicLit)
			v, err := "", error(nil)
			if ok && lit.Kind == token.STRING {
				v, err = strconv.Unquote(lit.Value)
			}
			switch {
			case !ok || lit.Kind != token.STRING || err != nil:
				out = append(out, rel+" calls "+name+" with a key that is not a string literal")
			case homeKey(v):
				out = append(out, rel+" calls "+name+"("+v+"): use testguard.Home")
			}
			return false // its key literal is judged here, not as a child's environment
		}
		return true
	})
	return out, nil
}

func TestBoundary(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no module root at %s: %v", root, err)
	}
	files := 0
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
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		v, err := violations(filepath.ToSlash(rel), src)
		for _, e := range v {
			t.Error(e)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("walked only %d Go files under %s", files, root)
	}
}

// Red-proofs: each rule refuses its case, and lets the allowed ones by.
func TestBoundaryRules(t *testing.T) {
	imp := func(p string) string { return "package x\nimport _ \"" + module + p + "\"\n" }
	for _, c := range []struct {
		rel, src string
		bad      bool
	}{
		{"internal/cli/x_test.go", "package x\nfunc f(t T) { t.Setenv(\"HOME\", d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f() { os.Setenv(\"XDG_CONFIG_HOME\", d) }\n", true},
		{"cmd/adc/x.go", "package x\nfunc f() { os.Setenv(\"HOME\", d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f(t T) { t.Setenv(\"TMPDIR\", d) }\n", false},
		{"internal/testguard/x.go", "package x\nfunc f(t T) { t.Setenv(\"HOME\", d) }\n", false},
		{"internal/testguard/fakes/forge/x.go", "package x\nfunc f(t T) { t.Setenv(\"HOME\", d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f() { os.Unsetenv(\"HOME\") }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f() { os.Unsetenv(\"XDG_DATA_HOME\") }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f(t T) { t.Setenv(key, d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f(t T) { t.Setenv(\"HO\"+\"ME\", d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f() { Setenv(\"HOME\", d) }\n", true},
		{"internal/cli/x_test.go", "package x\nfunc f() { os.Unsetenv(\"TMPDIR\") }\n", false},
		{"cmd/adc/x_test.go", "package x\nvar env = []string{\"HOME=\" + dir}\n", true},
		{"cmd/adc/x_test.go", "package x\nvar env = []string{`XDG_CONFIG_HOME=/x`}\n", true},
		{"cmd/adc/x_test.go", "package x\nvar env = []string{\"PATH=\" + p, \"MY_HOME=x\"}\n", false},
		{"internal/daemon/x.go", imp("internal/testguard"), true},
		{"internal/daemon/x.go", imp("internal/testguard/fakes/executors"), true},
		{"internal/daemon/x.go", imp("internal/testguard/fakes"), true},
		{"internal/daemon/x.go", imp("internal/testguard/fakes/clock"), true},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/executors"), false},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/clock"), false},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/owedanswers"), false},
		{"internal/daemon/x.go", imp("internal/testguard/fakes/owedanswers"), true},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/effecttarget"), false},
		{"internal/daemon/x.go", imp("internal/testguard/fakes/effecttarget"), true},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/effecttargetx"), true},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/sandbox"), false},
		{"internal/daemon/x.go", imp("internal/testguard/fakes/sandbox"), true},
		{"internal/fixtures/x.go", imp("internal/testguard/fakes/sandboxes"), true},
		{"internal/daemon/x_test.go", imp("internal/testguard"), false},
		{"internal/daemon/x.go", imp("internal/testguardian"), false},
	} {
		v, err := violations(c.rel, []byte(c.src))
		if err != nil {
			t.Fatalf("%s: %v", c.rel, err)
		}
		if (len(v) > 0) != c.bad {
			t.Errorf("%s %q: violations %v, want refused=%v", c.rel, c.src, v, c.bad)
		}
	}
}
