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

func meta() Meta {
	return Meta{Checkpoint: "a", RecordedAt: t0, Arch: "linux/arm64", Commit: types.GitID("git:" + strings.Repeat("a", 40)),
		SchemaSet: types.Digest("sha256:" + strings.Repeat("b", 64)),
		Routes: []types.BenchmarkRoute{{Executor: "exe_01m4tm99haemqt8es2qfbwj8pa", Role: "builder", Family: "stub-builder"},
			{Executor: "exe_01m4tm99haemr8ajyf09v4ytzb", Role: "reviewer", Family: "stub-reviewer"}},
		Fixtures: []types.BenchmarkFixture{{Path: "fixtures/t0/go-mini/repo/go.mod", Digest: types.Digest("sha256:" + strings.Repeat("c", 64))}}}
}

// worked is a task of two build rounds (two attempts in the first),
// accept, gate and review, closed clean: each figure below is exact.
func worked(t *testing.T) (Task, map[string]types.AttemptID) {
	a := map[string]types.AttemptID{}
	for _, n := range []string{"b1", "b1x", "b2", "r"} {
		a[n] = types.AttemptID(id(t, "att"))
	}
	c1, c2, c3, c4 := types.CallID(id(t, "cal")), types.CallID(id(t, "cal")), types.CallID(id(t, "cal")), types.CallID(id(t, "cal"))
	return Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0), Closed: at(900), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean,
		Nodes: []Node{
			{Stage: "build", Round: 1, Claimed: at(10), Done: at(110), Attempts: []types.AttemptID{a["b1"], a["b1x"]}},
			{Stage: "accept", Round: 1, Claimed: at(110), Done: at(111)},
			{Stage: "build", Round: 2, Claimed: at(200), Done: at(260), Attempts: []types.AttemptID{a["b2"]}},
			{Stage: "gate", Round: 1, Claimed: at(300), Done: at(340)},
			{Stage: "review", Round: 1, Claimed: at(400), Done: at(430), Attempts: []types.AttemptID{a["r"]}},
			{Stage: "review", Round: 1, Claimed: at(440), Done: at(450)}, // a replacement keeps its round
		},
		Usage: []types.UsageMeasurement{
			measure(t, c1, a["b1"], "final", 0, 1000, 100, 3000, "adapter_report"),
			measure(t, c2, a["b1x"], "final", 0, 500, 50, 1500, "adapter_report"),
			measure(t, c3, a["b2"], "delta", 0, 700, 70, 2100, "adapter_report"), // no final: uncertain
			cached(measure(t, c4, a["r"], "final", 0, 2000, 10, 5000, "adapter_report")),
		},
		Interventions: []Intervention{{Stage: "build", Kind: RailRaise}, {Stage: "review", Kind: Answer}, {Stage: "build", Kind: Rescue}},
	}, a
}

// cached is m with cache reads and writes.
func cached(m types.UsageMeasurement) types.UsageMeasurement {
	m.Tokens.CacheRead, m.Tokens.CacheWrite = 300, 40
	return m
}

func TestRecord(t *testing.T) {
	tk, _ := worked(t)
	run, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw); err != nil {
		t.Fatalf("the run against its schema: %v\n%s", err, raw)
	}
	got := run.Tasks[0]
	if got.WallMs == nil || *got.WallMs != 900_000 || *got.Outcome != types.OutcomeEmpty || got.Reason.String() != "completed_clean" || got.Hold != nil || got.Rescues != 1 {
		t.Fatalf("the task: %+v", got)
	}
	stages := map[types.BenchmarkStageStage]types.BenchmarkStage{}
	var order []types.BenchmarkStageStage
	for _, s := range got.Stages {
		stages[s.Stage] = s
		order = append(order, s.Stage)
	}
	if strings.Join(strs(order), ",") != "build,accept,gate,review" {
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
	if len(b.Sources) != 1 || b.Sources[0] != "adapter_report" {
		t.Errorf("build's sources: %v", b.Sources)
	}
	if a := stages["accept"]; a.WallMs != 1000 || a.Attempts != 0 || len(a.Charge)+len(a.Outstanding) != 0 || a.Tokens.In != 0 {
		t.Errorf("accept: %+v", a)
	}
	if r := stages["review"]; r.WallMs != 40_000 || r.Rounds != 1 || r.Attempts != 1 || r.Tokens.In != 2000 || r.Tokens.CacheRead != 300 || r.Tokens.CacheWrite != 40 ||
		r.Interventions.Answers != 1 || len(r.Charge) != 1 || r.Charge[0].AmountMicros != 5000 {
		t.Errorf("review: %+v", r)
	}
	s := Summary(run)
	for _, want := range []string{string(tk.ID), "empty/completed_clean", "| | build | 160.000 | 2200 / 220 / 0 / 0 | 0.004500 USD cash (2 calls) | 0.002100 USD cash (1 calls) | 3 | 2 |", "rescues | 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary lacks %q:\n%s", want, s)
		}
	}
}

func strs(ss []types.BenchmarkStageStage) (out []string) {
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}

// A held task: its hold, no closure and no wall time; an unfinished node is
// counted apart from the stage's wall time.
func TestRecordHeld(t *testing.T) {
	tk, _ := worked(t)
	tk.Closed, tk.Hold = time.Time{}, "budget_rail"
	tk.Nodes = append(tk.Nodes, Node{Stage: "build", Round: 3, Claimed: at(500)})
	run, err := Record(meta(), []Task{tk})
	if err != nil {
		t.Fatal(err)
	}
	got := run.Tasks[0]
	if got.WallMs != nil || got.Outcome != nil || got.Reason != nil || got.Hold == nil || *got.Hold != "budget_rail" {
		t.Fatalf("a held task: %+v", got)
	}
	if b := got.Stages[0]; b.Unfinished != 1 || b.WallMs != 160_000 || b.Rounds != 3 {
		t.Errorf("an unfinished node: %+v", b)
	}
	raw, _ := json.Marshal(run)
	if _, err := contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw); err != nil {
		t.Fatalf("a held run against its schema: %v", err)
	}
}

// It refuses rather than guesses.
func TestRecordRefuses(t *testing.T) {
	for name, edit := range map[string]func(*Task, map[string]types.AttemptID){
		"a measurement of no node's attempt": func(tk *Task, _ map[string]types.AttemptID) {
			tk.Usage = append(tk.Usage, measure(t, types.CallID(id(t, "cal")), types.AttemptID(id(t, "att")), "final", 0, 1, 1, 1, "adapter_report"))
		},
		"an attempt of two nodes":    func(tk *Task, a map[string]types.AttemptID) { tk.Nodes[1].Attempts = []types.AttemptID{a["b1"]} },
		"an unknown stage":           func(tk *Task, _ map[string]types.AttemptID) { tk.Nodes[1].Stage = "deploy" },
		"done before it was claimed": func(tk *Task, _ map[string]types.AttemptID) { tk.Nodes[1].Done = at(100) },
		"closed before its approval": func(tk *Task, _ map[string]types.AttemptID) { tk.Closed = at(-1) },
		"an unknown intervention":    func(tk *Task, _ map[string]types.AttemptID) { tk.Interventions[0].Kind = "nudge" },
		"a call in two currencies": func(tk *Task, a map[string]types.AttemptID) {
			m := measure(t, tk.Usage[0].Call, a["b1"], "delta", 1, 1, 1, 1, "adapter_report")
			m.Cost.Currency = "EUR"
			tk.Usage = append(tk.Usage, m)
		},
	} {
		tk, a := worked(t)
		edit(&tk, a)
		if _, err := Record(meta(), []Task{tk}); err == nil {
			t.Errorf("%s: recorded", name)
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
	tk.Usage = append(tk.Usage, measure(t, types.CallID(id(t, "cal")), a["r"], "final", 0, 10, 1, 7, "adapter_report"))
	tk.Closed, tk.Hold = time.Time{}, "budget_rail"
	other := Task{ID: types.TaskID(id(t, "tsk")), Approved: at(0)}
	cur, err := Record(meta(), []Task{tk, other})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range Compare(prev, cur) {
		got = append(got, string(d.Stage)+":"+d.Metric+":"+d.Was+"->"+d.Is)
	}
	want := []string{":outcome:empty/completed_clean->held: budget_rail", "build:wall_ms:160000->200000",
		"review:tokens.in:2000->2010", "review:tokens.out:10->11", "review:charge_micros:5000->5007", ":task:absent->present"}
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
