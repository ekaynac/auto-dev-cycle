// SPDX-License-Identifier: Apache-2.0

package dispatch

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// reservation is what Match reserves for card on executor e, the lease's
// footprint, and the spend it read (nil when it read none), which usdRail
// then checks the reservation against (Opus 22a-5c-1 L2: one read per
// Match) (#226 entries 103, 120):
//   - a local endpoint reserves one slot (Q8);
//   - any other executor needs its footprint_prior, which is the lease's
//     footprint: without one it is refused precondition_failed, its node
//     held source_invalid_config naming the executor (no default number);
//   - an unpriced one reserves attempt_cap, context_tokens ×
//     expected_turns;
//   - a priced one reserves min(fit, attempt_cap), where fit is the most
//     tokens the USD rails leave at the executor's higher token rate
//     (dayFit's computation over both rails): a fit below one turn
//     (context_tokens) is refused as the rail that binds, its budget_rail
//     hold named;
//   - a reservation past its group's stated limit is refused
//     precondition_failed, held source_invalid_config naming the executor:
//     no attempt of it could ever be granted (Opus 22a-5c-1 M1).
func (d Dispatcher) reservation(tx *eventstore.Tx, card Card, executor types.ExecutorID, e types.RegistryExecutor) (types.LeaseReservationsItem, types.LeaseFootprint, *spend, *Refused, error) {
	res := types.LeaseReservationsItem{Group: e.Group, Unit: types.CapacityUnitTokens}
	var fp types.LeaseFootprint
	if p := e.FootprintPrior; p != nil {
		fp = types.LeaseFootprint{ContextTokens: p.ContextTokens, ExpectedTurns: p.ExpectedTurns}
	}
	if e.Destination.Kind == types.RegistryDestinationKindLocalEndpoint { // one slot (Q8)
		res.Amount, res.Unit = 1, types.CapacityUnitSlots
		return res, fp, nil, nil, nil
	}
	attemptCap, r, err := d.footprint(executor, e)
	if err != nil || r != nil {
		return res, fp, nil, r, err
	}
	res.Amount = attemptCap
	if e.Price == nil {
		return res, fp, nil, nil, nil
	}
	b, err := d.budget(tx, card)
	if err != nil || b.unrailed {
		return res, fp, &b, nil, err // usdRail refuses a priced executor with no rails
	}
	rail, left, fit, capped := b.fit(e, graph.RailTaskUSD, graph.RailDayUSD)
	if !capped {
		return res, fp, &b, nil, nil
	}
	if fit < fp.ContextTokens {
		hold, err := graph.RailHold(rail)
		return res, fp, &b, &Refused{Check: "budget", PlacementCheck: types.PlacementCheck{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonBudgetRailReached,
			Detail: fmt.Sprintf("%s: %d USD micros left buy %d tokens, less than one turn's %d (#226 entry 120)", rail, left, fit, fp.ContextTokens)}, Hold: &hold}, err
	}
	res.Amount = min(fit, attemptCap)
	return res, fp, &b, nil, nil
}

// footprint is a direct API executor e's attempt_cap, or the refusal its
// configuration earns: no footprint_prior, or an attempt_cap past its
// group's stated limit (precondition_failed, held source_invalid_config on
// the executor).
func (d Dispatcher) footprint(executor types.ExecutorID, e types.RegistryExecutor) (int64, *Refused, error) {
	if e.FootprintPrior == nil {
		r, err := configRefusal("footprint_prior", "executor "+string(executor), "executor "+string(executor)+" declares no footprint_prior (#226 entry 103)")
		return 0, r, err
	}
	attemptCap := mulSat(e.FootprintPrior.ContextTokens, e.FootprintPrior.ExpectedTurns)
	if limit, ok := d.Registry.Limit(e.Group); ok && attemptCap > limit {
		r, err := configRefusal("capacity", "executor "+string(executor), fmt.Sprintf("executor %s's attempt_cap %d is past its group %s's limit %d", executor, attemptCap, e.Group, limit))
		return 0, r, err
	}
	return attemptCap, nil, nil
}

// Configured: no configuration refuses card on executor now (Mythos
// 22a-5c-1 M1): the placement checks pass, a direct API's footprint is
// admissible, and a priced executor is under USD rails. What clears a
// placement hold the configuration raised; the rails' room is Match's.
func (d Dispatcher) Configured(tx *eventstore.Tx, card Card, executor types.ExecutorID) (bool, error) {
	_, roster, err := graph.Units{}.Roster(tx, graph.UnitOf(card.Node), d.Registry)
	if err != nil {
		return false, err
	}
	if _, _, blocks := blocked(d.checks(card, executor, roster)); blocks {
		return false, nil
	}
	e, _ := d.Registry.Executor(executor)
	if e.Destination.Kind == types.RegistryDestinationKindLocalEndpoint {
		return true, nil
	}
	if _, r, err := d.footprint(executor, e); err != nil || r != nil {
		return false, err
	}
	if e.Price == nil {
		return true, nil
	}
	b, err := d.budget(tx, card)
	return err == nil && !b.unrailed, err
}

// RoomToday: the day's USD rail leaves card's node at least one turn on
// executor now (#226 entry 120), so a per_day_usd hold on it is stale: a
// new UTC day, or a profile change, reopened it. It is reservation's own
// computation over the day's rail alone (spend.fit), so the clear and the
// match cannot disagree (Opus 22a-5c-1 L3). An unpriced executor, one with
// no prior, or no day rail at all leave the rail nothing to hold.
func (d Dispatcher) RoomToday(tx *eventstore.Tx, card Card, executor types.ExecutorID) (bool, error) {
	e, ok := d.Registry.Executor(executor)
	if !ok || e.Price == nil || e.FootprintPrior == nil {
		return true, nil
	}
	b, err := d.budget(tx, card)
	if err != nil || b.unrailed {
		return b.unrailed, err
	}
	_, _, fit, capped := b.fit(e, graph.RailDayUSD)
	return !capped || fit >= e.FootprintPrior.ContextTokens, nil
}

// spend is a task's rails and what its task and its project's day have
// spent, in USD micros.
type spend struct {
	caps            graph.Caps
	unrailed        bool // no USD rail is configured: a priced executor is refused
	perTask, perDay int64
	day             string
}

// budget is card's task's spend now, under d's rails.
func (d Dispatcher) budget(tx *eventstore.Tx, card Card) (spend, error) {
	task, err := graph.TaskOf(tx, card.Node)
	if err != nil {
		return spend{}, err
	}
	s := spend{caps: graph.DefaultCaps, day: tx.Now().UTC().Format(time.DateOnly)}
	if d.Rails != nil {
		if s.caps, err = d.Rails(tx, task); err != nil {
			return s, err
		}
	}
	if s.caps.PerTaskMicros < 0 || s.caps.PerDayMicros < 0 {
		s.unrailed = true
		return s, nil
	}
	s.perTask, s.perDay, err = spent(tx, task, card.Project, s.day)
	return s, err
}

// fit is, among rails (graph.RailTaskUSD, graph.RailDayUSD), the one
// with the least left (the first named on a tie), what it leaves, and the
// most tokens that buys at e's higher token rate (tokensFor); capped false
// when none of rails caps.
func (s spend) fit(e types.RegistryExecutor, rails ...string) (rail string, left, tokens int64, capped bool) {
	for _, r := range rails {
		cap, spent := s.caps.PerTaskMicros, s.perTask
		if r == graph.RailDayUSD {
			cap, spent = s.caps.PerDayMicros, s.perDay
		}
		if cap < 0 {
			continue
		}
		l := max(cap-spent, 0)
		if !capped || l < left {
			rail, left, capped = r, l, true
		}
	}
	if !capped || e.Price == nil {
		return rail, left, maxAmount, capped
	}
	return rail, left, tokensFor(left, max(e.Price.InputUsdMicrosPerMtok, e.Price.OutputUsdMicrosPerMtok)), true
}

// tokensFor is the most tokens micros buy at price USD micros per million
// tokens, priced as ledger.PriceTokens rounds (up): ⌊micros × 10⁶ / price⌋,
// saturating; any number at price 0.
func tokensFor(micros, price int64) int64 {
	if price <= 0 {
		return maxAmount
	}
	q := new(big.Int).Mul(big.NewInt(micros), big.NewInt(1_000_000))
	q.Quo(q, big.NewInt(price))
	if !q.IsInt64() || q.Int64() > maxAmount {
		return maxAmount
	}
	return q.Int64()
}

// maxAmount is the contract's largest amount (resource-group.json Amount).
const maxAmount = 1<<53 - 1

// mulSat is a×b, at most maxAmount.
func mulSat(a, b int64) int64 {
	if a != 0 && b > maxAmount/a {
		return maxAmount
	}
	return min(a*b, maxAmount)
}

// placementPrefix marks the signal of a hold placement raised on
// configuration (PlacementSignal): the reconcile loop clears only those,
// never a source_invalid_config hold another component raised.
const placementPrefix = "placement: "

// PlacementSignal is the signal instance of a placement hold naming what
// to change (the executor, the project's profile, the registry).
func PlacementSignal(what string) string { return placementPrefix + what }

// IsPlacementSignal: signal is a placement hold's (PlacementSignal).
func IsPlacementSignal(signal string) bool { return strings.HasPrefix(signal, placementPrefix) }

// configRefusal is a placement check that configuration blocks, named
// check, refused precondition_failed with detail, and its node held
// source_invalid_config on signal (the executor, or the project's profile),
// the operator's: no later match of it can pass until the configuration
// changes (Opus 22a-5c-1 M1).
func configRefusal(check, signal, detail string) (*Refused, error) {
	m, err := held("", "source_invalid_config", PlacementSignal(signal), types.OwnerOperator, nil)
	if err != nil {
		return nil, err
	}
	return &Refused{Check: check, PlacementCheck: types.PlacementCheck{Outcome: types.OutcomeCouldNotRun, Reason: vocab.ReasonPreconditionFailed, Detail: detail},
		Hold: m.Hold}, nil
}
