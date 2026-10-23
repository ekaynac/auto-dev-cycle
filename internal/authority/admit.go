// SPDX-License-Identifier: Apache-2.0

package authority

// Admission (M1 P22a-3; D51; I22): one admitted task through M1, a held
// task included.
//   - The controller admits: its system actor's command (the reconcile
//     loop's Admit action, P22a-4) runs Admit, which takes the next task
//     (NextAdmission) and records task.admitted, then the task's candidate
//     template (graph.Candidate, under the deferred-surface guard), in that
//     one command: no node of a task precedes its admission.
//   - The next task is the oldest in Ready (by id: TypeIDs are time
//     ordered) whose approval is in force, and only while no admitted task
//     is non-terminal. A held task stays admitted; only task.done (the
//     review's go selected, P22a-6) ends one. A cancelled task, one not in
//     Ready, or one with no approval in force is never admitted.
//   - The admitting command re-checks all of it in its own transaction
//     (AdmitTask), whatever decided it: the task is still in Ready (not
//     cancelled since), its approval in force, the slot free. The fold
//     refuses the same from the log alone, on append and on replay.
//   - The tasks projection holds the admitted task under the admitted key
//     until it is done. Admission removes the task's ready/ key, so the
//     Ready bound frees a place.
//   - Tasks is the graph's claim authority (graph.Tasks): a node is claimed
//     or reused only while its task is admitted (I22 consumption) and its
//     approval in force, so a revocation before the first claim refuses
//     it, authority_revoked, and no attempt starts (I1 consumption); an
//     admitted task keeps the hold and revoke path.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The admission and done event kinds, the State of a done task and the
// key of the admitted task in the tasks projection.
const (
	TaskAdmittedEvent = "task.admitted/v1"
	TaskDoneEvent     = "task.done/v1"
	TaskDoneState     = "done"
	admittedKey       = "admitted"
	// admissiblePrefix indexes, in task id order, every task admission may
	// take: "admissible/<task>" while it is in Ready with its approval in
	// force (Astra #316 M3). putTask keeps it.
	admissiblePrefix = "admissible/"
)

// putTask stores t's record and keeps the admissible index in step with
// it: every write of a task record goes through it.
func putTask(tx *eventstore.Tx, t TaskRecord) error {
	err := putJSON(tx, TaskProjection, t.ID, t)
	if admissible(t) == nil {
		return errors.Join(err, putJSON(tx, TaskProjection, admissiblePrefix+t.ID, t.ID))
	}
	return errors.Join(err, tx.Delete(TaskProjection, admissiblePrefix+t.ID))
}

// AdmittedTask is the admitted task that is not done, if any.
func AdmittedTask(tx *eventstore.Tx) (types.TaskID, bool, error) {
	return loadJSON[types.TaskID](tx, TaskProjection, admittedKey)
}

// NextAdmission is the task admission takes next, if any: none while an
// admitted task is not done; else the oldest Ready task whose approval is
// in force: the admissible index's first key, one read whatever the
// history (Astra #316 M3).
func NextAdmission(tx *eventstore.Tx) (types.TaskID, bool, error) {
	if _, busy, err := tx.Get(TaskProjection, admittedKey); err != nil || busy {
		return "", false, err
	}
	key, ok, err := tx.FirstKey(TaskProjection, admissiblePrefix)
	return types.TaskID(strings.TrimPrefix(key, admissiblePrefix)), ok, err
}

// admissible refuses t unless it is in Ready with its approval in force.
func admissible(t TaskRecord) error {
	switch {
	case t.State != TaskReady:
		return fmt.Errorf("task %s is %s, not %s", t.ID, t.State, TaskReady)
	case t.Approval == nil || t.Approval.Status != ApprovalInForce:
		return fmt.Errorf("task %s has no approval in force", t.ID)
	}
	return nil
}

// Admit is the admitting command's decision: the next task's admission
// and its candidate template, under allowed (adcd's node kinds); no
// events when no task is next.
func Admit(tx *eventstore.Tx, allowed []types.NodeKind) ([]eventstore.Event, error) {
	id, ok, err := NextAdmission(tx)
	if err != nil || !ok {
		return nil, err
	}
	return AdmitTask(tx, id, allowed)
}

// AdmitTask is task's task.admitted, then its candidate template's nodes
// (graph.Candidate, built here: no other template, Opus #309 L1), checked
// again in this command: the task is recorded and in Ready (not cancelled
// since it was chosen), its approval in force, and no admitted task is not
// done; the template passes the deferred-surface guard
// (graph.RecordTemplate).
func AdmitTask(tx *eventstore.Tx, task types.TaskID, allowed []types.NodeKind) ([]eventstore.Event, error) {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	if err != nil || !ok {
		return nil, errors.Join(err, fmt.Errorf("authority: %w: task %s is not recorded", eventstore.ErrStaleExpectedState, task))
	}
	if err := admissible(t); err != nil {
		return nil, fmt.Errorf("authority: %w: %w", eventstore.ErrStaleExpectedState, err)
	}
	if held, busy, err := tx.Get(TaskProjection, admittedKey); err != nil || busy {
		return nil, errors.Join(err, fmt.Errorf("authority: %w: task %s is admitted and not done", eventstore.ErrStaleExpectedState, held))
	}
	template, err := graph.Candidate(t.ID, t.Project)
	if err != nil {
		return nil, err
	}
	nodes, err := graph.RecordTemplate(template, t.ID, t.Project, allowed)
	if err != nil {
		return nil, err
	}
	evs, err := event(TaskAdmittedEvent, t.ID, &t.Project, types.TaskAdmitted{Task: t.ID, Project: t.Project, Approval: t.Approval.ID,
		At: types.Time(tx.Now().Format(time.RFC3339Nano))})
	return append(evs, nodes...), err
}

// DoneTask is the admitted task's task.done, as a candidate: it frees
// admission (the loop records it once its candidate is decided, 22a-7).
// It is refused unless its candidate is decided (graph.CandidateGo,
// checked here too, whoever calls it: Mythos chunk 5a L1) and Endable.
func DoneTask(tx *eventstore.Tx, task types.TaskID) ([]eventstore.Event, error) {
	return done(tx, task, types.TaskDoneAsCandidate, nil)
}

// AbandonTask is the admitted task's task.done, abandoned (#226 entry 100):
// its approval is no longer in force (authority_revoked), and no lease on
// any of its nodes is live. The reconcile loop records it (P22a-4).
func AbandonTask(tx *eventstore.Tx, task types.TaskID) ([]eventstore.Event, error) {
	reason := types.TaskDoneReasonValueAuthorityRevoked
	return done(tx, task, types.TaskDoneAsAbandoned, &reason)
}

func done(tx *eventstore.Tx, task types.TaskID, as types.TaskDoneAs, reason *types.TaskDoneReasonValue) ([]eventstore.Event, error) {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("authority: %w: task %s is not recorded", eventstore.ErrStaleExpectedState, task)
	}
	if err := endable(tx, t, as); err != nil { // never stale: a refusal that persists is logged (Opus #408 H1)
		return nil, fmt.Errorf("authority: task %s is not %s: %w", task, as, err)
	}
	if as == types.TaskDoneAsCandidate {
		if go_, err := decided(tx, t.ID); err != nil { // a read fault, never a refusal (Mythos #408 L5)
			return nil, err
		} else if !go_ {
			return nil, errors.Join(fmt.Errorf("authority: task %s is not %s: %w: no review of its last build round completed go", task, as, ErrNotEndable))
		}
	}
	return event(TaskDoneEvent, t.ID, &t.Project, types.TaskDone{Task: t.ID, Project: t.Project, As: as, Reason: reason,
		At: types.Time(tx.Now().Format(time.RFC3339Nano))})
}

// endable refuses t's end as as unless t is admitted and: abandoned, its
// approval is not in force; as a candidate, none of its current nodes is
// claimed or held (Opus #309 M2); and, either way, nothing runs any of its
// nodes, superseded ones included, whatever their state: every lease on
// their units has ended and every attempt on them is sealed (Astra #316
// H1; Opus #312 M3). It reads the task's node index and each unit's open
// index, never the whole graph (M3).
func endable(tx *eventstore.Tx, t TaskRecord, as types.TaskDoneAs) error {
	if t.State != TaskAdmitted {
		return fmt.Errorf("%w: task %s is %s, not admitted", ErrNotEndable, t.ID, t.State)
	}
	if as == types.TaskDoneAsAbandoned && t.Approval != nil && t.Approval.Status == ApprovalInForce {
		return fmt.Errorf("%w: task %s's approval is in force: it is not abandoned", ErrNotEndable, t.ID)
	}
	nodes, err := graph.TaskNodes(tx, t.ID)
	if err != nil {
		return err
	}
	for _, n := range nodes { // every node claimed or held first, then the open units (Opus #408 re-check L1)
		if s := n.State.State; as == types.TaskDoneAsCandidate && (s == types.NodeStateStateClaimed || s == types.NodeStateStateHeld) {
			return fmt.Errorf("%w: task %s's node %s is %s", ErrNotEndable, t.ID, n.Node.ID, s)
		}
	}
	for _, n := range nodes {
		unit := graph.UnitOf(n.Node.ID)
		what, entry, running, err := lease.Unended(tx, unit)
		if err != nil {
			return err
		}
		if running {
			return ErrEnding{Unit: unit, Node: n.Node.ID, Entry: entry, Lease: strings.HasPrefix(entry, "lea_"), What: what}
		}
	}
	return nil
}

// EndingOf is how task, admitted, would end now, and Endable's refusal of
// it: abandoned once its approval is not in force (Abandon's case), else
// as a candidate when decided (decided; Done's); ok false when it would
// end neither way. The loop (reconcile.Observe: Candidate, and Ending
// either way: Opus #408 re-check M2), the runner's End and the card
// (ending_blocked) all read it.
func EndingOf(tx *eventstore.Tx, task types.TaskID) (as types.TaskDoneAs, ok bool, err error) {
	t, found, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	switch {
	case err != nil:
		return "", false, err
	case !found || t.State != TaskAdmitted:
		return "", false, nil
	case t.Approval == nil || t.Approval.Status != ApprovalInForce:
		as = types.TaskDoneAsAbandoned
	default:
		go_, err := decided(tx, task)
		if err != nil || !go_ {
			return "", false, err
		}
		as = types.TaskDoneAsCandidate
	}
	return as, true, endable(tx, t, as)
}

// decided reports whether task's candidate is decided: graph.CandidateGo
// over its nodes, the board's done_candidate rule (DoneTask's and
// EndingOf's; Mythos chunk 5a L1, L2).
func decided(tx *eventstore.Tx, task types.TaskID) (bool, error) {
	nodes, err := graph.TaskNodes(tx, task)
	return err == nil && graph.CandidateGo(nodes), err
}

// Endable is endable for task, read in tx: nil when task's end as as may be
// recorded. Its refusal is ErrNotEndable; while a unit of the task still
// runs, an ErrEnding. The one rule for "the task may end", DoneTask's and
// AbandonTask's and the loop's (reconcile.Observe; Opus #408 H1).
func Endable(tx *eventstore.Tx, task types.TaskID, as types.TaskDoneAs) error {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("%w: task %s is not recorded", ErrNotEndable, task))
	}
	return endable(tx, t, as)
}

// ErrNotEndable is Endable's refusal: never stale, so a refusal that
// persists is the loop's to log (Opus #408 H1).
var ErrNotEndable = errors.New("authority: the task cannot end yet")

// The causes an ErrEnding gives a reader (the board's ending_blocked): the
// controller's phrases, never the error's own text, which may name local
// paths or stores (#226 entry 166).
const (
	EndingSeal  = "the attempt's seal did not land"
	EndingLease = "the lease did not end"
)

// EndingCauses are every cause an ErrEnding gives.
var EndingCauses = []string{EndingSeal, EndingLease}

// ErrEnding is Endable's refusal while Node's Unit still runs: a lease not
// ended (Lease), or an attempt not sealed. What is the store's own text,
// for the log only.
type ErrEnding struct {
	Unit  types.UnitID
	Node  types.NodeID
	Entry string // the open index's entry: the lease's or the attempt's id
	Lease bool
	What  string
}

func (e ErrEnding) Error() string {
	return fmt.Sprintf("%s: node %s still runs: %s", ErrNotEndable, e.Node, e.What)
}

func (e ErrEnding) Unwrap() error { return ErrNotEndable }

// Cause is e's phrase, one of EndingCauses.
func (e ErrEnding) Cause() string {
	if e.Lease {
		return EndingLease
	}
	return EndingSeal
}

// systemEvent loads ev's task, under ev's id and project, and refuses ev
// unless a live system actor wrote it at its time.
func systemEvent(tx *eventstore.Tx, ev eventstore.Event, task types.TaskID, project types.ProjectID, at types.Time) (TaskRecord, error) {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	if err != nil || !ok || ev.Subject != string(task) || ev.Project == nil || *ev.Project != string(t.Project) || t.Project != project ||
		at != types.Time(ev.At) {
		return t, errors.Join(err, errors.New("an admission or a done is of a recorded task, under its id and project, at its event's time"))
	}
	by, err := liveActor(tx, types.ActorID(ev.Actor))
	if err == nil && by.Kind != types.ActorKindSystem {
		err = fmt.Errorf("actor %s is a %s, not the controller's system actor", by.ID, by.Kind)
	}
	return t, err
}

// admitTask admits ev, an admission: by the system actor, of a task in
// Ready under the approval in force it names, while no admitted task is
// not done. The task leaves the Ready bound and holds the admitted key.
func admitTask(tx *eventstore.Tx, ev eventstore.Event) error {
	var x types.TaskAdmitted
	if err := json.Unmarshal(ev.Payload, &x); err != nil {
		return err
	}
	t, err := systemEvent(tx, ev, x.Task, x.Project, x.At)
	if err == nil {
		err = admissible(t)
	}
	if err == nil && t.Approval.ID != x.Approval {
		err = fmt.Errorf("the admission names approval %s, not the task's %s", x.Approval, t.Approval.ID)
	}
	if err != nil {
		return err
	}
	if held, busy, err := tx.Get(TaskProjection, admittedKey); err != nil || busy {
		return errors.Join(err, fmt.Errorf("task %s is admitted and not done (I22)", held))
	}
	t.State = TaskAdmitted
	return errors.Join(putTask(tx, t), putJSON(tx, TaskProjection, admittedKey, t.ID),
		tx.Delete(TaskProjection, "ready/"+string(t.Project)+"/"+t.ID))
}

// doneTask admits ev, a done: by the system actor, of the admitted task,
// as endable allows its end; an abandonment names its reason, a candidate
// none. It frees admission.
func doneTask(tx *eventstore.Tx, ev eventstore.Event) error {
	var x types.TaskDone
	if err := json.Unmarshal(ev.Payload, &x); err != nil {
		return err
	}
	t, err := systemEvent(tx, ev, x.Task, x.Project, x.At)
	if err == nil && (x.As == types.TaskDoneAsAbandoned) != (x.Reason != nil) {
		err = errors.New("an abandonment names its reason, and a candidate none")
	}
	if err == nil {
		err = endable(tx, t, x.As)
	}
	if err != nil {
		return err
	}
	t.State, t.DoneAs = TaskDoneState, string(x.As)
	return errors.Join(putTask(tx, t), tx.Delete(TaskProjection, admittedKey))
}

// Tasks is the graph's task authority (graph.Tasks), over the tasks
// projection.
type Tasks struct{}

// Recordable refuses a node of task unless the task is admitted.
func (Tasks) Recordable(tx *eventstore.Tx, task types.TaskID) error {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	switch {
	case err != nil:
		return err
	case !ok:
		return graph.ErrNotAdmitted{Task: task, State: "unrecorded"}
	case t.State != TaskAdmitted:
		return graph.ErrNotAdmitted{Task: task, State: t.State}
	}
	return nil
}

// Claimable refuses a run of a node of task unless the task is admitted
// with its approval in force.
func (ts Tasks) Claimable(tx *eventstore.Tx, task types.TaskID) error {
	if err := ts.Recordable(tx, task); err != nil {
		return err
	}
	t, _, err := loadJSON[TaskRecord](tx, TaskProjection, string(task))
	if err == nil && (t.Approval == nil || t.Approval.Status != ApprovalInForce) {
		err = graph.ErrRevalidated{Reason: vocab.ReasonAuthorityRevoked, Detail: fmt.Sprintf("task %s's approval is not in force", task)}
	}
	return err
}
