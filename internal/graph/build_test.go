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
		detail  string
	}{
		"found with a diff": {BuildFacts{Submitted: true, Reported: types.OutcomeFound, Diff: diff},
			types.ResultKindPatch, types.OutcomeFound, vocab.ReasonCompletedWithFindings, true, "the build produced a change"},
		"empty with a diff": {BuildFacts{Submitted: true, Reported: types.OutcomeEmpty, Diff: diff},
			types.ResultKindPatch, types.OutcomeFound, vocab.ReasonCompletedWithFindings, true, "the build produced a change"},
		"found with no diff": {BuildFacts{Submitted: true, Reported: types.OutcomeFound},
			types.ResultKindPatch, types.OutcomeEmpty, vocab.ReasonCompletedClean, false, ""},
		"a 402 after edits": {BuildFacts{Submitted: true, Reported: types.OutcomeCouldNotRun, Diff: diff, Exit: credit},
			types.ResultKindPartialPatch, types.OutcomeCouldNotRun, vocab.ReasonBrokerCreditExhausted, true, "the executor reported could_not_run; its partial change is sealed"},
		"a 402 before the first edit": {BuildFacts{Submitted: true, Reported: types.OutcomeCouldNotRun, Exit: credit},
			types.ResultKindPatch, types.OutcomeCouldNotRun, vocab.ReasonBrokerCreditExhausted, false, "the executor reported could_not_run; nothing changed"},
		"a reported failure after edits": {BuildFacts{Submitted: true, Reported: types.OutcomeFailed, Diff: diff},
			types.ResultKindPartialPatch, types.OutcomeCouldNotRun, vocab.ReasonExecutorIncomplete, true, "the executor reported failed; its partial change is sealed"},
		"no accepted result": {BuildFacts{Reported: types.OutcomeFound},
			types.ResultKindPatch, types.OutcomeCouldNotRun, vocab.ReasonExecutorIncomplete, false, "no accepted result; nothing changed"},
		"stopped after edits": {BuildFacts{Diff: diff, Exit: exitmap.Exit{StopRequested: true}},
			types.ResultKindPartialPatch, types.OutcomeCouldNotRun, vocab.ReasonAttemptStopped, true, "no accepted result; its partial change is sealed"},
	} {
		c.facts.Project = prjID
		r, err := Build(k, c.facts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		done := c.outcome != types.OutcomeCouldNotRun
		if r.Kind != c.kind || r.Outcome != c.outcome || r.Reason != c.reason || r.Completed != done || r.Computation != k.Key || r.Detail != c.detail {
			t.Errorf("%s: %s %s/%s completed=%t %q", name, r.Kind, r.Outcome, r.Reason, r.Completed, r.Detail)
		}
		if got := len(r.Value) == 1 && r.Value[0].ContentDigest == diff; got != c.value || (!c.value && len(r.Value) != 0) {
			t.Errorf("%s: value %+v", name, r.Value)
		}
		if err := Check(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Check refuses a reason that does not fit the outcome, which the schema
// alone accepts.
func TestCheckFitsTheReason(t *testing.T) {
	k := types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}
	r, err := Build(k, BuildFacts{Submitted: true, Reported: types.OutcomeFound, Project: prjID})
	if err != nil {
		t.Fatal(err)
	}
	r.Reason = vocab.ReasonBrokerCreditExhausted
	if err := Check(r); err == nil {
		t.Fatal("empty with broker_credit_exhausted passed")
	}
}

// A result's identity is its own, never the key's: two builds of one key
// are two results.
func TestResultIdentityIsNotTheKey(t *testing.T) {
	k := types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}
	f := BuildFacts{Submitted: true, Reported: types.OutcomeFound, Diff: diff, Project: prjID}
	a, err := Build(k, f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(k, f)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Computation != b.Computation || !strings.HasPrefix(string(a.ID), "res_") {
		t.Fatalf("%s, %s", a.ID, b.ID)
	}
}
