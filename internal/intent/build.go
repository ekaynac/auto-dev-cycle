// SPDX-License-Identifier: Apache-2.0

package intent

// A builder's rendered context (P22a-5c-2b-1; P22a-6d for a repair round):
// what a build's prompt renders, read from the log alone, by the runner
// that builds the builder's intent and by the fold that binds it, so the
// two agree byte for byte. The build's claimed inputs, in declared order:
//   - plan: the task's brief (brief.Prompt), included at that prompt's
//     size (the first round's prompt is it alone, as before);
//   - a repair round's findings-n: the review's builder-safe findings view
//     (graph.Context renders a review's result for a build as it alone),
//     listed under "Review of your previous candidate", included at its
//     blob's size; an unreviewable verdict (graph.Unreviewable: no view)
//     its detail, the controller's text;
//   - its candidate-n: the previous candidate's diff (review.Fenced),
//     included at its blob's size, or omitted for budget past
//     review.MaxDiff (shortcut: the repair builder then sees the
//     unreviewable verdict, not its diff; a bound for it is 22a-7's), or
//     "changed nothing" for an empty candidate.
//
// Any other shape of claim, or a context graph.Context renders otherwise,
// is refused.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

// Build is build node's PromptContext in tx, rendered its graph.Context.
func Build(tx *eventstore.Tx, blobs receipt.Blobs, node types.NodeID, rendered graph.Rendered) (PromptContext, error) {
	results, err := claimedResults(tx, node)
	if err != nil {
		return PromptContext{}, err
	}
	out := PromptContext{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{}, Bytes: map[types.Digest]int64{}}
	read := func(ref types.Ref, limit int64) ([]byte, bool, error) {
		b, ok, err := readBlob(blobs, ref.ContentDigest, limit)
		if err == nil && b == nil {
			err = fmt.Errorf("%w: blob %s holds other bytes", ErrUnbound, ref.ContentDigest)
		}
		return b, ok, err
	}
	var prompt strings.Builder
	var whole []types.Ref // the context as graph.Context renders it
	for i, rr := range results {
		v := rr.Result.Value
		if rr.Result.Kind == types.ResultKindReview { // a build's context renders a review's builder-safe refs alone (graph.Context)
			v = slices.DeleteFunc(slices.Clone(v), func(r types.Ref) bool { return !graph.BuilderSafe(types.NodeKindReview, rr, r) })
		}
		whole = append(whole, v...)
		switch k := rr.Result.Kind; {
		case i == 0 && k == types.ResultKindPlan && len(v) == 1:
			b, ok, err := read(v[0], MaxBrief)
			if err == nil && !ok {
				err = fmt.Errorf("%w: the brief is over %d bytes", ErrUnbound, MaxBrief)
			}
			if err != nil {
				return PromptContext{}, err
			}
			pb, err := brief.Decode(b)
			if err != nil {
				return PromptContext{}, err
			}
			p := brief.Prompt(pb)
			prompt.Write(p)
			out.Included, out.Bytes[v[0].IdentityDigest] = append(out.Included, v[0]), int64(len(p))
		case i > 0 && k == types.ResultKindReview:
			prompt.WriteString("\n## Review of your previous candidate\n\n")
			switch {
			case graph.Unreviewable(rr.Result) && len(v) == 0:
				prompt.WriteString("It could not be reviewed: " + rr.Result.Detail + "\n")
			case len(v) == 1 && v[0].Type == types.RefTypeView:
				b, ok, err := read(v[0], maxSafeBytes)
				if err == nil && !ok {
					err = fmt.Errorf("%w: the findings view is over %d bytes", ErrUnbound, maxSafeBytes)
				}
				var list string
				if err == nil {
					list, err = review.ViewMarkdown(b)
				}
				if err != nil {
					return PromptContext{}, err
				}
				prompt.WriteString("Its reviewer decided no_go, with these findings:\n\n" + list)
				out.Included, out.Bytes[v[0].IdentityDigest] = append(out.Included, v[0]), int64(len(b))
			default:
				return PromptContext{}, fmt.Errorf("%w: build %s's findings are %d refs, not a findings view", ErrUnbound, node, len(v))
			}
		case i > 0 && k == types.ResultKindPatch && len(v) == 0:
			prompt.WriteString("\n## Your previous candidate\n\nIt changed nothing.\n")
		case i > 0 && k == types.ResultKindPatch && len(v) == 1:
			b, ok, err := read(v[0], review.MaxDiff)
			if err != nil {
				return PromptContext{}, err
			}
			if !ok {
				prompt.WriteString(fmt.Sprintf("\n## Your previous candidate\n\nIts diff is over %d bytes: not shown.\n", review.MaxDiff))
				out.Omitted = append(out.Omitted, types.ManifestIntentOmittedItem{Ref: v[0], Reason: types.ManifestIntentOmittedItemReasonBudget})
				continue
			}
			prompt.WriteString("\n## Your previous candidate (unified diff)\n\n" + review.Fenced(string(b)) + "\n")
			out.Included, out.Bytes[v[0].IdentityDigest] = append(out.Included, v[0]), int64(len(b))
		default:
			return PromptContext{}, fmt.Errorf("%w: build %s's input %d is a %s of %d refs, not what a build renders", ErrUnbound, node, i, k, len(v))
		}
	}
	if len(out.Included) == 0 || len(rendered.Omitted) != 0 || !sameRefs(rendered.Included, whole) {
		return PromptContext{}, fmt.Errorf("%w: build %s's rendered context is not its brief and its repairs' findings and candidates", ErrUnbound, node)
	}
	out.Prompt = []byte(prompt.String())
	if int64(len(out.Prompt)) > MaxPrompt {
		return PromptContext{}, fmt.Errorf("%w: the prompt is over %d bytes", ErrUnbound, MaxPrompt)
	}
	return out, nil
}
