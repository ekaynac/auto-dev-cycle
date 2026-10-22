// SPDX-License-Identifier: Apache-2.0

package intent

// A reviewer's rendered context (P22a-6b; I18): what review.Prompt renders
// for a review node, read from the log alone, by the runner that builds
// the reviewer's intent and by the fold that binds it (bind), so the two
// agree byte for byte. The review's claimed inputs, in its declared order:
//   - plan: the task's brief, its title and requirement (brief.Prompt) the
//     prompt's task;
//   - patch: the build's diff, the prompt's candidate change, read to at
//     most review.MaxDiff+1 bytes (Prompt refuses a longer one);
//   - gate: the gate's result, its receipt and its report. Only the
//     report's verdict and counts are rendered (gate.SummaryOf, its bytes
//     checked as gate.CheckReport checks them), never an id (#321 L7): the
//     receipt is in the context (graph.Context renders the result whole)
//     and omitted, reason policy;
//   - required: the review's requirement, which holds no ref.
//
// Included are the brief, the patch and the report, each at its blob's
// size; any other shape of claim, or a context graph.Context renders
// otherwise, is refused.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// ReviewContext is a review node's rendered prompt and its context as the
// reviewer's intent states it (the file's comment).
type ReviewContext struct {
	Prompt   []byte
	Included []types.Ref
	Omitted  []types.ManifestIntentOmittedItem
	Bytes    map[types.Digest]int64 // each included ref's size, by identity digest
}

// Rendered is c as the context intent.Reviewer takes.
func (c ReviewContext) Rendered() graph.Rendered {
	return graph.Rendered{Included: slices.Clone(c.Included), Omitted: slices.Clone(c.Omitted)}
}

// Review is node's ReviewContext in tx, rendered its graph.Context.
func Review(tx *eventstore.Tx, blobs receipt.Blobs, node types.NodeID, rendered graph.Rendered) (ReviewContext, error) {
	ins, ok, err := graph.ClaimInputs(tx, node)
	if err != nil || !ok {
		return ReviewContext{}, errors.Join(err, fmt.Errorf("%w: review %s is not claimed", ErrUnbound, node))
	}
	out := ReviewContext{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{}, Bytes: map[types.Digest]int64{}}
	var whole []types.Ref // the context as graph.Context renders it
	var in review.PromptInput
	include := func(ref types.Ref, limit int64) ([]byte, error) {
		b, err := readAtMost(blobs, ref.ContentDigest, limit)
		out.Included, out.Bytes[ref.IdentityDigest] = append(out.Included, ref), int64(len(b))
		return b, err
	}
	for _, c := range ins {
		if c.Result == nil {
			return ReviewContext{}, fmt.Errorf("%w: review %s's input %s is no result", ErrUnbound, node, c.Name)
		}
		var raw types.ResultRecorded
		b, found, err := tx.Get(graph.ResultProjection, string(*c.Result))
		if err == nil && found {
			err = json.Unmarshal(b, &raw)
		}
		if err != nil || !found {
			return ReviewContext{}, errors.Join(err, fmt.Errorf("%w: review %s's input %s names no recorded result", ErrUnbound, node, c.Name))
		}
		v := raw.Result.Value
		whole = append(whole, v...)
		switch k := raw.Result.Kind; {
		case k == types.ResultKindPlan && len(v) == 1:
			b, err := include(v[0], MaxBrief)
			if err != nil {
				return ReviewContext{}, err
			}
			pb, err := brief.Decode(b)
			if err != nil {
				return ReviewContext{}, err
			}
			in.Requirement = string(brief.Prompt(pb))
		case k == types.ResultKindPatch && len(v) == 1:
			b, err := include(v[0], review.MaxDiff)
			if err != nil {
				return ReviewContext{}, err
			}
			in.Diff = string(b)
		case k == types.ResultKindGateResult && len(v) == 2 && v[0].Type == types.RefTypeReceipt && v[1].Type == types.RefTypeBlob:
			b, err := include(v[1], maxSafeBytes)
			if err != nil {
				return ReviewContext{}, err
			}
			s, err := gate.SummaryOf(b)
			if err != nil {
				return ReviewContext{}, err
			}
			in.Gate = review.Gate{Passed: s.Passed, Failed: s.Failed, Skipped: s.Skipped, Green: s.Green}
			out.Omitted = append(out.Omitted, types.ManifestIntentOmittedItem{Ref: v[0], Reason: types.ManifestIntentOmittedItemReasonPolicy})
		case k == types.ResultKindReviewRequirement && len(v) == 0:
		default:
			return ReviewContext{}, fmt.Errorf("%w: review %s's input %s is a %s of %d refs, not what a review renders", ErrUnbound, node, c.Name, k, len(v))
		}
	}
	if len(out.Included) != 3 || len(rendered.Omitted) != 0 || !sameRefs(rendered.Included, whole) {
		return ReviewContext{}, fmt.Errorf("%w: review %s's rendered context is not its brief, patch and gate result", ErrUnbound, node)
	}
	if out.Prompt, err = review.Prompt(in); err != nil {
		return ReviewContext{}, err
	}
	return out, nil
}

// sameRefs is whether a and b name the same refs, in order.
func sameRefs(a, b []types.Ref) bool {
	return slices.EqualFunc(a, b, func(x, y types.Ref) bool { return x.IdentityDigest == y.IdentityDigest })
}

// readAtMost is blob d's bytes, read to at most limit+1 bytes: over limit
// is an error, as are bytes that are not d's content.
func readAtMost(blobs receipt.Blobs, d types.Digest, limit int64) ([]byte, error) {
	rc, err := blobs.Open(string(d))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err = errors.Join(err, rc.Close()); err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: blob %s is over %d bytes", ErrUnbound, d, limit)
	}
	if sum := sha256.Sum256(b); types.Digest(fmt.Sprintf("sha256:%x", sum)) != d {
		return nil, fmt.Errorf("%w: blob %s holds other bytes", ErrUnbound, d)
	}
	return b, nil
}
