// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"bytes"
	"context"
	"encoding/json"
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
	// conclusions are a gate result's; its receipt's conclusions_digest is
	// their RFC 8785 digest.
	conclusions = json.RawMessage(`{"tests": "pass"}`)
)

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
		"produced_at": "2026-10-05T12:00:00Z", "valid_until": until, "outcome": "empty", "reason": "completed_clean", "detail": "", "verdict": nil,
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

func (g graphStore) receiptBlob(subject map[string]any, until *string) types.Digest {
	return g.blob(receiptDoc(subject, until))
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
	g.t.Helper()
	rec := g.newNode(kind)
	rec.Node.Params = json.RawMessage(`{"node":"` + string(rec.Node.ID) + `"}`)
	n := g.record(rec)
	g.must(g.claimOf(n))
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Value, r.Conclusions = rk, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", value, ptr(conclusions)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: r})
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, r.ID) })
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
	return func(types.Node, types.InputSpec) (*receipt.Requirement, error) {
		s := treeSubject(tree, demand)
		b, _ := json.Marshal(s)
		var sub types.Subject
		if err := json.Unmarshal(b, &sub); err != nil {
			return nil, err
		}
		return &receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest}, Subject: sub}, nil
	}
}

func noRequirement(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }

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

// refused: n's one input is missing with reason.
func (g graphStore) refused(n types.NodeID, need Requirements, reason vocab.Reason) bool {
	res, miss := g.resolve(n, need, at)
	return len(res) == 0 && len(miss) == 1 && miss[0].Refusal != nil && miss[0].Refusal.Reason == reason && miss[0].Name == "gate"
}

// waits: n's one input is missing with no refusal: a structural wait.
func (g graphStore) waits(n types.NodeID, need Requirements) bool {
	res, miss := g.resolve(n, need, at)
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
	bare, _ := g.gated("")
	twice, _ := g.completedWith(types.NodeKindGate, types.ResultKindGateResult,
		[]types.Ref{g.sealed(types.RefTypeReceipt, rcpID, good), g.sealed(types.RefTypeReceipt, rcpB, g.receiptBlob(treeSubject(treeB, demandA), nil))})
	misnamed, _ := g.completedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpB, good)})
	for name, gate := range map[string]types.NodeID{
		"a result with no receipt":                       bare,
		"a result with two receipts":                     twice,
		"a receipt that is not the one its Ref names":    misnamed,
		"a receipt of other conclusions than the result": g.gatedOnly(g.blob(other)),
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
		"a receipt Ref with no blob":      {g.reviewOf(g.gatedOnly("sha256:" + strings.Repeat("9", 64))), gateOn(treeA, demandA)},
		"a blob that is not a receipt":    {g.reviewOf(g.gatedOnly(g.blob(map[string]any{"id": rcpID}))), gateOn(treeA, demandA)},
	} {
		if _, _, err := g.resolveWith(c.node, map[string]Resolved{}, c.need); err == nil {
			t.Errorf("%s resolves", name)
		}
	}
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
	ask := g.newNode(types.NodeKindReview)
	ask.Node.Inputs = []types.InputSpec{{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}}}
	asking := g.record(ask)
	if res, miss, err := g.resolveWith(asking, map[string]Resolved{"ruling": {Approval: aprA, Revision: 2}}, noRequirement); err != nil || res["ruling"].Approval != aprA || len(miss) != 0 {
		t.Errorf("the answer: %v %+v %v", res, miss, err)
	}
	for name, answered := range map[string]map[string]Resolved{
		"no answer":                     {},
		"an answer of another revision": {"ruling": {Approval: aprA, Revision: 3}},
		"an answer with no approval":    {"ruling": {Revision: 2}},
	} {
		if res, miss, err := g.resolveWith(asking, answered, noRequirement); err != nil || len(res) != 0 || len(miss) != 1 || miss[0].Name != "ruling" {
			t.Errorf("%s: %v %+v %v", name, res, miss, err)
		}
	}
}
