// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// Images resolves a pinned image in the host's storage, never pulling
// (sandbox.Host).
type Images interface {
	Resolve(ctx context.Context, ref string) (sandbox.Image, error)
}

// Credentials is P11's launch-readiness seam: whether the gateway holds
// the named credential for a destination. Nothing implements it until
// P11.
type Credentials interface {
	Holds(ctx context.Context, destination, credential types.Slug) (bool, error)
}

// Readiness is what the launch boundary observed outside the writer, bound
// to what it observed for: the executor, its destination and credential
// reference, and the image. Only Observe makes one; the zero value was
// observed for nothing, so it is never ready. image and credential are nil
// when the pinned image is present at its digest, and when the gateway
// holds the destination's credential (or it needs none).
type Readiness struct {
	observed          bool
	executor          types.ExecutorID
	destination       types.Slug
	credentialRef     *types.Slug
	imageRef          string
	image, credential error
}

var errNoGateway = errors.New("dispatch: no gateway confirms credentials (P11)")

// Observe probes, outside the writer transaction, what the launch of card
// on executor needs from the host and the gateway.
func (d Dispatcher) Observe(ctx context.Context, card Card, executor types.ExecutorID) Readiness {
	r := Readiness{observed: true, executor: executor, imageRef: card.Image}
	if d.Images == nil {
		r.image = sandbox.ErrImage
	} else if _, err := d.Images.Resolve(ctx, card.Image); err != nil {
		r.image = err
	}
	e, ok := d.Registry.Executor(executor)
	r.destination, r.credentialRef = e.Destination.ID, e.CredentialRef
	switch {
	case !ok:
		r.credential = fmt.Errorf("dispatch: executor %s is not registered", executor)
	case e.CredentialRef == nil: // a local endpoint has none
	case d.Credentials == nil:
		r.credential = errNoGateway
	default:
		held, err := d.Credentials.Holds(ctx, e.Destination.ID, *e.CredentialRef)
		if err == nil && !held {
			err = fmt.Errorf("dispatch: the gateway holds no credential %s for destination %s", *e.CredentialRef, e.Destination.ID)
		}
		r.credential = err
	}
	return r
}

// Miss is a launch the readiness check refused: why, and the node hold
// naming the missing item, nil when the node no longer holds its claim.
type Miss struct {
	Reason string
	Hold   *types.NodeHold
}

func (m *Miss) Error() string { return "dispatch: launch refused: " + m.Reason }

// Launch is the launch readiness check in the command that records
// attempt.started: under the granted lease at the expected revision, it
// starts card's node's next attempt (lease.StartNext: the next fencing
// token), or, on a miss, refuses the start, releases the lease and holds
// the node, and returns the miss.
func (d Dispatcher) Launch(tx *eventstore.Tx, id types.LeaseID, expected int64, card Card, attempt types.AttemptID, base types.AttemptBase, ready Readiness) ([]eventstore.Event, *Miss, error) {
	l, _, ok, err := lease.LeaseOf(tx, id)
	if err != nil || !ok || l.Unit != graph.UnitOf(card.Node) {
		return nil, nil, errors.Join(err, fmt.Errorf("dispatch: lease %s is not one of node %s", id, card.Node))
	}
	miss, err := d.miss(tx, l, card, ready)
	if err != nil {
		return nil, nil, err
	}
	if miss == nil {
		evs, err := lease.StartNext(tx, id, expected, attempt, base)
		return evs, nil, err
	}
	evs, err := lease.RefuseStart(tx, id, expected, miss.Reason)
	if err == nil && miss.Hold != nil {
		var held []eventstore.Event
		held, err = graph.Raise(tx, card.Node, *miss.Hold, nil)
		evs = append(evs, held...)
	}
	if err != nil {
		return nil, nil, err
	}
	return evs, miss, nil
}

// miss re-checks the launch of l, failing closed: the node still claimed
// and of the card's project, its roster still the registry's (13:106),
// the placement the lease recorded still what the registry and the card
// derive, an image store and (for a credentialed destination) a gateway
// configured, the readiness observed for exactly this executor,
// destination, credential and image, the image present and the credential
// held, in that order. Only a refusal of the node itself is a miss; a
// store error is returned.
func (d Dispatcher) miss(tx *eventstore.Tx, l types.Lease, card Card, ready Readiness) (*Miss, error) {
	project, roster, err := graph.Units{}.Placement(tx, l.Unit, d.Registry)
	var ni graph.ErrNotIndependent
	var ns graph.ErrNotStartable
	switch {
	case errors.As(err, &ni): // as the evaluator holds it (graph.Evaluate)
		return held("the registry no longer gives roster member "+string(ni.Executor)+" its family",
			"reviewer_unavailable", "registry:"+string(ni.Executor), types.OwnerSystem, nil)
	case errors.As(err, &ns):
		return &Miss{Reason: reason("the node is no longer claimed: " + ns.Detail)}, nil
	case err != nil:
		return nil, err
	case project != card.Project:
		return &Miss{Reason: reason("node " + string(card.Node) + " is of project " + string(project) + ", not the card's")}, nil
	}
	now := named(d.checks(card, l.Executor, roster))
	for i, was := range named(l.PlacementChecks) {
		if was != now[i] {
			return held("placement "+was.name+" changed since the grant",
				"source_invalid_config", "executor "+string(l.Executor), types.OwnerOperator, nil)
		}
	}
	e, _ := d.Registry.Executor(l.Executor) // registered: the placement still holds
	image := func() (*Miss, error) {
		ref := "toolchain image"
		return held("the pinned image is not present at its digest", "wrong_owner", "image "+card.Image, types.OwnerNamedOwner, &ref)
	}
	credential := func() (*Miss, error) {
		return held("no credential for destination "+string(e.Destination.ID), "source_invalid_config",
			"destination "+string(e.Destination.ID), types.OwnerOperator, nil)
	}
	switch {
	case d.Images == nil:
		return image()
	case e.CredentialRef != nil && d.Credentials == nil:
		return credential()
	case !ready.observed || ready.executor != l.Executor || ready.destination != e.Destination.ID ||
		!sameRef(ready.credentialRef, e.CredentialRef) || ready.imageRef != card.Image:
		return &Miss{Reason: "the readiness was not observed for this lease's executor, destination, credential and image"}, nil
	case ready.image != nil:
		return image()
	case ready.credential != nil:
		return credential()
	}
	return nil, nil
}

func sameRef(a, b *types.Slug) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// reason fits a refusal's reason to lease-event's 1024 bytes.
func reason(s string) string {
	if len(s) <= 1024 {
		return s
	}
	return strings.ToValidUTF8(s[:1021], "") + "..."
}

// held is a miss with a registered node hold on signal.
func held(why, hold, signal string, owner types.Owner, ref *string) (*Miss, error) {
	id, err := ids.New("hld")
	if err != nil {
		return nil, err
	}
	return &Miss{Reason: reason(why), Hold: &types.NodeHold{ID: types.HoldID(id), Hold: hold, SignalInstance: signal,
		Owner: owner, OwnerRef: ref, Pages: []types.NodeHoldPagesItem{}}}, nil
}
