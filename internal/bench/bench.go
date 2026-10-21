// SPDX-License-Identifier: Apache-2.0

// Package bench is the M1 benchmark (docs/design/m1/00-slice-plan.md §2.8;
// P25): a checkpoint's records/benchmark-run, its deltas against an earlier
// run, and the summary its exit record carries.
//
// Decisions:
//   - Project-agnostic: the tasks, routes and fixtures are inputs; nothing
//     here names a project (§2.8's runs still name py-mini; draft 11 leaves
//     the stub project open).
//   - Pure: Record computes from what the recorder reads over the API (adc
//     task show and the ledger, W18): a Task's controller event times, its
//     nodes with their attempts and each attempt's executor (attempt →
//     node, P8a), the attempts' usage measurements, the ledger's bound
//     calls with their liabilities, the task's interventions, and each
//     route's registry price. The command that reads them, adc dev bench
//     record, comes with W18.
//   - Trusted, not checked: that Task.Calls is complete (an unmeasured call
//     left out of it disappears); that Task.Usage carries every report of
//     each bound call (a dropped gateway count leaves a call unsettled at a
//     lower figure, with no trace); the reported cost.basis and currency
//     labels (they select which rule prices a call); each Call.Attempt (for
//     an unmeasured call it alone decides the stage); Meta.Prices (a zero
//     price removes the floor); and each Call.Liability (a positive wrong
//     one is recorded). adc dev bench record must therefore pass every call
//     ledger.BoundTo returns for each lease of the task, every measurement
//     the ledger holds for each of them, and as a call's liability the
//     usd_micros group's amount of capacity.LiabilitiesOf once the lease
//     has settled, and the lease's usd_micros reservation while it has
//     not; zero only when the lease has neither. A lease's liability is
//     passed once per lease, on one of its unmeasured or unsettled calls,
//     and zero on the others; never repeated per call.
//   - Wall time uses controller event times only. A stage's wall time sums
//     its nodes', so two nodes running side by side count twice (§2.8,
//     "summed over the stage's nodes"). A node never claimed is counted
//     pending, apart from unfinished.
//   - Each call belongs to one stage, then its standing is
//     ledger.StandingOf over the stage's reports: only a settled call is
//     charged; its tokens are the standing's figure, the gateway's count
//     included. A settled call, on any basis, is charged at no less than
//     its tokens at the route's registry price (ledger.PriceMicros), as the
//     ledger never trusts a reported cost nor its basis label (#226 entry
//     62), and a charge the floor raised is recorded under basis cash, any
//     other under its reported basis; free and local routes declare a zero
//     price, so there it leaves no floor. An unsettled call is outstanding,
//     under basis cash, at the
//     largest of its reported USD cost, that price and its liability, never
//     as zero nor as settled. No currency is converted: a settled call in
//     another currency is refused when its floor is positive, and an
//     unsettled one when it reports an amount. A call's basis is its
//     adapter and provider reports' (the gateway's only when it has no
//     other), never the first report's.
//   - The calls are the ledger's bound-call inventory (Task.Calls), not the
//     measurements: every measured call must be bound, and a bound call
//     with no measurement is outstanding at its liability (cash, USD).
//   - Each sum is checked against the contract's maximum (2^53-1) before
//     it is added; a sum past it is refused.
//   - It refuses rather than guesses: an empty run; a task, attempt or call
//     twice in the run; a task without an approval time, closed and held,
//     neither, closed without an outcome and reason, closed before its
//     approval, or held at an unknown hold; any time after RecordedAt, or a
//     duration past time.Duration's maximum; a node with an unknown stage,
//     done or attempted but never claimed, or done before it was claimed;
//     an attempt on no route's executor or of two nodes; a bound call of
//     no node's attempt, bound twice or with a negative liability; a
//     measurement whose attempt is no node's, whose call is unbound, or
//     with a negative cost; a call of two stages or executors, in two
//     currencies or two bases; an unsettled call whose amount is zero (no
//     liability, no reported USD cost, no priced tokens: a crashed attempt
//     whose lease holds no usd_micros amount, or a zero-figure call on a
//     zero-priced route); an intervention of an unknown kind, or not a
//     rescue and at an unknown stage; a route without a price, or declared
//     twice differently; and a run its schema refuses (a settled call in no
//     currency among them).
//   - It records and never gates: Compare reports the metrics that grew.
package bench

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const schema = "adc.benchmark-run/v1"

// Stages are §2.8's stages, in their order; a repair round counts as its
// kind.
var Stages = []types.BenchmarkStageStage{"build", "accept", "gate", "review", "rebase", "review_fresh", "gate_refresh", "merge", "closure"}

// Intervention kinds.
const (
	Approval  = "approval"
	Answer    = "answer"
	RailRaise = "rail_raise"
	Rescue    = "rescue"
)

var kinds = []string{Approval, Answer, RailRaise, Rescue}

// Node is one node of a task: its stage, round, node.claimed time (zero
// while pending: planned and never claimed), and done time
// (result.selected; for merge the effect's observed or reconciled
// completed), zero while unfinished, with its attempts.
type Node struct {
	Stage    types.BenchmarkStageStage
	Round    int
	Claimed  time.Time
	Done     time.Time
	Attempts []Attempt
}

// Attempt is one attempt of a node and the executor it ran on.
type Attempt struct {
	ID       types.AttemptID
	Executor types.ExecutorID
}

// Intervention is one operator or supervisor command attributed to the
// task, at a stage (a rescue's stage is ignored: rescues are counted apart).
type Intervention struct {
	Stage types.BenchmarkStageStage
	Kind  string
}

// Task is what the recorder reads of one task: approval.recorded, and the
// closure's result.selected (zero when it did not close) with its outcome,
// or the task's hold.
type Task struct {
	ID            types.TaskID
	Approved      time.Time
	Closed        time.Time
	Outcome       types.Outcome
	Reason        types.ReasonCode
	Hold          string
	Nodes         []Node
	Usage         []types.UsageMeasurement
	Calls         []Call
	Interventions []Intervention
}

// Call is one bound call of the task, from the ledger's inventory: the
// attempt it is attributed to and its liability in USD micros. The
// recorder trusts all three (see the package doc for what the caller must
// pass).
type Call struct {
	ID        types.CallID
	Attempt   types.AttemptID
	Liability int64
}

// maxSum is the contract's maximum integer (2^53-1).
const maxSum = 1<<53 - 1

// span is b-a in milliseconds, refusing a duration time.Sub saturated.
func span(a, b time.Time) (int64, error) {
	if d := b.Sub(a); d == time.Duration(math.MaxInt64) {
		return 0, fmt.Errorf("a duration from %s to %s past the maximum", a.Format(time.RFC3339), b.Format(time.RFC3339))
	} else {
		return d.Milliseconds(), nil
	}
}

// add adds v to *dst, refusing first a negative v or a sum past maxSum.
func add(err *error, dst *int64, v int64) {
	if *err == nil && (v < 0 || v > maxSum-*dst) {
		*err = fmt.Errorf("a sum past %d", maxSum)
	}
	if *err == nil {
		*dst += v
	}
}

// Meta is a run's provenance, with each route's registry price
// (config/executor-registry), which prices an unsettled call.
type Meta struct {
	Checkpoint types.BenchmarkRunCheckpoint
	RecordedAt time.Time
	Arch       string // GOOS/GOARCH of the controller host
	Commit     types.GitID
	SchemaSet  types.Digest
	Routes     []types.BenchmarkRoute
	Prices     map[types.ExecutorID]types.RegistryPrice
	Fixtures   []types.BenchmarkFixture
}

// Record is the benchmark run of tasks, valid against records/benchmark-run.
func Record(m Meta, tasks []Task) (types.BenchmarkRun, error) {
	if len(tasks) == 0 {
		return types.BenchmarkRun{}, errors.New("bench: no tasks")
	}
	route := map[types.ExecutorID]types.BenchmarkRoute{}
	for _, r := range m.Routes {
		if q, ok := route[r.Executor]; ok && q != r {
			return types.BenchmarkRun{}, fmt.Errorf("bench: route %s declared twice, conflicting", r.Executor)
		} else if _, ok := m.Prices[r.Executor]; !ok {
			return types.BenchmarkRun{}, fmt.Errorf("bench: route %s has no price", r.Executor)
		}
		route[r.Executor] = r
	}
	reg, err := vocab.Load()
	if err != nil {
		return types.BenchmarkRun{}, err
	}
	run := types.BenchmarkRun{Schema: schema, Checkpoint: m.Checkpoint, RecordedAt: types.Time(m.RecordedAt.UTC().Format(time.RFC3339)), Arch: m.Arch,
		BuildCommit: m.Commit, SchemaSet: m.SchemaSet, Routes: slices.Clone(m.Routes), Fixtures: slices.Clone(m.Fixtures), Tasks: []types.BenchmarkTask{}}
	seen := map[string]bool{}
	for _, t := range tasks {
		bt, err := task(m, route, reg, t)
		if err == nil {
			err = once(seen, t)
		}
		if err != nil {
			return types.BenchmarkRun{}, fmt.Errorf("bench: task %s: %w", t.ID, err)
		}
		run.Tasks = append(run.Tasks, bt)
	}
	raw, err := json.Marshal(run)
	if err == nil {
		_, err = contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw)
	}
	if err != nil {
		return types.BenchmarkRun{}, fmt.Errorf("bench: the run: %w", err)
	}
	return run, nil
}

// once refuses t's id, an attempt or a call already in seen, then adds them.
func once(seen map[string]bool, t Task) error {
	ids := map[string]bool{"task " + string(t.ID): true}
	for _, n := range t.Nodes {
		for _, a := range n.Attempts {
			ids["attempt "+string(a.ID)] = true
		}
	}
	for _, c := range t.Calls {
		ids["call "+string(c.ID)] = true
	}
	for _, k := range slices.Sorted(maps.Keys(ids)) {
		if seen[k] {
			return fmt.Errorf("%s twice in the run", k)
		}
		seen[k] = true
	}
	return nil
}

func task(m Meta, route map[types.ExecutorID]types.BenchmarkRoute, reg *vocab.Registry, t Task) (types.BenchmarkTask, error) {
	bt := types.BenchmarkTask{Task: t.ID, Stages: []types.BenchmarkStage{}}
	switch {
	case t.Approved.IsZero():
		return bt, errors.New("no approval time")
	case !t.Closed.IsZero() && t.Hold != "":
		return bt, errors.New("closed and held")
	case t.Closed.IsZero() && t.Hold == "":
		return bt, errors.New("neither closed nor held")
	case !t.Closed.IsZero() && (t.Outcome == "" || t.Reason.String() == ""):
		return bt, errors.New("closed without an outcome and reason")
	case !t.Closed.IsZero() && t.Closed.Before(t.Approved):
		return bt, errors.New("closed before its approval")
	case t.Approved.After(m.RecordedAt) || t.Closed.After(m.RecordedAt):
		return bt, errors.New("approved or closed after the run was recorded")
	case !t.Closed.IsZero():
		ms, err := span(t.Approved, t.Closed)
		if err != nil {
			return bt, err
		}
		o, r := t.Outcome, t.Reason
		bt.WallMs, bt.Outcome, bt.Reason = &ms, &o, &r
	default:
		if _, ok := reg.Holds[t.Hold]; !ok {
			return bt, fmt.Errorf("an unknown hold %q", t.Hold)
		}
		h := t.Hold
		bt.Hold = &h
	}
	for _, iv := range t.Interventions {
		if !slices.Contains(kinds, iv.Kind) {
			return bt, fmt.Errorf("an unknown intervention %q", iv.Kind)
		} else if iv.Kind != Rescue && !slices.Contains(Stages, iv.Stage) {
			return bt, fmt.Errorf("an intervention at an unknown stage %q", iv.Stage)
		}
		bt.Rescues += b2i(iv.Kind == Rescue)
	}
	nodeOf, exec := map[types.AttemptID]int{}, map[types.AttemptID]types.ExecutorID{}
	for i, n := range t.Nodes {
		if !slices.Contains(Stages, n.Stage) {
			return bt, fmt.Errorf("an unknown stage %q", n.Stage)
		} else if n.Claimed.IsZero() && (!n.Done.IsZero() || len(n.Attempts) > 0) {
			return bt, fmt.Errorf("a %s node done or attempted, with no claimed time", n.Stage)
		} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {
			return bt, fmt.Errorf("a %s node done before it was claimed", n.Stage)
		} else if n.Claimed.After(m.RecordedAt) || n.Done.After(m.RecordedAt) {
			return bt, fmt.Errorf("a %s node after the run was recorded", n.Stage)
		}
		for _, a := range n.Attempts {
			if _, ok := route[a.Executor]; !ok {
				return bt, fmt.Errorf("a %s attempt on executor %q, which is no route's", n.Stage, a.Executor)
			} else if _, dup := nodeOf[a.ID]; dup {
				return bt, fmt.Errorf("attempt %s of two nodes", a.ID)
			}
			nodeOf[a.ID], exec[a.ID] = i, a.Executor
		}
	}
	// Each bound call to its attempt's node, then each measurement to its
	// call's stage and executor, then per stage.
	callAttempt, liability := map[types.CallID]types.AttemptID{}, map[types.CallID]int64{}
	bound := map[types.BenchmarkStageStage][]types.CallID{}
	for _, c := range t.Calls {
		i, ok := nodeOf[c.Attempt]
		if !ok {
			return bt, fmt.Errorf("bound call %s of attempt %s, which is no node's", c.ID, c.Attempt)
		} else if _, dup := callAttempt[c.ID]; dup {
			return bt, fmt.Errorf("call %s bound twice", c.ID)
		} else if c.Liability < 0 {
			return bt, fmt.Errorf("call %s with a negative liability", c.ID)
		}
		callAttempt[c.ID], liability[c.ID] = c.Attempt, c.Liability
		bound[t.Nodes[i].Stage] = append(bound[t.Nodes[i].Stage], c.ID)
	}
	usage := map[types.BenchmarkStageStage][]types.UsageMeasurement{}
	for _, u := range t.Usage {
		i, ok := nodeOf[u.Attempt]
		if !ok {
			return bt, fmt.Errorf("measurement %s of attempt %s, which is no node's", u.ID, u.Attempt)
		}
		ca, ok := callAttempt[u.Call]
		if !ok {
			return bt, fmt.Errorf("measurement %s of call %s, which is no bound call", u.ID, u.Call)
		} else if s := t.Nodes[nodeOf[ca]].Stage; s != t.Nodes[i].Stage {
			return bt, fmt.Errorf("call %s in two stages, %s and %s", u.Call, s, t.Nodes[i].Stage)
		} else if exec[ca] != exec[u.Attempt] {
			return bt, fmt.Errorf("call %s on two executors", u.Call)
		} else if u.Cost.AmountMicros < 0 {
			return bt, fmt.Errorf("measurement %s with a negative cost", u.ID)
		}
		usage[t.Nodes[i].Stage] = append(usage[t.Nodes[i].Stage], u)
	}
	price := map[types.CallID]types.RegistryPrice{}
	for c, a := range callAttempt {
		price[c] = m.Prices[exec[a]]
	}
	for _, s := range Stages {
		st, used, err := stage(s, t, usage[s], bound[s], liability, price)
		if err != nil {
			return bt, err
		} else if used {
			bt.Stages = append(bt.Stages, st)
		}
	}
	return bt, nil
}

// stage is stage s's metrics, and whether the task has any of it.
func stage(s types.BenchmarkStageStage, t Task, usage []types.UsageMeasurement, bound []types.CallID, liability map[types.CallID]int64, price map[types.CallID]types.RegistryPrice) (types.BenchmarkStage, bool, error) {
	st := types.BenchmarkStage{Stage: s, Sources: []types.BenchmarkStageSourcesItem{}, Charge: []types.BenchmarkAmount{}, Outstanding: []types.BenchmarkAmount{}}
	rounds, used, err := map[int]bool{}, false, error(nil)
	for _, n := range t.Nodes {
		if n.Stage != s {
			continue
		}
		used = true
		if n.Claimed.IsZero() {
			st.Pending++
			continue
		}
		rounds[n.Round] = true
		add(&err, &st.Attempts, int64(len(n.Attempts)))
		if n.Done.IsZero() {
			st.Unfinished++
		} else {
			ms, serr := span(n.Claimed, n.Done)
			err = cmp.Or(err, serr)
			add(&err, &st.WallMs, ms)
		}
	}
	st.Rounds = int64(len(rounds))
	for _, iv := range t.Interventions {
		if iv.Stage != s || iv.Kind == Rescue {
			continue
		}
		switch used = true; iv.Kind {
		case Approval:
			st.Interventions.Approvals++
		case Answer:
			st.Interventions.Answers++
		case RailRaise:
			st.Interventions.RailRaises++
		}
	}
	if err != nil || len(bound) == 0 {
		return st, used, err
	}
	calls := map[types.CallID]ledger.Call{}
	if len(usage) > 0 {
		if calls, err = ledger.Reconcile(usage); err != nil {
			return st, false, err
		}
	}
	reports, sources := map[types.CallID][]types.UsageMeasurement{}, map[types.BenchmarkStageSourcesItem]bool{}
	for _, u := range usage {
		reports[u.Call], sources[types.BenchmarkStageSourcesItem(u.SourceOfTruth)] = append(reports[u.Call], u), true
	}
	st.Sources = append(st.Sources, slices.Sorted(maps.Keys(sources))...)
	settled, outstanding := map[[2]string]*types.BenchmarkAmount{}, map[[2]string]*types.BenchmarkAmount{}
	for _, id := range slices.Sorted(slices.Values(bound)) {
		into, owed, basis, micros, currency := outstanding, true, types.BenchmarkAmountBasis("cash"), liability[id], "USD"
		if _, measured := calls[id]; measured {
			if basis, err = basisOf(reports[id]); err != nil {
				return st, false, fmt.Errorf("call %s: %w", id, err)
			}
			standing := ledger.StandingOf(reports[id], id)
			f := *standing.Usage
			add(&err, &st.Tokens.In, f.In)
			add(&err, &st.Tokens.Out, f.Out)
			add(&err, &st.Tokens.CacheRead, f.CacheRead)
			add(&err, &st.Tokens.CacheWrite, f.CacheWrite)
			reported, c := calls[id].Usage.CostMicros, calls[id].Currency
			priced, perr := ledger.PriceMicros(f, price[id])
			if perr != nil {
				return st, false, fmt.Errorf("call %s: %w", id, perr)
			}
			switch {
			case standing.Settled && c != "" && c != "USD" && priced > 0:
				return st, false, fmt.Errorf("call %s: a settled %s amount, which cannot be compared with its USD valuation", id, c)
			case standing.Settled && c != "" && c != "USD":
				into, owed, micros, currency = settled, false, reported, c
			case standing.Settled:
				// A settled call, on any basis, at no less than its registry
				// price: the ledger never trusts a reported cost nor its
				// basis label (#226 entry 62); a free or local route's zero
				// price leaves no floor.
				into, owed, micros, currency = settled, false, max(reported, priced), c
				if priced > reported {
					basis = "cash" // the floor is a cash valuation
				}
			case c != "" && c != "USD" && reported != 0:
				return st, false, fmt.Errorf("call %s: an unsettled %s amount, which cannot be compared with its USD valuation", id, c)
			default:
				// An unsettled call's floor is a cash valuation, recorded
				// under cash whatever its reported basis.
				basis = "cash"
				if c == "" || c == "USD" {
					micros = max(micros, reported)
				}
				micros = max(micros, priced)
			}
		}
		if owed && micros <= 0 {
			return st, false, fmt.Errorf("call %s: unsettled, with no positive liability", id)
		}
		k := [2]string{string(basis), currency}
		if into[k] == nil {
			into[k] = &types.BenchmarkAmount{Basis: basis, Currency: currency}
		}
		add(&err, &into[k].AmountMicros, micros)
		add(&err, &into[k].Calls, 1)
		if err != nil {
			return st, false, fmt.Errorf("stage %s: %w", s, err)
		}
	}
	st.Charge, st.Outstanding = amounts(settled), amounts(outstanding)
	return st, true, nil
}

// basisOf is a call's basis: its adapter and provider reports', or the
// gateway's when it has no other; two bases are refused.
func basisOf(ms []types.UsageMeasurement) (types.BenchmarkAmountBasis, error) {
	own, gateway := map[types.BenchmarkAmountBasis]bool{}, map[types.BenchmarkAmountBasis]bool{}
	for _, m := range ms {
		at := own
		if m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayCount || m.SourceOfTruth == types.UsageMeasurementSourceOfTruthGatewayFloor {
			at = gateway
		}
		at[types.BenchmarkAmountBasis(m.Cost.Basis)] = true
	}
	if len(own) == 0 {
		own = gateway
	}
	if len(own) != 1 {
		return "", fmt.Errorf("in %d bases", len(own))
	}
	return slices.Collect(maps.Keys(own))[0], nil
}

// amounts is m's amounts, by basis then currency.
func amounts(m map[[2]string]*types.BenchmarkAmount) []types.BenchmarkAmount {
	out := []types.BenchmarkAmount{}
	for _, k := range slices.SortedFunc(maps.Keys(m), func(a, b [2]string) int { return cmp.Or(strings.Compare(a[0], b[0]), strings.Compare(a[1], b[1])) }) {
		out = append(out, *m[k])
	}
	return out
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
