// SPDX-License-Identifier: Apache-2.0

// Package review computes a review node's result and its D55 report from
// the reviewer's typed decision (P16b).
//
// Decisions:
//   - The decision is the fact: the reviewer's typed output
//     (adapters/http-chat's defaultDecisionSchema: go or no_go, a summary,
//     findings), admitted to the blob store by the adapter host. The
//     controller reads it again strictly (contract.ReadStrict: no
//     duplicate key, valid UTF-8, no trailing data; exactly the schema's
//     keys, in their case) and within the schema's bounds; it never trusts
//     the adapter's check.
//   - The outcome table: go → empty/completed_clean; no_go →
//     found/completed_with_findings, which a repair round takes
//     (graph.Repair); no submitted decision → could_not_run with the exit
//     map's reason; a decision that is not one → could_not_run/
//     degenerate_output. The adapter's own outcome never decides.
//   - The result's value is the decision, when it is one, a no_go's
//     findings view (below: the repair builder's findings input), and the
//     D55 report, last, each with the review's
//     rendered input ids as its lineage: the decision derives from what
//     the reviewer saw, the gate report included (I18). The detail never
//     echoes the reviewer's text.
//   - A no_go's result also holds the builder-safe findings view (#226
//     entry 94(2), the operator's decision; 22a-6L): each finding's
//     severity and title only, every gate id of at least minRedacted
//     bytes (Facts.GateIDs) in a title replaced by "[test]", as a view
//     ref whose lineage is the decision's (truthful): graph.Records marks
//     it BuilderSafe, so a repair builder takes it where the decision,
//     derived from the declassified gate report, would refuse its intent.
//   - D55: the report records the elapsed time, the charge links (the
//     reviewer's model call ids), the roster, the verdict, each finding's
//     severity and title (cut to maxTitle bytes), and the reviewed
//     candidate's round, and for a no_go the repair round it causes
//     (that round + 1). P30's ledger reads it.
package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

const (
	reportSchema = "adc.review-report/v1"
	maxSummary   = 4096 // characters, as the decision schema bounds them
	maxFindings  = 100
	maxTitle     = 256
	maxDetail    = 4096
	maxDecision  = 1 << 20 // bytes: the adapter host's bound on a decision (adapterhost.BoundDecision)
	viewSchema   = "adc.findings-view/v1"
	minRedacted  = 4 // bytes: a shorter gate id is no channel, and redacting it would garble titles
)

// The decision's verdicts.
const (
	Go   = "go"
	NoGo = "no_go"
)

var severities = []string{"critical", "high", "medium", "low"}

// Blobs is the blob store as a review's result needs it.
type Blobs interface {
	receipt.Blobs
	Put(r io.Reader, declared string) (string, error)
}

// Facts are what a review's result is computed from, all observed by the
// controller.
type Facts struct {
	// Submitted: the attempt's result was accepted (attempt.result_submitted),
	// and Decision is its admitted blob's digest.
	Submitted bool
	Decision  types.Digest
	// Exit is what the controller knows of a reviewer that did not complete.
	Exit exitmap.Exit
	// CandidateRound is the round of the build whose candidate was
	// reviewed (graph.Repair's n), not the review's own round: a review of
	// a repaired build is a new node, always round 1 (Opus #291 L1).
	CandidateRound int64
	Roster         []types.RosterMember
	Calls          []types.CallID // the reviewer's model calls: the charge links
	Elapsed        time.Duration
	Project        types.ProjectID
	// Inputs are the ids of the refs the review's claim rendered: the
	// lineage of its decision and report, which derive from them (I18;
	// Opus #291 M2).
	Inputs []string
	// GateIDs are the ids of the gate report the claim rendered
	// (gate.ReportIDs): kept out of the findings view.
	GateIDs []string
}

// Finding is one finding of a decision.
type Finding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// Decision is a reviewer's typed decision.
type Decision struct {
	Decision string    `json:"decision"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Parse reads b as a decision, strictly and within its bounds: from the
// tree contract.ReadStrict reads (no duplicate key, valid UTF-8, no
// trailing data), whose objects must have exactly the schema's keys, in
// their case (Opus #291 H1: a decoder matching keys in any case read
// "Decision" beside "decision").
func Parse(b []byte) (Decision, error) {
	if len(b) > maxDecision {
		return Decision{}, fmt.Errorf("review: a decision of over %d bytes", maxDecision)
	}
	v, err := contract.ReadStrict(b)
	if err != nil {
		return Decision{}, err
	}
	top, ok := exactly(v, "decision", "summary", "findings")
	if !ok {
		return Decision{}, errors.New("review: a decision has exactly a decision, a summary and findings")
	}
	verdict, ok1 := top["decision"].(string)
	summary, ok2 := top["summary"].(string)
	findings, ok3 := top["findings"].([]any)
	switch {
	case !ok1 || !ok2 || !ok3:
		return Decision{}, errors.New("review: a decision's fields are not of their types")
	case verdict != Go && verdict != NoGo:
		return Decision{}, errors.New("review: the decision is go or no_go")
	case utf8.RuneCountInString(summary) > maxSummary || len(findings) > maxFindings:
		return Decision{}, errors.New("review: the decision is past its bounds")
	}
	d := Decision{Decision: verdict, Summary: summary, Findings: []Finding{}}
	for _, x := range findings {
		f, ok := exactly(x, "severity", "title", "detail")
		severity, ok1 := f["severity"].(string)
		title, ok2 := f["title"].(string)
		detail, ok3 := f["detail"].(string)
		if !ok || !ok1 || !ok2 || !ok3 || !slices.Contains(severities, severity) ||
			utf8.RuneCountInString(title) > maxTitle || utf8.RuneCountInString(detail) > maxDetail {
			return Decision{}, errors.New("review: a finding is not one")
		}
		d.Findings = append(d.Findings, Finding{severity, title, detail})
	}
	return d, nil
}

// exactly is v as an object when its keys are exactly keys.
func exactly(v any, keys ...string) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(keys) {
		return nil, false
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	return m, true
}

// Result is the review node's result for key under f, publishing its D55
// report in blobs.
func Result(key types.ComputationKey, f Facts, blobs Blobs) (types.Result, error) {
	id, err := ids.New("res")
	if err != nil {
		return types.Result{}, err
	}
	r := types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindReview, Value: []types.Ref{}}
	var d Decision
	switch {
	case !f.Submitted || f.Decision == "":
		r.Outcome, r.Reason, r.Detail = types.OutcomeCouldNotRun, exitmap.Reason(f.Exit), "no accepted decision"
	default:
		b, rerr := read(blobs, f.Decision)
		if rerr != nil {
			return types.Result{}, rerr
		}
		if d, err = Parse(b); err != nil {
			r.Outcome, r.Reason, r.Detail = types.OutcomeCouldNotRun, vocab.ReasonDegenerateOutput, "the decision is not a typed decision"
			d = Decision{}
			break
		}
		ref, serr := seal(f, f.Decision)
		if serr != nil {
			return types.Result{}, serr
		}
		r.Value, r.Completed = append(r.Value, ref), true
		if d.Decision == Go {
			r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
		} else {
			r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, fmt.Sprintf("the reviewer's decision is no_go, with %d findings", len(d.Findings))
			vref, verr := view(f, d, ref.ID, blobs)
			if verr != nil {
				return types.Result{}, verr
			}
			r.Value = append(r.Value, vref)
		}
	}
	report, err := Report(f, r, d)
	if err != nil {
		return types.Result{}, err
	}
	pd, err := blobs.Put(bytes.NewReader(report), "")
	if err != nil {
		return types.Result{}, err
	}
	pref, err := seal(f, types.Digest(pd))
	if err != nil {
		return types.Result{}, err
	}
	r.Value = append(r.Value, pref)
	return r, graph.Check(r)
}

// view publishes d's builder-safe findings view, derived from the decision
// ref decision.
func view(f Facts, d Decision, decision string, blobs Blobs) (types.Ref, error) {
	ids := slices.DeleteFunc(slices.Clone(f.GateIDs), func(id string) bool { return len(id) < minRedacted })
	slices.SortFunc(ids, func(a, b string) int { return len(b) - len(a) }) // the longest first: no id redacted inside another's mark
	type finding struct {
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}
	findings := []finding{}
	for _, x := range d.Findings {
		title := x.Title
		for _, id := range ids {
			title = strings.ReplaceAll(title, id, "[test]")
		}
		findings = append(findings, finding{x.Severity, title})
	}
	b, err := json.Marshal(struct {
		Schema   string    `json:"schema"`
		Findings []finding `json:"findings"`
	}{viewSchema, findings})
	if err != nil {
		return types.Ref{}, err
	}
	vd, err := blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		return types.Ref{}, err
	}
	return knowledge.Seal(types.Ref{Type: types.RefTypeView, ID: vd, Scope: types.RefScope{Project: &f.Project},
		Lineage: []string{decision}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(vd)})
}

// Report is the D55 record of one review node with result r and decision
// d (empty when there is none).
func Report(f Facts, r types.Result, d Decision) ([]byte, error) {
	type finding struct {
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}
	findings := []finding{}
	for _, x := range d.Findings {
		title := x.Title
		if len(title) > maxTitle {
			title = strings.ToValidUTF8(title[:maxTitle-len("…")], "") + "…"
		}
		findings = append(findings, finding{x.Severity, title})
	}
	var repair *int64
	if d.Decision == NoGo {
		next := f.CandidateRound + 1
		repair = &next
	}
	return json.Marshal(struct {
		Schema      string               `json:"schema"`
		ElapsedMS   int64                `json:"elapsed_ms"`
		Round       int64                `json:"candidate_round"`
		Roster      []types.RosterMember `json:"roster"`
		Outcome     types.Outcome        `json:"outcome"`
		Reason      string               `json:"reason"`
		Decision    string               `json:"decision"`
		Findings    []finding            `json:"findings"`
		RepairRound *int64               `json:"repair_round"`
		ChargeLinks []types.CallID       `json:"charge_links"`
	}{reportSchema, f.Elapsed.Milliseconds(), f.CandidateRound, append([]types.RosterMember{}, f.Roster...), r.Outcome, r.Reason.String(), d.Decision,
		findings, repair, append([]types.CallID{}, f.Calls...)})
}

// read is the blob d's bytes, at most maxDecision of them and one more:
// a longer blob is no decision (Parse refuses its cut).
func read(blobs Blobs, d types.Digest) ([]byte, error) {
	rc, err := blobs.Open(string(d))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxDecision+1))
	return b, errors.Join(err, rc.Close())
}

// seal is the blob ref of content d, in f's project, derived from the
// review's inputs (sorted, each once).
func seal(f Facts, d types.Digest) (types.Ref, error) {
	lineage := slices.Compact(slices.Sorted(slices.Values(f.Inputs)))
	return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(d), Scope: types.RefScope{Project: &f.Project},
		Lineage: append([]string{}, lineage...), Restrictions: []types.Restriction{}, ContentDigest: d})
}
