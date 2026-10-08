// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

const prjID = "prj_01m3250vkwffc979nzjw0g2sf9"

var diff = types.Digest("sha256:" + strings.Repeat("d", 64))

// The build outcome table (13 "Facts, not claims"), row by row, with the
// reason from the exit map whatever the adapter reported.
func TestBuildOutcomeTable(t *testing.T) {
	k := types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}
	credit := exitmap.Exit{Hints: []string{"http_5xx", "credit_exhausted"}}
	for name, c := range map[string]struct {
		facts   BuildFacts
		kind    types.ResultKind
		outcome types.Outcome
		reason  vocab.Reason
		value   bool
	}{
		"completed with a diff":       {BuildFacts{Completed: true, Diff: diff}, types.ResultKindPatch, types.OutcomeFound, vocab.ReasonCompletedWithFindings, true},
		"completed with no diff":      {BuildFacts{Completed: true}, types.ResultKindPatch, types.OutcomeEmpty, vocab.ReasonCompletedClean, false},
		"a 402 after edits":           {BuildFacts{Diff: diff, Exit: credit}, types.ResultKindPartialPatch, types.OutcomeCouldNotRun, vocab.ReasonBrokerCreditExhausted, true},
		"a 402 before the first edit": {BuildFacts{Exit: credit}, types.ResultKindPatch, types.OutcomeCouldNotRun, vocab.ReasonBrokerCreditExhausted, false},
		"incomplete with no cause":    {BuildFacts{}, types.ResultKindPatch, types.OutcomeCouldNotRun, vocab.ReasonExecutorIncomplete, false},
		"stopped after edits":         {BuildFacts{Diff: diff, Exit: exitmap.Exit{StopRequested: true}}, types.ResultKindPartialPatch, types.OutcomeCouldNotRun, vocab.ReasonAttemptStopped, true},
	} {
		c.facts.Project = prjID
		r, err := Build(k, c.facts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Kind != c.kind || r.Outcome != c.outcome || r.Reason != c.reason || r.Completed != c.facts.Completed || r.Computation != k.Key {
			t.Errorf("%s: %s %s/%s completed=%t", name, r.Kind, r.Outcome, r.Reason, r.Completed)
		}
		if got := len(r.Value) == 1 && r.Value[0].ContentDigest == diff; got != c.value || (!c.value && len(r.Value) != 0) {
			t.Errorf("%s: value %+v", name, r.Value)
		}
		if err := Check(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A result's identity is its own, never the key's: two builds of one key
// are two results.
func TestResultIdentityIsNotTheKey(t *testing.T) {
	k := types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}
	a, err := Build(k, BuildFacts{Completed: true, Diff: diff, Project: prjID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(k, BuildFacts{Completed: true, Diff: diff, Project: prjID})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Computation != b.Computation || !strings.HasPrefix(string(a.ID), "res_") {
		t.Fatalf("%s, %s", a.ID, b.ID)
	}
}
