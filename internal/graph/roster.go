// SPDX-License-Identifier: Apache-2.0

package graph

// The reviewer roster in a review's identity (13 "The reviewer roster is
// part of a review's identity"; 06 rule 3; plan 15 G6a).
//
// Decisions:
//   - A review node's params are its roster: each member's executor and
//     that executor's model family, sorted by executor, each executor once.
//     The roster is in the params digest, so in the computation key.
//   - The family is derived from the executor registry (Families), never
//     asserted by a caller: a review or review_requirement node is recorded
//     only through RecordReview, which refuses a roster whose families are
//     not the registry's, and an executor of unknown family fails closed
//     (ErrUnknownFamily, never a member). The fold reads no registry: it
//     checks the roster's shape and that no family is unknown, and trusts
//     RecordReview for the rest, as it trusts ClaimEvidence's contract.
//   - The required review is a review_requirement result, recorded by a
//     controller node with no inputs whose params are the requirement
//     {subject, roster, definition_version, revision}: the fold refuses a
//     result whose conclusions are not exactly its node's params.
//   - ReviewReusable decides whether a review need not run because a prior
//     review took an equivalent input: only when both have one non-input
//     identity (kind, definition version, params with the roster, key
//     generation) and the input is Equivalent for the prior review. Across
//     a roster change the replacement always runs.
//   - A review declares at most one review_requirement input. The claim fold
//     refuses a requirement whose roster or definition version is not the
//     review's. Selecting a new revision on a roster change is G6b's
//     supersession.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Families is the controller's executor registry: an executor's model
// family, receipt.UnknownFamily for an executor it does not hold.
type Families interface {
	Family(types.ExecutorID) string
}

// ErrUnknownFamily: the registry knows no family for a roster's executor,
// so it is never a reviewer (06 rule 3: unknown is not independent).
type ErrUnknownFamily struct{ Executor types.ExecutorID }

func (e ErrUnknownFamily) Error() string {
	return "graph: executor " + string(e.Executor) + " has no known model family"
}

// Roster is the roster of executors, sorted, each with the family the
// registry derives for it; an unknown family is ErrUnknownFamily
// (checkRoster).
func Roster(executors []types.ExecutorID, families Families) ([]types.RosterMember, error) {
	out := []types.RosterMember{}
	for _, e := range executors {
		out = append(out, types.RosterMember{Executor: e, Family: types.Name(families.Family(e))})
	}
	slices.SortFunc(out, func(a, b types.RosterMember) int { return strings.Compare(string(a.Executor), string(b.Executor)) })
	return out, checkRoster(out)
}

// RecordReview is Record for a review or review_requirement node, whose
// roster must be the one the registry derives for its executors.
func RecordReview(r types.NodeRecorded, families Families) ([]eventstore.Event, error) {
	roster, err := rosterOf(r.Node)
	if err != nil {
		return nil, err
	}
	executors := []types.ExecutorID{}
	for _, m := range roster {
		executors = append(executors, m.Executor)
	}
	want, err := Roster(executors, families)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(roster, want) {
		return nil, errors.New("graph: the roster is not the executors' sorted, with the registry's families")
	}
	return record(r)
}

// rosterOf is n's roster: its params' for a review, its requirement's for
// a review_requirement node, nil for any other kind.
func rosterOf(n types.Node) ([]types.RosterMember, error) {
	switch n.Kind {
	case types.NodeKindReview:
		p, err := n.ParamsAsReviewNodeParams()
		return p.Roster, err
	case types.NodeKindReviewRequirement:
		p, err := n.ParamsAsReviewRequirement()
		return p.Roster, err
	}
	return nil, nil
}

// checkRoster: a roster is sorted by executor, names each once, and has no
// unknown family.
func checkRoster(roster []types.RosterMember) error {
	if len(roster) == 0 {
		return errors.New("graph: a roster has a reviewer")
	}
	for i, m := range roster {
		if m.Family == receipt.UnknownFamily {
			return ErrUnknownFamily{m.Executor}
		}
		if i > 0 && roster[i-1].Executor >= m.Executor {
			return errors.New("graph: a roster is sorted by executor and names each once")
		}
	}
	return nil
}

// checkReview holds a review or review_requirement node to what its params
// schema cannot say: its roster's shape; a requirement node has no inputs;
// a review declares at most one review_requirement input.
func checkReview(n types.Node) error {
	roster, err := rosterOf(n)
	if err != nil || roster == nil {
		return err
	}
	if err := checkRoster(roster); err != nil {
		return err
	}
	if n.Kind == types.NodeKindReviewRequirement && len(n.Inputs) != 0 {
		return errors.New("graph: a review_requirement node has no inputs")
	}
	if len(requirementInputs(n)) > 1 {
		return errors.New("graph: a review declares one review_requirement input at most")
	}
	return nil
}

// requirementInputs are n's review_requirement inputs.
func requirementInputs(n types.Node) []types.Name {
	out := []types.Name{}
	for _, in := range n.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement {
			out = append(out, in.Name)
		}
	}
	return out
}

// requirementOf decodes a review_requirement result's conclusions.
func requirementOf(r types.Result) (types.ReviewRequirement, error) {
	var req types.ReviewRequirement
	if r.Kind != types.ResultKindReviewRequirement || r.Conclusions == nil {
		return req, fmt.Errorf("graph: result %s is no review requirement", r.ID)
	}
	d := json.NewDecoder(strings.NewReader(string(*r.Conclusions)))
	d.DisallowUnknownFields()
	return req, d.Decode(&req)
}

// recordsRequirement: a review_requirement result's conclusions are its
// node's params, exactly.
func recordsRequirement(rec nodeRecord, r types.Result) error {
	if r.Kind != types.ResultKindReviewRequirement {
		return nil
	}
	if r.Conclusions == nil {
		return errors.New("a review requirement result has conclusions")
	}
	want, err := ParamsDigest(rec.Recorded.Node.Params)
	got, gerr := ParamsDigest(*r.Conclusions)
	if err = errors.Join(err, gerr); err != nil || got != want {
		return errors.Join(err, errors.New("a review requirement's conclusions are its node's params"))
	}
	return nil
}

// bindsRequirement: the requirement a review's claim resolved has the
// review's roster and definition version.
func bindsRequirement(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved) error {
	for _, name := range requirementInputs(rec.Recorded.Node) {
		r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(resolved[name].Result))
		if err != nil {
			return err
		}
		req, err := requirementOf(r.Result)
		roster, rerr := rosterOf(rec.Recorded.Node)
		if err = errors.Join(err, rerr); err != nil {
			return err
		}
		if !slices.Equal(req.Roster, roster) || req.DefinitionVersion != rec.Recorded.Node.DefinitionVersion {
			return fmt.Errorf("requirement %s is not of the review's roster and definition version", r.Result.ID)
		}
	}
	return nil
}

// ReviewReusable reports whether review need not run because prior, a
// review of the same non-input identity taking input name from the same
// producer, ran on used, and fresh, the producer's selected result now,
// is Equivalent to it for prior. Across a
// roster change nothing is reusable: the replacement runs (13 "Nothing
// reuses a review across rosters").
func ReviewReusable(tx *eventstore.Tx, prior, review types.NodeID, name types.Name, used, fresh types.ResultID, blobs receipt.Blobs) (bool, error) {
	was, err := loadNode(tx, prior)
	rec, rerr := loadNode(tx, review)
	if err = errors.Join(err, rerr); err != nil {
		return false, err
	}
	a, b := was.Recorded.Node, rec.Recorded.Node
	named := func(in types.InputSpec) bool { return in.Name == name }
	i, j := slices.IndexFunc(a.Inputs, named), slices.IndexFunc(b.Inputs, named)
	if i < 0 || j < 0 || !sameSource(a.Inputs[i], b.Inputs[j]) {
		return false, fmt.Errorf("graph: %s and %s do not both take %s from one producer", prior, review, name)
	}
	if same, err := sameIdentity(a, b, was.State.Key, rec.State.Key); err != nil || !same {
		return false, err
	}
	return Equivalent(tx, prior, name, used, fresh, blobs)
}

// sameSource: x and y take results of one kind from one producer.
func sameSource(x, y types.InputSpec) bool {
	sx, ok := x.Source.Variant.(*types.ResultSource)
	sy, oky := y.Source.Variant.(*types.ResultSource)
	return ok && oky && *sx == *sy
}

// sameIdentity: a and b have one non-input identity (13): kind, definition
// version, params (a review's roster) and key generation, none (0) for an
// unclaimed node.
func sameIdentity(a, b types.Node, ka, kb *types.ComputationKey) (bool, error) {
	pa, err := ParamsDigest(a.Params)
	pb, berr := ParamsDigest(b.Params)
	gen := func(k *types.ComputationKey) int64 {
		if k == nil {
			return 0
		}
		return k.Generation
	}
	return a.Kind == b.Kind && a.DefinitionVersion == b.DefinitionVersion && (pa == pb || mutation.Is(mutation.EquivalenceIgnoresRoster)) &&
		gen(ka) == gen(kb), errors.Join(err, berr)
}
