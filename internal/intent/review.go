// SPDX-License-Identifier: Apache-2.0

package intent

// A reviewer's rendered context (P22a-6b; I18): what review.Prompt renders
// for a review node, read from the log alone, by the runner that builds
// the reviewer's intent and by the fold that binds it (bind), so the two
// agree byte for byte. The review's claimed inputs, in its declared order:
//   - plan: the task's brief, its title and requirement (brief.Prompt) the
//     prompt's task;
//   - patch: the build's diff, the prompt's candidate change (none for an
//     empty candidate, patch/empty with no ref: "no changes"), read to at
//     most review.MaxDiff+1 bytes (Prompt refuses a longer one; 22a-6c
//     records a verdict for a candidate whose context cannot be built:
//     Opus 22a-6b M2);
//   - gate: the gate's result, its receipt and its report, matched by
//     type, read at the gate's own bound (gate.MaxReportBytes). Only the
//     report's verdict and counts are rendered (gate.SummaryOf, its bytes
//     checked as gate.CheckReport checks them), never an id (#321 L7): the
//     receipt is in the context (graph.Context renders the result whole)
//     and omitted, reason policy;
//   - required: the review's requirement, which holds no ref.
//
// Included are the brief, the patch and the report, each at its blob's
// size (for a reviewer, an included item's bytes are its blob's, not what
// it renders into: a builder's is its prompt's; Opus 22a-6b L1, stated); any other shape of claim, or a context graph.Context renders
// otherwise, is refused.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// ErrUnreviewable: the candidate gives no review context (a diff over
// review.MaxDiff, or one review.Prompt refuses): the review's verdict,
// never the controller's failure (22a-6c; Opus 22a-6b M2).
var ErrUnreviewable = errors.New("intent: the candidate cannot be reviewed")

// PromptContext is a model node's rendered prompt and its context as its
// consumer's intent states it: a reviewer's (Review) or a builder's
// (Build).
type PromptContext struct {
	Prompt   []byte
	Included []types.Ref
	Omitted  []types.ManifestIntentOmittedItem
	Bytes    map[types.Digest]int64 // each included ref's size, by identity digest
	GateIDs  []string               // the gate report's ids (gate.ReportIDs), never rendered: what a findings view redacts
}

// Rendered is c as the context intent.Reviewer or Builder takes.
func (c PromptContext) Rendered() graph.Rendered {
	return graph.Rendered{Included: slices.Clone(c.Included), Omitted: slices.Clone(c.Omitted)}
}

// Review is node's PromptContext in tx, rendered its graph.Context.
func Review(tx *eventstore.Tx, blobs receipt.Blobs, node types.NodeID, rendered graph.Rendered) (PromptContext, error) {
	results, _, err := claimedResults(tx, node)
	if err != nil {
		return PromptContext{}, err
	}
	out := PromptContext{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{}, Bytes: map[types.Digest]int64{}}
	var whole []types.Ref // the context as graph.Context renders it
	var in review.PromptInput
	empty := false // an empty candidate: review.Prompt renders no change
	include := func(ref types.Ref, limit int64) ([]byte, error) {
		b, ok, err := readBlob(blobs, ref.ContentDigest, limit)
		switch {
		case err != nil || ok:
		case int64(len(b)) > limit: // the candidate's: a patch over review.MaxDiff (Opus 22a-6b M2)
			err = fmt.Errorf("%w: blob %s is over %d bytes", ErrUnreviewable, ref.ContentDigest, limit)
		default:
			err = fmt.Errorf("%w: blob %s", ErrBlobDamaged, ref.ContentDigest) // the host's (Opus 22a-6d H1 re-check L2)
		}
		out.Included, out.Bytes[ref.IdentityDigest] = append(out.Included, ref), int64(len(b))
		return b, err
	}
	for i, raw := range results {
		v := raw.Result.Value
		whole = append(whole, v...)
		switch k := raw.Result.Kind; {
		case k == types.ResultKindPlan && len(v) == 1:
			b, err := include(v[0], MaxBrief)
			if err != nil {
				return PromptContext{}, err
			}
			pb, err := brief.Decode(b)
			if err != nil {
				return PromptContext{}, err
			}
			in.Requirement = string(brief.Prompt(pb))
		case k == types.ResultKindPatch && len(v) == 0: // patch/empty: no diff, and no ref (Astra chunk 3 M2)
			empty = true
		case k == types.ResultKindPatch && len(v) == 1:
			b, err := include(v[0], review.MaxDiff)
			if err != nil {
				return PromptContext{}, err
			}
			in.Diff = string(b)
		case k == types.ResultKindGateResult && len(v) == 2 && v[0].Type != v[1].Type && slices.ContainsFunc(v, isReceipt) &&
			slices.ContainsFunc(v, func(r types.Ref) bool { return r.Type == types.RefTypeBlob }):
			rcpt := v[slices.IndexFunc(v, isReceipt)] // by type, in either order (graph.GateShape; Opus 22a-6b L2)
			b, err := include(v[1-slices.IndexFunc(v, isReceipt)], gate.MaxReportBytes)
			if err != nil {
				return PromptContext{}, err
			}
			s, err := gate.SummaryOf(b)
			if err == nil {
				out.GateIDs, err = gate.ReportIDs(b)
			}
			if err != nil {
				return PromptContext{}, err
			}
			in.Gate = review.Gate{Passed: s.Passed, Failed: s.Failed, Skipped: s.Skipped, Green: s.Green}
			out.Omitted = append(out.Omitted, types.ManifestIntentOmittedItem{Ref: rcpt, Reason: types.ManifestIntentOmittedItemReasonPolicy})
		case k == types.ResultKindReviewRequirement && len(v) == 0:
		default:
			return PromptContext{}, fmt.Errorf("%w: review %s's input %d is a %s of %d refs, not what a review renders", ErrUnbound, node, i, k, len(v))
		}
	}
	if len(out.Included) != 3-b2i(empty) || len(rendered.Omitted) != 0 || !sameRefs(rendered.Included, whole) {
		return PromptContext{}, fmt.Errorf("%w: review %s's rendered context is not its brief, patch and gate result", ErrUnbound, node)
	}
	if out.Prompt, err = review.Prompt(in); err != nil {
		return PromptContext{}, fmt.Errorf("%w: %w", ErrUnreviewable, err)
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// claimedResults are the results node's claim took, in its declared
// order, and the inputs' names: every input a result (a review's and a
// build's are).
func claimedResults(tx *eventstore.Tx, node types.NodeID) ([]types.ResultRecorded, []types.Name, error) {
	ins, ok, err := graph.ClaimInputs(tx, node)
	if err != nil || !ok {
		return nil, nil, errors.Join(err, fmt.Errorf("%w: %s is not claimed", ErrUnbound, node))
	}
	var out []types.ResultRecorded
	var names []types.Name
	for _, c := range ins {
		if c.Result == nil {
			return nil, nil, fmt.Errorf("%w: %s's input %s is no result", ErrUnbound, node, c.Name)
		}
		var raw types.ResultRecorded
		b, found, err := tx.Get(graph.ResultProjection, string(*c.Result))
		if err == nil && found {
			err = json.Unmarshal(b, &raw)
		}
		if err != nil || !found {
			return nil, nil, errors.Join(err, fmt.Errorf("%w: %s's input %s names no recorded result", ErrUnbound, node, c.Name))
		}
		out, names = append(out, raw), append(names, c.Name)
	}
	return out, names, nil
}

// isReceipt is whether r is a receipt.
func isReceipt(r types.Ref) bool { return r.Type == types.RefTypeReceipt }

// sameRefs is whether a and b name the same refs, in order.
func sameRefs(a, b []types.Ref) bool {
	return slices.EqualFunc(a, b, func(x, y types.Ref) bool { return x.IdentityDigest == y.IdentityDigest })
}
