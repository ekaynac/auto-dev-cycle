// SPDX-License-Identifier: Apache-2.0

// Package dispatch is M1's one dispatch entry point, match(card, executor)
// (D51; docs/design/m1/00-slice-plan.md P8b; 01 §2.2 "Board and validated
// pull"): claim → reserve → launch readiness → fencing.
//
// Decisions:
//   - The card is a node already claimed (graph.Claim and ClaimReview make
//     the claim). Match checks that the lease's unit is that claimed node's
//     and of the card's project before anything is reserved (#226 entry
//     47), and the registry admits the destination before any reservation
//     (registry.Admits, #226 entry 39).
//   - Placement is recorded on the lease, never re-derived from memory: each
//     of the lease's placement checks names what it decided on (the data
//     policy's digest, the pinned executor's registry entry digest, the
//     image pin, the roster's digest). A blocked check places nothing: no
//     lease is recorded, the refusal goes back to the caller (P22a's
//     reconcile), so a failed match leaves no runnable assignment.
//   - Reservation is the lease's request and grant in one command
//     (lease.RequestGranted): capacity refuses the whole command when a
//     group lacks room, so no lease is left behind.
//   - The launch readiness check runs in the command that records
//     attempt.started (Launch). What lives outside the log (the image in
//     the host's storage; the gateway's credential, P11) is observed
//     before the command (Observe); the registry, roster and placement are
//     re-derived inside it and compared with what the lease recorded, so a
//     registry change between claim and start is a miss (13:106).
//   - A miss refuses the start and releases the lease in one command, with
//     a registered node hold naming the missing item: source_invalid_config
//     for a credential or a changed registry, wrong_owner for an image. No
//     attempt starts, so no rail allowance is spent (P17 counts attempts).
//     A node no longer claimed gets no hold: its own state says what is
//     next.
//   - Lease holds (#226 entry 4) are not written here: every hold P8b
//     raises is a node hold, which has its id and since.
//   - Inert (§1's gating table): nothing in adcd constructs a Dispatcher,
//     so match returns no assignment and no attempt starts until P22a
//     activates `launch`. P11's credential seam (Credentials) has no
//     implementation; without one, a destination that needs a credential
//     never launches.
//   - Only the first fallback step exists (fallback chains are M2); the
//     rails (paid authority, budget) are P17's, and their checks say so.
package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Card is a claimed node offered for placement, with what its project
// profile and template fix for it.
type Card struct {
	Node      types.NodeID
	Project   types.ProjectID
	Policy    types.ProjectProfileDataPolicy // the profile's data policy
	Image     string                         // the pinned toolchain image, name@sha256:hex
	Tokens    types.Amount                   // a direct API's reservation: the attempt's footprint (Q8)
	Footprint types.LeaseFootprint
	Priority  int64
	Deadline  types.Time
}

// Dispatcher places cards on registered executors and launches them.
type Dispatcher struct {
	Registry    registry.Registry
	Images      Images
	Credentials Credentials
}

// Refused is a match that placed nothing: the placement check that
// blocked it.
type Refused struct {
	Check string
	types.PlacementCheck
}

func (r *Refused) Error() string {
	return fmt.Sprintf("dispatch: placement %s: %s/%s: %s", r.Check, r.Outcome, r.Reason, r.Detail)
}

// Match places card on executor: it checks the node, then records the
// lease with its placement checks and grants it, reserving its group in the
// same command. A blocked check is a *Refused with no event.
func (d Dispatcher) Match(tx *eventstore.Tx, card Card, executor types.ExecutorID) ([]eventstore.Event, types.LeaseID, error) {
	unit := graph.UnitOf(card.Node)
	project, roster, err := graph.Units{}.Placement(tx, unit, d.Registry)
	if err != nil {
		return nil, "", err
	}
	if project != card.Project {
		return nil, "", fmt.Errorf("dispatch: node %s is of project %s, not the card's %s", card.Node, project, card.Project)
	}
	checks := d.checks(card, executor, roster)
	if name, c, ok := blocked(checks); ok {
		return nil, "", &Refused{Check: name, PlacementCheck: c}
	}
	e, _ := d.Registry.Executor(executor)
	res := types.LeaseReservationsItem{Group: e.Group, Amount: card.Tokens, Unit: types.CapacityUnitTokens}
	if e.Destination.Kind == types.RegistryDestinationKindLocalEndpoint { // one slot (Q8)
		res.Amount, res.Unit = 1, types.CapacityUnitSlots
	}
	id, err := ids.New("lea")
	if err != nil {
		return nil, "", err
	}
	l := types.Lease{ID: types.LeaseID(id), Unit: unit, Role: types.Name(e.Role), Executor: executor, State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{res}, Priority: card.Priority, Footprint: card.Footprint,
		Deadline: card.Deadline, PlacementChecks: checks}
	evs, err := lease.RequestGranted(tx, l)
	return evs, l.ID, err
}

// checks derives the placement of card on executor from the registry now,
// each check naming what it decided on.
func (d Dispatcher) checks(card Card, executor types.ExecutorID, roster []types.RosterMember) types.LeasePlacementChecks {
	pass := func(detail string) types.PlacementCheck {
		return types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Detail: detail}
	}
	fail := func(o types.Outcome, r vocab.Reason, detail string) types.PlacementCheck {
		return types.PlacementCheck{Outcome: o, Reason: r, Detail: detail}
	}
	e, ok := d.Registry.Executor(executor)
	c := types.LeasePlacementChecks{
		DataFloor:     pass("policy " + digest(card.Policy)),
		Destinations:  pass("executor " + string(executor) + " " + digest(e)),
		Capability:    pass("image " + card.Image),
		Independence:  pass("roster " + digest(roster)),
		PaidAuthority: pass("not checked here: the rails are P17's"),
		Budget:        pass("not checked here: the rails are P17's"),
		FallbackScope: pass("the first step; fallback chains are M2's"),
	}
	if err := d.Registry.Admits(card.Policy, executor); err != nil {
		c.DataFloor = fail(types.OutcomeFailed, vocab.ReasonRoutingViolation, err.Error())
	}
	if !ok {
		c.Destinations = fail(types.OutcomeFailed, vocab.ReasonRoutingViolation, "the executor is not registered")
	}
	if card.Image == "" {
		c.Capability = fail(types.OutcomeCouldNotRun, vocab.ReasonPreconditionFailed, "no pinned image")
	}
	member := slices.ContainsFunc(roster, func(m types.RosterMember) bool { return m.Executor == executor })
	if (roster != nil) != (e.Role == types.RegistryExecutorRoleReviewer) || (roster != nil && !member) {
		c.Independence = fail(types.OutcomeFailed, vocab.ReasonFallbackScope,
			"a rostered node places only on its pinned roster member, a reviewer; any other node on a builder")
	}
	return c
}

// blocked is the first check that blocks the grant.
func blocked(c types.LeasePlacementChecks) (string, types.PlacementCheck, bool) {
	for _, n := range named(c) {
		if n.check.Outcome != types.OutcomeEmpty {
			return n.name, n.check, true
		}
	}
	return "", types.PlacementCheck{}, false
}

type namedCheck struct {
	name  string
	check types.PlacementCheck
}

func named(c types.LeasePlacementChecks) []namedCheck {
	return []namedCheck{{"data_floor", c.DataFloor}, {"destinations", c.Destinations}, {"capability", c.Capability},
		{"independence", c.Independence}, {"paid_authority", c.PaidAuthority}, {"budget", c.Budget}, {"fallback_scope", c.FallbackScope}}
}

// digest is the sha256 of v's JSON.
func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // generated types always marshal
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
