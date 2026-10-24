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
	ls.Unit, ls.Reservations, ls.Price = graph.UnitOf(plan), res, startedPrice
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
		Sequence: 1, Tokens: startedTokens, Turns: 1, Milliseconds: 5,
		Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	w.do(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(m)
		return []eventstore.Event{ev}, err
	})
	return running, done, plan, attempt
}

// startedPrice is the price startedTask's lease records (nil: none), and
// startedTokens the tokens its call reports.
var (
	startedPrice  *types.RegistryPrice
	startedTokens = types.UsageMeasurementTokens{In: 300, Out: 200}
)

// taskReadAs is task's record as actor reads it, the zero record when it
// reads none.
func taskReadAs(t *testing.T, w *seeder, actor types.ActorID, task types.TaskID) types.TaskRead {
	t.Helper()
	return taskReadWith(t, w, actor, task, nil)
}

// taskReadWith is taskReadAs with the reads' stall (api.ReadAt.Stalled).
func taskReadWith(t *testing.T, w *seeder, actor types.ActorID, task types.TaskID, stalled func(types.TaskID) (string, bool)) (rec types.TaskRead) {
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
		recs, err := api.Registered()["tasks"].List(tx, api.ReadAt{Now: time.Now(), Scope: scope, Stalled: stalled})
		for _, r := range recs {
			if r, ok := r.(types.TaskRead); ok && r.ID == task {
				rec = r
			}
		}
		return err
	})
	return rec
}

// taskOneAs is task's record in the one task's read (api.ReadTask) as
// actor reads it, its benchmark facts included; the zero record when it
// reads none.
func taskOneAs(t *testing.T, w *seeder, actor types.ActorID, task types.TaskID) (rec types.TaskRead) {
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
		rec, _, err = api.ReadTask(tx, api.ReadAt{Now: time.Now(), Scope: scope}, task, true)
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
		none := func(*eventstore.Tx, types.Node, types.InputSpec) (*receipt.Requirement, error) { return nil, nil }
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
		f.Outcome != nil || f.Reason != nil || f.Tokens == nil || f.Tokens.In != 300 || f.Tokens.Out != 200 ||
		f.Charge == nil || f.Charge.OutstandingCalls != 1 || f.Charge.SettledUsdMicros != nil || f.Charge.OutstandingUsdMicros != nil {
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
			ended(w, w.sys, first)
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SupersedeAttempt(tx, first, 3, ls.ID, 2, second, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
			})
			by := &second
			if !bySuccessor { // a result with no attempt is recorded only once no attempt runs the node (Astra #316 H1)
				by = nil
				w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
					return lease.LoseRunning(tx, second, types.AttemptLostReasonValueControllerRestart)
				})
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
	if board.ID != running || board.FirstAttempt != nil || board.Bench != nil {
		t.Errorf("a board reader: %+v", board)
	}
	if rec := taskReadAs(t, w, reader("board", "attempt"), running); rec.FirstAttempt == nil || rec.FirstAttempt.Attempt != attempt || rec.FirstAttempt.Tokens != nil || rec.Bench != nil {
		t.Errorf("an attempt reader: %+v", rec)
	}
	if rec := taskOneAs(t, w, reader("board", "attempt"), running); rec.ID != running || rec.Bench != nil {
		t.Errorf("an attempt reader's one-task read: %+v", rec)
	}
	if rec := taskOneAs(t, w, reader("board", "model_call"), running); rec.ID != running || rec.Bench != nil {
		t.Errorf("a model-call reader not reading attempts: %+v", rec)
	}
	both := reader("board", "attempt", "model_call")
	if rec := taskReadAs(t, w, both, running); rec.FirstAttempt == nil || rec.FirstAttempt.Tokens == nil || rec.FirstAttempt.Tokens.In != 300 || rec.Bench != nil {
		t.Errorf("a model-call reader's list (never the benchmark facts: Opus #430 H1): %+v", rec)
	}
	if rec := taskOneAs(t, w, both, running); rec.FirstAttempt == nil || rec.Bench == nil {
		t.Errorf("a model-call reader's one-task read: %+v", rec)
	}
}

// attemptsOn claims node and starts k attempts on it, one after another:
// each lost, sealed and superseded by the next, under a lease of its own.
func attemptsOn(t *testing.T, w *seeder, node types.NodeID, k int) {
	t.Helper()
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, node, map[string]graph.Resolved{}, 1)
	})
	base := types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"}
	units := graph.Units{Tasks: authority.Tasks{}}
	var prev types.AttemptID
	for i := range k {
		ls := w.lease(4)
		ls.Unit, ls.Reservations = graph.UnitOf(node), []types.LeaseReservationsItem{}
		w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
		w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
		})
		a := types.AttemptID(newID(t, "att"))
		if i == 0 {
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartAttempt(tx, ls.ID, 2, a, base) })
		} else {
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.LoseRunning(tx, prev, types.AttemptLostReasonValueControllerRestart)
			})
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SealMinimal(tx, prev, 2, units, w.blobs, nil, w.sys)
			})
			ended(w, w.sys, prev)
			w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SupersedeAttempt(tx, prev, 3, ls.ID, 2, a, base)
			})
		}
		prev = a
	}
}

// Astra #335 M1: a task read's work is bounded by the board's and the
// task's own nodes, never by the installation's attempt history: reading
// project A's task, with its attempts and model calls, reads the same
// number of projection rows whether project B's task has started none, 2
// attempts or 12 (counted, never timed): B's first attempt is never read
// for A's reader.
func TestTaskReadWorkIsBounded(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	work := func(k int) int64 {
		w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
		op := fullHistory(t, w)
		mine := taskOf(w, plannedIn(t, w, graphProject))
		attemptsOn(t, w, plannedIn(t, w, other), k) // A done; B admitted, with its history
		now := time.Now().UTC()
		sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op}
		g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{graphProject}},
			Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"board", "attempt", "model_call"}},
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
		before := eventstore.ProjectionRowsRead()
		if rec := taskReadAs(t, w, sup.ID, mine); rec.ID != mine {
			t.Fatalf("A's task is not read: %+v", rec)
		}
		return eventstore.ProjectionRowsRead() - before
	}
	if none, few, many := work(0), work(2), work(12); none != few || few != many {
		t.Errorf("rows read for A's task: %d with no attempt in B, %d with 2, %d with 12", none, few, many)
	}
}

// Opus #320 M2: a task's first attempt is one in its own project, since
// every node of a task is in its project: the fold refuses one recorded in
// another (so the read needs no check of its own).
func TestTaskReadFirstAttemptInItsProject(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	task := taskOf(w, plannedIn(t, w, graphProject))
	stray := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	err := w.try(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: task, Project: other, Node: stray})
	})
	if err == nil || !strings.Contains(err.Error(), "in another project") {
		t.Errorf("a node of the task in another project: %v", err)
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
//
// The cancel may find the private socket's bearer verifications full
// (defaultLimits.private.verify, 2): a read holds its slot through its
// Argon2id hash and its whole read, the background wait reads every 100 ms,
// and a read a timed-out wait abandoned holds its slot until its hash
// ends. A full gate refuses at once, never queued: HTTP 503 with
// Retry-After and nothing decided (#226 entry 125), which adc reports as
// could_not_run with the key to repeat. So the cancel is repeated with the
// same idempotency key, after its Retry-After, at most cancelTries times,
// and only on that refusal: it is decided once.
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
		code, _, _ := c.adcWithin(15*time.Second, op, "task", "wait", string(ready), "--until", "closed", "--every", "100ms")
		waited <- code
	}()
	time.Sleep(300 * time.Millisecond)
	scope := fmt.Sprintf(`{"task":%q}`, ready)
	const cancelTries = 5
	key := "cancel-" + newID(t, "cmd")
	for try := 1; ; try++ {
		code, out, errb := c.adc(op, "task", "cancel", "--expect", fmt.Sprint(readyAt), "--reason", "r", "--json", "--scope", scope, "--idempotency-key", key)
		if code == entry.ExitOK {
			// Never a replay: a refusal that decided nothing left nothing to replay.
			if a, ok := response(t, out).(*types.CommandApplied); !ok || a.Disposition != "applied" || a.Replayed {
				t.Fatalf("the cancel, try %d: %s", try, out)
			}
			break
		}
		if try == cancelTries || code != entry.ExitCouldNotRun || !strings.Contains(errb, "HTTP 503 without a command response") {
			t.Fatalf("the cancel, try %d of %d: %d %s %s\nthe controller's log:\n%s", try, cancelTries, code, out, errb, c.logs.String())
		}
		t.Logf("the cancel, try %d: HTTP 503 without a command response; repeating it with its key", try)
		time.Sleep(time.Second) // the refusal's Retry-After
	}
	if code := <-waited; code != entry.ExitOK {
		t.Errorf("a wait the task closes during: %d", code)
	}
	if code, out, errb := c.adc(op, "task", "show", newID(t, "tsk")); code != entry.ExitRefused {
		t.Errorf("task show of no task: %d %s %s", code, out, errb)
	}
}

// P22a-5c-2c (Astra #335 L3; slice plan §4.3; Opus 22a-5c-2c M1): the
// first attempt's charge is priced at its lease's registry price, never
// from a reported cost. Its call not settled yet is listed apart, as the
// USD rail counts it (dispatch.Cost): on a live lease, the reservation or
// the usage so far, the larger, at the higher token rate. The settled
// charge is 0 with no settled call. With no price on the lease
// (TestTaskReadMetrics) both are unknown (null), never zero.
//   - 300 in and 200 out reported on a 4-token reservation: 500 tokens at
//     10 USD/Mtok, 5000 micros;
//   - a zero report (#226 entry 69) on a 100-token reservation: at least
//     the priced reservation, 1000 micros, never 0.
func TestTaskReadCharge(t *testing.T) {
	startedPrice = &types.RegistryPrice{InputUsdMicrosPerMtok: 2_000_000, OutputUsdMicrosPerMtok: 10_000_000}
	t.Cleanup(func() { startedPrice, startedTokens = nil, types.UsageMeasurementTokens{In: 300, Out: 200} })
	for name, c := range map[string]struct {
		reserve int64
		tokens  types.UsageMeasurementTokens
		want    int64
	}{
		"usage past the reservation": {4, types.UsageMeasurementTokens{In: 300, Out: 200}, 5000},
		"a zero report":              {100, types.UsageMeasurementTokens{}, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			startedTokens = c.tokens
			w := seed(t, stateDir(t), daemonProjection(t, tokens(1000)))
			fullHistory(t, w)
			running, _, _, _ := startedTask(t, w, w.lease(c.reserve).Reservations, true)
			rec := taskReadAs(t, w, w.op, running)
			raw, err := json.Marshal(rec)
			ch := rec.FirstAttempt.Charge
			if err != nil || contract.Validate("api/task", raw) != nil || ch == nil || ch.OutstandingCalls != 1 || ch.OutstandingUsdMicros == nil ||
				*ch.OutstandingUsdMicros != c.want || ch.SettledUsdMicros == nil || *ch.SettledUsdMicros != 0 {
				t.Fatalf("the priced first attempt's charge: %s, %v", raw, err)
			}
		})
	}
}
