// SPDX-License-Identifier: Apache-2.0

// Package fixtures runs the M0 contract fixtures (docs/design/m0/11): each
// is a registered runner that drives the real components in a scratch
// state directory and returns one (outcome, reason) per step; Run compares
// that sequence with the fixture's golden. Fakes stand in only for external
// dependencies.
package fixtures

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	goldens "github.com/ekaynac/auto-dev-cycle/fixtures/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Step is one step's result: an outcome and a reason, as goldens name them.
type Step struct {
	Outcome vocab.Outcome `json:"outcome"`
	Reason  string        `json:"reason"`
}

func (s Step) String() string { return string(s.Outcome) + "/" + s.Reason }

// Golden is a decoded fixture golden (schemas/testing/contract-fixture).
type Golden struct {
	Name      string `json:"name"`
	Invariant string `json:"invariant"`
	Expected  []Step `json:"expected"`
	Mutation  string `json:"mutation"`
}

// runner drives one fixture in dir and returns its steps and the
// assertions about committed state it found violated (a fixture failure);
// an error means the fixture could not run.
type runner func(ctx context.Context, dir string) (steps []Step, violations []string, err error)

// runners are the registered fixtures, by name.
var runners = map[string]runner{
	"idempotency-key-reuse": idempotencyKeyReuse,
	"stale-expected-state":  staleExpectedState,
}

// Goldens loads and validates every golden, checking that each has a
// runner, names a known mutation, and is named after its file, and that
// every runner has a golden.
func Goldens() ([]Golden, error) { return goldensFrom(goldens.Goldens) }

func goldensFrom(fsys fs.FS) ([]Golden, error) {
	files, err := fs.Glob(fsys, "m0/*.json")
	if err != nil {
		return nil, err
	}
	reg, err := vocab.Load()
	if err != nil {
		return nil, err
	}
	var out []Golden
	seen := map[string]bool{}
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		if err := contract.Validate("testing/contract-fixture", b); err != nil {
			return nil, fmt.Errorf("fixtures: %s: %w", f, err)
		}
		var g Golden
		if err := contract.UnmarshalStrict(b, &g); err != nil {
			return nil, fmt.Errorf("fixtures: %s: %w", f, err)
		}
		switch {
		case g.Name+".json" != path.Base(f):
			return nil, fmt.Errorf("fixtures: %s names fixture %s", f, g.Name)
		case runners[g.Name] == nil:
			return nil, fmt.Errorf("fixtures: %s has no runner", g.Name)
		case !slices.Contains(mutation.Known, g.Mutation):
			return nil, fmt.Errorf("fixtures: %s names unknown mutation %s", g.Name, g.Mutation)
		}
		// Each expected step is a result a writer may emit: the reason is
		// current and allowed with that outcome (the detail is not golden).
		for i, st := range g.Expected {
			reason, err := vocab.ParseReason(st.Reason)
			if err == nil {
				err = reg.Check(vocab.Result{Outcome: st.Outcome, Reason: reason, Detail: "golden"})
			}
			if err != nil {
				return nil, fmt.Errorf("fixtures: %s step %d: %w", g.Name, i+1, err)
			}
		}
		seen[g.Name] = true
		out = append(out, g)
	}
	for _, name := range slices.Sorted(maps.Keys(runners)) {
		if !seen[name] {
			return nil, fmt.Errorf("fixtures: runner %s has no golden", name)
		}
	}
	return out, nil
}

// Result is one fixture's run.
type Result struct {
	Golden     Golden
	Got        []Step
	Violations []string
	Pass       bool
	Detail     string
}

// Run runs g's fixture in a fresh scratch directory and compares its steps
// with the golden.
func Run(ctx context.Context, g Golden) (Result, error) {
	dir, err := os.MkdirTemp("", "adc-fixture-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	got, violations, err := runners[g.Name](ctx, dir)
	if err != nil {
		return Result{}, fmt.Errorf("fixtures: %s: %w", g.Name, err)
	}
	r := Result{Golden: g, Got: got, Violations: violations, Pass: slices.Equal(got, g.Expected) && len(violations) == 0}
	var detail []string
	if !slices.Equal(got, g.Expected) {
		detail = append(detail, fmt.Sprintf("got %s, want %s", steps(got), steps(g.Expected)))
	}
	r.Detail = strings.Join(append(detail, violations...), "; ")
	return r, nil
}

func steps(ss []Step) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.String())
	}
	return "[" + strings.Join(out, " ") + "]"
}

// commandStep maps a command's outcome to its step: applied (or replayed) is
// empty/completed_clean; a refusal the writer names is failed/<reason>. Any
// other error means the fixture could not run.
func commandStep(err error) (Step, error) {
	switch {
	case err == nil:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	case errors.Is(err, eventstore.ErrIdempotencyKeyReused):
		return Step{vocab.Failed, vocab.ReasonIdempotencyKeyReused.String()}, nil
	case errors.Is(err, eventstore.ErrStaleExpectedState):
		return Step{vocab.Failed, vocab.ReasonStaleExpectedState.String()}, nil
	}
	return Step{}, err
}
