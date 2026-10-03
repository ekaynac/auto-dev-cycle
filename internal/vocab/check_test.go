// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// synthetic writes a module whose package vocab mirrors the real one: the
// check matches the type Reason of any package named vocab, and the constants
// of its vocab.gen.go.
func synthetic(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/x\n\ngo 1.22\n")
	write(t, root, "vocab/vocab.go", `package vocab

type Reason string

type Result struct {
	Outcome string
	Reason  Reason
	Detail  string
}
`)
	write(t, root, "vocab/vocab.gen.go", `package vocab

const (
	ReasonQuotaExhausted Reason = "quota_exhausted"
	ReasonStaleAttempt   Reason = "stale_attempt"
)
`)
	return root
}

func findings(t *testing.T, reg *Registry, root string) string {
	t.Helper()
	got, err := reg.CheckSource(root)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range got {
		lines = append(lines, f.Pos+": "+f.Problem)
	}
	return strings.Join(lines, "\n")
}

func TestCheckSourceFindsBypasses(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "vocab/manual.go", `package vocab

const ReasonHandWritten Reason = "stale_attempt"
`)
	write(t, root, "a/a.go", `package a

import "example.com/x/vocab"

const myConst = "typo_const"

var _ = vocab.Result{Outcome: "failed", Reason: "quota_exhausted", Detail: "d"}
var _ = vocab.Result{"failed", "typo_positional", "d"}
var _ = vocab.Result{Reason: myConst}

func assign(r *vocab.Result) { r.Reason = "stale_attempt" }

func refused(r vocab.Reason) {}

func call() { refused("typo_helper") }

var _ vocab.Reason = "quota_" + "exhausted"
var _ = vocab.Reason("typo_conversion")

func compare(r vocab.Reason) bool { return r == "quota_exhausted" }

var _ = map[vocab.Reason]int{"typo_key": 1}
var _ = vocab.ReasonHandWritten
`)
	write(t, root, "b/alias.go", `package b

import v "example.com/x/vocab"

var _ = v.Result{Reason: "typo_alias"}
`)
	write(t, root, "c/dot.go", `package c

import . "example.com/x/vocab"

var _ = Result{Reason: "typo_dot"}
`)
	// Clean: generated constants by name (qualified, parenthesised, dot
	// imported), a non-constant conversion, a test file, generated and
	// fixture trees.
	write(t, root, "d/clean.go", `package d

import (
	"os"

	"example.com/x/vocab"
	. "example.com/x/vocab"
)

var _ = vocab.Result{Reason: vocab.ReasonStaleAttempt}
var _ = vocab.Result{Reason: (vocab.ReasonQuotaExhausted)}
var _ = Result{Reason: ReasonQuotaExhausted}
var _ = vocab.Reason(os.Getenv("R"))
`)
	write(t, root, "d/clean_test.go", `package d

import "example.com/x/vocab"

var _ = vocab.Result{Reason: "deliberately_bad"}
`)
	write(t, root, "gen/g/g.go", `package g

import "example.com/x/vocab"

var _ = vocab.Result{Reason: "in_generated_code"}
`)
	write(t, root, "fixtures/f/f.go", `package f

import "example.com/x/vocab"

var _ = vocab.Result{Reason: "in_a_fixture"}
`)

	want := strings.Join([]string{
		`a/a.go:7:49: reason "quota_exhausted" as a raw literal; use vocab.ReasonQuotaExhausted`,
		`a/a.go:8:32: unregistered reason "typo_positional" (raw literal)`,
		`a/a.go:9:30: unregistered reason "typo_const" (non-generated constant myConst)`,
		`a/a.go:11:43: reason "stale_attempt" as a raw literal; use vocab.ReasonStaleAttempt`,
		`a/a.go:15:23: unregistered reason "typo_helper" (raw literal)`,
		`a/a.go:17:22: reason "quota_exhausted" as a constant expression; use vocab.ReasonQuotaExhausted`,
		`a/a.go:18:9: unregistered reason "typo_conversion" (conversion)`,
		`a/a.go:20:49: reason "quota_exhausted" as a raw literal; use vocab.ReasonQuotaExhausted`,
		`a/a.go:22:30: unregistered reason "typo_key" (raw literal)`,
		`a/a.go:23:9: reason "stale_attempt" as a non-generated constant ReasonHandWritten; use vocab.ReasonStaleAttempt`,
		`b/alias.go:5:26: unregistered reason "typo_alias" (raw literal)`,
		`c/dot.go:5:24: unregistered reason "typo_dot" (raw literal)`,
		`vocab/manual.go:3:34: reason "stale_attempt" as a raw literal; use vocab.ReasonStaleAttempt`,
	}, "\n")
	if got := findings(t, reg, root); got != want {
		t.Fatalf("findings:\n%s\nwant:\n%s", got, want)
	}
}

func TestCheckSourceCleanModule(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "a/a.go", `package a

import "example.com/x/vocab"

var _ = vocab.Result{Reason: vocab.ReasonStaleAttempt}
`)
	if got := findings(t, reg, root); got != "" {
		t.Fatalf("unexpected findings:\n%s", got)
	}
}

func TestCheckSourceRefusesUnloadableCode(t *testing.T) {
	reg := mustLoad(t)
	t.Run("no module", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "bad.go", "package")
		if _, err := reg.CheckSource(root); err == nil {
			t.Fatal("an unloadable tree must make the check could_not_run, not pass")
		}
	})
	t.Run("type error in a skipped directory", func(t *testing.T) {
		root := synthetic(t)
		write(t, root, "gen/bad/bad.go", "package bad\n\nvar X int = \"s\"\n")
		if _, err := reg.CheckSource(root); err == nil || !strings.Contains(err.Error(), "type-check") {
			t.Fatalf("a skipped package that does not type-check must fail the check, got %v", err)
		}
	})
	t.Run("type error in an imported package", func(t *testing.T) {
		root := synthetic(t)
		write(t, root, "gen/bad/bad.go", "package bad\n\nvar X int = \"s\"\n")
		write(t, root, "a/a.go", "package a\n\nimport \"example.com/x/gen/bad\"\n\nvar Y = bad.X\n")
		if _, err := reg.CheckSource(root); err == nil || !strings.Contains(err.Error(), "type-check") {
			t.Fatalf("an imported package that does not type-check must fail the check, got %v", err)
		}
	})
	t.Run("type error", func(t *testing.T) {
		root := synthetic(t)
		write(t, root, "a/a.go", "package a\n\nvar x int = \"s\"\n")
		if _, err := reg.CheckSource(root); err == nil || !strings.Contains(err.Error(), "type-check") {
			t.Fatalf("a package that does not type-check must fail the check, got %v", err)
		}
	})
}

func TestGoNamesRefusesCollisions(t *testing.T) {
	if _, err := GoNames([]string{"a_b", "a__b"}); err == nil || !strings.Contains(err.Error(), "both give the Go name AB") {
		t.Fatalf("collision not refused: %v", err)
	}
	if _, err := GoNames([]string{"_"}); err == nil {
		t.Fatal("empty Go name not refused")
	}
	got, err := GoNames([]string{"quota_exhausted", "stale_attempt"})
	if err != nil || got["quota_exhausted"] != "QuotaExhausted" {
		t.Fatalf("GoNames = %v, %v", got, err)
	}
}

// The generated constants are exactly the registry: every reason and hold,
// no more, each named by GoName and carrying its own code.
func TestGeneratedConstantsCoverTheRegistry(t *testing.T) {
	reg := mustLoad(t)
	f, err := parser.ParseFile(token.NewFileSet(), genFile, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				v, err := strconv.Unquote(vs.Values[i].(*ast.BasicLit).Value)
				if err != nil {
					t.Fatal(err)
				}
				got[name.Name] = v
			}
		}
	}
	want := map[string]string{}
	for code := range reg.Reasons {
		want["Reason"+GoName(code)] = code
	}
	for hold := range reg.Holds {
		want["Hold"+GoName(hold)] = hold
	}
	if !reflect.DeepEqual(got, want) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("missing or wrong: %s = %q (generated %q)", k, v, got[k])
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("extra generated constant %s", k)
			}
		}
	}
}

// Astra #24: generics instantiated with a reason type are reported, since
// their reasons come from type-parameter literals.
func TestCheckSourceReportsReasonGenerics(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "g/g.go", `package g

import "example.com/x/vocab"

func bad[T ~string]() T { return "typo_generic" }

type Box[T ~string] struct{}

func (Box[T]) Reason() T { return "typo_method" }

var _ = vocab.Result{Reason: bad[vocab.Reason]()}
var _ = Box[vocab.Reason]{}.Reason
var _ = bad[string]()
`)
	got := findings(t, reg, root)
	for _, want := range []string{"g/g.go:11:30: generic bad instantiated with a reason type", "g/g.go:12:9: generic Box instantiated with a reason type"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n")+1 != 2 {
		t.Errorf("want exactly two findings (bad[string] is not a reason), got:\n%s", got)
	}
}

// Astra #24: the reason fields of the generated contract records
// (types.ReasonCode) are reason types too; a converted generated constant is
// the constant.
func TestCheckSourceCoversContractReasonFields(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "types/types.go", `package types

type ReasonCode = string

type Predicate struct {
	Reason ReasonCode
}
`)
	write(t, root, "r/r.go", `package r

import (
	"example.com/x/types"
	"example.com/x/vocab"
)

var _ = types.Predicate{Reason: "typo_field"}
var _ = types.Predicate{Reason: string(vocab.ReasonStaleAttempt)}
var _ = types.Predicate{Reason: types.ReasonCode(vocab.ReasonStaleAttempt)}
`)
	want := `r/r.go:8:33: unregistered reason "typo_field" (raw literal)`
	if got := findings(t, reg, root); got != want {
		t.Fatalf("findings:\n%s\nwant:\n%s", got, want)
	}
}

// Astra #24: a deprecated code may not be written, even through its
// generated constant.
func TestCheckSourceReportsDeprecatedConstants(t *testing.T) {
	reg := mustLoad(t)
	dep := &Registry{Outcomes: reg.Outcomes, Holds: reg.Holds, Reasons: map[string]types.Reason{}}
	for k, v := range reg.Reasons {
		dep.Reasons[k] = v
	}
	x := dep.Reasons["stale_attempt"]
	since := "0.2.0"
	x.Deprecated = &since
	dep.Reasons["stale_attempt"] = x
	root := synthetic(t)
	write(t, root, "a/a.go", `package a

import "example.com/x/vocab"

var _ = vocab.Result{Reason: vocab.ReasonStaleAttempt}
`)
	want := `a/a.go:5:30: deprecated reason "stale_attempt" (since 0.2.0); writers may not emit it`
	if got := findings(t, dep, root); got != want {
		t.Fatalf("findings:\n%s\nwant:\n%s", got, want)
	}
}
