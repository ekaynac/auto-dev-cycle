// SPDX-License-Identifier: Apache-2.0

package dispatch

// The USD rails (M1 P17b; I16; slice plan §6 L3 and the USD clause). At
// lease admission, Match places a lease only when, with its reservation
// priced, the task's spend stays within per_task_usd and its project's
// spend on the UTC day of the grant within per_day_usd.
//   - A lease's spend: a lease with no call costs its reservation while it
//     is live, nothing once ended. A call bound to it costs, once settled,
//     its usage priced at the executor's declared price (ledger.PriceMicros:
//     never the reported cost); unsettled, its conservative figure: the
//     liability its settlement kept (capacity.LiabilitiesOf), or, while the
//     lease is live, the larger of the reservation and the reports and the
//     gateway's counts (ledger.StandingOf), at the higher token rate. So a
//     hostile zero final still counts (#226 entry 69).
//   - An executor with no price (a local or free route) costs nothing.
//   - A liability stays on its lease's grant day; a new day does not
//     release it, it only starts a new day's rail.
//   - At a rail, the placement's budget check fails, could_not_run/
//     budget_rail_reached, naming the budget_rail hold (rail:per_task_usd
//     or rail:per_day_usd) the caller raises on the node (Refused.Hold).
//     per_task_usd is raised for the task by task.raise_rail; per_day_usd
//     only by a profile change, or the next day.
//   - No rails (Dispatcher.Rails nil), or a USD rail no profile set: not
//     checked. adcd always passes its Rails (P22a).

import (
	"fmt"
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

// usdRail is the budget check of placing res for card's node on executor:
// a pass, or the rail it reaches and that rail's hold.
func (d Dispatcher) usdRail(tx *eventstore.Tx, card Card, executor types.ExecutorID, res types.LeaseReservationsItem) (types.PlacementCheck, *types.NodeHold, error) {
	pass := types.PlacementCheck{Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean}
	task, err := graph.TaskOf(tx, card.Node)
	if err != nil {
		return pass, nil, err
	}
	if d.Rails == nil {
		pass.Detail = "no rails: the USD rails are not checked"
		return pass, nil, nil
	}
	caps, err := d.Rails(tx, task)
	if err != nil {
		return pass, nil, err
	}
	e, _ := d.Registry.Executor(executor)
	next, err := reserved(res, e.Price)
	if err != nil {
		return pass, nil, err
	}
	day := tx.Now().UTC().Format(time.DateOnly)
	perTask, perDay, err := d.spent(tx, task, card.Project, day)
	if err != nil {
		return pass, nil, err
	}
	for _, r := range []struct {
		rail       string
		cap, spent int64
	}{{graph.RailTaskUSD, caps.PerTaskMicros, perTask}, {graph.RailDayUSD, caps.PerDayMicros, perDay}} {
		if r.cap < 0 || r.spent+next <= r.cap {
			continue
		}
		h, err := graph.RailHold(r.rail)
		return types.PlacementCheck{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonBudgetRailReached,
			Detail: fmt.Sprintf("%s: %d spent and %d reserved, USD micros, past the rail's %d", r.rail, r.spent, next, r.cap)}, &h, err
	}
	pass.Detail = fmt.Sprintf("USD micros: task %d of %d, day %s %d of %d, with this lease's %d", perTask, caps.PerTaskMicros, day, perDay, caps.PerDayMicros, next)
	return pass, nil, nil
}

// spent is, in USD micros, what task's leases cost, and what project's
// leases granted on day (UTC) cost.
func (d Dispatcher) spent(tx *eventstore.Tx, task types.TaskID, project types.ProjectID, day string) (perTask, perDay int64, err error) {
	ids, err := tx.Keys(lease.LeaseProjection, "")
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		l, _, ok, err := lease.LeaseOf(tx, types.LeaseID(id))
		if err != nil || !ok {
			return 0, 0, err
		}
		t, p, owned, err := graph.UnitOwner(tx, l.Unit)
		if err != nil || !owned || p != project {
			if err != nil {
				return 0, 0, err
			}
			continue
		}
		at, granted, err := lease.GrantedAt(tx, l.ID)
		if err != nil {
			return 0, 0, err
		}
		onDay := granted && at.UTC().Format(time.DateOnly) == day
		if t != task && (!onDay || mutation.Is(mutation.DayRailIgnoresOtherTasks)) {
			continue
		}
		cost, err := d.cost(tx, l)
		if err != nil {
			return 0, 0, err
		}
		if t == task {
			perTask += cost
		}
		if onDay {
			perDay += cost
		}
	}
	return perTask, perDay, nil
}

// cost is lease l's spend, in USD micros.
func (d Dispatcher) cost(tx *eventstore.Tx, l types.Lease) (int64, error) {
	e, ok := d.Registry.Executor(l.Executor)
	if !ok {
		return 0, fmt.Errorf("dispatch: lease %s's executor %s is not registered", l.ID, l.Executor)
	}
	if e.Price == nil {
		return 0, nil
	}
	calls, err := ledger.BoundTo(tx, l.ID)
	if err != nil {
		return 0, err
	}
	live := !lease.Ended(l.State)
	held := int64(0) // the reservation's tokens
	for _, r := range l.Reservations {
		if r.Unit == types.CapacityUnitTokens {
			held += int64(r.Amount)
		}
	}
	if len(calls) == 0 {
		if !live {
			return 0, nil
		}
		return ledger.PriceTokens(held, *e.Price)
	}
	var total int64
	for _, call := range calls {
		ms, err := ledger.Measurements(tx, call)
		if err != nil {
			return 0, err
		}
		s := ledger.StandingOf(ms, call)
		var c int64
		switch {
		case s.Settled && s.Usage != nil:
			c, err = ledger.PriceMicros(*s.Usage, *e.Price)
		case mutation.Is(mutation.USDRailSettledOnly):
		case live:
			tokens := held
			if s.Usage != nil {
				tokens = max(tokens, s.Usage.In+s.Usage.Out+s.Usage.CacheRead+s.Usage.CacheWrite)
			}
			c, err = ledger.PriceTokens(tokens, *e.Price)
		default:
			var owed map[types.ResourceGroupID]int64
			if owed, _, err = capacity.LiabilitiesOf(tx, l.ID, groupsOf(l)); err == nil {
				for _, a := range owed {
					var p int64
					p, err = ledger.PriceTokens(a, *e.Price)
					c += p
				}
			}
		}
		if err != nil {
			return 0, err
		}
		total += c
	}
	return total, nil
}

// reserved is res priced at price, the higher token rate; nothing on a
// route with no price.
func reserved(res types.LeaseReservationsItem, price *types.RegistryPrice) (int64, error) {
	if price == nil || res.Unit != types.CapacityUnitTokens {
		return 0, nil
	}
	return ledger.PriceTokens(int64(res.Amount), *price)
}

// groupsOf are l's reserved groups.
func groupsOf(l types.Lease) []types.ResourceGroupID {
	out := make([]types.ResourceGroupID, 0, len(l.Reservations))
	for _, r := range l.Reservations {
		out = append(out, r.Group)
	}
	return out
}
