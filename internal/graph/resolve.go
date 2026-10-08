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
//   - The result holds exactly one receipt Ref, and the receipt is the
//     Ref's (its id) and the result's (its conclusions_digest is the RFC
//     8785 digest of the result's conclusions); else receipt_incomplete.
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
		req, err := need(rec.Recorded.Node, in)
		if err == nil && req == nil && (src.ResultKind == types.ResultKindGateResult || src.ResultKind == types.ResultKindEvidence) {
			err = fmt.Errorf("graph: input %s takes a %s and has no receipt requirement", in.Name, src.ResultKind)
		}
		if err != nil {
			return nil, nil, err
		}
		if req != nil && !mutation.Is(mutation.ResolveSkipsApplicability) {
			if err := applies(r.Result, *req, blobs, now); err != nil {
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
func applies(r types.Result, req receipt.Requirement, blobs receipt.Blobs, now time.Time) error {
	refs := slices.DeleteFunc(slices.Clone(r.Value), func(ref types.Ref) bool { return ref.Type != types.RefTypeReceipt })
	if len(refs) != 1 {
		return &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete, Detail: fmt.Sprintf("the result holds %d receipts, not one", len(refs))}
	}
	rc, err := blobs.Open(string(refs[0].ContentDigest))
	if err != nil {
		return err
	}
	b, err := io.ReadAll(rc)
	if err = errors.Join(err, rc.Close()); err != nil {
		return err
	}
	rcpt, err := contract.Decode[types.Receipt]("records/receipt", b)
	if err != nil {
		return err
	}
	var conclusions types.Digest
	if r.Conclusions != nil {
		d, err := eventstore.ContentDigest(*r.Conclusions)
		if err != nil {
			return err
		}
		conclusions = types.Digest(d)
	}
	if string(rcpt.ID) != refs[0].ID || rcpt.ConclusionsDigest != conclusions {
		return &receipt.Refusal{Reason: vocab.ReasonReceiptIncomplete, Detail: "the receipt is not the one the result's Ref and conclusions name"}
	}
	return receipt.Applies(rcpt, req, now)
}
