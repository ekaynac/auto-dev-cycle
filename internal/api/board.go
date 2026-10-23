// SPDX-License-Identifier: Apache-2.0

package api

// The read-only board (docs/plan/01 §2.2 "Board and validated pull", D51;
// M1 P4b): one card per task, a projection of the task and graph records
// in the read's snapshot. Nobody writes it, and nothing on it is stored.
//
// Decisions:
//   - A Ready task's column is its approval's: ready while its spec's
//     approval is in force, else awaiting_approval (none, or revoked). A
//     cancelled task (authority.TaskCancelledState) is done, with the
//     cancelled badge; with nodes it is unplaced, as it was never admitted.
//   - An admitted task's (authority.TaskAdmitted, task.admitted's) column
//     is its current (unsuperseded) nodes': running while a plan or build
//     node is claimed or held; else verifying while an accept, gate or
//     review node is; else done_candidate when a review node of the task's
//     last build round completed go (graph.CandidateGo, the loop's rule for
//     its task.done too; merge is inactive in M1(a));
//     else ready, with the admitted badge. A held node keeps its stage's
//     column and carries its hold badges. A done task (task.done) is
//     done_candidate as a candidate, or done with the abandoned badge when
//     abandoned (its approval revoked), with its nodes and no admitted badge.
//     While it would end (a decided candidate, or abandoned) but a unit of
//     it still runs, its card says so (ending_blocked: authority.EndingOf's
//     refusal, the loop's own rule, with the controller's phrase; a visible
//     state, not a hold, as a completed node carries none). While it is
//     admitted and nothing moves it (no node claimed or held, no action of
//     the loop applying), its card says why (stalled: reconcile.StallOf,
//     with the controller's phrase; #226 entry 184).
//   - A node card's round is its candidate's (graph.CandidateRound): a
//     repair's accept, gate and review, recorded in round 1, show their
//     build's round (Opus #408 L1).
//   - A column is never guessed. A task whose records do not determine one
//     (an unknown state, a node kind outside M1(a)'s template, nodes of a
//     task not admitted or not recorded, a damaged record, ...) is listed
//     unplaced, with no column and a coarse reason; the other cards stand
//     and the read succeeds.
//   - Scope first: a task is derived only when the caller reads the board
//     in its project (its task record's, else the one its nodes pinned,
//     else none: installation-wide), so a record the caller cannot read
//     never shapes what it is sent. Reads authorizes each card again.
//   - A board read covers, by design, the task and hold data of the
//     projects it reads: a card's id, project, title, approval and
//     admission state and creation time, and per hold its id, hold, signal
//     instance, owner, since and age. owner_ref (a concrete actor or owner)
//     is sent only when the caller also reads holds in that project. Never
//     sent: the spec's requirement, its attachments' names or digests, the
//     spec digest (a digest over them, as the proposals read withholds its
//     binding digest), and a hold's reason, escalation and pages.
//   - Lease holds stay on GET /v1/holds: no lease is bound to a node
//     before P8a.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
)

// boardDomain and holdDomain are the read kinds a board read checks.
const (
	boardDomain types.Name = "board"
	holdDomain  types.Name = "hold"
)

// stages is the column each node kind of M1(a)'s template moves its
// task's card to while claimed or held ("": none, a controller node).
var stages = map[types.NodeKind]types.BoardPlacedCardColumn{
	types.NodeKindPlan: types.BoardPlacedCardColumnRunning, types.NodeKindBuild: types.BoardPlacedCardColumnRunning,
	types.NodeKindAccept: types.BoardPlacedCardColumnVerifying, types.NodeKindGate: types.BoardPlacedCardColumnVerifying,
	types.NodeKindReview: types.BoardPlacedCardColumnVerifying, types.NodeKindReviewRequirement: "",
}

// board is the card of every task the caller reads the board of, in task
// id order, then the unplaced tasks of no record, in task id order.
var board = Resource{
	Domain: boardDomain,
	Schema: "api/board.json",
	List: func(tx *eventstore.Tx, at ReadAt) ([]any, error) {
		ids, err := authority.TaskIDs(tx)
		if err != nil {
			return nil, err
		}
		nodes, err := graph.Nodes(tx)
		if err != nil {
			return nil, err
		}
		byTask := map[types.TaskID][]graph.NodeView{}
		for _, n := range nodes {
			byTask[n.Task] = append(byTask[n.Task], n)
		}
		out := []any{}
		// The recorded tasks, then the nodes' tasks of no record.
		for i, id := range append(ids, slices.Sorted(maps.Keys(byTask))...) {
			ns, ok := byTask[id]
			if i >= len(ids) && !ok {
				continue // a recorded task's nodes, listed with it
			}
			delete(byTask, id)
			t, recorded, project, err := boardTask(tx, id)
			switch {
			case err != nil:
				return nil, err
			case !at.Scope.ReadsRecord(boardDomain, project): // never derived
			case i >= len(ids):
				out = append(out, unplaced(id, project, types.BoardUnplacedCardReasonTaskUnrecorded))
			case !recorded:
				out = append(out, unplaced(id, project, types.BoardUnplacedCardReasonDamagedRecord))
			default:
				if ending, err := endingOf(tx, t); err != nil { // one task's unreadable record, never the board's (Mythos chunk 5a L4)
					out = append(out, unplaced(id, project, types.BoardUnplacedCardReasonDamagedRecord))
				} else {
					out = append(out, boardCard(t, ns, at.Now, at.Scope.ReadsRecord(holdDomain, project), ending, stalledOf(at, t)))
				}
			}
		}
		return out, nil
	},
	Project: func(tx *eventstore.Tx, rec any) (*types.ProjectID, error) {
		var id types.TaskID
		card, _ := rec.(types.BoardCard)
		switch c := card.Variant.(type) {
		case *types.BoardPlacedCard:
			id = c.ID
		case *types.BoardUnplacedCard:
			id = c.ID
		default:
			return nil, fmt.Errorf("api: a board record of type %T", c)
		}
		_, _, project, err := boardTask(tx, id)
		return project, err
	},
	Fields: []string{"type", "id", "project", "title", "column", "admitted", "cancelled", "abandoned", "approval", "created_at", "nodes", "ending_blocked", "stalled", "reason"},
}

// boardTask is task id's record (recorded: it exists and reads as one)
// and its project: its record's; for a record that does not read as one,
// the registered project its project field alone names, else the one its
// nodes pinned; else nil.
func boardTask(tx *eventstore.Tx, id types.TaskID) (t authority.TaskRecord, recorded bool, project *types.ProjectID, err error) {
	raw, ok, err := tx.Get(authority.TaskProjection, id)
	if err != nil {
		return t, false, nil, err
	}
	if ok && json.Unmarshal(raw, &t) == nil && t.ID == id {
		return t, true, &t.Project, nil
	}
	var named struct {
		Project types.ProjectID `json:"project"`
	}
	if ok && json.Unmarshal(raw, &named) == nil && named.Project != "" {
		_, registered, err := tx.Get(authority.ProjectProjection, named.Project)
		if err != nil || registered {
			return t, false, &named.Project, err
		}
	}
	p, pinned, err := graph.TaskProject(tx, id)
	if err != nil || !pinned {
		return t, false, nil, err
	}
	return t, false, &p, nil
}

func unplaced(id types.TaskID, project *types.ProjectID, reason types.BoardUnplacedCardReason) types.BoardCard {
	return types.BoardCard{Variant: &types.BoardUnplacedCard{Type: "unplaced", ID: id, Project: project, Reason: reason}}
}

// boardCard is t's card over its nodes (node id order), its hold badges
// aged at now, with their owner_ref only when ownerRefs, and ending, its
// ending_blocked (endingOf), and stalled, its stall (stalledOf); or t
// unplaced when its records do not determine its column.
func boardCard(t authority.TaskRecord, nodes []graph.NodeView, now time.Time, ownerRefs bool, ending *types.BoardEndingBlocked, stalled *types.BoardStalled) types.BoardCard {
	project := t.Project
	c := &types.BoardPlacedCard{Type: "placed", ID: t.ID, Project: t.Project, Title: t.Spec.Title, Admitted: t.State == authority.TaskAdmitted,
		Approval: types.BoardPlacedCardApprovalNone, CreatedAt: t.CreatedAt, Nodes: []types.BoardNodeCard{}, EndingBlocked: ending, Stalled: stalled}
	if a := t.Approval; a != nil {
		if a.Status != authority.ApprovalInForce && a.Status != authority.ApprovalRevoked {
			return unplaced(t.ID, &project, types.BoardUnplacedCardReasonUnknownState)
		}
		c.Approval = types.BoardPlacedCardApproval(a.Status)
	}
	switch {
	case (t.State == authority.TaskReady || t.State == authority.TaskCancelledState) && len(nodes) > 0:
		return unplaced(t.ID, &project, types.BoardUnplacedCardReasonNodesBeforeAdmission)
	case t.State == authority.TaskCancelledState:
		c.Column, c.Cancelled = types.BoardPlacedCardColumnDone, true
		return types.BoardCard{Variant: c}
	case t.State == authority.TaskReady && c.Approval == types.BoardPlacedCardApprovalInForce:
		c.Column = types.BoardPlacedCardColumnReady
		return types.BoardCard{Variant: c}
	case t.State == authority.TaskReady:
		c.Column = types.BoardPlacedCardColumnAwaitingApproval
		return types.BoardCard{Variant: c}
	case !c.Admitted && t.State != authority.TaskDoneState:
		return unplaced(t.ID, &project, types.BoardUnplacedCardReasonUnknownState)
	}
	active := map[types.BoardPlacedCardColumn]bool{}
	for _, n := range nodes {
		stage, known := stages[n.Node.Kind]
		switch {
		case !known:
			return unplaced(t.ID, &project, types.BoardUnplacedCardReasonDeferredNodeKind)
		case n.Project != t.Project:
			return unplaced(t.ID, &project, types.BoardUnplacedCardReasonNodeInAnotherProject)
		case n.State.Result != nil && n.Outcome == nil:
			return unplaced(t.ID, &project, types.BoardUnplacedCardReasonDamagedRecord)
		}
		card, reason := nodeCard(n, now, ownerRefs)
		if r := graph.CandidateRound(nodes, n.Node); r > 0 { // a stage's round is its candidate's (Opus #408 L1)
			card.Round = r
		}
		if reason != "" {
			return unplaced(t.ID, &project, reason)
		}
		c.Nodes = append(c.Nodes, card)
		if s := n.State.State; s == types.NodeStateStateClaimed || s == types.NodeStateStateHeld {
			active[stage] = true
		}
	}
	switch {
	case t.State == authority.TaskDoneState && t.DoneAs == string(types.TaskDoneAsAbandoned):
		c.Column, c.Abandoned = types.BoardPlacedCardColumnDone, true
		return types.BoardCard{Variant: c}
	case t.State == authority.TaskDoneState:
		c.Column = types.BoardPlacedCardColumnDoneCandidate
		return types.BoardCard{Variant: c}
	}
	active[types.BoardPlacedCardColumnDoneCandidate] = graph.CandidateGo(nodes)
	c.Column = types.BoardPlacedCardColumnReady
	for _, col := range []types.BoardPlacedCardColumn{types.BoardPlacedCardColumnRunning, types.BoardPlacedCardColumnVerifying, types.BoardPlacedCardColumnDoneCandidate} {
		if active[col] {
			c.Column = col
			break
		}
	}
	return types.BoardCard{Variant: c}
}

// stalledOf is t's stalled: its cause, when t is the admitted task the
// controller's loop found stalled at its last pass (at.Stalled; the one
// rule is reconcile.StallOf): admitted, nothing of it claimed or held,
// and no action of the loop moving it (#226 entry 184). A visible state,
// not a hold; nil otherwise.
func stalledOf(at ReadAt, t authority.TaskRecord) *types.BoardStalled {
	if at.Stalled == nil || t.State != authority.TaskAdmitted {
		return nil
	}
	cause, ok := at.Stalled(t.ID)
	if !ok {
		return nil
	}
	return &types.BoardStalled{Cause: types.BoardStalledCause(cause)}
}

// endingOf is t's ending_blocked: its end, as the loop would record it
// (authority.EndingOf: abandoned once its approval is not in force, or a
// decided candidate), refused because a unit of it still runs
// (authority.ErrEnding), with that refusal's phrase, never its text (#226
// entry 166); nil otherwise. Its error is a record it could not read: the
// caller lists that task unplaced (damaged_record), the board's own rule.
func endingOf(tx *eventstore.Tx, t authority.TaskRecord) (*types.BoardEndingBlocked, error) {
	var e authority.ErrEnding
	switch _, ok, err := authority.EndingOf(tx, t.ID); {
	case !ok:
		return nil, err
	case errors.As(err, &e):
		return &types.BoardEndingBlocked{Unit: e.Unit, Cause: types.BoardEndingBlockedCause(e.Cause())}, nil
	case err == nil, errors.Is(err, authority.ErrNotEndable):
		return nil, nil
	default:
		return nil, err
	}
}

// nodeCard is n's card: a badge per hold (hold, signal, owner, age at now)
// and per structural wait (the input, and its producer or owed input); or
// the reason its records do not give one.
func nodeCard(n graph.NodeView, now time.Time, ownerRefs bool) (types.BoardNodeCard, types.BoardUnplacedCardReason) {
	c := types.BoardNodeCard{ID: n.Node.ID, Kind: types.BoardNodeCardKind(n.Node.Kind), Round: n.Node.Round,
		State: types.BoardNodeCardState(n.State.State), Holds: []types.BoardHoldBadge{}, Waiting: []types.BoardWaitBadge{}}
	for _, h := range n.State.Holds {
		since, err := time.Parse(time.RFC3339Nano, string(h.Since))
		if err != nil {
			return c, types.BoardUnplacedCardReasonDamagedRecord
		}
		b := types.BoardHoldBadge{ID: h.ID, Hold: h.Hold, SignalInstance: h.SignalInstance, Owner: h.Owner, Since: h.Since,
			AgeSeconds: max(0, int64(now.Sub(since)/time.Second))}
		if ownerRefs {
			b.OwnerRef = h.OwnerRef
		}
		c.Holds = append(c.Holds, b)
	}
	for _, name := range n.State.WaitingOn {
		w := types.BoardWaitBadge{Input: name}
		for _, in := range n.Node.Inputs {
			switch src := in.Source.Variant.(type) {
			case *types.ResultSource:
				if in.Name == name {
					w.Producer = &src.Producer
				}
			case *types.AnswerSource:
				if in.Name == name {
					w.OwedInput = &src.OwedInput
				}
			}
		}
		if w.Producer == nil && w.OwedInput == nil {
			return c, types.BoardUnplacedCardReasonUndeclaredWait
		}
		c.Waiting = append(c.Waiting, w)
	}
	return c, ""
}
