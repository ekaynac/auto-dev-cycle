// SPDX-License-Identifier: Apache-2.0

// Package review computes a review node's result and its D55 report from
// the reviewer's typed decision (P16b).
//
// Decisions:
//   - The decision is the fact: the reviewer's typed output
//     (adapters/http-chat's defaultDecisionSchema: go or no_go, a summary,
//     findings), admitted to the blob store by the adapter host. The
//     controller reads it again strictly (contract.ReadStrict: no
//     duplicate key, valid UTF-8, no trailing data; no unknown field) and
//     within the schema's bounds; it never trusts the adapter's check.
//   - The outcome table: go → empty/completed_clean; no_go →
//     found/completed_with_findings, which a repair round takes
//     (graph.Repair); no submitted decision → could_not_run with the exit
//     map's reason; a decision that is not one → could_not_run/
//     degenerate_output. The adapter's own outcome never decides.
//   - The result's value is the decision (the repair builder's findings
//     input), when it is one, and the D55 report. The detail never echoes
//     the reviewer's text.
//   - D55: the report records the elapsed time, the charge links (the
//     reviewer's model call ids), the roster, the verdict, each finding's
//     severity and title (cut to maxTitle bytes), and the round, and for a
//     no_go the repair round it causes (round + 1). P30's ledger reads it.
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
	Exit    exitmap.Exit
	Round   int64
	Roster  []types.RosterMember
	Calls   []types.CallID // the reviewer's model calls: the charge links
	Elapsed time.Duration
	Project types.ProjectID
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

// Parse reads b as a decision, strictly and within its bounds.
func Parse(b []byte) (Decision, error) {
	if len(b) > maxDecision {
		return Decision{}, fmt.Errorf("review: a decision of over %d bytes", maxDecision)
	}
	if _, err := contract.ReadStrict(b); err != nil {
		return Decision{}, err
	}
	type finding struct { // a field left out decodes to nil: every one is required
		Severity *string `json:"severity"`
		Title    *string `json:"title"`
		Detail   *string `json:"detail"`
	}
	var raw struct {
		Decision *string    `json:"decision"`
		Summary  *string    `json:"summary"`
		Findings *[]finding `json:"findings"`
	}
	if err := contract.UnmarshalStrict(b, &raw); err != nil {
		return Decision{}, err
	}
	switch {
	case raw.Decision == nil || raw.Summary == nil || raw.Findings == nil:
		return Decision{}, errors.New("review: a decision has a decision, a summary and findings")
	case *raw.Decision != Go && *raw.Decision != NoGo:
		return Decision{}, errors.New("review: the decision is go or no_go")
	case utf8.RuneCountInString(*raw.Summary) > maxSummary || len(*raw.Findings) > maxFindings:
		return Decision{}, errors.New("review: the decision is past its bounds")
	}
	d := Decision{Decision: *raw.Decision, Summary: *raw.Summary, Findings: []Finding{}}
	for _, f := range *raw.Findings {
		if f.Severity == nil || f.Title == nil || f.Detail == nil || !slices.Contains(severities, *f.Severity) ||
			utf8.RuneCountInString(*f.Title) > maxTitle || utf8.RuneCountInString(*f.Detail) > maxDetail {
			return Decision{}, errors.New("review: a finding is not one")
		}
		d.Findings = append(d.Findings, Finding{*f.Severity, *f.Title, *f.Detail})
	}
	return d, nil
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
		ref, serr := seal(f.Project, f.Decision)
		if serr != nil {
			return types.Result{}, serr
		}
		r.Value, r.Completed = append(r.Value, ref), true
		if d.Decision == Go {
			r.Outcome, r.Reason = types.OutcomeEmpty, vocab.ReasonCompletedClean
		} else {
			r.Outcome, r.Reason, r.Detail = types.OutcomeFound, vocab.ReasonCompletedWithFindings, fmt.Sprintf("the reviewer's decision is no_go, with %d findings", len(d.Findings))
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
	pref, err := seal(f.Project, types.Digest(pd))
	if err != nil {
		return types.Result{}, err
	}
	r.Value = append(r.Value, pref)
	return r, graph.Check(r)
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
		next := f.Round + 1
		repair = &next
	}
	return json.Marshal(struct {
		Schema      string               `json:"schema"`
		ElapsedMS   int64                `json:"elapsed_ms"`
		Round       int64                `json:"round"`
		Roster      []types.RosterMember `json:"roster"`
		Outcome     types.Outcome        `json:"outcome"`
		Reason      string               `json:"reason"`
		Decision    string               `json:"decision"`
		Findings    []finding            `json:"findings"`
		RepairRound *int64               `json:"repair_round"`
		ChargeLinks []types.CallID       `json:"charge_links"`
	}{reportSchema, f.Elapsed.Milliseconds(), f.Round, append([]types.RosterMember{}, f.Roster...), r.Outcome, r.Reason.String(), d.Decision,
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

// seal is the blob ref of content d, in project.
func seal(project types.ProjectID, d types.Digest) (types.Ref, error) {
	return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(d), Scope: types.RefScope{Project: &project},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: d})
}
