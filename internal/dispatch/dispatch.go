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
//   - The launch fails closed: an observation (Readiness) is opaque, bound
//     to the executor, destination, credential reference and image it was
//     made for, and its zero value is never ready; with no image store, or
//     no gateway for a credentialed destination, nothing launches.
//   - A miss refuses the start and releases the lease in one command, with
//     a registered node hold naming the missing item: source_invalid_config
//     for a credential or a changed placement, reviewer_unavailable for a
//     roster the registry no longer gives (as graph.Evaluate holds it),
//     wrong_owner for an image. No attempt starts, so no rail allowance is
//     spent (P17 counts attempts). Each clears (#226 entry 156): the two
//     source_invalid_config holds are placement holds (Placement),
//     cleared once Configured admits the node again; the image hold
//     (IsImageSignal) once the pinned image resolves; reviewer_unavailable
//     as graph.Evaluate's. A node no longer claimed or of another
//     project, or a readiness observed for something else, gets no hold.
//   - A unit already placed (a live lease, or a running attempt) is not
//     matched again, nor one whose last lease has not settled (P21).
//   - Lease holds (#226 entry 4) are not written here: every hold P8b
//     raises is a node hold, which has its id and since.
//   - Inert (§1's gating table): adcd's reconcile loop matches nothing
//     and starts no attempt until P22a activates `launch`. P11's gateway
//     (internal/gateway) is adcd's credential seam (Credentials, P22a-5c-2a);
//     without one, a destination that needs a credential never launches.
//   - Only the first fallback step exists (fallback chains are M2). The
//     budget check is the USD rails' (P17b, usd.go); paid authority is not
//     checked here, as its check says.
package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Node    types.NodeID
	Project types.ProjectID
	Policy  types.ProjectProfileDataPolicy // the profile's data policy
	// PolicyDigest is that policy's digest over its recorded bytes
	// (authority.PolicyDigest), as a plan brief's restriction names it: the
	// data_floor check's (Mythos 22a-5c-3 L1).
	PolicyDigest types.Digest
	Image        string // the pinned toolchain image, name@sha256:hex
	Priority     int64
	Deadline     types.Time
}

// Dispatcher places cards on registered executors and launches them.
type Dispatcher struct {
	Registry    registry.Registry
	Images      Images
	Credentials Credentials
	Rails       graph.Rails // each task's rails (P17a, P17b); nil: graph.DefaultCaps, which has no USD rail, so a priced executor is refused
	// Floors are each role's request floor (adcd's callFloors): a priced
	// lease is never shrunk below one call's (Floors.Call). Nil: one turn's
	// context_tokens alone.
	Floors registry.Floors
}

// Refused is a match that placed nothing: the placement check that
// blocked it, and, for a USD rail, the budget_rail hold the caller raises
// on the node (graph.Raise) in its own command (P17b).
type Refused struct {
	Check string
	types.PlacementCheck
	Hold *types.NodeHold
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
	if ni := (graph.ErrNotIndependent{}); errors.As(err, &ni) { // a claimed review whose roster a restart's registry no longer gives: held, as at launch (Astra chunk 3 H1)
		m, herr := rosterMiss(ni)
		if herr != nil {
			return nil, "", herr
		}
		return nil, "", errors.Join(err, &Refused{Check: "roster", PlacementCheck: types.PlacementCheck{Outcome: types.OutcomeCouldNotRun,
			Reason: vocab.ReasonReviewerNotIndependent, Detail: ni.Error()}, Hold: m.Hold})
	}
	if err != nil {
		return nil, "", err
	}
	if project != card.Project {
		return nil, "", fmt.Errorf("dispatch: node %s is of project %s, not the card's %s", card.Node, project, card.Project)
	}
	if err := placed(tx, unit); err != nil {
		return nil, "", err
	}
	checks := d.checks(card, executor, roster)
	if name, c, ok := blocked(checks); ok {
		_, registered := d.Registry.Executor(executor)
		h, err := heldOn(name, card, executor, registered)
		return nil, "", errors.Join(err, &Refused{Check: name, PlacementCheck: c, Hold: h})
	}
	e, _ := d.Registry.Executor(executor)
	res, footprint, b, refused, err := d.reservation(tx, card, executor, e)
	if err != nil {
		return nil, "", err
	}
	if refused != nil {
		return nil, "", refused
	}
	budget, hold, err := d.usdRail(tx, card, executor, res, b)
	if err != nil {
		return nil, "", err
	}
	if checks.Budget = budget; budget.Outcome != types.OutcomeEmpty {
		return nil, "", &Refused{Check: "budget", PlacementCheck: budget, Hold: hold}
	}
	id, err := ids.New("lea")
	if err != nil {
		return nil, "", err
	}
	l := types.Lease{ID: types.LeaseID(id), Unit: unit, Role: types.Name(e.Role), Executor: executor, Price: e.Price, State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{res}, Priority: card.Priority, Footprint: footprint,
		Deadline: card.Deadline, PlacementChecks: checks}
	evs, err := lease.RequestGranted(tx, l)
	return evs, l.ID, err
}

// placed refuses a unit already placed (occupied), so one node is matched
// once.
func placed(tx *eventstore.Tx, unit types.UnitID) error {
	why, err := occupied(tx, unit)
	if err == nil && why != "" {
		err = errors.New("dispatch: " + why)
	}
	return err
}

// Unplaced: nothing is placed on unit (occupied), so Match may place it:
// what the reconcile loop reads before it matches a node.
func Unplaced(tx *eventstore.Tx, unit types.UnitID) (bool, error) {
	why, err := occupied(tx, unit)
	return err == nil && why == "", err
}

// occupied is why unit is placed: a live lease (requested, waiting,
// granted or active), a running attempt, or a last lease not yet settled;
// "" when none.
func occupied(tx *eventstore.Tx, unit types.UnitID) (string, error) {
	if id, live, err := lease.LiveOn(tx, unit); err != nil || live {
		return fmt.Sprintf("unit %s already has live lease %s", unit, id), err
	}
	cur, ok, err := lease.CurrentAttempt(tx, unit)
	if err != nil || !ok {
		return "", err
	}
	a, _, _, err := lease.AttemptOf(tx, cur)
	if err != nil {
		return "", err
	}
	if a.Status == types.AttemptStatusRunning {
		return fmt.Sprintf("unit %s's attempt %s is running", unit, cur), nil
	}
	// A replacement waits until its predecessor's lease settled (X3: step 4
	// before step 5), so it is sized against what that settlement returned.
	l, _, ok, err := lease.LeaseOf(tx, a.Lease)
	if err == nil && (!ok || !lease.Ended(l.State)) {
		return fmt.Sprintf("unit %s's last lease %s has not settled", unit, a.Lease), nil
	}
	return "", err
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
		DataFloor:     pass("policy " + string(card.PolicyDigest)),
		Destinations:  pass("executor " + string(executor) + " " + digest(e)),
		Capability:    pass(imageSignal(card.Image)), // bounded: the detail holds 4096
		Independence:  pass("roster " + digest(roster)),
		PaidAuthority: pass("not checked here: the rails are P17's"),
		Budget:        pass("the USD rails are checked with the reservation (Match); the attempt rail at launch"),
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

// heldOn is the hold a blocked placement check raises on card's node
// (Opus 22a-5c-1 M1): each is configuration, which no later match passes
// until it changes, so the node is held source_invalid_config, the
// operator's, naming what to change: the project's profile for its data
// policy (data_floor) and its pinned image (capability); the executor for
// its registration (destinations, and any check of an executor the
// registry does not name) and its role (independence).
func heldOn(check string, card Card, executor types.ExecutorID, registered bool) (*types.NodeHold, error) {
	signal := Placement{"executor", string(executor)}
	if registered && (check == "data_floor" || check == "capability") {
		signal = Placement{"profile", string(card.Project)}
	}
	r, err := configRefusal(check, signal, "")
	if err != nil {
		return nil, err
	}
	return r.Hold, nil
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

// digest is the sha256 of v's JSON as Go's encoding/json writes it (not a
// canonical form): stable within one build, compared only by Launch.
func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // generated types always marshal
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
