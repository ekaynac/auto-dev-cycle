// SPDX-License-Identifier: Apache-2.0

// Package graph is the artifact graph's controller side (docs/design/m0/13):
// computation keys and the build outcome table. In M0 it runs under the
// contract fixtures only; adcd does not instantiate task graphs before M1.
package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// ErrKey: a key cannot be computed from what was given. Every refusal wraps it.
var ErrKey = errors.New("graph: no computation key")

// Resolved is what one declared input resolved to (13 "Evaluation"): the
// selected result of a result input, or, for an answer input, the approval
// that answers the owed input and the revision it binds (04 "Owed inputs").
type Resolved struct {
	Result   types.ResultID   `json:"result,omitempty"`
	Approval types.ApprovalID `json:"approval,omitempty"`
	Revision int64            `json:"revision,omitempty"`
}

// keyed is the document a key hashes. Inputs are sorted by name: names are
// unique within a node, so the order a template declares them in is not
// part of the key, and two instances of one kind with the same inputs
// share it (13 [G10], duplicate demands).
type keyed struct {
	Kind              types.NodeKind `json:"kind"`
	DefinitionVersion string         `json:"definition_version"`
	Params            string         `json:"params"`
	Inputs            []keyedInput   `json:"inputs"`
	Generation        int64          `json:"generation"`
}

type keyedInput struct {
	Name string `json:"name"`
	Resolved
}

// ParamsDigest is the digest of a node's params: they are read by the
// adapter wire's strict reader first (contract.ReadStrict: no duplicate
// key, integer spellings only, integers within ±(2^53−1), bounded depth),
// then digested through the event store's canonicaliser (RFC 8785).
func ParamsDigest(params json.RawMessage) (types.Digest, error) {
	if _, err := contract.ReadStrict(params); err != nil {
		return "", fmt.Errorf("%w: params: %w", ErrKey, err)
	}
	d, err := eventstore.Digest(params)
	if err != nil {
		return "", fmt.Errorf("%w: params: %w", ErrKey, err)
	}
	return types.Digest(d), nil
}

// Key computes n's computation key at generation (13 [G1]):
// sha256 over the RFC 8785 form of {kind, definition_version, params (the
// params digest), inputs, generation}, where inputs are the declared
// inputs sorted by name, each with what it resolved to: {name, result} for
// a result input, {name, approval, revision} for an answer input. A
// generation bump re-keys the same node; it never makes a new one.
func Key(n types.Node, resolved map[string]Resolved, generation int64) (types.ComputationKey, error) {
	// The canonicaliser refuses a generation past 2^53−1.
	if generation < 1 {
		return types.ComputationKey{}, fmt.Errorf("%w: generation %d is below 1", ErrKey, generation)
	}
	if strings.TrimSpace(n.DefinitionVersion) == "" {
		return types.ComputationKey{}, fmt.Errorf("%w: no definition version", ErrKey)
	}
	params, err := ParamsDigest(n.Params)
	if err != nil {
		return types.ComputationKey{}, err
	}
	doc := keyed{Kind: n.Kind, DefinitionVersion: n.DefinitionVersion, Params: string(params), Inputs: []keyedInput{}, Generation: generation}
	for _, in := range n.Inputs {
		r := resolved[in.Name] // a missing resolution has no id, and is refused below
		if err := checkResolved(in, r); err != nil {
			return types.ComputationKey{}, fmt.Errorf("%w: input %s: %w", ErrKey, in.Name, err)
		}
		doc.Inputs = append(doc.Inputs, keyedInput{Name: in.Name, Resolved: r})
	}
	// One resolution per declared name: this also refuses a name declared
	// twice, since the resolutions are keyed by name.
	if len(resolved) != len(doc.Inputs) {
		return types.ComputationKey{}, fmt.Errorf("%w: %d resolutions for %d declared inputs", ErrKey, len(resolved), len(doc.Inputs))
	}
	slices.SortFunc(doc.Inputs, func(a, b keyedInput) int { return strings.Compare(a.Name, b.Name) })
	b, err := json.Marshal(doc)
	if err != nil {
		return types.ComputationKey{}, err
	}
	d, err := eventstore.Digest(b)
	if err != nil {
		return types.ComputationKey{}, fmt.Errorf("%w: %w", ErrKey, err)
	}
	return types.ComputationKey{Key: types.Digest(d), Generation: generation}, nil
}

// checkResolved: a result input resolves to a result id only, an answer
// input to an approval id with exactly the revision the input declares.
func checkResolved(in types.InputSpec, r Resolved) error {
	switch s := in.Source.Variant.(type) {
	case *types.ResultSource:
		if r.Approval != "" || r.Revision != 0 {
			return errors.New("a result input resolved to an approval")
		}
		return ids.Check(string(r.Result), "res")
	case *types.AnswerSource:
		if r.Result != "" {
			return errors.New("an answer input resolved to a result")
		}
		if r.Revision != s.Revision {
			return fmt.Errorf("the approval binds revision %d, the input declares %d", r.Revision, s.Revision)
		}
		return ids.Check(string(r.Approval), "apr")
	}
	return errors.New("an input with no source")
}
