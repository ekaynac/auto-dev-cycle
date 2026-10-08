// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

func target(id types.ResourceGroupID) types.ResourceGroup {
	g := grp(id, types.ResourceGroupKindEvidenceTarget, types.CapacityUnitSlots, lim(1))
	g.ReleaseRule = types.ResourceGroupReleaseRuleOnRestoreVerified
	return g
}

// An evidence target returns on a verified restore, and only it does.
func TestNewReservationsTargets(t *testing.T) {
	if _, err := capacity.NewReservations([]types.ResourceGroup{target(gpu)}); err != nil {
		t.Fatal(err)
	}
	onSettle := target(gpu)
	onSettle.ReleaseRule = types.ResourceGroupReleaseRuleOnSettle
	restoring := grp(gpu, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, lim(1))
	restoring.ReleaseRule = types.ResourceGroupReleaseRuleOnRestoreVerified
	for name, g := range map[string]types.ResourceGroup{"a target on settle": onSettle, "a concurrency group on restore": restoring} {
		if _, err := capacity.NewReservations([]types.ResourceGroup{g}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// raw submits one hand-built lease event: what a forged log line would be.
func (w *world) raw(kind string, l types.Lease, targets []types.ResourceGroupID) error {
	p := map[string]any{"transition": strings.TrimSuffix(kind, "/v1"), "reason": nil, "lease": l, "attempt": nil, "successor": nil, "result": nil, "targets": targets}
	b, _ := json.Marshal(p)
	id, _ := ids.New("evt")
	return w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: id, Kind: kind, Subject: string(l.ID), Payload: b}}, nil
	})
}

func (w *world) recordRestore(r types.RestoreEvidence) error {
	return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RecordRestore(tx, r) })
}

// A lease of a target settles only through a verified restore recorded in
// the log for its attempt and target. A failed restore keeps the target's
// slot, across a restart and a rebuild, so the next lease is refused; a
// verified restore and the settlement return it. A lease of no target owes
// no restore, and settles plainly.
func TestTargetsStayExcludedUntilRestored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	groups := []types.ResourceGroup{target(quota), grp(gpu, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, lim(2))}
	w := openAt(t, dir, hooked, groups...)
	settle := lease.LeaseEvent{Kind: lease.Settled, Settlement: &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}}

	plain := w.requested(reserve(gpu, 1, types.CapacityUnitSlots))
	for _, s := range lifecycle(w, plain, "att_01m3250vm7f6ktw39dszqnyddr")[:4] {
		if err := w.do(s); err != nil {
			t.Fatal(err)
		}
	}
	restoring, err := lease.ApplyLease(leaseOf(t, w, plain), lease.LeaseEvent{Kind: lease.RestoreStarted}, true, w.holds)
	if err != nil {
		t.Fatal(err)
	}
	if w.change(plain, 4, lease.LeaseEvent{Kind: lease.RestoreStarted}) == nil || w.raw(lease.RestoreStarted+"/v1", restoring, nil) == nil {
		t.Fatal("a lease of no target started a restore")
	}
	if err := w.change(plain, 4, settle); err != nil {
		t.Fatalf("a lease of no target did not settle plainly: %v", err)
	}

	att := types.AttemptID("att_01m3250vkwffc979nzjw0g2sf9")
	id := w.requested(reserve(quota, 1, types.CapacityUnitSlots), reserve(gpu, 1, types.CapacityUnitSlots))
	if w.change(id, 1, granted) == nil {
		t.Fatal("a grant naming no target of a target's lease was admitted")
	}
	steps := lifecycle(w, id, att)
	steps[0] = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 1, lease.LeaseEvent{Kind: lease.Granted, Targets: []types.ResourceGroupID{quota}}, w.holds)
	}
	for _, s := range steps[:4] {
		if err := w.do(s); err != nil {
			t.Fatal(err)
		}
	}
	if w.change(id, 4, settle) == nil {
		t.Fatal("a lease of a target settled without a restore")
	}
	if rs, _ := lease.ApplyLease(leaseOf(t, w, id), lease.LeaseEvent{Kind: lease.RestoreStarted}, true, w.holds); w.raw(lease.RestoreStarted+"/v1", rs, []types.ResourceGroupID{quota}) == nil {
		t.Fatal("a transition other than the grant carried targets")
	}
	early := types.RestoreEvidence{Receipt: "rcp_01m3250vm7f6ktw39dszqnyddr", Lease: id, Attempt: att, Group: quota, Cycle: 1, Verified: true}
	if w.recordRestore(early) == nil {
		t.Fatal("a restore was recorded before the lease restores")
	}
	if err := w.change(id, 4, lease.LeaseEvent{Kind: lease.RestoreStarted}); err != nil {
		t.Fatal(err)
	}
	rcpFailed, rcpOK := types.ReceiptID("rcp_01m3250vkwffc979nzjw0g2sf9"), types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
	evidence := func(rcp types.ReceiptID, ok bool) types.RestoreEvidence {
		r := types.RestoreEvidence{Receipt: rcp, Lease: id, Attempt: att, Group: quota, Cycle: 1, Verified: ok}
		if !ok {
			r.Detail = "snapshot gone"
		}
		return r
	}
	foreignAttempt, foreignTarget := evidence("rcp_01m3250vnfeyrv67x6w0ddqr6a", true), evidence("rcp_01m3250v9xft08ssr4ygq6g2np", true)
	foreignAttempt.Attempt, foreignTarget.Group = "att_01m3250vnfeyrv67x6w0ddqr6a", gpu
	if w.recordRestore(foreignAttempt) == nil || w.recordRestore(foreignTarget) == nil {
		t.Fatal("a restore of another attempt or of no target was recorded")
	}
	verify := func(rcp types.ReceiptID) error {
		return w.change(id, 5, lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &rcp})
	}
	if verify(rcpOK) == nil {
		t.Fatal("a restore receipt with no recorded restore verified the target")
	}
	if err := w.recordRestore(evidence(rcpFailed, false)); err != nil {
		t.Fatal(err)
	}
	if verify(rcpFailed) == nil {
		t.Fatal("a failed restore verified the target")
	}
	forged, err := lease.ApplyLease(leaseOf(t, w, id), lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &rcpOK}, true, w.holds)
	if err != nil || w.raw(lease.RestoreVerified+"/v1", forged, nil) == nil {
		t.Fatalf("a forged restore_verified with no recorded restore was accepted (%v)", err)
	}
	hold := &types.LeaseHoldValue{Hold: lease.RestoreHold, SignalInstance: "restore:x"}
	if err := w.change(id, 5, lease.LeaseEvent{Kind: lease.RestoreFailed, Reason: "snapshot gone", Hold: hold}); err != nil {
		t.Fatal(err)
	}
	w.close()
	w = openAt(t, dir, hooked, groups...)
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
	next := w.requested(reserve(quota, 1, types.CapacityUnitSlots))
	exhausted(t, w.change(next, 1, lease.LeaseEvent{Kind: lease.Granted, Targets: []types.ResourceGroupID{quota}}), vocab.ReasonCapacityReservedFull)
	if err := w.recordRestore(evidence(rcpOK, true)); err != nil {
		t.Fatal(err)
	}
	for i, e := range []lease.LeaseEvent{{Kind: lease.RestoreVerified, Receipt: &rcpOK}, settle} {
		if err := w.change(id, int64(6+i), e); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.outstanding(quota); n != 0 {
		t.Fatalf("outstanding %d after the verified settlement", n)
	}
	if err := w.change(next, 1, lease.LeaseEvent{Kind: lease.Granted, Targets: []types.ResourceGroupID{quota}}); err != nil {
		t.Fatal(err)
	}
	checkGroups(t, w, groups...)
}

// checkGroups: the configured groups match the recorded grants; a group
// removed or reclassified after history exists is refused by name, and an
// unreadable grant record is an error.
func checkGroups(t *testing.T, w *world, groups ...types.ResourceGroup) {
	check := func(gs ...types.ResourceGroup) error {
		r, err := capacity.NewReservations(gs)
		if err != nil {
			t.Fatal(err)
		}
		return w.s.View(context.Background(), r.CheckGroups)
	}
	if err := check(groups...); err != nil {
		t.Fatal(err)
	}
	reclassified := grp(quota, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, lim(1))
	for name, c := range map[string]struct {
		gs   []types.ResourceGroup
		want string
	}{
		"a removed group":       {groups[1:], string(quota) + ", which lease"},
		"a removed plain group": {groups[:1], string(gpu) + ", which lease"},
		"a reclassified group":  {[]types.ResourceGroup{reclassified, groups[1]}, string(quota) + " changed kind"},
	} {
		if err := check(c.gs...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, tx.Put(capacity.GrantedProjection, "lea_01m3250vnfeyrv67x6w0ddqr6a", []byte(`{"groups":5}`))
	}); err != nil {
		t.Skipf("no projection write outside a hook: %v", err)
	}
	if check(groups...) == nil {
		t.Fatal("an unreadable grant record was read as matching")
	}
}

// A log written before targets were wired, whose lease.granted carries no
// targets, replays without drift under this build: its grant record is
// `{}`, as it always was.
func TestLegacyGrantReplaysWithoutDrift(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	g := grp(gpu, types.ResourceGroupKindConcurrency, types.CapacityUnitSlots, lim(2))
	legacy := lease.Kinds()
	legacy[lease.Granted+"/v1"] = eventstore.Kind{Schema: "common/defs"} // the store as main wrote it
	w := openKinds(t, dir, hooked, legacy, g)
	id := w.requested(reserve(gpu, 1, types.CapacityUnitSlots))
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := lease.ChangeLease(tx, id, 1, granted, w.holds)
		for i := range evs { // the payload as main wrote it: no targets
			var p map[string]any
			_ = json.Unmarshal(evs[i].Payload, &p)
			delete(p, "targets")
			evs[i].Payload, _ = json.Marshal(p)
		}
		return evs, err
	}); err != nil {
		t.Fatal(err)
	}
	w.close()
	w = openAt(t, dir, hooked, g)
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
	var raw []byte
	if err := w.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		raw, _, err = tx.Get(capacity.GrantedProjection, string(id))
		return err
	}); err != nil || string(raw) != `{}` {
		t.Fatalf("a grant of no target records %s (%v), want {}", raw, err)
	}
	if err := w.change(id, 2, lease.LeaseEvent{Kind: lease.Released}); err == nil {
		t.Log("released") // the legacy lease still moves under this build
	}
}

// targetLease is a target lease of groups, granted, its attempt stopped and
// its restore started (cycle 1).
func (w *world) targetLease(att types.AttemptID, groups ...types.ResourceGroupID) types.LeaseID {
	var res []types.LeaseReservationsItem
	for _, g := range groups {
		res = append(res, reserve(g, 1, types.CapacityUnitSlots))
	}
	id := w.requested(res...)
	steps := lifecycle(w, id, att)
	steps[0] = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 1, lease.LeaseEvent{Kind: lease.Granted, Targets: groups}, w.holds)
	}
	for _, s := range steps[:4] {
		if err := w.do(s); err != nil {
			w.t.Fatal(err)
		}
	}
	if err := w.change(id, 4, lease.LeaseEvent{Kind: lease.RestoreStarted}); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// A success voided in its cycle never verifies: by a later failed restore
// of its target, or by lease.restore_failed; a rebuild agrees.
func TestRestoreCycleVoidsEarlierSuccesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	w := openAt(t, dir, hooked, target(quota))
	att := types.AttemptID("att_01m3250vkwffc979nzjw0g2sf9")
	id := w.targetLease(att, quota)
	r := func(rcp types.ReceiptID, ok bool, cycle int64) types.RestoreEvidence {
		e := types.RestoreEvidence{Receipt: rcp, Lease: id, Attempt: att, Group: quota, Cycle: cycle, Verified: ok}
		if !ok {
			e.Detail = "snapshot gone"
		}
		return e
	}
	r1, r2, r3 := types.ReceiptID("rcp_01m3250vkwffc979nzjw0g2sf9"), types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw"), types.ReceiptID("rcp_01m3250vnfeyrv67x6w0ddqr6a")
	if w.recordRestore(r(r3, true, 2)) == nil {
		t.Fatal("a restore of another cycle was recorded")
	}
	for _, e := range []types.RestoreEvidence{r(r1, true, 1), r(r2, false, 1)} {
		if err := w.recordRestore(e); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(rcp types.ReceiptID, rev int64) error {
		return w.change(id, rev, lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &rcp})
	}
	if verify(r1, 5) == nil {
		t.Fatal("a success voided by a later failure verified the target")
	}
	if err := w.recordRestore(r(r3, true, 1)); err != nil {
		t.Fatal(err)
	}
	if verify(r1, 5) == nil {
		t.Fatal("a voided success verified the target beside a standing one")
	}
	hold := &types.LeaseHoldValue{Hold: lease.RestoreHold, SignalInstance: "restore:x"}
	if err := w.change(id, 5, lease.LeaseEvent{Kind: lease.RestoreFailed, Reason: "snapshot gone", Hold: hold}); err != nil {
		t.Fatal(err)
	}
	w.close()
	w = openAt(t, dir, hooked, target(quota))
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
	if verify(r3, 6) == nil || verify(r1, 6) == nil {
		t.Fatal("a success the cycle's restore_failed voided verified the target after a replay")
	}
	r4 := types.ReceiptID("rcp_01m3250v9xft08ssr4ygq6g2np")
	if err := w.recordRestore(r(r4, true, 1)); err != nil {
		t.Fatal(err)
	}
	if err := verify(r4, 6); err != nil {
		t.Fatalf("a standing success did not verify: %v", err)
	}
}

// Every target the grant recorded needs its verified restore: with only A
// verified, the restore is refused and neither slot returns.
func TestRestoreVerifiesEveryTarget(t *testing.T) {
	b := types.ResourceGroupID("rg_01m3250vz2ezn81cgvbkcn17jd")
	w := open(t, target(quota), target(b))
	att := types.AttemptID("att_01m3250vkwffc979nzjw0g2sf9")
	id := w.targetLease(att, quota, b)
	rcp := types.ReceiptID("rcp_01m3250vkwffc979nzjw0g2sf9")
	if err := w.recordRestore(types.RestoreEvidence{Receipt: rcp, Lease: id, Attempt: att, Group: quota, Cycle: 1, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if w.change(id, 5, lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &rcp}) == nil {
		t.Fatal("a restore of one of two targets verified the lease")
	}
	if w.outstanding(quota) != 1 || w.outstanding(b) != 1 {
		t.Fatalf("a slot returned: %d, %d", w.outstanding(quota), w.outstanding(b))
	}
	rcpB := types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
	if err := w.recordRestore(types.RestoreEvidence{Receipt: rcpB, Lease: id, Attempt: att, Group: b, Cycle: 1, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.change(id, 5, lease.LeaseEvent{Kind: lease.RestoreVerified, Receipt: &rcp}); err != nil {
		t.Fatal(err)
	}
	late := types.RestoreEvidence{Receipt: "rcp_01m3250vnfeyrv67x6w0ddqr6a", Lease: id, Attempt: att, Group: b, Cycle: 1, Verified: true}
	if w.recordRestore(late) == nil {
		t.Fatal("a restore was recorded after the lease's restore was verified")
	}
}
