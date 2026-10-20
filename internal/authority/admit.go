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
)

// AdmittedTask is the admitted task that is not done, if any.
func AdmittedTask(tx *eventstore.Tx) (types.TaskID, bool, error) {
	return loadJSON[types.TaskID](tx, TaskProjection, admittedKey)
}

// NextAdmission is the task admission takes next, if any: none while an
// admitted task is not done; else the oldest Ready task whose approval is
// in force.
func NextAdmission(tx *eventstore.Tx) (types.TaskID, bool, error) {
	if _, busy, err := tx.Get(TaskProjection, admittedKey); err != nil || busy {
		return "", false, err
	}
	ids, err := TaskIDs(tx)
	for _, id := range ids {
		t, ok, lerr := loadJSON[TaskRecord](tx, TaskProjection, id)
		if err = errors.Join(err, lerr); err != nil {
			return "", false, err
		}
		if ok && admissible(t) == nil {
			return t.ID, true, nil
		}
	}
	return "", false, err
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
// admission (P22a-6 records it when the task's review go is selected).
// None of the task's current nodes may be claimed or held.
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
	if err == nil && ok {
		err = endable(tx, t, as)
	}
	if err != nil || !ok {
		return nil, errors.Join(fmt.Errorf("authority: %w: task %s is not %s", eventstore.ErrStaleExpectedState, task, as), err)
	}
	return event(TaskDoneEvent, t.ID, &t.Project, types.TaskDone{Task: t.ID, Project: t.Project, As: as, Reason: reason,
		At: types.Time(tx.Now().Format(time.RFC3339Nano))})
}

// endable refuses t's end as as unless t is admitted and: as a candidate,
// none of its current nodes is claimed or held (Opus #309 M2); abandoned,
// its approval is not in force and nothing runs any of its nodes: every
// lease on their units has ended and every attempt on them is sealed (no
// attempt of it runs; Opus #312 M3).
func endable(tx *eventstore.Tx, t TaskRecord, as types.TaskDoneAs) error {
	if t.State != TaskAdmitted {
		return fmt.Errorf("task %s is %s, not admitted", t.ID, t.State)
	}
	if as == types.TaskDoneAsAbandoned && t.Approval != nil && t.Approval.Status == ApprovalInForce {
		return fmt.Errorf("task %s's approval is in force: it is not abandoned", t.ID)
	}
	nodes, err := graph.Nodes(tx)
	for _, n := range nodes {
		if err != nil || n.Task != t.ID {
			continue
		}
		var what string
		var running bool
		switch s := n.State.State; {
		case as == types.TaskDoneAsCandidate && (s == types.NodeStateStateClaimed || s == types.NodeStateStateHeld):
			return fmt.Errorf("task %s's node %s is %s", t.ID, n.Node.ID, s)
		case as == types.TaskDoneAsAbandoned:
			if what, running, err = lease.Unended(tx, graph.UnitOf(n.Node.ID)); err == nil && running {
				return fmt.Errorf("task %s's node %s still runs: %s", t.ID, n.Node.ID, what)
			}
		}
	}
	return err
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
	return errors.Join(putJSON(tx, TaskProjection, t.ID, t), putJSON(tx, TaskProjection, admittedKey, t.ID),
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
	return errors.Join(putJSON(tx, TaskProjection, t.ID, t), tx.Delete(TaskProjection, admittedKey))
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
