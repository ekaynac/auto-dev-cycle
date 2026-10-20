// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The stub builder's price (registry-stub.json): $1 and $4 per million
// input and output tokens. A card reserves 1,000 tokens: 4,000 micros at
// the higher rate. noCap is a USD rail no test reaches.
const (
	reservedMicros = 4000
	noCap          = int64(1) << 50
)

// usdCaps are rails with only the USD caps, in micros (NoUSDRail: none).
func usdCaps(perTask, perDay *int64) graph.Rails {
	return func(*eventstore.Tx, types.TaskID) (graph.Caps, error) {
		return graph.Caps{AttemptsPerRound: 9, RoundsPerTask: 9, MergeOperationsPerGeneration: 9, PerTaskMicros: *perTask, PerDayMicros: *perDay}, nil
	}
}

// nodeIn records and claims a plan node of task.
func (e env) nodeIn(t types.TaskID) types.NodeID {
	e.t.Helper()
	r := types.NodeRecorded{Type: "node_recorded", Task: t, Project: project, Node: types.Node{ID: types.NodeID(must(ids.New("nod"))),
		Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`), Inputs: []types.InputSpec{},
		Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	e.must(func(*eventstore.Tx) ([]eventstore.Event, error) { return graph.Record(r) })
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, r.Node.ID, map[string]graph.Resolved{}, 1)
	})
	return r.Node.ID
}

// measured records m of call on the lease and attempt, in USD.
func (e env) measured(call types.CallID, a types.AttemptID, l types.LeaseID, kind types.UsageMeasurementKind, seq, in, out int64, src types.UsageMeasurementSourceOfTruth) {
	e.t.Helper()
	e.measuredIn(call, a, l, "USD", kind, seq, in, out, src)
}

// measuredIn is measured, its cost in currency.
func (e env) measuredIn(call types.CallID, a types.AttemptID, l types.LeaseID, currency string, kind types.UsageMeasurementKind, seq, in, out int64, src types.UsageMeasurementSourceOfTruth) {
	e.t.Helper()
	m := types.UsageMeasurement{ID: types.MeasurementID(must(ids.New("msr"))), Call: call, Attempt: a, Lease: l, Kind: kind, Sequence: seq,
		Tokens: types.UsageMeasurementTokens{In: in, Out: out}, Turns: 1, Milliseconds: 5,
		Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: currency, Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: src}
	e.must(func(*eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(m)
		return []eventstore.Event{ev}, err
	})
}

// spentOn places a lease for node, makes one call under it reported by
// report, then ends and settles the lease as its calls stand.
func (e env) spentOn(d Dispatcher, node types.NodeID, report func(types.CallID, types.AttemptID, types.LeaseID)) types.LeaseID {
	e.t.Helper()
	id, err := e.match(d, card(node), builder)
	if err != nil {
		e.t.Fatal(err)
	}
	a, miss := e.launch(d, id, card(node), builder)
	if miss != nil {
		e.t.Fatal(miss)
	}
	call := types.CallID(must(ids.New("cal")))
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return modelcall.Intend(tx, gateway.Binding{Lease: id, Attempt: a, Call: call}, "stub")
	})
	report(call, a, id)
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.LoseRunning(tx, a, types.AttemptLostReasonValueAgentDisconnect)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SealMinimal(tx, a, 2, graph.Units{}, e.blobs, nil, actor)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, id, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "test"}, nil)
	})
	e.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		s, err := capacity.Settlement(tx, id)
		if err != nil {
			return nil, err
		}
		return lease.ChangeLease(tx, id, 4, lease.LeaseEvent{Kind: lease.Settled, Settlement: &s}, nil)
	})
	return id
}

// hostileZeroFinal is #226 entry 69's call: the gateway counted 300 in and
// 200 out, and the adapter's final says zero.
func (e env) hostileZeroFinal(call types.CallID, a types.AttemptID, l types.LeaseID) {
	e.measured(call, a, l, types.UsageMeasurementKindDelta, 1, 300, 200, types.UsageMeasurementSourceOfTruthGatewayCount)
	e.measured(call, a, l, types.UsageMeasurementKindFinal, 2, 0, 0, types.UsageMeasurementSourceOfTruthAdapterReport)
}

// refusedOn is a match of node refused on rail, with its budget_rail hold.
func refusedOn(t *testing.T, err error, rail string) {
	t.Helper()
	var r *Refused
	if !errors.As(err, &r) || r.Check != "budget" || r.Outcome != types.OutcomeCouldNotRun || r.Reason != vocab.ReasonBudgetRailReached ||
		r.Hold == nil || r.Hold.Hold != vocab.HoldBudgetRail || r.Hold.SignalInstance != graph.RailSignal(rail) || r.Hold.Owner != types.OwnerOperator {
		t.Fatalf("a match past %s: %v", rail, err)
	}
}

// The USD rails (P17b; fault 2: "a liability left out of the USD rail"):
//   - U3, #226 entry 69: a hostile zero final leaves its call unsettled, and
//     its liability (the reservation, above the gateway's counts) counts:
//     the next lease past per_task_usd is refused, budget_rail
//     (rail:per_task_usd) named, nothing placed;
//   - U1: a live lease's reservation counts too;
//   - a raised rail places it;
//   - an executor with no price costs nothing.
func TestUSDTaskRail(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	perTask, perDay := int64(2*reservedMicros-1), noCap
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true), Rails: usdCaps(&perTask, &perDay)}
	first := e.nodeIn(task)
	e.spentOn(d, first, e.hostileZeroFinal)
	next := e.nodeIn(task)
	_, err := e.match(d, card(next), builder)
	refusedOn(t, err, graph.RailTaskUSD)
	if l, live, lerr := e.liveOn(next); lerr != nil || live {
		t.Fatalf("the refused match placed %s", l)
	}
	perTask = 2 * reservedMicros // the raise
	id, err := e.match(d, card(next), builder)
	if err != nil {
		t.Fatalf("within the raised rail: %v", err)
	}
	// U1: the live lease's reservation counts.
	third := e.nodeIn(task)
	if _, err := e.match(d, card(third), builder); err == nil {
		t.Fatal("a third lease past the rail, the second one live")
	} else {
		refusedOn(t, err, graph.RailTaskUSD)
	}
	l, _, _ := e.state(id, next)
	if !strings.Contains(l.PlacementChecks.Budget.Detail, "task 4000 of 8000") {
		t.Errorf("the placed lease's budget check: %+v", l.PlacementChecks.Budget)
	}
}

// U2, the day rail: another task's lease of the project granted the same
// UTC day counts against per_day_usd; one granted the day before does not
// (a liability stays on its lease's day).
func TestUSDDayRail(t *testing.T) {
	t.Cleanup(func() { clock = time.Now })
	reg, gs := stub(t, nil)
	e := open(t, gs)
	perTask, perDay := noCap, int64(2*reservedMicros-1)
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true), Rails: usdCaps(&perTask, &perDay)}
	other := types.TaskID("tsk_01m3250v9xft08ssr4ygq6g2nq")
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	clock = func() time.Time { return yesterday }
	e.spentOn(d, e.nodeIn(other), e.hostileZeroFinal)
	clock = time.Now
	if _, err := e.match(d, card(e.nodeIn(task)), builder); err != nil {
		t.Fatalf("yesterday's spend counted today: %v", err)
	}
	_, err := e.match(d, card(e.nodeIn(task)), builder)
	refusedOn(t, err, graph.RailDayUSD)
	if _, err := e.match(d, card(e.nodeIn(other)), builder); err == nil {
		t.Error("another task's lease past the day rail")
	}
}

// U4: a settled call costs its usage at the executor's declared price,
// never its reported cost; a lease with no call costs its reservation
// while live and nothing once ended; an executor with no price, nothing.
func TestUSDCost(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	perTask, perDay := noCap, noCap
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true), Rails: usdCaps(&perTask, &perDay)}
	settled := e.spentOn(d, e.nodeIn(task), func(call types.CallID, a types.AttemptID, l types.LeaseID) {
		e.measured(call, a, l, types.UsageMeasurementKindDelta, 1, 1000, 0, types.UsageMeasurementSourceOfTruthGatewayCount)
		e.measured(call, a, l, types.UsageMeasurementKindFinal, 2, 1000, 0, types.UsageMeasurementSourceOfTruthAdapterReport)
	})
	hostile := e.spentOn(d, e.nodeIn(task), e.hostileZeroFinal)
	live, err := e.match(d, card(e.nodeIn(task)), builder)
	if err != nil {
		t.Fatal(err)
	}
	e.view(func(tx *eventstore.Tx) error {
		for name, c := range map[string]struct {
			l    types.LeaseID
			edit func(*types.Lease)
			want int64
		}{
			"settled at 1,000 input tokens":                       {settled, nil, 1000},
			"the hostile zero final at its 1,000-token liability": {hostile, nil, reservedMicros},
			"a live lease at its reservation":                     {live, nil, reservedMicros},
			"no price":                                            {hostile, func(l *types.Lease) { l.Price = nil }, 0},
			// the price placed with, not the registry's now (Opus #302 M1)
			"priced as placed": {settled, func(l *types.Lease) { l.Price.InputUsdMicrosPerMtok = 2_000_000 }, 2000},
		} {
			l, _, _, err := lease.LeaseOf(tx, c.l)
			if err != nil {
				return err
			}
			if c.edit != nil {
				p := *l.Price
				l.Price = &p
				c.edit(&l)
			}
			if got, err := cost(tx, l); err != nil || got != c.want {
				t.Errorf("%s: %d, %v; want %d", name, got, err, c.want)
			}
		}
		// A lease of two groups is not counted: an error (Opus #302 L3).
		l, _, _, err := lease.LeaseOf(tx, live)
		if err != nil {
			return err
		}
		l.Reservations = append(l.Reservations, l.Reservations[0])
		if _, err := cost(tx, l); err == nil || !strings.Contains(err.Error(), "reserves 2 groups") {
			t.Errorf("a lease of two groups: %v", err)
		}
		return nil
	})
}

// Saturation (Opus #302 H1): a call with a delta past what a price can hold,
// or reports the ledger cannot sum (its saturated standing), costs past
// every rail: the next lease is refused with the hold, never an error that
// wedges placement.
func TestUSDSaturates(t *testing.T) {
	for name, report := range map[string]func(e env) func(types.CallID, types.AttemptID, types.LeaseID){
		"a huge delta": func(e env) func(types.CallID, types.AttemptID, types.LeaseID) {
			return func(c types.CallID, a types.AttemptID, l types.LeaseID) {
				e.measured(c, a, l, types.UsageMeasurementKindDelta, 1, 3_000_000_000_000_000, 0, types.UsageMeasurementSourceOfTruthAdapterReport)
			}
		},
		"an unsummable standing": func(e env) func(types.CallID, types.AttemptID, types.LeaseID) {
			return func(c types.CallID, a types.AttemptID, l types.LeaseID) {
				for seq := range int64(3) {
					e.measured(c, a, l, types.UsageMeasurementKindDelta, seq+1, 1<<53-1, 0, types.UsageMeasurementSourceOfTruthAdapterReport)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			reg, gs := stub(t, nil)
			e := open(t, gs)
			perTask, perDay := noCap, noCap
			d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true), Rails: usdCaps(&perTask, &perDay)}
			e.spentOn(d, e.nodeIn(task), report(e))
			_, err := e.match(d, card(e.nodeIn(task)), builder)
			refusedOn(t, err, graph.RailTaskUSD)
		})
	}
}

// A retired executor (Opus #302 M1): the registry no longer names the
// builder the task spent on; its leases still count at their price, and
// the next lease, on a new builder, is refused at the rail, not wedged. An
// unpriced executor's lease is placed past the rail, reserving nothing
// priced (Opus #302 L1).
func TestUSDRetiredAndUnpriced(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	perTask, perDay := int64(reservedMicros+1), noCap
	d := Dispatcher{Registry: reg, Images: images{}, Credentials: creds(true), Rails: usdCaps(&perTask, &perDay)}
	e.spentOn(d, e.nodeIn(task), e.hostileZeroFinal)
	const successor = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pb")
	retired := d
	doc, err := registry.Load(config + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Executors {
		if doc.Executors[i].ID == builder {
			doc.Executors[i].ID = successor
		}
	}
	groups := slices.Clone(gs)
	for i := range groups {
		groups[i].Members = slices.Clone(groups[i].Members)
		for j, m := range groups[i].Members {
			if m == builder {
				groups[i].Members[j] = successor
			}
		}
	}
	if retired.Registry, err = registry.New(doc, groups); err != nil {
		t.Fatal(err)
	}
	_, err = e.match(retired, card(e.nodeIn(task)), successor)
	refusedOn(t, err, graph.RailTaskUSD)
	free := d
	free.Registry, _ = stub(t, func(doc *types.ExecutorRegistry) {
		for i := range doc.Executors {
			doc.Executors[i].Price, doc.Executors[i].CostBasis = nil, types.RegistryExecutorCostBasisFree
		}
	})
	if _, err := e.match(free, card(e.nodeIn(task)), builder); err != nil {
		t.Errorf("an unpriced executor past the rail: %v", err)
	}
}

// liveOn is node's unit's live lease, if any.
func (e env) liveOn(node types.NodeID) (types.LeaseID, bool, error) {
	var id types.LeaseID
	var live bool
	err := e.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		id, live, err = lease.LiveOn(tx, graph.UnitOf(node))
		return err
	})
	return id, live, err
}

// No rails fail closed (P17b, decision A): a Dispatcher with no Rails
// refuses a lease on a priced executor, could_not_run/precondition_failed,
// naming no hold and placing nothing; on an unpriced one it places it.
func TestUSDNoRails(t *testing.T) {
	reg, gs := stub(t, nil)
	e := open(t, gs)
	n := e.nodeIn(task)
	_, err := e.match(Dispatcher{Registry: reg}, card(n), builder)
	var r *Refused
	if !errors.As(err, &r) || r.Check != "budget" || r.Outcome != types.OutcomeCouldNotRun || r.Reason != vocab.ReasonPreconditionFailed || r.Hold != nil {
		t.Fatalf("a priced executor with no rails: %v", err)
	}
	if _, live, err := e.liveOn(n); err != nil || live {
		t.Fatalf("the refused match placed a lease: %v", err)
	}
	// Rails with no USD rail (DefaultCaps') fail closed the same (Opus #302 H2).
	noUSD := Dispatcher{Registry: reg, Rails: func(*eventstore.Tx, types.TaskID) (graph.Caps, error) { return graph.DefaultCaps, nil }}
	if _, err := e.match(noUSD, card(n), builder); !errors.As(err, &r) || r.Reason != vocab.ReasonPreconditionFailed {
		t.Fatalf("a priced executor under no USD rail: %v", err)
	}
	free, _ := stub(t, func(doc *types.ExecutorRegistry) {
		for i := range doc.Executors {
			doc.Executors[i].Price, doc.Executors[i].CostBasis = nil, types.RegistryExecutorCostBasisFree
		}
	})
	if _, err := e.match(Dispatcher{Registry: free}, card(n), builder); err != nil {
		t.Errorf("an unpriced executor with no rails: %v", err)
	}
}
