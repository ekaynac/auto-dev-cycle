// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reviewNeedsItsTree: a review does not start with another tree's gate
// result. The gate result's receipt is about tree A; a review of tree A
// resolves it, a review of tree B finds it missing as evidence_inapplicable
// (13 "Evaluation"; 06; 11 intake). The requirement is the fixture's until
// the review kind's own (G4b), which also refuses the lease and the commit.
func reviewNeedsItsTree(ctx context.Context, dir string) ([]Step, []string, error) {
	return resolveGate(ctx, dir, []resolveCase{{gate: demandA, tree: treeA, demand: demandA}, {gate: demandA, tree: treeB, demand: demandA}})
}

// receiptForAnotherDemand: a receipt for another demand does not apply,
// whatever its conclusions. Two gate results with the same predicate
// conclusions, on the same tree, hold receipts for demands A and B: B's
// serves demand B, A's does not (06 rule 6; 11 intake).
func receiptForAnotherDemand(ctx context.Context, dir string) ([]Step, []string, error) {
	return resolveGate(ctx, dir, []resolveCase{{gate: demandB, tree: treeA, demand: demandB}, {gate: demandA, tree: treeA, demand: demandB}})
}

const (
	treeA     = "git:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	treeB     = "git:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	gProject  = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	gTask     = types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np")
	receiptID = "rcp_01m3250v57fhm9v1z9xd1wk8gw"
)

var (
	demandA = types.Digest("sha256:" + strings.Repeat("e", 64))
	demandB = types.Digest("sha256:" + strings.Repeat("f", 64))
)

// resolveCase resolves a review of the gate whose receipt is about tree A
// under gate, requiring a go-test gate receipt about tree under demand.
type resolveCase struct {
	gate   types.Digest
	tree   string
	demand types.Digest
}

func subjectOf(tree string, demand types.Digest) (types.Subject, error) {
	b, err := json.Marshal(map[string]any{"type": "tree", "project": gProject, "tree": tree, "head": nil, "contract_revision": "1",
		"environment": map[string]any{"identity": instrumentDigest, "description": "linux/arm64"},
		"demand":      map[string]any{"key": demand, "run": "run_01m3250v03fpq92bcrrpmkpsj9"}})
	var s types.Subject
	return s, errors.Join(err, json.Unmarshal(b, &s))
}

// graphFix is a scratch store with the graph fold and a selected plan.
type graphFix struct {
	ctx     context.Context
	s       *eventstore.Store
	blobs   *blobstore.Store
	actor   string
	plan    types.NodeID
	planRes types.ResultID
}

// openGraph opens the fixture's store in dir; close releases it.
func openGraph(ctx context.Context, dir string) (g *graphFix, close func() error, err error) {
	state := filepath.Join(dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	blobs, err := blobstore.Open(state)
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	var s *eventstore.Store
	if err == nil {
		s, err = eventstore.Open(ctx, lock, eventstore.Config{Kinds: graph.Kinds(), Blobs: blobs,
			Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(graph.Project)}})
	}
	if err != nil {
		return nil, nil, errors.Join(err, lock.Release())
	}
	close = func() error { return errors.Join(s.Close(), lock.Release()) }
	g = &graphFix{ctx: ctx, s: s, blobs: blobs}
	if g.actor, err = ids.New("act"); err == nil {
		g.plan, g.planRes, err = g.complete(types.NodeKindPlan, json.RawMessage(`{}`), []types.InputSpec{}, map[string]graph.Resolved{}, types.ResultKindPlan, []types.Ref{}, nil)
	}
	if err != nil {
		return nil, nil, errors.Join(err, close())
	}
	return g, close, nil
}

// do submits one command whose logic is fn.
func (g *graphFix) do(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	if err == nil {
		_, err = g.s.SubmitApplied(g.ctx, eventstore.Command{ID: cmd, Actor: g.actor, Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			}))
	}
	return err
}

// record records a node of kind in the task, on input.
func (g *graphFix) record(kind types.NodeKind, params json.RawMessage, input []types.InputSpec) (types.NodeID, error) {
	nid, err := ids.New("nod")
	n := types.NodeRecorded{Type: "node_recorded", Task: gTask, Project: gProject, Node: types.Node{ID: types.NodeID(nid), Kind: kind, DefinitionVersion: "1",
		Round: 1, Params: params, Inputs: input, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	if kind == types.NodeKindEvidence { // an evidence node leases its target (12)
		n.Node.Requires.Resources = []types.ResourceGroupKind{types.ResourceGroupKindEvidenceTarget}
	}
	if err == nil {
		err = g.do(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(n) })
	}
	return n.Node.ID, err
}

// complete records, claims and completes a node of kind on input, with
// a result of kind holding value.
func (g *graphFix) complete(kind types.NodeKind, params json.RawMessage, input []types.InputSpec, resolved map[string]graph.Resolved, rk types.ResultKind, value []types.Ref, conclusions *json.RawMessage) (types.NodeID, types.ResultID, error) {
	n, err := g.record(kind, params, input)
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Claim(tx, n, resolved, 1) })
	}
	if err != nil {
		return "", "", err
	}
	r, err := g.selectResult(n, rk, value, conclusions)
	return n, r, err
}

// selectResult records and selects a clean result of kind holding value
// for n's current key.
func (g *graphFix) selectResult(n types.NodeID, rk types.ResultKind, value []types.Ref, conclusions *json.RawMessage) (types.ResultID, error) {
	rid, err := ids.New("res")
	st, _, serr := g.node(n)
	if err = errors.Join(err, serr); err != nil {
		return "", err
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: rk, Value: value, Conclusions: conclusions,
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.RecordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
	}); err != nil {
		return "", err
	}
	return r.ID, g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Select(tx, n, r.ID) })
}

// node is n's state and its claim's command, read from the projections.
func (g *graphFix) node(n types.NodeID) (types.NodeState, *string, error) {
	var rec struct {
		State types.NodeState `json:"state"`
	}
	var c struct {
		Command *string `json:"command"`
	}
	err := g.s.View(g.ctx, func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(graph.NodeProjection, string(n))
		if err == nil {
			err = json.Unmarshal(raw, &rec)
		}
		if raw, ok, gerr := tx.Get(graph.ClaimProjection, string(n)); err == nil && gerr == nil && ok {
			err = json.Unmarshal(raw, &c)
		}
		return err
	})
	return rec.State, c.Command, err
}

// count is the number of events of kind in the log.
func (g *graphFix) count(kind string) (n int, err error) {
	err = g.s.View(g.ctx, func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == kind {
				n++
			}
		}
		return err
	})
	return n, err
}

// gateOf completes a gate on the plan whose result holds a clean go-test
// receipt about tree under demand, valid until until (nil: no limit).
func (g *graphFix) gateOf(tree string, demand types.Digest, until *types.Time) (types.NodeID, error) {
	ref, conclusions, err := g.gateReceipt(tree, demand, until)
	if err != nil {
		return "", err
	}
	gate, _, err := g.complete(types.NodeKindGate, json.RawMessage(`{"demand":"`+string(demand)+`"}`), on("plan", g.plan, types.ResultKindPlan),
		map[string]graph.Resolved{"plan": {Result: g.planRes}}, types.ResultKindGateResult, []types.Ref{ref}, &conclusions)
	return gate, err
}

// reviewOf records a review taking gate's result.
func (g *graphFix) reviewOf(gate types.NodeID) (types.NodeID, error) {
	return g.record(types.NodeKindReview, json.RawMessage(`{}`), on("gate", gate, types.ResultKindGateResult))
}

func on(name string, producer types.NodeID, kind types.ResultKind) []types.InputSpec {
	return []types.InputSpec{{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}}}}
}

// resolved is the step of resolving review at at against a go-test
// receipt about tree under demand: empty/completed_clean when its gate
// input resolves, could_not_run with the refusal's reason when it does not.
func (g *graphFix) resolved(review types.NodeID, tree string, demand types.Digest, at time.Time) (Step, error) {
	subject, err := subjectOf(tree, demand)
	if err != nil {
		return Step{}, err
	}
	need := func(types.Node, types.InputSpec) (*receipt.Requirement, error) {
		return &receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest}, Subject: subject}, nil
	}
	var res map[string]graph.Resolved
	var miss []graph.Missing
	if err := g.s.View(g.ctx, func(tx *eventstore.Tx) (err error) {
		res, miss, err = graph.Resolve(tx, review, nil, need, g.blobs, at)
		return err
	}); err != nil {
		return Step{}, err
	}
	switch {
	case len(miss) == 1 && miss[0].Refusal != nil:
		return Step{vocab.CouldNotRun, miss[0].Refusal.Reason.String()}, nil
	case len(miss) == 0 && res["gate"].Result != "":
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, nil
	}
	return Step{}, fmt.Errorf("the review resolved neither way: %+v", miss)
}

// resolveGate builds plan → gate → review for each gate a case names (its
// result holding a clean receipt about tree A under that demand), and
// resolves the case's review.
func resolveGate(ctx context.Context, dir string, cases []resolveCase) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	reviews := map[types.Digest]types.NodeID{}
	for _, c := range cases {
		if _, ok := reviews[c.gate]; ok {
			continue
		}
		gate, err := g.gateOf(treeA, c.gate, nil)
		if err == nil {
			reviews[c.gate], err = g.reviewOf(gate)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	var o observe
	for _, c := range cases {
		st, err := g.resolved(reviews[c.gate], c.tree, c.demand, receiptsAt)
		if err != nil {
			return nil, nil, err
		}
		o.steps = append(o.steps, st)
	}
	return o.steps, o.violations, nil
}

func ptrTo[T any](v T) *T { return &v }

// gateReceipt publishes a clean go-test receipt about tree under demand,
// valid until until, and returns its Ref and its rule 2a conclusions
// document, which a result holding it carries as its conclusions.
func (g *graphFix) gateReceipt(tree string, demand types.Digest, until *types.Time) (types.Ref, json.RawMessage, error) {
	subject, err := subjectOf(tree, demand)
	if err != nil {
		return types.Ref{}, nil, err
	}
	r := types.Receipt{ID: receiptID, Kind: types.ReceiptKindGate, Subject: subject,
		Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest},
		Provenance: types.ReceiptProvenance{Command: "cmd_01m3250vz2ezn81cgvbkcn17jd", Calls: []types.CallID{}, ModelFamilies: []types.Name{}, Contributors: []types.ActorID{}},
		ProducedAt: types.Time(time.Now().UTC().Format(time.RFC3339Nano)), ValidUntil: until, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean,
		Predicates: []types.Predicate{pred("tests", vocab.Empty, vocab.ReasonCompletedClean)}}
	r.Predicates[0].Detail = ""
	conclusions, digest, err := receipt.Conclusions(r)
	if err != nil {
		return types.Ref{}, nil, err
	}
	r.ConclusionsDigest = digest
	b, err := json.Marshal(r)
	if err != nil {
		return types.Ref{}, nil, err
	}
	d, err := g.blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		return types.Ref{}, nil, err
	}
	ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeReceipt, ID: receiptID, Scope: types.RefScope{Project: ptrTo(gProject)},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
	return ref, conclusions, err
}
