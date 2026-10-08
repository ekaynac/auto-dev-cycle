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

// A lease of a target settles only through a verified restore. A failed restore keeps the target's
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
	if err := w.change(id, 4, lease.LeaseEvent{Kind: lease.RestoreStarted}); err != nil {
		t.Fatal(err)
	}
	rcpOK := types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
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
		"a removed group":      {groups[1:], string(quota) + ", which lease"},
		"a reclassified group": {[]types.ResourceGroup{reclassified, groups[1]}, string(quota) + " changed kind"},
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
