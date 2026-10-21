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
//     nodes with their executor and attempts (attempt → node, P8a), the
//     attempts' usage measurements and the task's interventions, and each
//     route's registry price. The command that reads them, adc dev bench
//     record, comes with W18.
//   - Wall time uses controller event times only. A stage's wall time sums
//     its nodes', so two nodes running side by side count twice (§2.8,
//     "summed over the stage's nodes").
//   - Each call belongs to one stage, then its standing is
//     ledger.StandingOf over the stage's reports: only a settled call is
//     charged; its tokens are the standing's figure, the gateway's count
//     included. An unsettled call is outstanding at the largest of its
//     reported USD cost, its tokens at the route's registry price
//     (ledger.PriceMicros) and its reservation-backed liability, never as
//     zero nor as settled; one in another currency with a reported amount
//     is refused (no conversion), and one whose amount is zero is refused.
//     A call's basis is its adapter and provider reports' (the gateway's
//     only when it has no other), never the first report's.
//   - The calls are the ledger's bound-call inventory (Task.Calls), not the
//     measurements: every measured call must be bound, and a bound call
//     with no measurement is outstanding at its liability (cash, USD), or
//     refused when it has none.
//   - Each sum is checked against the contract's maximum (2^53-1) before
//     it is added; a sum past it is refused.
//   - It refuses rather than guesses: an empty run; a task, attempt or call
//     twice in the run; a task without an approval time, closed and held,
//     neither, closed without an outcome and reason, closed before its
//     approval, or held at an unknown hold; a node with an unknown stage, no
//     claimed time, done before it was claimed, or attempts on no route's
//     executor; an attempt of two nodes; a bound call of no node's attempt,
//     bound twice or with a negative liability; a measurement whose attempt
//     is no node's or whose call is unbound; a call of two stages or
//     executors, in two currencies or two bases; an intervention of an
//     unknown kind, or not a rescue and at an unknown stage; a route without
//     a price, or declared twice differently; and a run its schema refuses
//     (a settled call in no currency among them).
//   - It records and never gates: Compare reports the metrics that grew.
package bench

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// Node is one node of a task: its stage, round, the executor its attempts
// ran on, node.claimed time, and done time (result.selected; for merge the
// effect's observed or reconciled completed), zero while unfinished, with
// its attempts.
type Node struct {
	Stage    types.BenchmarkStageStage
	Round    int
	Executor types.ExecutorID
	Claimed  time.Time
	Done     time.Time
	Attempts []types.AttemptID
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
// attempt it is attributed to and its reservation-backed liability in USD
// micros (capacity.LiabilitiesOf's share; zero when the ledger holds none).
type Call struct {
	ID        types.CallID
	Attempt   types.AttemptID
	Liability int64
}

// maxSum is the contract's maximum integer (2^53-1).
const maxSum = 1<<53 - 1

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
			ids["attempt "+string(a)] = true
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
	case !t.Closed.IsZero():
		ms, o, r := t.Closed.Sub(t.Approved).Milliseconds(), t.Outcome, t.Reason
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
	nodeOf := map[types.AttemptID]int{}
	for i, n := range t.Nodes {
		if !slices.Contains(Stages, n.Stage) {
			return bt, fmt.Errorf("an unknown stage %q", n.Stage)
		} else if n.Claimed.IsZero() {
			return bt, fmt.Errorf("a %s node with no claimed time", n.Stage)
		} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {
			return bt, fmt.Errorf("a %s node done before it was claimed", n.Stage)
		} else if _, ok := route[n.Executor]; len(n.Attempts) > 0 && !ok {
			return bt, fmt.Errorf("a %s node on executor %q, which is no route's", n.Stage, n.Executor)
		}
		for _, a := range n.Attempts {
			if _, dup := nodeOf[a]; dup {
				return bt, fmt.Errorf("attempt %s of two nodes", a)
			}
			nodeOf[a] = i
		}
	}
	// Each bound call to its attempt's node, then each measurement to its
	// call's stage and executor, then per stage.
	callNode, liability := map[types.CallID]int{}, map[types.CallID]int64{}
	bound := map[types.BenchmarkStageStage][]types.CallID{}
	for _, c := range t.Calls {
		i, ok := nodeOf[c.Attempt]
		if !ok {
			return bt, fmt.Errorf("bound call %s of attempt %s, which is no node's", c.ID, c.Attempt)
		} else if _, dup := callNode[c.ID]; dup {
			return bt, fmt.Errorf("call %s bound twice", c.ID)
		} else if c.Liability < 0 {
			return bt, fmt.Errorf("call %s with a negative liability", c.ID)
		}
		callNode[c.ID], liability[c.ID] = i, c.Liability
		bound[t.Nodes[i].Stage] = append(bound[t.Nodes[i].Stage], c.ID)
	}
	usage := map[types.BenchmarkStageStage][]types.UsageMeasurement{}
	for _, u := range t.Usage {
		i, ok := nodeOf[u.Attempt]
		if !ok {
			return bt, fmt.Errorf("measurement %s of attempt %s, which is no node's", u.ID, u.Attempt)
		}
		j, ok := callNode[u.Call]
		if !ok {
			return bt, fmt.Errorf("measurement %s of call %s, which is no bound call", u.ID, u.Call)
		} else if t.Nodes[j].Stage != t.Nodes[i].Stage {
			return bt, fmt.Errorf("call %s in two stages, %s and %s", u.Call, t.Nodes[j].Stage, t.Nodes[i].Stage)
		} else if t.Nodes[j].Executor != t.Nodes[i].Executor {
			return bt, fmt.Errorf("call %s on two executors", u.Call)
		}
		usage[t.Nodes[i].Stage] = append(usage[t.Nodes[i].Stage], u)
	}
	price := map[types.CallID]types.RegistryPrice{}
	for c, i := range callNode {
		price[c] = m.Prices[t.Nodes[i].Executor]
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
		used, rounds[n.Round] = true, true
		add(&err, &st.Attempts, int64(len(n.Attempts)))
		if n.Done.IsZero() {
			st.Unfinished++
		} else {
			add(&err, &st.WallMs, n.Done.Sub(n.Claimed).Milliseconds())
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
			reported := calls[id].Usage.CostMicros
			if standing.Settled {
				into, owed, micros, currency = settled, false, reported, calls[id].Currency
			} else {
				priced, perr := ledger.PriceMicros(f, price[id])
				if perr != nil {
					return st, false, fmt.Errorf("call %s: %w", id, perr)
				}
				if c := calls[id].Currency; c != "" && c != "USD" && reported != 0 {
					return st, false, fmt.Errorf("call %s: an unsettled %s amount, which cannot be compared with its USD valuation", id, c)
				} else if c == "" || c == "USD" {
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
