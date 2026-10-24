// SPDX-License-Identifier: Apache-2.0

package gosel

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// The go command's selection rules, as accept and the gate's acceptance
// read them (Astra #429 M6): what go test ./... walks and collects by name,
// what it runs by a function's name, and what the gate's platform builds,
// its default architecture level and the toolchain's experiments included,
// in positive and negated constraints (Astra #429 H1).
func TestTheGoCommandsRules(t *testing.T) {
	for p, want := range map[string]bool{"a_test.go": true, "p/a_test.go": true, "p/testdata/a_test.go": false, "vendor/x/a_test.go": false,
		"_x/a_test.go": false, ".x/a_test.go": false, "p/_a_test.go": false, "p/.a_test.go": false, "p/a.go": false} {
		if TestFile(p) != want {
			t.Errorf("%s: collected by name %t", p, !want)
		}
	}
	for p, want := range map[string]bool{"a.go": true, "p/a_test.go": true, "p/testdata/a.go": false, "p/_a.go": false, "p/a.txt": false} {
		if Active(p) != want {
			t.Errorf("%s: active %t", p, !want)
		}
	}
	for name, want := range map[string]bool{"Test": true, "TestX": true, "Test_x": true, "Testx": false, "FuzzX": true, "ExampleX": true, "Examplex": false,
		"BenchmarkX": false, "Tést": false, "helper": false} {
		if RunName(name) != want {
			t.Errorf("%s: run %t", name, !want)
		}
	}
	exp := ""
	for _, tag := range build.Default.ToolTags {
		if strings.HasPrefix(tag, "goexperiment.") {
			exp = tag
		}
	}
	arm, amd := [2]string{"linux", "arm64"}, [2]string{"linux", "amd64"}
	for _, c := range []struct {
		pl         [2]string
		name, cons string
		want       bool
	}{
		{arm, "a_test.go", "", true},
		{arm, "a_amd64_test.go", "", false},
		{arm, "a_windows_test.go", "", false},
		{arm, "a_test.go", "//go:build arm64.v8.0", true},
		{arm, "a_test.go", "//go:build !arm64.v8.0", false},
		{amd, "a_test.go", "//go:build amd64.v1", true},
		{amd, "a_test.go", "//go:build !amd64.v1", false},
		{arm, "a_test.go", "//go:build cgo", false},
		{arm, "a_test.go", "//go:build !cgo", true},
		{arm, "a_test.go", "//go:build integration", false},
		{arm, "a_test.go", "//go:build go1.1", true},
		{arm, "a_test.go", "//go:build go1.999", false},
		{arm, "a_test.go", "//go:build ignore", false},
	} {
		if got := Builds(c.pl, "p/"+c.name, []byte(c.cons+"\n\npackage p\n")); got != c.want {
			t.Errorf("%v %s %q: builds %t", c.pl, c.name, c.cons, got)
		}
	}
	if exp != "" && (!Builds(arm, "p/a_test.go", []byte("//go:build "+exp+"\n\npackage p\n")) || Builds(arm, "p/a_test.go", []byte("//go:build !"+exp+"\n\npackage p\n"))) {
		t.Errorf("the toolchain's experiment %s", exp)
	}
	for src, want := range map[string]bool{"package p\n\nimport \"C\"\n": true, "package p\n\nimport `C`\n": true, "package p\n\nimport \"CC\"\n": false, "package p\n\nimport (\n\t\"os\"\n\t\"C\"\n)\n": true, "package p\n\nimport \"os\"\n": false} {
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ImportsOnly)
		if err != nil || Cgo(f) != want {
			t.Errorf("%q: cgo %t, %v", src, !want, err)
		}
	}
	if !Walked("p/build/x.py", SkipDir) || Walked("p/build/x.py", func(d string) bool { return d == "build" }) {
		t.Error("Walked's skip")
	}
	// Astra #429 r2 M4: the module metadata and receiver rules, accept's.
	named, module, ok := Modules([]byte("module m\n\ngo 1.16\n\nrequire lib v1.0.0\n\nreplace foo => ./third/foo\n"))
	if !ok || module != "m" || !named["lib"] || !named["foo"] || !named["m"] {
		t.Errorf("Modules: %v %q %t", named, module, ok)
	}
	if _, _, ok := Modules([]byte("not a go.mod\n")); ok {
		t.Error("Modules of no go.mod")
	}
	for p, want := range map[string]bool{"fmt": true, "math/rand/v2": true, "foo/bar": false, "foo": false, "lib/x": false, "m/inv": false, "example.com/x": false, "food/x": true} {
		if Std(p, named) != want {
			t.Errorf("Std(%s): %t", p, !want)
		}
	}
	if Std("fmt", map[string]bool{"": true}) || !Std("fmt", nil) || Std("a.b/c", nil) {
		t.Error("Std's every-path and nil names")
	}
	for _, p := range []string{"/x", "/", "", "a//b", "a/", "./x", "x/../y"} { // no import path: none, and each returns (Opus #429 r7 B1)
		if Std(p, named) || Std(p, nil) {
			t.Errorf("Std(%q)", p)
		}
	}
	for src, want := range map[string]string{"func (s *S[P]) M() {}": "S", "func (S) M() {}": "S", "func (s *S[K, V]) M() {}": "S", "func F() {}": "",
		"func (s (*S)) M() {}": "S", "func (s *(S)) M() {}": "S", "func (s ((S))) M() {}": "S", "func (s (*S[P])) M() {}": "S"} {
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", "package p\n\n"+src+"\n", 0)
		if err != nil || Recv(f.Decls[0].(*ast.FuncDecl)) != want {
			t.Errorf("Recv(%s): %v", src, err)
		}
	}
	if !slices.Equal(ReleaseTags, build.Default.ReleaseTags) {
		t.Error("the release tags")
	}
}
