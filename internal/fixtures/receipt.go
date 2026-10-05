// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// refusalStep maps a receipt or authority check to its step: passed is
// empty/completed_clean; evidence that does not serve is could_not_run and
// a refused authority step failed, each with its own reason. Any other
// error means the fixture could not run.
func refusalStep(err error) (Step, error) {
	var r *receipt.Refusal
	var a *authority.Refusal
	switch {
	case err == nil:
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	case errors.As(err, &r):
		return Step{vocab.CouldNotRun, r.Reason.String()}, nil
	case errors.As(err, &a):
		return Step{vocab.Failed, a.Reason.String()}, nil
	}
	return Step{}, err
}

// refused records a receipt or authority check's step.
func (o *observe) refused(err error) error {
	st, err := refusalStep(err)
	if err != nil {
		return err
	}
	o.steps = append(o.steps, st)
	return nil
}

// derived records a receipt's derivation as its step.
func (o *observe) derived(d receipt.Derived, err error) error {
	if err != nil {
		return err
	}
	o.steps = append(o.steps, Step{d.Outcome, d.Reason.String()})
	return nil
}

var (
	instrumentDigest = types.Digest("sha256:" + strings.Repeat("a", 64))
	receiptsAt       = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
)

// goTest is a gate instrument: a required tests predicate with JUnit
// evidence, and the producer's own status as an optional predicate.
func goTest() types.InstrumentContract {
	return types.InstrumentContract{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest, SubjectType: types.InstrumentContractSubjectTypeTree,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "tests", EvidenceRequired: []string{"application/junit+xml"}}},
		OptionalPredicates: []string{"reported_status"}, Preconditions: []types.Name{},
		ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{"junit": "diagnostic"}}
}

// reviewPanel is a review instrument whose receipts are about proposals.
func reviewPanel() types.InstrumentContract {
	return types.InstrumentContract{Name: "review-panel", Version: "1.0.0", Digest: instrumentDigest, SubjectType: types.InstrumentContractSubjectTypeProposal,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "findings", EvidenceRequired: []string{}}},
		OptionalPredicates: []string{}, Preconditions: []types.Name{}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}
}

func pred(name string, o vocab.Outcome, reason vocab.Reason, ev ...types.PredicateEvidenceItem) types.Predicate {
	return types.Predicate{Name: name, Outcome: types.Outcome(o), Reason: reason, Detail: name, Evidence: append([]types.PredicateEvidenceItem{}, ev...)}
}

// newReceipt is a receipt of kind about subject from instrument c, by the
// given model families; its derived fields are left for Derive.
func newReceipt(kind types.ReceiptKind, c types.InstrumentContract, subject types.Subject, families []string, ps ...types.Predicate) (types.Receipt, error) {
	var idv [2]string
	for i, p := range []string{"rcp", "cmd"} {
		v, err := ids.New(p)
		if err != nil {
			return types.Receipt{}, err
		}
		idv[i] = v
	}
	return types.Receipt{ID: types.ReceiptID(idv[0]), Kind: kind, Instrument: types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest},
		Subject: subject, Provenance: types.ReceiptProvenance{Command: types.CommandID(idv[1]), Calls: []types.CallID{}, ModelFamilies: families,
			Contributors: []types.ActorID{}}, ProducedAt: "2026-10-05T11:00:00Z", Predicates: append([]types.Predicate{}, ps...)}, nil
}

// recorded is r with the derived fields the controller records.
func recorded(c types.InstrumentContract, r types.Receipt, blobs receipt.Blobs) (types.Receipt, error) {
	d, err := receipt.Derive(c, r, blobs)
	r.Outcome, r.Reason, r.Detail, r.Verdict, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.Verdict, d.ConclusionsDigest
	return r, err
}

// gateRun is a gate instrument's run: its JUnit evidence published in the
// blob store, and the tree subject its receipts are about.
func gateRun(dir string) (*blobstore.Store, types.PredicateEvidenceItem, types.Subject, error) {
	blobs, err := blobstore.Open(filepath.Join(dir, "state"))
	if err != nil {
		return nil, types.PredicateEvidenceItem{}, types.Subject{}, err
	}
	dg, err := blobs.Put(strings.NewReader("<testsuite/>"), "")
	return blobs, types.PredicateEvidenceItem{Blob: types.Digest(dg), MediaType: "application/junit+xml", Artifact: "junit"},
		types.Subject{Variant: &types.TreeSubject{Type: "tree", Project: projectA, Tree: types.GitID("git:" + strings.Repeat("c", 40)),
			Environment: types.Environment{Identity: instrumentDigest, Description: "linux/amd64"}, ContractRevision: "3"}}, err
}

// nestedReceiptLie: the producer's own status says ok while the inner
// tests predicate found a failing test; the receipt derives the finding,
// and one recorded clean over it is refused (11 "nested receipt lie"; 06
// "Derivation" 2 and 3).
func nestedReceiptLie(_ context.Context, dir string) (steps []Step, violations []string, err error) {
	blobs, junit, tree, err := gateRun(dir)
	if err != nil {
		return nil, nil, err
	}
	ok := pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean)
	var o observe
	for _, tests := range []types.Predicate{pred("tests", vocab.Empty, vocab.ReasonCompletedClean, junit), pred("tests", vocab.Found, vocab.ReasonGateTestFailed, junit)} {
		r, err := newReceipt(types.ReceiptKindGate, goTest(), tree, []string{}, ok, tests)
		if err != nil {
			return nil, nil, err
		}
		if err := o.derived(receipt.Derive(goTest(), r, blobs)); err != nil {
			return nil, nil, err
		}
		// The producer records its outer status as the receipt's.
		lie, err := recorded(goTest(), r, blobs)
		if err != nil {
			return nil, nil, err
		}
		lie.Outcome, lie.Reason, lie.Detail = types.Outcome(vocab.Empty), vocab.ReasonCompletedClean, ""
		err = receipt.Check(goTest(), lie, blobs)
		o.check((err == nil) == (tests.Outcome == types.Outcome(vocab.Empty)), "a receipt recorded clean over a %s inner predicate: %v", tests.Outcome, err)
	}
	return o.steps, o.violations, nil
}

// omittedRequiredPredicate: a receipt without its required predicate, or
// with none at all, is could_not_run/receipt_incomplete, never empty (11
// "omitted required predicate"; 06 "Derivation" 1).
func omittedRequiredPredicate(_ context.Context, dir string) (steps []Step, violations []string, err error) {
	blobs, junit, tree, err := gateRun(dir)
	if err != nil {
		return nil, nil, err
	}
	var o observe
	for _, ps := range [][]types.Predicate{
		{pred("tests", vocab.Empty, vocab.ReasonCompletedClean, junit)},
		{pred("reported_status", vocab.Empty, vocab.ReasonCompletedClean)},
		{},
	} {
		r, err := newReceipt(types.ReceiptKindGate, goTest(), tree, []string{}, ps...)
		if err != nil {
			return nil, nil, err
		}
		d, err := receipt.Derive(goTest(), r, blobs)
		if err := o.derived(d, err); err != nil {
			return nil, nil, err
		}
		o.check(len(ps) == 1 && ps[0].Name == "tests" || d.Detail == "missing predicate tests", "the detail %q does not name the omitted predicate", d.Detail)
	}
	return o.steps, o.violations, nil
}

// proposalSubject is a review subject: a proposal and its binding digest.
func proposalSubject(p types.ProposalID, binding types.Digest) types.Subject {
	return types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: p, BindingDigest: binding}}
}

// unknownReviewerFamily: a reviewer counts as independent only with known
// families disjoint from the proposer's; an unknown family on either side
// blocks (11 "unknown reviewer family"; 06 rule 3).
func unknownReviewerFamily(context.Context, string) (steps []Step, violations []string, err error) {
	prp, err := ids.New("prp")
	if err != nil {
		return nil, nil, err
	}
	var o observe
	for _, c := range []struct{ reviewer, proposer string }{{"claude", "gpt"}, {"unknown", "gpt"}, {"claude", "unknown"}} {
		r, err := newReceipt(types.ReceiptKindReview, reviewPanel(), proposalSubject(types.ProposalID(prp), instrumentDigest), []string{c.reviewer},
			pred("findings", vocab.Empty, vocab.ReasonCompletedClean))
		if err == nil {
			r, err = recorded(reviewPanel(), r, nil)
		}
		if err != nil {
			return nil, nil, err
		}
		if err := o.refused(receipt.Independent(r, []string{c.proposer})); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

// unrelatedGOReuse: a GO applies only to the proposal and binding digest
// it names; reused for another proposal, or for the same proposal bound to
// another change, it does not apply (11 "unrelated GO reuse"; 06 rule 4).
func unrelatedGOReuse(context.Context, string) (steps []Step, violations []string, err error) {
	var prp [2]string
	for i := range prp {
		if prp[i], err = ids.New("prp"); err != nil {
			return nil, nil, err
		}
	}
	bound, other := instrumentDigest, types.Digest("sha256:"+strings.Repeat("b", 64))
	r, err := newReceipt(types.ReceiptKindReview, reviewPanel(), proposalSubject(types.ProposalID(prp[0]), bound), []string{"claude"},
		pred("findings", vocab.Empty, vocab.ReasonCompletedClean))
	if err != nil {
		return nil, nil, err
	}
	var o observe
	if err := o.derived(receipt.Derive(reviewPanel(), r, nil)); err != nil {
		return nil, nil, err
	}
	if r, err = recorded(reviewPanel(), r, nil); err != nil {
		return nil, nil, err
	}
	o.check(r.Verdict != nil && *r.Verdict == types.ReceiptVerdictValueGo, "a clean review did not derive GO")
	for _, s := range []types.Subject{proposalSubject(types.ProposalID(prp[0]), bound), proposalSubject(types.ProposalID(prp[1]), bound), proposalSubject(types.ProposalID(prp[0]), other)} {
		req := receipt.Requirement{Kind: types.ReceiptKindReview, Instrument: r.Instrument, Subject: s}
		if err := o.refused(receipt.Applies(r, req, receiptsAt)); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}
