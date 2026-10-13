// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/evidence"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// isolatedProof is a proof that proves attempt's run isolated.
func isolatedProof(attempt types.AttemptID) types.IsolationProof {
	return types.IsolationProof{Run: evidence.RunOf(attempt), Setting: map[string]string{"network": "loopback"}, Interfaces: []string{evidence.Loopback}}
}

func newAttempt() types.AttemptID { return types.AttemptID(must(ids.New("att"))) }

// published is r for n, with an attempt of its own, started on n's unit,
// and the proof of its isolation when n is an evidence node. It runs
// commands: call it outside a transaction.
func (g graphStore) published(n types.NodeID, r types.Result) types.ResultRecorded {
	rr := types.ResultRecorded{Node: n, Result: r}
	if g.recordOf(n).Node.Kind == types.NodeKindEvidence {
		attempt := g.attemptOn(n)
		rr.Attempt, rr.Isolation = &attempt, ptr(isolatedProof(attempt))
	}
	return rr
}

// classedWalk is the walk contract classing trace diagnostic and
// remote-stderr restricted.
func classedWalk() types.InstrumentContract {
	c := walkContract()
	c.ArtifactClasses = map[string]types.InstrumentContractArtifactClassesValue{
		"trace": types.InstrumentContractArtifactClassesValueDiagnostic, "remote-stderr": types.InstrumentContractArtifactClassesValueRestricted}
	return c
}

// walkReceipt publishes a walk receipt about tree A under demand A, citing
// evidence, derived under c (which must declare every artifact it cites).
func (g graphStore) walkReceipt(c types.InstrumentContract, evidence ...any) types.Digest {
	doc := receiptDoc(treeSubject(treeA, demandA), nil)
	doc["instrument"] = map[string]any{"name": c.Name, "version": c.Version, "digest": c.Digest}
	p := doc["predicates"].([]any)[0].(map[string]any)
	p["name"], p["evidence"] = "steps", append([]any{}, evidence...)
	return g.derivedUnder(c, doc)
}

func cites(d types.Digest, artifact string) any {
	return map[string]any{"blob": d, "media_type": "text/plain", "artifact": artifact}
}

// An evidence result is published only with a proof, from its own run's
// wrapper, that proves its isolation: CommitEvidence records any other as
// could_not_run/isolation_unproven, never selected, and the fold refuses
// a published one without its proof, and a second result of one attempt.
func TestCommitEvidenceIsolation(t *testing.T) {
	g := openStore(t)
	n := g.walk("isolated", 9, 9)
	env := withContracts(g.env(noReceipts), contractsOf(walkContract()))
	commitWith := func(n types.NodeID, attempt types.AttemptID, proof types.IsolationProof) (types.Result, error) {
		var got types.Result
		err := g.do(func(tx *eventstore.Tx) (_ []eventstore.Event, err error) {
			r := g.evidence(n, nil)
			r.Value = []types.Ref{g.sealed(types.RefTypeBlob, "trace", g.blob("trace"))}
			got, err = CommitEvidence(tx, n, r, attempt, proof, env)
			return nil, err
		})
		return got, err
	}
	for name, edit := range map[string]func(*types.IsolationProof){
		"an empty setting":             func(p *types.IsolationProof) { p.Setting = map[string]string{} },
		"an empty setting, no network": func(p *types.IsolationProof) { p.Setting, p.Interfaces = map[string]string{}, []string{} },
		"a probe in another wrapper":   func(p *types.IsolationProof) { p.Run = evidence.RunOf(newAttempt()) },
		"the host network seen":        func(p *types.IsolationProof) { p.Interfaces = []string{"eth0", evidence.Loopback} },
		"the host reached":             func(p *types.IsolationProof) { p.HostReachable = true },
		"no setting at all":            func(p *types.IsolationProof) { p.Setting, p.Interfaces = nil, nil },
		"an option without a value":    func(p *types.IsolationProof) { p.Setting = map[string]string{"network": ""} },
	} {
		attempt := g.attemptOn(n)
		proof := isolatedProof(attempt)
		edit(&proof)
		got, err := commitWith(n, attempt, proof)
		if err != nil || got.Outcome != types.OutcomeCouldNotRun || got.Reason != vocab.ReasonIsolationUnproven || len(got.Value) != 0 || g.state(n).State != types.NodeStateStateClaimed {
			t.Errorf("%s: %v, published %+v, node %s", name, err, got, g.state(n).State)
		}
	}
	attempt := g.attemptOn(n)
	if got, err := commitWith(n, attempt, isolatedProof(attempt)); err != nil || got.Outcome != types.OutcomeEmpty || g.state(n).State != types.NodeStateStateCompleted {
		t.Fatalf("the isolated run: %v %+v, node %s", err, got, g.state(n).State)
	}
	// One attempt records one result: its proof publishes nothing else.
	again := g.walk("again", 9, 9)
	if _, err := commitWith(again, attempt, isolatedProof(attempt)); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Errorf("an attempt's proof published a second result: %v", err)
	}

	// The fold refuses, from the log alone, an evidence result with no or a
	// failing proof, and a proof on another kind's result.
	// Each case is on a node of its own, whose attempt is its current,
	// finishing one unless the case says otherwise, so each is refused for
	// its own reason.
	fresh := func(name string) types.ResultRecorded {
		n := g.walk(name, 9, 9)
		return g.published(n, g.evidence(n, nil))
	}
	bare := fresh("bare")
	bare.Attempt, bare.Isolation = nil, nil
	empty := fresh("empty")
	empty.Isolation.Setting = map[string]string{}
	foreign := fresh("foreign")
	foreign.Isolation.Run = evidence.RunOf(newAttempt())
	unattempted := fresh("unattempted")
	unattempted.Attempt = nil
	gate := g.record(g.newNode(types.NodeKindGate))
	g.must(g.claimOf(gate))
	gated := g.attemptOn(gate) // the gate's own attempt: only the proof is wrong
	onGate := types.ResultRecorded{Node: gate, Result: g.evidence(gate, nil), Attempt: &gated, Isolation: ptr(isolatedProof(gated))}
	onGate.Result.Kind, onGate.Result.Conclusions = types.ResultKindGateResult, ptr(conclusions)
	// Nor an attempt never started, or started on another node's unit
	// (#226 entry 3): its result is refused on replay as at admission.
	unstarted := fresh("unstarted")
	unstarted.Attempt = ptr(newAttempt())
	unstarted.Isolation.Run = evidence.RunOf(*unstarted.Attempt)
	elsewhere := fresh("elsewhere")
	elsewhere.Attempt = ptr(g.attemptOn(g.walk("elsewhere-run", 9, 9)))
	elsewhere.Isolation.Run = evidence.RunOf(*elsewhere.Attempt)
	// Nor the result of an attempt that did not submit one: stopped, or
	// superseded by its node's next attempt.
	cut := g.walk("stopped", 9, 9)
	stopped := g.published(cut, g.evidence(cut, nil))
	stopped.Attempt = ptr(g.runningOn(cut))
	stopped.Isolation.Run = evidence.RunOf(*stopped.Attempt)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StopRunning(tx, *stopped.Attempt, types.AttemptStopReasonValueRevoke)
	})
	retired := fresh("retired")
	g.attemptOn(retired.Node)
	for name, c := range map[string]struct {
		rr   types.ResultRecorded
		want string
	}{
		"no proof":                    {bare, "published only with its run's isolation proof"},
		"an empty setting":            {empty, "the isolation setting is empty"},
		"a proof from another run":    {foreign, "the isolation probe ran in"},
		"a proof without its attempt": {unattempted, "published only with its run's isolation proof"},
		"a proof on a gate result":    {onGate, "only an evidence result carries an isolation proof"},
		"an attempt never started":    {unstarted, "never started"},
		"another node's attempt":      {elsewhere, "not node"},
		"a stopped attempt's":         {stopped, "is stopping"},
		"a superseded attempt's":      {retired, "is superseded"},
	} {
		rr := c.rr
		rr.Type = "result_recorded"
		if err := g.do(forge(ResultRecorded, string(rr.Result.ID), prjID, &rr)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the fold published it, or refused it for another reason than %q: %v", name, c.want, err)
		}
	}
	// A bound, submitted attempt's own result with its proof is published.
	own := fresh("own")
	own.Type = "result_recorded"
	g.must(forge(ResultRecorded, string(own.Result.ID), prjID, &own))
}

// renderOn records r on walk n, selects it, claims a review taking it and
// renders the review's controller context under contracts.
func (g graphStore) renderOn(n types.NodeID, r types.Result, c Contracts) (Rendered, error) {
	g.t.Helper()
	rr := g.published(n, r)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return recordResult(tx, rr, g.blobs) })
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, n, r.ID) })
	review := g.consumerOf(n, types.ResultKindEvidence)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, review, map[string]Resolved{"gate": {Result: r.ID}}, 1)
	})
	var out Rendered
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		out, err = Context(tx, review, Env{Blobs: g.blobs, Authority: owedanswers.Authority{}, Contracts: c})
		return err
	})
	return out, err
}

// A controller context renders an evidence result's artifacts by its
// node's contract: the receipt (by identity) and a diagnostic artifact are
// included; a restricted one, one the contract does not class, a blob
// named under two classes, a ref the receipt does not name and a second
// receipt-typed ref are omitted as restricted. Without the contracts, the
// context is refused, never rendered whole.
func TestContextContentClasses(t *testing.T) {
	g := openStore(t)
	n := g.walk("classed", 3, 3)
	trace, stderr, dump, both, both2, loose := g.blob("trace"), g.blob("stderr"), g.blob("dump"), g.blob("both"), g.blob("both2"), g.blob("loose")
	declaring := classedWalk() // declares dump too; the registered contract does not class it
	declaring.ArtifactClasses["dump"] = types.InstrumentContractArtifactClassesValueDiagnostic
	d := g.walkReceipt(declaring, cites(trace, "trace"), cites(stderr, "remote-stderr"), cites(dump, "dump"),
		cites(both, "trace"), cites(both, "remote-stderr"), cites(both2, "remote-stderr"), cites(both2, "trace"))
	r := g.evidence(n, nil)
	r.Conclusions = ptr(g.conclusions[d])
	r.Value = []types.Ref{g.sealed(types.RefTypeBlob, "trace", trace), g.sealed(types.RefTypeReceipt, rcpID, d), g.sealed(types.RefTypeBlob, "stderr", stderr),
		g.sealed(types.RefTypeBlob, "dump", dump), g.sealed(types.RefTypeBlob, "both", both), g.sealed(types.RefTypeBlob, "both2", both2),
		g.sealed(types.RefTypeBlob, "loose", loose)}
	got, err := g.renderOn(n, r, contractsOf(classedWalk()))
	if err != nil {
		t.Fatal(err)
	}
	if want := []types.Ref{r.Value[0], r.Value[1]}; !reflect.DeepEqual(got.Included, want) {
		t.Errorf("included %+v, want the trace and the receipt", got.Included)
	}
	var omitted []types.Ref
	for _, o := range got.Omitted {
		if o.Reason != types.ManifestIntentOmittedItemReasonRestricted {
			t.Errorf("omitted %s as %s", o.Ref.ID, o.Reason)
		}
		omitted = append(omitted, o.Ref)
	}
	if !reflect.DeepEqual(omitted, r.Value[2:]) {
		t.Errorf("omitted %+v, want stderr, dump, both, both2 and loose", omitted)
	}
	for name, c := range map[string]Contracts{"no contracts": nil, "a failing registry": func(types.InstrumentRef) (types.InstrumentContract, error) {
		return types.InstrumentContract{}, errors.New("down")
	}} {
		w := g.walk(name, 3, 3)
		if out, err := g.renderOn(w, withValue(g.evidence(w, nil), r), c); err == nil || out.Included != nil {
			t.Errorf("%s: rendered %+v", name, out)
		}
	}
}

// withValue is r with src's value and conclusions.
func withValue(r, src types.Result) types.Result {
	r.Value, r.Conclusions = src.Value, src.Conclusions
	return r
}

// The receipt is recognised by identity, not by its Ref's type, and must
// be the evidence node's instrument's: a restricted blob typed as a second
// receipt is refused with the result's receipt, and a receipt of another
// instrument is refused when the result is recorded.
func TestContextReceiptIdentity(t *testing.T) {
	g := openStore(t)
	n := g.walk("typed", 3, 3)
	stderr := g.blob("stderr")
	d := g.walkReceipt(classedWalk(), cites(stderr, "remote-stderr"))
	r := g.evidence(n, nil)
	r.Conclusions = ptr(g.conclusions[d])
	r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d), g.sealed(types.RefTypeReceipt, must(ids.New("rcp")), stderr)}
	if out, err := g.renderOn(n, r, contractsOf(classedWalk())); err == nil || slicesHas(out.Included, stderr) {
		t.Errorf("a second receipt-typed ref: %v %+v", err, out)
	}
	m := g.walk("other-instrument", 3, 3)
	lax := goTest
	lax.ArtifactClasses = map[string]types.InstrumentContractArtifactClassesValue{"remote-stderr": types.InstrumentContractArtifactClassesValueDiagnostic}
	doc := receiptDoc(treeSubject(treeA, demandA), nil)
	doc["predicates"].([]any)[0].(map[string]any)["evidence"] = []any{cites(stderr, "remote-stderr")}
	other := g.derivedUnder(lax, doc)
	s := g.evidence(m, nil)
	s.Conclusions, s.Value = ptr(g.conclusions[other]), []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, other), g.sealed(types.RefTypeBlob, "stderr", stderr)}
	held := g.published(m, s)
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return recordResult(tx, held, g.blobs) }); err == nil {
		t.Error("an evidence result holding another instrument's receipt was recorded")
	}
	// The fold reads no blobs: a raw event holding it is refused by Context.
	rr := g.published(m, s)
	rr.Type = "result_recorded"
	g.must(forge(ResultRecorded, string(s.ID), prjID, &rr))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return selectResult(tx, m, s.ID) })
	review := g.consumerOf(m, types.ResultKindEvidence)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return claimAny(tx, review, map[string]Resolved{"gate": {Result: s.ID}}, 1)
	})
	err := g.s.View(context.Background(), func(tx *eventstore.Tx) error {
		out, err := Context(tx, review, Env{Blobs: g.blobs, Authority: owedanswers.Authority{}, Contracts: contractsOf(classedWalk())})
		if slicesHas(out.Included, stderr) {
			t.Error("another instrument's diagnostic class rendered the walk's restricted artifact")
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not the evidence node's instrument") {
		t.Errorf("a forged result with another instrument's receipt: %v", err)
	}
}

func slicesHas(refs []types.Ref, d types.Digest) bool {
	for _, r := range refs {
		if r.ContentDigest == d {
			return true
		}
	}
	return false
}
