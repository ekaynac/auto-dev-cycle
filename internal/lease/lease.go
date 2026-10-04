// SPDX-License-Identifier: Apache-2.0

// Package lease holds the lease and attempt state machines and the fencing
// rule (docs/design/m0/05): every transition is an event, terminal states are
// immutable, publication closes atomically with the event that leaves
// running, and an output lands only from the current attempt's exact token
// while its publication is open. Functions return new records and never
// change their arguments; the event store writes the events.
package lease

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// ErrTransition: the record is not in a state that allows the event.
var ErrTransition = errors.New("lease: transition not allowed")

// The lease event kinds of note 05's two tables.
const (
	Requested       = "lease.requested"
	Waiting         = "lease.waiting"
	Granted         = "lease.granted"
	AttemptStarted  = "attempt.started"
	Expired         = "lease.expired"
	Completed       = "lease.completed"
	Revoked         = "lease.revoked"
	Preempted       = "lease.preempted"
	Settled         = "lease.settled"
	Released        = "lease.released"
	RestoreStarted  = "lease.restore_started"
	RestoreVerified = "lease.restore_verified"
	RestoreFailed   = "lease.restore_failed"
)

// LeaseEvent is one lease transition with the data it carries.
type LeaseEvent struct {
	Kind       string
	Hold       *types.LeaseHoldValue       // lease.waiting
	Attempt    *types.AttemptID            // attempt.started
	Receipt    *types.ReceiptID            // lease.restore_verified
	Settlement *types.LeaseSettlementValue // lease.settled
}

type leaseStep struct {
	from  types.LeaseState
	event string
}

// leaseTable is note 05's lease state machine. Evidence-target leases settle
// only through a verified restore (restoreTable).
var leaseTable = map[leaseStep]types.LeaseState{
	{types.LeaseStateRequested, Waiting}:      types.LeaseStateWaiting,
	{types.LeaseStateWaiting, Requested}:      types.LeaseStateRequested,
	{types.LeaseStateRequested, Granted}:      types.LeaseStateGranted,
	{types.LeaseStateGranted, AttemptStarted}: types.LeaseStateActive,
	{types.LeaseStateGranted, Expired}:        types.LeaseStateReleasing,
	{types.LeaseStateActive, Completed}:       types.LeaseStateSettling,
	{types.LeaseStateActive, Revoked}:         types.LeaseStateSettling,
	{types.LeaseStateActive, Preempted}:       types.LeaseStateSettling,
	{types.LeaseStateActive, Expired}:         types.LeaseStateSettling,
	{types.LeaseStateReleasing, Released}:     types.LeaseStateReleased,
}

var plainSettle = map[leaseStep]types.LeaseState{
	{types.LeaseStateSettling, Settled}: types.LeaseStateSettled,
}

var restoreTable = map[leaseStep]types.LeaseState{
	{types.LeaseStateSettling, RestoreStarted}:       types.LeaseStateRestoring,
	{types.LeaseStateRestoring, RestoreVerified}:     types.LeaseStateSettlingVerified,
	{types.LeaseStateRestoring, RestoreFailed}:       types.LeaseStateRestoreFailed,
	{types.LeaseStateRestoreFailed, RestoreVerified}: types.LeaseStateSettlingVerified,
	{types.LeaseStateSettlingVerified, Settled}:      types.LeaseStateSettled,
}

// ApplyLease returns the lease after event e. evidenceTarget says whether the
// lease holds an evidence_target group, which owes a verified restore before
// it settles; holds is the hold registry.
func ApplyLease(l types.Lease, e LeaseEvent, evidenceTarget bool, holds map[string]types.Hold) (types.Lease, error) {
	step := leaseStep{l.State, e.Kind}
	to, ok := leaseTable[step]
	if !ok {
		settle := plainSettle
		if evidenceTarget {
			settle = restoreTable
		}
		if to, ok = settle[step]; !ok {
			return types.Lease{}, fmt.Errorf("%w: %s on a %s lease", ErrTransition, e.Kind, l.State)
		}
	}
	n := l
	n.State, n.Hold = to, nil
	switch e.Kind {
	case Waiting:
		if e.Hold == nil || e.Hold.SignalInstance == "" {
			return types.Lease{}, errors.New("lease: a wait names its hold and signal instance")
		}
		if _, ok := holds[e.Hold.Hold]; !ok {
			return types.Lease{}, fmt.Errorf("lease: hold %q is not registered", e.Hold.Hold)
		}
		h := *e.Hold
		n.Hold = &h
	case AttemptStarted:
		if e.Attempt == nil {
			return types.Lease{}, errors.New("lease: attempt.started names the attempt")
		}
		a := *e.Attempt
		n.Attempt = &a
	case RestoreVerified:
		if e.Receipt == nil {
			return types.Lease{}, errors.New("lease: a verified restore names its receipt")
		}
		r := *e.Receipt
		n.RestoreReceipt = &r
	case Settled:
		s, err := settlement(l, e.Settlement)
		if err != nil {
			return types.Lease{}, err
		}
		n.Settlement = &s
	}
	return n, nil
}

// settlement checks and copies a lease.settled payload: settled calls and
// uncertain liabilities are disjoint, and only a lease with a verified
// restore publishes a result (the publication guard).
func settlement(l types.Lease, s *types.LeaseSettlementValue) (types.LeaseSettlementValue, error) {
	if s == nil {
		return types.LeaseSettlementValue{}, errors.New("lease: lease.settled carries its settlement")
	}
	if s.Published != nil && l.RestoreReceipt == nil {
		return types.LeaseSettlementValue{}, errors.New("lease: only a lease with a verified restore publishes a result")
	}
	settled := map[types.CallID]bool{}
	for _, c := range s.Calls {
		settled[c] = true
	}
	for _, c := range s.Liabilities {
		if settled[c] {
			return types.LeaseSettlementValue{}, fmt.Errorf("lease: call %s is both settled and an uncertain liability", c)
		}
	}
	out := types.LeaseSettlementValue{Calls: slices.Clone(s.Calls), Liabilities: slices.Clone(s.Liabilities)}
	if s.Published != nil {
		p := *s.Published
		out.Published = &p
	}
	return out, nil
}
