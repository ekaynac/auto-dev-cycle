// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// whatever its conclusions. Two gate results with the same conclusions, on
// the same tree, hold receipts for demands A and B: B's serves demand B,
// A's does not (06 rule 6; 11 intake).
func receiptForAnotherDemand(ctx context.Context, dir string) ([]Step, []string, error) {
	return resolveGate(ctx, dir, []resolveCase{{gate: demandB, tree: treeA, demand: demandB}, {gate: demandA, tree: treeA, demand: demandB}})
}

const (
	treeA = "git:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	treeB = "git:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
	b, err := json.Marshal(map[string]any{"type": "tree", "project": "prj_01m3250vkwffc979nzjw0g2sf9", "tree": tree, "head": nil, "contract_revision": "1",
		"environment": map[string]any{"identity": instrumentDigest, "description": "linux/arm64"},
		"demand":      map[string]any{"key": demand, "run": "run_01m3250v03fpq92bcrrpmkpsj9"}})
	var s types.Subject
	return s, errors.Join(err, json.Unmarshal(b, &s))
}

// resolveGate builds plan → gate → review for each gate a case names (its
// result holding a clean receipt about tree A under that demand, with the
// same conclusions), and resolves the case's review.
func resolveGate(ctx context.Context, dir string, cases []resolveCase) (steps []Step, violations []string, err error) {
	state := filepath.Join(dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	blobs, err := blobstore.Open(state)
	if err != nil {
		return nil, nil, err
	}
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: graph.Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: eventstore.ProjectEach(graph.Project)}})
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	actor, err := ids.New("act")
	if err != nil {
		return nil, nil, err
	}
	do := func(fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
		cmd, err := ids.New("cmd")
		if err == nil {
			_, err = s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: actor, Action: "fixture.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
				eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
					evs, err := fn(tx)
					return evs, json.RawMessage(`{}`), err
				}))
		}
		return err
	}
	const project, task = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9"), types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2np")
	// complete records, claims and completes a node of kind on input, with
	// a result of kind holding value.
	complete := func(kind types.NodeKind, params json.RawMessage, input []types.InputSpec, resolved map[string]graph.Resolved, rk types.ResultKind, value []types.Ref, conclusions *json.RawMessage) (types.NodeID, types.ResultID, error) {
		nid, err1 := ids.New("nod")
		rid, err2 := ids.New("res")
		if err := errors.Join(err1, err2); err != nil {
			return "", "", err
		}
		n := types.NodeRecorded{Type: "node_recorded", Task: task, Project: project, Node: types.Node{ID: types.NodeID(nid), Kind: kind, DefinitionVersion: "1",
			Round: 1, Params: params, Inputs: input, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
		key, err := graph.Key(n.Node, resolved, 1)
		r := types.Result{ID: types.ResultID(rid), Computation: key.Key, Kind: rk, Value: value, Conclusions: conclusions,
			Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
		for _, fn := range []func(*eventstore.Tx) ([]eventstore.Event, error){
			func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(n) },
			func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Claim(tx, n.Node.ID, resolved, 1) },
			func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return graph.RecordResult(tx, types.ResultRecorded{Node: n.Node.ID, Result: r})
			},
			func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Select(tx, n.Node.ID, r.ID) },
		} {
			if err == nil {
				err = do(fn)
			}
		}
		return n.Node.ID, r.ID, err
	}
	on := func(name string, producer types.NodeID, kind types.ResultKind) []types.InputSpec {
		return []types.InputSpec{{Name: types.Name(name), Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}}}}
	}
	plan, planRes, err := complete(types.NodeKindPlan, json.RawMessage(`{}`), []types.InputSpec{}, map[string]graph.Resolved{}, types.ResultKindPlan, []types.Ref{}, nil)
	if err != nil {
		return nil, nil, err
	}
	reviews := map[types.Digest]types.NodeID{}
	for _, c := range cases {
		if _, ok := reviews[c.gate]; ok {
			continue
		}
		gateReceipt, err := gateReceiptBlob(blobs, treeA, c.gate)
		if err != nil {
			return nil, nil, err
		}
		ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeReceipt, ID: "rcp_01m3250v57fhm9v1z9xd1wk8gw", Scope: types.RefScope{Project: ptrTo(project)},
			Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: gateReceipt})
		if err != nil {
			return nil, nil, err
		}
		gate, _, err := complete(types.NodeKindGate, json.RawMessage(`{"demand":"`+string(c.gate)+`"}`), on("plan", plan, types.ResultKindPlan),
			map[string]graph.Resolved{"plan": {Result: planRes}}, types.ResultKindGateResult, []types.Ref{ref}, ptrTo(gateConclusions))
		rid, ierr := ids.New("nod")
		if err := errors.Join(err, ierr); err != nil {
			return nil, nil, err
		}
		review := types.NodeRecorded{Type: "node_recorded", Task: task, Project: project, Node: types.Node{ID: types.NodeID(rid), Kind: types.NodeKindReview,
			DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: on("gate", gate, types.ResultKindGateResult), Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
		if err := do(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(review) }); err != nil {
			return nil, nil, err
		}
		reviews[c.gate] = review.Node.ID
	}
	var o observe
	for _, c := range cases {
		subject, err := subjectOf(c.tree, c.demand)
		if err != nil {
			return nil, nil, err
		}
		need := func(types.Node, types.InputSpec) (*receipt.Requirement, error) {
			return &receipt.Requirement{Kind: types.ReceiptKindGate, Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest}, Subject: subject}, nil
		}
		var res map[string]graph.Resolved
		var miss []graph.Missing
		if err := s.View(ctx, func(tx *eventstore.Tx) (err error) {
			res, miss, err = graph.Resolve(tx, reviews[c.gate], nil, need, blobs, receiptsAt)
			return err
		}); err != nil {
			return nil, nil, err
		}
		switch {
		case len(miss) == 1 && miss[0].Refusal != nil:
			o.steps = append(o.steps, Step{vocab.CouldNotRun, miss[0].Refusal.Reason.String()})
		case len(miss) == 0 && res["gate"].Result != "":
			o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
		default:
			return nil, nil, errors.New("the review resolved neither way")
		}
	}
	return o.steps, o.violations, nil
}

func ptrTo[T any](v T) *T { return &v }

// gateConclusions are every gate result's here; each receipt's
// conclusions_digest is their RFC 8785 digest.
var gateConclusions = json.RawMessage(`{"tests":"pass"}`)

// gateReceiptBlob publishes a clean go-test receipt about tree under demand.
func gateReceiptBlob(blobs *blobstore.Store, tree string, demand types.Digest) (types.Digest, error) {
	subject, err := subjectOf(tree, demand)
	if err != nil {
		return "", err
	}
	r := types.Receipt{ID: "rcp_01m3250v57fhm9v1z9xd1wk8gw", Kind: types.ReceiptKindGate, Subject: subject,
		Instrument: types.InstrumentRef{Name: "go-test", Version: "1.0.0", Digest: instrumentDigest},
		Provenance: types.ReceiptProvenance{Command: "cmd_01m3250vz2ezn81cgvbkcn17jd", Calls: []types.CallID{}, ModelFamilies: []types.Name{}, Contributors: []types.ActorID{}},
		ProducedAt: types.Time(receiptsAt.Format(time.RFC3339)), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean,
		Predicates: []types.Predicate{pred("tests", vocab.Empty, vocab.ReasonCompletedClean)}}
	r.Predicates[0].Detail = ""
	conclusions, err := eventstore.ContentDigest(gateConclusions)
	if err != nil {
		return "", err
	}
	r.ConclusionsDigest = types.Digest(conclusions)
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	d, err := blobs.Put(bytes.NewReader(b), "")
	return types.Digest(d), err
}
