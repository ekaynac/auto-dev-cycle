// SPDX-License-Identifier: Apache-2.0

package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// The reviewer's prompt (P31; D56): what a review node shows its reviewer,
// and what reviewer qualification qualifies. P22a renders the review node's
// input with Prompt, so that a qualified route is qualified for the prompt
// it runs with: the qualification record binds PromptDigest.
//
// Decisions (the lead, 2026-10-20):
//   - The prompt holds the task's requirement, the candidate's diff and the
//     gate's summary (counts and verdict), and nothing else: never the
//     task's acceptance tests or any other restricted attachment (I18),
//     which no PromptInput field can carry.
//   - The template is fixed; PromptDigest covers it and the bounds, so a
//     change to either is another prompt.
//   - The adapter's system message (adapters/http-chat, the reviewer role)
//     and its response schema are the route's, not the prompt's.

// The prompt's bounds: the requirement and the diff in bytes.
const (
	maxRequirement = 64 << 10
	maxDiff        = 512 << 10
)

const promptTemplate = `You are reviewing one candidate change to a repository for the task below. The task's own acceptance tests are withheld from you; the repository's tests ran on the candidate, with the result under "Gate".

Decide "go" only when the change meets every part of the task and introduces no defect: a wrong result, an input it fails on (a panic included), an edge case it gets wrong, or a test it deletes or weakens. Decide "no_go" otherwise, with one finding per problem.

## Task

%s

## Gate

%s

## Candidate change (unified diff)

%s
`

// PromptDigest is the digest of the prompt's template and bounds.
var PromptDigest = func() types.Digest {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d", promptTemplate, maxRequirement, maxDiff))
	return types.Digest("sha256:" + hex.EncodeToString(sum[:]))
}()

// Gate is the gate's summary a reviewer sees: its verdict and its counts.
type Gate struct {
	Passed, Failed, Skipped int
	Green                   bool // the gate's verdict: every test it ran passed
}

// PromptInput is everything a reviewer's prompt holds.
type PromptInput struct {
	Requirement string // the task's requirement, as approved
	Diff        string // the candidate's unified diff against its base
	Gate        Gate
}

// Prompt is the reviewer's prompt for in: valid UTF-8 within its bounds.
func Prompt(in PromptInput) ([]byte, error) {
	switch {
	case strings.TrimSpace(in.Requirement) == "" || strings.TrimSpace(in.Diff) == "":
		return nil, fmt.Errorf("review: a prompt needs a requirement and a diff")
	case len(in.Requirement) > maxRequirement || len(in.Diff) > maxDiff:
		return nil, fmt.Errorf("review: a prompt's requirement or diff is over its bound")
	case !utf8.ValidString(in.Requirement) || !utf8.ValidString(in.Diff):
		return nil, fmt.Errorf("review: a prompt's requirement or diff is not UTF-8")
	}
	verdict := "red: not every test passed"
	if in.Gate.Green {
		verdict = "green: every test it ran passed"
	}
	gate := fmt.Sprintf("%s (%d passed, %d failed, %d skipped).", verdict, in.Gate.Passed, in.Gate.Failed, in.Gate.Skipped)
	return fmt.Appendf(nil, promptTemplate, strings.TrimSpace(in.Requirement), gate, "```diff\n"+strings.TrimRight(in.Diff, "\n")+"\n```"), nil
}
