// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var walkDigest = types.Digest("sha256:" + strings.Repeat("a", 64))

// walk is the evidence fixtures' instrument contract: a walk of a tree,
// run once its dns precondition passes.
func walk() types.InstrumentContract {
	return types.InstrumentContract{Name: "walk", Version: "1", Digest: walkDigest, SubjectType: types.InstrumentContractSubjectTypeTree,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "steps", EvidenceRequired: []string{}}},
		OptionalPredicates: []string{}, Preconditions: []types.Name{"dns"}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}
}

func walkContracts(ref types.InstrumentRef) (types.InstrumentContract, error) {
	if c := walk(); ref == (types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest}) {
		return c, nil
	}
	return types.InstrumentContract{}, fmt.Errorf("no instrument contract %s %s", ref.Name, ref.Version)
}

// walkEnv evaluates evidence nodes under the walk contract.
func (g *graphFix) walkEnv() graph.Env {
	return graph.Env{Need: noNeed, Blobs: g.blobs, Authority: owedanswers.Authority{}, Contracts: walkContracts}
}

// walkParams are a walk's params on the tree input for demand ("" is the
// plan's own evidence), with caps per demand and per subject.
func walkParams(demand types.Digest, perDemand, perSubject int) json.RawMessage {
	key := "null"
	if demand != "" {
		key = `"` + string(demand) + `"`
	}
	return json.RawMessage(fmt.Sprintf(`{"instrument":{"name":"walk","version":"1","digest":"%s"},`+
		`"grant":"apr_01m3250v0aer9ss2097svmefj8","target":"tgt_01m3250v0aer9ss2097svmefj8","subject_input":"tree","demand":{"key":%s},`+
		`"limits":{"max_attempts_per_demand":%d,"max_attempts_per_subject":%d,"deadline":"PT1H","cooldown":"PT0S"},`+
		`"restore":{"required":false,"procedure":null}}`, walkDigest, key, perDemand, perSubject))
}

// walkOn records a walk of tree for demand and claims it through the
// evaluator, which checks it against the walk contract.
func (g *graphFix) walkOn(tree types.NodeID, demand types.Digest, perDemand, perSubject int) (types.NodeID, error) {
	n, err := g.record(types.NodeKindEvidence, walkParams(demand, perDemand, perSubject), on("tree", tree, types.ResultKindTree))
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Evaluate(tx, []types.NodeID{n}, g.walkEnv())
		})
	}
	return n, err
}

// tree completes an integration node with a result holding tree A.
func (g *graphFix) tree() (types.NodeID, error) {
	ref, err := knowledge.Seal(types.Ref{Type: types.RefTypeGitTree, ID: treeA, Scope: types.RefScope{Project: ptrTo(gProject)},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: walkDigest})
	if err != nil {
		return "", err
	}
	n, _, err := g.complete(types.NodeKindIntegration, json.RawMessage(`{}`), on("plan", g.plan, types.ResultKindPlan),
		map[string]graph.Resolved{"plan": {Result: g.planRes}}, types.ResultKindTree, []types.Ref{ref}, nil)
	return n, err
}

// attempt is the step of n's next attempt, n claimed: once a cap is spent,
// could_not_run/evidence_cap_exhausted held on wrong_owner; else a clean
// walk.
func (g *graphFix) attempt(n types.NodeID) (Step, error) {
	var exhausted bool
	err := g.s.View(g.ctx, func(tx *eventstore.Tx) (err error) {
		exhausted, err = graph.Exhausted(tx, n)
		return err
	})
	if err != nil {
		return Step{}, err
	}
	if !exhausted {
		_, err := g.selectResult(n, types.ResultKindEvidence, []types.Ref{}, ptrTo(json.RawMessage(`{"steps":"pass"}`)))
		return Step{vocab.Empty, vocab.ReasonCompletedClean.String()}, err
	}
	st, _, err := g.node(n)
	rid, rerr := ids.New("res")
	if err = errors.Join(err, rerr); err != nil {
		return Step{}, err
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: types.ResultKindEvidence, Value: []types.Ref{},
		Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonEvidenceCapExhausted, Detail: "the evidence caps are spent", Completed: false}
	err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, graph.Commit(tx, n, r, g.walkEnv()) })
	if err == nil {
		err = g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Exhaust(tx, n, r.ID, "project-owner")
		})
	}
	return Step{vocab.CouldNotRun, r.Reason.String()}, err
}

// evidenceNewDemandNewAttempts: a new demand gets new attempts, and the
// same demand does not (12; 11 intake). With one attempt per demand, the
// walk of demand A runs once; invalidated, its next attempt is
// evidence_cap_exhausted, since the count outlives the key; the walk of
// demand B on the same tree still runs.
func evidenceNewDemandNewAttempts(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	tree, err := g.tree()
	var a, b types.NodeID
	if err == nil {
		a, err = g.walkOn(tree, demandA, 1, 3)
	}
	for _, fn := range []func() (Step, error){
		func() (Step, error) { return g.attempt(a) },
		func() (Step, error) {
			if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Invalidate(tx, a) }); err != nil {
				return Step{}, err
			}
			return g.attempt(a)
		},
		func() (Step, error) {
			if b, err = g.walkOn(tree, demandB, 1, 3); err != nil {
				return Step{}, err
			}
			return g.attempt(b)
		},
	} {
		if err != nil {
			return nil, nil, err
		}
		var st Step
		st, err = fn()
		steps = append(steps, st)
	}
	return steps, nil, err
}

// evidenceTotalCapExhausted: the total cap per subject holds across
// demands (12; 11 intake). With two attempts per tree, the walks of
// demands A and B run; the walk of demand C is evidence_cap_exhausted and
// held on wrong_owner.
func evidenceTotalCapExhausted(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	g, closeFn, err := openGraph(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, closeFn()) }()
	tree, err := g.tree()
	var last types.NodeID
	for _, d := range []types.Digest{demandA, demandB, types.Digest("sha256:" + strings.Repeat("c", 64))} {
		if err == nil {
			last, err = g.walkOn(tree, d, 1, 2)
		}
		var st Step
		if err == nil {
			st, err = g.attempt(last)
		}
		steps = append(steps, st)
	}
	st, _, nerr := g.node(last)
	if err = errors.Join(err, nerr); err != nil {
		return nil, nil, err
	}
	if st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].Hold != "wrong_owner" || st.Holds[0].OwnerRef == nil ||
		*st.Holds[0].OwnerRef != "project-owner" {
		violations = append(violations, "the exhausted walk is not held on wrong_owner")
	}
	return steps, violations, nil
}
