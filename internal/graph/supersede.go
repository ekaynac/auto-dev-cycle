// SPDX-License-Identifier: Apache-2.0

package graph

// Supersession: a roster change replaces a review (13 "Replacement is a
// recorded supersession"; plan 15 G6b-1).
//
// Decisions:
//   - One writer transaction: attempt.stop_requested{preempt} for each of the
//     predecessor's running attempts, then review.superseded. An attempt in
//     any other state keeps it and settles normally. The fold refuses a
//     listed attempt that this same command did not stop for preemption, or
//     that another supersession listed (graph_preempted).
//   - The replacement is the event's own node, not a successor: it keeps
//     the predecessor's round, inputs, requirements and effect, with the
//     roster of the subject's next requirement revision, to which its
//     review_requirement input moves. It takes the predecessor's successor
//     slot (graph_successors), so the lineage does not fork.
//   - The predecessor is superseded: it records, selects, claims and
//     produces nothing from then on, and a completed one keeps its result
//     as history. A second supersession of it is ErrSuperseded.
//   - A subject has one selected requirement revision per task: its first
//     selection sets it, and only a supersession moves it, to the next
//     revision. A review is claimed only on its subject's current revision.
//   - Lineage follows supersession: a replacement's lineage includes what
//     the review it replaced asked, so the replacement's successor answers
//     it (lineageOwed).
//   - Holds (G6b-2): each of the predecessor's holds transfers to the
//     replacement, unchanged (id, since, owner, pages), or is retired with
//     its reason, in the supersession event itself. Which one is derived,
//     never chosen (holdsFor): a retry wait is of the predecessor's key, so
//     it retires as invalidated; a reviewer_unavailable wait on an executor
//     the new roster drops retires as member_removed; an awaiting_input
//     wait on an answer the replacement no longer declares retires as
//     question_moved; every other wait transfers. The fold derives the same
//     lists and refuses any other.
//   - A moved question (G6b-2): the caller may replace an answer input by
//     an answer to another owed input, or remove it (Moves), once its
//     question moved: the builder requires that the lineage no longer asks
//     the old owed input and asks the new one at its current revision. The
//     fold checks the shape only (an answer input replaced, under its name,
//     by one to an owed input the review did not declare, or removed); as
//     for a hold retired as revised, it cannot check the move until M1
//     records owed inputs.
//   - M1 binds attempts to nodes. Until then the caller names the
//     predecessor's attempts: the fold proves neither that the list is
//     complete nor that the listed attempts are the predecessor's. An
//     unlisted attempt is safe (nothing selects its result for either node)
//     but holds its capacity until it ends.
//   - A review whose result a dependant claimed is not superseded. G6b-2
//     keeps this refusal: re-pointing a claimed dependant would release its
//     claim, which no event does yet; the merge's wait for the replacement
//     is G7b-2's.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The supersession event and the requirement revisions' projection.
const (
	ReviewSuperseded      = "review.superseded/v1"
	RequirementProjection = "graph_requirements" // task/subject → its selected requirement revision
	PreemptedProjection   = "graph_preempted"    // attempt id → the review whose supersession stopped it
)

// ErrSuperseded: the node was superseded by another; it is no longer
// claimed, committed, succeeded or superseded.
type ErrSuperseded struct{ Node, By types.NodeID }

func (e ErrSuperseded) Error() string {
	return "graph: " + string(e.Node) + " is superseded by " + string(e.By)
}

// Reason is the refusal's reason code.
func (ErrSuperseded) Reason() vocab.Reason { return vocab.ReasonStaleExpectedState }

// superseded is rec's ErrSuperseded, nil for a node not superseded.
func superseded(rec nodeRecord) error {
	if rec.State.State != types.NodeStateStateSuperseded {
		return nil
	}
	return ErrSuperseded{rec.Recorded.Node.ID, *rec.State.SupersededBy}
}

// revision is a subject's selected requirement revision in a task, with
// its requirement's params digest.
type revision struct {
	Revision int64        `json:"revision"`
	Params   types.Digest `json:"params"`
}

// selectRevision makes req, the params of a review_requirement node of
// task, its subject's selected revision: the first, the current one again,
// or, by a supersession (next), the next one.
func selectRevision(tx *eventstore.Tx, task types.TaskID, req json.RawMessage, next bool) error {
	var r types.ReviewRequirement
	d, err := ParamsDigest(req)
	if err = errors.Join(err, json.Unmarshal(req, &r)); err != nil {
		return err
	}
	index := string(task) + "/" + string(r.Subject)
	cur, ok, err := load[revision](tx, RequirementProjection, index)
	switch {
	case err != nil:
		return err
	case ok && cur.Revision == r.Revision && cur.Params != d:
		return fmt.Errorf("revision %d of %s is another requirement", r.Revision, r.Subject)
	case ok && cur.Revision != r.Revision && (!next || r.Revision != cur.Revision+1):
		return fmt.Errorf("%s is at requirement revision %d: only a supersession moves it", r.Subject, cur.Revision)
	}
	return put(tx, RequirementProjection, index, revision{r.Revision, d})
}

// currentRevision: req, the params of the requirement node, is of task and
// its subject's selected revision there.
func currentRevision(tx *eventstore.Tx, task types.TaskID, node types.NodeID, req types.ReviewRequirement) error {
	n, err := loadNode(tx, node)
	cur, ok, cerr := load[revision](tx, RequirementProjection, string(task)+"/"+string(req.Subject))
	if err = errors.Join(err, cerr); err != nil || n.Recorded.Task != task || !ok || cur.Revision != req.Revision {
		return errors.Join(err, fmt.Errorf("requirement revision %d is not the selected revision of %s in the task", req.Revision, req.Subject))
	}
	return nil
}

// Moves are the answer inputs, by name, whose question moved to another
// owed input: the replacement answers the new one in their place, or drops
// them (nil).
type Moves map[types.Name]*types.AnswerSource

// Supersede is the one transaction replacing review pred after a roster
// change: a stop for each of attempts still running, then review.superseded
// with the replacement, whose roster is that of requirement, the
// review_requirement node of the subject's next revision, recorded before,
// and with pred's holds transferred or retired. The replacement's roster is
// checked against families, as RecordReview checks.
func Supersede(tx *eventstore.Tx, pred, requirement types.NodeID, attempts []types.AttemptID, families Families) ([]eventstore.Event, error) {
	return SupersedeMoving(tx, pred, requirement, attempts, families, nil, nil)
}

// SupersedeMoving is Supersede, with the answer inputs of moves replaced or
// removed; auth tells which questions moved.
func SupersedeMoving(tx *eventstore.Tx, pred, requirement types.NodeID, attempts []types.AttemptID, families Families, moves Moves, auth Authority) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, pred)
	req, rerr := loadNode(tx, requirement)
	id, ierr := ids.New("nod")
	if err = errors.Join(err, rerr, ierr); err != nil {
		return nil, err
	}
	if err := superseded(rec); err != nil && !mutation.Is(mutation.SupersedeTwice) {
		return nil, err
	}
	p, err := req.Recorded.Node.ParamsAsReviewRequirement()
	if err != nil || req.Recorded.Node.Kind != types.NodeKindReviewRequirement {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no review requirement", requirement))
	}
	n := rec.Recorded.Node
	n.ID = types.NodeID(id)
	if n.Params, err = json.Marshal(types.ReviewNodeParams{Subject: p.Subject, Roster: p.Roster}); err != nil {
		return nil, err
	}
	if n.Inputs, err = moved(tx, rec, requirement, moves, auth); err != nil {
		return nil, err
	}
	if err := registered(n, families); err != nil {
		return nil, err
	}
	held, retired, err := holdsFor(tx, rec, n)
	if err != nil {
		return nil, err
	}
	var evs []eventstore.Event
	stopped := []types.AttemptID{}
	for _, a := range slices.Compact(slices.Sorted(slices.Values(attempts))) {
		stop, err := lease.StopRunning(tx, a, types.AttemptStopReasonValuePreempt)
		if err != nil {
			return nil, err
		}
		if len(stop) > 0 && !mutation.Is(mutation.SupersessionKeepsAttempts) {
			evs, stopped = append(evs, stop...), append(stopped, a)
		}
	}
	s, err := event(ReviewSuperseded, string(pred), rec.Recorded.Project, &types.ReviewSuperseded{Type: "review_superseded", Predecessor: pred,
		Replacement: n, StoppedAttempts: stopped, TransferredHolds: holdIDs(held), RetiredHolds: retired})
	return append(evs, s...), err
}

// moved are rec's inputs for its replacement: its review_requirement input
// moved to requirement, and each answer input moves names replaced or
// removed. A move needs auth to show the question moved: rec's lineage no
// longer asks the old owed input, and asks the new one at its current
// revision.
func moved(tx *eventstore.Tx, rec nodeRecord, requirement types.NodeID, moves Moves, auth Authority) ([]types.InputSpec, error) {
	var owed []types.AnswerSource
	if len(moves) > 0 {
		if auth == nil {
			return nil, errors.New("graph: a moved question needs the authority")
		}
		var err error
		if owed, err = lineageOwed(tx, rec.Recorded.Predecessor, auth); err != nil {
			return nil, err
		}
	}
	out, used := []types.InputSpec{}, 0
	for _, in := range rec.Recorded.Node.Inputs {
		to, move := moves[in.Name]
		ask, answer := in.Source.Variant.(*types.AnswerSource)
		src, result := in.Source.Variant.(*types.ResultSource)
		switch {
		case result && src.ResultKind == types.ResultKindReviewRequirement:
			next := *src
			next.Producer = requirement
			in.Source.Variant = &next
		case !move || mutation.Is(mutation.ReplacementKeepsMovedAnswer):
		case !answer || asks(owed, ask.OwedInput):
			return nil, fmt.Errorf("graph: %s's question has not moved: its answer is not replaced", in.Name)
		case to == nil:
			used++
			continue
		case !owedIn(owed, to): // the old owed input is no longer asked, so it is never to
			return nil, ErrNotOwed{to.OwedInput}
		default:
			in.Source.Variant = &types.AnswerSource{Type: "answer", OwedInput: to.OwedInput, Revision: to.Revision}
		}
		if move {
			used++
		}
		out = append(out, in)
	}
	if used != len(moves) && !mutation.Is(mutation.ReplacementKeepsMovedAnswer) {
		return nil, errors.New("graph: a move names no answer input of the review")
	}
	return out, nil
}

// holdsFor splits rec's holds for its replacement n, in rec's order: those
// n still waits on transfer, unchanged; the others are retired, each with
// its reason.
func holdsFor(tx *eventstore.Tx, rec nodeRecord, n types.Node) ([]types.NodeHold, []types.RetiredHold, error) {
	roster, _, err := rosterOf(n)
	if err != nil {
		return nil, nil, err
	}
	held, retired := []types.NodeHold{}, []types.RetiredHold{}
	for _, h := range rec.State.Holds {
		by, _, err := load[heldBy](tx, HoldProjection, string(h.ID))
		if err != nil {
			return nil, nil, err
		}
		exe, onExecutor := strings.CutPrefix(h.SignalInstance, "registry:")
		var why types.RetiredHoldReason
		switch {
		case mutation.Is(mutation.SupersessionTransfersEveryHold):
		case by.Result != nil:
			why = types.RetiredHoldReasonInvalidated
		case h.Hold == "reviewer_unavailable" && onExecutor && !slices.ContainsFunc(roster, func(m types.RosterMember) bool { return string(m.Executor) == exe }):
			why = types.RetiredHoldReasonMemberRemoved
		case h.Hold == "awaiting_input" && !slices.ContainsFunc(n.Inputs, func(in types.InputSpec) bool {
			ask, ok := in.Source.Variant.(*types.AnswerSource)
			return ok && awaitSignal(ask) == h.SignalInstance
		}):
			why = types.RetiredHoldReasonQuestionMoved
		}
		if why == "" {
			held = append(held, h)
		} else {
			retired = append(retired, types.RetiredHold{Hold: h.ID, Reason: why})
		}
	}
	return held, retired, nil
}

func holdIDs(hs []types.NodeHold) []types.HoldID {
	out := []types.HoldID{}
	for _, h := range hs {
		out = append(out, h.ID)
	}
	return out
}

func supersededFold(tx *eventstore.Tx, ev eventstore.Event, p *types.ReviewSuperseded) error {
	rec, err := nodeOf(tx, ev, p.Predecessor)
	if err != nil || ev.Kind != ReviewSuperseded || ev.Subject != string(p.Predecessor) {
		return errors.Join(err, errors.New("a supersession is under its predecessor"))
	}
	if err := superseded(rec); err != nil && !mutation.Is(mutation.SupersedeTwice) {
		return err
	}
	if rec.Recorded.Node.Kind != types.NodeKindReview {
		return fmt.Errorf("a %s is not superseded: only a review", rec.Recorded.Node.Kind)
	}
	if err := errors.Join(unshared(tx, rec), unclaimed(tx, rec)); err != nil {
		return err
	}
	if _, exists, err := load[nodeRecord](tx, NodeProjection, string(p.Replacement.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the replacement is recorded"))
	}
	requirement, err := replaces(tx, ev, rec, p.Replacement)
	if err != nil {
		return err
	}
	if err := selectRevision(tx, rec.Recorded.Task, requirement.Recorded.Node.Params, true); err != nil {
		return err
	}
	if _, forked, err := load[types.NodeID](tx, SuccessorProjection, string(p.Predecessor)); err != nil || forked && !mutation.Is(mutation.SupersedeTwice) {
		return errors.Join(err, errors.New("the predecessor has a successor: supersede the lineage's last round"))
	}
	for _, a := range p.StoppedAttempts {
		at, by, ok, err := lease.AttemptOf(tx, a)
		_, listed, lerr := load[types.NodeID](tx, PreemptedProjection, string(a))
		if err = errors.Join(err, lerr); err != nil || !ok || listed || at.Status != types.AttemptStatusStopping || at.StopReason == nil ||
			*at.StopReason != types.AttemptStopReasonValuePreempt || by == nil || ev.Command == nil || *by != *ev.Command {
			return errors.Join(err, fmt.Errorf("attempt %s is not stopped for preemption by this command", a))
		}
		if err := put(tx, PreemptedProjection, string(a), p.Predecessor); err != nil {
			return err
		}
	}
	held, retired, err := holdsFor(tx, rec, p.Replacement)
	if err != nil || !slices.Equal(p.TransferredHolds, holdIDs(held)) || !slices.Equal(p.RetiredHolds, retired) {
		return errors.Join(err, errors.New("each of the review's holds transfers if its replacement still waits on it, or retires with its reason, once"))
	}
	for _, h := range held {
		by, _, err := load[heldBy](tx, HoldProjection, string(h.ID))
		if err = errors.Join(err, put(tx, HoldProjection, string(h.ID), heldBy{Node: p.Replacement.ID, Result: by.Result})); err != nil {
			return err
		}
	}
	if mutation.Is(mutation.SupersessionDropsHolds) {
		held = []types.NodeHold{}
	}
	st := types.NodeState{Node: p.Replacement.ID, WaitingOn: waitingOn(p.Replacement), Holds: held}
	settle(&st)
	n := nodeRecord{Recorded: types.NodeRecorded{Type: "node_recorded", Task: rec.Recorded.Task, Project: rec.Recorded.Project, Node: p.Replacement,
		Predecessor: rec.Recorded.Predecessor}, Replaces: &p.Predecessor, State: st}
	if err := errors.Join(put(tx, SuccessorProjection, string(p.Predecessor), p.Replacement.ID), putNode(tx, n)); err != nil {
		return err
	}
	if mutation.Is(mutation.SupersessionKeepsPredecessor) {
		return nil
	}
	rec.State.State, rec.State.WaitingOn, rec.State.Holds, rec.State.SupersededBy = types.NodeStateStateSuperseded, []types.Name{}, []types.NodeHold{}, &p.Replacement.ID
	return putNode(tx, rec)
}

// unshared: rec neither reuses another node's key nor holds a key that
// another node reuses, so no reuse binding outlives its supersession.
func unshared(tx *eventstore.Tx, rec nodeRecord) error {
	if rec.State.Key == nil {
		return nil
	}
	index := keyIndex(rec.Recorded.Task, rec.State.Key.Key)
	holder, _, err := load[types.NodeID](tx, KeyProjection, index)
	reusers, _, rerr := load[[]types.NodeID](tx, ReuseProjection, index)
	if err = errors.Join(err, rerr); err != nil || holder != rec.Recorded.Node.ID || len(reusers) > 0 {
		return errors.Join(err, errors.New("a review bound by reuse, or whose key is reused, is not superseded"))
	}
	return nil
}

// unclaimed: no other node's claim took rec's selected result.
func unclaimed(tx *eventstore.Tx, rec nodeRecord) error {
	if rec.State.Result == nil {
		return nil
	}
	nodes, err := tx.Keys(ClaimProjection, "")
	for _, n := range nodes {
		c, _, cerr := load[claim](tx, ClaimProjection, n)
		if err = errors.Join(err, cerr); err == nil && n != string(rec.Recorded.Node.ID) && slices.ContainsFunc(c.Inputs, func(in types.ClaimedInput) bool {
			return in.Result != nil && *in.Result == *rec.State.Result
		}) {
			return fmt.Errorf("%s claimed the review's result: re-pointing a dependant is G6b-2's and G7b-2's", n)
		}
	}
	return err
}

// replaces: n replaces rec, a review, after a roster change: the same kind,
// definition version, round, requirements, effect and subject, another
// roster, and the same inputs but for rec's one review_requirement input,
// which moves to the requirement returned (a node of rec's task at the next
// revision, with n's subject, roster and definition version), and for the
// answer inputs of moved questions, removed or replaced under their name by
// one answer to an owed input rec does not declare.
func replaces(tx *eventstore.Tx, ev eventstore.Event, rec nodeRecord, n types.Node) (nodeRecord, error) {
	p := rec.Recorded.Node
	roster, subject, err := rosterOf(p)
	nRoster, nSubject, nerr := rosterOf(n)
	if err = errors.Join(err, nerr); err != nil || n.Kind != p.Kind || n.DefinitionVersion != p.DefinitionVersion || n.Round != p.Round ||
		!reflect.DeepEqual(n.Requires, p.Requires) || !reflect.DeepEqual(n.Effect, p.Effect) || len(n.Inputs) > len(p.Inputs) ||
		nSubject != subject || slices.Equal(nRoster, roster) {
		return nodeRecord{}, errors.Join(err, errors.New("a replacement keeps its review's definition, round, requirements, effect, subject and inputs, with another roster"))
	}
	var from, to *types.ResultSource
	j := 0
	for i, in := range p.Inputs {
		var m types.InputSpec
		if j < len(n.Inputs) {
			m = n.Inputs[j]
		}
		a, ok := in.Source.Variant.(*types.ResultSource)
		b, ok2 := m.Source.Variant.(*types.ResultSource)
		_, answer := in.Source.Variant.(*types.AnswerSource)
		switch {
		case ok && ok2 && from == nil && in.Name == m.Name && a.ResultKind == types.ResultKindReviewRequirement && b.ResultKind == a.ResultKind:
			from, to = a, b
		case reflect.DeepEqual(in, m) || answeredAnew(p, n, in, m):
		case answer && in.Name != m.Name: // removed: its question moved
			continue
		default:
			return nodeRecord{}, fmt.Errorf("the replacement's input %d is not its review's", i)
		}
		j++
	}
	if j != len(n.Inputs) {
		return nodeRecord{}, errors.New("the replacement adds an input")
	}
	if from == nil {
		return nodeRecord{}, errors.New("a superseded review declares its review_requirement input")
	}
	old, err := loadNode(tx, from.Producer)
	next, nerr := nodeOf(tx, ev, to.Producer)
	if err = errors.Join(err, nerr); err != nil || next.Recorded.Task != rec.Recorded.Task || next.Recorded.Node.Kind != types.NodeKindReviewRequirement ||
		old.Recorded.Task != rec.Recorded.Task || old.Recorded.Node.Kind != types.NodeKindReviewRequirement {
		return nodeRecord{}, errors.Join(err, errors.New("the review's and the replacement's requirements are review_requirement nodes of its task"))
	}
	was, err := old.Recorded.Node.ParamsAsReviewRequirement()
	req, rerr := next.Recorded.Node.ParamsAsReviewRequirement()
	if err = errors.Join(err, rerr); err != nil || req.Revision != was.Revision+1 || req.Subject != subject || was.Subject != subject || !slices.Equal(req.Roster, nRoster) ||
		req.DefinitionVersion != n.DefinitionVersion {
		return nodeRecord{}, errors.Join(err, errors.New("the replacement's requirement is the next revision, of its subject, roster and definition version"))
	}
	return next, nil
}

// answeredAnew: m replaces p's answer input in, under its name, by n's one
// answer to an owed input p does not declare.
func answeredAnew(p, n types.Node, in, m types.InputSpec) bool {
	_, ok := in.Source.Variant.(*types.AnswerSource)
	b, ok2 := m.Source.Variant.(*types.AnswerSource)
	declares := func(x types.Node) int {
		return len(slices.DeleteFunc(slices.Clone(x.Inputs), func(i types.InputSpec) bool {
			c, ok := i.Source.Variant.(*types.AnswerSource)
			return !ok || c.OwedInput != b.OwedInput
		}))
	}
	return ok && ok2 && in.Name == m.Name && declares(p) == 0 && declares(n) == 1
}
