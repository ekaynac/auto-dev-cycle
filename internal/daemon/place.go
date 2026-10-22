// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
)

// attemptBound is one attempt's whole bound: a lease's deadline is its
// grant plus this (the session deadline the attempt run sets, 22a-5c-2).
const attemptBound = time.Hour

// placer is adcd's reconcile.Placer (P22a-5c-1): it places the admitted
// task's builds on the registry's builders under the task's rails, and
// clears a day-rail hold the day reopened. Inert while launch is inactive:
// the loop decides no Match or ClearRail until then.
type placer struct {
	d dispatch.Dispatcher
	// ready is the runner's check that its hold on node (signal) may clear
	// (runner.cleared); nil with no runner: its holds stay.
	ready func(tx *eventstore.Tx, node types.NodeID, signal string) (bool, error)
}

var _ reconcile.Placer = placer{}

// placeConfig is configuration no match of a node can pass until it
// changes: the node is held source_invalid_config on signal, the
// operator's (Opus 22a-5c-1 M1).
type placeConfig struct{ signal, detail string }

func (e *placeConfig) Error() string { return "daemon: " + e.detail }

// Place matches node on its executor, on the card its project's profile
// gives. A refusal raises the hold it names on node: the rail it reaches, or
// source_invalid_config naming the executor or the profile for
// configuration (dispatch's refusals, and a profile pinning other than one
// image or a registry with no builder: placeConfig), so a node no match
// can place waits held, never retried every pass. Any other error (a
// store's, a node placed meanwhile) is the loop's to log.
func (p placer) Place(tx *eventstore.Tx, node types.NodeID) ([]eventstore.Event, error) {
	card, exe, err := p.card(tx, node)
	var cfg *placeConfig
	if errors.As(err, &cfg) {
		id, err := ids.New("hld")
		if err != nil {
			return nil, err
		}
		return graph.Raise(tx, node, types.NodeHold{ID: types.HoldID(id), Hold: "source_invalid_config", SignalInstance: dispatch.PlacementSignal(cfg.signal),
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
// only placement, runner and launch image holds, dispatch.IsPlacementSignal,
// reconcile.IsRunnerSignal and dispatch.IsImageSignal) once the
// configuration admits node again: its card is made, and no placement
// check, missing credential, footprint or missing USD rail refuses it
// (Dispatcher.Configured); nothing otherwise (Mythos 22a-5c-1 M1). Match
// then places it, or raises the rail it reaches. A runner's hold clears
// once the runner's preconditions hold again (ready); a launch's image
// hold once the project's pinned image resolves (imageReady; #226 entry
// 156).
func (p placer) Unblocked(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error) {
	var ready func(*eventstore.Tx, types.NodeID, string) (bool, error)
	switch {
	case dispatch.IsImageSignal(signal): // a launch's wrong_owner hold
		ready = func(tx *eventstore.Tx, node types.NodeID, _ string) (bool, error) { return p.imageReady(tx, node) }
	case reconcile.IsRunnerSignal(signal): // the runner's hold (Opus 22a-5c-2a H2)
		if ready = p.ready; ready == nil {
			return nil, nil
		}
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
		return dispatch.Card{}, "", &placeConfig{"profile " + string(v.Project), fmt.Sprintf("project %s %v", v.Project, err)}
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
		return card, "", &placeConfig{"registry", "no builder is registered (adcd run --registry)"}
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
