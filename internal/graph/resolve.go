// SPDX-License-Identifier: Apache-2.0

package graph

// The resolver (13 "Evaluation", [G2]; 06; plan 15 G4a).
//
// Decisions:
//   - An input resolves to its producer's selected result: selected,
//     reusable (only a completed result that is not could_not_run is ever
//     selected), and applicable. A producer not completed at its identity's
//     current generation is a structural wait, not an error; one of another
//     project or with a result of another kind is missing (selectedOf, the
//     claim's own rule).
//   - Applicability is the consuming kind's: Requirements, Go code of the
//     kind at its definition_version (which keys it), names the receipt a
//     result must carry. A gate or evidence result holds its receipt as a
//     receipt Ref whose content digest names the receipt blob; the resolver
//     reads it and asks internal/receipt (Applies). A refusal, an expired
//     receipt included, makes the input missing, with the refusal's reason.
//     A gate or evidence input always has a requirement: none is an error.
//     A review requirement carries no receipt: it serves at its subject's
//     selected revision (serves, requirement.go).
//   - The result holds exactly one receipt Ref, and the receipt is the
//     Ref's (its id) and the result's: the result's conclusions are the
//     receipt's rule 2a document (receipt.Conclusions), whose digest the
//     receipt records; else receipt_incomplete.
//   - Answer inputs are the authority's (an input.answer approval binding
//     the owed input's revision, 04): the caller passes them resolved, and
//     one with no approval or of another revision is missing. G4b checks
//     the approval again at commit.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Requirements is a kind's: the receipt an input's result must carry, or
// nil when the input carries none.
type Requirements func(n types.Node, in types.InputSpec) (*receipt.Requirement, error)

// Missing is an input that did not resolve: a structural wait (Refusal
// nil), or a result that does not serve, with the refusal saying why.
type Missing struct {
	Name    types.Name
	Refusal *receipt.Refusal
	Detail  string
}

// Resolve resolves node's declared inputs at now: the result inputs from
// their producers, the answer inputs from answered. It returns the inputs
// that resolved and those that did not.
func Resolve(tx *eventstore.Tx, node types.NodeID, answered map[string]Resolved, need Requirements, blobs receipt.Blobs, now time.Time) (map[string]Resolved, []Missing, error) {
	rec, err := loadNode(tx, node)
	if err != nil || need == nil {
		return nil, nil, errors.Join(err, errors.New("graph: resolve needs the kind's requirements"))
	}
	resolved, missing := map[string]Resolved{}, []Missing{}
	for _, in := range rec.Recorded.Node.Inputs {
		src, ok := in.Source.Variant.(*types.ResultSource)
		if !ok {
			ask := in.Source.Variant.(*types.AnswerSource)
			if r, ok := answered[in.Name]; ok && r.Approval != "" && r.Revision == ask.Revision {
				resolved[in.Name] = r
			} else {
				missing = append(missing, Missing{Name: in.Name, Detail: fmt.Sprintf("no approved answer at revision %d", ask.Revision)})
			}
			continue
		}
		r, why, err := selectedOf(tx, rec, src)
		if err != nil {
			return nil, nil, err
		}
		if why != "" {
			missing = append(missing, Missing{Name: in.Name, Detail: why})
			continue
		}
		if src.ResultKind == types.ResultKindReviewRequirement { // no receipt: it serves at its subject's selected revision (serves)
			resolved[in.Name] = Resolved{Result: r.Result.ID}
			continue
		}
		req, err := need(rec.Recorded.Node, in)
		if err == nil && req == nil && carriesConclusions(src.ResultKind) {
			err = fmt.Errorf("graph: input %s takes a %s and has no receipt requirement", in.Name, src.ResultKind)
		}
		if err != nil {
			return nil, nil, err
		}
		if req != nil && !mutation.Is(mutation.ResolveSkipsApplicability) {
			err := applies(tx, r, *req, blobs, now)
			if err == nil && rec.Recorded.Node.Kind == types.NodeKindMerge {
				err = refreshed(tx, rec, r, blobs)
			}
			if err != nil {
				var refusal *receipt.Refusal
				if !errors.As(err, &refusal) {
					return nil, nil, err
				}
				missing = append(missing, Missing{Name: in.Name, Refusal: refusal, Detail: refusal.Detail})
				continue
			}
		}
		resolved[in.Name] = Resolved{Result: r.Result.ID}
	}
	return resolved, missing, nil
}

// applies reads the receipt a result holds and decides whether it applies.
func applies(tx *eventstore.Tx, r types.ResultRecorded, req receipt.Requirement, blobs receipt.Blobs, now time.Time) error {
	_, rcpt, err := receiptOf(r, blobs)
	if err == nil {
		err = sinceRenewal(tx, r.Node, rcpt)
	}
	if err != nil {
		return err
	}
	return receipt.Applies(rcpt, req, now)
}

// sinceRenewal refuses a receipt produced before node's last renewal: the
// renewal asks for fresher evidence, not only a newer result.
func sinceRenewal(tx *eventstore.Tx, node types.NodeID, rcpt types.Receipt) error {
	ren, renewed, err := load[renewal](tx, RenewalProjection, string(node))
	if err != nil || !renewed {
		return err
	}
	p, err1 := time.Parse(time.RFC3339Nano, string(rcpt.ProducedAt))
	at, err2 := time.Parse(time.RFC3339Nano, string(ren.At))
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	if p.Before(at) {
		return &receipt.Refusal{Reason: vocab.ReasonEvidenceInapplicable, Detail: fmt.Sprintf("the receipt was produced before %s's renewal at %s", node, ren.At)}
	}
	return nil
}

// receiptRef is the one receipt Ref a result holds.
func receiptRef(r types.Result) (types.Ref, error) {
	refs := slices.DeleteFunc(slices.Clone(r.Value), func(ref types.Ref) bool { return ref.Type != types.RefTypeReceipt })
	if len(refs) != 1 {
		return types.Ref{}, &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete, Detail: fmt.Sprintf("the result holds %d receipts, not one", len(refs))}
	}
	return refs[0], nil
}

// readReceipt decodes the receipt blob ref names.
func readReceipt(ref types.Ref, blobs receipt.Blobs) (types.Receipt, error) {
	if blobs == nil {
		return types.Receipt{}, errors.New("graph: a receipt Ref needs the blob store")
	}
	rc, err := blobs.Open(string(ref.ContentDigest))
	if err != nil {
		return types.Receipt{}, err
	}
	b, err := io.ReadAll(rc)
	if err = errors.Join(err, rc.Close()); err != nil {
		return types.Receipt{}, err
	}
	return contract.Decode[types.Receipt]("records/receipt", b)
}

// carriesConclusions: the result kinds whose conclusions are their
// receipt's rule 2a document (records/result).
func carriesConclusions(k types.ResultKind) bool {
	return k == types.ResultKindGateResult || k == types.ResultKindEvidence
}

// receiptOf reads the receipt a recorded result holds. It must be the
// Ref's (its id), whole (its conclusions_digest is its own document's),
// valid until what the result recorded, and, for a kind that carries
// conclusions, the result's. A receipt that is not is a receipt_incomplete
// Refusal.
func receiptOf(rr types.ResultRecorded, blobs receipt.Blobs) (types.Ref, types.Receipt, error) {
	r := rr.Result
	ref, err := receiptRef(r)
	if err != nil {
		return ref, types.Receipt{}, err
	}
	rcpt, err := readReceipt(ref, blobs)
	if err != nil {
		return ref, rcpt, err
	}
	_, conclusions, err := receipt.Conclusions(rcpt)
	if err != nil {
		return ref, rcpt, err
	}
	var held string
	if r.Conclusions != nil {
		if held, err = eventstore.ContentDigest(*r.Conclusions); err != nil {
			return ref, rcpt, err
		}
	}
	bound := (rr.ReceiptValidUntil == nil) == (rcpt.ValidUntil == nil) && (rcpt.ValidUntil == nil || sameTime(string(*rr.ReceiptValidUntil), string(*rcpt.ValidUntil)))
	if string(rcpt.ID) != ref.ID || rcpt.ConclusionsDigest != conclusions || !bound || carriesConclusions(r.Kind) && types.Digest(held) != conclusions {
		return ref, rcpt, &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete, Detail: "the receipt is not the one the result's Ref, validity and conclusions name"}
	}
	return ref, rcpt, nil
}

// refreshed: r, a check result a merge takes, holds a receipt about its
// generation's prospective tree, the git tree its carry_over's attestation
// holds (what the merge applies), when the receipt is about a tree. A
// receipt about anything else (a package's runtime) is bound by the
// merge's requirement alone.
func refreshed(tx *eventstore.Tx, rec nodeRecord, r types.ResultRecorded, blobs receipt.Blobs) error {
	co, err := loadNode(tx, producerOf(rec.Recorded.Node, "carry_over"))
	_, rcpt, cerr := receiptOf(r, blobs)
	if err = errors.Join(err, cerr); err != nil {
		return err
	}
	sub, tree := rcpt.Subject.Variant.(*types.TreeSubject)
	if !tree {
		return nil
	}
	var prospective types.ResultRecorded
	ok := false
	if co.State.Result != nil {
		prospective, ok, err = load[types.ResultRecorded](tx, ResultProjection, string(*co.State.Result))
	}
	if err == nil && (!ok || len(prospective.Result.Value) == 0 || string(sub.Tree) != prospective.Result.Value[0].ID) {
		err = &receipt.Refusal{Reason: vocab.ReasonEvidenceInapplicable, Detail: fmt.Sprintf("the receipt is about %s, not the generation's prospective tree", sub.Tree)}
	}
	return err
}
