// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// recordedIntent is the manifest.intent attempt recorded, from the log.
func recordedIntent(t *testing.T, w *seeder, attempt types.AttemptID) (types.ManifestIntent, bool) {
	t.Helper()
	var mi types.ManifestIntent
	var found bool
	w.view(func(tx *eventstore.Tx) (err error) {
		mi, found, err = intent.RecordedOf(tx, attempt)
		return err
	})
	return mi, found
}

// P22a-5c-2b-1 (I18, I10 consumption; W17): the build's launch records its
// builder's manifest.intent in the launch command, after the attempt it
// binds: one per attempt, for its task and attempt, stated as the
// builder's, its one input the plan's brief, included, at the lease's
// executor, model and destination. The fold checks it as it lands.
func TestTheBuildersIntentIsRecordedAtLaunch(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.untilRun(t)
	a, l := rw.attemptOf(t)
	mi, ok := recordedIntent(t, rw.w, a.ID)
	if !ok || mi.Consumer.Role != "builder" || mi.Attempt == nil || *mi.Attempt != a.ID || mi.Task == nil || *mi.Task != rw.task ||
		mi.Consumer.Executor != l.Executor || len(mi.Inputs) != 1 || len(mi.Included) != 1 || mi.Included[0].Ref.IdentityDigest != mi.Inputs[0].IdentityDigest ||
		!strings.HasPrefix(string(mi.ID), "cmf_") || knowledge.CheckIntent(mi) != nil {
		t.Fatalf("the recorded intent of %s: %v %+v", a.ID, ok, mi)
	}
}

// P22a-5c-2b-1: the fold refuses an intent that is not what the launch
// records, and the launch with it starts nothing: one stated as a
// reviewer's, one of another task or of none, one whose included positions
// skip (knowledge.CheckIntent), and one naming the brief under no
// restriction (Mythos 22a-5c-3 M1: not the input its record names). A
// second intent for an attempt that recorded one is refused too.
func TestAForgedIntentIsRefused(t *testing.T) {
	other := types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2zz")
	for name, edit := range map[string]func(*types.ManifestIntent){
		"stated as a reviewer's":           func(m *types.ManifestIntent) { m.Consumer.Role = "reviewer" },
		"of another task":                  func(m *types.ManifestIntent) { m.Task = &other },
		"of no task":                       func(m *types.ManifestIntent) { m.Task = nil },
		"its brief included at position 1": func(m *types.ManifestIntent) { m.Included[0].Order = 1 },
		"the brief under no restriction": func(m *types.ManifestIntent) {
			r := m.Inputs[0]
			r.Restrictions = []types.Restriction{}
			sealed, err := knowledge.Seal(r)
			if err != nil {
				t.Fatal(err)
			}
			m.Inputs[0], m.Included[0].Ref = sealed, sealed
		},
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t, true, builder(nil, false, nil))
			recordIntent = func(mi types.ManifestIntent) (eventstore.Event, error) {
				mi.Inputs = append([]types.Ref{}, mi.Inputs...)
				mi.Included = append([]types.ManifestIntentIncludedItem{}, mi.Included...)
				edit(&mi)
				return intent.Record(mi)
			}
			t.Cleanup(func() { recordIntent = intent.Record })
			rw.untilRun(t)
			if id, ok, err := firstAttempt(rw); err != nil || ok {
				t.Errorf("an attempt %s started with the forged intent: %v", id, err)
			}
		})
	}
	t.Run("a second for the attempt", func(t *testing.T) {
		rw := newRunWorld(t, true, builder(nil, false, nil))
		rw.untilRun(t)
		a, _ := rw.attemptOf(t)
		mi, ok := recordedIntent(t, rw.w, a.ID)
		if !ok {
			t.Fatal("no intent recorded")
		}
		mi.ID = types.ManifestID(newID(t, "cmf"))
		if err := rw.w.try(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
			ev, err := intent.Record(mi)
			return []eventstore.Event{ev}, err
		}); err == nil {
			t.Error("a second intent for the attempt was recorded")
		}
	})
}

// P22a-5c-2b-1: a launch whose intent its fold refuses starts nothing: the
// launch command is refused whole, no attempt starts, no intent is
// recorded, and the run ends the lease.
func TestALaunchWithARefusedIntentStartsNothing(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	recordIntent = func(mi types.ManifestIntent) (eventstore.Event, error) {
		mi.Consumer.Role = "reviewer" // the fold refuses it: a build's consumer is its builder
		return intent.Record(mi)
	}
	t.Cleanup(func() { recordIntent = intent.Record })
	rw.untilRun(t)
	if id, ok, err := firstAttempt(rw); err != nil || ok {
		t.Fatalf("an attempt %s started: %v", id, err)
	}
	if _, live := leaseOn(rw.w, rw.build); live {
		t.Error("the refused launch left its lease live")
	}
	var leases []string
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		leases, err = tx.Keys(lease.LeaseProjection, "")
		return err
	})
	if len(leases) != 1 {
		t.Errorf("%d leases", len(leases))
	}
	if st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State; st.State != types.NodeStateStateHeld {
		t.Errorf("the build after a refused intent: %s", st.State)
	}
}
