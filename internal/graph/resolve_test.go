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
	cmdID = "cmd_01m3250vz2ezn81cgvbkcn17jd"
	runA  = "run_01m3250v03fpq92bcrrpmkpsj9"
)

var (
	instrumentDigest = types.Digest("sha256:" + strings.Repeat("a", 64))
	demandA          = types.Digest("sha256:" + strings.Repeat("e", 64))
	demandB          = types.Digest("sha256:" + strings.Repeat("f", 64))
	envID            = types.Digest("sha256:" + strings.Repeat("c", 64))
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

// receiptBlob publishes a clean gate receipt about subject, valid until
// until (nil: no limit), and returns its digest.
func (g graphStore) receiptBlob(subject map[string]any, until *string) types.Digest {
	g.t.Helper()
	doc := map[string]any{"id": rcpID, "kind": "gate", "instrument": map[string]any{"name": "go-test", "version": "1.0.0", "digest": instrumentDigest},
		"subject": subject, "provenance": map[string]any{"command": cmdID, "attempt": nil, "calls": []any{}, "model_families": []any{}, "contributors": []any{}},
		"produced_at": "2026-10-05T12:00:00Z", "valid_until": until, "outcome": "empty", "reason": "completed_clean", "detail": "", "verdict": nil,
		"predicates":         []any{map[string]any{"name": "tests", "outcome": "empty", "reason": "completed_clean", "detail": "", "evidence": []any{}}},
		"conclusions_digest": instrumentDigest, "supersedes": nil}
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

// gated records, claims and completes a gate node whose result holds the
// receipt blob; nil holds no receipt.
func (g graphStore) gated(receiptDigest types.Digest) (types.NodeID, types.Result) {
	g.t.Helper()
	rec := g.newNode(types.NodeKindGate)
	rec.Node.Params = json.RawMessage(`{"receipt":"` + string(receiptDigest) + `"}`) // a key of its own
	n := g.record(rec)
	g.must(g.claimOf(n))
	r := unrecorded(g.t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Value = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", []types.Ref{}
	r.Conclusions = ptr(json.RawMessage(`{"tests":"pass"}`))
	if receiptDigest != "" {
		ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeReceipt, ID: rcpID, Scope: types.RefScope{Project: ptr(types.ProjectID(prjID))},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: receiptDigest})
		if err != nil {
			g.t.Fatal(err)
		}
		diffRef, err := knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: string(diff), Scope: types.RefScope{Project: ptr(types.ProjectID(prjID))},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: diff})
		if err != nil {
			g.t.Fatal(err)
		}
		r.Value = []types.Ref{diffRef, ref} // the receipt need not come first
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: r})
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, r.ID) })
	return n, r
}

// reviewOf is a review node taking gate's result.
func (g graphStore) reviewOf(gate types.NodeID) types.NodeID {
	r := g.nodeOn(types.NodeKindReview, gate)
	r.Node.Inputs[0].Source.Variant.(*types.ResultSource).ResultKind = types.ResultKindGateResult
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

// An input resolves to its producer's selected result only while that
// result applies: the same tree and demand, still fresh. Otherwise it is
// missing with the refusal's reason; a producer not completed is a wait.
func TestResolve(t *testing.T) {
	g := openStore(t)
	until := "2026-10-08T11:00:00Z"
	gate, r := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	review := g.reviewOf(gate)
	inapplicable := &vocab.ReasonEvidenceInapplicable
	for name, c := range map[string]struct {
		need   Requirements
		reason *vocab.Reason
	}{
		"the same tree and demand": {gateOn(treeA, demandA), nil},
		"another tree":             {gateOn(treeB, demandA), inapplicable},
		"another demand":           {gateOn(treeA, demandB), inapplicable},
		"no requirement":           {func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }, nil},
	} {
		res, miss := g.resolve(review, c.need, at)
		switch {
		case c.reason == nil && (len(miss) != 0 || res["gate"].Result != r.ID):
			t.Errorf("%s: %v, missing %+v", name, res, miss)
		case c.reason != nil && (len(miss) != 1 || miss[0].Refusal == nil || miss[0].Refusal.Reason != *c.reason || miss[0].Name != "gate" || len(res) != 0):
			t.Errorf("%s: missing %+v", name, miss)
		}
	}
	expired, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &until))
	if _, miss := g.resolve(g.reviewOf(expired), gateOn(treeA, demandA), at); len(miss) != 1 || miss[0].Refusal == nil || miss[0].Refusal.Reason != vocab.ReasonEvidenceInapplicable {
		t.Errorf("an expired receipt: %+v", miss)
	}
	bare, _ := g.gated("")
	if _, miss := g.resolve(g.reviewOf(bare), gateOn(treeA, demandA), at); len(miss) != 1 || miss[0].Refusal == nil || miss[0].Refusal.Reason != vocab.ReasonReceiptIncomplete {
		t.Errorf("a result with no receipt: %+v", miss)
	}
	waiting := g.record(g.newNode(types.NodeKindGate))
	if res, miss := g.resolve(g.reviewOf(waiting), gateOn(treeA, demandA), at); len(res) != 0 || len(miss) != 1 || miss[0].Refusal != nil {
		t.Errorf("a producer not completed: %v %+v", res, miss)
	}
	ask := g.nodeOn(types.NodeKindReview, gate)
	ask.Node.Inputs = []types.InputSpec{{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}}}
	asking := g.record(ask)
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		res, miss, err := Resolve(tx, asking, map[string]Resolved{"ruling": {Approval: aprA, Revision: 2}}, gateOn(treeA, demandA), g.blobs, at)
		if err == nil && (res["ruling"].Approval != aprA || len(miss) != 0) {
			err = errors.New("the answer did not pass through")
		}
		if _, miss, _ = Resolve(tx, asking, map[string]Resolved{}, gateOn(treeA, demandA), g.blobs, at); err == nil && len(miss) != 1 {
			err = errors.New("an unanswered input is not missing")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
