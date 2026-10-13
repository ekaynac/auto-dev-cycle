// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/testguard/fakes/owedanswers"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// supersessionFix is a store with the lease, graph and owed-answer folds,
// which a restart closes and reopens. While crash is set, the writer fails
// inside the transaction at a review.superseded event: after the command's
// stops are appended and folded, before the supersession is folded.
type supersessionFix struct {
	*graphFix
	dir   string
	holds map[string]types.Hold
	close func() error
	crash atomic.Bool
	// watched is an attempt the crashing command stops; folded, whether its
	// stop was folded when the crash came.
	watched types.AttemptID
	folded  atomic.Bool
}

// crashing is the projector that fails while crash is set.
func (f *supersessionFix) crashing(tx *eventstore.Tx, evs []eventstore.Event) error {
	if f.crash.Load() && slices.ContainsFunc(evs, func(e eventstore.Event) bool { return e.Kind == graph.ReviewSuperseded }) {
		a, by, _, err := lease.AttemptOf(tx, f.watched)
		f.folded.Store(err == nil && a.Status == types.AttemptStatusStopping && by != nil)
		return errCrash
	}
	return nil
}

func (f *supersessionFix) open() error {
	state := filepath.Join(f.dir, "state")
	lock, err := eventstore.AcquireLock(state)
	if err != nil {
		return err
	}
	blobs, err := blobstore.Open(state)
	none := func(*eventstore.Tx, eventstore.Command) error { return nil }
	var s *eventstore.Store
	if err == nil {
		kinds := graph.Kinds()
		maps.Copy(kinds, lease.Kinds())
		maps.Copy(kinds, owedanswers.Kinds())
		project := eventstore.ProjectEach(lease.Projector{Blobs: blobs, Holds: f.holds, Units: graph.Units{}}.Project, f.crashing, graph.Project, owedanswers.Project)
		s, err = eventstore.Open(f.ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Hooks: eventstore.Hooks{Identify: none, Authorize: none, Project: project}})
	}
	if err != nil {
		return errors.Join(err, lock.Release())
	}
	f.s, f.blobs = s, blobs
	f.close = func() error { return errors.Join(s.Close(), lock.Release()) }
	return nil
}

// doKeyed submits fn as the command with idempotency key key.
func (f *supersessionFix) doKeyed(key string, fn func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	_, err := f.submitKeyed(key, fn)
	return err
}

// submitKeyed is doKeyed, with what the writer committed or replayed.
func (f *supersessionFix) submitKeyed(key string, fn func(*eventstore.Tx) ([]eventstore.Event, error)) (c eventstore.Outcome, err error) {
	cmd, err := ids.New("cmd")
	if err == nil {
		c, err = f.s.SubmitApplied(f.ctx, eventstore.Command{ID: cmd, Actor: f.actor, Action: "fixture.command", IdempotencyKey: key, Input: []byte(`{}`)},
			eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := fn(tx)
				return evs, json.RawMessage(`{}`), err
			}))
	}
	return c, err
}

// restart closes the store and opens it again.
func (f *supersessionFix) restart() error {
	if err := f.close(); err != nil {
		return err
	}
	return f.open()
}

// revision is the subject candidate's selected requirement revision.
func (f *supersessionFix) revision() (r struct {
	Revision int64 `json:"revision"`
}, err error) {
	err = f.s.View(f.ctx, func(tx *eventstore.Tx) error {
		raw, ok, err := tx.Get(graph.RequirementProjection, string(gTask)+"/candidate")
		if err == nil && ok {
			err = json.Unmarshal(raw, &r)
		}
		return err
	})
	return r, err
}

// attempt is the attempt's projected record.
func (f *supersessionFix) attempt(id types.AttemptID) (a types.Attempt, err error) {
	err = f.s.View(f.ctx, func(tx *eventstore.Tx) (err error) { a, _, _, err = lease.AttemptOf(tx, id); return err })
	return a, err
}

// supersessions are the review.superseded payloads in the log.
func (f *supersessionFix) supersessions() (out []types.ReviewSuperseded, err error) {
	err = f.s.View(f.ctx, func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			var g types.GraphEvent
			if e.Kind == graph.ReviewSuperseded && err == nil {
				err = json.Unmarshal(e.Payload, &g)
				out = append(out, *g.Variant.(*types.ReviewSuperseded))
			}
		}
		return err
	})
	return out, err
}

// requirementOf are the params of requirement revision rev for a review by
// exes, families from reviewers.
func requirementOf(rev int64, exes ...types.ExecutorID) (json.RawMessage, error) {
	roster, err := graph.Roster(exes, reviewers)
	if err != nil {
		return nil, err
	}
	return json.Marshal(types.ReviewRequirement{Subject: "candidate", Roster: roster, DefinitionVersion: "1", Revision: rev})
}

// supersessionStep is a command's step: the refusals the supersession's
// cases name, and any other error as itself, which no golden expects.
func supersessionStep(err error) Step {
	var sup graph.ErrSuperseded
	var notOwed graph.ErrNotOwed
	switch {
	case err == nil:
		return cleanStep
	case errors.As(err, &sup):
		return Step{vocab.Failed, sup.Reason().String()}
	case errors.As(err, new(*lease.Stale)):
		return Step{vocab.Failed, vocab.ReasonStaleAttempt.String()}
	case errors.As(err, &notOwed):
		return refusedStep
	}
	return Step{vocab.Failed, err.Error()}
}

// errCrash stands in for a controller crash inside a transaction.
var errCrash = errors.New("crash")

// rosterChangeOneSupersession: a roster change is one recorded supersession
// (13 "Replacement is a recorded supersession", "What M0 delivers"; plan 15
// G6b-1). A review by exeA, under requirement revision 1, has its attempt
// running, bound to it (M1 P8a: its lease's unit is the review). The
// roster changes to exeB (revision 2). The steps:
//  1. the supersession, after a crash inside its first transaction (its
//     stop appended and folded, the supersession not yet) recorded nothing
//     that survives a restart: it stops the running attempt, whose
//     submission then races it;
//  2. after a restart, the same command replays;
//  3. a second supersession of the review is refused;
//  4. the attempt's late result is refused, stopped or finishing;
//  5. a result is never selected for the superseded review: committing one
//     to it is refused;
//  6. the replacement's successor answers what the superseded review
//     asked.
//
// The log holds one supersession, the racing submission and the stop
// ordered either way, and replay rebuilds the same state. A forged
// supersession listing the attempt it did not stop for preemption (or
// that it revoked), one that another supersession of the command listed,
// and one leaving the attempt running unlisted, are refused.
func rosterChangeOneSupersession(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	reg, err := vocab.Load()
	actor, aerr := ids.New("act")
	if err = errors.Join(err, aerr); err != nil {
		return nil, nil, err
	}
	f := &supersessionFix{graphFix: &graphFix{ctx: ctx, actor: actor}, dir: dir, holds: reg.Holds}
	if err := f.open(); err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.close()) }()
	review, second, next, running, err := f.superseding()
	if err != nil {
		return nil, nil, err
	}
	var o observe
	_, err = f.startOn(second)
	o.check(err != nil, "an attempt started on a review never claimed")
	supersede := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Supersede(tx, review, next, reviewers)
	}
	key, err := ids.New("cmd")
	if err != nil {
		return nil, nil, err
	}
	// The crash: inside the transaction, after its stops are appended and
	// folded, before the supersession is folded. After a restart nothing of
	// it remains: no stop, supersession, replacement or revision, and no
	// command result (the retry below runs, it is not replayed).
	f.watched = running
	f.crash.Store(true)
	err = f.doKeyed(key, supersede)
	f.crash.Store(false)
	if !errors.Is(err, errCrash) {
		return nil, nil, fmt.Errorf("the crashed command: %v", err)
	}
	if err := f.restart(); err != nil {
		return nil, nil, err
	}
	var a types.Attempt
	var by *string
	err = f.s.View(ctx, func(tx *eventstore.Tx) (err error) { a, by, _, err = lease.AttemptOf(tx, running); return err })
	pre, _, serr := f.node(review)
	sups, uerr := f.supersessions()
	rev, rerr := f.revision()
	if err = errors.Join(err, serr, uerr, rerr); err != nil {
		return nil, nil, err
	}
	o.check(f.folded.Load(), "the crash came before the command's stops were folded")
	o.check(a.Status == types.AttemptStatusRunning && by == nil && len(sups) == 0 && pre.State == types.NodeStateStateClaimed && rev.Revision == 1,
		"the crashed supersession left a %s attempt (stopped by %v), %d supersessions, a %s review, revision %d", a.Status, by, len(sups), pre.State, rev.Revision)
	// Forged supersessions are refused: one listing the attempt it did not
	// stop for preemption, and one leaving it running, unlisted (last: the
	// fold's own check, graph.supersession-admits-running-attempt).
	for _, listed := range [][]types.AttemptID{{running}, {}} {
		forged, err := ids.New("cmd")
		if err != nil {
			return nil, nil, err
		}
		o.check(f.doKeyed(forged, listing(supersede, listed...)) != nil, "a supersession listing %v, the running attempt not stopped, was recorded", listed)
	}
	// Nor is one listing the running attempt it stopped for another reason.
	// Nor is a second supersession in the same command listing an attempt
	// the first stopped.
	twice, err := ids.New("cmd")
	if err != nil {
		return nil, nil, err
	}
	o.check(f.doKeyed(twice, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := graph.Supersede(tx, review, next, reviewers)
		other, oerr := listing(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.Supersede(tx, second, next, reviewers)
		}, running)(tx)
		return append(evs, other...), errors.Join(err, oerr)
	}) != nil, "two supersessions listing one attempt were recorded")
	revoking, err := ids.New("cmd")
	if err != nil {
		return nil, nil, err
	}
	o.check(f.doKeyed(revoking, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := listing(supersede, running)(tx)
		revoke, rerr := lease.ChangeAttempt(tx, running, 1, lease.AttemptEvent{Kind: lease.StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
		return append(revoke, evs...), errors.Join(err, rerr)
	}) != nil, "a supersession listing an attempt it revoked was recorded")
	var wg sync.WaitGroup
	var raced error
	wg.Go(func() { raced = f.do(submitted(running)) })
	first, err := f.submitKeyed(key, supersede)
	o.steps = append(o.steps, supersessionStep(err))
	o.check(err != nil || !first.Replayed, "the retry after the crash replayed a result")
	wg.Wait()
	if err := f.restart(); err != nil {
		return nil, nil, err
	}
	diffs, err := f.s.RebuildProjections(ctx)
	if err != nil {
		return nil, nil, err
	}
	o.check(len(diffs) == 0, "replay reached another state: %+v", diffs)
	again, err := f.submitKeyed(key, supersede)
	o.check(err != nil || again.Replayed, "the retried command after the restart ran again")
	o.steps = append(o.steps, supersessionStep(err), supersessionStep(f.do(supersede)), supersessionStep(f.do(submitted(running))))
	st, _, err := f.node(review)
	rid, ierr := ids.New("res")
	if err = errors.Join(err, ierr); err != nil || st.Key == nil {
		return nil, nil, errors.Join(err, errors.New("the review has no key"))
	}
	r := types.Result{ID: types.ResultID(rid), Computation: st.Key.Key, Kind: types.ResultKindReview, Value: []types.Ref{},
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	o.steps = append(o.steps, supersessionStep(f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return nil, graph.Commit(tx, review, r, f.env(treeA, demandA))
	})))
	var rep types.NodeID
	if st.SupersededBy != nil {
		rep = *st.SupersededBy
	}
	_, err = f.successorOf(rep, answerTo(owedRuling))
	o.steps = append(o.steps, supersessionStep(err))

	sups, err = f.supersessions()
	if err != nil {
		return nil, nil, err
	}
	o.check(len(sups) == 1, "the log holds %d supersessions, want 1", len(sups))
	want := types.AttemptStatusStopping
	if raced == nil { // the submission was ordered first
		want = types.AttemptStatusFinishing
	}
	if a, err = f.attempt(running); err != nil {
		return nil, nil, err
	}
	stopped := len(sups) > 0 && slices.Equal(sups[0].StoppedAttempts, []types.AttemptID{running})
	o.check(a.Status == want && stopped == (want == types.AttemptStatusStopping),
		"the attempt is %s (stopped by the supersession: %t), want %s", a.Status, stopped, want)
	return o.steps, o.violations, nil
}

// submitted submits a clean result of attempt, token 1.
func submitted(attempt types.AttemptID) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		call, err := ids.New("cal")
		if err != nil {
			return nil, err
		}
		return lease.SubmitResult(tx, attempt, 1, types.SubmittedResult{Call: types.CallID(call), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
	}
}

// superseding completes requirement revision 1 (exeA) and records a review
// by exeA under it, which asks owedRuling, claimed, with its attempt
// running; it records revision 2 (exeB), and another review of the
// subject, not claimed.
func (f *supersessionFix) superseding() (review, second, next types.NodeID, running types.AttemptID, err error) {
	f.plan, f.planRes, err = f.complete(types.NodeKindPlan, json.RawMessage(`{}`), []types.InputSpec{}, map[string]graph.Resolved{}, types.ResultKindPlan, []types.Ref{}, nil)
	one, err1 := requirementOf(1, exeA)
	two, err2 := requirementOf(2, exeB)
	if err = errors.Join(err, err1, err2); err != nil {
		return "", "", "", "", err
	}
	req, err := f.record(types.NodeKindReviewRequirement, one, []types.InputSpec{})
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.ClaimReview(tx, req, map[string]graph.Resolved{}, 1, reviewers)
		})
	}
	var res types.ResultID
	if err == nil { // a requirement's result is found, its conclusions its params
		st, _, serr := f.node(req)
		rid, ierr := ids.New("res")
		if err = errors.Join(serr, ierr); err == nil && st.Key == nil {
			err = errors.New("the requirement is not claimed")
		}
		if err == nil {
			res = types.ResultID(rid)
			r := types.Result{ID: res, Computation: st.Key.Key, Kind: types.ResultKindReviewRequirement, Value: []types.Ref{}, Conclusions: &one,
				Outcome: types.OutcomeFound, Reason: vocab.ReasonCompletedWithFindings, Detail: "required", Completed: true}
			err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return nil, graph.Commit(tx, req, r, f.env(treeA, demandA))
			})
		}
	}
	if err == nil {
		review, err = f.record(types.NodeKindReview, soleReviewer, append(on("plan", f.plan, types.ResultKindPlan), on("required", req, types.ResultKindReviewRequirement)...))
	}
	if err == nil {
		err = f.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return graph.ClaimReview(tx, review, map[string]graph.Resolved{"plan": {Result: f.planRes}, "required": {Result: res}}, 0, reviewers)
		})
	}
	if err == nil {
		next, err = f.record(types.NodeKindReviewRequirement, two, []types.InputSpec{})
	}
	if err == nil { // another review of the subject, not claimed
		second, err = f.record(types.NodeKindReview, soleReviewer, append(append(on("plan", f.plan, types.ResultKindPlan), on("required", req, types.ResultKindReviewRequirement)...),
			on("again", f.plan, types.ResultKindPlan)...))
	}
	if err == nil {
		err = f.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
			return owedanswers.Record(owedanswers.Answer{Owed: owedRuling, Project: gProject, Asker: review, Owner: types.ActorID(f.actor), Revision: 1, Approval: ptrTo(approvalOne)})
		})
	}
	if err == nil {
		running, err = f.startOn(review)
	}
	return review, second, next, running, err
}

// startOn starts the first attempt on node's unit, under a fresh lease.
func (f *supersessionFix) startOn(node types.NodeID) (types.AttemptID, error) {
	l, att, err := freshLease()
	l.Unit = graph.UnitOf(node)
	base := types.AttemptBase{Tree: types.GitID(treeA), ContractRevision: "7"}
	for _, step := range []func(*eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, l.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, f.holds)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartAttempt(tx, l.ID, 2, att, base) },
	} {
		if err == nil {
			err = f.do(step)
		}
	}
	return att, err
}

// listing is fn's supersession alone, listing attempts as the ones it
// stopped.
func listing(fn func(*eventstore.Tx) ([]eventstore.Event, error), attempts ...types.AttemptID) func(*eventstore.Tx) ([]eventstore.Event, error) {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := fn(tx)
		if err != nil {
			return nil, err
		}
		var g types.GraphEvent
		last := evs[len(evs)-1]
		if err := json.Unmarshal(last.Payload, &g); err != nil {
			return nil, err
		}
		g.Variant.(*types.ReviewSuperseded).StoppedAttempts = append([]types.AttemptID{}, attempts...)
		last.Payload, err = json.Marshal(g)
		return []eventstore.Event{last}, err
	}
}
