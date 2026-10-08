// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"errors"
	"reflect"
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
// id; raise stamps its time.
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

// stamped raises h on n at the command's time.
func stamped(n types.NodeID, h types.NodeHold) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		h.Since = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return Raise(tx, n, h)
	}
}

func page(n types.NodeID, h types.HoldID, owner types.Owner, ref *string, channel types.Name) logic {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Page(tx, n, h, owner, ref, &channel) }
}

func (g graphStore) holds(n types.NodeID) []types.NodeHold { return g.state(n).Holds }

// A hold's life: raised on a waiting successor, paged, moved with its id,
// owner and pages to another node of the task (13 supersession), paged
// again there, cleared; an escalation; a could_not_run retry held on its
// signal; a held node invalidated. Every state is a valid node-state, and
// the log rebuilds.
func TestHoldLifecycle(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(a))
	succ := g.newNode(types.NodeKindBuild)
	succ.Predecessor, succ.Node.Round = &a, 2
	succ.Node.Inputs = append(succ.Node.Inputs, types.InputSpec{Name: "ruling", Source: types.InputSpecSource{Variant: &types.AnswerSource{Type: "answer", OwedInput: ownID, Revision: 2}}})
	b := g.record(succ)
	h := awaitingInput(t)
	g.must(stamped(b, h))
	g.must(page(b, h.ID, types.OwnerGrantHolder, ptr(actID), "telegram"))
	if st := g.state(b); st.State != types.NodeStateStateHeld || len(st.WaitingOn) != 2 || len(st.Holds[0].Pages) != 1 {
		t.Fatalf("held and paged: %+v", st)
	}
	before := g.holds(b)[0]
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Transfer(tx, b, a, h.ID) })
	moved := g.holds(a)[0]
	if g.state(a).State != types.NodeStateStateHeld || g.state(b).State != types.NodeStateStateWaiting ||
		!reflect.DeepEqual(moved, before) {
		t.Fatalf("the transfer lost the hold's history: %+v, was %+v", moved, before)
	}
	g.must(page(a, h.ID, types.OwnerGrantHolder, ptr(actID), "email"))
	if p := g.holds(a)[0].Pages; len(p) != 2 || !reflect.DeepEqual(p[0], before.Pages[0]) || *p[1].Channel != "email" || p[1].SignalInstance != h.SignalInstance {
		t.Fatalf("the paging history did not carry on: %+v", p)
	}
	if v, _ := view[types.NodeID](g, HoldProjection, string(h.ID)); v != a {
		t.Fatalf("the hold index names %s", v)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Clear(tx, a, h.ID) })
	if st := g.state(a); st.State != types.NodeStateStateClaimed || len(st.Holds) != 0 {
		t.Fatalf("cleared: %+v", st)
	}

	// An escalation: evidence_restore_pending passes from the system to the
	// environment owner the profile names, who owns it from then on.
	e := newHold(t, "evidence_restore_pending", types.OwnerSystem, nil)
	g.must(stamped(a, e))
	g.must(page(a, e.ID, types.OwnerSystem, nil, "telegram"))
	g.must(page(a, e.ID, types.OwnerEnvironmentOwner, ptr("staging-owner"), "email"))
	if h := g.holds(a)[0]; h.Owner != types.OwnerEnvironmentOwner || *h.OwnerRef != "staging-owner" || len(h.Pages) != 2 {
		t.Fatalf("escalated: %+v", h)
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Clear(tx, a, e.ID) })

	// A could_not_run result retries on its signal: held on the hold its
	// reason names; a reason that names none is the rails'.
	credit := must(Build(*g.state(a).Key, BuildFacts{Project: prjID, Exit: exitHints("credit_exhausted")}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: a, Result: credit})
	})
	r := newHold(t, "", types.OwnerOperator, nil)
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r.Since = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return Retry(tx, a, credit.ID, r)
	})
	if h := g.holds(a)[0]; h.Hold != "broker_credit_exhausted" || *h.Reason != vocab.ReasonBrokerCreditExhausted {
		t.Fatalf("the retry hold: %+v", h)
	}
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return RecordResult(tx, types.ResultRecorded{Node: a, Result: must(Build(*g.state(a).Key, BuildFacts{Project: prjID}))})
	}); err == nil {
		t.Fatal("a held node recorded a result")
	}
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Clear(tx, a, r.ID) }) // the signal
	plain := must(Build(*g.state(a).Key, BuildFacts{Project: prjID}))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return event(ResultRecorded, string(plain.ID), prjID, &types.ResultRecorded{Type: "result_recorded", Node: a, Result: plain})
	})
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return Retry(tx, a, plain.ID, newHold(t, "", types.OwnerOperator, nil))
	}); err == nil {
		t.Fatal("executor_incomplete raised a hold")
	}

	// A held node is invalidated: its holds end with its generation.
	g.must(stamped(a, awaitingInput(t)))
	g.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Invalidate(tx, a) })
	if st := g.state(a); st.State != types.NodeStateStateClaimed || len(st.Holds) != 0 || st.Key.Generation != 2 {
		t.Fatalf("invalidated while held: %+v", st)
	}
	diffs, err := g.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %+v", err, diffs)
	}
}

// The fold refuses hold events its state does not derive.
func TestHoldRefusals(t *testing.T) {
	g := openStore(t)
	a := g.record(g.newNode(types.NodeKindBuild))
	g.must(g.claimOf(a))
	c := g.record(g.newNode(types.NodeKindBuild))
	h := awaitingInput(t)
	g.must(stamped(c, h))
	stranger := g.newNode(types.NodeKindBuild)
	stranger.Task = tskB
	s := g.record(stranger)
	done, _ := g.plan, g.planRes // completed
	raised := func(n types.NodeID, kind, subject string, f func(*types.NodeHold)) logic {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			x := awaitingInput(t)
			x.Since = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
			f(&x)
			return event(kind, subject, prjID, &types.HoldRaised{Type: "hold_raised", Node: n, Hold: x})
		}
	}
	none := func(*types.NodeHold) {}
	paged := func(n types.NodeID, kind, subject string, hold types.HoldID, owner types.Owner, ref *string) logic {
		return forge(kind, subject, prjID, &types.HoldPaged{Type: "hold_paged", Node: n, Hold: hold, Owner: owner, OwnerRef: ref})
	}
	moved := func(subject string, from, to types.NodeID, kind string) logic {
		return forge(kind, subject, prjID, &types.HoldTransferred{Type: "hold_transferred", From: from, To: to, Hold: h.ID})
	}
	for name, fn := range map[string]logic{
		"a hold under another subject":   raised(a, HoldRaised, string(c), none),
		"a hold under node.hold_cleared": raised(a, HoldCleared, string(a), none),
		"a hold raised at another time":  raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.Since = "2026-01-01T00:00:00Z" }),
		"a hold raised with a page": raised(a, HoldRaised, string(a), func(x *types.NodeHold) {
			x.Pages = []types.NodeHoldPagesItem{{At: x.Since, Owner: x.Owner, OwnerRef: x.OwnerRef, SignalInstance: "x"}}
		}),
		"an unregistered hold":             raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.Hold = "no_such_hold" }),
		"a hold raised by another owner":   raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.Owner, x.OwnerRef = types.OwnerOperator, nil }),
		"a reason that names another hold": raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.Reason = ptr(vocab.ReasonBrokerCreditExhausted) }),
		"a reason that names no hold":      raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.Reason = ptr(vocab.ReasonExecutorIncomplete) }),
		"a hold id raised twice":           raised(a, HoldRaised, string(a), func(x *types.NodeHold) { x.ID = h.ID }),
		"a hold on a completed node":       raised(done, HoldRaised, string(done), none),
		"a hold in another project": func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			evs, err := raised(a, HoldRaised, string(a), none)(tx)
			evs[0].Project = ptr(string(prjB))
			return evs, err
		},
		"a page under another subject":              paged(c, HoldPaged, string(a), h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page under node.hold_cleared":            paged(c, HoldCleared, string(c), h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page of a hold not held":                 paged(a, HoldPaged, string(a), h.ID, types.OwnerGrantHolder, ptr(actID)),
		"a page of another hold":                    paged(c, HoldPaged, string(c), "hld_01m3250v57fhm9v1z9xd1wk8gw", types.OwnerGrantHolder, ptr(actID)),
		"a page to another owner":                   paged(c, HoldPaged, string(c), h.ID, types.OwnerOperator, nil),
		"a page to the owner, another ref":          paged(c, HoldPaged, string(c), h.ID, types.OwnerGrantHolder, ptr("act_01m3250vm7f6ktw39dszqnyddr")),
		"a clear of a hold not held":                forge(HoldCleared, string(a), prjID, &types.HoldCleared{Type: "hold_cleared", Node: a, Hold: h.ID}),
		"a clear under another subject":             forge(HoldCleared, string(a), prjID, &types.HoldCleared{Type: "hold_cleared", Node: c, Hold: h.ID}),
		"a clear under node.hold_raised":            forge(HoldRaised, string(c), prjID, &types.HoldCleared{Type: "hold_cleared", Node: c, Hold: h.ID}),
		"a transfer to the same node":               moved(string(c), c, c, HoldTransferred),
		"a transfer to another task":                moved(string(s), c, s, HoldTransferred),
		"a transfer to a completed node":            moved(string(done), c, done, HoldTransferred),
		"a transfer under the old node":             moved(string(c), c, a, HoldTransferred),
		"a transfer of a hold not held":             forge(HoldTransferred, string(c), prjID, &types.HoldTransferred{Type: "hold_transferred", From: a, To: c, Hold: h.ID}),
		"a transfer under node.hold_cleared":        moved(string(a), c, a, HoldCleared),
		"an invalidation of a held, unclaimed node": forge(NodeInvalidated, string(c), prjID, &types.NodeInvalidated{Type: "node_invalidated", Node: c, Key: *g.state(a).Key}),
	} {
		if err := g.do(fn); err == nil || !strings.Contains(err.Error(), "graph: event") {
			t.Errorf("%s: %v", name, err)
		}
	}
	var stale ErrKeyStale
	if err := g.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return Retry(tx, c, g.planRes, awaitingInput(t)) }); err == nil || errors.As(err, &stale) {
		t.Errorf("a retry on another node's result: %v", err)
	}
}

func exitHints(hints ...string) exitmap.Exit { return exitmap.Exit{Hints: hints} }
