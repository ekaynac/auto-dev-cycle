// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Opus #433 L1 (a): a reviewer the provider refuses for its account is held
// as a build is (§2.6 fault-provider-402, -429; #226 entry 193): its
// session ends with no decision submitted, so the review's result is
// recorded with no attempt (record), could_not_run with the reason the exit
// map gives, and the review is held, as that result's retry wait, on the
// hold the reason names.
//   - 429: quota_reset until the reset (here 300 ms), owned by the system;
//     no relaunch while it holds, and once the reset has come the review is
//     placed again, runs and completes with the reviewer's go;
//   - 402: broker_credit_exhausted on the reviewer's destination, owned by
//     the operator; nothing clears it.
func TestAProviderRefusalHoldsTheReview(t *testing.T) {
	for name, c := range map[string]struct {
		status      int
		reason      vocab.Reason
		hold        string
		owner       types.Owner
		clearsAfter time.Duration // the reset: the review runs again once it has come; 0: never
		refusedRuns int32         // the reviewer's runs that end with no decision (the provider refused them)
	}{
		"a 429": {status: 429, reason: vocab.ReasonQuotaExhausted, hold: vocab.HoldQuotaReset, owner: types.OwnerSystem, clearsAfter: 300 * time.Millisecond, refusedRuns: 1},
		"a 402": {status: 402, reason: vocab.ReasonBrokerCreditExhausted, hold: vocab.HoldBrokerCreditExhausted, owner: types.OwnerOperator, refusedRuns: 1 << 30},
	} {
		t.Run(name, func(t *testing.T) {
			rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
			if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
				t.Fatalf("the gate: %s, holds %+v", v.State.State, v.State.Holds)
			}
			reg := stubRegistry(t, nil, nil)
			rw.reviewRunning(reg, reviewer(goDecision, false, nil))
			var runs atomic.Int32
			reviewerRuns = func() bool { return runs.Add(1) <= c.refusedRuns }
			t.Cleanup(func() { reviewerRuns = nil })
			rw.observed = gateway.Observed{Statuses: []int{c.status}, Reset: c.clearsAfter}
			var review graph.NodeView
			for range 12 {
				rw.passesFor(t, 1, 0)
				if review = reviewNodes(rw)[types.NodeKindReview]; len(reviewResults(rw, review.Node.ID)) > 0 {
					break
				}
			}
			rs := reviewResults(rw, review.Node.ID)
			if len(rs) != 1 || rs[0].Attempt != nil || rs[0].Result.Outcome != types.OutcomeCouldNotRun || rs[0].Result.Reason != c.reason {
				t.Fatalf("the review's results: %+v", rs)
			}
			st := review.State
			if st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].Hold != c.hold || st.Holds[0].Owner != c.owner ||
				st.Holds[0].Reason == nil || *st.Holds[0].Reason != c.reason {
				t.Fatalf("the review: %s, holds %+v", st.State, st.Holds)
			}
			if c.hold == vocab.HoldBrokerCreditExhausted {
				e, _ := reg.Executor(stubReviewer)
				if p, ok := reconcile.ParseCreditProbe(st.Holds[0].SignalInstance); !ok || p.Destination != e.Destination.ID {
					t.Errorf("the credit hold's signal %q, want the reviewer's destination %s's", st.Holds[0].SignalInstance, e.Destination.ID)
				}
			} else if _, ok := reconcile.ParseQuotaReset(st.Holds[0].SignalInstance); !ok {
				t.Errorf("the quota hold's signal %q", st.Holds[0].SignalInstance)
			}
			rw.passesFor(t, 3, 0)
			if st := reviewNodes(rw)[types.NodeKindReview].State; len(reviewResults(rw, review.Node.ID)) != 1 || len(st.Holds) != 1 || runs.Load() != 1 {
				t.Fatalf("while held: %d runs, %d results, %+v", runs.Load(), len(reviewResults(rw, review.Node.ID)), st)
			}
			if c.clearsAfter == 0 {
				return
			}
			rw.observed = gateway.Observed{}
			time.Sleep(c.clearsAfter)
			for range 8 {
				rw.passesFor(t, 1, 0)
				if review = reviewNodes(rw)[types.NodeKindReview]; review.State.State == types.NodeStateStateCompleted {
					break
				}
			}
			if rs := reviewResults(rw, review.Node.ID); review.State.State != types.NodeStateStateCompleted || len(rs) != 2 || runs.Load() != 2 ||
				rs[1].Result.Outcome != types.OutcomeEmpty {
				t.Fatalf("once the reset came: %s, %d runs, results %+v", review.State.State, runs.Load(), rs)
			}
		})
	}
}

// gateSwitch resolves every reference to one image of this platform
// (gateImages) while present is set, and none otherwise.
type gateSwitch struct{ present atomic.Bool }

func (g *gateSwitch) Resolve(ctx context.Context, ref string) (sandbox.Image, error) {
	if !g.present.Load() {
		return sandbox.Image{}, sandbox.ErrImage
	}
	return gateImages{}.Resolve(ctx, ref)
}

// Opus #433 L1 (b): the gate's own resolve finding the image absent (its
// checkout, which accept shares) records could_not_run/
// controller_run_failed and holds the gate as the launch's miss does
// (imageHeld: wrong_owner naming the image), while the image is absent;
// once it resolves, the hold clears and the gate completes. The loop's
// placer and the runner share one image store, as adcd's wiring does.
func TestTheGatesImageHoldClearsOnceItResolves(t *testing.T) {
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"README.md": "hello\n"}, nil)
	images := &gateSwitch{}
	images.present.Store(true)
	rw.r.place.d.Images = images
	pl := rw.x.Placer.(placer)
	pl.d.Images = images
	rw.x.Placer = pl
	acceptLister = func(sandbox.Host, sandbox.Image) accept.Lister { return nil }
	t.Cleanup(func() { acceptLister = defaultLister })
	rw.x.Acceptor = rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	until := func(what string, done func() bool) {
		t.Helper()
		for range 12 {
			if done() {
				return
			}
			rw.passesFor(t, 1, 0)
		}
		if !done() {
			t.Fatalf("%s: the nodes %+v", what, nodesOf(rw.w, rw.task))
		}
	}
	until("accept completing", func() bool {
		return nodesOf(rw.w, rw.task)[types.NodeKindAccept].State.State == types.NodeStateStateCompleted
	})
	images.present.Store(false)
	until("the gate held", func() bool { return len(nodesOf(rw.w, rw.task)[types.NodeKindGate].State.Holds) > 0 })
	gate := nodesOf(rw.w, rw.task)[types.NodeKindGate]
	imageHeld(t, rw, gate.State)
	if got := resultsOn(t, rw.w, gate.Node.ID); len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun ||
		got[0].Result.Reason != vocab.ReasonControllerRunFailed {
		t.Errorf("the gate's results: %+v", got)
	}
	rw.passesFor(t, 3, 0)
	if st := nodesOf(rw.w, rw.task)[types.NodeKindGate].State; len(st.Holds) != 1 || len(resultsOn(t, rw.w, gate.Node.ID)) != 1 {
		t.Fatalf("the image hold, the image still absent: %+v", st)
	}
	images.present.Store(true)
	until("the gate completing", func() bool {
		return nodesOf(rw.w, rw.task)[types.NodeKindGate].State.State == types.NodeStateStateCompleted
	})
}

// Opus #433 L1 (c): when the provider hold's own command fails (its
// retry wait not written), the run still ends the attempt and revokes its
// lease, so the build, claimed with its could_not_run result, holds
// nothing and has no live lease, and the loop places and launches it again:
// never a claim with no lease and no hold (#423's stall).
func TestAFailedProviderHoldPlacesTheBuildAgain(t *testing.T) {
	retryHold = func(*eventstore.Tx, types.NodeID, types.ResultID, types.NodeHold, *string) ([]eventstore.Event, error) {
		return nil, errors.New("the hold's command failed")
	}
	t.Cleanup(func() { retryHold = graph.Retry })
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), true, nil))
	rw.observed = gateway.Observed{Statuses: []int{200, 429}}
	rw.untilRun(t)
	b := nodesOf(rw.w, rw.task)[types.NodeKindBuild]
	if got := resultsOn(t, rw.w, rw.build); len(got) != 1 || got[0].Result.Reason != vocab.ReasonQuotaExhausted {
		t.Fatalf("the build's results: %+v", got)
	}
	if _, live := leaseOn(rw.w, rw.build); b.State.State != types.NodeStateStateClaimed || len(b.State.Holds) != 0 || live {
		t.Fatalf("after the failed hold: %s, holds %+v, a live lease %v", b.State.State, b.State.Holds, live)
	}
	rw.observed = gateway.Observed{}
	for range 3 {
		if rw.runs.count() > 1 {
			break
		}
		rw.passesFor(t, 1, 0)
	}
	if rw.runs.count() != 2 {
		t.Fatalf("the build after the failed hold: %d runs; %+v", rw.runs.count(), nodesOf(rw.w, rw.task)[types.NodeKindBuild].State)
	}
}

// Opus #433 L2: checkout's image resolve (accept's and the gate's) is
// bounded as the launch's is (resolveImage): a podman stuck past
// resolveTimeout is the host's (errHost), held for a bounded retry, never
// the image missing (wrong_owner). Accept's own bound is set past
// resolveTimeout, so an unbounded resolve is cut by it instead, and read as
// the image missing.
func TestAHungImageResolveAtCheckoutIsRetried(t *testing.T) {
	rw := newRunWorld(t, true, builder(writes(map[string]string{"README.md": "hello\n"}), false, nil))
	rw.untilRun(t)
	oldResolve, oldAccept := resolveTimeout, acceptBound
	resolveTimeout, acceptBound = 50*time.Millisecond, 2*time.Second
	t.Cleanup(func() { resolveTimeout, acceptBound = oldResolve, oldAccept })
	rw.images.hang.Store(true)
	v := rw.accepting(t, nil)
	if len(v.State.Holds) != 1 || v.State.Holds[0].Hold != vocab.HoldSourceInvalidConfig || !strings.HasPrefix(v.State.Holds[0].SignalInstance, runnerText("retry 1 until ")) {
		t.Fatalf("accept after a hung resolve: %s, holds %+v", v.State.State, v.State.Holds)
	}
}
