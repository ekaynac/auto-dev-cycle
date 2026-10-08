// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

const actID = "act_01m3250v0aer9ss2097svmefj8"

// newHold is a hold of the given registered name and owner, with a fresh
// id; the builders stamp its time and record its escalation.
func newHold(t *testing.T, name string, owner types.Owner, ref *string) types.NodeHold {
	id, err := ids.New("hld")
	if err != nil {
		t.Fatal(err)
	}
	return types.NodeHold{ID: types.HoldID(id), Hold: name, SignalInstance: ownID + "@2", Owner: owner, OwnerRef: ref, Pages: []types.NodeHoldPagesItem{}}
}

func awaitingInput(t *testing.T) types.NodeHold {
	return newHold(t, "awaiting_input", types.OwnerGrantHolder, ptr(actID))
}

func now(tx *eventstore.Tx) types.Time { return types.Time(tx.Now().UTC().Format(time.RFC3339Nano)) }

// raised raises h on n, with env as the profile's environment owner.
func raised(n types.NodeID, h types.NodeHold, env *string) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Raise(tx, n, h, env) }
}

func page(n types.NodeID, h types.HoldID, owner types.Owner, ref *string, channel types.Name) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Page(tx, n, h, owner, ref, &channel) }
}

func clear(n types.NodeID, h types.NodeHold) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Clear(tx, n, h.ID, h.SignalInstance) }
}

func (g graphStore) holds(n types.NodeID) []types.NodeHold { return g.state(n).Holds }

// A hold's life: raised with its registry values, paged once per owner, an
// escalation only after page_after, cleared on its signal; a could_not_run
// retry held on its signal and retired by an invalidation, while another
// wait carries over. Every state is a valid node-state, and the log
// rebuilds, also after a registry edit.
func TestHoldLifecycle(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(a))

	// A waiting node held, then cleared, waits again.
	w := g.record(g.newNode(types.NodeKindBuild))
	wh := awaitingInput(t)
	g.must(raised(w, wh, nil))
	g.must(clear(w, wh))
	if st := g.state(w); st.State != types.NodeStateStateWaiting || len(st.WaitingOn) != 1 {
		t.Fatalf("a waiting node cleared: %+v", st)
	}

	h := awaitingInput(t)
	g.must(raised(a, h, nil))
	if got := g.holds(a)[0]; got.EscalateTo != nil || got.PageAfter == nil || *got.PageAfter != "PT1H" {
		t.Fatalf("the recorded registry values: %+v", got)
	}
	g.must(page(a, h.ID, types.OwnerGrantHolder, ptr(actID), "telegram"))
	if p := g.holds(a)[0].Pages; len(p) != 1 || p[0].SignalInstance != h.SignalInstance || *p[0].Channel != "telegram" {
		t.Fatalf("the page: %+v", p)
	}
	if err := g.do(page(a, h.ID, types.OwnerGrantHolder, ptr(actID), "email")); err == nil {
		t.Fatal("the owner was paged twice for one signal")
	}
	g.must(clear(a, h))
	if st := g.state(a); st.State != types.NodeStateStateClaimed || len(st.Holds) != 0 {
		t.Fatalf("cleared: %+v", st)
	}

	// evidence_restore_pending escalates from the system to the environment
	// owner the profile names, never before page_after; with no environment
	// owner named, to the operator (02).
	e := newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	g.must(raised(a, e, ptr("staging-owner")))
	g.must(page(a, e.ID, types.OwnerSystem, nil, "telegram"))
	if err := g.do(page(a, e.ID, types.OwnerEnvironmentOwner, ptr("staging-owner"), "email")); err == nil {
		t.Fatal("an escalation before page_after")
	}
	g.must(clear(a, e))
	e = newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	e.PageAfter = ptr(types.Duration("PT0S")) // a project's override: due at once
	g.must(raised(a, e, ptr("staging-owner")))
	g.must(page(a, e.ID, types.OwnerSystem, nil, "telegram"))
	g.must(page(a, e.ID, types.OwnerEnvironmentOwner, ptr("staging-owner"), "email"))
	if h := g.holds(a)[0]; h.Owner != types.OwnerEnvironmentOwner || *h.OwnerRef != "staging-owner" || len(h.Pages) != 2 {
		t.Fatalf("escalated: %+v", h)
	}
	if err := g.do(page(a, e.ID, types.OwnerEnvironmentOwner, ptr("other-owner"), "email")); err == nil {
		t.Fatal("a page re-pointed the escalated owner")
	}
	g.must(clear(a, e))
	f := newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	g.must(raised(a, f, nil))
	if h := g.holds(a)[0]; *h.EscalateTo != types.OwnerOperator || h.EscalateRef != nil {
		t.Fatalf("the operator fallback: %+v", h)
	}
	g.must(clear(a, f))

	// A page_after past what the clock holds is an error, never due.
	long := newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	for _, d := range []types.Duration{"P9999999999D", "P999999999D"} { // too many digits; past the clock
		long.PageAfter = ptr(d)
		g.must(raised(a, long, ptr("staging-owner")))
		if err := g.do(page(a, long.ID, types.OwnerEnvironmentOwner, ptr("staging-owner"), "email")); err == nil {
			t.Fatalf("page_after %s was due", d)
		}
		g.must(clear(a, long))
		long = newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	}

	// A could_not_run result retries on its signal: held on the hold its
	// reason names, once per result; a reason that names none is the rails'.
	credit := must(Build(*g.state(a).Key, BuildFacts{Project: prjID, Exit: exitmap.Exit{Hints: []string{"credit_exhausted"}}}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: a, Result: credit}, g.blobs)
	})
	retry := func(res types.ResultID, h types.NodeHold) logic {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			h.Since = now(tx)
			return Retry(tx, a, res, h, nil)
		}
	}
	r := newHold(t, "", types.OwnerOperator, nil)
	g.must(retry(credit.ID, r))
	if h := g.holds(a)[0]; h.Hold != "broker_credit_exhausted" || *h.Reason != vocab.ReasonBrokerCreditExhausted {
		t.Fatalf("the retry hold: %+v", h)
	}
	if err := g.do(retry(credit.ID, newHold(t, "", types.OwnerOperator, nil))); err == nil {
		t.Fatal("a second retry wait of one result")
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: a, Result: must(Build(*g.state(a).Key, BuildFacts{Project: prjID}))}, g.blobs)
	}); err == nil {
		t.Fatal("a held node recorded a result")
	}

	// An invalidation retires the retry wait of the old key by an event and
	// keeps the budget wait, which still blocks results.
	b := newHold(t, "budget_rail", types.OwnerOperator, nil)
	g.must(raised(a, b, nil))
	before := g.count(HoldCleared)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, a) })
	st := g.state(a)
	if st.State != types.NodeStateStateHeld || len(st.Holds) != 1 || st.Holds[0].ID != b.ID || st.Key.Generation != 2 || g.count(HoldCleared) != before+1 {
		t.Fatalf("invalidated while held: %+v", st)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: a, Result: must(Build(*g.state(a).Key, BuildFacts{Project: prjID}))}, g.blobs)
	}); err == nil {
		t.Fatal("a node still held on its budget recorded a result")
	}
	if err := g.do(retry(credit.ID, newHold(t, "", types.OwnerOperator, nil))); err == nil {
		t.Fatal("a retry on the old key's result")
	}

	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
	// A registry edit changes no recorded hold: the log still rebuilds.
	edited, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	delete(edited.Holds, "budget_rail")
	entry := edited.Holds["evidence_restore_pending"]
	entry.Owner, entry.EscalateTo = types.OwnerOperator, nil
	edited.Holds["evidence_restore_pending"] = entry
	registry = func() (*vocab.Registry, error) { return edited, nil }
	t.Cleanup(func() { registry = vocab.Load })
	if diffs, err := g.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild after a registry edit: %v %+v", err, diffs)
	}
}

// The fold refuses hold events its state does not derive.
func TestHoldRefusals(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(a))
	c := g.record(g.newNode(types.NodeKindBuild))
	h := awaitingInput(t)
	g.must(raised(c, h, nil))
	never := newHold(t, "awaiting_input", types.OwnerGrantHolder, ptr(actID))
	never.Since = "x"
	silent := g.record(g.newNode(types.NodeKindBuild))
	other := g.record(g.newNode(types.NodeKindBuild)) // claimed, with its own could_not_run result
	g.must(g.claimOf(other))
	foreign := must(Build(*g.state(other).Key, BuildFacts{Project: prjID, Exit: exitmap.Exit{Hints: []string{"credit_exhausted"}}}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: other, Result: foreign}, g.blobs)
	})
	done := g.plan // completed
	forged := func(n types.NodeID, kind, subject string, project types.ProjectID, result *types.ResultID, f func(*types.NodeHold)) logic {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			x := awaitingInput(t)
			x.Since, x.PageAfter = now(tx), ptr(types.Duration("PT1H"))
			f(&x)
			return event(kind, subject, project, &types.HoldRaised{Type: "hold_raised", Node: n, Hold: x, Result: result})
		}
	}
	none := func(*types.NodeHold) {}
	credit := ptr(vocab.ReasonBrokerCreditExhausted)
	paged := func(n types.NodeID, kind, subject string, project types.ProjectID, hold types.HoldID, owner types.Owner, ref *string) logic {
		return forge(kind, subject, project, &types.HoldPaged{Type: "hold_paged", Node: n, Hold: hold, Owner: owner, OwnerRef: ref})
	}
	signal := func(n types.NodeID, kind, subject string, project types.ProjectID, hold types.HoldID, instance string) logic {
		return forge(kind, subject, project, &types.HoldCleared{Type: "hold_cleared", Node: n, Hold: hold,
			Cause: types.HoldClearedCause{Variant: &types.Signalled{Type: "signal", SignalInstance: instance}}})
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		never.Since, never.PageAfter = now(tx), nil
		return event(HoldRaised, string(silent), prjID, &types.HoldRaised{Type: "hold_raised", Node: silent, Hold: never})
	})
	for name, fn := range map[string]logic{
		"a hold under another subject":   forged(a, HoldRaised, string(c), prjID, nil, none),
		"a hold under node.hold_cleared": forged(a, HoldCleared, string(a), prjID, nil, none),
		"a hold in another project":      forged(a, HoldRaised, string(a), prjB, nil, none),
		"a hold raised at another time":  forged(a, HoldRaised, string(a), prjID, nil, func(x *types.NodeHold) { x.Since = "2026-01-01T00:00:00Z" }),
		"a hold raised with a page": forged(a, HoldRaised, string(a), prjID, nil, func(x *types.NodeHold) {
			x.Pages = []types.NodeHoldPagesItem{{At: x.Since, Owner: x.Owner, OwnerRef: x.OwnerRef, SignalInstance: "x"}}
		}),
		"a hold id raised twice":                    forged(a, HoldRaised, string(a), prjID, nil, func(x *types.NodeHold) { x.ID = h.ID }),
		"a hold on a completed node":                forged(done, HoldRaised, string(done), prjID, nil, none),
		"a reason with no result":                   forged(a, HoldRaised, string(a), prjID, nil, func(x *types.NodeHold) { x.Reason = credit }),
		"a result with no reason":                   forged(other, HoldRaised, string(other), prjID, &foreign.ID, none),
		"a retry on another node's result":          forged(a, HoldRaised, string(a), prjID, &foreign.ID, func(x *types.NodeHold) { x.Reason = credit }),
		"a retry with another reason":               forged(other, HoldRaised, string(other), prjID, &foreign.ID, func(x *types.NodeHold) { x.Reason = ptr(vocab.ReasonQuotaExhausted) }),
		"a retry on an unrecorded result":           forged(a, HoldRaised, string(a), prjID, ptr(types.ResultID(resB)), func(x *types.NodeHold) { x.Reason = credit }),
		"a page under another subject":              paged(c, HoldPaged, string(a), prjID, h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page under node.hold_cleared":            paged(c, HoldCleared, string(c), prjID, h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page in another project":                 paged(c, HoldPaged, string(c), prjB, h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page of a hold not held":                 paged(a, HoldPaged, string(a), prjID, h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page of another hold":                    paged(c, HoldPaged, string(c), prjID, "hld_01m3250v57fhm9v1z9xd1wk8gw", types.OwnerGrantHolder, ptr(actID)),
		"a page to another owner":                   paged(c, HoldPaged, string(c), prjID, h.ID, types.OwnerOperator, nil),
		"a page to the owner, another ref":          paged(c, HoldPaged, string(c), prjID, h.ID, types.OwnerGrantHolder, ptr("act_01m3250vm7f6ktw39dszqnyddr")),
		"a page of a hold never paged":              paged(silent, HoldPaged, string(silent), prjID, never.ID, types.OwnerGrantHolder, ptr(actID)),
		"a clear of a hold not held":                signal(a, HoldCleared, string(a), prjID, h.ID, h.SignalInstance),
		"a clear under another subject":             signal(c, HoldCleared, string(a), prjID, h.ID, h.SignalInstance),
		"a clear under node.hold_raised":            signal(c, HoldRaised, string(c), prjID, h.ID, h.SignalInstance),
		"a clear in another project":                signal(c, HoldCleared, string(c), prjB, h.ID, h.SignalInstance),
		"a clear on another signal":                 signal(c, HoldCleared, string(c), prjID, h.ID, "own@9"),
		"a wait retired as invalidated":             forge(HoldCleared, string(c), prjID, &types.HoldCleared{Type: "hold_cleared", Node: c, Hold: h.ID, Cause: types.HoldClearedCause{Variant: &types.Retired{Type: "retired", Reason: "invalidated"}}}),
		"an invalidation of a held, unclaimed node": forge(NodeInvalidated, string(c), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: c, Key: *g.state(a).Key}),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A reason that names no hold is the rails'; a found result has no
	// retry wait; nor has a result of an old key.
	z := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(z))
	plain := must(Build(*g.state(z).Key, BuildFacts{Project: prjID}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: z, Result: plain}, g.blobs)
	})
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Retry(tx, z, plain.ID, newHold(t, "", types.OwnerOperator, nil), nil)
	}); err == nil {
		t.Error("executor_incomplete raised a retry wait")
	}
	found := unrecorded(t, *g.state(z).Key)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: z, Result: found}, g.blobs)
	})
	stale := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(stale))
	old := must(Build(*g.state(stale).Key, BuildFacts{Project: prjID, Exit: exitmap.Exit{Hints: []string{"credit_exhausted"}}}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return recordResult(tx, types.ResultRecorded{Node: stale, Result: old}, g.blobs)
	})
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, stale) })
	for name, fn := range map[string]logic{
		"a retry wait of a found result":      forged(z, HoldRaised, string(z), prjID, &found.ID, func(x *types.NodeHold) { x.Reason = ptr(vocab.ReasonCompletedWithFindings) }),
		"a retry wait of an old key's result": forged(stale, HoldRaised, string(stale), prjID, &old.ID, func(x *types.NodeHold) { x.Reason = credit }),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A retry wait of the current key is not retired.
	r := newHold(t, "", types.OwnerOperator, nil)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r.Since = now(tx)
		return Retry(tx, other, foreign.ID, r, nil)
	})
	if err := g.do(forge(HoldCleared, string(other), prjID, &types.HoldCleared{Type: "hold_cleared", Node: other, Hold: r.ID,
		Cause: types.HoldClearedCause{Variant: &types.Retired{Type: "retired", Reason: "invalidated"}}})); err == nil {
		t.Error("a retry wait of the current key was retired")
	}
	// The builders check the registry and the actor.
	for name, fn := range map[string]logic{
		"an unregistered hold":       raised(a, newHold(t, "no_such_hold", types.OwnerOperator, nil), nil),
		"a hold by another owner":    raised(a, newHold(t, "awaiting_input", types.OwnerOperator, nil), nil),
		"a named owner with no ref":  raised(a, newHold(t, "wrong_owner", types.OwnerNamedOwner, nil), nil),
		"a grant holder, not an act": raised(a, newHold(t, "awaiting_input", types.OwnerGrantHolder, ptr("act_00000000000000000000000000")), nil),
		"a page to a non-actor":      page(c, h.ID, types.OwnerGrantHolder, ptr("act_00000000000000000000000000"), "email"),
		"a retry on a plan result": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return Retry(tx, a, g.planRes, awaitingInput(t), nil)
		},
	} {
		if err := g.do(fn); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
}
