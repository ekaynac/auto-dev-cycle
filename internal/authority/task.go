// SPDX-License-Identifier: Apache-2.0

package authority

// Task intake (M1 slice plan §5.1 P3; D51): task.create puts a task in its
// project's Ready backlog and, for an operator, records the approval of its
// spec digest. Creation is not admission: no task leaves Ready here.
//   - Derive mints the id (Minted: never proposed; --expect 0 is its
//     absence). A supervisor needs a grant on the project and the seat.
//   - The logic runs after authority: an unregistered project, or an
//     attachment that is not the actor's own live upload, is
//     stale_expected_state; a sixth Ready task is ready_backlog_full. It
//     publishes the uploads; the drop Request returns runs after the commit.
//   - The spec digest is the scope's content digest (RFC 8785). The
//     attachments are the acceptance tests, classed restricted and withheld
//     from builder and reviewer; the fold refuses any other classing.
//   - An operator's create also commits approval.recorded (proposal null,
//     policy none, D31), which the fold admits only as exactly
//     TaskApprovalOf the task, in its creating command. A supervisor's task
//     has none. Revoking the approver revokes it, shown on the task.
//   - Inert until P22a: adcd projects task.created; task.create stays
//     deferred (the intake capability).

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The task action, its scope schema, its event kind, the projection
// holding the tasks (TaskRecord by id, with the ready/<project>/<task> and
// approver/<actor>/<task> indexes) and the Ready backlog's bound.
const (
	TaskCreateAction   = "task.create"
	TaskCreateScope    = "api/scope/task-create"
	TaskCreated        = "task.created/v1"
	TaskProjection     = "tasks"
	TaskObjectType     = "task"
	TaskReady          = "ready"
	TaskAdmitted       = "admitted" // P22a's task.admitted; the board reads it (P22a's obligations: backlog #226 entry 42)
	MaxReadyTasks      = 5
	ApprovalInForce    = "in_force"
	ApprovalRevoked    = "revoked"
	taskApprovalReason = "an operator's task.create approves the spec it records"
)

// TaskRecord is a task as recorded: the task.created payload, its state,
// the approval of its spec digest (nil: none) and its creating command.
type TaskRecord struct {
	types.Task
	State    string        `json:"state"`
	Approval *TaskApproval `json:"approval"`
	Command  string        `json:"command"`
}

// TaskApproval is the approval bound to a task's spec digest, in force or
// revoked.
type TaskApproval struct {
	ID        types.ApprovalID `json:"id"`
	Actor     types.ActorID    `json:"actor"`
	Digest    types.Digest     `json:"digest"`
	At        types.Time       `json:"at"`
	Status    string           `json:"status"`
	RevokedAt *types.Time      `json:"revoked_at"`
}

// TaskIDs is every recorded task's id, in id order: the keys under the
// tsk_ prefix, never the ready/ and approver/ indexes.
func TaskIDs(tx *eventstore.Tx) ([]types.TaskID, error) { return tx.Keys(TaskProjection, "tsk_") }

// TaskKinds registers the task event kinds: task.created (records/task)
// and task.rail_raised (records/rail-raised, P17a).
func TaskKinds() eventstore.Kinds {
	return eventstore.Kinds{TaskCreated: {Schema: "records/task"}, TaskRailRaised: {Schema: "records/rail-raised"}}
}

// TaskRecordOf is task id's record, which --expect is checked against.
func TaskRecordOf(id types.TaskID) func(*eventstore.Tx) (json.RawMessage, bool, error) {
	return func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(TaskProjection, id) }
}

// TaskAcceptance is the content digests of task id's acceptance blobs, from
// its record (I18: an intent is checked against them, never against a
// caller's list). The fold admits a task only with one acceptance blob per
// attachment (classed), so a task with attachments never reads as having
// none. An unknown task is refused.
func TaskAcceptance(tx *eventstore.Tx, id types.TaskID) ([]types.Digest, error) {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, id)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("authority: task %s has no record", id)
	}
	out := make([]types.Digest, len(t.Acceptance))
	for i, a := range t.Acceptance {
		out[i] = a.Digest
	}
	return out, nil
}

// TaskCreateHandler derives task.create.
type TaskCreateHandler struct{}

// Derive checks the scope, then derives a new task, under a fresh id, in
// the scope's project.
func (TaskCreateHandler) Derive(_ *eventstore.Tx, by types.Actor, input json.RawMessage) (Derived, error) {
	if by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return Derived{}, refusal(vocab.ReasonActorKindRefused, "actor %s is a %s; only operators and supervisors create tasks", by.ID, by.Kind)
	}
	in, err := taskInputOf(input)
	if err != nil {
		return Derived{}, err
	}
	id, err := ids.New("tsk")
	return Derived{Object: id, Minted: true, ObjectType: TaskObjectType, Project: &in.Project, Record: TaskRecordOf(id)}, err
}

// taskInputOf is the scope, refused as ErrInvalidScope unless its schema
// admits it and its attachment paths can coexist: none twice, none
// another's ancestor directory.
func taskInputOf(raw json.RawMessage) (types.TaskCreateScope, error) {
	in, err := contract.Decode[types.TaskCreateScope](TaskCreateScope, raw)
	seen := map[string]bool{} // path: true for a file, false for a directory
	for _, a := range in.Attachments {
		if _, taken := seen[a.Name]; taken && err == nil {
			err = fmt.Errorf("attachment %s is named twice or is another's directory", a.Name)
		}
		seen[a.Name] = true
		for d := path.Dir(a.Name); d != "."; d = path.Dir(d) {
			if file, ok := seen[d]; file && err == nil {
				err = fmt.Errorf("attachment %s is under attachment %s", a.Name, d)
			} else if !ok {
				seen[d] = false
			}
		}
	}
	if err != nil {
		return in, fmt.Errorf("%w: %s input: %w", ErrInvalidScope, TaskCreateAction, err)
	}
	return in, nil
}

// Uploads is where task.create's attachments are uploads
// (*blobstore.Store).
type Uploads interface {
	Publish(actor, digest string, now time.Time) (time.Time, error)
	Drop(actor, digest string, uploadedAt time.Time) error
}

// TaskCreate is task.create's request layer over the uploads.
type TaskCreate struct{ Uploads Uploads }

// Request is the logic for c, a command the Authorizer allowed (the task
// and its operator's approval), and the drop of its uploads once applied.
func (h TaskCreate) Request(c eventstore.Command) (eventstore.Decide, func(eventstore.Committed) error) {
	published := map[string]time.Time{} // digest: upload time
	decide := eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		id, bound := tx.Bound()
		if c.Action != TaskCreateAction || !bound || h.Uploads == nil {
			return nil, nil, fmt.Errorf("authority: %s is no derived %s with uploads", c.Action, TaskCreateAction)
		}
		in, err := taskInputOf(c.Input)
		by, err2 := liveActor(tx, types.ActorID(c.Actor))
		if err := errors.Join(err, err2, ids.Check(id, "tsk")); err != nil {
			return nil, nil, err
		}
		if _, ok, err := tx.Get(ProjectProjection, in.Project); err != nil {
			return nil, nil, err
		} else if !ok {
			return nil, nil, fmt.Errorf("authority: %w: project %s is not registered", eventstore.ErrStaleExpectedState, in.Project)
		}
		if ready, err := tx.Keys(TaskProjection, "ready/"+string(in.Project)+"/"); err != nil {
			return nil, nil, err
		} else if len(ready) >= MaxReadyTasks {
			return nil, nil, refusal(vocab.ReasonReadyBacklogFull, "project %s holds %d Ready tasks, at most %d", in.Project, len(ready), MaxReadyTasks)
		}
		for _, a := range in.Attachments {
			at, err := h.Uploads.Publish(c.Actor, a.Digest, tx.Now())
			if errors.Is(err, blobstore.ErrNoUpload) {
				err = fmt.Errorf("authority: %w: attachment %s is no live upload of yours", eventstore.ErrStaleExpectedState, a.Name)
			}
			if err != nil {
				return nil, nil, err
			}
			published[a.Digest] = at
		}
		dg, err := eventstore.ContentDigest(c.Input)
		if err != nil {
			return nil, nil, err
		}
		t := types.Task{ID: id, Project: in.Project, Spec: in, SpecDigest: dg, Acceptance: acceptanceOf(in),
			CreatedBy: by.ID, CreatedAt: types.Time(tx.Now().Format(time.RFC3339Nano))}
		evs, err := event(TaskCreated, id, &in.Project, t)
		apr, err2 := ids.New("apr")
		var approval any
		if by.Kind == types.ActorKindOperator {
			more, err3 := ApprovalEvent(TaskApprovalOf(t, c.Input, types.ApprovalID(apr), t.CreatedAt))
			evs, approval, err = append(evs, more...), apr, errors.Join(err, err3)
		}
		res, err4 := json.Marshal(map[string]any{"task": id, "state": TaskReady, "spec_digest": dg, "approval": approval})
		return evs, res, errors.Join(err, err2, err4)
	})
	drop := func(res eventstore.Committed) error {
		if res.Disposition != eventstore.DispositionApplied || res.Replayed {
			return nil
		}
		var err error
		for digest, at := range published {
			err = errors.Join(err, h.Uploads.Drop(c.Actor, digest, at))
		}
		return err
	}
	return decide, drop
}

// acceptanceOf is the spec's attachments as acceptance blobs: restricted,
// withheld from builder and reviewer.
func acceptanceOf(in types.TaskCreateScope) []types.AcceptanceBlob {
	out := make([]types.AcceptanceBlob, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		out = append(out, types.AcceptanceBlob{Name: a.Name, Digest: a.Digest, Class: types.AcceptanceBlobClassRestricted,
			WithheldFrom: []types.AcceptanceBlobWithheldFromItem{types.AcceptanceBlobWithheldFromItemBuilder, types.AcceptanceBlobWithheldFromItemReviewer}})
	}
	return out
}

// TaskApprovalOf is the approval an operator's create of t records: of
// the task at version 0 and its spec digest, the spec as its scope.
func TaskApprovalOf(t types.Task, spec json.RawMessage, id types.ApprovalID, at types.Time) types.Approval {
	zero, dg, project := int64(0), t.SpecDigest, t.Project
	return types.Approval{ID: id, Actor: t.CreatedBy, ActorKind: types.ApprovalActorKindOperator, Action: TaskCreateAction,
		ChangeClass: []types.ChangeClass{}, Project: &project, Object: types.ObjectRef{Type: TaskObjectType, ID: t.ID, Version: &zero, Digest: &dg},
		ExpectedState: types.ExpectedState{Version: &zero}, Scope: spec,
		Confirmation: types.ApprovalConfirmation{Policy: types.ApprovalConfirmationPolicyNone, SatisfiedBy: []types.EventID{}},
		Answers:      []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: taskApprovalReason, At: at}
}

// TaskProjector admits task.created events and revokes a revoked actor's
// task approvals; it runs after the projects projector. A task's approval
// is folded by recordApproval, which hands it to recordTaskApproval.
type TaskProjector struct{}

// Project is the eventstore.Hooks.Project fold for the tasks.
func (TaskProjector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		var err error
		switch ev.Kind {
		case TaskCreated:
			err = addTask(tx, ev)
		case TaskRailRaised:
			err = railRaised(tx, ev)
		case identity.ActorRevoked + "/v1":
			var a types.IdentityEvent
			if err = json.Unmarshal(ev.Payload, &a); err == nil && a.Actor != nil {
				err = revokeApprovals(tx, a.Actor.ID, types.Time(ev.At))
			}
		}
		if err != nil {
			return fmt.Errorf("authority: event %s: task: %w", ev.ID, err)
		}
	}
	return nil
}

// addTask admits a new task in a registered project with room in its
// backlog, by a live operator or supervisor, at its event, its spec
// digest its spec's and its acceptance classed as its spec's attachments.
func addTask(tx *eventstore.Tx, ev eventstore.Event) error {
	var t types.Task
	var raw struct{ Spec json.RawMessage }
	if err := errors.Join(json.Unmarshal(ev.Payload, &t), json.Unmarshal(ev.Payload, &raw)); err != nil {
		return err
	}
	dg, err := eventstore.ContentDigest(raw.Spec)
	if err != nil || ev.Subject != t.ID || ev.Project == nil || *ev.Project != t.Project || t.Spec.Project != t.Project ||
		string(t.CreatedBy) != ev.Actor || t.CreatedAt != types.Time(ev.At) || ev.Command == nil {
		return errors.Join(err, errors.New("a task's event is under its id, in its spec's project, by its creator, at its time"))
	}
	if dg != t.SpecDigest || !classed(t) {
		return errors.New("the spec digest or the acceptance classing is not the spec's")
	}
	by, err := liveActor(tx, t.CreatedBy)
	if err != nil || by.Kind != types.ActorKindOperator && by.Kind != types.ActorKindSupervisor {
		return errors.Join(err, errors.New("a live operator or supervisor creates a task"))
	}
	_, project, err1 := tx.Get(ProjectProjection, t.Project)
	_, exists, err2 := tx.Get(TaskProjection, t.ID)
	ready, err3 := tx.Keys(TaskProjection, "ready/"+string(t.Project)+"/")
	if err := errors.Join(err1, err2, err3); err != nil || !project || exists || len(ready) >= MaxReadyTasks {
		return errors.Join(err, errors.New("the project is unregistered, the task exists or the backlog is full"))
	}
	if err := putJSON(tx, TaskProjection, t.ID, TaskRecord{Task: t, State: TaskReady, Command: *ev.Command}); err != nil {
		return err
	}
	return putJSON(tx, TaskProjection, "ready/"+string(t.Project)+"/"+t.ID, t.ID)
}

// classed: t's acceptance is its spec's attachments, each restricted and
// withheld from builder and reviewer.
func classed(t types.Task) bool {
	ok := len(t.Acceptance) == len(t.Spec.Attachments)
	for i, x := range t.Acceptance {
		ok = ok && x.Name == t.Spec.Attachments[i].Name && x.Digest == t.Spec.Attachments[i].Digest && x.Class == types.AcceptanceBlobClassRestricted &&
			slices.Contains(x.WithheldFrom, types.AcceptanceBlobWithheldFromItemBuilder) && slices.Contains(x.WithheldFrom, types.AcceptanceBlobWithheldFromItemReviewer)
	}
	return ok
}

// recordTaskApproval admits a, an approval with no proposal, only as the
// approval of the task's creating operator in its creating command, under
// a fresh approval id, binding the task's spec digest. It checks the
// binding fields only, so a stored approval replays whatever else a later
// TaskApprovalOf would write.
func recordTaskApproval(tx *eventstore.Tx, ev eventstore.Event, a types.Approval) error {
	t, ok, err := loadJSON[TaskRecord](tx, TaskProjection, a.Object.ID)
	if err != nil || !ok || strings.Contains(a.Object.ID, "/") {
		return errors.Join(err, fmt.Errorf("an approval with no proposal approves a task's spec; %s is none", a.Object.ID))
	}
	id := "approval_id/" + string(a.ID) // shared with proposal approvals; approvalOf finds no proposal under it
	if _, dup, err := tx.Get(ProposalProjection, id); err != nil || dup {
		return errors.Join(err, fmt.Errorf("approval %s is already recorded", a.ID))
	}
	by, err := liveActor(tx, a.Actor)
	if err != nil || by.Kind != types.ActorKindOperator || a.Actor != t.CreatedBy || t.Approval != nil || ev.Command == nil ||
		*ev.Command != t.Command || a.At != types.Time(ev.At) || ev.Project == nil || *ev.Project != string(t.Project) {
		return errors.Join(err, errors.New("a task's approval is its operator's, once, in its creating command"))
	}
	dg, err := eventstore.ContentDigest(a.Scope)
	o, x := a.Object, a.ExpectedState
	if err != nil || o.Type != TaskObjectType || o.ID != t.ID || o.Version == nil || *o.Version != 0 || o.Digest == nil ||
		*o.Digest != t.SpecDigest || dg != t.SpecDigest || a.Action != TaskCreateAction || a.ActorKind != types.ApprovalActorKindOperator ||
		a.Project == nil || *a.Project != t.Project || x.Version == nil || *x.Version != 0 || a.Grant != nil ||
		a.Confirmation.Policy != types.ApprovalConfirmationPolicyNone {
		return errors.Join(err, fmt.Errorf("the approval does not bind task %s's spec digest %s", t.ID, t.SpecDigest))
	}
	if err := putJSON(tx, ProposalProjection, id, t.ID); err != nil {
		return err
	}
	t.Approval = &TaskApproval{ID: a.ID, Actor: a.Actor, Digest: t.SpecDigest, At: a.At, Status: ApprovalInForce}
	if err := putJSON(tx, TaskProjection, t.ID, t); err != nil {
		return err
	}
	return putJSON(tx, TaskProjection, "approver/"+string(a.Actor)+"/"+t.ID, t.ID)
}

// revokeApprovals marks revoked, at at, every task approval in force by
// actor.
func revokeApprovals(tx *eventstore.Tx, actor types.ActorID, at types.Time) error {
	prefix := "approver/" + string(actor) + "/"
	keys, err := tx.Keys(TaskProjection, prefix)
	for _, k := range keys {
		t, ok, lerr := loadJSON[TaskRecord](tx, TaskProjection, strings.TrimPrefix(k, prefix))
		if err = errors.Join(err, lerr); err != nil || !ok || t.Approval == nil {
			return errors.Join(err, fmt.Errorf("the approval index names no approved task: %s", k))
		}
		t.Approval.Status, t.Approval.RevokedAt = ApprovalRevoked, &at
		if err := errors.Join(putJSON(tx, TaskProjection, t.ID, t), tx.Delete(TaskProjection, k)); err != nil {
			return err
		}
	}
	return err
}
