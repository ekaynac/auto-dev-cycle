// SPDX-License-Identifier: Apache-2.0

// Package vocab owns the outcome, reason and hold vocabulary
// (docs/design/m0/02). It loads the embedded registries in schemas/vocab,
// validates them against schemas/vocab/registry.json and their cross-file
// rules, and decides whether a result is well-formed. No other package
// decides what a reason means.
package vocab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// Outcome is one of the four outcomes. There is no boolean ok.
type Outcome string

// The four outcomes (schemas/vocab/outcomes.yaml; a test keeps them equal).
const (
	Found       Outcome = "found"
	Empty       Outcome = "empty"
	CouldNotRun Outcome = "could_not_run"
	Failed      Outcome = "failed"
)

// Reason is a reason code. Code names reasons only through the generated
// Reason* constants (vocab.gen.go); `adc dev vocab check` reports any other
// constant expression of this type. A Reason built from a non-constant string
// (e.g. one decoded from a record) is checked at runtime by Registry.Check.
type Reason string

// The contract runtime asks the vocabulary about every reason field of every
// record it decodes (any code ever registered) or encodes (registered and not
// deprecated).
func init() {
	contract.SetReasonCheck(func(code string, writing bool) error {
		reg, err := Load()
		if err != nil {
			return fmt.Errorf("reason vocabulary: %w", err)
		}
		x, ok := reg.Reasons[code]
		if !ok {
			return fmt.Errorf("unregistered reason %q", code)
		}
		if writing && x.Deprecated != nil {
			return fmt.Errorf("reason %q is deprecated since %s; writers may not emit it", code, *x.Deprecated)
		}
		return nil
	})
}

// Registry is the loaded, cross-checked vocabulary.
type Registry struct {
	Outcomes map[Outcome]string      // outcome → description
	Reasons  map[string]types.Reason // code → entry
	Holds    map[string]types.Hold   // hold → entry
}

var (
	once   sync.Once
	loaded *Registry
	errLd  error
)

// Load returns the embedded registry, loaded and checked once.
func Load() (*Registry, error) {
	once.Do(func() {
		paths, err := schemas.VocabPaths()
		if err != nil {
			errLd = err
			return
		}
		loaded, errLd = loadFrom(schemas.FS(), paths)
	})
	return loaded, errLd
}

// loadFrom reads and cross-checks the registries at paths in fsys.
func loadFrom(fsys fs.FS, paths []string) (*Registry, error) {
	r := &Registry{Outcomes: map[Outcome]string{}, Reasons: map[string]types.Reason{}, Holds: map[string]types.Hold{}}
	seen := map[string]string{}
	for _, p := range paths {
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		// Exactly one YAML document: yaml.Unmarshal would read the first and
		// silently ignore any further (possibly malformed) ones.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		var doc any
		if err := dec.Decode(&doc); err != nil { // duplicate keys are an error in yaml.v3
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: a registry file holds exactly one YAML document", p)
		}
		js, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		reg, err := contract.Decode[types.VocabRegistry]("vocab/registry", js)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		kind := fmt.Sprintf("%T", reg.Variant)
		if prev, dup := seen[kind]; dup {
			return nil, fmt.Errorf("%s: a second %s registry (first: %s)", p, kind, prev)
		}
		seen[kind] = p
		switch v := reg.Variant.(type) {
		case *types.OutcomesFile:
			for _, o := range v.Outcomes {
				if _, dup := r.Outcomes[Outcome(o.Outcome)]; dup {
					return nil, fmt.Errorf("%s: outcome %s registered twice", p, o.Outcome)
				}
				r.Outcomes[Outcome(o.Outcome)] = o.Description
			}
		case *types.ReasonsFile:
			for _, x := range v.Reasons {
				if _, dup := r.Reasons[x.Code]; dup {
					return nil, fmt.Errorf("%s: reason %s registered twice", p, x.Code)
				}
				r.Reasons[x.Code] = x
			}
		case *types.HoldsFile:
			for _, h := range v.Holds {
				if _, dup := r.Holds[h.Hold]; dup {
					return nil, fmt.Errorf("%s: hold %s registered twice", p, h.Hold)
				}
				r.Holds[h.Hold] = h
			}
		}
	}
	if len(seen) != 3 {
		return nil, fmt.Errorf("vocabulary needs exactly one outcomes, reasons and holds registry; found %d kinds", len(seen))
	}
	for code, x := range r.Reasons {
		if x.Hold != nil {
			if _, ok := r.Holds[*x.Hold]; !ok {
				return nil, fmt.Errorf("reason %s leads to unregistered hold %s", code, *x.Hold)
			}
		}
	}
	return r, nil
}

// Result is the (outcome, reason, detail) triple every result carries.
type Result struct {
	Outcome Outcome `json:"outcome"`
	Reason  Reason  `json:"reason"`
	Detail  string  `json:"detail"`
}

// MarshalJSON is the result's emission boundary: a result is written only
// if Registry.Check accepts it (registered, non-deprecated reason allowed
// with the outcome, non-empty detail), however it is marshalled.
func (r Result) MarshalJSON() ([]byte, error) {
	reg, err := Load()
	if err != nil {
		return nil, err
	}
	if err := reg.Check(r); err != nil {
		return nil, err
	}
	type plain Result
	return json.Marshal(plain(r))
}

// UnmarshalJSON reads a result: its outcome must be one of the four and its
// reason registered (deprecated codes stay readable).
func (r *Result) UnmarshalJSON(b []byte) error {
	type plain Result
	var p plain
	if err := contract.UnmarshalStrict(b, &p); err != nil {
		return err
	}
	reg, err := Load()
	if err != nil {
		return err
	}
	if _, ok := reg.Outcomes[p.Outcome]; !ok {
		return fmt.Errorf("%w: unknown outcome %q", ErrMalformed, p.Outcome)
	}
	if _, ok := reg.Reasons[string(p.Reason)]; !ok {
		return fmt.Errorf("%w: unregistered reason %q", ErrMalformed, p.Reason)
	}
	*r = Result(p)
	return nil
}

// ErrMalformed marks a result that breaks the vocabulary rules.
var ErrMalformed = errors.New("malformed result")

// Check decides whether a writer may emit r: the reason is registered, not
// deprecated, allowed with this outcome, and every non-empty result carries
// a non-empty detail (nöbetçi lesson 1.1: empty error text read as success).
func (reg *Registry) Check(r Result) error {
	if _, ok := reg.Outcomes[r.Outcome]; !ok {
		return fmt.Errorf("%w: unknown outcome %q", ErrMalformed, r.Outcome)
	}
	x, ok := reg.Reasons[string(r.Reason)]
	if !ok {
		return fmt.Errorf("%w: unregistered reason %q", ErrMalformed, r.Reason)
	}
	if x.Deprecated != nil {
		return fmt.Errorf("%w: reason %q is deprecated since %s; writers may not emit it", ErrMalformed, r.Reason, *x.Deprecated)
	}
	allowed := false
	for _, o := range x.Outcomes {
		allowed = allowed || Outcome(o) == r.Outcome
	}
	if !allowed {
		return fmt.Errorf("%w: reason %q does not accompany outcome %q", ErrMalformed, r.Reason, r.Outcome)
	}
	if r.Outcome != Empty && r.Detail == "" {
		return fmt.Errorf("%w: a %s result needs a non-empty detail", ErrMalformed, r.Outcome)
	}
	return nil
}

// ErrNotEmpty is returned by RequireEmpty for any outcome other than empty.
var ErrNotEmpty = errors.New("result is not empty")

// RequireEmpty is the only way to treat a result as "nothing found". A
// could_not_run is never read as empty: the caller must hold or retry.
func RequireEmpty(r Result) error {
	if r.Outcome == Empty {
		return nil
	}
	return fmt.Errorf("%w: %s/%s: %s", ErrNotEmpty, r.Outcome, r.Reason, r.Detail)
}

// HoldFor returns the hold a reason leads to when it stops work, if any.
func (reg *Registry) HoldFor(reason Reason) (types.Hold, bool) {
	x, ok := reg.Reasons[string(reason)]
	if !ok || x.Hold == nil {
		return types.Hold{}, false
	}
	h, ok := reg.Holds[*x.Hold]
	return h, ok
}
