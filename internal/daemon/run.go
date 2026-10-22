// SPDX-License-Identifier: Apache-2.0

package daemon

// The attempt run (P22a-5c-2a; W15, W16): what runs a placed build's
// attempt, from its lease's grant to its result. The loop decides Run for a
// build whose lease is granted, no attempt started on it (reconcile.Run);
// the runner starts one goroutine per such build, never two (an in-memory
// set), outside any command:
//  1. what the attempt runs over, read in one snapshot: the lease, its
//     executor, the unit's pin, and the build's one input, the plan's
//     brief (internal/brief), which the I18 walk (intent.Check over
//     intent.Load's records) must pass: a forged brief is never handed to
//     the builder;
//  2. the workspace: the project's repository at the unit's pinned base, or
//     the branch's tree for its first attempt (openWorkspace);
//  3. the gateway: the attempt's session on a socket of its own
//     (egress.open), its token bound to the lease, the attempt and the call;
//  4. the adapter's session, through the one seam (AdapterStarter: in the
//     attempt's container, containerAdapter, 22a-5d), its image resolved
//     and checked before anything else, the exchange prepared and checked
//     (adapterhost.Host.Prepare), then the container's command made over
//     it (AdapterSession.Ready), the prompt the brief's title and
//     requirement, the tools the edit loop's;
//  5. the launch (placer.launch: the readiness observed, then
//     dispatch.Launch in one command): a miss discards the exchange and
//     starts nothing, the lease released and the node held by Launch;
//  6. the run (Prepared.Run), then the gateway's end, outside any store
//     transaction (#308: Unbind waits for the effects it admitted);
//  7. the completion (complete).
//
// A session whose end is unverified (session.ErrUnverified: a contained
// adapter's removal failed) may still write its workspace: nothing of it
// is diffed, and its attempt directory is kept, never torn down, so the
// cid file there stays the start sweep's handle on the container (22a-5d;
// the lead's ruling). Only the clone, which nothing mounts, is removed.
//
// A failure before the launch ends the lease in the same run (Mythos
// 22a-5c-1 M2): the start is refused and the lease released, so the unit is
// unplaced, its reservation freed, and the loop matches the build again. A
// launch whose own command failed is read back: an attempt it started is
// completed as one that never ran. The run is bounded by the session's
// deadline and the stores' own bounds. Inert until launch is active: the
// loop decides no Run until then. The builder's manifest.intent is recorded
// in 5c-2b (W17); restart (an attempt left running or finishing with no
// runner) is 22a-5c-2c's. adcd builds the runner with a sandbox (runnerOf,
// P22a-5c-2a-w; 22a-5d).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/accept"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost"
	"github.com/ekaynac/auto-dev-cycle/internal/adapterhost/session"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab/exitmap"
)

// buildContractRevision is a build attempt's AttemptBase.ContractRevision:
// an M1 build names no instrument contract.
const buildContractRevision = "1"

// runPrepared is Prepared.Run; a test makes the session's end unverified.
var runPrepared = (*adapterhost.Prepared).Run

// runBounds are an attempt's waits on its adapter (adapterhost.Bounds),
// within attemptBound, the lease's deadline.
var runBounds = adapterhost.Bounds{Start: 30 * time.Second, Handshake: 30 * time.Second, Result: attemptBound - 5*time.Minute, Shutdown: 30 * time.Second}

// sessionDeadline is the adapter session's whole bound: every wait, and a
// minute for the host's own steps.
var sessionDeadline = runBounds.Start + runBounds.Handshake + runBounds.Result + runBounds.Shutdown + time.Minute

// builderOutputTokens is prepare's max_output_tokens, one call's: the
// lease's reservation, which the gateway enforces, bounds the attempt.
const builderOutputTokens = 8000

// maxTree bounds the output tree the completion copies to diff; past it
// the tree is the candidate's fault, unsafe_output.
const maxTree = 1 << 30

// maxBrief bounds the brief the run reads.
const maxBrief = 1 << 20

// runner runs placed builds' attempts (see the file's comment).
type runner struct {
	ctx      context.Context // the daemon's: a run outlives the pass that started it
	store    *eventstore.Store
	system   identity.Principal
	blobs    *blobstore.Store
	place    placer
	env      graph.Env
	host     *adapterhost.Host // its Root, the exchanges', is one configurable value (22a-5d)
	box      sandbox.Host
	clones   string // the controller's own directory the workspaces are cloned and copied under
	sockets  string // the directory the attempts' gateway sockets are made in
	gateway  func(gateway.Binding, string) (types.AdapterPrepareParamsGatewayValue, attemptEnd, error)
	adapters AdapterStarter
	log      io.Writer
	done     func() // after a run ends (the loop's Nudge)

	mu    sync.Mutex
	live  map[types.NodeID]bool
	tries map[types.NodeID]int // failures before the launch in a row, by node (holdOn)
	runs  sync.WaitGroup
}

var _ reconcile.Runner = (*runner)(nil)

// Run starts node's attempt run under granted lease l, unless one is going:
// true when it started one. A run that failed nudges no pass (Opus
// 22a-5c-2a H2): what failed holds its node, and the loop's timer paces
// the rest.
func (r *runner) Run(node types.NodeID, l types.LeaseID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[node] {
		return false
	}
	if r.live == nil {
		r.live = map[types.NodeID]bool{}
	}
	r.live[node] = true
	r.runs.Add(1)
	go func() {
		defer r.runs.Done()
		defer func() {
			r.mu.Lock()
			delete(r.live, node)
			r.mu.Unlock()
		}()
		err := r.attempt(node, l)
		if err != nil && r.log != nil {
			fmt.Fprintf(r.log, "adcd: the attempt run of %s: %v\n", node, err)
		}
		if err == nil && r.done != nil {
			r.done()
		}
	}()
	return true
}

// Wait waits for every run started.
func (r *runner) Wait() { r.runs.Wait() }

// plan is what an attempt runs over, read in one snapshot.
type plan struct {
	view     graph.NodeView
	profile  types.ProjectProfile
	lease    types.Lease
	executor types.RegistryExecutor
	pin      *types.GitID // the unit's first attempt's base, if any
	prompt   []byte
}

// attempt is one run of node under l. A panic anywhere in it is recovered
// as its error (Opus 22a-5c-2a M2): before the launch the lease is ended as
// on any failure; after it, the attempt is ended as one whose facts could
// not be gathered (abandon).
func (r *runner) attempt(node types.NodeID, l types.LeaseID) (err error) {
	ctx := r.ctx
	var pl plan
	var aid types.AttemptID
	launched := false
	defer func() {
		if p := recover(); p != nil {
			perr := fmt.Errorf("the run panicked: %v", p)
			err = errors.Join(err, perr, guarded(func() error {
				if launched {
					return r.abandon(ctx, pl, node, aid, vocab.ReasonAmbiguousState, "the run panicked after its launch")
				}
				return r.endLease(ctx, l, node, perr)
			}))
		}
	}()
	pl, err = r.plan(ctx, node, l)
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	aid, cid, err := newRunIDs()
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	ws, err := openWorkspace(ctx, r.box, r.clones, pl.profile, aid, pl.pin)
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	unverified := false // the session's end unverified: its process may still write the workspace
	defer func() {
		if unverified { // kept: its cid is the start sweep's handle on a container that may still run
			err = errors.Join(err, ws.Keep(), fmt.Errorf("the attempt directory of %s is kept: its session's end is unverified", aid))
			return
		}
		err = errors.Join(err, ws.Close())
	}()
	socket := filepath.Join(r.sockets, string(aid)+".sock")
	gv, end, err := r.gateway(gateway.Binding{Lease: l, Attempt: aid, Call: cid}, socket)
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	var obs gateway.Observed
	ended := false
	endGateway := func() (err error) { // outside any transaction (#308)
		if !ended {
			ended = true
			obs, err = end(ctx)
		}
		return err
	}
	defer func() { err = errors.Join(err, endGateway()) }()
	image, toolchain, err := pinnedImage(pl.profile)
	if err != nil {
		err = fmt.Errorf("%w: project %s %w", errConfig, pl.view.Project, err)
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	lockfiles := make([]string, len(pl.profile.Toolchain.DependencyRecipe.Lockfiles))
	for i, f := range pl.profile.Toolchain.DependencyRecipe.Lockfiles {
		lockfiles[i] = string(f)
	}
	sess, err := r.adapters.Start(ctx, AdapterRun{Attempt: aid, Box: ws.run, Workspace: ws.Path(), Exchange: filepath.Join(r.host.Root, adapterhost.ExchangeName(aid)),
		Socket: socket, Deadline: sessionDeadline, Image: image, Toolchain: toolchain, Lockfiles: lockfiles})
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	base := types.AttemptBase{Tree: ws.Base, ContractRevision: buildContractRevision}
	p, err := r.host.Prepare(adapterhost.Spec{Lease: l, Attempt: aid, Base: base, Call: cid, Adapter: sess.Config, Prompt: pl.prompt, Bounds: runBounds,
		Tools: adapterhost.EditLoopTools(), Path: sess.Path,
		Prepare: types.AdapterPrepareParams{Role: "builder", Executor: pl.lease.Executor, Model: types.AdapterLabel(pl.executor.Model),
			Workspace: types.AdapterPrepareParamsWorkspace{Path: sess.Path(ws.Path()), Writable: true},
			Limits: types.AdapterPrepareParamsLimits{MaxTurns: max(pl.lease.Footprint.ExpectedTurns, 1), MaxOutputTokens: builderOutputTokens,
				WallSeconds: int64(runBounds.Result / time.Second)},
			ToolPolicy: types.AdapterPrepareParamsToolPolicy{Allowed: adapterhost.EditLoopTools(), Mediated: []types.AdapterLabel{}},
			Gateway:    &gv}})
	if err == nil && sess.Ready != nil { // the exchange is made: the container's command can be
		if err = sess.Ready(); err != nil {
			err = errors.Join(err, p.Discard())
		}
	}
	if err != nil {
		return errors.Join(err, r.endLease(ctx, l, node, err))
	}
	miss, err := r.place.launch(ctx, r.store, r.system, node, l, aid, base)
	if err != nil || miss != nil {
		err = errors.Join(err, p.Discard())
		if miss != nil { // Launch released the lease and held the node
			return errors.Join(err, miss)
		}
		// The launch's command failed, but may have applied: read back,
		// whatever ctx, whether it started the attempt.
		started, serr := r.started(context.WithoutCancel(ctx), aid)
		if serr != nil { // read again: abandon only an attempt seen to exist (Opus 22a-5c-2a r2 L3)
			started, serr = r.started(context.WithoutCancel(ctx), aid)
		}
		switch {
		case serr != nil: // unknown: the lease is ended if still granted; an attempt, if any, is restart's
			return errors.Join(err, serr, r.endLease(ctx, l, node, nil))
		case !started:
			return errors.Join(err, r.endLease(ctx, l, node, err))
		}
		launched = true
		return errors.Join(err, endGateway(), r.abandon(ctx, pl, node, aid, vocab.ReasonAmbiguousState, "the launch's command failed after it applied")) // no adapter ran (Mythos L3)
	}
	launched = true
	r.mu.Lock()
	delete(r.tries, node)
	r.mu.Unlock()
	out, runErr := runPrepared(p, ctx)
	unverified = errors.Is(out.Session, session.ErrUnverified) || errors.Is(runErr, session.ErrUnverified)
	endErr := endGateway()
	return errors.Join(runErr, endErr, r.complete(ctx, pl, node, aid, ws, out, obs, unverified))
}

// guarded is f, a panic in it returned as an error.
func guarded(f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("ending the run panicked: %v", p)
		}
	}()
	return f()
}

// newRunIDs are a fresh attempt's id and its call's.
func newRunIDs() (types.AttemptID, types.CallID, error) {
	a, err := ids.New("att")
	c, cerr := ids.New("cal")
	return types.AttemptID(a), types.CallID(c), errors.Join(err, cerr)
}

// plan reads what node's attempt under l runs over: node, its project's
// profile, its one input, the plan's brief, which the I18 walk must pass
// (intent.ErrNotTheBrief: a forgery), the lease (granted, on node's unit)
// and its executor, the unit's pin, and the prompt, from the brief.
func (r *runner) plan(ctx context.Context, node types.NodeID, l types.LeaseID) (plan, error) {
	var pl plan
	var in types.Ref
	err := r.store.View(ctx, func(tx *eventstore.Tx) error {
		var err error
		if pl.view, err = graph.ViewOf(tx, node); err != nil {
			return err
		}
		prof, ok, err := authority.ProjectProfile(tx, pl.view.Project)
		if err != nil || !ok {
			return errors.Join(err, fmt.Errorf("%w: project %s has no adopted profile", errConfig, pl.view.Project))
		}
		pl.profile = *prof
		rendered, err := graph.Context(tx, node, r.env)
		if err != nil {
			return err
		}
		if len(rendered.Included) != 1 || len(rendered.Omitted) != 0 {
			return fmt.Errorf("%w: the build takes %d inputs and omits %d, not the plan's brief alone", errContent, len(rendered.Included), len(rendered.Omitted))
		}
		in = rendered.Included[0]
		recs, err := intent.Load(tx, r.blobs, pl.view.Task)
		if err == nil {
			err = intent.Check(recs, "builder", types.ManifestIntent{Consumer: types.ManifestIntentConsumer{Role: "builder"}, Inputs: []types.Ref{in}})
		}
		if err != nil {
			return err
		}
		var found bool
		if pl.lease, _, found, err = lease.LeaseOf(tx, l); err != nil || !found || pl.lease.State != types.LeaseStateGranted || pl.lease.Unit != graph.UnitOf(node) {
			return errors.Join(err, fmt.Errorf("lease %s is no granted lease of node %s", l, node))
		}
		if pl.executor, found = r.place.d.Registry.Executor(pl.lease.Executor); !found {
			return fmt.Errorf("executor %s is not registered", pl.lease.Executor)
		}
		if first, ok, err := lease.FirstOn(tx, graph.UnitOf(node)); err != nil {
			return err
		} else if ok {
			a, _, _, err := lease.AttemptOf(tx, first)
			if err != nil {
				return err
			}
			pl.pin = &a.Base.Tree
		}
		return nil
	})
	if err != nil {
		return pl, err
	}
	rc, err := r.blobs.Open(string(in.ContentDigest))
	if err != nil {
		return pl, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxBrief))
	if err = errors.Join(err, rc.Close()); err != nil {
		return pl, err
	}
	pb, err := brief.Decode(b)
	if err != nil {
		return pl, err
	}
	pl.prompt = brief.Prompt(pb)
	return pl, nil
}

// started reports whether attempt was started (its launch's command
// applied, whatever it returned).
func (r *runner) started(ctx context.Context, attempt types.AttemptID) (bool, error) {
	var ok bool
	err := r.store.View(ctx, func(tx *eventstore.Tx) (err error) {
		_, _, ok, err = lease.AttemptAt(tx, attempt)
		return err
	})
	return ok, err
}

// endLease ends l, granted and never launched (Mythos 22a-5c-1 M2): its
// start refused and its release in one command (lease.RefuseStart), so its
// unit is unplaced and its reservation freed; a lease found releasing is
// released. A lease past both is left as it is.
// With cause, a failure before the launch that is no shutdown, node is
// held source_invalid_config on a runner signal (holdOn) in the same
// command as the refusal (Opus 22a-5c-2a H2), so no pass matches it again
// until the hold clears (cleared). A hold that cannot be written is logged,
// and the refusal and the release are committed all the same (r2 L2).
func (r *runner) endLease(ctx context.Context, l types.LeaseID, node types.NodeID, cause error) error {
	hold := cause != nil && ctx.Err() == nil
	dirs := r.dirsReady()
	tries := 0 // the retry count of the hold raised, once its command applies
	if err := r.command(ctx, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		cur, rev, ok, err := lease.LeaseOf(tx, l)
		switch {
		case err != nil || !ok:
			return nil, err
		case cur.State == types.LeaseStateReleasing: // a release left over
			return lease.ChangeLease(tx, l, rev, lease.LeaseEvent{Kind: lease.Released}, nil)
		case cur.State != types.LeaseStateGranted:
			return nil, nil
		}
		// Refused and released in one command, with the hold (Mythos L1): a
		// lease left releasing would hold its reservation while the unit
		// was placed again.
		evs, err := lease.RefuseStart(tx, l, rev, vocab.ReasonPreconditionFailed.String())
		if err != nil || !hold {
			return evs, err
		}
		held, n, herr := r.holdOn(tx, node, dirs, cause)
		if herr != nil {
			r.logf("adcd: the runner's hold on %s could not be written: %v", node, herr)
			return evs, nil
		}
		tries = n
		return append(evs, held...), nil
	}); err != nil {
		return err
	}
	if tries > 0 { // the hold applied: the try is spent (r3 L4)
		r.mu.Lock()
		if r.tries == nil {
			r.tries = map[types.NodeID]int{}
		}
		r.tries[node] = tries
		r.mu.Unlock()
	}
	return nil
}

// logf writes one line to the runner's log, if any.
func (r *runner) logf(format string, args ...any) {
	if r.log != nil {
		fmt.Fprintf(r.log, format+"\n", args...)
	}
}

// The runner's hold signals (reconcile.RunnerSignal): its directories; its
// project's profile at the hold; a bounded retry; and the retries spent.
var (
	runnerDirs   = reconcile.RunnerSignal("dirs")
	runnerFailed = reconcile.RunnerSignal("failed")
)

// retryBase is the first retry's wait; each next one doubles it (Opus
// 22a-5c-2a r2 M1). maxTries failures before the launch, in a row, spend
// the retries.
var retryBase = time.Minute

const maxTries = 5

// errConfig marks a failure before the launch that only a configuration
// change fixes: the project's profile, its repository's URL or base.
var errConfig = errors.New("daemon: the runner's configuration")

// errContent marks a failure before the launch no retry fixes: what the
// build takes is not the plan's brief alone (r3 L2).
var errContent = errors.New("daemon: the build's input")

// contentFault reports whether cause is a refusal of what the build takes:
// the I18 walk's (intent.ErrAcceptanceInput: a forged brief among them) or
// its input's shape (errContent).
func contentFault(cause error) bool {
	return errors.Is(cause, intent.ErrAcceptanceInput) || errors.Is(cause, errContent)
}

// configFault reports whether cause is errConfig's: its profile missing,
// or its repository refused, another than the profile names, or without
// its pinned base; or the profile's image not prepared for it, or carrying
// another adapter than the installed release's (sandbox.ErrManifest,
// ErrAdapter among it: a profile pinning the rebuilt image clears it).
func configFault(cause error) bool {
	return errors.Is(cause, errConfig) || errors.Is(cause, ErrRemoteMismatch) || errors.Is(cause, ErrBaseMismatch) || errors.Is(cause, sandbox.ErrSpec) ||
		errors.Is(cause, sandbox.ErrManifest)
}

// holdOn raises the runner's hold on node for cause, while it is claimed:
//   - on its directories, when one is missing (dirs false);
//   - on its project's profile as it is now, for a configuration fault
//     (configFault): cleared by a profile change;
//   - otherwise a bounded retry (r2 M1), "retry <n> until <time>": the
//     n-th failure in a row waits retryBase·2^(n-1) from now; the
//     maxTries-th is "failed", which nothing clears: the recovery is to
//     revoke the task's approval, which abandons it. The count is the
//     runner's, in memory, and counts a try only once its hold applied
//     (r3 L4; n, which endLease records): a restart starts it again;
//   - "failed" at once for a refusal of what the build takes (contentFault,
//     r3 L2): no retry fixes it.
//
// A wrong repository URL fails its clone as a network blip does (sandbox's
// ErrGit), so it retries and ends "failed" (r3 L3, stated); once profiles
// can change, a profile change should clear "failed" too.
func (r *runner) holdOn(tx *eventstore.Tx, node types.NodeID, dirs bool, cause error) (evs []eventstore.Event, n int, err error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil || v.State.State != types.NodeStateStateClaimed {
		return nil, 0, err
	}
	signal := runnerDirs
	switch {
	case !dirs:
	case configFault(cause):
		d, _, err := authority.ProjectProfileDigest(tx, v.Project)
		if err != nil {
			return nil, 0, err
		}
		signal = reconcile.RunnerSignal("profile " + string(d))
	case contentFault(cause):
		signal = runnerFailed
	default:
		r.mu.Lock()
		n = r.tries[node] + 1
		r.mu.Unlock()
		signal = runnerFailed
		if n < maxTries {
			until := tx.Now().Add(retryBase << (n - 1)).UTC().Format(time.RFC3339Nano)
			signal = reconcile.RunnerSignal(fmt.Sprintf("retry %d until %s", n, until))
		}
	}
	id, err := ids.New("hld")
	if err != nil {
		return nil, 0, err
	}
	evs, err = raise(tx, node, types.NodeHold{ID: types.HoldID(id), Hold: vocab.HoldSourceInvalidConfig, SignalInstance: signal,
		Owner: types.OwnerOperator, Pages: []types.NodeHoldPagesItem{}}, nil)
	if err != nil {
		return nil, 0, err
	}
	return evs, n, nil
}

// raise is graph.Raise; a test makes it fail.
var raise = graph.Raise

// dirsReady reports whether the runner's directories are directories.
func (r *runner) dirsReady() bool {
	for _, d := range []string{r.host.Root, r.sockets, r.clones} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// cleared reports whether node's runner hold on signal may clear (the
// placer's Unblocked): the runner's directories are there again; its
// project's profile changed since the hold; or a retry's time has come.
// The spent retries ("failed") never clear.
func (r *runner) cleared(tx *eventstore.Tx, node types.NodeID, signal string) (bool, error) {
	var n int
	var until string
	switch {
	case signal == runnerDirs:
		return r.dirsReady(), nil
	case signal == runnerFailed:
		return false, nil
	}
	if _, err := fmt.Sscanf(signal, reconcile.RunnerSignal("retry %d until %s"), &n, &until); err == nil {
		t, err := time.Parse(time.RFC3339Nano, until)
		return err == nil && !tx.Now().Before(t), err
	}
	v, err := graph.ViewOf(tx, node)
	if err != nil {
		return false, err
	}
	d, _, err := authority.ProjectProfileDigest(tx, v.Project)
	return err == nil && signal != reconcile.RunnerSignal("profile "+string(d)), err
}

// complete records the build's result from the facts of attempt's run and
// ends the attempt and its lease (record). Its reads and commands run under
// a context of their own (WithoutCancel), so a shutdown mid-run still
// completes it (Opus 22a-5c-2a H1):
//   - the facts: the controller's own diff of the workspace (copied
//     without .git, within diffBound) against its base (accept.Diff),
//     published; whether the attempt submitted a result and what it
//     reported; its exit (a stop, no result, the gateway's statuses, the
//     error events' hints). A tree whose session's end is unverified is
//     never diffed;
//   - the result: graph.Build's from the facts, or, for an unsafe output
//     tree (screen, accept.ErrUnsafeTree, or one past maxTree),
//     failed/unsafe_output (the candidate's fault);
//   - facts that cannot be gathered (the attempt unreadable, the diff not
//     taken or not published) end the attempt all the same (abandon).
func (r *runner) complete(ctx context.Context, pl plan, node types.NodeID, attempt types.AttemptID, ws *workspace, out adapterhost.Outcome, obs gateway.Observed, unverified bool) error {
	stopped, err := r.stopped(ctx, attempt)
	if err != nil {
		return errors.Join(err, r.abandon(ctx, pl, node, attempt, vocab.ReasonStoreUnreadable, "the attempt's record could not be read"))
	}
	facts := graph.BuildFacts{Project: pl.view.Project, Submitted: out.Submitted != nil,
		Exit: exitmap.Exit{StopRequested: stopped, Protocol: out.Result == nil && !stopped, Statuses: slices.Clone(obs.Statuses), Hints: slices.Clone(out.Hints)}}
	if out.Submitted != nil {
		facts.Reported = out.Submitted.Outcome
	}
	if unverified { // the run's own verdict (r3 L1): a tree something may still write is never diffed (09), whatever was submitted (r2 N1)
		return r.abandon(ctx, pl, node, attempt, vocab.ReasonAmbiguousState, "the session's end is unverified")
	}
	patch, ignored, unsafe, err := r.diff(ctx, ws, attempt)
	if err != nil {
		return errors.Join(err, r.abandon(ctx, pl, node, attempt, vocab.ReasonAmbiguousState, "the controller's diff of the workspace could not be taken"))
	}
	var diff *types.Digest
	if len(patch) > 0 {
		d, err := r.blobs.Put(bytes.NewReader(patch), "")
		if err != nil {
			return errors.Join(err, r.abandon(ctx, pl, node, attempt, vocab.ReasonAmbiguousState, "the controller's diff could not be published"))
		}
		facts.Diff = types.Digest(d)
		diff = &facts.Diff
	}
	facts.Ignored = ignored
	return r.record(ctx, pl.lease.ID, node, attempt, facts.Submitted, diff, ignored, func(key types.ComputationKey) (types.Result, error) {
		if unsafe != "" {
			id, err := ids.New("res")
			return types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindPatch, Value: []types.Ref{}, Outcome: types.OutcomeFailed,
				Reason: vocab.ReasonUnsafeOutput, Detail: unsafe, Completed: true}, err
		}
		return graph.Build(key, facts)
	})
}

// abandon ends attempt as one whose facts could not be gathered (Opus
// 22a-5c-2a H1, M2): could_not_run for reason, with no attempt and no diff,
// every step of record taken.
func (r *runner) abandon(ctx context.Context, pl plan, node types.NodeID, attempt types.AttemptID, reason vocab.Reason, detail string) error {
	return r.record(context.WithoutCancel(ctx), pl.lease.ID, node, attempt, false, nil, nil, func(key types.ComputationKey) (types.Result, error) {
		id, err := ids.New("res")
		return types.Result{ID: types.ResultID(id), Computation: key.Key, Kind: types.ResultKindPatch, Value: []types.Ref{}, Outcome: types.OutcomeCouldNotRun,
			Reason: reason, Detail: detail}, err
	})
}

// record records node's result (result, over its claim's key) and ends
// attempt and lease l, each step in its own command, every step tried
// whatever an earlier one returned:
//   - an attempt that submitted records it (graph.CommitAttempt) before its
//     seal; one that did not is lost (agent_disconnect) if still running,
//     sealed, and the result is recorded with no attempt (graph.Commit);
//   - a result selected (anything but could_not_run) completes the unit:
//     the attempt is completed with its outcome and reason, and its lease
//     completed; otherwise the unit continues (the sealed attempt is
//     superseded by the next launch) and the lease is revoked with the
//     result's reason. The lease then settles (capacity.Settlement).
func (r *runner) record(ctx context.Context, l types.LeaseID, node types.NodeID, attempt types.AttemptID, submitted bool, diff *types.Digest, ignored []string,
	result func(types.ComputationKey) (types.Result, error)) error {
	var recorded *types.Result
	commit := func(withAttempt bool) func(*eventstore.Tx) ([]eventstore.Event, error) {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			v, err := graph.ViewOf(tx, node)
			if err != nil || v.State.Key == nil {
				return nil, errors.Join(err, fmt.Errorf("node %s holds no claim", node))
			}
			res, err := result(*v.State.Key)
			if err == nil && withAttempt {
				err = graph.CommitAttempt(tx, node, res, attempt, r.env)
			} else if err == nil {
				err = graph.Commit(tx, node, res, r.env)
			}
			if err == nil {
				recorded = &res
			}
			return nil, err
		}
	}
	seal := func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		_, rev, _, err := lease.AttemptAt(tx, attempt)
		if err != nil {
			return nil, err
		}
		return lease.SealWith(tx, attempt, rev, graph.Units{Tasks: authority.Tasks{}}, r.blobs, diff, ignored, r.system.Actor())
	}
	var steps []func(*eventstore.Tx) ([]eventstore.Event, error)
	if submitted {
		steps = append(steps, commit(true), seal)
	} else {
		steps = append(steps, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.LoseRunning(tx, attempt, types.AttemptLostReasonValueAgentDisconnect)
		}, seal, commit(false))
	}
	var errs []error
	for _, step := range steps {
		errs = append(errs, r.command(ctx, step))
	}
	end := lease.LeaseEvent{Kind: lease.Revoked, Reason: "no_result"}
	if recorded != nil && recorded.Outcome != types.OutcomeCouldNotRun {
		end = lease.LeaseEvent{Kind: lease.Completed}
		errs = append(errs, r.command(ctx, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			_, rev, _, err := lease.AttemptAt(tx, attempt)
			if err != nil {
				return nil, err
			}
			return lease.ChangeAttempt(tx, attempt, rev, lease.AttemptEvent{Kind: lease.AttemptDone,
				Completion: &types.AttemptCompletionValue{Outcome: recorded.Outcome, Reason: types.ReasonCode(recorded.Reason)}})
		}))
	} else if recorded != nil {
		end.Reason = recorded.Reason.String()
	}
	errs = append(errs, r.endActive(ctx, l, end))
	return errors.Join(errs...)
}

// stopped reports whether attempt's stop was requested (exitmap's
// StopRequested).
func (r *runner) stopped(ctx context.Context, attempt types.AttemptID) (bool, error) {
	var a types.Attempt
	err := r.store.View(context.WithoutCancel(ctx), func(tx *eventstore.Tx) (err error) {
		a, _, _, err = lease.AttemptOf(tx, attempt)
		return err
	})
	return a.StopReason != nil, err
}

// diffBound bounds the controller's diff of one tree.
const diffBound = 10 * time.Minute

// maxEntries bounds the entries screen walks; past it the tree is unsafe.
const maxEntries = 1 << 20

// diffOf is accept.Diff; a test makes it fail.
var diffOf = accept.Diff

// diff is the controller's own diff of ws against its base, within
// diffBound, over a copy of its tree, and the output files .gitignore kept
// out of it (accept.Diff's ignored, sealed with the attempt for the
// acceptance: Mythos 22a-5c-2a M1): an unsafe tree's reason instead
// (screen, or accept's), and no diff.
func (r *runner) diff(ctx context.Context, ws *workspace, attempt types.AttemptID) (patch []byte, ignored []string, unsafe string, err error) {
	if unsafe, err = screen(ws.Path()); err != nil || unsafe != "" {
		return nil, nil, unsafe, err
	}
	tree := filepath.Join(r.clones, string(attempt)+".tree")
	defer func() { err = errors.Join(err, os.RemoveAll(tree)) }()
	switch err := ws.run.CopyTree(tree, maxTree); {
	case errors.Is(err, sandbox.ErrSpec):
		return nil, nil, "the output tree is past its bound or holds what no tree may", nil
	case err != nil:
		return nil, nil, "", err
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diffBound)
	defer cancel()
	patch, ignored, err = diffOf(dctx, ws.repo, string(ws.Base[len("git:"):]), tree)
	if errors.Is(err, accept.ErrUnsafeTree) {
		return nil, nil, "the output tree is unsafe: a git directory or a look-alike, a path git refuses, or a diff past its bound", nil
	}
	return patch, ignored, "", err
}

// leaves reports whether target, a relative link's, from dir (relative to
// root) leaves the tree, step by step, or passes through another symlink
// on its way (Opus 22a-5c-2a r2 L1: a chain whose links each stay can
// leave). Its last step may be a symlink: screen checks that one itself.
func leaves(root, dir, target string) bool {
	cur := filepath.Clean(dir)
	parts := strings.Split(filepath.ToSlash(target), "/")
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if cur == "." {
				return true
			}
			cur = filepath.Dir(cur)
			continue
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(filepath.Join(root, cur)); err == nil && fi.Mode()&fs.ModeSymlink != 0 && i < len(parts)-1 {
			return true
		}
	}
	return false
}

// screen is why the tree at root is unsafe_output, "" when it is not (Opus
// 22a-5c-2a L1, L2; Mythos L2): a .git anywhere (CopyTree would drop it,
// so the patch would not be what the tree holds; the workspace has none of
// its own), an entry that is no file, directory or symlink (a FIFO, a
// socket, a device: CopyTree drops those too), a symlink whose target
// leaves the tree or passes through another symlink, or more than
// maxEntries entries.
func screen(root string) (string, error) {
	why, n := "", 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		switch n++; {
		case n > maxEntries:
			why = fmt.Sprintf("the output tree holds more than %d entries", maxEntries)
		case d.Name() == ".git": // at the top too: the workspace has none of its own (Mythos L2)
			why = "the output tree holds a .git: " + rel
		case d.Type()&(fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice|fs.ModeIrregular) != 0:
			why = "the output tree holds an entry that is no file, directory or symlink: " + rel
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || leaves(root, filepath.Dir(rel), target) {
				why = "the output tree holds a symlink whose target leaves it or passes through another symlink: " + rel
			}
		}
		if why != "" {
			return fs.SkipAll
		}
		return nil
	})
	return why, err
}

// endActive ends lease l after its attempt: active, by end (completed, or
// revoked); settling, settled as capacity.Settlement derives. A lease
// already past a step is left as it is.
func (r *runner) endActive(ctx context.Context, l types.LeaseID, end lease.LeaseEvent) error {
	for _, from := range []types.LeaseState{types.LeaseStateActive, types.LeaseStateSettling} {
		if err := r.command(ctx, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			cur, rev, ok, err := lease.LeaseOf(tx, l)
			switch {
			case err != nil || !ok || cur.State != from:
				return nil, err
			case from == types.LeaseStateActive:
				return lease.ChangeLease(tx, l, rev, end, nil)
			}
			st, err := capacity.Settlement(tx, l)
			if err != nil {
				return nil, err
			}
			return lease.ChangeLease(tx, l, rev, lease.LeaseEvent{Kind: lease.Settled, Settlement: &st}, nil)
		}); err != nil {
			return err
		}
	}
	return nil
}

// errUnchanged is a step whose logic emits nothing: its command commits
// nothing.
var errUnchanged = errors.New("daemon: the step changes nothing")

// command runs logic in one system command (reconcile.RunAction), whatever
// ctx's cancellation; logic that changes nothing commits nothing.
func (r *runner) command(ctx context.Context, logic func(*eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		return err
	}
	_, err = r.store.SubmitApplied(context.WithoutCancel(ctx), r.system.Command(cmd, reconcile.RunAction, cmd, json.RawMessage(`{}`), nil),
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := logic(tx)
			if err == nil && len(evs) == 0 && tx.Emitted() == 0 {
				err = errUnchanged
			}
			return evs, json.RawMessage(`{}`), err
		}))
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}
