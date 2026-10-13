// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/evidence"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var loopback = map[string]string{"network": "loopback"}

// runIn is the step of n's next attempt, a clean walk holding value, run
// in a fake wrapper under setting, its isolation probe in the same wrapper
// unless foreign: the result CommitEvidence records.
func (g *graphFix) runIn(n types.NodeID, setting map[string]string, foreign bool, value []types.Ref, conclusions *json.RawMessage) (Step, error) {
	st, _, err := g.node(n)
	rid, rerr := ids.New("res")
	aid, aerr := g.nextAttempt(n)
	other, oerr := ids.New("att")
	if err = errors.Join(err, rerr, aerr, oerr); err != nil || st.Key == nil {
		return Step{}, errors.Join(err, errors.New("the walk is not claimed"))
	}
	wrapper := sandbox.Wrapper{Run: evidence.RunOf(types.AttemptID(aid)), Setting: setting}
	if foreign { // the probe ran in another run's wrapper
		wrapper.Run = evidence.RunOf(types.AttemptID(other))
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: types.ResultKindEvidence, Value: value, Conclusions: conclusions,
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, err = graph.CommitEvidence(tx, n, r, types.AttemptID(aid), wrapper.Probe(), g.walkEnv())
		return nil, err
	})
	return Step{vocab.Outcome(r.Outcome), r.Reason.String()}, err
}

// nextAttempt starts n's next attempt under a fresh lease on n's unit (M1
// P8a), with its result submitted (finishing): its first, or the successor
// of its last, finishing at its second revision, which is sealed with the
// minimal record, then superseded.
func (g *graphFix) nextAttempt(n types.NodeID) (types.AttemptID, error) {
	l, att, err := freshLease()
	l.Unit = graph.UnitOf(n)
	var last types.AttemptID
	var continues bool
	if err == nil {
		err = g.s.View(g.ctx, func(tx *eventstore.Tx) (err error) {
			last, continues, err = lease.CurrentAttempt(tx, l.Unit)
			return err
		})
	}
	base := types.AttemptBase{Tree: types.GitID(treeA), ContractRevision: "7"}
	steps := []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, nil)
		},
	}
	if !continues {
		steps = append(steps, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartAttempt(tx, l.ID, 2, att, base) })
	} else {
		steps = append(steps,
			func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SealMinimal(tx, last, 2, graph.Units{}, g.blobs, nil, types.ActorID(g.actor))
			},
			func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SupersedeAttempt(tx, last, 3, l.ID, 2, att, base)
			})
	}
	steps = append(steps, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // its result is submitted: it records one
		a, _, _, err := lease.AttemptOf(tx, att)
		call, cerr := ids.New("cal")
		if err = errors.Join(err, cerr); err != nil {
			return nil, err
		}
		return lease.SubmitResult(tx, att, a.FencingToken, types.SubmittedResult{Call: types.CallID(call), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
	})
	for _, step := range steps {
		if err == nil {
			err = g.do(step)
		}
	}
	return att, err
}

// emptyIsolationNeverPublished: an evidence result is published only with
// its isolation proven by a probe inside the run's own wrapper (12
// "Content"). A walk run under an empty isolation setting, and one whose
// probe ran in another run's wrapper, are could_not_run/isolation_unproven
// and never selected; a walk under a loopback setting is published.
func emptyIsolationNeverPublished(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	tree, err := g.tree()
	var o observe
	for _, c := range []struct {
		demand  types.Digest
		setting map[string]string
		foreign bool
	}{{demandA, map[string]string{}, false}, {demandB, loopback, true}, {types.Digest("sha256:" + strings.Repeat("c", 64)), loopback, false}} {
		var n types.NodeID
		var st Step
		if err == nil {
			n, err = g.walkOn(tree, c.demand, 1, 3)
		}
		if err == nil {
			st, err = g.runIn(n, c.setting, c.foreign, []types.Ref{}, ptrTo(json.RawMessage(`{"steps":"pass"}`)))
		}
		var state types.NodeState
		if err == nil {
			state, _, err = g.node(n)
		}
		if err != nil {
			return nil, nil, err
		}
		o.check((st.Outcome == vocab.Empty) == (state.State == types.NodeStateStateCompleted), "walk %d: %s, node %s", len(o.steps)+1, st, state.State)
		o.steps = append(o.steps, st)
	}
	return o.steps, o.violations, nil
}

// walkNeed requires of an evidence input a walk receipt about tree under
// demand; other inputs need none.
func walkNeed(tree string, demand types.Digest) graph.Requirements {
	return func(_ types.Node, in types.InputSpec) (*receipt.Requirement, error) {
		if src, ok := in.Source.Variant.(*types.ResultSource); !ok || src.ResultKind != types.ResultKindEvidence {
			return nil, nil
		}
		subject, err := subjectOf(tree, demand)
		return &receipt.Requirement{Kind: types.ReceiptKindIntegration, Instrument: types.InstrumentRef{Name: "walk", Version: "1", Digest: walkDigest}, Subject: subject}, err
	}
}

// restrictedArtifactNotInPrompt: a restricted artifact never reaches a
// prompt (12 "Content"). A walk's published result holds its receipt, a
// diagnostic trace, a restricted remote stderr and a blob the receipt does
// not name. The review taking it renders the receipt and the trace; the
// stderr and the unnamed blob are omitted as restricted, and the manifest
// intent built from the context is valid. A restricted ref in the context
// is failed/restriction_dropped. Delivered later as a tool result, the
// stderr is refused (failed/restriction_dropped); the trace is delivered.
func restrictedArtifactNotInPrompt(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	tree, err := g.tree()
	var walk, review types.NodeID
	if err == nil {
		walk, err = g.walkOn(tree, demandA, 1, 3)
	}
	var arts []types.Ref
	for _, a := range []string{"trace", "remote-stderr", "loose"} {
		var ref types.Ref
		if err == nil {
			ref, err = g.artifact(a)
		}
		arts = append(arts, ref)
	}
	var rcpt types.Ref
	var conclusions json.RawMessage
	if err == nil {
		steps := pred("steps", vocab.Empty, vocab.ReasonCompletedClean, types.PredicateEvidenceItem{Blob: arts[0].ContentDigest, MediaType: "text/plain", Artifact: "trace"},
			types.PredicateEvidenceItem{Blob: arts[1].ContentDigest, MediaType: "text/plain", Artifact: "remote-stderr"})
		steps.Detail = ""
		rcpt, conclusions, err = g.receiptWith(types.ReceiptKindIntegration, types.InstrumentRef{Name: "walk", Version: "1", Digest: walkDigest}, treeA, demandA, nil, []types.Restriction{}, steps)
	}
	if err == nil {
		_, err = g.runIn(walk, loopback, false, append([]types.Ref{rcpt}, arts...), &conclusions)
	}
	env := graph.Env{Need: walkNeed(treeA, demandA), Blobs: g.blobs, Authority: owedanswers.Authority{}, Contracts: walkContracts, Families: reviewers}
	if err == nil {
		review, err = g.record(types.NodeKindReview, soleReviewer, on("walk", walk, types.ResultKindEvidence))
	}
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{review}, env)
		})
	}
	var c graph.Rendered
	if err == nil {
		err = g.s.View(ctx, func(tx *eventstore.Tx) (err error) {
			c, err = graph.Context(tx, review, env)
			return err
		})
	}
	in, ierr := intentOf(c.Included...)
	if err = errors.Join(err, ierr); err != nil {
		return nil, nil, err
	}
	var o observe
	o.check(hasRef(c.Included, rcpt) && hasRef(c.Included, arts[0]), "the receipt or the diagnostic trace is not rendered")
	for _, a := range arts[1:] {
		st := Step{vocab.Empty, vocab.ReasonCompletedClean.String()}
		omitted := slices.ContainsFunc(c.Omitted, func(x types.ManifestIntentOmittedItem) bool {
			return x.Ref.IdentityDigest == a.IdentityDigest && x.Reason == types.ManifestIntentOmittedItemReasonRestricted
		})
		if hasRef(c.Included, a) || !omitted {
			st = Step{vocab.Failed, vocab.ReasonRestrictionDropped.String()}
		}
		o.steps = append(o.steps, st)
		in.Inputs = append(in.Inputs, a)
	}
	in.Omitted = c.Omitted
	o.check(knowledge.CheckIntent(in) == nil, "the manifest intent built from the context is invalid: %v", knowledge.CheckIntent(in))
	// Tool delivery: the omitted stderr is refused, the diagnostic trace is
	// delivered.
	for _, a := range []types.Ref{arts[1], arts[0]} {
		if err := o.checked(knowledge.CheckDelivery(in, turn(in, 1, []types.Ref{a}))); err != nil {
			return nil, nil, err
		}
	}
	return o.steps, o.violations, nil
}

// artifact publishes an artifact blob named name and returns its Ref.
func (g *graphFix) artifact(name string) (types.Ref, error) {
	b, err := json.Marshal(map[string]string{"artifact": name})
	var d string
	if err == nil {
		d, err = g.blobs.Put(bytes.NewReader(b), "")
	}
	if err != nil {
		return types.Ref{}, err
	}
	return knowledge.Seal(types.Ref{Type: types.RefTypeBlob, ID: name, Scope: types.RefScope{Project: ptrTo(gProject)},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: types.Digest(d)})
}

// hasRef: refs hold r, by identity.
func hasRef(refs []types.Ref, r types.Ref) bool {
	return slices.ContainsFunc(refs, func(x types.Ref) bool { return x.IdentityDigest == r.IdentityDigest })
}
