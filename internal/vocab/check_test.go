// SPDX-License-Identifier: Apache-2.0

package vocab

import (
	"os"
	"path/filepath"
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

func TestGeneratedConstantsCoverTheRegistry(t *testing.T) {
	reg := mustLoad(t)
	reasons := map[Reason]string{ReasonQuotaExhausted: "quota_exhausted", ReasonStaleAttempt: "stale_attempt"}
	for got, want := range reasons {
		if string(got) != want {
			t.Errorf("constant value %q, want %q", got, want)
		}
	}
	if HoldBrokerCreditExhausted != "broker_credit_exhausted" {
		t.Errorf("hold constant value %q", HoldBrokerCreditExhausted)
	}
	if _, ok := reg.Reasons[string(ReasonFallbackScope)]; !ok {
		t.Error("generated constant names an unregistered reason")
	}
}
