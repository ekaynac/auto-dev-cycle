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
//     listed under "The review of your round n candidate (findings-n)"
//     (Opus 22a-6d L2), included at its blob's size; an unreviewable verdict (graph.Unreviewable: no view)
//     its detail, the controller's text;
//   - its candidate-n: round n's candidate's diff (review.Fenced), under
//     "Your round n candidate", included at its blob's size (once: a diff a
//     later round repeated is named as the same), or omitted for budget past
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
	results, names, err := claimedResults(tx, node)
	if err != nil {
		return PromptContext{}, err
	}
	out := PromptContext{Included: []types.Ref{}, Omitted: []types.ManifestIntentOmittedItem{}, Bytes: map[types.Digest]int64{}}
	listed := map[types.Digest]string{} // each ref listed once, under the first input that takes it (a candidate repeated by rounds)
	var prompt strings.Builder
	var whole []types.Ref // the context as graph.Context renders it
	for i, rr := range results {
		v := rr.Result.Value
		if rr.Result.Kind == types.ResultKindReview { // a build's context renders a review's builder-safe refs alone (graph.Context)
			v = slices.DeleteFunc(slices.Clone(v), func(r types.Ref) bool { return !graph.BuilderSafe(types.NodeKindReview, rr, r) })
		}
		whole = append(whole, v...)
		round := roundOf(names[i]) // its section's round (Opus 22a-6d L2)
		switch k := rr.Result.Kind; {
		case i == 0 && k == types.ResultKindPlan && len(v) == 1:
			b, ok, err := readBlob(blobs, v[0].ContentDigest, MaxBrief)
			if err == nil && !ok {
				err = fmt.Errorf("%w: the brief is over %d bytes", ErrUnbound, MaxBrief)
			}
			if err != nil {
				return PromptContext{}, err
			}
			pb, err := brief.Decode(b)
			if err != nil { // its bytes are its digest's (readBlob): no brief, the content's (Astra chunk 4b M1)
				return PromptContext{}, fmt.Errorf("%w: %s: %w", ErrNotTheBrief, v[0].ID, err)
			}
			p := brief.Prompt(pb)
			prompt.Write(p)
			out.Included, out.Bytes[v[0].IdentityDigest] = append(out.Included, v[0]), int64(len(p))
			listed[v[0].IdentityDigest] = string(names[i])
		case i > 0 && k == types.ResultKindReview:
			fmt.Fprintf(&prompt, "\n## The review of your round %s candidate (%s)\n\n", round, names[i])
			switch {
			case graph.Unreviewable(rr.Result) && len(v) == 0:
				prompt.WriteString("It could not be reviewed: " + rr.Result.Detail + "\n")
			case len(v) == 1 && v[0].Type == types.RefTypeView:
				b, ok, err := readBlob(blobs, v[0].ContentDigest, maxSafeBytes)
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
				if first, dup := listed[v[0].IdentityDigest]; dup {
					return PromptContext{}, fmt.Errorf("%w: %s's findings view is %s's", ErrUnbound, names[i], first) // a view names its round: never two rounds'
				}
				out.Included, out.Bytes[v[0].IdentityDigest] = append(out.Included, v[0]), int64(len(b))
				listed[v[0].IdentityDigest] = string(names[i])
			default:
				return PromptContext{}, fmt.Errorf("%w: build %s's findings are %d refs, not a findings view", ErrUnbound, node, len(v))
			}
		case i > 0 && k == types.ResultKindPatch && len(v) == 0:
			fmt.Fprintf(&prompt, "\n## Your round %s candidate (%s)\n\nIt changed nothing.\n", round, names[i])
		case i > 0 && k == types.ResultKindPatch && len(v) == 1:
			if first, dup := listed[v[0].IdentityDigest]; dup { // the same diff again: listed once (Opus 22a-6d H1's identical patch)
				fmt.Fprintf(&prompt, "\n## Your round %s candidate (%s)\n\nIts diff is the same as %s's.\n", round, names[i], first)
				continue
			}
			listed[v[0].IdentityDigest] = string(names[i])
			b, ok, err := readBlob(blobs, v[0].ContentDigest, review.MaxDiff)
			if err != nil {
				return PromptContext{}, err
			}
			if !ok {
				fmt.Fprintf(&prompt, "\n## Your round %s candidate (%s)\n\nIts diff is over %d bytes: not shown.\n", round, names[i], review.MaxDiff)
				out.Omitted = append(out.Omitted, types.ManifestIntentOmittedItem{Ref: v[0], Reason: types.ManifestIntentOmittedItemReasonBudget})
				continue
			}
			fmt.Fprintf(&prompt, "\n## Your round %s candidate (%s, unified diff)\n\n%s\n", round, names[i], review.Fenced(string(b)))
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

// roundOf is the round an input named findings-<n> or candidate-<n> is of
// (graph.Repair's names), its name when it is neither.
func roundOf(name types.Name) string {
	for _, p := range []string{"findings-", "candidate-"} {
		if n, ok := strings.CutPrefix(string(name), p); ok {
			return n
		}
	}
	return string(name)
}
