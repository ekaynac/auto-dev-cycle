// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// walkContract is the contract evidenceCaps' walks run under: a tree subject.
func walkContract() types.InstrumentContract {
	return types.InstrumentContract{Name: "walk", Version: "1", Digest: walkDigest, SubjectType: types.InstrumentContractSubjectTypeTree,
		RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "steps", EvidenceRequired: []string{}}},
		OptionalPredicates: []string{}, Preconditions: []types.Name{"dns"}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}
}

func contractsOf(c types.InstrumentContract) Contracts {
	return func(ref types.InstrumentRef) (types.InstrumentContract, error) {
		if ref != (types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest}) {
			return c, errors.New("no such contract")
		}
		return c, nil
	}
}

func noReceipts(*eventstore.Tx, types.Node, types.InputSpec) (*receipt.Requirement, error) {
	return nil, nil
}

func withContracts(e Env, c Contracts) Env { e.Contracts = c; return e }

// walkNode is an unrecorded walk of the store's tree for demand, with caps.
func (g graphStore) walkNode(demand string, perDemand, perSubject int) types.NodeRecorded {
	rec := g.newNode(types.NodeKindEvidence)
	rec.Node.Params = evidenceCaps(demand, "tree", perDemand, perSubject)
	return rec
}

// walk records and claims a walk of the store's tree for demand.
func (g graphStore) walk(demand string, perDemand, perSubject int) types.NodeID {
	g.t.Helper()
	n := g.record(g.walkNode(demand, perDemand, perSubject))
	g.must(g.claimOf(n))
	return n
}

// evidence is a result of n's key: clean, or could_not_run with reason.
func (g graphStore) evidence(n types.NodeID, reason *vocab.Reason) types.Result {
	id, _ := ids.New("res")
	r := types.Result{ID: types.ResultID(id), Computation: g.state(n).Key.Key, Kind: types.ResultKindEvidence, Value: []types.Ref{},
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true, Conclusions: ptr(json.RawMessage(`{"steps":"pass"}`))}
	if reason != nil {
		r.Outcome, r.Reason, r.Detail, r.Completed, r.Conclusions = types.OutcomeCouldNotRun, *reason, "not run", false, nil
	}
	return r
}

func (g graphStore) exhausted(n types.NodeID) bool {
	var out bool
	if err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) { out, err = Exhausted(tx, n); return err }); err != nil {
		g.t.Fatal(err)
	}
	return out
}

// Attempts are counted per demand: what did not run spends none, an
// exhaustion records only once a cap is spent, and a counted result never
// past it, whatever the key: an invalidation leaves the count.
func TestEvidenceAttemptsPerDemand(t *testing.T) {
	g := openStore(t)
	n := g.walk("a", 1, 3)
	for _, reason := range []vocab.Reason{vocab.ReasonPreconditionFailed, vocab.ReasonCapacityReservedFull} {
		if err := g.recordResult(n, g.evidence(n, &reason)); err != nil || g.exhausted(n) {
			t.Fatalf("%s: %v, or it spent the attempt", reason, err)
		}
	}
	capped, stopped := vocab.ReasonEvidenceCapExhausted, vocab.ReasonAttemptStopped
	if g.recordResult(n, g.evidence(n, &capped)) == nil {
		t.Fatal("an exhaustion recorded while the cap is unspent")
	}
	if err := g.recordResult(n, g.evidence(n, &stopped)); err != nil || !g.exhausted(n) {
		t.Fatalf("a stopped run: %v, or it left the cap unspent", err)
	}
	if g.recordResult(n, g.evidence(n, nil)) == nil {
		t.Fatal("an attempt past the cap per demand recorded")
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, n) })
	if !g.exhausted(n) || g.recordResult(n, g.evidence(n, &capped)) != nil {
		t.Fatal("the invalidation reset the count, or the exhaustion was refused")
	}
	if b := g.walk("b", 1, 3); g.exhausted(b) {
		t.Fatal("a new demand found its cap spent")
	}
}

// Every could_not_run reason says whether the run started: it is in
// exactly one of notRun and ran.
func TestCouldNotRunReasonsClassified(t *testing.T) {
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for code, r := range reg.Reasons {
		if slices.Contains(r.Outcomes, types.ReasonOutcomesItem(types.OutcomeCouldNotRun)) {
			codes = append(codes, code)
			if slices.Contains(notRun, code) == slices.Contains(ran, code) {
				t.Errorf("%s is in neither or both of notRun and ran", code)
			}
		}
	}
	for _, code := range append(slices.Clone(notRun), ran...) {
		if !slices.Contains(codes, code) {
			t.Errorf("%s is no could_not_run reason", code)
		}
	}
}

// The total cap per subject holds across demands of one instrument, and a
// re-produced tree is the same subject.
func TestEvidenceTotalCapPerSubject(t *testing.T) {
	g := openStore(t)
	for _, d := range []string{"a", "b"} {
		n := g.walk(d, 1, 2)
		if err := g.recordResult(n, g.evidence(n, nil)); err != nil {
			t.Fatal(err)
		}
	}
	c := g.walk("c", 1, 2)
	if !g.exhausted(c) || g.recordResult(c, g.evidence(c, nil)) == nil {
		t.Fatal("a third demand ran past the total cap")
	}
	other := walkContract()
	other.Digest = "sha256:" + types.Digest(strings.Repeat("b", 64))
	rec := g.walkNode("c", 1, 2)
	rec.Node.Params = json.RawMessage(strings.Replace(string(rec.Node.Params), walkDigest, string(other.Digest), 1))
	n := g.record(rec)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ClaimEvidence(tx, n, map[string]Resolved{"tree": {Result: g.tree.res}}, 1, contractsOf(other))
	})
	if g.exhausted(n) {
		t.Fatal("another instrument shares the subject's total")
	}
	// The tree, invalidated and produced again with the same content.
	tree := g.tree.node
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, tree) })
	old, _ := view[types.ResultRecorded](g, ResultProjection, string(g.tree.res))
	again := unrecorded(t, *g.state(tree).Key)
	again.Kind, again.Outcome, again.Reason, again.Detail, again.Value, again.Conclusions = types.ResultKindTree, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", old.Result.Value, nil
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: tree, Result: again}, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, tree, again.ID) })
	g.tree.res = again.ID
	if d := g.walk("d", 1, 2); !g.exhausted(d) {
		t.Fatal("the re-produced tree has a fresh budget")
	}
}

// A count binds the caps it was first counted under: a node with other
// caps is refused at claim, and by the count fold.
func TestEvidenceCapsBind(t *testing.T) {
	g := openStore(t)
	late := g.walk("a", 2, 2) // claimed before anything is bound
	a := g.walk("a", 1, 2)
	if err := g.recordResult(a, g.evidence(a, nil)); err != nil {
		t.Fatal(err)
	}
	if err := g.recordResult(late, g.evidence(late, nil)); err == nil || !strings.Contains(err.Error(), "bound cap") {
		t.Fatalf("the fold counted under another cap per demand: %v", err)
	}
	pre := vocab.ReasonPreconditionFailed
	if err := g.recordResult(late, g.evidence(late, &pre)); err != nil {
		t.Fatalf("the node that lost the bind race could not record its precondition failure: %v", err)
	}
	for name, rec := range map[string]types.NodeRecorded{"the same demand": g.walkNode("a", 2, 2), "another total": g.walkNode("b", 1, 3)} {
		n := g.record(rec)
		if err := g.do(g.claimOf(n)); err == nil || !strings.Contains(err.Error(), "bound cap") {
			t.Errorf("%s with other caps claimed: %v", name, err)
		}
	}
	var err error
	_ = g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		n := g.walkNode("x", 3, 2).Node
		p, _ := n.ParamsAsEvidenceNodeParams()
		_, err = budgetFor(tx, p, g.tree.res)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unnested caps counted: %v", err)
	}
}

// An evidence node runs only under its contract: the same instrument, and
// a subject input of a kind of the contract's subject type.
func TestCheckContract(t *testing.T) {
	g := openStore(t)
	walk := g.walkNode("a", 1, 1).Node
	pkg := walk
	pkg.Inputs = []types.InputSpec{{Name: "tree", Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: g.tree.node, ResultKind: types.ResultKindPackage}}}}
	for name, n := range map[string]types.Node{"a tree": walk, "a package": pkg} {
		if err := CheckContract(n, walkContract()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	edit := func(f func(*types.InstrumentContract)) types.InstrumentContract { c := walkContract(); f(&c); return c }
	plan, none, nameless := walk, walk, walk
	plan.Inputs = g.newNode(types.NodeKindGate).Node.Inputs
	none.Inputs = []types.InputSpec{}
	nameless.Params = json.RawMessage(strings.Replace(string(walk.Params), `"subject_input":"tree"`, `"subject_input":"repo"`, 1))
	for name, c := range map[string]struct {
		n types.Node
		c types.InstrumentContract
	}{
		"another name":       {walk, edit(func(c *types.InstrumentContract) { c.Name = "probe" })},
		"another version":    {walk, edit(func(c *types.InstrumentContract) { c.Version = "2" })},
		"another digest":     {walk, edit(func(c *types.InstrumentContract) { c.Digest = "sha256:" + types.Digest(strings.Repeat("b", 64)) })},
		"a skipped restore":  {walk, edit(func(c *types.InstrumentContract) { c.RestoreProcedure = ptr(types.Name("snapshot-revert")) })},
		"a proposal subject": {walk, edit(func(c *types.InstrumentContract) { c.SubjectType = types.InstrumentContractSubjectTypeProposal })},
		"a plan subject":     {plan, walkContract()},
		"no inputs":          {none, walkContract()},
		"no such input":      {nameless, walkContract()},
	} {
		if CheckContract(c.n, c.c) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	forged := g.walkNode("a", 1, 1)
	forged.Node.Inputs = []types.InputSpec{}
	if g.do(forge(NodeRecorded, string(forged.Node.ID), prjID, &forged)) == nil {
		t.Fatal("the fold recorded an evidence node with no subject input")
	}
	n := g.record(g.walkNode("a", 1, 2))
	if g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Claim(tx, n, map[string]Resolved{"tree": {Result: g.tree.res}}, 1)
	}) == nil {
		t.Fatal("Claim claimed an evidence node outside its contract")
	}
	failing := func(types.InstrumentRef) (types.InstrumentContract, error) {
		return types.InstrumentContract{}, errors.New("registry down")
	}
	restoring := edit(func(c *types.InstrumentContract) { c.RestoreProcedure = ptr(types.Name("snapshot-revert")) })
	for name, c := range map[string]Contracts{"no contracts": nil, "a failing registry": failing, "another version": contractsOf(edit(func(c *types.InstrumentContract) { c.Version = "2" })),
		"a restore the walk skips": contractsOf(restoring)} {
		if err := g.do(evaluate(withContracts(g.env(noReceipts), c), n)); err == nil || g.state(n).Key != nil {
			t.Errorf("%s: the evaluator claimed the walk (%v)", name, err)
		}
	}
	g.must(evaluate(withContracts(g.env(noReceipts), contractsOf(walkContract())), n))
	if g.state(n).Key == nil {
		t.Fatal("the evaluator did not claim a walk under its contract")
	}
	// A subject holding two Refs has no one content to count against.
	ref := g.sealed(types.RefTypeGitTree, "git:"+strings.Repeat("b", 40), "sha256:"+types.Digest(strings.Repeat("e", 64)))
	twoRefs, _ := g.completedWith(types.NodeKindIntegration, types.ResultKindTree, []types.Ref{ref, ref})
	two := g.walkNode("a", 1, 1)
	two.Node.Inputs[0].Source.Variant = &types.ResultSource{Type: "result", Producer: twoRefs, ResultKind: types.ResultKindTree}
	m := g.record(two)
	if g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ClaimEvidence(tx, m, map[string]Resolved{"tree": {Result: *g.state(twoRefs).Result}}, 1, contractsOf(walkContract()))
	}) == nil {
		t.Error("a walk of a subject with two Refs claimed")
	}
	// A due renewal re-checks the contract of a completed walk first; a walk
	// whose receipt is fresh asks for no contract.
	if err := g.recordResult(n, g.evidence(n, nil)); err != nil {
		t.Fatal(err)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, g.results(n)[0]) })
	g.must(evaluate(withContracts(g.env(noReceipts), nil), n))
	expired := g.walk("expired", 1, 2)
	doc := receiptDoc(treeSubject(treeA, demandA), ptr("2026-01-01T00:00:00Z"))
	doc["instrument"] = map[string]any{"name": "walk", "version": "1", "digest": walkDigest}
	doc["predicates"].([]any)[0].(map[string]any)["name"] = "steps"
	d := g.derivedUnder(walkContract(), doc) // the walk's own receipt, expired
	r := g.evidence(expired, nil)
	r.Conclusions, r.Value = ptr(g.conclusions[d]), []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
	if err := g.recordResult(expired, r); err != nil {
		t.Fatal(err)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, expired, r.ID) })
	n = expired
	for name, c := range map[string]Contracts{"no contracts": nil, "a failing registry": failing, "another version": contractsOf(edit(func(c *types.InstrumentContract) { c.Version = "2" }))} {
		if g.do(evaluate(withContracts(g.env(noReceipts), c), n)) == nil {
			t.Errorf("%s: the evaluator renewed a walk outside its contract", name)
		}
	}
	g.must(evaluate(withContracts(g.env(noReceipts), contractsOf(walkContract())), n))
}

// results are n's recorded results.
func (g graphStore) results(n types.NodeID) []types.ResultID {
	var out []types.ResultID
	for _, e := range g.events(ResultRecorded) {
		var p types.ResultRecorded
		if json.Unmarshal(e.Payload, &p) == nil && p.Node == n {
			out = append(out, p.Result.ID)
		}
	}
	return out
}

func (g graphStore) events(kind string) []eventstore.Event {
	var out []eventstore.Event
	_ = g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == kind {
				out = append(out, e)
			}
		}
		return err
	})
	return out
}

// Probe holds a precondition failure on the probe of one of its contract's
// preconditions, and nothing else; Exhaust holds an exhaustion on
// wrong_owner for the project owner, and nothing else.
func TestProbeAndExhaust(t *testing.T) {
	g := openStore(t)
	n := g.walk("a", 2, 2) // the runner fault spends one
	pre, fault, capped := vocab.ReasonPreconditionFailed, vocab.ReasonRunnerFault, vocab.ReasonEvidenceCapExhausted
	failed, faulty := g.evidence(n, &pre), g.evidence(n, &fault)
	if err := errors.Join(g.recordResult(n, failed), g.recordResult(n, faulty)); err != nil {
		t.Fatal(err)
	}
	env := Env{Contracts: contractsOf(walkContract())}
	other := walkContract()
	other.Version = "2"
	probe := func(r types.ResultID, precondition types.Name, env Env) error {
		id, _ := ids.New("hld")
		h := types.NodeHold{ID: types.HoldID(id), Owner: types.OwnerEnvironmentOwner, OwnerRef: ptr("env"), Pages: []types.NodeHoldPagesItem{}}
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Probe(tx, n, r, precondition, h, nil, env) })
	}
	if probe(failed.ID, "disk", env) == nil || probe(faulty.ID, "dns", env) == nil || probe(failed.ID, "dns", Env{Contracts: contractsOf(other)}) == nil {
		t.Fatal("a probe of no contract precondition, of no precondition failure, or outside the contract held the node")
	}
	if err := probe(failed.ID, "dns", env); err != nil {
		t.Fatal(err)
	}
	if h := g.holds(n); len(h) != 1 || h[0].Hold != "precondition_failed" || h[0].SignalInstance != "probe:dns" {
		t.Fatalf("holds %+v", h)
	}
	g.must(clear(n, g.holds(n)[0]))
	if err := g.recordResult(n, g.evidence(n, nil)); err != nil {
		t.Fatal(err)
	}
	spent := g.evidence(n, &capped)
	if err := g.recordResult(n, spent); err != nil {
		t.Fatal(err)
	}
	exhaust := func(r types.ResultID) error {
		return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Exhaust(tx, n, r, "project-owner") })
	}
	retry := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		h := newHold(t, "", types.OwnerNamedOwner, ptr("project-owner"))
		h.SignalInstance = "caps:" + string(spent.ID) // the builder's own signal: Retry still refuses
		return Retry(tx, n, spent.ID, h, nil)
	})
	if exhaust(faulty.ID) == nil || retry == nil {
		t.Fatal("an exhaustion of no exhausted result, or through Retry, held the node")
	}
	if err := exhaust(spent.ID); err != nil {
		t.Fatal(err)
	}
	if h := g.holds(n); len(h) != 1 || h[0].Hold != "wrong_owner" || h[0].SignalInstance != "caps:"+string(spent.ID) || *h[0].OwnerRef != "project-owner" {
		t.Fatalf("holds %+v", h)
	}
}
