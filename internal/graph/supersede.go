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
//     it retires as invalidated; a reviewer_unavailable wait on
//     registry:<executor>, a member of the predecessor's roster that the
//     new roster drops, retires as member_removed; an awaiting_input wait
//     on an answer the predecessor declares and the replacement no longer
//     does retires as question_moved; every other wait transfers, matched
//     or not. The fold derives the same lists and refuses any other.
//   - A moved question (G6b-2): the caller may replace an answer input by
//     an answer to another owed input (Moves), once its question moved:
//     the builder requires that the lineage no longer asks the old owed
//     input and asks the new one at its current revision, which the review
//     declares no other answer to. The fold checks the shape (an answer
//     input replaced, under its name, by one to an owed input the review
//     did not declare) and, through the authority (M1 P6, eligible.go),
//     that the question moved. A removal (13:206) drops the answer input of
//     a question the lineage no longer asks, and retires its wait as
//     question_moved; only the fold that reads the authority admits it, so
//     M0's fold (ProjectM0) still refuses one.
//   - A replaced question is checked again before the replacement runs
//     (displaced): the evaluator leaves a node waiting, and Commit refuses
//     it (authority_revoked), while its lineage still asks an owed input
//     that a supersession in that lineage replaced and the node does not
//     answer. So a raw supersession that replaces an answer still owed
//     never runs without it. M0's claim fold reads no authority, so there
//     the claim is inert, as an answer claimed directly at a stale revision
//     is; the fold that reads it refuses the claim (M1 P6, eligible.go).
//   - Attempts are bound to nodes (M1 P8a, units.go), so the attempts
//     stopped are derived, never named. A node has one current attempt:
//     the supersession stops it if it runs, and lists exactly that one. The
//     fold derives the same list from the lease projections, after the
//     command's stops are folded, and refuses any other list, and a
//     supersession that leaves the predecessor's attempt running: an
//     unlisted running attempt would hold its capacity, and could publish,
//     after its node was replaced.
//   - A review whose result a dependant claimed is not superseded. G6b-2
//     keeps this refusal: re-pointing a claimed dependant would release its
//     claim, which no event does yet. A queue generation's carry_over is the
//     exception (G7b-2a): its merge waits for the replacement. Any
//     supersession is refused, by the builder and the fold, while a merge
//     of the task on the review's subject has a live operation
//     (revisionFenced, fence.go): an intended merge resumes without
//     re-validation.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

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
// its requirement's params digest and the node that last selected it (a
// generation's carry_over and merge take it, G7b-2a).
type revision struct {
	Revision int64        `json:"revision"`
	Params   types.Digest `json:"params"`
	Node     types.NodeID `json:"node,omitempty"`
}

// selectRevision makes req, the params of node, a review_requirement node
// of task, its subject's selected revision: the first, the current one
// again, or, by a supersession (next), the next one.
func selectRevision(tx *eventstore.Tx, task types.TaskID, node types.NodeID, req json.RawMessage, next bool) error {
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
	case ok && cur.Revision == r.Revision && cur.Node != "":
		node = cur.Node // the revision's node is its first selection's: an identical one selected again does not move it
	}
	return put(tx, RequirementProjection, index, revision{r.Revision, d, node})
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
// owed input: the replacement answers the new one in their place. A nil
// move removes the answer input, once its question left the lineage (M1
// P6): the fold re-checks it against the recorded owed inputs.
type Moves map[types.Name]*types.AnswerSource

// Supersede is the one transaction replacing review pred after a roster
// change: a stop for pred's attempt if it still runs, then review.superseded
// with the replacement, whose roster is that of requirement, the
// review_requirement node of the subject's next revision, recorded before,
// and with pred's holds transferred or retired. The replacement's roster is
// checked against families, as RecordReview checks.
func Supersede(tx *eventstore.Tx, pred, requirement types.NodeID, families Families) ([]eventstore.Event, error) {
	return SupersedeMoving(tx, pred, requirement, families, nil, nil)
}

// SupersedeMoving is Supersede, with the answer inputs of moves replaced;
// auth tells which questions moved.
func SupersedeMoving(tx *eventstore.Tx, pred, requirement types.NodeID, families Families, moves Moves, auth Authority) ([]eventstore.Event, error) {
	req, err := loadNode(tx, requirement)
	if err != nil {
		return nil, err
	}
	return supersedeOnto(tx, pred, req.Recorded.Node, families, moves, auth)
}

// SupersedeOnto records requirement, a review_requirement node of pred's
// subject at pred's requirement's next revision with another roster
// (RecordReview), and supersedes pred onto it, in one command: no revision
// is ever recorded without the supersession that moves to it (P22a-6a,
// Opus 22a-6a H1).
func SupersedeOnto(tx *eventstore.Tx, pred types.NodeID, requirement types.NodeRecorded, families Families) ([]eventstore.Event, error) {
	recorded, err := RecordReview(requirement, families)
	if err != nil {
		return nil, err
	}
	evs, err := supersedeOnto(tx, pred, requirement.Node, families, nil, nil)
	return append(recorded, evs...), err
}

// supersedeOnto is SupersedeMoving onto req, the requirement node.
func supersedeOnto(tx *eventstore.Tx, pred types.NodeID, req types.Node, families Families, moves Moves, auth Authority) ([]eventstore.Event, error) {
	requirement := req.ID
	rec, err := loadNode(tx, pred)
	id, ierr := ids.New("nod")
	if err = errors.Join(err, ierr); err != nil {
		return nil, err
	}
	if err := superseded(rec); err != nil && !mutation.Is(mutation.SupersedeTwice) {
		return nil, err
	}
	if err := errors.Join(unclaimed(tx, rec), revisionFenced(tx, rec)); err != nil { // as the fold refuses it, before any attempt is stopped
		return nil, err
	}
	p, err := req.ParamsAsReviewRequirement()
	if err != nil || req.Kind != types.NodeKindReviewRequirement {
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
	if a, running, err := runningAttempt(tx, pred); err != nil {
		return nil, err
	} else if running && !mutation.Is(mutation.SupersessionKeepsAttempts) {
		if evs, err = lease.StopRunning(tx, a.ID, types.AttemptStopReasonValuePreempt); err != nil {
			return nil, err
		}
		stopped = append(stopped, a.ID)
	}
	s, err := event(ReviewSuperseded, string(pred), rec.Recorded.Project, &types.ReviewSuperseded{Type: "review_superseded", Predecessor: pred,
		Replacement: n, StoppedAttempts: stopped, TransferredHolds: holdIDs(held), RetiredHolds: retired})
	return append(evs, s...), err
}

// moved are rec's inputs for its replacement: its review_requirement input
// moved to requirement, and each answer input moves names replaced. A move
// needs auth to show the question moved: rec's lineage no longer asks the
// old owed input, and asks the new one at its current revision, which no
// other answer of the replacement answers.
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
		case move && !answer:
			return nil, fmt.Errorf("graph: %s is no answer input: only an answer moves", in.Name)
		case result && src.ResultKind == types.ResultKindReviewRequirement:
			next := *src
			next.Producer = requirement
			in.Source.Variant = &next
		case !move || mutation.Is(mutation.ReplacementKeepsMovedAnswer):
		default:
			if err := movedQuestion(owed, in.Name, ask, to); err != nil {
				return nil, err
			}
			if to != nil && answeredBy(rec.Recorded.Node.Inputs, moves, in.Name, to.OwedInput) {
				return nil, fmt.Errorf("graph: %s moves to %s, which another answer of the review answers", in.Name, to.OwedInput)
			}
			if to != nil {
				in.Source.Variant = &types.AnswerSource{Type: "answer", OwedInput: to.OwedInput, Revision: to.Revision}
			}
		}
		if move {
			used++
		}
		if !move || to != nil || mutation.Is(mutation.ReplacementKeepsMovedAnswer) {
			out = append(out, in) // a removal drops the answer input (13:206, M1 P6)
		}
	}
	if used != len(moves) && !mutation.Is(mutation.ReplacementKeepsMovedAnswer) {
		return nil, errors.New("graph: a move names no answer input of the review")
	}
	return out, nil
}

// answeredBy: an answer input of inputs other than name answers owed, or
// another move goes to it.
func answeredBy(inputs []types.InputSpec, moves Moves, name types.Name, owed types.OwedInputID) bool {
	return slices.ContainsFunc(inputs, func(in types.InputSpec) bool {
		ask, ok := in.Source.Variant.(*types.AnswerSource)
		to := moves[in.Name]
		return in.Name != name && (ok && ask.OwedInput == owed || to != nil && to.OwedInput == owed)
	})
}

// holdsFor splits rec's holds for its replacement n, in rec's order: a
// wait on what rec depends on and n no longer does is retired with its
// reason; every other hold, matched or not, transfers unchanged.
func holdsFor(tx *eventstore.Tx, rec nodeRecord, n types.Node) ([]types.NodeHold, []types.RetiredHold, error) {
	was, _, err := rosterOf(rec.Recorded.Node)
	roster, _, nerr := rosterOf(n)
	if err = errors.Join(err, nerr); err != nil {
		return nil, nil, err
	}
	member := func(r []types.RosterMember, exe string) bool {
		return slices.ContainsFunc(r, func(m types.RosterMember) bool { return string(m.Executor) == exe })
	}
	answers := func(x types.Node, signal string) bool {
		return slices.ContainsFunc(x.Inputs, func(in types.InputSpec) bool {
			ask, ok := in.Source.Variant.(*types.AnswerSource)
			return ok && awaitSignal(ask) == signal
		})
	}
	held, retired := []types.NodeHold{}, []types.RetiredHold{}
	for _, h := range rec.State.Holds {
		by, _, err := load[heldBy](tx, HoldProjection, string(h.ID))
		if err != nil {
			return nil, nil, err
		}
		of, exe, parsed := ParseIndependence(h.SignalInstance)
		onExecutor := parsed && of == "registry"
		var why types.RetiredHoldReason
		switch {
		case mutation.Is(mutation.SupersessionTransfersEveryHold):
		case by.Result != nil:
			why = types.RetiredHoldReasonInvalidated
		// Raise does not check a reviewer_unavailable signal: one that is not
		// registry:<member> matches nothing, and transfers.
		case h.Hold == "reviewer_unavailable" && onExecutor && member(was, exe) && !member(roster, exe):
			why = types.RetiredHoldReasonMemberRemoved
		case h.Hold == "awaiting_input" && answers(rec.Recorded.Node, h.SignalInstance) && !answers(n, h.SignalInstance):
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

func supersededFold(tx *eventstore.Tx, ev eventstore.Event, p *types.ReviewSuperseded, removals bool) error {
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
	if err := errors.Join(unshared(tx, rec), unclaimed(tx, rec), revisionFenced(tx, rec)); err != nil {
		return err
	}
	if _, exists, err := load[nodeRecord](tx, NodeProjection, string(p.Replacement.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the replacement is recorded"))
	}
	requirement, err := replaces(tx, ev, rec, p.Replacement, removals)
	if err != nil {
		return err
	}
	if err := selectRevision(tx, rec.Recorded.Task, requirement.Recorded.Node.ID, requirement.Recorded.Node.Params, true); err != nil {
		return err
	}
	if _, forked, err := load[types.NodeID](tx, SuccessorProjection, string(p.Predecessor)); err != nil || forked && !mutation.Is(mutation.SupersedeTwice) {
		return errors.Join(err, errors.New("the predecessor has a successor: supersede the lineage's last round"))
	}
	if err := stoppedAttempts(tx, ev, p); err != nil {
		return err
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
	if err := putNode(tx, rec); err != nil {
		return err
	}
	return retire(tx, rec.Recorded.Node, p.Replacement)
}

// retire supersedes the requirement pred took by the one its replacement
// takes, once no review that is not superseded takes it and it is held or
// open (Opus 22a-6a re-check L1): no requirement no review takes is left
// held or open in its task. A completed one is its revision's record; a
// claimed one is not retired (adcd moves off a requirement only once it
// completed, or while it is held: stated).
func retire(tx *eventstore.Tx, pred, replacement types.Node) error {
	from, to := takenRequirement(pred), takenRequirement(replacement)
	if from == "" || from == to {
		return nil
	}
	old, err := loadNode(tx, from)
	if err != nil {
		return err
	}
	switch old.State.State {
	case types.NodeStateStateHeld, types.NodeStateStateRunnable, types.NodeStateStateWaiting:
	default:
		return nil
	}
	nodes, err := TaskNodes(tx, old.Recorded.Task)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.Node.Kind == types.NodeKindReview && n.State.State != types.NodeStateStateSuperseded && takenRequirement(n.Node) == from {
			return nil
		}
	}
	old.State.State, old.State.WaitingOn, old.State.Holds, old.State.SupersededBy = types.NodeStateStateSuperseded, []types.Name{}, []types.NodeHold{}, &to
	return putNode(tx, old)
}

// takenRequirement is the review_requirement node review takes ("": none).
func takenRequirement(review types.Node) types.NodeID {
	for _, in := range review.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.ResultKind == types.ResultKindReviewRequirement {
			return src.Producer
		}
	}
	return ""
}

// stoppedAttempts: p lists exactly the predecessor's attempt that this
// command stopped for preemption, if it did, and leaves none running. A
// listed attempt is indexed to the supersession that stopped it.
func stoppedAttempts(tx *eventstore.Tx, ev eventstore.Event, p *types.ReviewSuperseded) error {
	a, running, err := runningAttempt(tx, p.Predecessor)
	if err != nil {
		return err
	}
	if running && !mutation.Is(mutation.SupersessionAdmitsRunningAttempt) {
		return fmt.Errorf("attempt %s of %s still runs: its supersession stops it", a.ID, p.Predecessor)
	}
	want := []types.AttemptID{}
	if a.ID != "" {
		_, by, _, err := lease.AttemptOf(tx, a.ID)
		_, listed, lerr := load[types.NodeID](tx, PreemptedProjection, string(a.ID))
		if err = errors.Join(err, lerr); err != nil {
			return err
		}
		if !listed && a.Status == types.AttemptStatusStopping && a.StopReason != nil && *a.StopReason == types.AttemptStopReasonValuePreempt &&
			by != nil && ev.Command != nil && *by == *ev.Command {
			want = append(want, a.ID)
		}
	}
	if !slices.Equal(p.StoppedAttempts, want) {
		return fmt.Errorf("the supersession lists %v, not %v, the attempt of %s this command stopped for preemption", p.StoppedAttempts, want, p.Predecessor)
	}
	for _, id := range want {
		if err := put(tx, PreemptedProjection, string(id), p.Predecessor); err != nil {
			return err
		}
	}
	return nil
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

// unclaimed: no other node's claim took rec's selected result, but a queue
// generation's carry_over: its merge consumes the review's requirement at
// the subject's selected revision (serves), so it waits for the
// replacement (G7b-2a). No supersession moves a subject's revision while
// a merge of it is live (revisionFenced), so the exception never lets a
// review be superseded under an intended merge.
func unclaimed(tx *eventstore.Tx, rec nodeRecord) error {
	if rec.State.Result == nil {
		return nil
	}
	nodes, err := tx.Keys(ClaimProjection, "")
	for _, n := range nodes {
		c, _, cerr := load[claim](tx, ClaimProjection, n)
		claimant, lerr := loadNode(tx, types.NodeID(n))
		err = errors.Join(err, cerr, lerr)
		took := err == nil && n != string(rec.Recorded.Node.ID) && slices.ContainsFunc(c.Inputs, func(in types.ClaimedInput) bool {
			return in.Result != nil && *in.Result == *rec.State.Result
		})
		switch {
		case !took:
		case claimant.Recorded.Node.Kind != types.NodeKindCarryOver:
			return fmt.Errorf("%s claimed the review's result: re-pointing a dependant is G6b-2's", n)
		}
	}
	return err
}

// revisionFenced: superseding rec, a review, moves its subject's
// requirement revision, which no live merge of its task on that subject
// may see move (subjectFenced): whichever review of the subject it is,
// carried or not.
func revisionFenced(tx *eventstore.Tx, rec nodeRecord) error {
	_, subject, err := rosterOf(rec.Recorded.Node)
	if err != nil {
		return err
	}
	return subjectFenced(tx, rec.Recorded.Task, subject)
}

// replaces: n replaces rec, a review, after a roster change: the same kind,
// definition version, round, requirements, effect and subject, another
// roster, and the same inputs but for rec's one review_requirement input,
// which moves to the requirement returned (a node of rec's task at the next
// revision, with n's subject, roster and definition version), and for the
// answer inputs of moved questions, replaced under their name by one answer
// to an owed input rec does not declare. A removed answer input is admitted
// only with removals, by the fold that checks its question moved (M1 P6).
func replaces(tx *eventstore.Tx, ev eventstore.Event, rec nodeRecord, n types.Node, removals bool) (nodeRecord, error) {
	p := rec.Recorded.Node
	roster, subject, err := rosterOf(p)
	nRoster, nSubject, nerr := rosterOf(n)
	if err = errors.Join(err, nerr); err != nil || n.Kind != p.Kind || n.DefinitionVersion != p.DefinitionVersion || n.Round != p.Round ||
		!reflect.DeepEqual(n.Requires, p.Requires) || !reflect.DeepEqual(n.Effect, p.Effect) || nSubject != subject || slices.Equal(nRoster, roster) {
		return nodeRecord{}, errors.Join(err, errors.New(keeps))
	}
	var from, to *types.ResultSource
	j := 0
	for i, in := range p.Inputs {
		if j == len(n.Inputs) || n.Inputs[j].Name != in.Name {
			if _, answer := in.Source.Variant.(*types.AnswerSource); !answer || !removals {
				return nodeRecord{}, fmt.Errorf("%s: its input %d is missing (only an answer is removed, by a fold that reads the owed inputs, M1)", keeps, i)
			}
			continue // a removed answer, whose question moved (eligible)
		}
		m := n.Inputs[j]
		j++
		a, ok := in.Source.Variant.(*types.ResultSource)
		b, ok2 := m.Source.Variant.(*types.ResultSource)
		switch {
		case ok && ok2 && from == nil && in.Name == m.Name && a.ResultKind == types.ResultKindReviewRequirement && b.ResultKind == a.ResultKind:
			from, to = a, b
		case reflect.DeepEqual(in, m) || answeredAnew(p, n, in, m):
		default:
			return nodeRecord{}, fmt.Errorf("the replacement's input %d is not its review's", i)
		}
	}
	if j != len(n.Inputs) {
		return nodeRecord{}, fmt.Errorf("%s: it adds an input", keeps)
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

// keeps is what a replacement keeps of its review.
const keeps = "a replacement keeps its review's definition, round, requirements, effect, subject and inputs, with another roster"

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

// displaced is the answer input of a supersession in rec's lineage (rec, or
// an ancestor, replaced another review) whose owed input owed, rec's
// lineage's questions, still asks, and which rec does not answer: the
// supersession replaced a question that has not moved.
func displaced(tx *eventstore.Tx, rec nodeRecord, owed []types.AnswerSource) (types.Name, bool, error) {
	if mutation.Is(mutation.DisplacedQuestionUnchecked) {
		return "", false, nil
	}
	answers := func(id types.OwedInputID) bool {
		return slices.ContainsFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool {
			ask, ok := in.Source.Variant.(*types.AnswerSource)
			return ok && ask.OwedInput == id
		})
	}
	for x := rec; ; {
		for r := x; r.Replaces != nil; {
			pred, err := loadNode(tx, *r.Replaces)
			if err != nil {
				return "", false, err
			}
			for _, in := range pred.Recorded.Node.Inputs {
				a, ok := in.Source.Variant.(*types.AnswerSource)
				if ok && asks(owed, a.OwedInput) && !answers(a.OwedInput) {
					return in.Name, true, nil
				}
			}
			r = pred
		}
		if x.Recorded.Predecessor == nil {
			return "", false, nil
		}
		var err error
		if x, err = loadNode(tx, *x.Recorded.Predecessor); err != nil {
			return "", false, err
		}
	}
}
