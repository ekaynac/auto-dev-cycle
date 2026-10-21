// SPDX-License-Identifier: Apache-2.0

package bench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func id(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// measure is one usage report of call on attempt.
func measure(t *testing.T, call types.CallID, a types.AttemptID, kind types.UsageMeasurementKind, seq, in, out, micros int64, src types.UsageMeasurementSourceOfTruth) types.UsageMeasurement {
	return types.UsageMeasurement{ID: types.MeasurementID(id(t, "msr")), Call: call, Attempt: a, Lease: types.LeaseID(id(t, "lea")), Kind: kind, Sequence: seq,
		Tokens: types.UsageMeasurementTokens{In: in, Out: out}, Cost: types.UsageMeasurementCost{AmountMicros: micros, Currency: "USD", Basis: "cash"},
		SourceOfTruth: src}
}

// on is the attempts ids, each on executor e.
func on(e types.ExecutorID, ids ...types.AttemptID) []Attempt {
	out := make([]Attempt, 0, len(ids))
	for _, a := range ids {
		out = append(out, Attempt{ID: a, Executor: e})
	}
	return out
}

const builder, reviewer = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa"), types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")

func meta() Meta {
	price := types.RegistryPrice{InputUsdMicrosPerMtok: 1_000_000, OutputUsdMicrosPerMtok: 10_000_000}
	return Meta{Checkpoint: "a", RecordedAt: at(3600), Arch: "linux/arm64", Commit: types.GitID("git:" + strings.Repeat("a", 40)),
		SchemaSet: types.Digest("sha256:" + strings.Repeat("b", 64)),
		Routes:    []types.BenchmarkRoute{{Executor: builder, Role: "builder", Family: "stub-builder"}, {Executor: reviewer, Role: "reviewer", Family: "stub-reviewer"}},
		Prices:    map[types.ExecutorID]types.RegistryPrice{builder: price, reviewer: price},
		Fixtures:  []types.BenchmarkFixture{{Path: "fixtures/t0/go-mini/repo/go.mod", Digest: types.Digest("sha256:" + strings.Repeat("c", 64))}}}
}

// worked is a task of two build rounds (two attempts in the first),
// accept, gate and review, an approval at merge, closed clean: each figure
// below is exact.
func worked(t *testing.T) (Task, map[string]types.AttemptID) {
	a := map[string]types.AttemptID{}
	for _, n := range []string{"b1", "b1x", "b2", "r"} {
		a[n] = types.AttemptID(id(t, "att"))
	}
	c1, c2, c3, c4 := types.CallID(id(t, "cal")), types.CallID(id(t, "cal")), types.CallID(id(t, "cal")), types.CallID(id(t, "cal"))
	return Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Closed: at(900), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean,
		Nodes: []Node{
			{Stage: "build", Round: 1, Claimed: at(10), Done: at(110), Attempts: on(builder, a["b1"], a["b1x"])},
			{Stage: "accept", Round: 1, Claimed: at(110), Done: at(111)},
			{Stage: "build", Round: 2, Claimed: at(200), Done: at(260), Attempts: on(builder, a["b2"])},
			{Stage: "gate", Round: 1, Claimed: at(300), Done: at(340)},
			{Stage: "review", Round: 1, Claimed: at(400), Done: at(430), Attempts: on(reviewer, a["r"])},
			{Stage: "review", Round: 1, Claimed: at(440), Done: at(450)}, // a replacement keeps its round
		},
		Usage: []types.UsageMeasurement{
			measure(t, c1, a["b1"], "final", 0, 1000, 100, 3000, "adapter_report"),
			measure(t, c2, a["b1x"], "final", 0, 500, 50, 1500, "adapter_report"),
			measure(t, c3, a["b2"], "delta", 0, 700, 70, 2100, "adapter_report"), // no final: uncertain
			cached(measure(t, c4, a["r"], "final", 0, 2000, 10, 5000, "adapter_report")),
			// The gateway's counts, which the finals bear out: settled.
			measure(t, c1, a["b1"], "delta", 0, 1000, 100, 0, "gateway_count"),
			measure(t, c2, a["b1x"], "delta", 0, 500, 50, 0, "gateway_count"),
			measure(t, c4, a["r"], "delta", 0, 2000, 10, 0, "gateway_count"),
		},
		Calls: []Call{{ID: c1, Attempt: a["b1"]}, {ID: c2, Attempt: a["b1x"]}, {ID: c3, Attempt: a["b2"], Liability: 900}, {ID: c4, Attempt: a["r"]}},
		Interventions: []Intervention{{Stage: "build", Kind: RailRaise}, {Stage: "review", Kind: Answer}, {Stage: "build", Kind: Rescue},
			{Stage: "rebase", Kind: Rescue},   // counted apart: no rebase stage
			{Stage: "merge", Kind: Approval}}, // merge: interventions only
	}, a
}

// cached is m with cache reads and writes.
func cached(m types.UsageMeasurement) types.UsageMeasurement {
	m.Tokens.CacheRead, m.Tokens.CacheWrite = 300, 40
	return m
}

// many adds to tk Astra's 2,049 uncertain calls on attempt a: 2,048 of in
// tokens and micros reported USD, and one of at most 2,048 of each, so that
// a sum of M = 2^53-1 wraps to zero at 2^64.
func many(t *testing.T, tk *Task, a types.AttemptID, in, micros int64) {
	for i := range 2049 {
		c := types.CallID(id(t, "cal"))
		if i == 2048 {
			in, micros = min(in, 2048), min(micros, 2048)
		}
		tk.Calls = append(tk.Calls, Call{ID: c, Attempt: a})
		tk.Usage = append(tk.Usage, measure(t, c, a, "delta", 0, in, 0, micros, "adapter_report"))
	}
}

func TestRecord(t *testing.T) {
	tk, _ := worked(t)
	m := meta()
	m.RecordedAt = at(3600).In(time.FixedZone("TRT", 11*3600)) // recorded in UTC (the 21st at +11)
	run, err := Record(m, []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	if run.RecordedAt != "2026-10-20T13:00:00Z" || Path(run) != "benchmarks/m1/2026-10-20-linux-arm64-a.json" {
		t.Errorf("recorded at %s, to %s", run.RecordedAt, Path(run))
	}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw); err != nil {
		t.Fatalf("the run against its schema: %v\n%s", err, raw)
	}
	got := run.Tasks[0]
	if got.WallMs == nil || *got.WallMs != 900_000 || *got.Outcome != types.OutcomeEmpty || got.Reason.String() != "completed_clean" || got.Hold != nil || got.Rescues != 2 {
		t.Fatalf("the task: %+v", got)
	}
	stages := map[types.BenchmarkStageStage]types.BenchmarkStage{}
	var order []types.BenchmarkStageStage
	for _, s := range got.Stages {
		stages[s.Stage] = s
		order = append(order, s.Stage)
	}
	if strings.Join(strs(order), ",") != "build,accept,gate,review,merge" {
		t.Errorf("the stages %v", order)
	}
	b := stages["build"]
	if b.WallMs != 160_000 || b.Attempts != 3 || b.Rounds != 2 || b.Unfinished != 0 || b.Tokens.In != 2200 || b.Tokens.Out != 220 ||
		b.Interventions.RailRaises != 1 || b.Interventions.Answers != 0 {
		t.Errorf("build: %+v", b)
	}
	if len(b.Charge) != 1 || b.Charge[0].AmountMicros != 4500 || b.Charge[0].Calls != 2 || b.Charge[0].Basis != "cash" || b.Charge[0].Currency != "USD" {
		t.Errorf("build's charge (settled calls only): %+v", b.Charge)
	}
	if len(b.Outstanding) != 1 || b.Outstanding[0].AmountMicros != 2100 || b.Outstanding[0].Calls != 1 {
		t.Errorf("build's outstanding (the uncertain call, never zero): %+v", b.Outstanding)
	}
	if strings.Join(srcs(b.Sources), ",") != "adapter_report,gateway_count" {
		t.Errorf("build's sources: %v", b.Sources)
	}
	if a := stages["accept"]; a.WallMs != 1000 || a.Attempts != 0 || len(a.Charge)+len(a.Outstanding) != 0 || a.Tokens.In != 0 {
		t.Errorf("accept: %+v", a)
	}
	if r := stages["review"]; r.WallMs != 40_000 || r.Rounds != 1 || r.Attempts != 1 || r.Tokens.In != 2000 || r.Tokens.CacheRead != 300 || r.Tokens.CacheWrite != 40 ||
		r.Interventions.Answers != 1 || len(r.Charge) != 1 || r.Charge[0].AmountMicros != 5000 {
		t.Errorf("review: %+v", r)
	}
	if mg := stages["merge"]; mg.Interventions.Approvals != 1 || mg.Attempts != 0 || mg.WallMs != 0 {
		t.Errorf("merge (an approval, no node): %+v", mg)
	}
	s := Summary(run)
	for _, want := range []string{string(tk.ID), "empty/completed_clean",
		"| | build | 160.000 | 0 | 2200 / 220 / 0 / 0 | adapter_report, gateway_count | 0.004500 USD cash (2 calls) | 0.002100 USD cash (1 calls) | 3 | 2 | 0 / 0 / 1 |",
		"| | merge | 0.000 | 0 | 0 / 0 / 0 / 0 | – | – | – | 0 | 0 | 1 / 0 / 0 |", "rescues | 2"} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary lacks %q:\n%s", want, s)
		}
	}
}

func srcs(ss []types.BenchmarkStageSourcesItem) (out []string) {
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}

func strs(ss []types.BenchmarkStageStage) (out []string) {
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}

// A held task: its hold, no closure and no wall time; an unfinished node is
// counted apart from the stage's wall time, and a node never claimed (Opus
// r2 M3: admission-held) apart as pending, in no round.
func TestRecordHeld(t *testing.T) {
	tk, _ := worked(t)
	tk.Closed, tk.Hold = time.Time{}, "budget_rail"
	tk.Nodes = append(tk.Nodes, Node{Stage: "build", Round: 3, Claimed: at(500)}, Node{Stage: "build", Round: 4})
	run, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	got := run.Tasks[0]
	if got.WallMs != nil || got.Outcome != nil || got.Reason != nil || got.Hold == nil || *got.Hold != "budget_rail" {
		t.Fatalf("a held task: %+v", got)
	}
	if b := got.Stages[0]; b.Unfinished != 1 || b.Pending != 1 || b.WallMs != 160_000 || b.Rounds != 3 {
		t.Errorf("an unfinished node: %+v", b)
	}
	if s := Summary(run); !strings.Contains(s, "**held: budget_rail**") || !strings.Contains(s, "| | build | 160.000 | 1 (+1 pending) |") {
		t.Errorf("the summary of a held task:\n%s", s)
	}
	raw, _ := json.Marshal(run)
	if _, err := contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw); err != nil {
		t.Fatalf("a held run against its schema: %v", err)
	}
}

// It refuses rather than guesses.
func TestRecordRefuses(t *testing.T) {
	gw := func(c types.CallID, a types.AttemptID) types.UsageMeasurement {
		return measure(t, c, a, "delta", 0, 10, 1, 0, "gateway_count")
	}
	for _, row := range []struct {
		name, want string
		edit       func(*Meta, *[]Task, map[string]types.AttemptID)
	}{
		{"a measurement of no node's attempt", "no node's", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Usage = append((*ts)[0].Usage, measure(t, types.CallID(id(t, "cal")), types.AttemptID(id(t, "att")), "final", 0, 1, 1, 1, "adapter_report"))
		}},
		{"an attempt of two nodes", "two nodes", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			(*ts)[0].Nodes[1].Attempts = on(builder, a["b1"])
		}},
		{"an unknown stage", "unknown stage", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Nodes[1].Stage = "deploy" }},
		{"done before it was claimed", "before it was claimed", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Nodes[1].Done = at(100) }},
		{"no claimed time", "no claimed time", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Nodes[1].Claimed = time.Time{} }},
		{"closed before its approval", "before its approval", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Closed = at(-1) }},
		{"no approval time", "no approval time", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Approved = time.Time{} }},
		{"closed and held", "closed and held", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Hold = "budget_rail" }},
		{"neither closed nor held", "neither", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Closed = time.Time{} }},
		{"closed without an outcome", "without an outcome", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Outcome = "" }},
		{"closed without a reason", "without an outcome", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Reason = types.ReasonCode{} }},
		{"an unknown hold", "unknown hold", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Closed, (*ts)[0].Hold = time.Time{}, "a|b"
		}},
		{"an unknown intervention", "unknown intervention", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Interventions[0].Kind = "nudge" }},
		{"an unknown intervention at an unknown stage", "unknown intervention", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Interventions = append((*ts)[0].Interventions, Intervention{Stage: "deploy", Kind: "nudge"})
		}},
		{"an approval at no stage", "unknown stage", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Interventions = append((*ts)[0].Interventions, Intervention{Kind: Approval})
		}},
		{"a call in two currencies", "", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			m := measure(t, (*ts)[0].Usage[0].Call, a["b1"], "delta", 1, 1, 1, 1, "adapter_report")
			m.Cost.Currency = "EUR"
			(*ts)[0].Usage = append((*ts)[0].Usage, m)
		}},
		{"a call in two bases", "bases", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			m := measure(t, (*ts)[0].Usage[0].Call, a["b1"], "delta", 1, 1, 1, 1, "adapter_report")
			m.Cost.Basis = "subscription_equivalent"
			(*ts)[0].Usage = append((*ts)[0].Usage, m)
		}},
		{"a call in two stages", "two stages", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			(*ts)[0].Usage = append((*ts)[0].Usage, measure(t, (*ts)[0].Usage[2].Call, a["r"], "final", 1, 700, 70, 2100, "adapter_report"))
		}},
		{"a call in two stages, in another currency", "two stages", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			m := measure(t, (*ts)[0].Usage[2].Call, a["r"], "final", 1, 700, 70, 2100, "adapter_report")
			m.Cost.Currency = "EUR"
			(*ts)[0].Usage = append((*ts)[0].Usage, m)
		}},
		{"a call on two executors", "two executors", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			x := types.AttemptID(id(t, "att"))
			(*ts)[0].Nodes[5].Attempts = on(builder, x)
			(*ts)[0].Usage = append((*ts)[0].Usage, gw((*ts)[0].Usage[3].Call, x))
		}},
		{"a settled call in no currency", "the run", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Usage[0].Cost.Currency = ""
		}},
		{"a node on no route's executor", "no route's", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Nodes[0].Attempts[0].Executor = "exe_01m4tm99haemqt8es2qfbwj8pb"
		}},
		{"a route without a price", "no price", func(m *Meta, _ *[]Task, _ map[string]types.AttemptID) { delete(m.Prices, reviewer) }},
		{"an empty run", "no tasks", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { *ts = nil }},
		{"a task twice", "twice", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			u, _ := worked(t)
			u.ID = (*ts)[0].ID
			*ts = append(*ts, u)
		}},
		{"an attempt in two tasks", "twice", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			*ts = append(*ts, Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
				Nodes: []Node{{Stage: "build", Claimed: at(1), Attempts: on(builder, a["b2"])}}})
		}},
		{"a call in two tasks", "twice", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			x := types.AttemptID(id(t, "att"))
			*ts = append(*ts, Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
				Nodes: []Node{{Stage: "build", Claimed: at(1), Attempts: on(builder, x)}},
				Usage: []types.UsageMeasurement{gw((*ts)[0].Usage[0].Call, x)}, Calls: []Call{{ID: (*ts)[0].Usage[0].Call, Attempt: x}}})
		}},
		// The review of #313 by Astra (docs/reviews/2026-10-21-pr313-astra.md).
		{"H1: a zero-token final, no liability", "no positive liability", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			c := types.CallID(id(t, "cal"))
			(*ts)[0].Calls = append((*ts)[0].Calls, Call{ID: c, Attempt: a["r"]})
			(*ts)[0].Usage = append((*ts)[0].Usage, measure(t, c, a["r"], "final", 0, 0, 0, 0, "adapter_report"))
		}},
		{"H2: a crashed attempt's call, no liability", "no positive liability", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			(*ts)[0].Calls = append((*ts)[0].Calls, Call{ID: types.CallID(id(t, "cal")), Attempt: a["r"]})
		}},
		{"H2: a measured call not bound", "no bound call", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Calls = (*ts)[0].Calls[1:]
		}},
		{"H2: a call bound twice", "bound twice", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Calls = append((*ts)[0].Calls, (*ts)[0].Calls[0])
		}},
		{"H2: a bound call of no node's attempt", "no node's", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Calls = append((*ts)[0].Calls, Call{ID: types.CallID(id(t, "cal")), Attempt: types.AttemptID(id(t, "att"))})
		}},
		{"H2: a negative liability", "negative liability", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Calls[2].Liability = -1
		}},
		{"H3: an unsettled 1 EUR-micro delta", "cannot be compared", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			c := types.CallID(id(t, "cal"))
			m := measure(t, c, a["r"], "delta", 0, 5000, 500, 1, "adapter_report")
			m.Cost.Currency = "EUR"
			(*ts)[0].Calls = append((*ts)[0].Calls, Call{ID: c, Attempt: a["r"]})
			(*ts)[0].Usage = append((*ts)[0].Usage, m)
		}},
		{"H4: 2,049 calls whose money sums past the maximum", "sum past", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			many(t, &(*ts)[0], a["r"], 1, maxSum)
		}},
		{"H4: 2,049 calls whose tokens sum past the maximum", "sum past", func(m *Meta, ts *[]Task, a map[string]types.AttemptID) {
			m.Prices[reviewer] = types.RegistryPrice{OutputUsdMicrosPerMtok: 10_000_000}
			many(t, &(*ts)[0], a["r"], maxSum, 1)
		}},
		{"M5: a priced executor no route declares", "no route's", func(m *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			x := types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pb")
			m.Prices[x] = m.Prices[builder]
			(*ts)[0].Nodes[0].Attempts[0].Executor = x
		}},
		{"M5: a route declared twice, conflicting", "conflicting", func(m *Meta, _ *[]Task, _ map[string]types.AttemptID) {
			m.Routes = append(m.Routes, types.BenchmarkRoute{Executor: builder, Role: "reviewer", Family: "stub-builder"})
		}},
		// Opus's re-check of #313 (docs/reviews/2026-10-21-pr313-opus-r2.md).
		{"M1: a settled cash call in EUR", "a settled EUR", func(_ *Meta, ts *[]Task, a map[string]types.AttemptID) {
			c := types.CallID(id(t, "cal"))
			f, g := measure(t, c, a["r"], "final", 0, 100, 10, 300, "adapter_report"), measure(t, c, a["r"], "delta", 0, 100, 10, 0, "gateway_count")
			f.Cost.Currency, g.Cost.Currency = "EUR", "EUR"
			(*ts)[0].Calls = append((*ts)[0].Calls, Call{ID: c, Attempt: a["r"]})
			(*ts)[0].Usage = append((*ts)[0].Usage, f, g)
		}},
		{"M3: an attempted node never claimed", "no claimed time", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Nodes[2].Claimed, (*ts)[0].Nodes[2].Done = time.Time{}, time.Time{}
		}},
		{"L1: closed after the run was recorded", "after the run was recorded", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Closed = at(3601) }},
		{"L1: a node done after the run was recorded", "after the run was recorded", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Nodes[5].Done = at(3601) }},
		{"L1: a task's duration past the maximum", "past the maximum", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Approved = time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC)
		}},
		{"L1: a node's duration past the maximum", "past the maximum", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Nodes[3].Claimed = time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC)
		}},
		{"L2: a negative cost", "negative cost", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) { (*ts)[0].Usage[1].Cost.AmountMicros = -1 }},
		{"L3: an attempt on no route's executor beside one on a route", "no route's", func(_ *Meta, ts *[]Task, _ map[string]types.AttemptID) {
			(*ts)[0].Nodes[0].Attempts[1].Executor = "exe_01m4tm99haemqt8es2qfbwj8pb"
		}},
		{"a run its schema refuses", "the run", func(m *Meta, _ *[]Task, _ map[string]types.AttemptID) { m.Arch = "linux_arm64" }},
	} {
		tk, a := worked(t)
		m, ts := meta(), []Task{tk}
		row.edit(&m, &ts, a)
		if _, err := Record(m, ts); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: %v", row.name, err)
		}
	}
}

// A settled call, on any basis, is charged no less than its standing figure
// at its route's registry price (Opus r2 M1, Mythos H1: the ledger never
// trusts a reported cost nor its basis label); a zero-priced route leaves no
// floor, and there a settled call in another currency keeps its amount.
func TestRecordSettledFloor(t *testing.T) {
	basis := func(b string, ms ...types.UsageMeasurement) []types.UsageMeasurement {
		ms[0].Cost.Basis = types.UsageMeasurementCostBasis(b)
		return ms
	}
	eur := func(ms ...types.UsageMeasurement) []types.UsageMeasurement {
		for i := range ms {
			ms[i].Cost.Currency = "EUR"
		}
		return ms
	}
	for _, row := range []struct {
		name     string
		usage    func(types.CallID, types.AttemptID) []types.UsageMeasurement
		free     bool
		basis    types.BenchmarkAmountBasis
		currency string
		micros   int64
	}{
		{"a zero final the count bears out", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "final", 0, 2000, 10, 0, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count")}
		}, false, "cash", "USD", 2100},
		{"a zero provider reconciliation below the count", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "provider_reconciliation", 0, 0, 0, 0, "provider_usage"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count")}
		}, false, "cash", "USD", 2100},
		{"a reported cost above the price kept", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "final", 0, 2000, 10, 9000, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count")}
		}, false, "cash", "USD", 9000},
		// Mythos r2 L1: a charge the floor raised is a cash valuation, under cash.
		{"a zero subscription-equivalent final, floored under cash", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return basis("subscription_equivalent", measure(t, c, a, "final", 0, 2000, 10, 0, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count"))
		}, false, "cash", "USD", 2100},
		// Mythos H1: Opus's input 1, its adapter labelling it free.
		{"a zero final labelled free on a priced route, floored under cash", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return basis("free", measure(t, c, a, "final", 0, 2000, 10, 0, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count"))
		}, false, "cash", "USD", 2100},
		{"a subscription-equivalent cost above the floor keeps its basis", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return basis("subscription_equivalent", measure(t, c, a, "final", 0, 2000, 10, 9000, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count"))
		}, false, "subscription_equivalent", "USD", 9000},
		{"a zero free final on a zero-priced route", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return basis("free", measure(t, c, a, "final", 0, 2000, 10, 0, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count"))
		}, true, "free", "USD", 0},
		{"a settled EUR call on a zero-priced route kept", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return eur(measure(t, c, a, "final", 0, 2000, 10, 50, "adapter_report"), measure(t, c, a, "delta", 0, 2000, 10, 0, "gateway_count"))
		}, true, "cash", "EUR", 50},
	} {
		a, c := types.AttemptID(id(t, "att")), types.CallID(id(t, "cal"))
		tk := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
			Nodes: []Node{{Stage: "review", Claimed: at(1), Attempts: on(reviewer, a)}}, Usage: row.usage(c, a), Calls: []Call{{ID: c, Attempt: a}}}
		m := meta()
		if row.free {
			m.Prices[reviewer] = types.RegistryPrice{}
		}
		run, err := Record(m, []Task{tk})
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		r := run.Tasks[0].Stages[0]
		if r.Tokens.In != 2000 || r.Tokens.Out != 10 || len(r.Outstanding) != 0 ||
			len(r.Charge) != 1 || r.Charge[0] != (types.BenchmarkAmount{Basis: row.basis, Currency: row.currency, AmountMicros: row.micros, Calls: 1}) {
			t.Errorf("%s: %+v", row.name, r)
		}
	}
}

// Opus r2 L3: each attempt carries its executor, so a node's replacement
// attempt on another route is priced at that route's price.
func TestRecordAttemptExecutor(t *testing.T) {
	m := meta()
	m.Prices[reviewer] = types.RegistryPrice{InputUsdMicrosPerMtok: 2_000_000, OutputUsdMicrosPerMtok: 20_000_000}
	a, b, c := types.AttemptID(id(t, "att")), types.AttemptID(id(t, "att")), types.CallID(id(t, "cal"))
	tk := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
		Nodes: []Node{{Stage: "review", Claimed: at(1), Attempts: append(on(builder, a), on(reviewer, b)...)}},
		Usage: []types.UsageMeasurement{measure(t, c, b, "delta", 0, 700, 70, 0, "adapter_report")}, Calls: []Call{{ID: c, Attempt: b}}}
	run, err := Record(m, []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	if r := run.Tasks[0].Stages[0]; len(r.Outstanding) != 1 || r.Outstanding[0].AmountMicros != 2800 || r.Attempts != 2 {
		t.Errorf("the replacement attempt's price: %+v", r)
	}
}

// A call's standing decides charge or outstanding (ledger.StandingOf): a
// call only the gateway counted, or whose final is under the gateway's
// count, is outstanding at its tokens' registry price, its tokens the
// count; one whose reported cost is lower is raised to that price.
func TestRecordStanding(t *testing.T) {
	for _, row := range []struct {
		name      string
		usage     func(types.CallID, types.AttemptID) []types.UsageMeasurement
		in        int64
		micros    int64
		liability int64
	}{
		{"only the gateway's count", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "delta", 0, 5000, 500, 0, "gateway_count")}
		}, 5000, 10_000, 0},
		{"a zero final under the count", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "final", 0, 0, 0, 0, "adapter_report"), measure(t, c, a, "delta", 0, 5000, 500, 0, "gateway_count")}
		}, 5000, 10_000, 0},
		{"a delta reported below its price", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "delta", 0, 700, 70, 100, "adapter_report")}
		}, 700, 1400, 0},
		// Astra's H1: a zero-token final takes its reservation-backed liability.
		{"a zero-token final at its liability", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "final", 0, 0, 0, 0, "adapter_report")}
		}, 0, 4000, 4000},
		// Astra's H2: a crashed attempt's call, unmeasured, at its liability.
		{"an unmeasured call at its liability", func(types.CallID, types.AttemptID) []types.UsageMeasurement { return nil }, 0, 5000, 5000},
		// A liability above the price wins.
		{"a liability above its price", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			return []types.UsageMeasurement{measure(t, c, a, "delta", 0, 700, 70, 100, "adapter_report")}
		}, 700, 3000, 3000},
		// Mythos L2: an unsettled subscription-equivalent call's floor, a
		// cash valuation, is recorded under cash.
		{"an unsettled subscription-equivalent delta, under cash", func(c types.CallID, a types.AttemptID) []types.UsageMeasurement {
			m := measure(t, c, a, "delta", 0, 700, 70, 0, "adapter_report")
			m.Cost.Basis = "subscription_equivalent"
			return []types.UsageMeasurement{m}
		}, 700, 1400, 0},
	} {
		a := types.AttemptID(id(t, "att"))
		c := types.CallID(id(t, "cal"))
		tk := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
			Nodes: []Node{{Stage: "review", Claimed: at(1), Attempts: on(reviewer, a)}}, Usage: row.usage(c, a), Calls: []Call{{ID: c, Attempt: a, Liability: row.liability}}}
		run, err := Record(meta(), []Task{tk})
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		r := run.Tasks[0].Stages[0]
		if r.Tokens.In != row.in || len(r.Charge) != 0 || len(r.Outstanding) != 1 ||
			r.Outstanding[0] != (types.BenchmarkAmount{Basis: "cash", Currency: "USD", AmountMicros: row.micros, Calls: 1}) {
			t.Errorf("%s: %+v", row.name, r)
		}
	}
}

// A call's basis is its adapter's, whatever order the gateway's reports
// (always cash) arrive in.
func TestRecordBasis(t *testing.T) {
	a, c := types.AttemptID(id(t, "att")), types.CallID(id(t, "cal"))
	final := measure(t, c, a, "final", 0, 1000, 100, 3000, "adapter_report")
	final.Cost.Basis = "subscription_equivalent"
	count := measure(t, c, a, "delta", 0, 1000, 100, 0, "gateway_count")
	for _, usage := range [][]types.UsageMeasurement{{final, count}, {count, final}} {
		tk := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail",
			Nodes: []Node{{Stage: "build", Claimed: at(1), Attempts: on(builder, a)}}, Usage: usage, Calls: []Call{{ID: c, Attempt: a}}}
		run, err := Record(meta(), []Task{tk})
		if err != nil {
			t.Fatal(err)
		}
		if b := run.Tasks[0].Stages[0]; len(b.Charge) != 1 || b.Charge[0] != (types.BenchmarkAmount{Basis: "subscription_equivalent", Currency: "USD", AmountMicros: 3000, Calls: 1}) {
			t.Errorf("the charge: %+v", b)
		}
	}
}

// Compare reports exactly what grew and an outcome that changed, and
// never gates (it returns deltas, no error).
func TestCompare(t *testing.T) {
	tk, a := worked(t)
	prev, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	if d := Compare(prev, prev); len(d) != 0 {
		t.Errorf("a run against itself: %+v", d)
	}
	tk.Nodes[0].Done = at(150) // build's wall time grows by 40 s
	tk.Nodes[3].Done = at(320) // gate's shrinks: no delta
	c := types.CallID(id(t, "cal"))
	tk.Usage = append(tk.Usage, measure(t, c, a["r"], "final", 0, 10, 1, 7, "adapter_report"), measure(t, c, a["r"], "delta", 0, 10, 1, 0, "gateway_count"))
	tk.Calls = append(tk.Calls, Call{ID: c, Attempt: a["r"]})
	tk.Closed, tk.Hold = time.Time{}, "budget_rail"
	other := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Hold: "budget_rail"}
	cur, err := Record(meta(), []Task{tk, other})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range Compare(prev, cur) {
		got = append(got, string(d.Stage)+":"+d.Metric+":"+d.Was+"->"+d.Is)
	}
	want := []string{":outcome:empty/completed_clean->held: budget_rail", "build:wall_ms:160000->200000",
		// The added call reports 7 micros, floored at its price, 10 + 10 micros.
		"review:tokens.in:2000->2010", "review:tokens.out:10->11", "review:charge_micros cash USD:5000->5020", ":task:absent->present"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("deltas:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d := Compare(cur, prev); len(d) == 0 || d[len(d)-1].Metric != "task" || d[len(d)-1].Is != "absent" {
		t.Errorf("a task gone: %+v", d)
	}
	late, _ := worked(t) // the task's own wall time grew
	late.ID, late.Closed = prev.Tasks[0].Task, at(950)
	later, err := Record(meta(), []Task{late})
	if err != nil {
		t.Fatal(err)
	}
	if d := Compare(prev, later); len(d) != 1 || d[0].Stage != "" || d[0].Metric != "wall_ms" || d[0].Was != "900000" || d[0].Is != "950000" {
		t.Errorf("a task's wall time grew: %+v", d)
	}
}

// Compare reports each metric that grew, charges and liabilities per basis
// and currency; Deltas renders them, escaping a cell.
func TestCompareMetrics(t *testing.T) {
	tk, _ := worked(t)
	prev, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	x := types.AttemptID(id(t, "att"))
	tk.Nodes = append(tk.Nodes, Node{Stage: "build", Round: 3, Claimed: at(500), Attempts: on(builder, x)})
	cx := types.CallID(id(t, "cal"))
	tk.Usage, tk.Calls = append(tk.Usage, measure(t, cx, x, "delta", 0, 100, 10, 300, "adapter_report")), append(tk.Calls, Call{ID: cx, Attempt: x})
	tk.Interventions = append(tk.Interventions, Intervention{Stage: "review", Kind: Answer}, Intervention{Kind: Rescue})
	tk.Nodes = append(tk.Nodes, Node{Stage: "gate", Round: 2}) // planned, never claimed
	cur, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range Compare(prev, cur) {
		got = append(got, string(d.Stage)+":"+d.Metric+":"+d.Was+"->"+d.Is)
	}
	want := []string{":rescues:2->3", "build:unfinished:0->1", "build:attempts:3->4", "build:rounds:2->3", "build:tokens.in:2200->2300",
		"build:tokens.out:220->230", "build:outstanding_micros cash USD:2100->2400", "gate:pending:0->1", "review:interventions:1->2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("deltas:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	usd := func(c string, n int64) types.BenchmarkAmount {
		return types.BenchmarkAmount{Basis: "cash", Currency: c, AmountMicros: n, Calls: 1}
	}
	if d := amountsGrew("t", "build", "charge_micros", []types.BenchmarkAmount{usd("USD", 10), usd("EUR", 50)}, []types.BenchmarkAmount{usd("USD", 20)}); len(d) != 1 ||
		d[0] != (Delta{"t", "build", "charge_micros cash USD", "10", "20"}) {
		t.Errorf("a rise in one currency behind a fall in another: %+v", d)
	}
	if s := Deltas([]Delta{{"t", "", "outcome", "a|b", "open"}}); !strings.Contains(s, "| `t` | – | outcome | a\\|b | open |") {
		t.Errorf("the deltas:\n%s", s)
	}
	if s := Deltas(nil); !strings.Contains(s, "No metric grew") {
		t.Errorf("no deltas: %s", s)
	}
}
