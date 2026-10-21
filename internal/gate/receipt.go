// SPDX-License-Identifier: Apache-2.0

package gate

// The gate's receipt, result and D55 report.
//
// Decisions:
//   - A gate receipt is about the tree the run checked out (Verdict.Tree),
//     in its environment: the image's id and platform, and the profile's
//     contract revision. Its instrument's digest binds the adapter, the
//     command and the image, and its demand, which a requirement always
//     carries, binds the baseline it was judged against: the baseline's
//     content digest and its base tree (Astra #286 M3). So a receipt of
//     another tree, image, command or baseline never applies
//     (Requirement; internal/receipt's Applies).
//   - Failure text is sanitised for consumers: the run's raw results are
//     the receipt's restricted evidence, never a ref of the result. The
//     result's value (what a reviewer's context renders: graph.Context does
//     not class gate results) is the receipt and the report, which hold the
//     controller's own detail, test ids and outcomes only.
//   - D55: the report records the elapsed time, the blocking findings and
//     the charge links. A gate makes no model call, so its links are an
//     explicit empty list; the repairs a finding causes are the next
//     round's, linked to this node by P30's ledger.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/results"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	reportSchema = "adc.gate-report/v1"
	maxReported  = 1000 // blocking findings in a report
	maxID        = 256  // bytes of a reported package or test name
	evidenceType = "application/x-go-test-json"
	// instrumentName is the instrument of every gate receipt Result makes:
	// Contract's name for the one result adapter Run accepts.
	instrumentName = "gate." + results.GoTestJSON
)

// Blobs is the blob store as the gate needs it.
type Blobs interface {
	receipt.Blobs
	Put(r io.Reader, declared string) (string, error)
}

// Subject is what a gate receipt is about, beside its tree and baseline.
type Subject struct {
	Project  types.ProjectID
	Revision string // the profile's contract revision
	// Acceptance is the acceptance blobs whose overlay the run applied: the
	// lineage of the receipt and the report, which derive from them
	// (#226 entry 91(3): the acceptance edge is kept; a reviewer takes them
	// through graph.RefRecord.Sanitised only).
	Acceptance []types.Digest
}

// Contract is the gate's instrument contract for g run in img.
func Contract(g types.ProjectProfileGate, img sandbox.Image) (types.InstrumentContract, error) {
	b, err := json.Marshal(struct {
		Adapter string   `json:"adapter"`
		Command []string `json:"command"`
		Image   string   `json:"image"`
	}{g.ResultAdapter, g.Command, img.ID})
	sum := sha256.Sum256(b)
	return types.InstrumentContract{Name: "gate." + g.ResultAdapter, Version: "1", Digest: types.Digest("sha256:" + hex.EncodeToString(sum[:])),
		SubjectType:        types.InstrumentContractSubjectTypeTree,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "tests", EvidenceRequired: []string{evidenceType}}},
		OptionalPredicates: []string{}, Preconditions: []types.Name{},
		ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{"results": types.InstrumentContractArtifactClassesValueRestricted}}, err
}

// subject is a receipt's or requirement's tree subject, its demand the
// baseline's (demand).
func subject(tree string, img sandbox.Image, s Subject, b *Baseline) (types.Subject, error) {
	key, err := demand(b)
	if err != nil {
		return types.Subject{}, err
	}
	run, err := ids.New("run")
	return types.Subject{Variant: &types.TreeSubject{Type: "tree", Project: s.Project, Tree: types.GitID("git:" + tree),
		Environment:      types.Environment{Identity: types.Digest("sha256:" + img.ID), Description: img.OS + "/" + img.Arch},
		ContractRevision: s.Revision, Demand: &types.DemandValue{Key: key, Run: types.EvidenceRunID(run)}}}, err
}

// demand is the key a gate's demand derives from its baseline: the
// baseline's content digest, which covers its base tree, report, command
// and platform image. A measurement has none (Measure's verdict carries no
// baseline), so no receipt is ever made of it.
func demand(b *Baseline) (types.Digest, error) {
	if b == nil {
		return "", fmt.Errorf("%w: a gate receipt needs its baseline: a measurement is no gate result", ErrSpec)
	}
	d, err := b.Digest()
	sum := sha256.Sum256([]byte("adc.gate-baseline/v1\x00" + string(d)))
	return types.Digest("sha256:" + hex.EncodeToString(sum[:])), err
}

// Requirement is what a gate receipt must be to serve tree against b.
func Requirement(c types.InstrumentContract, tree string, img sandbox.Image, s Subject, b *Baseline) (receipt.Requirement, error) {
	sub, err := subject(tree, img, s, b)
	return receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest},
		Subject: sub}, err
}

// Result publishes v as the gate node's result for key: its raw results
// as restricted evidence, its receipt (derived, never the producer's), and
// its report. command is the controller command that ran the gate.
func Result(key types.ComputationKey, v Verdict, c types.InstrumentContract, img sandbox.Image, s Subject, command types.CommandID, blobs Blobs, now time.Time) (types.Result, error) {
	put := func(b []byte) (types.Digest, error) {
		d, err := blobs.Put(bytes.NewReader(b), "")
		return types.Digest(d), err
	}
	if v.overlaid && len(s.Acceptance) == 0 { // a lineage that is not the run's: the report would read as derived from nothing
		return types.Result{}, fmt.Errorf("%w: the run applied an acceptance overlay, and the subject names no acceptance blob", ErrSpec)
	}
	lineage := []string{}
	for _, a := range slices.Compact(slices.Sorted(slices.Values(s.Acceptance))) {
		lineage = append(lineage, string(a))
	}
	seal := func(t types.RefType, id string, d types.Digest) (types.Ref, error) {
		return knowledge.Seal(types.Ref{Type: t, ID: id, Scope: types.RefScope{Project: &s.Project},
			Lineage: slices.Clone(lineage), Restrictions: []types.Restriction{}, ContentDigest: d})
	}
	stream, err := put(v.Stream)
	if err != nil {
		return types.Result{}, err
	}
	rid, err := ids.New("rcp")
	if err != nil {
		return types.Result{}, err
	}
	detail := v.Why
	if v.Outcome == vocab.Empty {
		detail = ""
	}
	sub, err := subject(v.Tree, img, s, v.baseline)
	if err != nil {
		return types.Result{}, err
	}
	r := types.Receipt{ID: types.ReceiptID(rid), Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest},
		Subject: sub, Provenance: types.ReceiptProvenance{Command: command, Calls: []types.CallID{}, ModelFamilies: []types.Name{}, Contributors: []types.ActorID{}},
		ProducedAt: types.Time(now.UTC().Format(time.RFC3339Nano)),
		Predicates: []types.Predicate{{Name: "tests", Outcome: types.Outcome(v.Outcome), Reason: v.Reason, Detail: detail,
			Evidence: []types.PredicateEvidenceItem{{Blob: stream, MediaType: evidenceType, Artifact: "results"}}}}}
	d, err := receipt.Derive(c, r, blobs)
	if err != nil {
		return types.Result{}, err
	}
	r.Outcome, r.Reason, r.Detail, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.ConclusionsDigest
	conclusions, _, err := receipt.Conclusions(r)
	if err != nil {
		return types.Result{}, err
	}
	rb, err := json.Marshal(r)
	if err != nil {
		return types.Result{}, err
	}
	rd, err := put(rb)
	if err != nil {
		return types.Result{}, err
	}
	report, err := v.Report()
	if err != nil {
		return types.Result{}, err
	}
	pd, err := put(report)
	if err != nil {
		return types.Result{}, err
	}
	rref, err := seal(types.RefTypeReceipt, rid, rd)
	if err != nil {
		return types.Result{}, err
	}
	pref, err := seal(types.RefTypeBlob, string(pd), pd)
	if err != nil {
		return types.Result{}, err
	}
	id, err := ids.New("res")
	return types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindGateResult, Value: []types.Ref{rref, pref},
		Conclusions: &conclusions, Outcome: r.Outcome, Reason: r.Reason, Detail: r.Detail,
		Completed: d.Outcome == vocab.Empty || d.Outcome == vocab.Found}, err
}

// Report is the D55 record of one gate node: the run's elapsed time, its
// verdict and blocking findings (test ids and outcomes, at most
// maxReported, the rest counted, each id cut to at most maxID bytes, its
// ellipsis included: a name is the candidate's text, Opus #286 L2), and
// its charge links (none). It holds at most about maxReported × 2 maxID
// bytes of ids. A short id stays verbatim: no prompt renders the report's
// bytes (Opus #321 L7: `review.Prompt` renders counts and the verdict
// only, the hostile-gate-output fixture's decision), and the findings
// view redacts the ids it is given (review.Facts.GateIDs).
func (v Verdict) Report() ([]byte, error) {
	id := func(s string) string {
		if len(s) <= maxID {
			return s
		}
		return strings.ToValidUTF8(s[:maxID-len("…")], "") + "…"
	}
	var blocking []reportFinding
	for _, x := range v.Results.Blocking() {
		blocking = append(blocking, reportFinding{id(x.Package), id(x.Name), x.Outcome})
	}
	pkgs := v.Results.BlockingPackages()
	for _, p := range slices.Sorted(maps.Keys(pkgs)) {
		blocking = append(blocking, reportFinding{Package: id(p), Outcome: pkgs[p]})
	}
	truncated := max(0, len(blocking)-maxReported)
	return json.Marshal(report{reportSchema, v.Elapsed.Milliseconds(), v.Tree, v.Outcome, v.Reason.String(), v.Why, len(v.Results.Results),
		append([]reportFinding{}, blocking[:len(blocking)-truncated]...), truncated, []types.CallID{}})
}

// report is Report's record.
type report struct {
	Schema      string          `json:"schema"`
	ElapsedMS   int64           `json:"elapsed_ms"`
	Tree        string          `json:"tree"`
	Outcome     vocab.Outcome   `json:"outcome"`
	Reason      string          `json:"reason"`
	Detail      string          `json:"detail"`
	Tests       int             `json:"tests"`
	Blocking    []reportFinding `json:"blocking"`
	Truncated   int             `json:"truncated"`
	ChargeLinks []types.CallID  `json:"charge_links"`
}

type reportFinding struct {
	Package string          `json:"package"`
	Name    string          `json:"name,omitempty"`
	Outcome results.Outcome `json:"outcome"`
}

// maxReportBytes bounds a report CheckReport reads: maxReported findings
// of two ids, with room for the rest.
const maxReportBytes = 2*maxReported*(2*maxID+64) + 64<<10

// MaxDetail bounds a gate receipt's free text a reviewer may take (Opus
// #321 r2 M2; Mythos #348 M1): its detail and its predicates', its
// environment's description, its contract revision and its instrument's
// version.
const MaxDetail = 1024

// CheckReport is report read as Report's bytes, strictly (Astra #321 H1:
// what a reviewer takes as a gate's sanitised report): the bytes
// json.Marshal gives of a report, exactly, with no other key, within
// Report's bounds (at most maxReported findings, each id at most maxID
// bytes, no charge link), and its content checked (Opus #321 r2 M2): its
// outcome one of the four, its reason a registered code, its tree a hex
// git id, its detail one of the controller's phrases (isWhy; Mythos #348
// L1), its counts at least 0, and each finding's outcome a test outcome.
func CheckReport(b []byte) error {
	var r report
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if len(b) > maxReportBytes {
		return fmt.Errorf("%w: a gate report of over %d bytes", ErrSpec, maxReportBytes)
	}
	if err := dec.Decode(&r); err != nil || r.Schema != reportSchema || len(r.Blocking) > maxReported || len(r.ChargeLinks) != 0 {
		return fmt.Errorf("%w: not a gate report (%v)", ErrSpec, err)
	}
	_, rerr := vocab.ParseReason(r.Reason)
	switch {
	case !slices.Contains([]vocab.Outcome{vocab.Found, vocab.Empty, vocab.CouldNotRun, vocab.Failed}, r.Outcome):
		return fmt.Errorf("%w: a gate report's outcome %q", ErrSpec, r.Outcome)
	case rerr != nil:
		return fmt.Errorf("%w: a gate report's reason: %v", ErrSpec, rerr)
	case !hexID(r.Tree):
		return fmt.Errorf("%w: a gate report's tree is no git id", ErrSpec)
	case !isWhy(r.Detail):
		return fmt.Errorf("%w: a gate report's detail is none of the controller's phrases", ErrSpec)
	case r.ElapsedMS < 0 || r.Tests < 0 || r.Truncated < 0:
		return fmt.Errorf("%w: a gate report's count below 0", ErrSpec)
	}
	for _, f := range r.Blocking {
		if len(f.Package) > maxID || len(f.Name) > maxID {
			return fmt.Errorf("%w: a gate report's id over %d bytes", ErrSpec, maxID)
		}
		if !slices.Contains([]results.Outcome{results.Pass, results.Skip, results.Missing, results.Incomplete, results.Fail}, f.Outcome) {
			return fmt.Errorf("%w: a gate report's finding outcome %q", ErrSpec, f.Outcome)
		}
	}
	if again, err := json.Marshal(r); err != nil || !bytes.Equal(again, b) {
		return fmt.Errorf("%w: not a gate report's bytes (%v)", ErrSpec, err)
	}
	return nil
}

// CheckReceipt is whether b is a gate receipt under id exactly as Result
// makes one (Opus #321 r2 M2; Mythos #348 M1, #355 M1): decoded strictly as
// records/receipt, of kind gate, its id the ref's, its instrument the
// gate's (instrumentName, its version at most MaxDetail bytes), its
// subject a tree whose environment description and contract revision are
// at most MaxDetail bytes, no call, model family or contributor in its
// provenance, its reason a registered code and its detail at most
// MaxDetail bytes, and exactly one predicate: tests, its reason a
// registered code, its detail at most MaxDetail bytes, no payload, and
// exactly one evidence item, the results artifact of the gate's media
// type.
func CheckReceipt(b []byte, id string) error {
	r, err := contract.Decode[types.Receipt]("records/receipt", b)
	if err != nil {
		return fmt.Errorf("%w: not a receipt (%v)", ErrSpec, err)
	}
	_, rerr := vocab.ParseReason(r.Reason.String())
	switch {
	case r.Kind != types.ReceiptKindGate || string(r.ID) != id:
		return fmt.Errorf("%w: not a gate receipt under %s", ErrSpec, id)
	case r.Instrument.Name != instrumentName || len(r.Instrument.Version) > MaxDetail:
		return fmt.Errorf("%w: a receipt of another instrument", ErrSpec)
	case len(r.Provenance.Calls) != 0 || len(r.Provenance.ModelFamilies) != 0 || len(r.Provenance.Contributors) != 0:
		return fmt.Errorf("%w: a gate receipt with calls, model families or contributors", ErrSpec)
	case rerr != nil || len(r.Detail) > MaxDetail:
		return fmt.Errorf("%w: a gate receipt's reason or detail (%v)", ErrSpec, rerr)
	case len(r.Predicates) != 1:
		return fmt.Errorf("%w: a gate receipt of %d predicates", ErrSpec, len(r.Predicates))
	}
	tree, ok := r.Subject.Variant.(*types.TreeSubject)
	if !ok || len(tree.Environment.Description) > MaxDetail || len(tree.ContractRevision) > MaxDetail {
		return fmt.Errorf("%w: a gate receipt's subject", ErrSpec)
	}
	p := r.Predicates[0]
	_, perr := vocab.ParseReason(p.Reason.String())
	if p.Name != "tests" || perr != nil || len(p.Detail) > MaxDetail || len(p.Payload) != 0 || len(p.Evidence) != 1 ||
		p.Evidence[0].Artifact != "results" || p.Evidence[0].MediaType != evidenceType {
		return fmt.Errorf("%w: a gate receipt's predicate", ErrSpec)
	}
	return nil
}

// hexID is whether s is a git object id: 40 (SHA-1) or 64 (SHA-256)
// lower-case hex digits.
func hexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ReportIDs is a gate report's ids (Report's): each blocking finding's
// package and test name, each once, in order; the ids review.Result keeps
// out of the builder's findings view (#226 entry 94(2)). A report with
// none gives an empty list, never nil (Astra #321 M3: nil is "not
// supplied" to review.Facts.GateIDs).
func ReportIDs(report []byte) ([]string, error) {
	var r struct {
		Schema   string `json:"schema"`
		Blocking []struct {
			Package string `json:"package"`
			Name    string `json:"name"`
		} `json:"blocking"`
	}
	if err := json.Unmarshal(report, &r); err != nil || r.Schema != reportSchema {
		return nil, fmt.Errorf("%w: not a gate report (%v)", ErrSpec, err)
	}
	out := []string{}
	for _, f := range r.Blocking {
		for _, id := range []string{f.Package, f.Name} {
			if id != "" && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// String is v's verdict, for logs.
func (v Verdict) String() string {
	return fmt.Sprintf("%s/%s: %s", v.Outcome, v.Reason, v.Why)
}
