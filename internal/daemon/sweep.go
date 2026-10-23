// SPDX-License-Identifier: Apache-2.0

package daemon

// The start sweep (P22a-5c-2c; I8, X3 "Late output"; #226 entries 81, 83):
// adcd runs it once with a runner, after restart recovery (recoverAtStart:
// every running attempt lost, sealed, its lease revoked and settled) and
// before the loop's first Run, so no run of this start exists yet:
//  1. RemoveStale removes every container a crashed controller left: every
//     batch run's, and every attempt's interactive one (no attempt is kept:
//     none runs);
//  2. the run directories and clones a crash left go (tearDownRuns,
//     removeClones): every run directory once RemoveStale's listing was
//     complete; otherwise only one a session whose end was unverified kept
//     (keptName, workspace.Keep) whose attempt RemoveStale named, so its
//     container is gone: the rest stay, with their cid, for a later sweep;
//  3. recovery.RefuseOrphans refuses the output each cut attempt left in
//     its exchange root (stale_attempt), and clears the exchange
//     directory;
//  4. each attempt left finishing (its result admitted, the run cut before
//     it was ended), or sealed or completed with its lease not yet ended,
//     is ended as the run would have: its own recorded result kept, its diff sealed (the
//     patch its result holds); with none, could_not_run/ambiguous_state is
//     recorded with no attempt and no diff. Its lease then completes or is
//     revoked, and settles; the node is matched again unless a result was
//     selected.
//
// Every step runs whatever an earlier one returned, and none blocks the
// start: its failures are the start's note, and the next start resumes
// them. The sweep runs only with a runner (no sandbox root, no adapter: no
// sweep), so a start without one leaves a finishing attempt, its lease and
// an orphan's output for the next start with one (stated, Opus 22a-5c-2c
// L4). One adcd holds a sandbox root (lockSandbox).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// removeStale is sandbox.Host.RemoveStale; a test removes in place.
var removeStale = func(r *runner, ctx context.Context) ([]string, error) {
	return r.box.RemoveStale(ctx, func(string) bool { return false })
}

// sweep is the start sweep (the file's comment): every step's failure,
// joined.
func (r *runner) sweep(ctx context.Context) error {
	removed, err := removeStale(r, ctx)
	errs := []error{err, r.tearDownRuns(removed, err == nil), r.removeClones()}
	errs = append(errs, recovery.Recovery{Store: r.store, Actor: r.system.Actor(), Units: graph.Units{}, Blobs: r.blobs, Exchanges: r.host.Root,
		Principal: r.system}.RefuseOrphans(ctx))
	errs = append(errs, r.endLeft(ctx))
	return errors.Join(errs...)
}

// tearDownRuns tears down the run directories a crash left under the
// sandbox root's runs/ (never the exchange or socket directories, the
// adapter host's and the gateway's). At start nothing runs, so with
// RemoveStale's listing complete (complete: it returned no error) no run
// directory has a live container, and every one goes, kept or not (Opus
// 22a-5c-2c M3); otherwise only a kept one whose attempt RemoveStale
// removed, and the rest wait for a later sweep.
func (r *runner) tearDownRuns(removed []string, complete bool) error {
	dirs, err := r.box.Runs()
	errs := []error{err}
	for _, dir := range dirs {
		if dir == filepath.Clean(r.host.Root) || dir == filepath.Clean(r.sockets) {
			continue
		}
		if !complete {
			f, err := os.Open(filepath.Join(dir, keptName))
			var b []byte
			if err == nil {
				b, err = io.ReadAll(io.LimitReader(f, 64))
				err = errors.Join(err, f.Close())
			}
			if errors.Is(err, os.ErrNotExist) {
				continue // not kept: its container may still exist
			}
			if err != nil || ids.Check(string(b), "att") != nil || !slices.Contains(removed, string(b)) {
				errs = append(errs, err) // its container may still exist: kept for a later sweep
				continue
			}
		}
		a, err := r.box.Kept(dir)
		if err == nil {
			err = a.Teardown()
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeClones removes what a crash left in the controller's clones
// directory (the workspaces' clones, the diffs' trees, accept's and the
// gate's clones): at start no run uses it.
func (r *runner) removeClones() error {
	entries, err := os.ReadDir(r.clones)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	errs := []error{err}
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(r.clones, e.Name())))
	}
	return errors.Join(errs...)
}

// left is an attempt the restart left to end: finishing, or sealed or
// completed with its lease not yet ended (active or settling).
type left struct {
	attempt types.Attempt
	node    types.NodeID
	result  *types.Result // its own recorded result, if any
}

// endLeft ends every attempt the restart left (the file's comment, 4).
func (r *runner) endLeft(ctx context.Context) error {
	var todo []left
	err := r.store.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(lease.AttemptProjection, "")
		for _, k := range keys {
			lf, ok, lerr := leftAt(tx, types.AttemptID(k))
			if err = errors.Join(err, lerr); ok {
				todo = append(todo, lf)
			}
		}
		return err
	})
	errs := []error{err}
	for _, lf := range todo {
		errs = append(errs, r.endOne(ctx, lf))
	}
	return errors.Join(errs...)
}

// leftAt is attempt as one left to end (left's): finishing, or sealed or
// completed with its lease active or settling; false otherwise.
func leftAt(tx *eventstore.Tx, attempt types.AttemptID) (left, bool, error) {
	a, _, ok, err := lease.AttemptAt(tx, attempt)
	if err != nil || !ok || a.Status != types.AttemptStatusFinishing && a.Status != types.AttemptStatusSealed && a.Status != types.AttemptStatusCompleted {
		return left{}, false, err
	}
	l, _, ok, err := lease.LeaseOf(tx, a.Lease)
	ending := ok && (l.State == types.LeaseStateActive || l.State == types.LeaseStateSettling)
	if err != nil || !ok || a.Status != types.AttemptStatusFinishing && !ending {
		return left{}, false, err
	}
	n, ok, err := graph.AttemptNode(tx, a.ID)
	if err != nil || !ok {
		return left{}, false, errors.Join(err, fmt.Errorf("attempt %s ran for no node", a.ID))
	}
	lf := left{attempt: a, node: n.Node.ID}
	res, byAttempt, err := graph.AttemptResult(tx, a.ID)
	if err != nil {
		return left{}, false, err
	}
	if byAttempt {
		lf.result = &res
	}
	return lf, true, nil
}

// freshIDs are the type ids an error names (a try's own command and event
// ids among them): End compares causes without them.
var freshIDs = regexp.MustCompile(`\b[a-z]{3}_[0-7][0-9a-hjkmnp-tv-z]{25}\b`)

// errNothingEnded is End's try after which what it was to end still runs.
var errNothingEnded = errors.New("daemon: what still runs was not ended")

// errEndRetried is End's failed try, logged by end itself (once per cause)
// and retried with backoff: start logs nothing more.
var errEndRetried = errors.New("daemon: the ending is retried")

// endRetry is a decided candidate's node End ends at runtime: when its next
// try is due, its failed tries in a row, and the last cause it logged.
type endRetry struct {
	next  time.Time
	tries int
	cause string
}

// End ends what node of task, a decided candidate's, still runs (Opus #408
// H1; #226 entry 166), as the start sweep would (endOne), when its retry is
// due: retryBase, doubling, capped at retryBase·2^(maxTries-1), with no
// limit on the tries (a fault that clears needs no restart), kept in memory
// as Reseal keeps its clocks. A cause is logged only when it changes. No
// hold: node is completed, which carries none (records/node-state.json);
// the board and task show read the same refusal (authority.Endable) as the
// card's ending_blocked. True when it started a try.
func (r *runner) End(task types.TaskID, node types.NodeID) bool {
	r.mu.Lock()
	due := !time.Now().Before(r.ending[node].next)
	r.mu.Unlock()
	return due && r.start(node, func() error { return r.end(task, node) })
}

// endingNow is task's end refused because a unit of it still runs
// (authority.EndingOf's ErrEnding), if it is.
func (r *runner) endingNow(task types.TaskID) (authority.ErrEnding, bool, error) {
	var e authority.ErrEnding
	var still bool
	err := r.store.View(r.ctx, func(tx *eventstore.Tx) error {
		nodes, err := graph.TaskNodes(tx, task)
		if err != nil {
			return err
		}
		_, ok, eerr := authority.EndingOf(tx, task, graph.CandidateGo(nodes))
		still = ok && errors.As(eerr, &e)
		return nil
	})
	return e, still, err
}

// end is one try of End: what task's end is refused on (EndingOf's
// ErrEnding), its open index's entry, ended as the start sweep ends an
// attempt left (leftAt, endOne). Then EndingOf is read again (Opus #408
// re-check M1): a try after which the same entry still runs, one leftAt
// does not cover (a lost attempt, a lease with no attempt) or one whose
// end failed, is an error: counted, backed off and logged once, never a
// pass's nudge. A try after which another entry runs is progress.
func (r *runner) end(task types.TaskID, node types.NodeID) error {
	before, still, err := r.endingNow(task)
	if err == nil && still && !before.Lease {
		var lf left
		var ok bool
		if err = r.store.View(r.ctx, func(tx *eventstore.Tx) (err error) {
			lf, ok, err = leftAt(tx, types.AttemptID(before.Entry))
			return err
		}); err == nil && ok {
			err = r.endOne(r.ctx, lf)
		}
	}
	if err == nil && still {
		after, again, rerr := r.endingNow(task)
		switch {
		case rerr != nil:
			err = rerr
		case again && after.Entry == before.Entry:
			err = fmt.Errorf("%w: %v", errNothingEnded, after)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.ending, node)
		return nil
	}
	if r.ending == nil {
		r.ending = map[types.NodeID]endRetry{}
	}
	e := r.ending[node]
	e.tries++
	e.next = time.Now().Add(retryBase << min(e.tries-1, maxTries-1))
	if msg := freshIDs.ReplaceAllString(err.Error(), "<id>"); msg != e.cause { // a cause, not the ids each try mints
		e.cause = msg
		r.logf("adcd: what node %s still runs could not be ended (retried with backoff): %v", node, err)
	}
	r.ending[node] = e
	return errEndRetried
}

// endOne ends lf as the run would have: sealed (a finishing attempt; its
// result's patch the diff), its result kept or could_not_run recorded with
// no attempt, then its lease ended (endAttempt). Two losses against the
// run's own seal, stated (Opus 22a-5c-2c L1): the output files .gitignore
// kept out of the diff are not carried (no result records them), so accept
// reports no ignored-file warnings for such an attempt; and a result with
// no patch (failed/unsafe_output, say) seals no diff, as one that changed
// nothing.
func (r *runner) endOne(ctx context.Context, lf left) error {
	if lf.attempt.Status == types.AttemptStatusCompleted { // only its lease's end is left
		return r.endActive(ctx, lf.attempt.Lease, lease.LeaseEvent{Kind: lease.Completed})
	}
	var errs []error
	recorded := lf.result
	if lf.attempt.Status == types.AttemptStatusFinishing {
		var diff *types.Digest
		if recorded != nil && recorded.Kind == types.ResultKindPatch && len(recorded.Value) > 0 {
			d := recorded.Value[0].ContentDigest
			diff = &d
		}
		errs = append(errs, r.command(ctx, r.sealStep(lf.attempt.ID, diff, nil)))
	}
	if recorded == nil {
		errs = append(errs, r.command(ctx, r.commitStep(lf.node, nil, func(key types.ComputationKey) (types.Result, error) {
			return resultOf(key, types.ResultKindPatch, types.OutcomeCouldNotRun, vocab.ReasonAmbiguousState, "the controller restarted before it recorded the attempt's result")
		}, &recorded)))
	}
	return errors.Join(append(errs, r.endAttempt(ctx, lf.attempt.Lease, lf.attempt.ID, recorded))...)
}

// resealAfter is how long a claimed accept node waits on its input's seal
// before the runner seals that attempt itself (Reseal).
var resealAfter = 2 * time.Minute

// Reseal seals the build attempt node, a claimed accept node, waits on
// (reconcile.Reseal), once it has waited resealAfter, as the start sweep
// ends an attempt left finishing (endOne): a seal that never landed while
// adcd runs (Opus 22a-5c-2b-3, Main's obligation for 5c-2c). nodes is the
// pass's whole list: a node no longer in it is forgotten (Opus 22a-5c-2c
// L2), so its clock starts again if it waits again. Its tries are bounded
// (maxTries, one a pass); when they are spent, node is held "runner:
// failed" with the cause. That hold, and the attempt's lease, stay until a
// restart: the start sweep then seals the attempt and ends its lease, but
// "runner: failed" is never cleared (the recovery: revoke the task's
// approval; stated, Opus 22a-5c-2c L3). True when it started a seal.
func (r *runner) Reseal(nodes []types.NodeID) bool {
	now := time.Now()
	var due []types.NodeID
	r.mu.Lock()
	if r.waiting == nil {
		r.waiting = map[types.NodeID]time.Time{}
	}
	for n := range r.waiting { // waiting no more (Opus 22a-5c-2c L2)
		if !slices.Contains(nodes, n) {
			delete(r.waiting, n)
			delete(r.resealTries, n)
		}
	}
	for _, node := range nodes {
		since, seen := r.waiting[node]
		if !seen {
			r.waiting[node], since = now, now
		}
		if now.Sub(since) >= resealAfter {
			due = append(due, node)
		}
	}
	r.mu.Unlock()
	started := false
	for _, node := range due {
		started = r.start(node, func() error { return r.reseal(node) }) || started
	}
	return started
}

// reseal is one try of Reseal.
func (r *runner) reseal(node types.NodeID) error {
	lf, ok, err := r.unsealedInput(r.ctx, node)
	if err == nil && ok {
		err = r.endOne(r.ctx, lf)
	}
	r.mu.Lock()
	n := r.resealTries[node] + 1
	if err == nil {
		delete(r.waiting, node)
		delete(r.resealTries, node)
	} else {
		if r.resealTries == nil {
			r.resealTries = map[types.NodeID]int{}
		}
		r.resealTries[node] = n
	}
	r.mu.Unlock()
	if err != nil && n >= maxTries {
		cause := fmt.Errorf("%w: the build's attempt %s could not be sealed: %w", errContent, lf.attempt.ID, err)
		return errors.Join(err, r.hold(node, cause))
	}
	return err
}

// unsealedInput is the build attempt accept node's input waits on: left
// finishing with its own result; false when there is none to seal.
func (r *runner) unsealedInput(ctx context.Context, node types.NodeID) (left, bool, error) {
	var lf left
	ok := false
	err := r.store.View(ctx, func(tx *eventstore.Tx) error {
		ins, _, err := graph.ClaimInputs(tx, node)
		if err != nil || len(ins) != 1 || ins[0].Result == nil {
			return err
		}
		raw, found, err := tx.Get(graph.ResultProjection, string(*ins[0].Result))
		var rr types.ResultRecorded
		if err == nil && found {
			err = json.Unmarshal(raw, &rr)
		}
		if err != nil || !found || rr.Attempt == nil {
			return err
		}
		a, _, found, err := lease.AttemptAt(tx, *rr.Attempt)
		if err != nil || !found || a.Checkpoint != nil || a.Status != types.AttemptStatusFinishing {
			return err
		}
		res := rr.Result
		lf, ok = left{attempt: a, node: rr.Node, result: &res}, true
		return nil
	})
	return lf, ok, err
}
