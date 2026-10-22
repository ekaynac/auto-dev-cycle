// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
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
// skip (knowledge.CheckIntent), one naming the brief under no restriction
// (Mythos 22a-5c-3 M1: not the input its record names), and each that
// misstates what the log holds (Opus 22a-5c-2b-1 M1, M2, L1): no inputs,
// the brief omitted, another executor, another rendered digest or size,
// another data policy digest, reported routing or a destination the
// brief's restriction does not allow, tokens over the lease's reservation,
// and included bytes over the budget. A second intent for an attempt that
// recorded one is refused too.
func TestAForgedIntentIsRefused(t *testing.T) {
	other := types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2zz")
	for name, edit := range map[string]func(*types.ManifestIntent){
		"stated as a reviewer's":           func(m *types.ManifestIntent) { m.Consumer.Role = "reviewer" },
		"of another task":                  func(m *types.ManifestIntent) { m.Task = &other },
		"of no task":                       func(m *types.ManifestIntent) { m.Task = nil },
		"its brief included at position 1": func(m *types.ManifestIntent) { m.Included[0].Order = 1 },
		"no inputs": func(m *types.ManifestIntent) {
			m.Inputs, m.Included, m.Omitted = []types.Ref{}, []types.ManifestIntentIncludedItem{}, []types.ManifestIntentOmittedItem{}
		},
		"the brief omitted": func(m *types.ManifestIntent) {
			m.Omitted = []types.ManifestIntentOmittedItem{{Ref: m.Included[0].Ref, Reason: types.ManifestIntentOmittedItemReasonBudget}}
			m.Included = []types.ManifestIntentIncludedItem{}
		},
		"another executor":        func(m *types.ManifestIntent) { m.Consumer.Executor = types.ExecutorID(newID(t, "exe")) },
		"another rendered digest": func(m *types.ManifestIntent) { m.RenderedDigest = types.Digest("sha256:" + strings.Repeat("0", 64)) },
		"another rendered size":   func(m *types.ManifestIntent) { m.Included[0].Bytes++ },
		"another policy digest": func(m *types.ManifestIntent) {
			m.Policy.DataPolicyDigest = "sha256:" + strings.Repeat("1", 64)
		},
		"reported routing":                func(m *types.ManifestIntent) { m.Routing = types.ManifestIntentRoutingReported },
		"a destination not allowed":       func(m *types.ManifestIntent) { m.Destination = "elsewhere" },
		"tokens of 2^40":                  func(m *types.ManifestIntent) { m.Budget.Tokens = 1 << 40 },
		"included over its budget":        func(m *types.ManifestIntent) { m.Budget.Bytes = m.Included[0].Bytes - 1 },
		"a budget not the prompt's bound": func(m *types.ManifestIntent) { m.Budget.Bytes = intent.MaxPrompt + 1 },
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

// Opus 22a-5c-2b-1 M1: the intent's call is the attempt's: an intent
// recorded under another call than the one the builder runs under is
// accepted at the launch (no fact of the call is recorded yet), but the
// attempt's result, submitted under its real call, is refused, so the
// attempt records no result of its own; and the run is abandoned with no
// diff, its reason named (Mythos 22a-5c-2b-1 L2): the only result of the
// build is could_not_run/precondition_failed, recorded with no attempt.
func TestTheIntentsCallIsTheAttempts(t *testing.T) {
	rw := newRunWorld(t, true, builder(func(ws string) {
		_ = os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n\nconst X = 1\n"), 0o600)
	}, false, nil))
	recordIntent = func(mi types.ManifestIntent) (eventstore.Event, error) {
		mi.Call = types.CallID(newID(t, "cal"))
		return intent.Record(mi)
	}
	t.Cleanup(func() { recordIntent = intent.Record })
	rw.untilRun(t)
	a, _ := rw.attemptOf(t)
	rw.w.view(func(tx *eventstore.Tx) error {
		if _, byAttempt, err := graph.AttemptResult(tx, a.ID); err != nil || byAttempt {
			t.Errorf("the attempt under another call than its intent's recorded its result: %v", err)
		}
		return nil
	})
	got := resultsOn(t, rw.w, rw.build)
	if len(got) != 1 || got[0].Attempt != nil || got[0].Result.Outcome != types.OutcomeCouldNotRun || got[0].Result.Reason != vocab.ReasonPreconditionFailed ||
		len(got[0].Result.Value) != 0 || !strings.Contains(got[0].Result.Detail, "another call") {
		t.Errorf("the build's results: %+v", got)
	}
}

// resultsOn are the results recorded for node, in no order.
func resultsOn(t *testing.T, w *seeder, node types.NodeID) []types.ResultRecorded {
	t.Helper()
	var out []types.ResultRecorded
	w.view(func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(graph.ResultProjection, "")
		for _, k := range keys {
			raw, _, gerr := tx.Get(graph.ResultProjection, k)
			var rr types.ResultRecorded
			if gerr == nil && json.Unmarshal(raw, &rr) == nil && rr.Node == node {
				out = append(out, rr)
			}
		}
		return err
	})
	return out
}

// Mythos 22a-5c-2b-1 L1: a model call bound to the attempt under another
// call than its intent's (model_call.intended) is refused.
func TestAModelCallUnderAnotherCallIsRefused(t *testing.T) {
	var rw *runWorld
	var intendErr error
	rw = newRunWorld(t, true, builder(func(string) {
		intendErr = rw.w.try(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			first, ok, err := lease.FirstOn(tx, graph.UnitOf(templateNodes(rw.w, rw.task)[types.NodeKindBuild]))
			if err != nil || !ok {
				return nil, errors.Join(err, errors.New("no attempt"))
			}
			a, _, _, err := lease.AttemptAt(tx, first)
			if err != nil {
				return nil, err
			}
			return modelcall.Intend(tx, gateway.Binding{Lease: a.Lease, Attempt: a.ID, Call: types.CallID(newID(t, "cal"))}, "stub")
		})
	}, false, nil))
	rw.untilRun(t)
	if intendErr == nil || !strings.Contains(intendErr.Error(), intent.ErrUnbound.Error()) {
		t.Errorf("a model call under another call than the intent's: %v", intendErr)
	}
}

// Mythos 22a-5c-2b-1 M1: the launch command refuses an intent that does
// not state its lease's executor as the registry registers it: another
// destination (one the brief's restriction allows) or another model.
func TestAnIntentNotAsRegisteredIsRefused(t *testing.T) {
	for name, edit := range map[string]func(*types.ManifestIntent){
		"destination other": func(m *types.ManifestIntent) { m.Destination = "other" },
		"another model":     func(m *types.ManifestIntent) { m.Consumer.ModelSnapshot = "another-model" },
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorldWith(t, true, builder(nil, false, nil), func(p map[string]any) {
				p["data_policy"].(map[string]any)["allowed_destinations"] = []string{"other", "stub"}
			})
			recordIntent = func(mi types.ManifestIntent) (eventstore.Event, error) {
				edit(&mi)
				return intent.Record(mi)
			}
			t.Cleanup(func() { recordIntent = intent.Record })
			rw.untilRun(t)
			if id, ok, err := firstAttempt(rw); err != nil || ok {
				t.Errorf("an attempt %s started with the intent: %v", id, err)
			}
		})
	}
}

// Opus 22a-5c-2b-1 L1-L3: the intent's sizes, routing and harness: the
// brief included at the prompt's size, within the prompt's bound; routing
// as the registry verifies the destination; the adapter at adcd's release.
func TestTheIntentsSizesAndHarness(t *testing.T) {
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.untilRun(t)
	a, _ := rw.attemptOf(t)
	mi, ok := recordedIntent(t, rw.w, a.ID)
	if !ok || mi.Budget.Bytes != maxPrompt || mi.Included[0].Bytes >= mi.Budget.Bytes || mi.Routing != types.ManifestIntentRoutingVerified ||
		!strings.HasSuffix(mi.Consumer.HarnessVersion, "@"+buildinfo.Get("adcd").Version) {
		t.Errorf("the intent's sizes, routing and harness: %+v", mi)
	}
}

// Opus 22a-5c-2b-1 L2: the intent's routing is the destination's, as the
// registry verifies it, not the profile's permission: under reported_ok,
// a verified destination's intent says verified.
func TestTheIntentsRoutingIsTheDestinations(t *testing.T) {
	rw := newRunWorldWith(t, true, builder(nil, false, nil), func(p map[string]any) {
		p["data_policy"].(map[string]any)["routing"] = "reported_ok"
	})
	rw.untilRun(t)
	a, _ := rw.attemptOf(t)
	if mi, ok := recordedIntent(t, rw.w, a.ID); !ok || mi.Routing != types.ManifestIntentRoutingVerified {
		t.Errorf("the intent under reported_ok to a verified destination: %v %s", ok, mi.Routing)
	}
}

// The intent's byte budget is 09's text bound, the host's.
func TestTheIntentsBudgetIsTheTextBound(t *testing.T) {
	if intent.MaxPrompt != adapterhost.BoundText {
		t.Errorf("intent.MaxPrompt %d, adapterhost.BoundText %d", intent.MaxPrompt, adapterhost.BoundText)
	}
}

// Opus 22a-5c-2b-1 M2: the intent's fold alone (the launch command's
// registry check passed over) refuses a destination or a routing a listed
// restriction does not allow.
func TestTheFoldRefusesAnUnallowedDestination(t *testing.T) {
	for name, edit := range map[string]func(*types.ManifestIntent){
		"a destination not allowed": func(m *types.ManifestIntent) { m.Destination = "elsewhere" },
		"reported routing":          func(m *types.ManifestIntent) { m.Routing = types.ManifestIntentRoutingReported },
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t, true, builder(nil, false, nil))
			recordIntent = func(mi types.ManifestIntent) (eventstore.Event, error) {
				edit(&mi)
				return intent.Record(mi)
			}
			checkRegistered = func(eventstore.Event, registry.Registry, types.ExecutorID) error { return nil }
			t.Cleanup(func() { recordIntent, checkRegistered = intent.Record, registered })
			rw.untilRun(t)
			if id, ok, err := firstAttempt(rw); err != nil || ok {
				t.Errorf("an attempt %s started with the intent: %v", id, err)
			}
		})
	}
}
