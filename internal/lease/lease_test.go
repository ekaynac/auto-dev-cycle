// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"errors"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	leaID = types.LeaseID("lea_01m3250v57fhm9v1z9xd1wk8gw")
	untID = types.UnitID("unt_01m3250vz2ezn81cgvbkcn17jd")
	attA  = types.AttemptID("att_01m3250vnfeyrv67x6w0ddqr6a")
	attB  = types.AttemptID("att_01m3250v57ft7aehkqwq0yrq3n")
	rcpID = types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
	calA  = types.CallID("cal_01m3250v9xft08ssr4ygq6g2np")
	resID = types.ResultID("res_01m3250vm7f6ktw39dszqnyddr")
	dgA   = types.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
)

func holds(t *testing.T) map[string]types.Hold {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Holds
}

func requested() types.Lease {
	ok := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	return types.Lease{ID: leaID, Unit: untID, Role: "builder", Executor: "exe_01m3250vnfeyrv67x6w0ddqr6a",
		State:        types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1000, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1000, ExpectedTurns: 5}, Deadline: "2026-10-04T14:00:00Z",
		PlacementChecks: types.LeasePlacementChecks{DataFloor: ok, Destinations: ok, Capability: ok, Independence: ok,
			PaidAuthority: ok, Budget: ok, FallbackScope: ok}}
}

// event returns a well-formed event of each kind.
func event(kind string) LeaseEvent {
	a, r := attA, rcpID
	e := LeaseEvent{Kind: kind}
	switch kind {
	case Waiting:
		e.Hold = &types.LeaseHoldValue{Hold: "quota_reset", SignalInstance: "2026-10-04T15:00:00Z"}
	case Revoked:
		e.Reason = "grant_revoked"
	case StartRefused:
		e.Reason = "precondition_failed"
	case RestoreFailed:
		e.Reason = "restore probe failed"
		e.Hold = &types.LeaseHoldValue{Hold: RestoreHold, SignalInstance: "tgt_01m3250v57fhm9v1z9xd1wk8gw"}
	case AttemptStarted:
		e.Attempt = &a
	case RestoreVerified:
		e.Receipt = &r
	case Settled:
		e.Settlement = &types.LeaseSettlementValue{Calls: []types.CallID{calA}, Liabilities: []types.CallID{}}
	}
	return e
}

var leaseKinds = []string{Requested, Waiting, Granted, StartRefused, AttemptStarted, Expired, Completed, Revoked, Preempted,
	Settled, Released, RestoreStarted, RestoreVerified, RestoreFailed}

var leaseStates = []types.LeaseState{types.LeaseStateRequested, types.LeaseStateWaiting, types.LeaseStateGranted,
	types.LeaseStateActive, types.LeaseStateReleasing, types.LeaseStateSettling, types.LeaseStateRestoring,
	types.LeaseStateRestoreFailed, types.LeaseStateSettlingVerified, types.LeaseStateSettled, types.LeaseStateReleased}

// reach drives a lease to state s along note 05's tables.
func reach(t *testing.T, s types.LeaseState, target bool) types.Lease {
	t.Helper()
	paths := map[types.LeaseState][]string{
		types.LeaseStateRequested:        {},
		types.LeaseStateWaiting:          {Waiting},
		types.LeaseStateGranted:          {Granted},
		types.LeaseStateActive:           {Granted, AttemptStarted},
		types.LeaseStateReleasing:        {Granted, Expired},
		types.LeaseStateReleased:         {Granted, Expired, Released},
		types.LeaseStateSettling:         {Granted, AttemptStarted, Completed},
		types.LeaseStateSettled:          {Granted, AttemptStarted, Completed, Settled},
		types.LeaseStateRestoring:        {Granted, AttemptStarted, Completed, RestoreStarted},
		types.LeaseStateRestoreFailed:    {Granted, AttemptStarted, Completed, RestoreStarted, RestoreFailed},
		types.LeaseStateSettlingVerified: {Granted, AttemptStarted, Completed, RestoreStarted, RestoreVerified},
	}
	if !target && (s == types.LeaseStateRestoring || s == types.LeaseStateRestoreFailed || s == types.LeaseStateSettlingVerified) {
		target = true // only an evidence-target lease reaches these
	}
	path := paths[s]
	if target && s == types.LeaseStateSettled {
		path = []string{Granted, AttemptStarted, Completed, RestoreStarted, RestoreVerified, Settled}
	}
	l := requested()
	for _, k := range path {
		var err error
		if l, err = ApplyLease(l, event(k), target, holds(t)); err != nil {
			t.Fatalf("reaching %s: %s: %v", s, k, err)
		}
	}
	return l
}

func encodes(t *testing.T, schema string, v any) {
	t.Helper()
	if _, err := contract.Encode(schema, v); err != nil {
		t.Errorf("%s: %v", schema, err)
	}
}

// Every (state, event) pair: allowed exactly as note 05's tables say, for
// plain and evidence-target leases; every reachable lease fits its schema;
// terminal states refuse everything.
func TestLeaseTable(t *testing.T) {
	type step = leaseStep
	common := map[step]types.LeaseState{
		{"requested", Waiting}: "waiting", {"waiting", Requested}: "requested", {"requested", Granted}: "granted",
		{"granted", AttemptStarted}: "active", {"granted", Expired}: "releasing", {"granted", StartRefused}: "releasing",
		{"active", Completed}: "settling", {"active", Revoked}: "settling", {"active", Preempted}: "settling",
		{"active", Expired}: "settling", {"releasing", Released}: "released",
	}
	plain := map[step]types.LeaseState{{"settling", Settled}: "settled"}
	restore := map[step]types.LeaseState{
		{"settling", RestoreStarted}: "restoring", {"restoring", RestoreVerified}: "settling_verified",
		{"restoring", RestoreFailed}: "restore_failed", {"restore_failed", RestoreVerified}: "settling_verified",
		{"settling_verified", Settled}: "settled",
	}
	for _, target := range []bool{false, true} {
		allowed := map[leaseStep]types.LeaseState{}
		for k, v := range common {
			allowed[k] = v
		}
		settle := plain
		if target {
			settle = restore
		}
		for k, v := range settle {
			allowed[k] = v
		}
		for _, s := range leaseStates {
			if !target && (s == types.LeaseStateRestoring || s == types.LeaseStateRestoreFailed || s == types.LeaseStateSettlingVerified) {
				continue
			}
			from := reach(t, s, target)
			encodes(t, "records/lease", from)
			for _, k := range leaseKinds {
				got, err := ApplyLease(from, event(k), target, holds(t))
				want, ok := allowed[leaseStep{s, k}]
				switch {
				case ok && err != nil:
					t.Errorf("target=%v %s --%s--> refused: %v", target, s, k, err)
				case ok && got.State != want:
					t.Errorf("target=%v %s --%s--> %s, want %s", target, s, k, got.State, want)
				case ok:
					encodes(t, "records/lease", got)
				case !ok && !errors.Is(err, ErrTransition):
					t.Errorf("target=%v %s --%s--> accepted (%v)", target, s, k, err)
				}
			}
		}
	}
	if _, ok := leaseTable[leaseStep{types.LeaseStateSettling, Settled}]; ok {
		t.Fatal("settling → settled must depend on the evidence target")
	}
}

// Every lease reaches settled or released: from each non-terminal state a
// path to a terminal one exists.
func TestEveryLeaseEnds(t *testing.T) {
	for _, target := range []bool{false, true} {
		for _, s := range leaseStates {
			if !target && (s == types.LeaseStateRestoring || s == types.LeaseStateRestoreFailed || s == types.LeaseStateSettlingVerified) {
				continue
			}
			l := reach(t, s, target)
			for steps := 0; l.State != types.LeaseStateSettled && l.State != types.LeaseStateReleased; steps++ {
				if steps > len(leaseStates) {
					t.Fatalf("target=%v: %s never ends", target, s)
				}
				moved := false
				for _, k := range []string{Requested, Granted, Expired, Released, RestoreStarted, RestoreVerified, Settled} {
					if n, err := ApplyLease(l, event(k), target, holds(t)); err == nil {
						l, moved = n, true
						break
					}
				}
				if !moved {
					t.Fatalf("target=%v: stuck at %s", target, l.State)
				}
			}
		}
	}
}

func TestLeaseEventData(t *testing.T) {
	h := holds(t)
	l := requested()
	bad := event(Waiting)
	bad.Hold = &types.LeaseHoldValue{Hold: "coffee_break", SignalInstance: "x"}
	if _, err := ApplyLease(l, bad, false, h); err == nil {
		t.Error("an unregistered hold")
	}
	if _, err := ApplyLease(l, LeaseEvent{Kind: Waiting, Hold: &types.LeaseHoldValue{Hold: "quota_reset"}}, false, h); err == nil {
		t.Error("a hold without its signal instance")
	}
	w, _ := ApplyLease(l, event(Waiting), false, h)
	if r, _ := ApplyLease(w, event(Requested), false, h); r.Hold != nil {
		t.Error("the hold outlived the wait")
	}
	if _, err := ApplyLease(reach(t, types.LeaseStateGranted, false), LeaseEvent{Kind: AttemptStarted}, false, h); err == nil {
		t.Error("attempt.started without the attempt")
	}
	if _, err := ApplyLease(reach(t, types.LeaseStateRestoring, true), LeaseEvent{Kind: RestoreVerified}, true, h); err == nil {
		t.Error("a verified restore without its receipt")
	}
	// Astra #48: the reasons note 05 requires, and the restore hold.
	if _, err := ApplyLease(reach(t, types.LeaseStateActive, false), LeaseEvent{Kind: Revoked}, false, h); err == nil {
		t.Error("a revocation without its reason")
	}
	if _, err := ApplyLease(reach(t, types.LeaseStateGranted, false), LeaseEvent{Kind: StartRefused}, false, h); err == nil {
		t.Error("a refused start without its reason")
	}
	restoring := reach(t, types.LeaseStateRestoring, true)
	noReason := event(RestoreFailed)
	noReason.Reason = ""
	wrongHold := event(RestoreFailed)
	wrongHold.Hold = &types.LeaseHoldValue{Hold: "quota_reset", SignalInstance: "x"}
	noHold := event(RestoreFailed)
	noHold.Hold = nil
	for name, e := range map[string]LeaseEvent{"no reason": noReason, "another hold": wrongHold, "no hold": noHold} {
		if _, err := ApplyLease(restoring, e, true, h); err == nil {
			t.Errorf("a failed restore with %s", name)
		}
	}
	failed, err := ApplyLease(restoring, event(RestoreFailed), true, h)
	if err != nil || failed.Hold == nil || failed.Hold.Hold != RestoreHold {
		t.Fatalf("a failed restore: %v, %+v", err, failed.Hold)
	}
	if retried, err := ApplyLease(failed, event(RestoreVerified), true, h); err != nil || retried.Hold != nil {
		t.Errorf("the restore hold outlived the verified retry: %v, %+v", err, retried.Hold)
	}
	settling := reach(t, types.LeaseStateSettling, false)
	if _, err := ApplyLease(settling, LeaseEvent{Kind: Settled}, false, h); err == nil {
		t.Error("lease.settled without its settlement")
	}
	// The publication guard: only a verified restore publishes.
	pub := event(Settled)
	r := resID
	pub.Settlement.Published = &r
	if _, err := ApplyLease(settling, pub, false, h); err == nil {
		t.Error("a plain lease published a result")
	}
	verified := reach(t, types.LeaseStateSettlingVerified, true)
	done, err := ApplyLease(verified, pub, true, h)
	if err != nil || done.Settlement.Published == nil || *done.RestoreReceipt != rcpID {
		t.Errorf("a verified lease publishing: %v", err)
	}
	encodes(t, "records/lease", done)
	both := event(Settled)
	both.Settlement.Liabilities = []types.CallID{calA}
	if _, err := ApplyLease(settling, both, false, h); err == nil {
		t.Error("a call both settled and uncertain")
	}
	// Records are returned, never changed in place.
	in := event(Settled)
	out, _ := ApplyLease(settling, in, false, h)
	in.Settlement.Calls[0] = "cal_01m3250v57fhm9v1z9xd1wk8gw"
	if out.Settlement.Calls[0] != calA || settling.Settlement != nil || settling.State != types.LeaseStateSettling {
		t.Error("ApplyLease aliased or changed its input")
	}
}
