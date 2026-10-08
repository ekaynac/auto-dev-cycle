// SPDX-License-Identifier: Apache-2.0

package graph

// The reviewer roster in a review's identity (13 "The reviewer roster is
// part of a review's identity"; 06 rule 3; plan 15 G6a).
//
// Decisions (13 has the rationale):
//   - A review's params are its subject and roster (executor, family),
//     sorted by executor, each once: both are in the key.
//   - Families come from the registry, never a caller: RecordReview and
//     ClaimReview both refuse a roster the registry does not derive now
//     (ErrNotIndependent); the evaluator holds such a node on
//     reviewer_unavailable and goes on. The fold reads no registry and
//     checks only the roster's shape: a raw node.claimed is outside the
//     guarantee, and M1's dispatch re-checks the registry.
//   - A review_requirement node has no inputs; its result's conclusions are
//     its params. A review's optional requirement input binds its subject,
//     roster and definition version (mandatory in G7b-2).
//   - ReviewReusable reuses only when the review's key, its other inputs
//     resolved now, is the prior's claimed key, with the same inputs.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Families is the controller's executor registry: an executor's model
// family, receipt.UnknownFamily for an executor it does not hold.
type Families interface {
	Family(types.ExecutorID) string
}

// ErrNotIndependent: a roster member's family is unknown, or is not the
// one the registry now derives for its executor (06 rule 3). Its outcome is
// could_not_run/reviewer_not_independent.
type ErrNotIndependent struct{ Executor types.ExecutorID }

func (e ErrNotIndependent) Error() string {
	return "graph: executor " + string(e.Executor) + " has no registered model family of its roster"
}

// Reason is the refusal's reason code; the evaluator holds the node on
// reviewer_unavailable instead of claiming it.
func (ErrNotIndependent) Reason() vocab.Reason { return vocab.ReasonReviewerNotIndependent }

// Roster is the roster of executors, sorted, each with the family the
// registry derives for it.
func Roster(executors []types.ExecutorID, families Families) ([]types.RosterMember, error) {
	out := []types.RosterMember{}
	for _, e := range executors {
		out = append(out, types.RosterMember{Executor: e, Family: types.Name(families.Family(e))})
	}
	slices.SortFunc(out, func(a, b types.RosterMember) int { return strings.Compare(string(a.Executor), string(b.Executor)) })
	return out, checkRoster(out)
}

// registered: n's roster is the one families derives for its executors.
func registered(n types.Node, families Families) error {
	roster, _, err := rosterOf(n)
	if err != nil || roster == nil || families == nil {
		return errors.Join(err, fmt.Errorf("graph: a %s node is checked against an executor registry", n.Kind))
	}
	for _, m := range roster {
		if types.Name(families.Family(m.Executor)) != m.Family { // an unknown family is no roster family (checkRoster)
			return ErrNotIndependent{m.Executor}
		}
	}
	return checkRoster(roster)
}

// RecordReview is Record for a review or review_requirement node, whose
// roster must be the one the registry derives for its executors.
func RecordReview(r types.NodeRecorded, families Families) ([]eventstore.Event, error) {
	if err := registered(r.Node, families); err != nil {
		return nil, err
	}
	return record(r)
}

// ClaimReview is Claim for a review or review_requirement node: its roster
// is checked against the registry again, whoever recorded it.
func ClaimReview(tx *eventstore.Tx, node types.NodeID, resolved map[string]Resolved, generation int64, families Families) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return nil, err
	}
	if err := registered(rec.Recorded.Node, families); err != nil {
		return nil, err
	}
	return claimNode(tx, rec, resolved, generation)
}

// rostered: the kinds with a roster.
func rostered(k types.NodeKind) bool {
	return k == types.NodeKindReview || k == types.NodeKindReviewRequirement
}

// rosterOf is n's roster and subject: its params' for a review, its
// requirement's for a review_requirement node, nil for any other kind.
func rosterOf(n types.Node) ([]types.RosterMember, types.Name, error) {
	switch n.Kind {
	case types.NodeKindReview:
		p, err := n.ParamsAsReviewNodeParams()
		return p.Roster, p.Subject, err
	case types.NodeKindReviewRequirement:
		p, err := n.ParamsAsReviewRequirement()
		return p.Roster, p.Subject, err
	}
	return nil, "", nil
}

// checkRoster: a roster has a member, is sorted by executor, names each
// once, and has no unknown family.
func checkRoster(roster []types.RosterMember) error {
	if len(roster) == 0 {
		return errors.New("graph: a roster has a reviewer")
	}
	for i, m := range roster {
		if m.Family == receipt.UnknownFamily {
			return ErrNotIndependent{m.Executor}
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
	roster, _, err := rosterOf(n)
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
// review's subject, roster and definition version. Only a review is bound.
func bindsRequirement(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved) error {
	if rec.Recorded.Node.Kind != types.NodeKindReview {
		return nil // carry_over and merge get their own rules (G7b-2)
	}
	for _, name := range requirementInputs(rec.Recorded.Node) {
		r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(resolved[name].Result))
		if err != nil {
			return err
		}
		req, err := requirementOf(r.Result)
		roster, subject, rerr := rosterOf(rec.Recorded.Node)
		if err = errors.Join(err, rerr); err != nil {
			return err
		}
		if req.Subject != subject || !slices.Equal(req.Roster, roster) || req.DefinitionVersion != rec.Recorded.Node.DefinitionVersion {
			return fmt.Errorf("requirement %s is not of the review's subject, roster and definition version", r.Result.ID)
		}
	}
	return nil
}

// ReviewReusable reports whether review need not run: prior declares the
// same inputs and ran on used for input name; review's other inputs,
// resolved now as a claim would (answers through env.Authority, then
// Resolve: selected, applicable and fresh results), with name at used, key
// to prior's claimed key at its generation; and fresh, name's producer's
// selected result now, is Equivalent to used for prior.
func ReviewReusable(tx *eventstore.Tx, prior, review types.NodeID, name types.Name, used, fresh types.ResultID, env Env) (bool, error) {
	was, err := loadNode(tx, prior)
	rec, rerr := loadNode(tx, review)
	c, claimed, cerr := load[claim](tx, ClaimProjection, string(prior))
	if err = errors.Join(err, rerr, cerr); err != nil || !claimed {
		return false, errors.Join(err, fmt.Errorf("graph: %s never ran", prior))
	}
	if !reflect.DeepEqual(was.Recorded.Node.Inputs, rec.Recorded.Node.Inputs) {
		return false, fmt.Errorf("graph: %s and %s declare other inputs", prior, review)
	}
	n := rec.Recorded.Node
	if mutation.Is(mutation.EquivalenceIgnoresRoster) {
		n.Params = was.Recorded.Node.Params
	}
	if tx.Now().IsZero() {
		return false, errors.New("graph: reuse is decided at a writer transaction's time")
	}
	answered, stale, err := answers(tx, rec, env.Authority)
	if err != nil || stale {
		return false, err // a stale answer: the review waits for its successor
	}
	// Every input, fresh on name included, resolves now for review itself:
	// selected in its project, applicable and unexpired.
	resolved, missing, err := Resolve(tx, review, answered, env.Need, env.Blobs, tx.Now())
	if err != nil || len(missing) > 0 || resolved[string(name)].Result != fresh {
		return false, err
	}
	// name's result is used's: Equivalent holds prior's claim to it.
	resolved[string(name)] = Resolved{Result: used}
	if k, err := Key(n, resolved, c.Key.Generation); err != nil || k != c.Key {
		return false, err
	}
	return Equivalent(tx, prior, name, used, fresh, env.Blobs)
}
