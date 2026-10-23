// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
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
// the named credential for a destination. gateway.Gateway implements it
// (P11).
type Credentials interface {
	Holds(ctx context.Context, destination, credential types.Slug) (bool, error)
}

// Readiness is what the launch boundary observed outside the writer, bound
// to what it observed for: the executor, its destination and credential
// reference, and the image. Only Observe makes one; the zero value was
// observed for nothing, so it is never ready. image and credential are nil
// when the pinned image is present at its digest, and when the gateway
// holds the destination's credential (or it needs none).
//
// Every binding is a value snapshot taken once, never a pointer into the
// registry, so the credential the gateway was asked about is the one the
// launch compares (Astra, #246).
type Readiness struct {
	observed          bool
	executor          types.ExecutorID
	destination       types.Slug
	hasCredential     bool
	credentialRef     types.Slug
	imageRef          string
	image, credential error
}

var errNoGateway = errors.New("dispatch: no gateway confirms credentials (P11)")

// ReadinessBound bounds each readiness check made in the writer's command
// (Configured's credential; daemon's image check for a hold), as Observe's
// are outside it (Opus #400 M2).
const ReadinessBound = 5 * time.Second

// Observe probes, outside the writer transaction, what the launch of card
// on executor needs from the host and the gateway.
func (d Dispatcher) Observe(ctx context.Context, card Card, executor types.ExecutorID) Readiness {
	r := Readiness{observed: true, executor: executor, imageRef: card.Image}
	if d.Images == nil {
		r.image = sandbox.ErrImage
	} else if _, err := d.Images.Resolve(ctx, card.Image); err != nil {
		r.image = err
	}
	e, _ := d.Registry.Executor(executor)
	r.destination = e.Destination.ID
	if e.CredentialRef != nil {
		r.hasCredential, r.credentialRef = true, *e.CredentialRef
	}
	r.credential = d.credentialReady(ctx, executor)
	return r
}

// credentialReady is why executor's launch lacks its credential, nil when
// it needs none or the gateway holds it: the one answer the launch's
// readiness (Observe) and the placement holds' clear (Configured) take
// (Opus #400 M2). An unregistered executor, no gateway and the gateway's
// error are all not ready.
func (d Dispatcher) credentialReady(ctx context.Context, executor types.ExecutorID) error {
	e, ok := d.Registry.Executor(executor)
	switch {
	case !ok:
		return fmt.Errorf("dispatch: executor %s is not registered", executor)
	case e.CredentialRef == nil: // a local endpoint has none
		return nil
	case d.Credentials == nil:
		return errNoGateway
	}
	held, err := d.Credentials.Holds(ctx, e.Destination.ID, *e.CredentialRef)
	if err == nil && !held {
		err = fmt.Errorf("dispatch: the gateway holds no credential %s for destination %s", *e.CredentialRef, e.Destination.ID)
	}
	return err
}

// boundTo: r was observed for exactly e's destination and credential
// reference, and for image.
func (r Readiness) boundTo(executor types.ExecutorID, e types.RegistryExecutor, image string) bool {
	if !r.observed || r.executor != executor || r.imageRef != image {
		return false
	}
	if r.destination != e.Destination.ID {
		return false
	}
	return r.hasCredential == (e.CredentialRef != nil) && (!r.hasCredential || r.credentialRef == *e.CredentialRef)
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
		return rosterMiss(ni)
	case errors.As(err, &ns):
		return &Miss{Reason: reason("the node is no longer claimed: " + ns.Detail)}, nil
	case err != nil:
		return nil, err
	case project != card.Project:
		return &Miss{Reason: reason("node " + string(card.Node) + " is of project " + string(project) + ", not the card's")}, nil
	}
	now := named(d.checks(card, l.Executor, roster))
	for i, was := range named(l.PlacementChecks) {
		// The budget check was the USD rails' at the grant, with the spend then
		// (P17b); the grant reserved it, so the launch does not re-read it.
		if was != now[i] && was.name != "budget" {
			return held("placement "+was.name+" changed since the grant", // Configured re-derives it (#226 entry 156)
				"source_invalid_config", Placement{"executor", string(l.Executor)}.Signal(), types.OwnerOperator, nil)
		}
	}
	e, _ := d.Registry.Executor(l.Executor) // registered: the placement still holds
	image := func() (*Miss, error) {
		ref := "toolchain image"
		return held("the pinned image is not present at its digest", "wrong_owner", imageSignal(card.Image), types.OwnerNamedOwner, &ref)
	}
	credential := func() (*Miss, error) {
		return held("no credential for destination "+string(e.Destination.ID), "source_invalid_config", // Configured checks it (#226 entry 156)
			Placement{"destination", string(e.Destination.ID)}.Signal(), types.OwnerOperator, nil)
	}
	switch {
	case d.Images == nil:
		return image()
	case e.CredentialRef != nil && d.Credentials == nil:
		return credential()
	case !ready.boundTo(l.Executor, e, card.Image):
		return &Miss{Reason: "the readiness was not observed for this lease's executor, destination, credential and image"}, nil
	case ready.image != nil:
		return image()
	case ready.credential != nil:
		return credential()
	}
	return d.attemptRail(tx, card.Node)
}

// attemptRail is a miss holding node on its task's attempt rail when the
// attempts started on its round (graph.RoundAttempts) reach it (P17a).
func (d Dispatcher) attemptRail(tx *eventstore.Tx, node types.NodeID) (*Miss, error) {
	task, err := graph.TaskOf(tx, node)
	if err != nil {
		return nil, err
	}
	caps := graph.DefaultCaps
	if d.Rails != nil {
		if caps, err = d.Rails(tx, task); err != nil {
			return nil, err
		}
	}
	started, err := graph.RoundAttempts(tx, node)
	if err != nil {
		return nil, err
	}
	if started < caps.AttemptsPerRound {
		return nil, nil
	}
	h, err := graph.RailHold(graph.RailAttempts)
	return &Miss{Reason: reason(fmt.Sprintf("%d attempts started on the round, its rail %d", started, caps.AttemptsPerRound)), Hold: &h}, err
}

// maxSignal is records/node-hold's bound on a signal instance.
const maxSignal = 1024

// imagePrefix starts imageSignal's signals.
const imagePrefix = "image "

// imageSignal names image in a hold's signal: "image <ref>", or, for a
// reference too long for the signal (the pinned-image schema has no
// bound), "image sha256:<hex of the ref>", so a miss always fits and
// releases its lease.
func imageSignal(image string) string {
	if s := imagePrefix + image; len(s) <= maxSignal {
		return s
	}
	sum := sha256.Sum256([]byte(image))
	return imagePrefix + "sha256:" + hex.EncodeToString(sum[:])
}

// IsImageSignal: signal is exactly one imageSignal makes (a launch's
// wrong_owner hold on its pinned image, which the reconcile loop clears
// once the project's pinned image resolves; #226 entry 156): "image " and
// a pinned reference (sandbox.Pinned: the profile schema's PinnedImage,
// the rule every pin was admitted by; Opus #425 L2, Mythos L1) within
// maxSignal, or the hashed form's digest (contract.IsDigest). One rule with
// imageSignal and the profile.
func IsImageSignal(signal string) bool {
	ref, ok := strings.CutPrefix(signal, imagePrefix)
	return ok && (sandbox.Pinned(ref) || contract.IsDigest(ref)) && imageSignal(ref) == signal
}

// reason fits a refusal's reason to lease-event's 1024 bytes.
func reason(s string) string {
	if len(s) <= 1024 {
		return s
	}
	return strings.ToValidUTF8(s[:1021], "") + "..."
}

// held is a miss with a registered node hold on signal.
// rosterMiss is the miss of a node whose roster member the registry no
// longer derives (ni): reviewer_unavailable on its registry entry, as the
// evaluator holds it; Match's refusal of it too (Astra chunk 3 H1).
func rosterMiss(ni graph.ErrNotIndependent) (*Miss, error) {
	return held("the registry no longer gives roster member "+string(ni.Executor)+" its family",
		"reviewer_unavailable", ni.SignalInstance(), types.OwnerSystem, nil) // as graph.HoldNotIndependent's (Opus 22a-6c re-check)
}

func held(why, hold, signal string, owner types.Owner, ref *string) (*Miss, error) {
	id, err := ids.New("hld")
	if err != nil {
		return nil, err
	}
	return &Miss{Reason: reason(why), Hold: &types.NodeHold{ID: types.HoldID(id), Hold: hold, SignalInstance: signal,
		Owner: owner, OwnerRef: ref, Pages: []types.NodeHoldPagesItem{}}}, nil
}
