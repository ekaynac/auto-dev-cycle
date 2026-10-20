// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/capability"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// withTable runs f with capability.Table changed by edit, then restores it.
func withTable(t *testing.T, edit func([]capability.Capability), f func()) {
	t.Helper()
	saved := slices.Clone(capability.Table)
	capability.Table = slices.Clone(saved)
	edit(capability.Table)
	defer func() { capability.Table = saved }()
	f()
}

// P22a-2 (W7): adcd wires a capability's actions only while it is active.
// Intake and answers are active in this build, so their actions are wired;
// with intake pending, its three actions have no handler and the matrix
// classes them deferred, owned by the item activating intake, while
// answers' stay wired. Every gated action is one capability's.
func TestGatedByCapability(t *testing.T) {
	intake := []string{"project.create", "task.create", "task.cancel"}
	for action := range gated() {
		if _, ok := capability.Gating(action); !ok {
			t.Errorf("%s is gated by no capability", action)
		}
	}
	for _, c := range capability.Table {
		for _, a := range c.Actions {
			if gated()[a] == nil {
				t.Errorf("%s's %s has no handler in adcd", c.Name, a)
			}
		}
	}
	hs := handlers()
	for _, a := range append(slices.Clone(intake), "input.answer") {
		if hs[a] == nil {
			t.Errorf("%s is not wired", a)
		}
	}
	withTable(t, func(tb []capability.Capability) { tb[0].Pending = []string{"a test"} }, func() {
		hs := handlers()
		rows, err := Matrix(mustLoad(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range intake {
			i := slices.IndexFunc(rows, func(r Row) bool { return r.Action == a })
			if hs[a] != nil || i < 0 || rows[i].Class != Deferred || !strings.Contains(rows[i].Owner.Ref, "§1 intake: handler built, activated by P22a") {
				t.Errorf("%s with intake pending: handler %v, row %+v", a, hs[a] != nil, rows[i])
			}
		}
		if hs["input.answer"] == nil {
			t.Error("answers lost input.answer")
		}
	})
}

// adcd registers the effect kinds of the active capabilities alone: none in
// this build (remote_process never, I15 effect); model_call once launch is
// active; an active kind with no definition refuses the start.
func TestEffectKinds(t *testing.T) {
	if ks, err := effectKinds(); err != nil || len(ks) != 0 {
		t.Fatalf("this build: %v %v", ks, err)
	}
	launch := slices.IndexFunc(capability.Table, func(c capability.Capability) bool { return c.Name == "launch" })
	withTable(t, func(tb []capability.Capability) {
		tb[launch].Rows, tb[launch].Pending = nil, nil
	}, func() {
		ks, err := effectKinds()
		if _, ok := ks[modelcall.Kind]; err != nil || len(ks) != 1 || !ok {
			t.Errorf("launch active: %v %v", ks, err)
		}
	})
	withTable(t, func(tb []capability.Capability) {
		tb[launch].Rows, tb[launch].EffectKinds = nil, []string{"remote_process"}
	}, func() {
		if _, err := effectKinds(); err == nil || !strings.Contains(err.Error(), "remote_process") {
			t.Errorf("an undefined active kind: %v", err)
		}
	})
}
