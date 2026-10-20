// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// taskOf is node's task.
func taskOf(w *seeder, node types.NodeID) types.TaskID {
	var task types.TaskID
	w.view(func(tx *eventstore.Tx) error {
		nodes, err := graph.Nodes(tx)
		for _, n := range nodes {
			if n.Node.ID == node {
				task = n.Task
			}
		}
		return err
	})
	return task
}

// startedTask seeds, through w (its system and operator set), task B
// admitted and done untouched, then task A admitted, its plan claimed and
// its first attempt started under a lease reserving res; with a model call
// reporting 300 tokens in and 200 out when calls. It returns A, B, A's plan
// and the attempt.
func startedTask(t *testing.T, w *seeder, res []types.LeaseReservationsItem, calls bool) (running, done types.TaskID, plan types.NodeID, attempt types.AttemptID) {
	t.Helper()
	done = taskOf(w, plannedIn(t, w, graphProject))
	plan = plannedIn(t, w, graphProject) // B is done: its nodes untouched
	running = taskOf(w, plan)
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	ls := w.lease(4)
	ls.Unit, ls.Reservations = graph.UnitOf(plan), res
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	attempt = types.AttemptID(newID(t, "att"))
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, ls.ID, 2, attempt, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
	})
	if !calls {
		return running, done, plan, attempt
	}
	call := types.CallID(newID(t, "cal"))
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return modelcall.Intend(tx, gateway.Binding{Lease: ls.ID, Attempt: attempt, Call: call}, "stub")
	})
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: call, Attempt: attempt, Lease: ls.ID, Kind: types.UsageMeasurementKindDelta,
		Sequence: 1, Tokens: types.UsageMeasurementTokens{In: 300, Out: 200}, Turns: 1, Milliseconds: 5,
		Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	w.do(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(m)
		return []eventstore.Event{ev}, err
	})
	return running, done, plan, attempt
}

// P22a-7 (W18; §2.3 assertion 5): the task read of a task whose first
// attempt runs under a lease whose model call reported 300 tokens in and
// 200 out: its card (admitted, running) and its first-attempt metrics (the
// attempt, its node, its status, the seconds since the approval, no result
// yet, the tokens), valid against api/task.
func TestTaskReadMetrics(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	running, _, plan, attempt := startedTask(t, w, w.lease(4).Reservations, true)
	read := func() (rec types.TaskRead) {
		t.Helper()
		w.view(func(tx *eventstore.Tx) error {
			var op types.Actor
			raw, _, err := tx.Get(identity.ActorProjection, string(w.op))
			if err == nil {
				err = json.Unmarshal(raw, &op)
			}
			scope, serr := authority.ReadScopeOf(tx, op, time.Now())
			if err = errors.Join(err, serr); err != nil {
				return err
			}
			recs, err := api.Registered()["tasks"].List(tx, api.ReadAt{Now: time.Now(), Scope: scope})
			for _, r := range recs {
				if r, ok := r.(types.TaskRead); ok && r.ID == running {
					rec = r
				}
			}
			return err
		})
		return rec
	}
	rec := read()
	raw, err := json.Marshal(rec)
	card, _ := rec.Card.Variant.(*types.BoardPlacedCard)
	f := rec.FirstAttempt
	if err != nil || contract.Validate("api/task", raw) != nil || card == nil || !card.Admitted || card.Column != types.BoardPlacedCardColumnRunning ||
		f == nil || f.Attempt != attempt || f.Node != plan || f.Status != string(types.AttemptStatusRunning) || (f.SecondsAfterApproval == nil || *f.SecondsAfterApproval > 60) ||
		f.Outcome != nil || f.Reason != nil || f.Tokens == nil || f.Tokens.In != 300 || f.Tokens.Out != 200 {
		t.Fatalf("the running task's record: %s, %v", raw, err)
	}
	// The plan's result selected: the first attempt's outcome and reason are
	// its node's; the card shows the task admitted, nothing running.
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		nodes, err := graph.Nodes(tx)
		var key types.ComputationKey
		for _, n := range nodes {
			if err == nil && n.Node.ID == plan {
				key, err = graph.Key(n.Node, map[string]graph.Resolved{}, 1)
			}
		}
		if err != nil {
			return nil, err
		}
		r := types.Result{ID: types.ResultID(newID(t, "res")), Computation: key.Key, Kind: types.ResultKindPlan, Value: []types.Ref{},
			Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
		none := func(types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
		return nil, graph.Commit(tx, plan, r, graph.Env{Need: none, Authority: authority.Answers{}})
	})
	rec = read()
	card, _ = rec.Card.Variant.(*types.BoardPlacedCard)
	if f := rec.FirstAttempt; f == nil || f.Outcome == nil || *f.Outcome != types.OutcomeEmpty || f.Reason == nil || *f.Reason != vocab.ReasonCompletedClean ||
		card == nil || card.Column != types.BoardPlacedCardColumnReady {
		t.Errorf("after the plan's result: %+v, %+v", f, card)
	}
}

// adcWithin is adc under a deadline d: a wait that would outlast it ends
// could_not_run, so a broken wait fails its row in seconds.
func (c *commanded) adcWithin(d time.Duration, tok string, args ...string) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	env := map[string]string{cli.SocketEnv: filepath.Join(c.dir, privateSocket), client.TokenEnv: tok}
	var out, errb bytes.Buffer
	code := cli.Main(ctx, args, &out, &errb, cli.ClientFromEnv(func(k string) string { return env[k] }))
	return code, out.String(), errb.String()
}

// P22a-7 (W18), through the installed commands: adc task show prints the
// running task's record (card and first attempt); adc task wait exits 0 on
// a column reached, 5 when its timeout passes first, 6 when the task ended
// elsewhere, 1 for a task not read and 64 for a column that is none or no
// task given; adc task show of no task is refused.
func TestTaskShowAndWait(t *testing.T) {
	var running, done types.TaskID
	var attempt types.AttemptID
	c := runCommanded(t, func(w *seeder, _ types.ActorID) {
		running, done, _, attempt = startedTask(t, w, []types.LeaseReservationsItem{}, false)
	})
	op := c.op.Reveal()
	code, out, errb := c.adc(op, "task", "show", string(running), "--json")
	var rec types.TaskRead
	if code != entry.ExitOK || contract.Validate("api/task", []byte(out)) != nil || json.Unmarshal([]byte(out), &rec) != nil ||
		rec.FirstAttempt == nil || rec.FirstAttempt.Attempt != attempt || rec.FirstAttempt.Tokens != nil {
		t.Fatalf("task show: %d %s %s", code, out, errb)
	}
	if code, out, _ := c.adc(op, "task", "show", string(running)); code != entry.ExitOK || !strings.Contains(out, "tasks "+string(running)) {
		t.Errorf("task show, as text: %d %s", code, out)
	}
	for name, want := range map[string]struct {
		args []string
		code int
	}{
		"a column reached":         {[]string{string(running), "--until", "ready,running"}, entry.ExitOK},
		"the timeout passes first": {[]string{string(running), "--until", "done", "--timeout", "300ms", "--every", "100ms"}, entry.ExitTimedOut},
		"a task ended elsewhere":   {[]string{string(done), "--until", "running"}, entry.ExitEnded},
		"no task read":             {[]string{newID(t, "tsk"), "--until", "running"}, entry.ExitRefused},
		"a column that is none":    {[]string{string(running), "--until", "finished"}, entry.ExitUsage},
		"no task given":            {[]string{"--until", "running"}, entry.ExitUsage},
	} {
		if code, out, errb := c.adcWithin(5*time.Second, op, append([]string{"task", "wait"}, want.args...)...); code != want.code {
			t.Errorf("%s: %d, want %d: %s %s", name, code, want.code, out, errb)
		}
	}
	if code, out, errb := c.adc(op, "task", "show", newID(t, "tsk")); code != entry.ExitRefused {
		t.Errorf("task show of no task: %d %s %s", code, out, errb)
	}
}
