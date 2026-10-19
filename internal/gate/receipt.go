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
	seal := func(t types.RefType, id string, d types.Digest) (types.Ref, error) {
		return knowledge.Seal(types.Ref{Type: t, ID: id, Scope: types.RefScope{Project: &s.Project},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: d})
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
// bytes of ids. A short id stays verbatim, by design (the
// hostile-gate-output fixture): the reviewer's handling of ids is P16's.
func (v Verdict) Report() ([]byte, error) {
	type finding struct {
		Package string          `json:"package"`
		Name    string          `json:"name,omitempty"`
		Outcome results.Outcome `json:"outcome"`
	}
	id := func(s string) string {
		if len(s) <= maxID {
			return s
		}
		return strings.ToValidUTF8(s[:maxID-len("…")], "") + "…"
	}
	var blocking []finding
	for _, x := range v.Results.Blocking() {
		blocking = append(blocking, finding{id(x.Package), id(x.Name), x.Outcome})
	}
	pkgs := v.Results.BlockingPackages()
	for _, p := range slices.Sorted(maps.Keys(pkgs)) {
		blocking = append(blocking, finding{Package: id(p), Outcome: pkgs[p]})
	}
	truncated := max(0, len(blocking)-maxReported)
	return json.Marshal(struct {
		Schema      string         `json:"schema"`
		ElapsedMS   int64          `json:"elapsed_ms"`
		Tree        string         `json:"tree"`
		Outcome     vocab.Outcome  `json:"outcome"`
		Reason      string         `json:"reason"`
		Detail      string         `json:"detail"`
		Tests       int            `json:"tests"`
		Blocking    []finding      `json:"blocking"`
		Truncated   int            `json:"truncated"`
		ChargeLinks []types.CallID `json:"charge_links"`
	}{reportSchema, v.Elapsed.Milliseconds(), v.Tree, v.Outcome, v.Reason.String(), v.Why, len(v.Results.Results),
		append([]finding{}, blocking[:len(blocking)-truncated]...), truncated, []types.CallID{}})
}

// String is v's verdict, for logs.
func (v Verdict) String() string {
	return fmt.Sprintf("%s/%s: %s", v.Outcome, v.Reason, v.Why)
}
