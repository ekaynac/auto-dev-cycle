// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	treeA = "git:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	treeB = "git:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rcpID = "rcp_01m3250v57fhm9v1z9xd1wk8gw"
	rcpB  = "rcp_01m3250v9xft08ssr4ygq6g2np"
	cmdID = "cmd_01m3250vz2ezn81cgvbkcn17jd"
	runA  = "run_01m3250v03fpq92bcrrpmkpsj9"
)

var (
	instrumentDigest = types.Digest("sha256:" + strings.Repeat("a", 64))
	demandA          = types.Digest("sha256:" + strings.Repeat("e", 64))
	demandB          = types.Digest("sha256:" + strings.Repeat("f", 64))
	envID            = types.Digest("sha256:" + strings.Repeat("c", 64))
	// conclusions are a gate result's whose receipt the store did not
	// derive.
	conclusions = json.RawMessage(`{"tests": "pass"}`)
)

// goTest is the gate instrument the receipts here are derived under.
var goTest = types.InstrumentContract{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest, SubjectType: types.InstrumentContractSubjectTypeTree,
	RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "tests", EvidenceRequired: []string{}}},
	OptionalPredicates: []string{}, Preconditions: []types.Name{}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}

// treeSubject is a gate receipt's subject: tree, under demand when set.
func treeSubject(tree string, demand types.Digest) map[string]any {
	var d any
	if demand != "" {
		d = map[string]any{"key": demand, "run": runA}
	}
	return map[string]any{"type": "tree", "project": prjID, "tree": tree, "head": nil,
		"environment": map[string]any{"identity": envID, "description": "linux/arm64"}, "contract_revision": "1", "demand": d}
}

// receiptDoc is a clean gate receipt about subject, valid until until (nil:
// no limit), concluding conclusions.
func receiptDoc(subject map[string]any, until *string) map[string]any {
	return map[string]any{"id": rcpID, "kind": "gate", "instrument": map[string]any{"name": "go-test", "version": "1.0.0", "digest": instrumentDigest},
		"subject": subject, "provenance": map[string]any{"command": cmdID, "attempt": nil, "calls": []any{}, "model_families": []any{}, "contributors": []any{}},
		"produced_at": time.Now().UTC().Format(time.RFC3339Nano), "valid_until": until, "outcome": "empty", "reason": "completed_clean", "detail": "", "verdict": nil,
		"predicates":         []any{map[string]any{"name": "tests", "outcome": "empty", "reason": "completed_clean", "detail": "", "evidence": []any{}}},
		"conclusions_digest": must(eventstore.ContentDigest(conclusions)), "supersedes": nil}
}

// blob publishes doc and returns its digest.
func (g graphStore) blob(doc any) types.Digest {
	g.t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		g.t.Fatal(err)
	}
	d, err := g.blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		g.t.Fatal(err)
	}
	return types.Digest(d)
}

// receiptBlob publishes a receipt about subject derived under goTest, and
// keeps its rule 2a conclusions document for the result that holds it.
func (g graphStore) receiptBlob(subject map[string]any, until *string) types.Digest {
	g.t.Helper()
	return g.derived(receiptDoc(subject, until))
}

// derived publishes the receipt doc derived under goTest, keeping its
// conclusions document.
func (g graphStore) derived(doc map[string]any) types.Digest { return g.derivedUnder(goTest, doc) }

// derivedUnder is derived under contract.
func (g graphStore) derivedUnder(contract types.InstrumentContract, doc map[string]any) types.Digest {
	g.t.Helper()
	b, err := json.Marshal(doc)
	var r types.Receipt
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	contract.Version = r.Instrument.Version // at the doc's instrument version
	d, derr := receipt.Derive(contract, r, g.blobs)
	concl, _, cerr := receipt.Conclusions(r)
	if err = errors.Join(err, derr, cerr); err != nil {
		g.t.Fatal(err)
	}
	r.Outcome, r.Reason, r.Detail, r.Verdict, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.Verdict, d.ConclusionsDigest
	digest := g.blob(r)
	g.conclusions[digest] = concl
	return digest
}

// sealed is a Ref of type to the content d, under id.
func (g graphStore) sealed(typ types.RefType, id string, d types.Digest) types.Ref {
	g.t.Helper()
	ref, err := knowledge.Seal(types.Ref{Type: typ, ID: id, Scope: types.RefScope{Project: ptr(types.ProjectID(prjID))},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: d})
	if err != nil {
		g.t.Fatal(err)
	}
	return ref
}

// gated records, claims and completes a gate node whose result holds a
// blob and the receipt blob, in that order; "" holds no receipt.
func (g graphStore) gated(receiptDigest types.Digest) (types.NodeID, types.Result) {
	value := []types.Ref{}
	if receiptDigest != "" { // the receipt need not come first
		value = []types.Ref{g.sealed(types.RefTypeBlob, string(diff), diff), g.sealed(types.RefTypeReceipt, rcpID, receiptDigest)}
	}
	return g.completedWith(types.NodeKindGate, types.ResultKindGateResult, value)
}

// completedWith records, claims and completes a node of kind, under a key
// of its own, with a result of kind rk holding value.
func (g graphStore) completedWith(kind types.NodeKind, rk types.ResultKind, value []types.Ref) (types.NodeID, types.Result) {
	return g.completedAs(kind, rk, value, func(n types.NodeID, r types.Result) logic {
		rr := g.published(n, r)
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return recordResult(tx, rr, g.blobs) }
	})
}

// forgedWith is completedWith with a result.recorded event the builder did
// not write: its receipt's validity bound to bound, whatever the blob says.
func (g graphStore) forgedWith(kind types.NodeKind, rk types.ResultKind, value []types.Ref, bound *types.Time) (types.NodeID, types.Result) {
	return g.completedAs(kind, rk, value, func(n types.NodeID, r types.Result) logic {
		return forge(ResultRecorded, string(r.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: n, Result: r, ReceiptValidUntil: bound})
	})
}

func (g graphStore) completedAs(kind types.NodeKind, rk types.ResultKind, value []types.Ref, recordOf func(types.NodeID, types.Result) logic) (types.NodeID, types.Result) {
	g.t.Helper()
	rec := g.newNode(kind)
	rec.Node.Params = json.RawMessage(`{"node":"` + string(rec.Node.ID) + `"}`)
	if kind == types.NodeKindEvidence {
		rec.Node.Params = evidenceParams(string(rec.Node.ID))
	}
	if kind == types.NodeKindReview {
		rec.Node.Params = reviewParams(exeA)
	}
	n := g.record(rec)
	if kind == types.NodeKindBuild {
		g.leaseOn(n, exeC) // its builder: gpt, apart from the tests' claude reviewers (P16b)
	}
	g.must(g.claimOf(n))
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Value, r.Conclusions = rk, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", value, ptr(conclusions)
	for _, ref := range value {
		if doc, ok := g.conclusions[ref.ContentDigest]; ok && ref.Type == types.RefTypeReceipt {
			r.Conclusions = ptr(doc)
			break
		}
	}
	if !carriesConclusions(rk) {
		r.Conclusions = nil
	}
	g.must(recordOf(n, r))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, r.ID) })
	return n, r
}

// reviewOf is a review node taking gate's result.
func (g graphStore) reviewOf(gate types.NodeID) types.NodeID {
	return g.consumerOf(gate, types.ResultKindGateResult)
}

// consumerOf is a review node taking producer's result as a kind result.
func (g graphStore) consumerOf(producer types.NodeID, kind types.ResultKind) types.NodeID {
	r := g.nodeOn(types.NodeKindReview, producer)
	r.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = kind
	r.Node.Inputs[0].Name = "gate"
	return g.record(r)
}

// gateOn is the review kind's requirement: a gate receipt from go-test about
// tree under demand.
func gateOn(tree string, demand types.Digest) Requirements {
	return func(*eventstore.Tx, types.Node, types.InputSpec) (*receipt.Requirement, error) {
		s := treeSubject(tree, demand)
		b, _ := json.Marshal(s)
		var sub types.Subject
		if err := json.Unmarshal(b, &sub); err != nil {
			return nil, err
		}
		return &receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest}, Subject: sub}, nil
	}
}

func noRequirement(*eventstore.Tx, types.Node, types.InputSpec) (*receipt.Requirement, error) {
	return nil, nil
}

func (g graphStore) resolveWith(n types.NodeID, answered map[string]Resolved, need Requirements) (map[string]Resolved, []Missing, error) {
	var res map[string]Resolved
	var miss []Missing
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		res, miss, err = Resolve(tx, n, answered, need, g.blobs, at)
		return err
	})
	return res, miss, err
}

func (g graphStore) resolve(n types.NodeID, need Requirements, at time.Time) (map[string]Resolved, []Missing) {
	g.t.Helper()
	var res map[string]Resolved
	var miss []Missing
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		res, miss, err = Resolve(tx, n, map[string]Resolved{}, need, g.blobs, at)
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return res, miss
}

var at = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// unrequired is res without a review's requirement, which every review
// declares (G7b-2).
func unrequired(res map[string]Resolved) map[string]Resolved {
	delete(res, "required")
	return res
}

// refused: n's one input but its requirement is missing with reason.
func (g graphStore) refused(n types.NodeID, need Requirements, reason vocab.Reason) bool {
	res, miss := g.resolve(n, need, at)
	res = unrequired(res)
	return len(res) == 0 && len(miss) == 1 && miss[0].Refusal != nil && miss[0].Refusal.Reason == reason && miss[0].Name == "gate"
}

// waits: n's one input but its requirement is missing with no refusal: a
// structural wait.
func (g graphStore) waits(n types.NodeID, need Requirements) bool {
	res, miss := g.resolve(n, need, at)
	res = unrequired(res)
	return len(res) == 0 && len(miss) == 1 && miss[0].Refusal == nil && miss[0].Detail != ""
}

// An input resolves to its producer's selected result only while that
// result applies: the same tree and demand, still fresh. Otherwise it is
// missing with the refusal's reason; a producer not completed is a wait.
func TestResolve(t *testing.T) {
	g := openStore(t)
	until := "2026-10-08T11:00:00Z"
	good := g.receiptBlob(treeSubject(treeA, demandA), nil)
	gate, r := g.gated(good)
	review := g.reviewOf(gate)
	if res, miss := g.resolve(review, gateOn(treeA, demandA), at); len(miss) != 0 || res["gate"].Result != r.ID {
		t.Errorf("the same tree and demand: %v, missing %+v", res, miss)
	}
	if !g.refused(review, gateOn(treeB, demandA), vocab.ReasonEvidenceInapplicable) || !g.refused(review, gateOn(treeA, demandB), vocab.ReasonEvidenceInapplicable) {
		t.Error("another tree or demand applies")
	}
	expired, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &until))
	if !g.refused(g.reviewOf(expired), gateOn(treeA, demandA), vocab.ReasonEvidenceInapplicable) {
		t.Error("an expired receipt applies")
	}
	other := receiptDoc(treeSubject(treeA, demandA), nil)
	other["conclusions_digest"] = instrumentDigest
	forged := g.blob(other)
	g.conclusions[forged] = g.conclusions[good] // the result's conclusions are the receipt's document
	bare, _ := g.gated("")
	// Forged: the fold refuses a gate's result of two receipts (Astra #321
	// H1); resolution refuses one recorded all the same.
	twice, _ := g.forgedWith(types.NodeKindGate, types.ResultKindGateResult,
		[]types.Ref{g.sealed(types.RefTypeReceipt, rcpID, good), g.sealed(types.RefTypeReceipt, rcpB, g.receiptBlob(treeSubject(treeB, demandA), nil))}, nil)
	misnamed, _ := g.completedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpB, good)})
	// A derived receipt held by a result whose conclusions are not its.
	unlike := g.receiptBlob(treeSubject(treeA, demandA), nil)
	g.conclusions[unlike] = conclusions
	for name, gate := range map[string]types.NodeID{
		"a result with no receipt":                       bare,
		"a result with two receipts":                     twice,
		"a receipt that is not the one its Ref names":    misnamed,
		"a receipt whose digest is not its conclusions'": g.gatedOnly(forged),
		"a receipt of other conclusions than the result": g.gatedOnly(unlike),
	} {
		if !g.refused(g.reviewOf(gate), gateOn(treeA, demandA), vocab.ReasonReceiptIncomplete) {
			t.Errorf("%s applies", name)
		}
	}
	if !g.waits(g.reviewOf(g.record(g.newNode(types.NodeKindGate))), gateOn(treeA, demandA)) {
		t.Error("a producer not completed is not a wait")
	}
	far, _ := g.planOf(tskB, prjB)
	if !g.waits(g.consumerOf(far, types.ResultKindPlan), noRequirement) || !g.waits(g.consumerOf(gate, types.ResultKindReview), noRequirement) {
		t.Error("a producer of another project, or a result of another kind, resolves")
	}
	if res, miss := g.resolve(g.record(g.newNode(types.NodeKindReview)), noRequirement, at); len(miss) != 0 || res["plan"].Result != g.planRes {
		t.Errorf("a plan input needs no receipt: %v %+v", res, miss)
	}
	evidence, _ := g.completedWith(types.NodeKindEvidence, types.ResultKindEvidence, []types.Ref{})
	for name, c := range map[string]struct {
		node types.NodeID
		need Requirements
	}{
		"no requirements":                 {review, nil},
		"no requirement for a gate input": {review, noRequirement},
		"no requirement for evidence":     {g.consumerOf(evidence, types.ResultKindEvidence), noRequirement},
		"a receipt Ref with no blob":      {g.reviewOf(g.forgedOnly("sha256:" + strings.Repeat("9", 64))), gateOn(treeA, demandA)},
		"a blob that is not a receipt":    {g.reviewOf(g.forgedOnly(g.blob(map[string]any{"id": rcpID}))), gateOn(treeA, demandA)},
	} {
		if _, _, err := g.resolveWith(c.node, map[string]Resolved{}, c.need); err == nil {
			t.Errorf("%s resolves", name)
		}
	}
}

// forgedOnly is a gate whose result, recorded by a forged event, holds
// only the receipt Ref to d, which the builder could not read.
func (g graphStore) forgedOnly(d types.Digest) types.NodeID {
	n, _ := g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}, nil)
	return n
}

// gatedOnly is a gate whose result holds only the receipt Ref to d.
func (g graphStore) gatedOnly(d types.Digest) types.NodeID {
	n, _ := g.completedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)})
	return n
}

// An answer input resolves only to an approval at its declared revision;
// G4b checks the approval again at commit.
func TestResolveAnswers(t *testing.T) {
	g := openStore(t)
	asking := g.asking(owes("ruling", ownID))
	if res, miss, err := g.resolveWith(asking, map[string]Resolved{"ruling": {Approval: aprA, Revision: 2}}, noRequirement); err != nil || res["ruling"].Approval != aprA || len(miss) != 0 {
		t.Errorf("the answer: %v %+v %v", res, miss, err)
	}
	for name, answered := range map[string]map[string]Resolved{
		"no answer":                     {},
		"an answer of another revision": {"ruling": {Approval: aprA, Revision: 3}},
		"an answer with no approval":    {"ruling": {Revision: 2}},
	} {
		if res, miss, err := g.resolveWith(asking, answered, noRequirement); err != nil || len(unrequired(res)) != 0 || len(miss) != 1 || miss[0].Name != "ruling" {
			t.Errorf("%s: %v %+v %v", name, res, miss, err)
		}
	}
}
