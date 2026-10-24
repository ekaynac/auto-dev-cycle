// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/intent"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// attemptBound is one attempt's whole bound: a lease's deadline is its
// grant plus this (the session deadline the attempt run sets, 22a-5c-2).
const attemptBound = time.Hour

// placer is adcd's reconcile.Placer (P22a-5c-1): it places the admitted
// task's builds on the registry's builders under the task's rails, and
// clears a day-rail hold the day reopened. The loop decides a Match or a
// ClearRail only while launch is active (this build, P22a).
type placer struct {
	d dispatch.Dispatcher
	// ready is the runner's check that its hold on node (signal) may clear
	// (runner.cleared); nil with no runner: its holds stay.
	ready func(tx *eventstore.Tx, node types.NodeID, signal string) (bool, error)
	// noSandbox: adcd started with no sandbox root, so no runner runs what
	// it would place (#226 entry 171; plan §0.3): every node is held
	// no_sandbox instead, nothing matched or leased.
	noSandbox bool
}

// runnerSandbox is a no_sandbox hold's signal: a runner's (Unblocked asks
// runner.cleared, which only a start with a sandbox root has).
var runnerSandbox = reconcile.RunnerHold{What: "sandbox"}.Signal()

// raiseNoSandbox holds node no_sandbox, the operator's: what a runner
// would run, with none (#226 entry 171; Opus #414 H1).
func raiseNoSandbox(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	id, err := ids.New("hld")
	if err != nil {
		return nil, err
	}
	return graph.Raise(tx, node, types.NodeHold{ID: types.HoldID(id), Hold: vocab.HoldNoSandbox, SignalInstance: runnerSandbox,
		Owner: types.OwnerOperator, Pages: []types.NodeHoldPagesItem{}}, nil)
}

// sandboxless is adcd's Runner and Acceptor with no sandbox root (no
// runner; Opus #414 H1): what a runner would run is held no_sandbox
// instead, never left with no hold. A granted lease is refused and
// released, and its node held, in one command; a claimed accept or gate,
// and an accept waiting on its input's seal (Reseal), held. End can hold
// nothing: the node it ends is completed, which carries no hold
// (records/node-state); the task's card names the unit still running
// (ending_blocked), and its line is logged once. A start with a sandbox
// root clears every no_sandbox hold (runner.cleared) and runs the work.
type sandboxless struct {
	r      *runner // its store, system actor and log only
	mu     *sync.Mutex
	logged map[types.NodeID]bool
}

func newSandboxless(d *Daemon, logw io.Writer) sandboxless {
	return sandboxless{r: &runner{ctx: context.Background(), store: d.store, system: d.system, log: logw}, mu: &sync.Mutex{}, logged: map[types.NodeID]bool{}}
}

// Run releases granted lease l and holds node.
func (s sandboxless) Run(node types.NodeID, l types.LeaseID) bool {
	err := s.r.command(context.Background(), func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		cur, rev, ok, err := lease.LeaseOf(tx, l)
		if err != nil || !ok || cur.State != types.LeaseStateGranted {
			return nil, err
		}
		evs, err := lease.RefuseStart(tx, l, rev, vocab.ReasonPreconditionFailed.String())
		if err != nil {
			return nil, err
		}
		held, err := raiseNoSandbox(tx, node)
		return append(evs, held...), err
	})
	return s.applied(node, err)
}

// Accept holds node.
func (s sandboxless) Accept(node types.NodeID) bool { return s.hold(node) }

// Gate holds node.
func (s sandboxless) Gate(node types.NodeID) bool { return s.hold(node) }

// Reseal holds each accept node of nodes, waiting on its input's seal.
func (s sandboxless) Reseal(nodes []types.NodeID) bool {
	any := false
	for _, n := range nodes {
		any = s.hold(n) || any
	}
	return any
}

// End holds nothing (a completed node carries none) and logs its node once.
func (s sandboxless) End(_ types.TaskID, node types.NodeID) bool {
	s.mu.Lock()
	seen := s.logged[node]
	s.logged[node] = true
	s.mu.Unlock()
	if !seen {
		s.r.logf("adcd: what node %s still runs is ended only by a start with --sandbox-root (the task's card: ending_blocked)", node)
	}
	return false
}

// hold holds node no_sandbox in its own command.
func (s sandboxless) hold(node types.NodeID) bool {
	return s.applied(node, s.r.command(context.Background(), func(tx *eventstore.Tx) ([]eventstore.Event, error) { return raiseNoSandbox(tx, node) }))
}

// applied reports whether a command applied, logging a failure once per node.
func (s sandboxless) applied(node types.NodeID, err error) bool {
	if err != nil {
		s.mu.Lock()
		seen := s.logged[node]
		s.logged[node] = true
		s.mu.Unlock()
		if !seen {
			s.r.logf("adcd: the no_sandbox hold on %s could not be written: %v", node, err)
		}
	}
	return err == nil
}

var _ reconcile.Placer = placer{}

// placeConfig is configuration no match of a node can pass until it
// changes: the node is held source_invalid_config on signal, the
// operator's (Opus 22a-5c-1 M1).
type placeConfig struct {
	signal dispatch.Placement
	detail string
}

func (e *placeConfig) Error() string { return "daemon: " + e.detail }

// Place matches node on its executor, on the card its project's profile
// gives. A refusal raises the hold it names on node: the rail it reaches, or
// source_invalid_config naming the executor or the profile for
// configuration (dispatch's refusals, and a profile pinning other than one
// image or a registry with no builder: placeConfig), so a node no match
// can place waits held, never retried every pass. Any other error (a
// store's, a node placed meanwhile) is the loop's to log. With no sandbox
// root (noSandbox) every node is held no_sandbox, the operator's, before
// any match: a start with one clears it (runner.cleared on runnerSandbox).
func (p placer) Place(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	if p.noSandbox {
		return raiseNoSandbox(tx, node)
	}
	card, exe, err := p.card(tx, node)
	var cfg *placeConfig
	if errors.As(err, &cfg) {
		id, err := ids.New("hld")
		if err != nil {
			return nil, err
		}
		return graph.Raise(tx, node, types.NodeHold{ID: types.HoldID(id), Hold: "source_invalid_config", SignalInstance: cfg.signal.Signal(),
			Owner: types.OwnerOperator, Pages: []types.NodeHoldPagesItem{}}, nil)
	}
	if err != nil {
		return nil, err
	}
	evs, _, err := p.d.Match(tx, card, exe)
	var refused *dispatch.Refused
	if errors.As(err, &refused) && refused.Hold != nil {
		return graph.Raise(tx, node, *refused.Hold, nil)
	}
	return evs, err
}

// Reopened clears node's day-rail hold when the day's rail leaves it a
// turn now (dispatch.Dispatcher.RoomToday); nothing otherwise.
func (p placer) Reopened(tx *eventstore.Tx, node types.NodeID, hold types.HoldID) ([]eventstore.Event, error) {
	card, exe, err := p.card(tx, node)
	if err != nil {
		return nil, err
	}
	room, err := p.d.RoomToday(tx, card, exe)
	if err != nil || !room {
		return nil, err
	}
	return graph.Clear(tx, node, hold, graph.RailSignal(graph.RailDayUSD))
}

// Unblocked clears node's placement hold (hold, on signal: the loop passes
// only placement, runner, launch image and quota reset holds,
// dispatch.ParsePlacement, reconcile.ParseRunner, dispatch.IsImageSignal
// and reconcile.ParseQuotaReset) once the
// configuration admits node again: its card is made, and no placement
// check, missing credential, footprint or missing USD rail refuses it
// (Dispatcher.Configured); nothing otherwise (Mythos 22a-5c-1 M1). Match
// then places it, or raises the rail it reaches. A runner's hold clears
// once the runner's preconditions hold again (ready); a launch's image
// hold, the launch's or the runner's (dispatch.ImageMiss), once the
// project's pinned image resolves (imageReady; #226 entry 156); a quota
// reset once the provider's reset time has come (§2.6 fault-provider-429).
// Stated (Opus #400 L2): a credentialed executor's placement hold
// stays, naming its own cause, while the gateway lacks the credential,
// even once that cause is fixed: Configured asks for both, as the launch
// would miss on the credential anyway.
func (p placer) Unblocked(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error) {
	var ready func(*eventstore.Tx, types.NodeID, string) (bool, error)
	_, runner := reconcile.ParseRunner(signal)
	_, placement := dispatch.ParsePlacement(signal)
	quota, reset := reconcile.ParseQuotaReset(signal)
	switch {
	case dispatch.IsImageSignal(signal): // a launch's wrong_owner hold
		ready = func(tx *eventstore.Tx, node types.NodeID, _ string) (bool, error) { return p.imageReady(tx, node) }
	case reset: // a provider's rate limit: its reset has come
		ready = func(tx *eventstore.Tx, _ types.NodeID, _ string) (bool, error) {
			return !tx.Now().Before(quota.Until), nil
		}
	case runner: // the runner's hold (Opus 22a-5c-2a H2)
		if ready = p.ready; ready == nil {
			return nil, nil
		}
	case !placement: // none of the placement's, the runner's, the launch's or the provider's: never cleared here (5c-1: typed, not a text prefix)
		return nil, nil
	}
	if ready != nil {
		ok, err := ready(tx, node, signal)
		if err != nil || !ok {
			return nil, err
		}
		return graph.Clear(tx, node, hold, signal)
	}
	card, exe, err := p.card(tx, node)
	var cfg *placeConfig
	if errors.As(err, &cfg) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ok, err := p.d.Configured(tx, card, exe)
	if err != nil || !ok {
		return nil, err
	}
	return graph.Clear(tx, node, hold, signal)
}

// imageReady reports whether node's project's one pinned image resolves
// now, within dispatch.ReadinessBound (the writer's command): its image
// hold clears, the runner's (Opus 22a-5c-2b-3 M1) and the launch's
// wrong_owner one (#226 entry 156). No image store resolves none. It
// resolves the profile's pin, not the image a hold names (Opus #400 L3):
// the two are one while placer.card takes card.Image from that one pin; a
// card whose image is not the profile's would need the hold's own.
func (p placer) imageReady(tx *eventstore.Tx, node types.NodeID) (bool, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil || p.d.Images == nil {
		return false, err
	}
	prof, ok, err := authority.ProjectProfile(tx, v.Project)
	if err != nil || !ok || len(prof.Toolchain.Images) != 1 {
		return false, err
	}
	for _, ref := range prof.Toolchain.Images {
		ctx, cancel := context.WithTimeout(context.Background(), dispatch.ReadinessBound)
		_, rerr := p.d.Images.Resolve(ctx, string(ref))
		cancel()
		return rerr == nil, nil
	}
	return false, nil
}

// card is node's card and executor: its project's data policy, pinned
// image (M1 places on a profile with exactly one) and priority, a deadline
// attemptBound from now, and the first builder, in id order, the data
// policy admits (the first builder when none does, so Match refuses it at
// data_floor). A stated false block (Opus 22a-5c-1 L1): only that first
// admitted builder is tried, so a second admitted builder with room on its
// rail or its group is never placed on while the first is refused.
func (p placer) card(tx *eventstore.Tx, node types.NodeID) (dispatch.Card, types.ExecutorID, error) {
	v, err := graph.ViewOf(tx, node)
	if err != nil {
		return dispatch.Card{}, "", err
	}
	if !slices.Contains(reconcile.Placed, v.Node.Kind) && v.Node.Kind != types.NodeKindReview {
		return dispatch.Card{}, "", fmt.Errorf("daemon: a %s node is not placed on an executor", v.Node.Kind)
	}
	prof, ok, err := authority.ProjectProfile(tx, v.Project)
	if err != nil || !ok {
		return dispatch.Card{}, "", errors.Join(err, fmt.Errorf("daemon: node %s's project %s has no profile", node, v.Project))
	}
	policy, _, err := authority.ProjectDataPolicy(tx, v.Project)
	if err != nil {
		return dispatch.Card{}, "", err
	}
	pd, err := authority.PolicyDigest(policy)
	if err != nil {
		return dispatch.Card{}, "", err
	}
	image, _, err := pinnedImage(*prof)
	if err != nil {
		return dispatch.Card{}, "", &placeConfig{dispatch.Placement{Of: "profile", ID: string(v.Project)}, fmt.Sprintf("project %s %v", v.Project, err)}
	}
	card := dispatch.Card{Node: node, Project: v.Project, Policy: prof.DataPolicy, PolicyDigest: pd, Image: image, Priority: prof.Priority,
		Deadline: types.Time(tx.Now().Add(attemptBound).UTC().Format(time.RFC3339))}
	if v.Node.Kind == types.NodeKindReview { // its pinned roster member, the reviewer the registry derived when it was recorded (P22a-6a)
		params, err := v.Node.ParamsAsReviewNodeParams()
		if err != nil || len(params.Roster) == 0 {
			return card, "", errors.Join(err, fmt.Errorf("daemon: review %s has no roster", node))
		}
		return card, params.Roster[0].Executor, nil
	}
	builders := p.d.Registry.Executors(types.RegistryExecutorRoleBuilder)
	if len(builders) == 0 {
		return card, "", &placeConfig{dispatch.Placement{Of: "registry"}, "no builder is registered (adcd run --registry)"}
	}
	for _, b := range builders {
		if p.d.Registry.Admits(card.Policy, b) == nil {
			return card, b, nil
		}
	}
	return card, builders[0], nil
}

// recordIntent is intent.Record, and checkRegistered registered; a test
// forges the event, or reaches the intent's fold alone.
var (
	recordIntent    = intent.Record
	checkRegistered = registered
)

// registered refuses an intent event that does not state its lease's
// executor exe as reg registers it: its destination, the routing its
// verification gives (routingOf) and its model (Mythos 22a-5c-2b-1 M1).
// The registry is configuration, not the log, so the launch command checks
// it, not the intent's fold: a replay under a changed registry refolds the
// same log.
func registered(ev eventstore.Event, reg registry.Registry, exe types.ExecutorID) error {
	var mi types.ManifestIntent
	if err := json.Unmarshal(ev.Payload, &mi); err != nil {
		return err
	}
	e, ok := reg.Executor(exe)
	switch {
	case !ok:
		return fmt.Errorf("%w: executor %s is not registered", intent.ErrUnbound, exe)
	case mi.Destination != string(e.Destination.ID) || mi.Routing != routingOf(e.Destination.Verification) || mi.Consumer.ModelSnapshot != e.Model:
		return fmt.Errorf("%w: its destination, routing or model is not executor %s's as registered", intent.ErrUnbound, exe)
	}
	return nil
}

// launch starts attempt on node under granted lease l from base, the
// launch boundary in two steps (I16, I21): the readiness observed outside
// the writer (dispatch.Dispatcher.Observe), then, in one system command
// that expects the lease at the revision read, dispatch.Launch, which
// starts the attempt or refuses it, releasing the lease and holding the
// node. The attempt run (22a-5c-2) calls it once its exchange is prepared
// (adapterhost.Host.Prepare), never before; a miss is returned, nothing
// started. With mi, the attempt's manifest.intent is recorded in the same
// command, after the attempt.started it binds, and checked by its fold
// (intent.Projector; P22a-5c-2b-1): an intent the fold refuses refuses
// the launch, so no attempt starts without its intent.
func (p placer) launch(ctx context.Context, store *eventstore.Store, system identity.Principal, node types.NodeID, l types.LeaseID,
	attempt types.AttemptID, base types.AttemptBase, mi *types.ManifestIntent) (*dispatch.Miss, error) {
	var card dispatch.Card
	var exe types.ExecutorID
	var rev int64
	if err := store.View(ctx, func(tx *eventstore.Tx) error {
		ls, r, ok, err := lease.LeaseOf(tx, l)
		if err == nil && !ok {
			err = fmt.Errorf("daemon: lease %s is not recorded", l)
		}
		if err != nil {
			return err
		}
		card, _, err = p.card(tx, node)
		exe, rev = ls.Executor, r
		return err
	}); err != nil {
		return nil, err
	}
	ready := p.d.Observe(ctx, card, exe)
	cmd, err := ids.New("cmd")
	input, merr := json.Marshal(map[string]any{"node": node, "lease": l, "attempt": attempt})
	if err = errors.Join(err, merr); err != nil {
		return nil, err
	}
	var miss *dispatch.Miss
	_, err = store.SubmitApplied(ctx, system.Command(cmd, reconcile.LaunchAction, cmd, input, nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, m, err := p.d.Launch(tx, l, rev, card, attempt, base, ready)
			miss = m
			if err == nil && m == nil && mi != nil {
				var ev eventstore.Event
				ev, err = recordIntent(*mi)
				if err == nil {
					err = checkRegistered(ev, p.d.Registry, exe)
				}
				evs = append(evs, ev)
			}
			return evs, json.RawMessage(`{}`), err
		}))
	return miss, err
}

// pinnedImage is p's one pinned toolchain image and its key: M1 places, and
// runs the adapter's container, on a profile with exactly one.
func pinnedImage(p types.ProjectProfile) (image, toolchain string, err error) {
	if len(p.Toolchain.Images) != 1 {
		return "", "", fmt.Errorf("pins %d toolchain images; M1 places on exactly one", len(p.Toolchain.Images))
	}
	for k, img := range p.Toolchain.Images {
		image, toolchain = string(img), k
	}
	return image, toolchain, nil
}
