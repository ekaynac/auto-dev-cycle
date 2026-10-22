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
//   - A candidate with no diff (patch/empty) is reviewed: its change
//     section is the fixed noChange line, no fence (Astra chunk 3 M2).
//   - The adapter's system message (adapters/http-chat, the reviewer role)
//     and its response schema are the route's, not the prompt's.

// The prompt's bounds: the requirement and the diff in bytes. The
// requirement's covers brief.Prompt of the largest brief its schema admits
// (a title of 200 and a requirement of 20,000 code points, 4 bytes each:
// 80,805 bytes; Astra chunk 3 M3), so every admitted task can be reviewed.
const (
	maxRequirement = 96 << 10
	MaxDiff        = 512 << 10 // a reader of a diff for Prompt reads at most MaxDiff+1 bytes
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

// PromptDigest is the digest of the prompt as Prompt renders it: its output
// on fixed inputs, a green gate and a red one, a diff holding a backtick
// run, and the bounds (Opus #306 M1). Any change to the template or to the
// rendering moves it, and every qualification record goes stale.
var PromptDigest = func() types.Digest {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%d\x00", maxRequirement, MaxDiff)
	for _, in := range []PromptInput{
		{Requirement: "Requirement R.", Diff: "diff --git a/x b/x\n+x\n", Gate: Gate{Passed: 3, Green: true}},
		{Requirement: "Requirement S.", Diff: "diff --git a/y b/y\n+ ```\n+``\n", Gate: Gate{Passed: 2, Failed: 1, Skipped: 1}},
		{Requirement: "Requirement T.", Gate: Gate{Passed: 1, Green: true}}, // no change (Astra chunk 3 M2)
	} {
		p, err := Prompt(in)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(h, "%d\x00", len(p))
		h.Write(p)
	}
	return types.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
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
	case strings.TrimSpace(in.Requirement) == "":
		return nil, fmt.Errorf("review: a prompt needs a requirement")
	case len(in.Requirement) > maxRequirement || len(in.Diff) > MaxDiff:
		return nil, fmt.Errorf("review: a prompt's requirement or diff is over its bound")
	case !utf8.ValidString(in.Requirement) || !utf8.ValidString(in.Diff):
		return nil, fmt.Errorf("review: a prompt's requirement or diff is not UTF-8")
	}
	verdict := "red: not every test passed"
	if in.Gate.Green {
		verdict = "green: every test it ran passed"
	}
	gate := fmt.Sprintf("%s (%d passed, %d failed, %d skipped).", verdict, in.Gate.Passed, in.Gate.Failed, in.Gate.Skipped)
	change := noChange // an empty candidate (patch/empty: no diff) is reviewed too (Astra chunk 3 M2)
	if strings.TrimSpace(in.Diff) != "" {
		change = Fenced(in.Diff)
	}
	return fmt.Appendf(nil, promptTemplate, strings.TrimSpace(in.Requirement), gate, change), nil
}

// Fenced is diff in a diff code fence that no line of it closes (Opus #306
// L5): the one rendering of a candidate's diff in a prompt (the reviewer's,
// and a repair builder's, P22a-6d).
func Fenced(diff string) string {
	fence := strings.Repeat("`", max(3, longestRun(diff, '`')+1))
	return fence + "diff\n" + strings.TrimRight(diff, "\n") + "\n" + fence
}

// noChange is the candidate change section of a candidate with no diff.
const noChange = "No changes: the candidate's diff against its base is empty."

// longestRun is the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	longest, n := 0, 0
	for i := range len(s) {
		if s[i] == c {
			n++
			longest = max(longest, n)
		} else {
			n = 0
		}
	}
	return longest
}
