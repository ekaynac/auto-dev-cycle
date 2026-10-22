// SPDX-License-Identifier: Apache-2.0

// Package reconcile is adcd's reconcile loop (M1 P22a-4; D59): a pure
// decision from a snapshot of the store to typed actions, each run in its
// own command of the system actor, which rechecks there what the decision
// read.
//   - Observe takes a Snapshot in one read transaction: the admitted task,
//     whether its approval is in force, and its open nodes (runnable, or
//     waiting: an evaluation claims a node whose wait has ended); or the task
//     admission takes next. Each subject is read at its version (the seq of
//     its last event).
//   - Decide is pure. Admit the next task while none is admitted, when the
//     snapshot allows admission (the admission capability is active).
//     Abandon the admitted task once its approval is no longer in force
//     (#226 entry 100: it can run nothing more). Only when the snapshot
//     allows evaluation (the launch capability is active: until then a
//     claimed node would wait for an attempt nothing can start): Evaluate
//     the admitted task's open nodes; Match each of its claimed nodes an
//     executor runs that nothing is placed on (P22a-5c-1: a build); and
//     ClearRail each of its nodes held on the day's USD rail, which a new
//     day or a profile change may have reopened (#226 entry 120); and
//     Unblock each of its nodes held on configuration by the placer itself
//     (a placement hold, dispatch.PlacementSignal), which a configuration
//     change may have fixed (Mythos 22a-5c-1 M1). Holds are collected for
//     the kinds Match places only (Mythos L1). Nothing else. The Placer (adcd's) makes a Match's card and executor and a
//     ClearRail's check.
//   - Executor.Run runs one action in one command. The command expects each
//     subject at the version the snapshot read (the writer's expected-state
//     check), and its logic rechecks the authority in its own transaction:
//     AdmitTask the task's Ready state, approval and the admitted slot;
//     AbandonTask the revoked approval and that nothing runs the task's
//     nodes; Evaluate that the task is still the admitted one with its
//     approval in force (authority.Tasks, Opus #309 L5: a refusal the loop
//     reads, never a fold error), then graph.Evaluate, whose claims adcd's
//     fold checks again (I22, I1). A changed subject is refused
//     stale_expected_state, appending nothing; the next pass decides again
//     from a fresh snapshot. An action whose logic emits no event commits
//     nothing (Opus #312 M1): it is not counted, so it never nudges a pass.
//   - Loop runs passes on one goroutine: at start (the resynchronisation of
//     a restart), every Every, and on a Nudge (after a pass that ran an
//     action, or a command adcd applied). A refused action is logged and the
//     pass goes on; nothing it decided carries over to the next pass. A
//     refusal is logged once per action and subject: the same refusal of
//     the same action on the same subject in the next pass is not logged
//     again, until a pass refuses it otherwise or not at all (Opus #312 L5;
//     Opus 22a-5c-1 M1: two subjects refused for different reasons are each
//     logged once, never alternately every pass). A pass's own failure is
//     logged once while it repeats. A panic in
//     a pass is recovered and returned as its error (Opus #312 M2): the pass
//     nudges nothing, so the timer paces the retries and a start pass never
//     takes adcd down.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The system actions the loop's commands record (none is a registered
// action a principal may ask for).
const (
	AdmitAction     = "reconcile.admit"
	AbandonAction   = "reconcile.abandon"
	EvaluateAction  = "reconcile.evaluate"
	MatchAction     = "reconcile.match"
	ClearRailAction = "reconcile.clear_rail"
	UnblockAction   = "reconcile.unblock"
	LaunchAction    = "reconcile.launch"
)

// Actions are the loop's system actions.
var Actions = []string{AdmitAction, AbandonAction, EvaluateAction, MatchAction, ClearRailAction, LaunchAction, UnblockAction}

// Placed are the node kinds Match places on an executor: a build (P22a-5c-1).
// A review is the review capability's (22a-6); a plan, accept and gate run
// in the controller.
var Placed = []types.NodeKind{types.NodeKindBuild}

// Versioned is a subject read at a version: the seq of its last event.
type Versioned struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Gates are what the active capabilities let the loop do.
type Gates struct {
	// Admits: the next task may be admitted (admission active).
	Admits bool
	// Evaluates: the admitted task's nodes may be evaluated (launch active).
	Evaluates bool
}

// Snapshot is what Decide reads.
type Snapshot struct {
	Gates
	// Admitted is the admitted task that is not done (ID "": none),
	// Revoked whether its approval is no longer in force, and Open its
	// runnable and waiting nodes: a waiting node's producer may have
	// completed since, and nothing but an evaluation ends its wait.
	Admitted Versioned
	Revoked  bool
	Open     []Versioned
	// Unplaced are its claimed nodes of a Placed kind that nothing is
	// placed on (dispatch.Unplaced), DayHeld its nodes held on the day's
	// USD rail.
	Unplaced    []Versioned
	DayHeld     []Held
	PlacingHeld []Held // held on configuration by the placer
	// Next is the task admission takes next (ID "": none, or one is
	// admitted).
	Next Versioned
}

// Held is a node read at its version and one hold it holds.
type Held struct {
	Node   Versioned    `json:"node"`
	Hold   types.HoldID `json:"hold"`
	Signal string       `json:"signal"`
}

// Action is one decision: Admit, Abandon, Evaluate, Match or ClearRail.
type Action interface {
	action() string
	subject() string // what the refusal log keys on, with the action
	expected() []eventstore.Expectation
}

// Admit admits Task, read at its version, with its candidate template.
type Admit struct{ Task Versioned }

// Abandon ends Task, read at its version, abandoned: its approval is not in
// force.
type Abandon struct{ Task Versioned }

// Evaluate evaluates Task's Goals, each read at its version.
type Evaluate struct {
	Task  types.TaskID
	Goals []Versioned
}

// Match places Node, read at its version, on an executor (Placer.Place).
type Match struct{ Node Versioned }

// ClearRail clears Held's day-rail hold once the day's rail leaves its
// node room (Placer.Reopened).
type ClearRail struct{ Held Held }

func (a Admit) subject() string     { return a.Task.ID }
func (a Abandon) subject() string   { return a.Task.ID }
func (e Evaluate) subject() string  { return string(e.Task) }
func (m Match) subject() string     { return m.Node.ID }
func (c ClearRail) subject() string { return c.Held.Node.ID }
func (u Unblock) subject() string   { return u.Held.Node.ID }

// Unblock clears Held's placement hold once the configuration it named
// admits its node again (Placer.Unblocked).
type Unblock struct{ Held Held }

func (Unblock) action() string { return UnblockAction }

func (u Unblock) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: u.Held.Node.ID, Version: u.Held.Node.Version}}
}

func (Match) action() string     { return MatchAction }
func (ClearRail) action() string { return ClearRailAction }

func (m Match) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: m.Node.ID, Version: m.Node.Version}}
}

func (c ClearRail) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: c.Held.Node.ID, Version: c.Held.Node.Version}}
}

func (Admit) action() string    { return AdmitAction }
func (Abandon) action() string  { return AbandonAction }
func (Evaluate) action() string { return EvaluateAction }

func (a Admit) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: a.Task.ID, Version: a.Task.Version}}
}

func (a Abandon) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: a.Task.ID, Version: a.Task.Version}}
}

func (e Evaluate) expected() []eventstore.Expectation {
	out := make([]eventstore.Expectation, len(e.Goals))
	for i, g := range e.Goals {
		out[i] = eventstore.Expectation{Subject: g.ID, Version: g.Version}
	}
	return out
}

// Observe is the store's Snapshot in tx under g.
func Observe(tx *eventstore.Tx, g Gates) (Snapshot, error) {
	s := Snapshot{Gates: g}
	admitted, ok, err := authority.AdmittedTask(tx)
	if err != nil {
		return s, err
	}
	if !ok {
		next, found, err := authority.NextAdmission(tx)
		if err == nil && found {
			s.Next.ID = string(next)
			s.Next.Version, err = tx.SubjectVersion(string(next))
		}
		return s, err
	}
	s.Admitted.ID = string(admitted)
	if s.Admitted.Version, err = tx.SubjectVersion(s.Admitted.ID); err != nil {
		return s, err
	}
	var revoked graph.ErrRevalidated
	if err := (authority.Tasks{}).Claimable(tx, admitted); errors.As(err, &revoked) {
		s.Revoked = true
	} else if err != nil {
		return s, err
	}
	nodes, err := graph.TaskNodes(tx, admitted) // the task's index, never the whole graph (Astra #316 M3)
	day := graph.RailSignal(graph.RailDayUSD)
	for _, n := range nodes {
		if err != nil || n.Task != admitted {
			continue
		}
		v := Versioned{ID: string(n.Node.ID)}
		switch st := n.State.State; {
		case st == types.NodeStateStateRunnable || st == types.NodeStateStateWaiting:
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Open = append(s.Open, v)
		case st == types.NodeStateStateClaimed && g.Evaluates && slices.Contains(Placed, n.Node.Kind):
			var free bool
			if free, err = dispatch.Unplaced(tx, graph.UnitOf(n.Node.ID)); err == nil && free {
				v.Version, err = tx.SubjectVersion(v.ID)
				s.Unplaced = append(s.Unplaced, v)
			}
		case st == types.NodeStateStateHeld && g.Evaluates && slices.Contains(Placed, n.Node.Kind):
			if v.Version, err = tx.SubjectVersion(v.ID); err != nil {
				break
			}
			for _, h := range n.State.Holds {
				switch held := (Held{Node: v, Hold: h.ID, Signal: h.SignalInstance}); {
				case h.Hold == vocab.HoldBudgetRail && h.SignalInstance == day:
					s.DayHeld = append(s.DayHeld, held)
				case h.Hold == vocab.HoldSourceInvalidConfig && dispatch.IsPlacementSignal(h.SignalInstance):
					s.PlacingHeld = append(s.PlacingHeld, held)
				}
			}
		}
	}
	return s, err
}

// Decide is the actions s calls for, in order.
func Decide(s Snapshot) []Action {
	switch {
	case s.Admitted.ID == "" && s.Admits && s.Next.ID != "":
		return []Action{Admit{Task: s.Next}}
	case s.Admitted.ID != "" && s.Revoked:
		return []Action{Abandon{Task: s.Admitted}}
	case s.Admitted.ID != "" && s.Evaluates:
		var out []Action
		if len(s.Open) > 0 {
			out = append(out, Evaluate{Task: types.TaskID(s.Admitted.ID), Goals: slices.Clone(s.Open)})
		}
		for _, n := range s.Unplaced {
			out = append(out, Match{Node: n})
		}
		for _, h := range s.DayHeld {
			out = append(out, ClearRail{Held: h})
		}
		for _, h := range s.PlacingHeld {
			out = append(out, Unblock{Held: h})
		}
		return out
	}
	return nil
}

// Executor runs actions as the system actor.
type Executor struct {
	Store  *eventstore.Store
	System identity.Principal
	// Kinds are the node kinds a template may hold (adcd's).
	Kinds []types.NodeKind
	// Env is what evaluation reads.
	Env graph.Env
	// Placer places and reopens nodes (adcd's); nil refuses Match and
	// ClearRail.
	Placer Placer
}

// Placer is what Match and ClearRail do, in the action's command.
type Placer interface {
	// Place matches node on an executor (dispatch.Dispatcher.Match), or,
	// refused with a hold, raises it on node; another refusal is an error.
	Place(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error)
	// Reopened clears node's day-rail hold when the day's rail leaves it a
	// turn now (a new UTC day, or a profile change): nothing otherwise.
	Reopened(tx *eventstore.Tx, node types.NodeID, hold types.HoldID) ([]eventstore.Event, error)
	// Unblocked clears node's placement hold (hold, on signal) when the
	// configuration now admits node: nothing otherwise.
	Unblocked(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error)
}

var errNoPlacer = errors.New("reconcile: no placer: the loop places nothing")

// errNothing is an action whose logic emits no event: its command commits
// nothing.
var errNothing = errors.New("reconcile: the action changes nothing")

// Run runs a in one command; stale_expected_state when a subject it read
// has changed.
func (x Executor) Run(ctx context.Context, a Action) error {
	cmd, err := ids.New("cmd")
	input, merr := json.Marshal(a)
	if err = errors.Join(err, merr); err != nil {
		return err
	}
	var logic func(*eventstore.Tx) ([]eventstore.Event, error)
	switch a := a.(type) {
	case Admit:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return authority.AdmitTask(tx, types.TaskID(a.Task.ID), x.Kinds)
		}
	case Abandon:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return authority.AbandonTask(tx, types.TaskID(a.Task.ID))
		}
	case Evaluate:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) { return x.evaluate(tx, a) }
	case Match:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Placer == nil {
				return nil, errNoPlacer
			}
			return x.Placer.Place(tx, types.NodeID(a.Node.ID))
		}
	case ClearRail:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Placer == nil {
				return nil, errNoPlacer
			}
			return x.Placer.Reopened(tx, types.NodeID(a.Held.Node.ID), a.Held.Hold)
		}
	case Unblock:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Placer == nil {
				return nil, errNoPlacer
			}
			return x.Placer.Unblocked(tx, types.NodeID(a.Held.Node.ID), a.Held.Hold, a.Held.Signal)
		}
	default:
		return fmt.Errorf("reconcile: no action %T", a)
	}
	_, err = x.Store.SubmitApplied(ctx, x.System.Command(cmd, a.action(), cmd, input, a.expected()), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := logic(tx)
			if err == nil && len(evs) == 0 {
				err = errNothing
			}
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

// evaluate is the evaluation of a's goals, while a's task is the admitted
// one with its approval in force (a refusal, authority_revoked, rather than
// an error of the fold's); adcd's fold rechecks each claim's task.
func (x Executor) evaluate(tx *eventstore.Tx, a Evaluate) ([]eventstore.Event, error) {
	admitted, ok, err := authority.AdmittedTask(tx)
	if err != nil || !ok || admitted != a.Task {
		return nil, errors.Join(err, fmt.Errorf("reconcile: %w: task %s is no longer the admitted one", eventstore.ErrStaleExpectedState, a.Task))
	}
	if err := (authority.Tasks{}).Claimable(tx, a.Task); err != nil {
		return nil, err
	}
	goals := make([]types.NodeID, len(a.Goals))
	for i, g := range a.Goals {
		goals[i] = types.NodeID(g.ID)
	}
	return graph.Evaluate(tx, goals, x.Env)
}

// Loop runs reconcile passes on one goroutine.
type Loop struct {
	Exec  Executor
	Gates Gates
	// Every is the period between passes (5 s in adcd).
	Every time.Duration
	Log   io.Writer
	nudge chan struct{}
	// logged are the refusals the last pass logged or kept quiet, by action
	// and subject; failed is the pass failure logged last ("" after a pass
	// that did not fail).
	logged map[string]string
	failed string
}

// NewLoop is a loop over x under g.
func NewLoop(x Executor, g Gates, every time.Duration, log io.Writer) *Loop {
	return &Loop{Exec: x, Gates: g, Every: every, Log: log, nudge: make(chan struct{}, 1)}
}

// Nudge asks for a pass now; it never blocks.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

// Run runs a pass now (the resynchronisation at start), then one every
// Every and on each Nudge, until ctx ends.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.Every)
	defer t.Stop()
	for {
		if ran, err := l.Pass(ctx); err != nil && ctx.Err() == nil {
			if msg := err.Error(); msg != l.failed {
				fmt.Fprintf(l.Log, "adcd: reconcile: %s\n", msg)
				l.failed = msg
			}
		} else if err == nil {
			l.failed = "" // a pass that did not fail (Mythos 22a-5c-1 L2)
			if ran > 0 {
				l.Nudge() // what it changed may call for more
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.nudge:
		}
	}
}

// Pass observes the store, decides and runs each action; ran is how many
// were applied. A refused action is logged (but while ctx ends), a stale
// one or one that changes nothing silently: the next pass decides again. A
// panic in the observation or an action is recovered as the pass's error.
func (l *Loop) Pass(ctx context.Context) (ran int, err error) {
	var s Snapshot
	if err := recovered("the observation", func() error {
		return l.Exec.Store.View(ctx, func(tx *eventstore.Tx) (err error) {
			s, err = Observe(tx, l.Gates)
			return err
		})
	}); err != nil {
		return 0, err
	}
	logged := map[string]string{}
	defer func() { l.logged = logged }()
	for _, a := range Decide(s) {
		var panicked *panicError
		switch err := recovered(a.action(), func() error { return l.Exec.Run(ctx, a) }); {
		case err == nil:
			ran++
		case errors.As(err, &panicked):
			return ran, err
		case errors.Is(err, eventstore.ErrStaleExpectedState), errors.Is(err, errNothing), ctx.Err() != nil:
		default:
			key, msg := a.action()+" "+a.subject(), fmt.Sprintf("%s refused: %v", a.action(), err)
			if l.logged[key] != msg {
				fmt.Fprintf(l.Log, "adcd: reconcile: %s\n", msg)
			}
			logged[key] = msg
		}
	}
	return ran, nil
}

// panicError is a panic recovered in a pass.
type panicError struct {
	what  string
	value any
}

func (p *panicError) Error() string { return fmt.Sprintf("%s panicked: %v", p.what, p.value) }

// recovered runs f, returning a panic in it as a panicError.
func recovered(what string, f func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &panicError{what: what, value: v}
		}
	}()
	return f()
}
