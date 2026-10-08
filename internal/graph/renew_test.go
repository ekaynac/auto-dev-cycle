// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var (
	past   = "2026-01-01T00:00:00Z" // before every event here
	future = "2099-01-01T00:00:00Z" // after every event here
)

// recordResult records r for n.
func (g graphStore) recordResult(n types.NodeID, r types.Result) error {
	return g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
	})
}

// A could_not_run evidence result retries on its probe: the retry wait's
// signal clears it, and the node, still claimed on the same key, records
// and selects its next attempt. No invalidation, no new generation.
func TestRetryKeepsTheKey(t *testing.T) {
	g := openStore(t)
	n := g.record(g.newNode(types.NodeKindEvidence))
	g.must(g.claimOf(n))
	key, command := *g.state(n).Key, g.claimed(n).Command
	failed := unrecorded(t, key)
	failed.Kind, failed.Outcome, failed.Reason, failed.Detail, failed.Completed, failed.Value = types.ResultKindEvidence, types.OutcomeCouldNotRun, vocab.ReasonPreconditionFailed, "probe", false, []types.Ref{}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: failed}, g.blobs)
	})
	h := newHold(t, "", types.OwnerEnvironmentOwner, ptr("staging-owner"))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Retry(tx, n, failed.ID, h, nil) })
	if st := g.state(n); st.State != types.NodeStateStateHeld || st.Holds[0].Hold != "precondition_failed" {
		t.Fatalf("the retry wait: %+v", st)
	}
	g.must(clear(n, h))
	r := g.selectGate(n)
	if st := g.state(n); st.State != types.NodeStateStateCompleted || *st.Result != r.ID || *st.Key != key || *g.claimed(n).Command != *command || g.count(NodeInvalidated) != 0 {
		t.Fatalf("the retry: %+v", st)
	}
}

// Renewal reopens the same key once the selected result's receipt has
// expired: the holder records and selects a fresher result at the same
// generation, with the same claim; its reuses are released and bind again,
// and the dependants resolve the fresher result. A rebuild agrees.
func TestRenew(t *testing.T) {
	g := openStore(t)
	holder, old := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	b := g.record(g.nodeLikeGate(holder))
	g.must(reuseOf(b, g.planRes))
	key, command := *g.state(holder).Key, g.claimed(holder).Command
	onHolder, onB := g.reviewOf(holder), g.reviewOf(b)
	if !g.refused(onB, gateOn(treeA, demandA), vocab.ReasonEvidenceInapplicable) {
		t.Fatal("an expired reuse applies")
	}
	var reused ErrReused
	if err := g.do(renew(b, g)); !errors.As(err, &reused) || reused.Holder != holder {
		t.Fatalf("a reused node renewed: %v", err)
	}
	fresh, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &future))
	never, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), nil))
	waiting := g.record(g.newNode(types.NodeKindGate))
	for name, n := range map[string]types.NodeID{"a fresh receipt": fresh, "a receipt with no limit": never, "a node not completed": waiting} {
		if err := g.do(renew(n, g)); err == nil {
			t.Errorf("%s renewed", name)
		}
	}
	if err := g.do(renew(fresh, g)); err == nil || !strings.Contains(err.Error(), "is fresh until") {
		t.Errorf("the builder's expiry guard: %v", err)
	}
	g.must(renew(holder, g))
	if st := g.state(holder); st.State != types.NodeStateStateClaimed || st.Result != nil || *st.Key != key || *g.claimed(holder).Command != *command {
		t.Fatalf("the renewed holder: %+v", st)
	}
	if st := g.state(b); st.State != types.NodeStateStateWaiting || !g.waits(onHolder, gateOn(treeA, demandA)) {
		t.Fatalf("the reuse during the renewal: %+v", st)
	}
	// The renewal is a boundary: the renewed result is not selected again,
	// so the node is not completed with it and does not renew again.
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, holder, old.ID) }); err == nil {
		t.Fatal("the renewed result selected again")
	}
	if err := g.do(renew(holder, g)); err == nil {
		t.Fatal("a renewal renewed again")
	}
	r := g.selectGate(holder)
	g.must(reuseOf(b, g.planRes))
	for _, d := range []types.NodeID{onHolder, onB} {
		if res, miss := g.resolve(d, gateOn(treeA, demandA), at); len(miss) != 0 || res["gate"].Result != r.ID || r.ID == old.ID {
			t.Errorf("a dependant after the renewal: %v %+v", res, miss)
		}
	}
	if g.count(NodeInvalidated) != 0 || g.count(NodeRenewed) != 1 || *g.state(b).Key != key {
		t.Error("the renewal moved the key")
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

func renew(n types.NodeID, g graphStore) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Renew(tx, n, g.blobs) }
}

// The fold refuses a renewal its state does not derive.
func TestRenewRefusals(t *testing.T) {
	g := openStore(t)
	digest := g.receiptBlob(treeSubject(treeA, demandA), &past)
	holder, r := g.gated(digest)
	key := *g.state(holder).Key
	b := g.record(g.nodeLikeGate(holder))
	g.must(reuseOf(b, g.planRes))
	build := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(build))
	built := g.result(build, true)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, build, built.ID) })
	renewal := func(subject string, project types.ProjectID, n types.NodeID, k types.ComputationKey, res types.ResultID, rcpt types.Digest, until string) logic {
		return forge(NodeRenewed, subject, project, &types.NodeRenewed{Type: "node_renewed", Node: n, Key: k, Result: res, Receipt: rcpt, ValidUntil: types.Time(until)})
	}
	_, twin := g.gated(digest) // another result holding the same receipt
	k2 := must(Key(g.newNodeLike(holder), resolutions(g.claimed(holder).Inputs), 2))
	for name, fn := range map[string]logic{
		"a renewal under another subject":  renewal(string(b), prjID, holder, key, r.ID, digest, past),
		"a renewal under node.reused":      forge(NodeReused, string(holder), prjID, &types.NodeRenewed{Type: "node_renewed", Node: holder, Key: key, Result: r.ID, Receipt: digest, ValidUntil: types.Time(past)}),
		"a renewal in another project":     renewal(string(holder), prjB, holder, key, r.ID, digest, past),
		"a renewal of another key":         renewal(string(holder), prjID, holder, k2, r.ID, digest, past),
		"a renewal of another result":      renewal(string(holder), prjID, holder, key, twin.ID, digest, past),
		"a renewal of another receipt":     renewal(string(holder), prjID, holder, key, r.ID, diff, past),
		"a renewal of an unbound validity": renewal(string(holder), prjID, holder, key, r.ID, digest, "2025-01-01T00:00:00Z"),
		"a renewal of another generation":  renewal(string(holder), prjID, holder, types.ComputationKey{Key: key.Key, Generation: 2}, r.ID, digest, past),
		"a renewal of a reused node":       renewal(string(b), prjID, b, key, r.ID, digest, past),
		"a renewal of a build":             renewal(string(build), prjID, build, *g.state(build).Key, built.ID, digest, past),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	g.must(renewal(string(holder), prjID, holder, key, r.ID, digest, past))
	if err := g.do(renewal(string(holder), prjID, holder, key, r.ID, digest, past)); err == nil {
		t.Error("a node renewed twice")
	}
	freshDigest := g.receiptBlob(treeSubject(treeA, demandA), &future)
	freshHolder, fr := g.gated(freshDigest)
	if err := g.do(renewal(string(freshHolder), prjID, freshHolder, *g.state(freshHolder).Key, fr.ID, freshDigest, future)); err == nil {
		t.Error("a renewal before the expiry")
	}
	if err := g.do(renew(b, g)); err == nil {
		t.Error("a reused node renewed while its holder renews")
	}
}

// A receipt has expired at its valid_until itself.
func TestExpiredAt(t *testing.T) {
	if expired, err := expiredAt(types.Time(past), past); err != nil || !expired {
		t.Errorf("at valid_until: %v %v", expired, err)
	}
}

// After a renewal the node selects only a result recorded after it: not
// the renewed blocking result, nor an older clean one with a long validity
// recorded before it (Astra's rollback).
func TestRenewalIsASelectionBoundary(t *testing.T) {
	g := openStore(t)
	rec := g.newNode(types.NodeKindGate)
	rec.Node.Params = json.RawMessage(`{"node":"` + string(rec.Node.ID) + `"}`)
	n := g.record(rec)
	g.must(g.claimOf(n))
	result := func(until string, outcome types.Outcome, reason vocab.Reason) types.Result {
		d := g.receiptBlob(treeSubject(treeA, demandA), &until)
		r := unrecorded(t, *g.state(n).Key)
		r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions = types.ResultKindGateResult, outcome, reason, "", ptr(g.conclusions[d])
		if outcome != types.OutcomeEmpty {
			r.Detail = "blocking"
		}
		r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return RecordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
		})
		return r
	}
	long := result(future, types.OutcomeEmpty, vocab.ReasonCompletedClean)
	short := result(past, types.OutcomeFound, vocab.ReasonCompletedWithFindings)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, short.ID) })
	g.must(renew(n, g))
	for name, id := range map[string]types.ResultID{"the renewed result": short.ID, "an older result": long.ID} {
		if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, id) }); err == nil {
			t.Errorf("%s selected after the renewal", name)
		}
	}
	if r := g.selectGate(n); *g.state(n).Result != r.ID {
		t.Error("a result recorded after the renewal is not selected")
	}
}

// A receipt-bearing review renews: its conclusions are null, its receipt
// still has to be whole and the Ref's.
func TestReviewRenews(t *testing.T) {
	g := openStore(t)
	d := g.receiptBlob(treeSubject(treeA, demandA), &past)
	n, _ := g.completedWith(types.NodeKindReview, types.ResultKindReview, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)})
	g.must(renew(n, g))
	if st := g.state(n); st.State != types.NodeStateStateClaimed {
		t.Fatalf("the renewed review: %+v", st)
	}
}

// A fresh receipt whose result.recorded event forged a past validity never
// serves: the resolver and the builder re-read the blob. A forged renewal of
// it only lets a replacement, recorded after it, be selected and resolve.
func TestForgedExpiry(t *testing.T) {
	g := openStore(t)
	d := g.receiptBlob(treeSubject(treeA, demandA), &future)
	n, _ := g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}, ptr(types.Time(past)))
	review := g.reviewOf(n)
	if !g.refused(review, gateOn(treeA, demandA), vocab.ReasonReceiptIncomplete) {
		t.Error("a forged binding serves")
	}
	unlimited := g.receiptBlob(treeSubject(treeA, demandA), nil)
	m, _ := g.forgedWith(types.NodeKindGate, types.ResultKindGateResult, []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, unlimited)}, ptr(types.Time(past)))
	if !g.refused(g.reviewOf(m), gateOn(treeA, demandA), vocab.ReasonReceiptIncomplete) {
		t.Error("a forged limit on a receipt with none serves")
	}
	if err := g.do(renew(n, g)); err == nil {
		t.Error("the builder renewed a forged binding")
	}
	st := g.state(n)
	g.must(forge(NodeRenewed, string(n), prjID, &types.NodeRenewed{Type: "node_renewed", Node: n, Key: *st.Key, Result: *st.Result, Receipt: d, ValidUntil: types.Time(past)}))
	r := g.selectGate(n)
	if res, miss := g.resolve(review, gateOn(treeA, demandA), at); len(miss) != 0 || res["gate"].Result != r.ID {
		t.Errorf("the replacement: %v %+v", res, miss)
	}
}

// RecordResult binds the validity of the one receipt it reads, never the
// caller's; the fold refuses a binding with no receipt; a later renewal's
// boundary forgets the results recorded before it.
func TestRecordResultBindsTheReceipt(t *testing.T) {
	g := openStore(t)
	n := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(n))
	plain := unrecorded(t, *g.state(n).Key)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: n, Result: plain, ReceiptValidUntil: ptr(types.Time(past))}, g.blobs)
	})
	if r, _ := view[types.ResultRecorded](g, ResultProjection, string(plain.ID)); r.ReceiptValidUntil != nil {
		t.Errorf("the caller's binding: %v", *r.ReceiptValidUntil)
	}
	gate := g.record(g.newNode(types.NodeKindGate))
	g.must(g.claimOf(gate))
	held := unrecorded(t, *g.state(gate).Key)
	held.Kind, held.Outcome, held.Reason, held.Detail, held.Value = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, diff)}
	held.Conclusions = ptr(conclusions)
	for name, blobs := range map[string]receipt.Blobs{"no blob": g.blobs, "no blob store": nil} {
		if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return RecordResult(tx, types.ResultRecorded{Node: gate, Result: held}, blobs)
		}); err == nil {
			t.Errorf("a receipt Ref with %s recorded", name)
		}
	}
	bare := unrecorded(t, *g.state(n).Key)
	if err := g.do(forge(ResultRecorded, string(bare.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: n, Result: bare, ReceiptValidUntil: ptr(types.Time(past))})); err == nil {
		t.Error("a binding with no receipt recorded")
	}
}

// A second renewal sets a new boundary: a result recorded after the first
// renewal but before the second is not selected after the second.
func TestRenewalBoundaryMoves(t *testing.T) {
	g := openStore(t)
	n, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	g.must(renew(n, g))
	between := func(until string) types.Result {
		d := g.receiptBlob(treeSubject(treeA, demandA), &until)
		r := unrecorded(t, *g.state(n).Key)
		r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", ptr(g.conclusions[d])
		r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, d)}
		g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return RecordResult(tx, types.ResultRecorded{Node: n, Result: r}, g.blobs)
		})
		return r
	}
	x, y := between(future), between(past)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, y.ID) })
	g.must(renew(n, g))
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, x.ID) }); err == nil {
		t.Error("a result from before the second renewal selected")
	}
}

// A result recorded after a renewal needs a receipt produced after it: an
// older receipt, still valid, is refused by RecordResult and, through a
// forged record, by the resolver.
func TestRenewalNeedsAFreshReceipt(t *testing.T) {
	g := openStore(t)
	n, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	review := g.reviewOf(n)
	doc := receiptDoc(treeSubject(treeA, demandA), &future)
	doc["produced_at"] = "2026-10-05T12:00:00Z" // before the renewal
	old := g.derived(doc)
	g.must(renew(n, g))
	r := unrecorded(t, *g.state(n).Key)
	r.Kind, r.Outcome, r.Reason, r.Detail, r.Conclusions = types.ResultKindGateResult, types.OutcomeEmpty, vocab.ReasonCompletedClean, "", ptr(g.conclusions[old])
	r.Value = []types.Ref{g.sealed(types.RefTypeReceipt, rcpID, old)}
	if err := g.recordResult(n, r); err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("an old receipt recorded after the renewal: %v", err)
	}
	g.must(forge(ResultRecorded, string(r.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: n, Result: r, ReceiptValidUntil: ptr(types.Time(future))}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Select(tx, n, r.ID) })
	if !g.refused(review, gateOn(treeA, demandA), vocab.ReasonEvidenceInapplicable) {
		t.Error("an old receipt serves after the renewal")
	}
}

// The boundary does not outlive the key: renewed, then invalidated, the
// node records and selects at the new generation.
func TestRenewThenInvalidate(t *testing.T) {
	g := openStore(t)
	n, _ := g.gated(g.receiptBlob(treeSubject(treeA, demandA), &past))
	g.must(renew(n, g))
	g.must(invalidate(n))
	r := g.selectGate(n)
	if st := g.state(n); st.State != types.NodeStateStateCompleted || *st.Result != r.ID || st.Key.Generation != 2 {
		t.Fatalf("after renew and invalidate: %+v", st)
	}
}
