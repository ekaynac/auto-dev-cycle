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
//     the admitted task's open nodes; Plan each of its claimed plan nodes,
//     whose result the controller records itself (P22a-5c-3: the plan
//     brief, internal/brief); Match each of its claimed nodes an
//     executor runs that nothing is placed on (P22a-5c-1: a build); and
//     ClearRail each of its nodes held on the day's USD rail, which a new
//     day or a profile change may have reopened (#226 entry 120); and
//     Unblock each of its nodes held on configuration by the placer itself
//     (a placement hold, dispatch.PlacementSignal, the launch's among them)
//     or on the launch's missing image (wrong_owner, dispatch.IsImageSignal;
//     #226 entry 156), which a configuration change may have fixed (Mythos
//     22a-5c-1 M1); and Run each of its
//     placed builds whose lease is granted, no attempt started on it
//     (P22a-5c-2a: the Runner, adcd's, starts the attempt's run outside any
//     command and returns; a run already going starts nothing); and Accept
//     each of its claimed accept nodes and Gate each of its claimed gate
//     nodes, the controller's (P22a-5c-2b-2, P22a-5c-2b-3: the Acceptor,
//     adcd's, likewise). Holds are collected for
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
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
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
	PlanAction      = "reconcile.plan"
	// RunAction is the attempt run's own commands (adcd's runner, P22a-5c-2a):
	// ending a lease never launched, and the build's completion.
	RunAction = "reconcile.run"
	// ReviewAction records a task's review nodes, commits a review
	// requirement's result, and re-derives a held review's roster
	// (P22a-6a).
	ReviewAction = "reconcile.review"
)

// Actions are the loop's system actions.
var Actions = []string{AdmitAction, AbandonAction, EvaluateAction, MatchAction, ClearRailAction, LaunchAction, UnblockAction, PlanAction, RunAction, ReviewAction}

// Placed are the node kinds Match places on an executor: a build (P22a-5c-1).
// A review is the review capability's (22a-6); a plan, accept and gate run
// in the controller.
var Placed = []types.NodeKind{types.NodeKindBuild}

// ControllerRun are the node kinds the controller runs itself, no lease:
// accept and the gate (P22a-5c-2b-2, P22a-5c-2b-3). Their runner holds
// are offered to Unblock as a placed node's are (Opus 22a-5c-2b-2 H1), so
// a bounded retry clears.
var ControllerRun = []types.NodeKind{types.NodeKindAccept, types.NodeKindGate}

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
	// Reviews: the admitted task's review is recorded, its requirement
	// committed and its review matched (review active; P22a-6a). It acts
	// only with Evaluates.
	Reviews bool
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
	PlacingHeld []Held // held on configuration by the placer, by the runner, or on the launch's image
	// Plans are its claimed plan nodes, whose result the controller records.
	Plans []Versioned
	// Granted are its claimed nodes of a Placed kind whose live lease is
	// granted, no attempt started on it: the runs to start.
	Granted []Granted
	// Accepts are its claimed accept nodes, and Gated its claimed gate
	// nodes, which the controller runs (P22a-5c-2b-2, P22a-5c-2b-3).
	Accepts []Versioned
	Gated   []Versioned
	// Unsealed are its claimed accept nodes whose input's build attempt is
	// not sealed yet: the seal the controller re-runs (P22a-5c-2c).
	Unsealed []Versioned
	// Reviewable is the admitted task's completed gate when the task has no
	// review node yet (ID "": none): its review is recorded (RecordReview).
	// Requirements are its claimed review_requirement nodes a review that
	// is not superseded takes, and ReviewHeld its review nodes held
	// reviewer_unavailable, and the requirement nodes so held that such a
	// review takes (P22a-6a; Opus 22a-6a re-check H1).
	Reviewable   Versioned
	Requirements []Versioned
	ReviewHeld   []Held
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

// runnerPrefix marks a hold the attempt run raised on a failure before
// its launch (Opus 22a-5c-2a H2): Unblock clears it, as a placement hold,
// once the runner's preconditions hold again (the Placer decides).
const runnerPrefix = "runner: "

// RunnerSignal is the signal of a runner's hold on what.
func RunnerSignal(what string) string { return runnerPrefix + what }

// IsRunnerSignal: signal is a runner hold's (RunnerSignal).
func IsRunnerSignal(signal string) bool { return strings.HasPrefix(signal, runnerPrefix) }

// Offered reports whether the loop offers a node's hold of kind on signal
// to the code that clears it, the one rule Observe collects holds by
// (#226 entry 156; Opus #400 M1): budget_rail on the day's USD rail
// (ClearRail: Placer.Reopened); source_invalid_config on a placement or a
// runner signal, and wrong_owner on a launch's missing image (Unblock:
// Placer.Unblocked); reviewer_unavailable (ReviewUnblock: Reviewer.Unheld).
// The loop leaves every other hold to its own clearer (an answer,
// task.raise_rail) or to its stated terminal state.
func Offered(kind, signal string) bool {
	switch kind {
	case vocab.HoldBudgetRail:
		return signal == graph.RailSignal(graph.RailDayUSD)
	case vocab.HoldSourceInvalidConfig:
		return dispatch.IsPlacementSignal(signal) || IsRunnerSignal(signal)
	case vocab.HoldWrongOwner:
		return dispatch.IsImageSignal(signal)
	}
	return kind == vocab.HoldReviewerUnavailable
}

// Granted is a placed node and its granted lease.
type Granted struct {
	Node  Versioned     `json:"node"`
	Lease types.LeaseID `json:"lease"`
}

// Action is one decision: Admit, Abandon, Evaluate, Plan, Match, ClearRail, Unblock or Run.
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

// Plan records Node's result, read at its version (Planner.Plan).
type Plan struct{ Node Versioned }

func (Plan) action() string { return PlanAction }

func (p Plan) subject() string { return p.Node.ID }

func (p Plan) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: p.Node.ID, Version: p.Node.Version}}
}

// Accept starts the controller's run of a claimed accept node (Acceptor):
// never a command of its own.
type Accept struct{ Node Versioned }

func (Accept) action() string    { return RunAction }
func (a Accept) subject() string { return a.Node.ID }
func (Accept) expected() []eventstore.Expectation {
	return nil
}

// Gate starts the controller's run of a claimed gate node (Acceptor's
// Gate): never a command of its own.
type Gate struct{ Node Versioned }

func (Gate) action() string    { return RunAction }
func (g Gate) subject() string { return g.Node.ID }
func (Gate) expected() []eventstore.Expectation {
	return nil
}

// RecordReview records the admitted task's review nodes once its gate
// Gate completed: its requirement and its review, the roster the registry
// derives now (Reviews.Record).
type RecordReview struct{ Gate Versioned }

func (RecordReview) action() string    { return ReviewAction }
func (r RecordReview) subject() string { return r.Gate.ID }
func (r RecordReview) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: r.Gate.ID, Version: r.Gate.Version}}
}

// Require commits a claimed review_requirement node's result: its params
// as its conclusions (Reviews.Require).
type Require struct{ Node Versioned }

func (Require) action() string    { return ReviewAction }
func (r Require) subject() string { return r.Node.ID }
func (r Require) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: r.Node.ID, Version: r.Node.Version}}
}

// ReviewUnblock re-derives a review's roster held reviewer_unavailable
// (Reviews.Unheld).
type ReviewUnblock struct{ Held Held }

func (ReviewUnblock) action() string    { return ReviewAction }
func (r ReviewUnblock) subject() string { return r.Held.Node.ID }
func (r ReviewUnblock) expected() []eventstore.Expectation {
	return []eventstore.Expectation{{Subject: r.Held.Node.ID, Version: r.Held.Node.Version}}
}

// Reseal starts the controller's seals of the build attempts claimed
// accept nodes' inputs wait on (Acceptor's Reseal), the pass's whole list:
// never a command of its own.
type Reseal struct{ Nodes []Versioned }

func (Reseal) action() string  { return RunAction }
func (Reseal) subject() string { return "unsealed" }
func (Reseal) expected() []eventstore.Expectation {
	return nil
}

// Run starts the attempt run of a placed node under its granted lease
// (Runner): never a command of its own.
type Run struct{ Granted Granted }

func (Run) action() string    { return RunAction }
func (r Run) subject() string { return r.Granted.Node.ID }
func (Run) expected() []eventstore.Expectation {
	return nil
}

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
	reviews := g.Evaluates && g.Reviews
	var gate Versioned
	reviewed := false                   // the task has a review node, in any state
	required := map[types.NodeID]bool{} // the requirement a review that is not superseded takes
	var claimedReqs []Versioned
	var heldReqs []Held
	for _, n := range nodes {
		if err != nil || n.Task != admitted {
			continue
		}
		v := Versioned{ID: string(n.Node.ID)}
		reviewed = reviewed || n.Node.Kind == types.NodeKindReview
		if n.Node.Kind == types.NodeKindReview && n.State.State != types.NodeStateStateSuperseded {
			for _, in := range n.Node.Inputs {
				if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement {
					required[src.Producer] = true
				}
			}
		}
		switch st := n.State.State; {
		case st == types.NodeStateStateCompleted && n.Node.Kind == types.NodeKindGate:
			gate.ID = v.ID
		case st == types.NodeStateStateClaimed && reviews && n.Node.Kind == types.NodeKindReviewRequirement:
			v.Version, err = tx.SubjectVersion(v.ID)
			claimedReqs = append(claimedReqs, v)
		case st == types.NodeStateStateHeld && reviews && (n.Node.Kind == types.NodeKindReview || n.Node.Kind == types.NodeKindReviewRequirement) &&
			slices.ContainsFunc(n.State.Holds, func(h types.NodeHold) bool { return h.Hold == vocab.HoldReviewerUnavailable }):
			if v.Version, err = tx.SubjectVersion(v.ID); err != nil {
				break
			}
			for _, h := range n.State.Holds {
				switch {
				case h.Hold != vocab.HoldReviewerUnavailable || !Offered(h.Hold, h.SignalInstance):
				case n.Node.Kind == types.NodeKindReview:
					s.ReviewHeld = append(s.ReviewHeld, Held{Node: v, Hold: h.ID, Signal: h.SignalInstance})
				default:
					heldReqs = append(heldReqs, Held{Node: v, Hold: h.ID, Signal: h.SignalInstance})
				}
			}
		case st == types.NodeStateStateRunnable || st == types.NodeStateStateWaiting || st == types.NodeStateStateHeld && len(n.State.Holds) > 0 &&
			!slices.ContainsFunc(n.State.Holds, func(h types.NodeHold) bool { return h.Hold != vocab.HoldAwaitingReceipt }):
			// A node held only awaiting_receipt is a goal: the evaluator
			// re-resolves it, clearing or re-raising its holds (P22a-6b).
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Open = append(s.Open, v)
		case st == types.NodeStateStateClaimed && g.Evaluates && n.Node.Kind == types.NodeKindAccept:
			var sealed bool
			if sealed, err = SealedInput(tx, n.Node.ID); err == nil {
				v.Version, err = tx.SubjectVersion(v.ID)
				if sealed {
					s.Accepts = append(s.Accepts, v)
				} else {
					s.Unsealed = append(s.Unsealed, v)
				}
			}
		case st == types.NodeStateStateClaimed && g.Evaluates && n.Node.Kind == types.NodeKindGate:
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Gated = append(s.Gated, v)
		case st == types.NodeStateStateClaimed && g.Evaluates && n.Node.Kind == types.NodeKindPlan:
			v.Version, err = tx.SubjectVersion(v.ID)
			s.Plans = append(s.Plans, v)
		case st == types.NodeStateStateClaimed && g.Evaluates && (slices.Contains(Placed, n.Node.Kind) || reviews && n.Node.Kind == types.NodeKindReview):
			var free bool
			if free, err = dispatch.Unplaced(tx, graph.UnitOf(n.Node.ID)); err == nil && free {
				v.Version, err = tx.SubjectVersion(v.ID)
				s.Unplaced = append(s.Unplaced, v)
			} else if err == nil && slices.Contains(Placed, n.Node.Kind) { // a granted review's run is 22a-6c's
				var l types.LeaseID
				if l, free, err = granted(tx, graph.UnitOf(n.Node.ID)); err == nil && free {
					v.Version, err = tx.SubjectVersion(v.ID)
					s.Granted = append(s.Granted, Granted{Node: v, Lease: l})
				}
			}
		case st == types.NodeStateStateHeld && g.Evaluates && (slices.Contains(Placed, n.Node.Kind) || slices.Contains(ControllerRun, n.Node.Kind) ||
			reviews && n.Node.Kind == types.NodeKindReview):
			if v.Version, err = tx.SubjectVersion(v.ID); err != nil {
				break
			}
			for _, h := range n.State.Holds {
				switch held := (Held{Node: v, Hold: h.ID, Signal: h.SignalInstance}); {
				case !Offered(h.Hold, h.SignalInstance), h.Hold == vocab.HoldReviewerUnavailable: // not the loop's; a review's is collected above
				case h.Hold == vocab.HoldBudgetRail:
					s.DayHeld = append(s.DayHeld, held)
				default:
					s.PlacingHeld = append(s.PlacingHeld, held)
				}
			}
		}
	}
	for _, v := range claimedReqs { // only one a live review takes, never an orphan (Opus 22a-6a M1)
		if required[types.NodeID(v.ID)] {
			s.Requirements = append(s.Requirements, v)
		}
	}
	for _, h := range heldReqs { // a held requirement a live review waits on (Opus 22a-6a re-check H1)
		if required[types.NodeID(h.Node.ID)] {
			s.ReviewHeld = append(s.ReviewHeld, h)
		}
	}
	if err == nil && reviews && gate.ID != "" && !reviewed {
		gate.Version, err = tx.SubjectVersion(gate.ID)
		s.Reviewable = gate
	}
	return s, err
}

// granted is unit's live lease when it is granted: placed, no attempt
// started on it.
func granted(tx *eventstore.Tx, unit types.UnitID) (types.LeaseID, bool, error) {
	id, live, err := lease.LiveOn(tx, unit)
	if err != nil || !live {
		return "", false, err
	}
	l, _, ok, err := lease.LeaseOf(tx, id)
	return id, ok && l.State == types.LeaseStateGranted, err
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
		for _, n := range s.Plans {
			out = append(out, Plan{Node: n})
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
		for _, g := range s.Granted {
			out = append(out, Run{Granted: g})
		}
		for _, n := range s.Accepts {
			out = append(out, Accept{Node: n})
		}
		for _, n := range s.Gated {
			out = append(out, Gate{Node: n})
		}
		if s.Reviewable.ID != "" {
			out = append(out, RecordReview{Gate: s.Reviewable})
		}
		for _, n := range s.Requirements {
			out = append(out, Require{Node: n})
		}
		for _, h := range s.ReviewHeld {
			out = append(out, ReviewUnblock{Held: h})
		}
		if len(s.Unsealed) > 0 {
			out = append(out, Reseal{Nodes: slices.Clone(s.Unsealed)})
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
	// Planner records a plan node's result (adcd's); nil leaves a claimed
	// plan to whoever records it, as before 22a-5c-3: Plan changes nothing.
	Planner Planner
	// Runner starts placed nodes' attempt runs (adcd's); nil starts none:
	// Run changes nothing.
	Runner Runner
	// Acceptor starts accept nodes' runs (adcd's); nil starts none: Accept
	// changes nothing.
	Acceptor Acceptor
	// Reviews records and re-derives the review nodes (adcd's); nil
	// records none: RecordReview, Require and ReviewUnblock change nothing.
	Reviews Reviews
}

// Reviews is what RecordReview, Require and ReviewUnblock do, in the
// action's command (P22a-6a).
type Reviews interface {
	// Record records the review nodes of gate's task: a review_requirement
	// and a review, rostered by the registry now.
	Record(tx *eventstore.Tx, gate types.NodeID) ([]eventstore.Event, error)
	// Require commits node's result, a claimed review requirement's.
	Require(tx *eventstore.Tx, node types.NodeID) error
	// Unheld clears node's reviewer_unavailable hold (hold, on signal), or
	// supersedes node onto an independent roster, once the registry now
	// gives one: nothing otherwise.
	Unheld(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error)
}

// Acceptor is what Accept and Gate do, outside any command: the
// controller's own runs of a task's checks.
type Acceptor interface {
	// Accept starts node's accept run and returns: false when one is
	// already going for node, starting nothing.
	Accept(node types.NodeID) bool
	// Gate starts node's gate run, likewise.
	Gate(node types.NodeID) bool
	// Reseal starts the seal of the build attempt each accept node of nodes
	// (every one the pass found waiting on its input's seal) waits on, when
	// it is due, forgetting any other it waited on, and returns false when
	// it starts nothing (none due, or each going).
	Reseal(nodes []types.NodeID) bool
}

// Runner is what Run does, outside any command.
type Runner interface {
	// Run starts node's attempt run under granted lease l and returns:
	// false when one is already going for node, starting nothing.
	Run(node types.NodeID, l types.LeaseID) bool
}

// Planner is what Plan does, in the action's command.
type Planner interface {
	// Plan records node's result, emitting its events (graph.Commit).
	Plan(tx *eventstore.Tx, node types.NodeID) error
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
	if r, ok := a.(Run); ok {
		if x.Runner == nil || !x.Runner.Run(types.NodeID(r.Granted.Node.ID), r.Granted.Lease) {
			return errNothing
		}
		return nil
	}
	if acc, ok := a.(Accept); ok {
		if x.Acceptor == nil || !x.Acceptor.Accept(types.NodeID(acc.Node.ID)) {
			return errNothing
		}
		return nil
	}
	if g, ok := a.(Gate); ok {
		if x.Acceptor == nil || !x.Acceptor.Gate(types.NodeID(g.Node.ID)) {
			return errNothing
		}
		return nil
	}
	if rs, ok := a.(Reseal); ok {
		nodes := make([]types.NodeID, 0, len(rs.Nodes))
		for _, n := range rs.Nodes {
			nodes = append(nodes, types.NodeID(n.ID))
		}
		if x.Acceptor == nil || !x.Acceptor.Reseal(nodes) {
			return errNothing
		}
		return nil
	}
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
	case Plan:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Planner == nil {
				return nil, errNothing
			}
			return nil, x.Planner.Plan(tx, types.NodeID(a.Node.ID))
		}
	case ClearRail:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Placer == nil {
				return nil, errNoPlacer
			}
			return x.Placer.Reopened(tx, types.NodeID(a.Held.Node.ID), a.Held.Hold)
		}
	case RecordReview:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Reviews == nil {
				return nil, errNothing
			}
			return x.Reviews.Record(tx, types.NodeID(a.Gate.ID))
		}
	case Require:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Reviews == nil {
				return nil, errNothing
			}
			return nil, x.Reviews.Require(tx, types.NodeID(a.Node.ID))
		}
	case ReviewUnblock:
		logic = func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			if x.Reviews == nil {
				return nil, errNothing
			}
			return x.Reviews.Unheld(tx, types.NodeID(a.Held.Node.ID), a.Held.Hold, a.Held.Signal)
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
			if err == nil && len(evs) == 0 && tx.Emitted() == 0 {
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

// SealedInput reports whether accept node's claimed input may be accepted
// now: false only while the build attempt that recorded it is not sealed
// yet (its checkpoint, which accept reads, is the command after its
// result), so the loop decides no Accept, and no run spins, until it is
// (Opus 22a-5c-2b-3 H2). An input of any other shape is accept's to refuse.
func SealedInput(tx *eventstore.Tx, node types.NodeID) (bool, error) {
	ins, _, err := graph.ClaimInputs(tx, node)
	if err != nil || len(ins) != 1 || ins[0].Result == nil {
		return true, err
	}
	raw, ok, err := tx.Get(graph.ResultProjection, string(*ins[0].Result))
	var rr types.ResultRecorded
	if err == nil && ok {
		err = json.Unmarshal(raw, &rr)
	}
	if err != nil || !ok || rr.Attempt == nil {
		return true, err
	}
	a, _, found, err := lease.AttemptAt(tx, *rr.Attempt)
	return err != nil || !found || a.Checkpoint != nil, err
}
