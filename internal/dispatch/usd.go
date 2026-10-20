// SPDX-License-Identifier: Apache-2.0

package dispatch

// The USD rails (M1 P17b; I16; slice plan §6 L3 and the USD clause). At
// lease admission, Match places a lease only when, with its reservation
// priced, the task's spend stays within per_task_usd and its project's
// spend on the UTC day of the grant within per_day_usd.
//   - A lease's spend, priced at the executor's price declared when it was
//     placed (Lease.Price; a retired or repriced executor changes nothing):
//     a lease with no call costs its reservation while it is live, nothing
//     once ended. Its call costs, once settled, its usage priced from its
//     tokens (ledger.PriceMicros: never the reported cost); unsettled, its
//     conservative figure: the liability its settlement kept
//     (capacity.LiabilitiesOf), or, while the lease is live, the larger of
//     the reservation and the reports and the gateway's counts
//     (ledger.StandingOf), at the higher token rate. So a hostile zero
//     final still counts (#226 entry 69).
//   - Pricing saturates, as the liability ledger does: a standing the
//     reports cannot be summed into (ledger.StandingOf's saturated figure),
//     or any price past the contract's integers, costs past every rail, so
//     it refuses with the hold rather than wedging placement. A standing
//     flagged inconsistent with a figure (a final under the gateway's
//     counts, mixed currencies) costs its conservative figure: tokens are
//     priced at the declared price whatever the reported currency.
//   - A lease of more than one group or call is not counted: it is an
//     error (Match places one group, and a lease binds one call).
//   - An executor with no price (a local or free route) costs nothing, and
//     a lease that reserves nothing priced is not checked.
//   - A liability stays on its lease's grant day; a new day does not
//     release it, it only starts a new day's rail.
//   - At a rail, the placement's budget check fails, could_not_run/
//     budget_rail_reached, naming the budget_rail hold (rail:per_task_usd
//     or rail:per_day_usd) the caller raises on the node (Refused.Hold).
//     per_task_usd is raised for the task by task.raise_rail; per_day_usd
//     only by a profile change, or the next day.
//   - Fail closed: no rails (Dispatcher.Rails nil), or a USD rail no
//     profile set (graph.NoUSDRail, DefaultCaps'), refuses a priced
//     executor (could_not_run/precondition_failed). adcd always passes its
//     Rails (P22a).

import (
	"fmt"
	"math"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// unaffordable is a cost past every rail: a saturated price.
const unaffordable = math.MaxInt64

// add is a+b, saturating at unaffordable.
func add(a, b int64) int64 {
	if a > unaffordable-b {
		return unaffordable
	}
	return a + b
}

// priced is a price, saturated: one past the contract's integers, or
// refused, is unaffordable.
func priced(micros int64, err error) int64 {
	if err != nil {
		return unaffordable
	}
	return micros
}

// usdRail is the budget check of placing res for card's node on executor:
// a pass, or the rail it reaches and that rail's hold.
func (d Dispatcher) usdRail(tx *eventstore.Tx, card Card, executor types.ExecutorID, res types.LeaseReservationsItem) (types.PlacementCheck, *types.NodeHold, error) {
	pass := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	task, err := graph.TaskOf(tx, card.Node)
	if err != nil {
		return pass, nil, err
	}
	e, _ := d.Registry.Executor(executor)
	next := reserved(res, e.Price)
	if next == 0 { // nothing priced: an unpriced executor, or slots
		pass.Detail = "the lease reserves nothing priced"
		return pass, nil, nil
	}
	caps := graph.DefaultCaps
	if d.Rails != nil {
		if caps, err = d.Rails(tx, task); err != nil {
			return pass, nil, err
		}
	}
	if caps.PerTaskMicros < 0 || caps.PerDayMicros < 0 {
		return types.PlacementCheck{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonPreconditionFailed,
			Detail: "no USD rails: a priced executor is placed only under them"}, nil, nil
	}
	day := tx.Now().UTC().Format(time.DateOnly)
	perTask, perDay, err := spent(tx, task, card.Project, day)
	if err != nil {
		return pass, nil, err
	}
	for _, r := range []struct {
		rail       string
		cap, spent int64
	}{{graph.RailTaskUSD, caps.PerTaskMicros, perTask}, {graph.RailDayUSD, caps.PerDayMicros, perDay}} {
		if r.cap < 0 || add(r.spent, next) <= r.cap {
			continue
		}
		h, err := graph.RailHold(r.rail)
		return types.PlacementCheck{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonBudgetRailReached,
			Detail: fmt.Sprintf("%s: %d spent and %d reserved, USD micros, past the rail's %d", r.rail, r.spent, next, r.cap)}, &h, err
	}
	pass.Detail = fmt.Sprintf("USD micros: task %d of %d, day %s %d of %d, with this lease's %d", perTask, caps.PerTaskMicros, day, perDay, caps.PerDayMicros, next)
	return pass, nil, nil
}

// spent is, in USD micros, saturating, what task's leases cost, and what
// project's leases granted on day (UTC) cost.
func spent(tx *eventstore.Tx, task types.TaskID, project types.ProjectID, day string) (perTask, perDay int64, err error) {
	ids, err := tx.Keys(lease.LeaseProjection, "")
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		l, _, ok, err := lease.LeaseOf(tx, types.LeaseID(id))
		if err == nil && !ok {
			err = fmt.Errorf("dispatch: lease %s is listed and has no record", id)
		}
		if err != nil {
			return 0, 0, err
		}
		p, t, owned, err := graph.Units{}.ProjectTask(tx, l.Unit)
		if err != nil {
			return 0, 0, err
		}
		if !owned || p != project {
			continue
		}
		at, granted, err := lease.GrantedAt(tx, l.ID)
		if err != nil {
			return 0, 0, err
		}
		onDay := granted && at.UTC().Format(time.DateOnly) == day
		if t != task && !onDay {
			continue
		}
		c, err := cost(tx, l)
		if err != nil {
			return 0, 0, err
		}
		if t == task {
			perTask = add(perTask, c)
		}
		if onDay {
			perDay = add(perDay, c)
		}
	}
	return perTask, perDay, nil
}

// cost is lease l's spend, in USD micros, saturating.
func cost(tx *eventstore.Tx, l types.Lease) (int64, error) {
	if l.Price == nil {
		return 0, nil
	}
	if len(l.Reservations) > 1 {
		return 0, fmt.Errorf("dispatch: lease %s reserves %d groups; the USD rails count leases of one", l.ID, len(l.Reservations))
	}
	calls, err := ledger.BoundTo(tx, l.ID)
	if err != nil {
		return 0, err
	}
	if len(calls) > 1 {
		return 0, fmt.Errorf("dispatch: lease %s binds %d calls; the USD rails count leases of one", l.ID, len(calls))
	}
	held := int64(0) // the reservation's tokens
	if len(l.Reservations) == 1 && l.Reservations[0].Unit == types.CapacityUnitTokens {
		held = int64(l.Reservations[0].Amount)
	}
	live := !lease.Ended(l.State)
	if len(calls) == 0 {
		if !live {
			return 0, nil
		}
		return priced(ledger.PriceTokens(held, *l.Price)), nil
	}
	ms, err := ledger.Measurements(tx, calls[0])
	if err != nil {
		return 0, err
	}
	s := ledger.StandingOf(ms, calls[0])
	switch {
	case s.Settled && s.Usage != nil:
		return priced(ledger.PriceMicros(*s.Usage, *l.Price)), nil
	case mutation.Is(mutation.USDRailSettledOnly):
		return 0, nil
	case live:
		tokens := held
		if s.Usage != nil {
			tokens = max(tokens, add(add(s.Usage.In, s.Usage.Out), add(s.Usage.CacheRead, s.Usage.CacheWrite)))
		}
		return priced(ledger.PriceTokens(tokens, *l.Price)), nil
	}
	owed, _, err := capacity.LiabilitiesOf(tx, l.ID, groupsOf(l)) // an inconsistent liability's amount is already its saturated figure
	if err != nil {
		return 0, err
	}
	var c int64
	for _, r := range l.Reservations { // one: the lease's liability, taken once
		if r.Unit == types.CapacityUnitTokens {
			c = add(c, priced(ledger.PriceTokens(owed[r.Group], *l.Price)))
		}
	}
	return c, nil
}

// reserved is res priced at price, the higher token rate, saturating;
// nothing on a route with no price, or for slots.
func reserved(res types.LeaseReservationsItem, price *types.RegistryPrice) int64 {
	if price == nil || res.Unit != types.CapacityUnitTokens {
		return 0
	}
	return priced(ledger.PriceTokens(int64(res.Amount), *price))
}

// groupsOf are l's reserved groups.
func groupsOf(l types.Lease) []types.ResourceGroupID {
	out := make([]types.ResourceGroupID, 0, len(l.Reservations))
	for _, r := range l.Reservations {
		out = append(out, r.Group)
	}
	return out
}
