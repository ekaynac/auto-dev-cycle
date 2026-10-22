// SPDX-License-Identifier: Apache-2.0

package daemon

// The accept run (P22a-5c-2b-2; W15): the controller runs a claimed accept
// node itself, no model, no lease. The loop decides Accept for it
// (reconcile.Accept); the runner starts one goroutine per node, never two
// (its live set), outside any command:
//  1. what the node takes, read in one snapshot: its claim's one input, the
//     build's result, and through it the attempt that recorded it: the
//     patch (its value's blob; none for an empty build), the attempt's base
//     tree, and the output files .gitignore kept out of the patch, as the
//     attempt's sealed record holds them (ignored_files);
//  2. a fresh clone of the project's repository (cloneRepo), where the base
//     tree lies, under the controller's own directory;
//  3. accept.Accept over the base and the patch, under the project's
//     protected paths and lockfiles (accept.PolicyOf), the ignored files its
//     warnings, and the test-integrity guard, accept.Integrity, which runs
//     in #339's killable child (accept.RunChildIfAsked, first in adcd's
//     main), within its own 10 minutes; go list runs in the profile's
//     one pinned image through the sandbox (accept.SandboxLister), and a
//     failure of it is the guard's to judge (the package unverifiable: the
//     candidate refused, fail closed);
//  4. its verdict's report, published, and accept.Result, recorded with no
//     attempt (graph.Commit) in one system command.
//
// The whole run is bounded by acceptBound. A failure of the run's checkout
// (the clone, the one pinned image: the configuration's), of accept's own
// run (the guard's child, git) or the bound cutting a guard that does not
// end (the candidate's integrity unverifiable) is recorded as
// could_not_run/controller_run_failed with no attempt, never an accepted
// tree, and the node is held in the same command, or, if that command is
// refused, held alone. A failure before it (a store's read, a blob that is
// not its digest's, the controller's directory, the profile, the report's
// blob, a panic) records nothing and holds the node. Either hold is the
// runner's (holdOn: a bounded retry, or the configuration's), which the
// loop offers to Unblock (reconcile.ControllerRun), so no pass runs the
// node again until it clears. A build attempt not sealed yet (its seal is
// the command after its result) is no failure: nothing is held, and the
// next pass runs the node. Inert until launch is active: the loop decides
// no Accept until then.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// acceptBound bounds one accept run: the clone, the patch's apply and the
// tree's write, and the guard's own 10 minutes (its child's deadline).
var acceptBound = 15 * time.Minute

// listLimits bound each go list run of the guard in the pinned image.
var listLimits = sandbox.Limits{Memory: 2 << 30, CPUs: "2", PIDs: 512, Timeout: 5 * time.Minute, Disk: 2 << 30}

// acceptLister is the guard's go list: in img, through h's sandbox; a test
// lists otherwise.
var acceptLister = defaultLister

// defaultLister is accept.SandboxLister under listLimits.
func defaultLister(h sandbox.Host, img sandbox.Image) accept.Lister {
	return accept.SandboxLister(h, img, listLimits)
}

// maxPatchRead bounds the patch an accept run reads: past accept's own
// bound, which refuses it unsafe_output.
const maxPatchRead = 64<<20 + 1

var _ reconcile.Acceptor = (*runner)(nil)

// Accept starts node's accept run, unless one is going: true when it
// started one. A run that failed nudges no pass.
func (r *runner) Accept(node types.NodeID) bool {
	return r.start(node, func() error { return r.acceptNode(node) })
}

// acceptance is what an accept run takes.
type acceptance struct {
	view    graph.NodeView
	profile types.ProjectProfile
	patch   []byte
	base    string // the base tree, hex
	ignored []string
}

// acceptNode is one accept run of node (the file's comment).
func (r *runner) acceptNode(node types.NodeID) (err error) {
	ctx, cancel := context.WithTimeout(r.ctx, acceptBound)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			perr := fmt.Errorf("the accept run panicked: %v", p)
			err = errors.Join(err, perr, guarded(func() error { return r.hold(node, perr) }))
		}
	}()
	a, err := r.acceptance(ctx, node, true)
	if errors.Is(err, errNotSealed) { // a backstop: the loop decides Accept only on a sealed input (reconcile.SealedInput); no hold, no nudge
		return err
	}
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	dir, err := os.MkdirTemp(r.clones, "accept-")
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	repo, img, err := r.checkout(ctx, a.profile, dir)
	if err != nil {
		return errors.Join(err, r.couldNotRun(ctx, node, err))
	}
	policy, err := accept.PolicyOf(a.profile)
	if err != nil {
		return errors.Join(err, r.hold(node, fmt.Errorf("%w: %w", errConfig, err)))
	}
	v, err := accept.Accept(ctx, accept.In{Repo: repo, Base: a.base, Patch: a.patch, Policy: policy, Ignored: a.ignored,
		Guard: accept.Integrity(repo, a.base, acceptLister(r.box, img), filepath.Join(r.box.Root, "runs"))})
	if err != nil {
		return errors.Join(err, r.couldNotRun(ctx, node, err))
	}
	report, err := v.Report()
	var d string
	if err == nil {
		d, err = r.blobs.Put(bytes.NewReader(report), "")
	}
	if err != nil {
		return errors.Join(err, r.hold(node, err))
	}
	return r.command(r.ctx, func(tx *eventstore.Tx) ([]eventstore.Event, error) { // past the bound's work: its deadline cuts no record
		cur, err := graph.ViewOf(tx, node)
		if err != nil || cur.State.State != types.NodeStateStateClaimed || cur.State.Key == nil {
			return nil, errors.Join(err, fmt.Errorf("accept node %s holds no claim", node))
		}
		res, err := accept.Result(*cur.State.Key, v, a.view.Project, types.Digest(d))
		if err != nil {
			return nil, err
		}
		return nil, graph.Commit(tx, node, res, r.env)
	})
}

// acceptance reads what node's accept run takes (acceptance's fields): its
// claim's one input, a build's result recorded by its attempt. claimed:
// node must still be claimed (its own run's); a gate reads its accept
// node's after it completed.
func (r *runner) acceptance(ctx context.Context, node types.NodeID, claimed bool) (acceptance, error) {
	var a acceptance
	var patch, checkpoint *types.Digest
	err := r.store.View(ctx, func(tx *eventstore.Tx) error {
		var err error
		if a.view, err = graph.ViewOf(tx, node); err != nil {
			return err
		}
		if a.view.Node.Kind != types.NodeKindAccept || claimed && a.view.State.State != types.NodeStateStateClaimed {
			return fmt.Errorf("node %s is no claimed accept node", node)
		}
		prof, ok, err := authority.ProjectProfile(tx, a.view.Project)
		if err != nil || !ok {
			return errors.Join(err, fmt.Errorf("%w: project %s has no adopted profile", errConfig, a.view.Project))
		}
		a.profile = *prof
		ins, _, err := graph.ClaimInputs(tx, node)
		if err != nil {
			return err
		}
		if len(ins) != 1 || ins[0].Result == nil {
			return fmt.Errorf("%w: accept node %s takes %d inputs, not one build's result", errContent, node, len(ins))
		}
		raw, ok, err := tx.Get(graph.ResultProjection, string(*ins[0].Result))
		var rr types.ResultRecorded
		if err == nil && ok {
			err = json.Unmarshal(raw, &rr)
		}
		switch {
		case err != nil:
			return err
		case !ok || rr.Attempt == nil || rr.Result.Kind != types.ResultKindPatch:
			return fmt.Errorf("%w: accept node %s's input is no patch its build's attempt recorded", errContent, node)
		case len(rr.Result.Value) > 0:
			d := rr.Result.Value[0].ContentDigest
			patch = &d
		}
		at, _, found, err := lease.AttemptAt(tx, *rr.Attempt)
		if err != nil || !found {
			return errors.Join(err, fmt.Errorf("the build's attempt %s is not recorded", *rr.Attempt))
		}
		a.base, checkpoint = strings.TrimPrefix(string(at.Base.Tree), "git:"), at.Checkpoint
		return nil
	})
	if err != nil {
		return a, err
	}
	if checkpoint == nil { // read first: an unsealed input reads no patch (Opus 22a-5c-2b-3 H2)
		return a, errNotSealed
	}
	if patch != nil {
		if a.patch, err = r.readBlob(*patch, maxPatchRead); err != nil {
			return a, err
		}
	}
	b, err := r.readBlob(*checkpoint, 1<<20)
	var rec types.AttemptRecord
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err == nil && rec.IgnoredFiles != nil {
		a.ignored = *rec.IgnoredFiles
	}
	return a, err
}

// errNotSealed: the build's attempt recorded its result and is not sealed
// yet (its seal is the next command): nothing to hold.
var errNotSealed = errors.New("the build's attempt is not sealed yet")

// readBlob is blob d's bytes, at most max, refused unless they hash to d
// (Opus 22a-5c-2b-2 L1: the store's Open does not re-hash).
func (r *runner) readBlob(d types.Digest, max int64) ([]byte, error) {
	rc, err := r.blobs.Open(string(d))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err = errors.Join(err, rc.Close()); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) > max || string(d) != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("blob %s is over %d bytes or not its content", d, max)
	}
	return b, nil
}

// checkout is the controller's clone of profile's repository under dir,
// and its one pinned image resolved. Each failure is the configuration's
// (errConfig; Opus 22a-5c-2b-2 M2, L2): a profile pinning other than one
// image, a repository the clone cannot reach, an image not present.
func (r *runner) checkout(ctx context.Context, profile types.ProjectProfile, dir string) (sandbox.Repo, sandbox.Image, error) {
	if len(profile.Toolchain.Images) != 1 {
		return sandbox.Repo{}, sandbox.Image{}, fmt.Errorf("%w: the profile pins %d images, not one", errConfig, len(profile.Toolchain.Images))
	}
	var image string
	for _, ref := range profile.Toolchain.Images {
		image = string(ref)
	}
	repo, err := cloneRepo(r.box, ctx, profile.Repo.Url, profile.Repo.BaseRef, filepath.Join(dir, "clone"))
	if err != nil {
		return repo, sandbox.Image{}, fmt.Errorf("%w: the repository: %w", errConfig, err)
	}
	img, err := r.place.d.Images.Resolve(ctx, image)
	if err != nil {
		return repo, img, fmt.Errorf("%w: the pinned image: %w", errConfig, err)
	}
	return repo, img, nil
}

// maxCauseDetail bounds the cause a could_not_run result's detail quotes.
const maxCauseDetail = 512

// couldNotRun records accept's run of node as could_not_run/controller_run_failed,
// cause its detail (the bound's cut when ctx, the run's, is done), with no
// attempt, and holds node, in one system command.
func (r *runner) couldNotRun(ctx context.Context, node types.NodeID, cause error) error {
	detail := "accept could not run: " + cause.Error()
	if ctx.Err() != nil {
		detail = fmt.Sprintf("accept did not end within its bound (%s): the candidate's integrity is unverifiable", acceptBound)
	}
	return r.notRunOrHold(node, types.ResultKindTree, detail, cause)
}

// notRun records the controller's run of node as could_not_run/
// controller_run_failed, a result of kind with no value, detail its detail (cut at
// maxCauseDetail), with no attempt, and holds node for cause, in one
// system command.
func (r *runner) notRun(node types.NodeID, kind types.ResultKind, detail string, cause error) error {
	if len(detail) > maxCauseDetail {
		detail = detail[:maxCauseDetail]
	}
	return r.holdAfter(node, cause, func(tx *eventstore.Tx) error {
		cur, err := graph.ViewOf(tx, node)
		if err != nil || cur.State.State != types.NodeStateStateClaimed || cur.State.Key == nil {
			return errors.Join(err, fmt.Errorf("node %s holds no claim", node))
		}
		id, err := ids.New("res")
		if err != nil {
			return err
		}
		return graph.Commit(tx, node, types.Result{ID: types.ResultID(id), Computation: cur.State.Key.Key, Kind: kind, Value: []types.Ref{},
			Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonControllerRunFailed, Detail: detail}, r.env)
	})
}

// notRunOrHold is notRun, or, when its command is refused (the commit's
// revalidation, say), the hold alone, so no refused record leaves the node
// to run again at once (Opus 22a-5c-2b-2 L4).
func (r *runner) notRunOrHold(node types.NodeID, kind types.ResultKind, detail string, cause error) error {
	err := r.notRun(node, kind, detail, cause)
	if err != nil && r.ctx.Err() == nil {
		return errors.Join(err, r.hold(node, cause))
	}
	return err
}

// hold holds node for cause, a failure of the controller's before its
// result (holdOn), in one system command.
func (r *runner) hold(node types.NodeID, cause error) error { return r.holdAfter(node, cause, nil) }

// holdAfter holds node for cause after first, if any, in one system
// command under the daemon's context (the run's bound may be what failed);
// a shutdown holds nothing.
func (r *runner) holdAfter(node types.NodeID, cause error, first func(*eventstore.Tx) error) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		return err
	}
	return r.holdAfterAs(types.CommandID(cmd), node, cause, first)
}

// holdAfterAs is holdAfter under the command id cmd (a gate's receipt
// names the command that records it).
func (r *runner) holdAfterAs(cmd types.CommandID, node types.NodeID, cause error, first func(*eventstore.Tx) error) error {
	ctx := r.ctx
	if ctx.Err() != nil {
		return nil
	}
	dirs := r.dirsReady()
	tries := 0
	err := r.commandAs(ctx, cmd, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if first != nil {
			if err := first(tx); err != nil {
				return nil, err
			}
		}
		evs, n, err := r.holdOn(tx, node, dirs, cause)
		tries = n
		return evs, err
	})
	if err == nil && tries > 0 {
		r.mu.Lock()
		if r.tries == nil {
			r.tries = map[types.NodeID]int{}
		}
		r.tries[node] = tries
		r.mu.Unlock()
	}
	return err
}
