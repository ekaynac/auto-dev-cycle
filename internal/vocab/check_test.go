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

// synthetic writes a module that mirrors the real shapes: the opaque
// gen/go/types ReasonCode with its unchecked ReasonCodeOf, and a package
// vocab with the alias, the parser and generated values.
func synthetic(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/x\n\ngo 1.22\n")
	write(t, root, "gen/go/types/types.go", `package types

type ReasonCode struct{ code string }

func ReasonCodeOf(code string) ReasonCode { return ReasonCode{code: code} }

type Predicate struct{ Reason ReasonCode }
`)
	write(t, root, "vocab/vocab.go", `package vocab

import "example.com/x/gen/go/types"

type Reason = types.ReasonCode

func ParseReason(code string) (Reason, error) { return types.ReasonCodeOf(code), nil }

type Result struct {
	Outcome string
	Reason  Reason
	Detail  string
}
`)
	write(t, root, "vocab/vocab.gen.go", `package vocab

import "example.com/x/gen/go/types"

var (
	ReasonQuotaExhausted = types.ReasonCodeOf("quota_exhausted")
	ReasonStaleAttempt   = types.ReasonCodeOf("stale_attempt")
)

const HoldAwaitingApproval = "awaiting_approval"
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

func TestCheckSourceFindings(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "a/a.go", `package a

import (
	"example.com/x/gen/go/types"
	"example.com/x/vocab"
)

var _ = types.Predicate{Reason: types.ReasonCodeOf("typo_unchecked")}
var _, _ = vocab.ParseReason("stale_attempt")
var _, _ = vocab.ParseReason(code)

const code = "quota_exhausted"

func overwrite() {
	vocab.ReasonStaleAttempt = vocab.ReasonQuotaExhausted
	p := &vocab.ReasonQuotaExhausted
	_ = p
}

func dynamic(s string) (vocab.Reason, error) { return vocab.ParseReason(s) }

func read(r vocab.Reason) bool { return r == vocab.ReasonStaleAttempt }
`)
	want := strings.Join([]string{
		`a/a.go:8:33: types.ReasonCodeOf builds an unchecked reason; use a generated vocab.Reason* value or vocab.ParseReason`,
		`a/a.go:9:12: ParseReason("stale_attempt") with a constant; use the generated vocab.ReasonStaleAttempt value`,
		`a/a.go:10:12: ParseReason("quota_exhausted") with a constant; use the generated vocab.ReasonQuotaExhausted value`,
		`a/a.go:15:2: assignment to the generated reason ReasonStaleAttempt, which is immutable`,
		`a/a.go:16:7: the address of the generated reason ReasonQuotaExhausted, which is immutable`,
	}, "\n")
	if got := findings(t, reg, root); got != want {
		t.Fatalf("findings:\n%s\nwant:\n%s", got, want)
	}
}

// The type does the rest: reasons from string literals, conversions,
// generics or named function types do not compile, so a clean module is
// clean and an attempt is a type-check failure, never a pass.
func TestCheckSourceTypeRefusesStringReasons(t *testing.T) {
	reg := mustLoad(t)
	root := synthetic(t)
	write(t, root, "a/a.go", `package a

import "example.com/x/vocab"

var _ = vocab.Result{Reason: vocab.ReasonStaleAttempt}
`)
	if got := findings(t, reg, root); got != "" {
		t.Fatalf("a clean module has findings:\n%s", got)
	}
	for name, src := range map[string]string{
		"literal":    `var _ = vocab.Result{Reason: "typo"}`,
		"conversion": `var _ = vocab.Reason("typo")`,
		"generic":    `func bad[T ~[]vocab.Reason]() T { return T{"typo"} }`,
		"named func": "type Emit func(vocab.Reason)\n\nfunc send(e Emit) { e(\"typo\") }",
		"append":     `var _ = append([]vocab.Reason{}, "typo")`,
	} {
		t.Run(name, func(t *testing.T) {
			root := synthetic(t)
			write(t, root, "b/b.go", "package b\n\nimport \"example.com/x/vocab\"\n\n"+src+"\n")
			if _, err := reg.CheckSource(root); err == nil || !strings.Contains(err.Error(), "type-check") {
				t.Fatalf("a string reason must fail to type-check, got %v", err)
			}
		})
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
	for name, files := range map[string]map[string]string{
		"type error":                    {"a/a.go": "package a\n\nvar x int = \"s\"\n"},
		"type error in a skipped dir":   {"gen/bad/bad.go": "package bad\n\nvar X int = \"s\"\n"},
		"type error in an imported pkg": {"gen/bad/bad.go": "package bad\n\nvar X int = \"s\"\n", "a/a.go": "package a\n\nimport \"example.com/x/gen/bad\"\n\nvar Y = bad.X\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := synthetic(t)
			for rel, src := range files {
				write(t, root, rel, src)
			}
			if _, err := reg.CheckSource(root); err == nil || !strings.Contains(err.Error(), "type-check") {
				t.Fatalf("a package that does not type-check must fail the check, got %v", err)
			}
		})
	}
}

func TestGoNamesRefusesCollisions(t *testing.T) {
	if _, err := GoNames([]string{"a_b", "a__b"}); err == nil || !strings.Contains(err.Error(), "both give the Go name AB") {
		t.Fatalf("collision not refused: %v", err)
	}
	if _, err := GoNames([]string{"_"}); err == nil {
		t.Fatal("empty Go name not refused")
	}
}

// The generated values are exactly the registry: every reason and hold, no
// more, each named by GoName and carrying its own code.
func TestGeneratedValuesCoverTheRegistry(t *testing.T) {
	reg := mustLoad(t)
	f, err := parser.ParseFile(token.NewFileSet(), "vocab.gen.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if call, isCall := vs.Values[i].(*ast.CallExpr); isCall {
					lit, ok = call.Args[0].(*ast.BasicLit)
				}
				if !ok {
					t.Fatalf("%s: not a literal code", name.Name)
				}
				v, err := strconv.Unquote(lit.Value)
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
		t.Fatalf("generated values differ from the registry:\n got %v\nwant %v", got, want)
	}
}
