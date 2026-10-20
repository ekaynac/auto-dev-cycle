// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// taskReadAs is task's record as actor reads it, the zero record when it
// reads none.
func taskReadAs(t *testing.T, w *seeder, actor types.ActorID, task types.TaskID) (rec types.TaskRead) {
	t.Helper()
	w.view(func(tx *eventstore.Tx) error {
		var a types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(actor))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		scope, serr := authority.ReadScopeOf(tx, a, time.Now())
		if err = errors.Join(err, serr); err != nil {
			return err
		}
		recs, err := api.Registered()["tasks"].List(tx, api.ReadAt{Now: time.Now(), Scope: scope})
		for _, r := range recs {
			if r, ok := r.(types.TaskRead); ok && r.ID == task {
				rec = r
			}
		}
		return err
	})
	return rec
}

// commitPlan commits plan's completed result, empty/completed_clean,
// recorded by attempt once it submits it (nil: by no attempt).
func commitPlan(t *testing.T, w *seeder, plan types.NodeID, attempt *types.AttemptID) {
	t.Helper()
	if attempt != nil {
		w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			a, _, _, err := lease.AttemptOf(tx, *attempt)
			if err != nil {
				return nil, err
			}
			return lease.SubmitResult(tx, *attempt, a.FencingToken, types.SubmittedResult{Call: types.CallID(newID(t, "cal")), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
		})
	}
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
		env := graph.Env{Need: none, Authority: authority.Answers{}}
		if attempt == nil {
			return nil, graph.Commit(tx, plan, r, env)
		}
		return nil, graph.CommitAttempt(tx, plan, r, *attempt, env)
	})
}

// P22a-7 (W18; §2.3 assertion 5): the task read of a task whose first
// attempt runs under a lease whose model call reported 300 tokens in and
// 200 out: its card (admitted, running) and its first-attempt metrics (the
// attempt, its node, its status, the seconds since the approval, no result
// yet, the tokens), valid against api/task. Once that attempt records its
// own result, its outcome and reason are the first attempt's.
func TestTaskReadMetrics(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	running, _, plan, attempt := startedTask(t, w, w.lease(4).Reservations, true)
	rec := taskReadAs(t, w, w.op, running)
	raw, err := json.Marshal(rec)
	card, _ := rec.Card.Variant.(*types.BoardPlacedCard)
	f := rec.FirstAttempt
	if err != nil || contract.Validate("api/task", raw) != nil || card == nil || !card.Admitted || card.Column != types.BoardPlacedCardColumnRunning ||
		f == nil || f.Attempt != attempt || f.Node != plan || f.Status != string(types.AttemptStatusRunning) || (f.SecondsAfterApproval == nil || *f.SecondsAfterApproval > 60) ||
		f.Outcome != nil || f.Reason != nil || f.Tokens == nil || f.Tokens.In != 300 || f.Tokens.Out != 200 {
		t.Fatalf("the running task's record: %s, %v", raw, err)
	}
	commitPlan(t, w, plan, &attempt)
	rec = taskReadAs(t, w, w.op, running)
	card, _ = rec.Card.Variant.(*types.BoardPlacedCard)
	if f := rec.FirstAttempt; f == nil || f.Outcome == nil || *f.Outcome != types.OutcomeEmpty || f.Reason == nil || *f.Reason != vocab.ReasonCompletedClean ||
		card == nil || card.Column != types.BoardPlacedCardColumnReady {
		t.Errorf("after the attempt's own result: %+v, %+v", f, card)
	}
}

// Opus #320 H1 and M2: the first attempt, lost, sealed and superseded by a
// second one (its start recorded too), stays the first (the earliest
// start), and a result it did not record is never its own: neither its
// successor's, selected, nor one recorded with no attempt.
func TestTaskReadFirstAttemptOwnResult(t *testing.T) {
	for name, bySuccessor := range map[string]bool{"the successor's result": true, "a result with no attempt": false} {
		t.Run(name, func(t *testing.T) {
			w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
			fullHistory(t, w)
			running, _, plan, first := startedTask(t, w, []types.LeaseReservationsItem{}, false)
			units := graph.Units{Tasks: authority.Tasks{}}
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.LoseRunning(tx, first, types.AttemptLostReasonValueControllerRestart)
			})
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SealMinimal(tx, first, 2, units, w.blobs, nil, w.sys)
			})
			ls := w.lease(4)
			ls.Unit, ls.Reservations = graph.UnitOf(plan), []types.LeaseReservationsItem{}
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
			})
			second := types.AttemptID(newID(t, "att"))
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SupersedeAttempt(tx, first, 3, ls.ID, 2, second, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
			})
			by := &second
			if !bySuccessor {
				by = nil
			}
			commitPlan(t, w, plan, by)
			w.view(func(tx *eventstore.Tx) error {
				if _, ok, err := lease.StartedAt(tx, second); err != nil || !ok {
					t.Errorf("the successor's start is not recorded: %v", err)
				}
				return nil
			})
			f := taskReadAs(t, w, w.op, running).FirstAttempt
			if f == nil || f.Attempt != first || f.Status != string(types.AttemptStatusSuperseded) || f.Outcome != nil || f.Reason != nil {
				t.Fatalf("the first attempt: %+v", f)
			}
		})
	}
}

// Opus #320 M1: the attempt fields are read under the attempt read kind,
// the tokens under model_call, as adc events gates attempt.* and
// model_call.*: a supervisor reading only the board gets the card and a
// null first_attempt; one also reading attempts gets it with null tokens;
// one also reading model calls gets the tokens.
func TestTaskReadKinds(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	op := fullHistory(t, w)
	running, _, _, attempt := startedTask(t, w, w.lease(4).Reservations, true)
	now := time.Now().UTC()
	reader := func(kinds ...types.Name) types.ActorID {
		sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op}
		g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{graphProject}},
			Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: kinds},
			ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
			ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
		w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			a, err := identity.CreateActor(sup)
			if err != nil {
				return nil, err
			}
			gr, err := authority.CreateGrant(tx, g)
			return append(a, gr...), err
		})
		return sup.ID
	}
	board := taskReadAs(t, w, reader("board"), running)
	if board.ID != running || board.FirstAttempt != nil {
		t.Errorf("a board reader: %+v", board)
	}
	if f := taskReadAs(t, w, reader("board", "attempt"), running).FirstAttempt; f == nil || f.Attempt != attempt || f.Tokens != nil {
		t.Errorf("an attempt reader: %+v", f)
	}
	if f := taskReadAs(t, w, reader("board", "attempt", "model_call"), running).FirstAttempt; f == nil || f.Tokens == nil || f.Tokens.In != 300 {
		t.Errorf("a model-call reader: %+v", f)
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

// P22a-7 (W18; Opus #320 H2, M2), through the installed commands: adc
// task show prints the running task's record (card and first attempt); adc
// task wait --until closed|held exits 0 on a state seen (the done task
// closed; the running task held; a Ready task closed during the wait, by
// its cancel 300 ms in), 6 when the task closed without the state asked
// for, 5 when its timeout passes first, 1 for a task not read and 64 for a
// state that is none or no task given; adc task show of no task is
// refused.
func TestTaskShowAndWait(t *testing.T) {
	var running, done, ready types.TaskID
	var attempt types.AttemptID
	var readyAt int64
	c := runCommanded(t, func(w *seeder, _ types.ActorID) {
		var plan types.NodeID
		running, done, plan, attempt = startedTask(t, w, []types.LeaseReservationsItem{}, false)
		raiseOn(t, w, w.op, plan)
		_, ready = readyTask(t, w, w.sys)
		w.view(func(tx *eventstore.Tx) (err error) { readyAt, err = tx.SubjectVersion(string(ready)); return err })
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
		"closed":                   {[]string{string(done), "--until", "closed"}, entry.ExitOK},
		"held":                     {[]string{string(running), "--until", "held"}, entry.ExitOK},
		"closed, not held":         {[]string{string(done), "--until", "held"}, entry.ExitEnded},
		"the timeout passes first": {[]string{string(running), "--until", "closed", "--timeout", "300ms", "--every", "100ms"}, entry.ExitTimedOut},
		"no task read":             {[]string{newID(t, "tsk"), "--until", "closed"}, entry.ExitRefused},
		"a column is no state":     {[]string{string(running), "--until", "running"}, entry.ExitUsage},
		"no task given":            {[]string{"--until", "closed"}, entry.ExitUsage},
	} {
		if code, out, errb := c.adcWithin(5*time.Second, op, append([]string{"task", "wait"}, want.args...)...); code != want.code {
			t.Errorf("%s: %d, want %d: %s %s", name, code, want.code, out, errb)
		}
	}
	waited := make(chan int, 1)
	go func() {
		code, _, _ := c.adcWithin(5*time.Second, op, "task", "wait", string(ready), "--until", "closed", "--every", "100ms")
		waited <- code
	}()
	time.Sleep(300 * time.Millisecond)
	scope := fmt.Sprintf(`{"task":%q}`, ready)
	if code, out, errb := c.adc(op, "task", "cancel", "--expect", fmt.Sprint(readyAt), "--reason", "r", "--json", "--scope", scope); code != entry.ExitOK {
		t.Fatalf("the cancel: %d %s %s", code, out, errb)
	}
	if code := <-waited; code != entry.ExitOK {
		t.Errorf("a wait the task closes during: %d", code)
	}
	if code, out, errb := c.adc(op, "task", "show", newID(t, "tsk")); code != entry.ExitRefused {
		t.Errorf("task show of no task: %d %s %s", code, out, errb)
	}
}
