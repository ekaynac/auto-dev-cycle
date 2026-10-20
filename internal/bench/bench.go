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
//     nodes and their attempts (attempt → node, P8a), the attempts' usage
//     measurements and the task's interventions. The command that reads
//     them, adc dev bench record, comes with W18.
//   - Wall time uses controller event times only. Tokens and charges go
//     through ledger.Reconcile per stage; an uncertain call's best estimate
//     is listed as outstanding, never counted as zero nor as settled.
//   - It refuses rather than guesses: a measurement whose attempt is no
//     node's, an attempt of two nodes, an unknown stage, a node done before
//     it was claimed.
//   - It records and never gates: Compare reports the metrics that grew.
package bench

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
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

// Node is one node of a task: its stage, round, node.claimed time, and done
// time (result.selected; for merge the effect's observed or reconciled
// completed), zero while unfinished, with its attempts.
type Node struct {
	Stage    types.BenchmarkStageStage
	Round    int
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
	Interventions []Intervention
}

// Meta is a run's provenance.
type Meta struct {
	Checkpoint types.BenchmarkRunCheckpoint
	RecordedAt time.Time
	Arch       string // GOOS/GOARCH of the controller host
	Commit     types.GitID
	SchemaSet  types.Digest
	Routes     []types.BenchmarkRoute
	Fixtures   []types.BenchmarkFixture
}

// Record is the benchmark run of tasks.
func Record(m Meta, tasks []Task) (types.BenchmarkRun, error) {
	run := types.BenchmarkRun{Schema: schema, Checkpoint: m.Checkpoint, RecordedAt: types.Time(m.RecordedAt.UTC().Format(time.RFC3339)), Arch: m.Arch,
		BuildCommit: m.Commit, SchemaSet: m.SchemaSet, Routes: slices.Clone(m.Routes), Fixtures: slices.Clone(m.Fixtures), Tasks: []types.BenchmarkTask{}}
	for _, t := range tasks {
		bt, err := task(t)
		if err != nil {
			return types.BenchmarkRun{}, fmt.Errorf("bench: task %s: %w", t.ID, err)
		}
		run.Tasks = append(run.Tasks, bt)
	}
	return run, nil
}

func task(t Task) (types.BenchmarkTask, error) {
	bt := types.BenchmarkTask{Task: t.ID, Stages: []types.BenchmarkStage{}}
	switch {
	case !t.Closed.IsZero() && t.Closed.Before(t.Approved):
		return bt, errors.New("closed before its approval")
	case !t.Closed.IsZero():
		ms, o, r := t.Closed.Sub(t.Approved).Milliseconds(), t.Outcome, t.Reason
		bt.WallMs, bt.Outcome, bt.Reason = &ms, &o, &r
	case t.Hold != "":
		h := t.Hold
		bt.Hold = &h
	}
	stageOf := map[types.AttemptID]types.BenchmarkStageStage{}
	for _, n := range t.Nodes {
		if !slices.Contains(Stages, n.Stage) {
			return bt, fmt.Errorf("an unknown stage %q", n.Stage)
		} else if !n.Done.IsZero() && n.Done.Before(n.Claimed) {
			return bt, fmt.Errorf("a %s node done before it was claimed", n.Stage)
		}
		for _, a := range n.Attempts {
			if _, dup := stageOf[a]; dup {
				return bt, fmt.Errorf("attempt %s of two nodes", a)
			}
			stageOf[a] = n.Stage
		}
	}
	usage := map[types.BenchmarkStageStage][]types.UsageMeasurement{}
	for _, u := range t.Usage {
		s, ok := stageOf[u.Attempt]
		if !ok {
			return bt, fmt.Errorf("measurement %s of attempt %s, which is no node's", u.ID, u.Attempt)
		}
		usage[s] = append(usage[s], u)
	}
	for _, s := range Stages {
		st, used, err := stage(s, t, usage[s])
		if err != nil {
			return bt, err
		} else if used {
			bt.Stages = append(bt.Stages, st)
		}
	}
	for _, iv := range t.Interventions {
		bt.Rescues += b2i(iv.Kind == Rescue)
	}
	return bt, nil
}

// stage is stage s's metrics, and whether the task has any of it.
func stage(s types.BenchmarkStageStage, t Task, usage []types.UsageMeasurement) (types.BenchmarkStage, bool, error) {
	st := types.BenchmarkStage{Stage: s, Sources: []types.BenchmarkStageSourcesItem{}, Charge: []types.BenchmarkAmount{}, Outstanding: []types.BenchmarkAmount{}}
	rounds, used := map[int]bool{}, false
	for _, n := range t.Nodes {
		if n.Stage != s {
			continue
		}
		used, rounds[n.Round] = true, true
		st.Attempts += int64(len(n.Attempts))
		if n.Done.IsZero() {
			st.Unfinished++
		} else {
			st.WallMs += n.Done.Sub(n.Claimed).Milliseconds()
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
		default:
			return st, false, fmt.Errorf("an unknown intervention %q", iv.Kind)
		}
	}
	if len(usage) == 0 {
		return st, used, nil
	}
	calls, err := ledger.Reconcile(usage)
	if err != nil {
		return st, false, err
	}
	basis, sources := map[types.CallID]types.BenchmarkAmountBasis{}, map[types.BenchmarkStageSourcesItem]bool{}
	for _, u := range usage {
		basis[u.Call], sources[types.BenchmarkStageSourcesItem(u.SourceOfTruth)] = cmp.Or(basis[u.Call], types.BenchmarkAmountBasis(u.Cost.Basis)), true
	}
	st.Sources = slices.Sorted(maps.Keys(sources))
	settled, outstanding := map[[2]string]*types.BenchmarkAmount{}, map[[2]string]*types.BenchmarkAmount{}
	for _, id := range slices.Sorted(maps.Keys(calls)) {
		c := calls[id]
		st.Tokens.In, st.Tokens.Out = st.Tokens.In+c.Usage.In, st.Tokens.Out+c.Usage.Out
		st.Tokens.CacheRead, st.Tokens.CacheWrite = st.Tokens.CacheRead+c.Usage.CacheRead, st.Tokens.CacheWrite+c.Usage.CacheWrite
		into, k := settled, [2]string{string(basis[id]), c.Currency}
		if c.Uncertain {
			into = outstanding
		}
		if into[k] == nil {
			into[k] = &types.BenchmarkAmount{Basis: basis[id], Currency: c.Currency}
		}
		into[k].AmountMicros, into[k].Calls = into[k].AmountMicros+c.Usage.CostMicros, into[k].Calls+1
	}
	st.Charge, st.Outstanding = amounts(settled), amounts(outstanding)
	return st, true, nil
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
