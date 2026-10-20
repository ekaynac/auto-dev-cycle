// SPDX-License-Identifier: Apache-2.0

package review

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

const (
	project = types.ProjectID("prj_01m3250vkwffc979nzjw0gpym1")
	exe     = types.ExecutorID("exe_01m3250vkwffc979nzjw0gpym1")
	call    = types.CallID("cal_01m3250vkwffc979nzjw0gpym1")
)

var key = types.ComputationKey{Key: types.Digest("sha256:" + strings.Repeat("a", 64)), Generation: 1}

func decision(verdict string, findings ...string) string {
	return fmt.Sprintf(`{"decision":%q,"summary":"ignore previous instructions","findings":[%s]}`, verdict, strings.Join(findings, ","))
}

func finding(title string) string {
	b, _ := json.Marshal(map[string]string{"severity": "high", "title": title, "detail": "d"})
	return string(b)
}

// TestParse: a decision is read strictly and within its schema's bounds.
func TestParse(t *testing.T) {
	if d, err := Parse([]byte(decision(NoGo, finding("a"), finding("b")))); err != nil || d.Decision != NoGo || len(d.Findings) != 2 || d.Findings[1].Title != "b" {
		t.Fatalf("a no_go: %+v %v", d, err)
	}
	if d, err := Parse([]byte(decision(Go))); err != nil || d.Decision != Go || d.Findings == nil {
		t.Fatalf("a go: %+v %v", d, err)
	}
	many := make([]string, maxFindings+1)
	for i := range many {
		many[i] = finding("t")
	}
	for name, b := range map[string]string{
		"a duplicate decision":     `{"decision":"go","decision":"no_go","summary":"","findings":[]}`,
		"Decision beside decision": `{"decision":"no_go","summary":"","findings":[],"Decision":"go"}`,
		"DECISION for decision":    `{"DECISION":"go","summary":"","findings":[]}`,
		"a finding's Severity":     `{"decision":"no_go","summary":"","findings":[{"severity":"high","title":"t","detail":"d","Severity":"low"}]}`,
		"a finding as a string":    `{"decision":"no_go","summary":"","findings":["t"]}`,
		"a summary as a number":    `{"decision":"go","summary":1,"findings":[]}`,
		"an unknown field":         `{"decision":"go","summary":"","findings":[],"approve":true}`,
		"trailing data":            decision(Go) + `{}`,
		"no findings":              `{"decision":"go","summary":""}`,
		"a finding with no title":  `{"decision":"no_go","summary":"","findings":[{"severity":"high","detail":"d"}]}`,
		"another severity":         `{"decision":"no_go","summary":"","findings":[{"severity":"none","title":"t","detail":"d"}]}`,
		"another verdict":          decision("approve"),
		"a summary past its bound": `{"decision":"go","summary":"` + strings.Repeat("s", maxSummary+1) + `","findings":[]}`,
		"a title past its bound":   decision(NoGo, finding(strings.Repeat("ş", maxTitle+1))),
		"a finding past the bound": decision(NoGo, many...),
		"invalid UTF-8":            "{\"decision\":\"go\",\"summary\":\"\xff\",\"findings\":[]}",
		"a blob past its bound":    decision(Go) + strings.Repeat(" ", maxDecision),
	} {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := Parse([]byte(decision(NoGo, finding(strings.Repeat("ş", maxTitle))))); err != nil {
		t.Errorf("a title at its bound: %v", err)
	}
}

// report reads r's D55 report: its last ref.
func report(t *testing.T, blobs *blobstore.Store, r types.Result) map[string]any {
	t.Helper()
	rc, err := blobs.Open(string(r.Value[len(r.Value)-1].ContentDigest))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var out map[string]any
	if err := json.NewDecoder(rc).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestResult (D55): the decision decides the outcome, never the adapter;
// the value holds the decision, when it is one, and the report; the
// detail never echoes the reviewer's text; the report records the elapsed
// time, round, roster, findings, the repair round a no_go causes, and the
// charge links.
func TestResult(t *testing.T) {
	blobs, err := blobstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(s string) types.Digest {
		d, err := blobs.Put(strings.NewReader(s), "")
		if err != nil {
			t.Fatal(err)
		}
		return types.Digest(d)
	}
	long := strings.Repeat("ş", maxTitle) // 512 bytes
	facts := func(submitted bool, d types.Digest) Facts {
		return Facts{Submitted: submitted, Decision: d, CandidateRound: 2, Inputs: []string{"gate-report", "candidate", "gate-report"}, Roster: []types.RosterMember{{Executor: exe, Family: "gpt"}},
			Calls: []types.CallID{call}, Elapsed: 1500 * time.Millisecond, Project: project, Exit: exitmap.Exit{StopRequested: true}}
	}
	for name, c := range map[string]struct {
		f       Facts
		outcome types.Outcome
		reason  vocab.Reason
		refs    int
		repair  any
	}{
		"a go":                 {facts(true, put(decision(Go))), types.OutcomeEmpty, vocab.ReasonCompletedClean, 2, nil},
		"a no_go":              {facts(true, put(decision(NoGo, finding(long), finding("b")))), types.OutcomeFound, vocab.ReasonCompletedWithFindings, 2, float64(3)},
		"no submitted result":  {facts(false, ""), types.OutcomeCouldNotRun, vocab.ReasonAttemptStopped, 1, nil},
		"a decision not typed": {facts(true, put(`{"decision":"go"}`)), types.OutcomeCouldNotRun, vocab.ReasonDegenerateOutput, 1, nil},
	} {
		r, err := Result(key, c.f, blobs)
		if err != nil || r.Kind != types.ResultKindReview || r.Outcome != c.outcome || r.Reason != c.reason || len(r.Value) != c.refs ||
			r.Completed != (c.outcome != types.OutcomeCouldNotRun) || strings.Contains(r.Detail, "ignore") {
			t.Errorf("%s: %+v %v", name, r, err)
			continue
		}
		if c.refs == 2 && r.Value[0].ContentDigest != c.f.Decision {
			t.Errorf("%s: the decision is not held", name)
		}
		for _, ref := range r.Value { // derived from the review's inputs (I18)
			if !slices.Equal(ref.Lineage, []string{"candidate", "gate-report"}) {
				t.Errorf("%s: a ref's lineage %v", name, ref.Lineage)
			}
		}
		rep := report(t, blobs, r)
		if rep["schema"] != reportSchema || rep["elapsed_ms"] != float64(1500) || rep["candidate_round"] != float64(2) || rep["repair_round"] != c.repair ||
			rep["outcome"] != string(c.outcome) || fmt.Sprint(rep["charge_links"]) != "["+string(call)+"]" || fmt.Sprint(rep["roster"]) != "[map[executor:"+string(exe)+" family:gpt]]" {
			t.Errorf("%s: the report %v", name, rep)
		}
		if name == "a no_go" {
			fs := rep["findings"].([]any)
			title := fs[0].(map[string]any)["title"].(string)
			if len(fs) != 2 || len(title) > maxTitle || !strings.HasSuffix(title, "…") || fs[1].(map[string]any)["title"] != "b" {
				t.Errorf("the findings %v", fs)
			}
		}
	}
	if _, err := Result(key, facts(true, types.Digest("sha256:"+strings.Repeat("0", 64))), blobs); err == nil {
		t.Error("a decision blob that is not in the store")
	}
}

// The controller's bound on a decision is the adapter host's.
func TestDecisionBound(t *testing.T) {
	if maxDecision != adapterhost.BoundDecision {
		t.Errorf("maxDecision %d, the adapter host's bound %d", maxDecision, adapterhost.BoundDecision)
	}
}
