// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/bench"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The stub registry's builder and reviewer (fixtures/m1/config/registry-stub.json).
const (
	stubBuilder  = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa")
	stubReviewer = types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")
)

func stubReg(t *testing.T) types.ExecutorRegistry {
	t.Helper()
	reg, err := registry.Load("../../fixtures/m1/config/registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// entryOf is the digest of reg's entry of id, as Match records it.
func entryOf(reg types.ExecutorRegistry, id types.ExecutorID) *types.Digest {
	for _, e := range reg.Executors {
		if e.ID == id {
			d := types.Digest(dispatch.EntryDigest(e))
			return &d
		}
	}
	return nil
}

func priceOf(reg types.ExecutorRegistry, id types.ExecutorID) *types.RegistryPrice {
	for _, e := range reg.Executors {
		if e.ID == id {
			p := *e.Price
			return &p
		}
	}
	return nil
}

func at(s string) *types.Time { v := types.Time(s); return &v }

// delta is an adapter's delta of call: it settles nothing.
func delta(call types.CallID, attempt types.AttemptID) types.UsageMeasurement {
	return types.UsageMeasurement{ID: "msr_01m5ne0000e0000000000000aa", Call: call, Attempt: attempt, Kind: types.UsageMeasurementKindDelta, Sequence: 1,
		Tokens: types.UsageMeasurementTokens{In: 100, Out: 50}, Cost: types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
}

// candidateRead is a task read of a candidate closed by its review's go:
// the plan, a build (two calls on one lease), its accept and gate, the
// review's requirement and the review; an answer to the review, a raise
// that released the build and one that released none.
func candidateRead(t *testing.T) types.TaskRead {
	t.Helper()
	reg := stubReg(t)
	empty, clean := types.OutcomeEmpty, vocab.ReasonCompletedClean
	found, findings := types.OutcomeFound, vocab.ReasonCompletedWithFindings
	node := func(id string, kind types.BenchNodeKind, claimed, selected string, attempts ...types.BenchAttempt) types.BenchNode {
		n := types.BenchNode{ID: types.NodeID(id), Kind: kind, Round: 1, State: types.BenchNodeStateCompleted, Outcome: &empty, Reason: &clean, Attempts: append([]types.BenchAttempt{}, attempts...)}
		if claimed != "" {
			n.ClaimedAt, n.SelectedAt = at(claimed), at(selected)
		}
		return n
	}
	build := node("nod_build", types.BenchNodeKindBuild, "2026-10-23T10:00:10Z", "2026-10-23T10:01:10Z", types.BenchAttempt{ID: "att_build", Lease: "lea_build"})
	build.Outcome, build.Reason = &found, &findings
	owed := int64(4000)
	zero := int64(0)
	done := types.TaskBenchDoneAsValueCandidate
	id, err := ids.New("tsk")
	if err != nil {
		t.Fatal(err)
	}
	return types.TaskRead{ID: types.TaskID(id), Project: "prj_01m5ne0000e0000000000000aa",
		Card: types.BoardCard{Variant: &types.BoardPlacedCard{Type: "placed", Column: types.BoardPlacedCardColumnDoneCandidate}},
		Bench: &types.TaskBench{ApprovedAt: at("2026-10-23T10:00:00Z"), DoneAs: &done,
			Closure: &types.BenchClosure{Node: "nod_review", SelectedAt: "2026-10-23T10:03:00Z", Outcome: empty, Reason: clean},
			Nodes: []types.BenchNode{node("nod_plan", types.BenchNodeKindPlan, "", ""), build,
				node("nod_accept", types.BenchNodeKindAccept, "2026-10-23T10:01:20Z", "2026-10-23T10:01:30Z"),
				node("nod_gate", types.BenchNodeKindGate, "2026-10-23T10:01:40Z", "2026-10-23T10:02:00Z"),
				node("nod_requirement", types.BenchNodeKindReviewRequirement, "", ""),
				node("nod_review", types.BenchNodeKindReview, "2026-10-23T10:02:10Z", "2026-10-23T10:03:00Z", types.BenchAttempt{ID: "att_review", Lease: "lea_review"})},
			Leases: []types.BenchLease{
				{ID: "lea_build", Executor: stubBuilder, Role: "builder", Price: priceOf(reg, stubBuilder), RegistryEntry: entryOf(reg, stubBuilder), LiabilityUsdMicros: &owed,
					Calls: []types.BenchCall{{ID: "cal_one", Attempt: "att_build", Measurements: []types.UsageMeasurement{delta("cal_one", "att_build")}},
						{ID: "cal_two", Attempt: "att_build", Measurements: []types.UsageMeasurement{delta("cal_two", "att_build")}}}},
				{ID: "lea_review", Executor: stubReviewer, Role: "reviewer", Price: priceOf(reg, stubReviewer), RegistryEntry: entryOf(reg, stubReviewer), LiabilityUsdMicros: &zero, Calls: []types.BenchCall{}}},
			Answers:    []types.NodeID{"nod_review"},
			RailRaises: []types.BenchRailRaise{{Rail: "rounds_per_task", Nodes: []types.NodeID{"nod_build"}}, {Rail: "attempts_per_round", Nodes: []types.NodeID{}}}}}
}

// P25: a closed candidate's inputs as bench.Record takes them: closed at
// its closing review's selection with its outcome; the four stages (the
// plan and the review's requirement no stage); each attempt on its lease's
// executor; the build lease's liability on its first call not settled
// only; the answer at the review, the raises at the build (one released
// it, one released none: the task's first stage); the two routes with
// their registry families and prices. Record takes them.
func TestBenchInputs(t *testing.T) {
	reg := stubReg(t)
	m, task, err := benchInputs(candidateRead(t), reg, true)
	if err != nil {
		t.Fatal(err)
	}
	if task.Closed.Format(time.RFC3339) != "2026-10-23T10:03:00Z" || task.Outcome != types.OutcomeEmpty || task.Reason != vocab.ReasonCompletedClean || task.Hold != "" ||
		task.Approved.Format(time.RFC3339) != "2026-10-23T10:00:00Z" {
		t.Errorf("the task's ends: %+v", task)
	}
	var stages []string
	for _, n := range task.Nodes {
		stages = append(stages, string(n.Stage))
	}
	if strings.Join(stages, ",") != "build,accept,gate,review" {
		t.Errorf("the stages: %v", stages)
	}
	if a := task.Nodes[0].Attempts; len(a) != 1 || a[0].Executor != stubBuilder || task.Nodes[3].Attempts[0].Executor != stubReviewer {
		t.Errorf("the attempts: %+v", task.Nodes)
	}
	if len(task.Calls) != 2 || task.Calls[0] != (bench.Call{ID: "cal_one", Attempt: "att_build", Liability: 4000}) || task.Calls[1].Liability != 0 || len(task.Usage) != 2 {
		t.Errorf("the calls: %+v", task.Calls)
	}
	want := []bench.Intervention{{Stage: "review", Kind: bench.Answer}, {Stage: "build", Kind: bench.RailRaise}, {Stage: "build", Kind: bench.RailRaise}}
	if len(task.Interventions) != 3 || task.Interventions[0] != want[0] || task.Interventions[1] != want[1] || task.Interventions[2] != want[2] {
		t.Errorf("the interventions: %+v", task.Interventions)
	}
	if len(m.Routes) != 2 || m.Routes[0] != (types.BenchmarkRoute{Executor: stubBuilder, Role: "builder", Family: "stub-builder"}) ||
		m.Routes[1] != (types.BenchmarkRoute{Executor: stubReviewer, Role: "reviewer", Family: "stub-reviewer"}) || m.Prices[stubBuilder] != *priceOf(reg, stubBuilder) {
		t.Errorf("the routes: %+v %+v", m.Routes, m.Prices)
	}
	m.Checkpoint, m.RecordedAt, m.Arch, m.Commit, m.SchemaSet, m.Fixtures = "a", time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC), "linux/arm64",
		"git:"+types.GitID(strings.Repeat("ab", 20)), "sha256:"+types.Digest(strings.Repeat("cd", 32)), []types.BenchmarkFixture{}
	if run, err := bench.Record(m, []bench.Task{task}); err != nil || *run.Tasks[0].WallMs != 180_000 {
		t.Errorf("bench.Record of them: %v %+v", err, run)
	}
}

// P25: each read bench record refuses (exit 1), named; a held task is
// recorded at its card's earliest hold.
func TestBenchInputsRefusals(t *testing.T) {
	reg := stubReg(t)
	for name, c := range map[string]struct {
		change func(*types.TaskRead)
		stub   bool
		why    string
	}{
		"no benchmark facts": {func(r *types.TaskRead) { r.Bench = nil }, true, "holds no benchmark facts"},
		"abandoned": {func(r *types.TaskRead) {
			as := types.TaskBenchDoneAsValueAbandoned
			r.Bench.DoneAs, r.Bench.Closure = &as, nil
		}, true, "was abandoned"},
		"neither closed nor held": {func(r *types.TaskRead) { r.Bench.DoneAs, r.Bench.Closure = nil, nil }, true, "neither closed nor held"},
		"a node past review": {func(r *types.TaskRead) {
			r.Bench.Nodes = append(r.Bench.Nodes, types.BenchNode{ID: "nod_merge", Kind: types.BenchNodeKindMerge, Attempts: []types.BenchAttempt{}})
		}, true, "no stage of --until candidate's"},
		"a plan with an attempt": {func(r *types.TaskRead) {
			r.Bench.Nodes[0].Attempts = []types.BenchAttempt{{ID: "att_build", Lease: "lea_build"}}
		}, true, "is a plan"},
		"an executor the registry lacks": {func(r *types.TaskRead) { r.Bench.Leases[1].Executor = "exe_01m5ne0000e0000000000000zz" }, true, "does not hold"},
		"a role not the registry's":      {func(r *types.TaskRead) { r.Bench.Leases[1].Role = "builder" }, true, "the registry's reviewer"},
		"another price":                  {func(r *types.TaskRead) { r.Bench.Leases[0].Price.InputUsdMicrosPerMtok++ }, true, "another price"},
		"no placed price":                {func(r *types.TaskRead) { r.Bench.Leases[0].Price = nil }, true, "another price"},
		"live routes of a stub run":      {func(*types.TaskRead) {}, false, "against --routes live"},
		"an unbounded liability":         {func(r *types.TaskRead) { r.Bench.Leases[0].LiabilityUsdMicros = nil }, true, "no figure bounds it"},
		"a liability with no call (Opus #430 M2)": {func(r *types.TaskRead) {
			owed := int64(700)
			r.Bench.Leases[1].LiabilityUsdMicros = &owed
		}, true, "liable for 700 usd_micros with no unsettled call"},
		"no registry entry recorded": {func(r *types.TaskRead) { r.Bench.Leases[1].RegistryEntry = nil }, true, "another registry entry"},
		"an answer to no stage":      {func(r *types.TaskRead) { r.Bench.Answers = []types.NodeID{"nod_plan"} }, true, "which is no stage"},
		"a raise of no stage":        {func(r *types.TaskRead) { r.Bench.RailRaises[0].Nodes = []types.NodeID{"nod_plan"} }, true, "which is no stage"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := candidateRead(t)
			c.change(&rec)
			var refused errRefused
			if _, _, err := benchInputs(rec, reg, c.stub); !errors.As(err, &refused) || !strings.Contains(err.Error(), c.why) {
				t.Errorf("want a refusal %q: %v", c.why, err)
			}
		})
	}
	other := stubReg(t) // Opus #430 M3: the same id, role and price, another family
	for i, e := range other.Executors {
		if e.ID == stubReviewer {
			other.Executors[i].Family = "another-family"
		}
	}
	var refused errRefused
	if _, _, err := benchInputs(candidateRead(t), other, true); !errors.As(err, &refused) || !strings.Contains(err.Error(), "another registry entry than executor "+string(stubReviewer)) {
		t.Errorf("a registry entry of another family: %v", err)
	}
	held := candidateRead(t)
	held.Bench.DoneAs, held.Bench.Closure = nil, nil
	card := held.Card.Variant.(*types.BoardPlacedCard)
	card.Nodes = []types.BoardNodeCard{{Holds: []types.BoardHoldBadge{{ID: "hld_b", Hold: "budget_rail", Since: "2026-10-23T10:05:00Z"}}},
		{Holds: []types.BoardHoldBadge{{ID: "hld_a", Hold: "awaiting_input", Since: "2026-10-23T10:04:00.5Z"}}}}
	if _, task, err := benchInputs(held, reg, true); err != nil || task.Hold != "awaiting_input" || !task.Closed.IsZero() {
		t.Errorf("a held task: %+v %v", task, err)
	}
}

// P25: bench record's usage errors (exit 64) and its written-once file.
func TestBenchRecordUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--out", "x", "--registry", "r"},
		{"--task", "tsk_x", "--registry", "r"},
		{"--task", "tsk_x", "--out", "x"},
		{"--task", "tsk_x", "--out", "x", "--registry", "r", "--routes", "fake"},
		{"--task", "tsk_x", "--out", "x", "--registry", "r", "--until", "merged"},
		{"--task", "tsk_x", "--out", "x", "--registry", "r", "--fixture", "../fixtures"},
		{"--task", "tsk_x", "--out", "x", "--registry", "r", "--fixture", "/etc"},
		{"--task", "tsk_x", "--out", "x", "--registry", "r", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Main(context.Background(), append([]string{"dev", "bench", "record"}, args...), &out, &errb, NoTransport{}); code != entry.ExitUsage || out.Len() != 0 {
			t.Errorf("%v: %d %s", args, code, errb.String())
		}
	}
	p := filepath.Join(t.TempDir(), "a.json")
	if err := writeOnce(p, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	var refused errRefused
	if err := writeOnce(p, []byte("two\n")); !errors.As(err, &refused) {
		t.Errorf("a second write: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one\n" {
		t.Errorf("the file: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".bench-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// Astra #432 H2, H3: a damaged read is never recorded. The card is checked
// before the closure: an accept's selected result missing (its card
// unplaced damaged_record) with the closing review intact is refused. The
// mapper keeps the read's integrity: a completed stage with no selected
// result or selection time, a claimed one with no claim time, and a
// closure that is not a completed review of the nodes with its outcome.
func TestBenchInputsDamage(t *testing.T) {
	reg := stubReg(t)
	for name, c := range map[string]struct {
		change func(*types.TaskRead)
		why    string
	}{
		"an accept's selected result missing, the closing review intact (H3)": {func(r *types.TaskRead) {
			r.Card = types.BoardCard{Variant: &types.BoardUnplacedCard{Type: "unplaced", Reason: types.BoardUnplacedCardReasonDamagedRecord}}
			r.Bench.Nodes[2].Outcome, r.Bench.Nodes[2].Reason = nil, nil
		}, "card is unplaced"},
		"a completed stage with no selected result (H3)": {func(r *types.TaskRead) { r.Bench.Nodes[2].Outcome = nil }, "completed with no selected result"},
		"a completed stage with no selection time (H2)":  {func(r *types.TaskRead) { r.Bench.Nodes[3].SelectedAt = nil }, "no selection time"},
		"a claimed stage with no claim time (H2)": {func(r *types.TaskRead) {
			r.Bench.Nodes[3].State, r.Bench.Nodes[3].Outcome, r.Bench.Nodes[3].SelectedAt, r.Bench.Nodes[3].ClaimedAt = types.BenchNodeStateClaimed, nil, nil, nil
		}, "no claim time"},
		"a closure of no node (H3)": {func(r *types.TaskRead) { r.Bench.Closure.Node = "nod_gone" }, "not a completed review"},
		"a closure of another outcome (H3)": {func(r *types.TaskRead) {
			r.Bench.Closure.Outcome = types.OutcomeFound
		}, "not a completed review"},
		"a closure of a gate (H3)": {func(r *types.TaskRead) {
			r.Bench.Closure.Node, r.Bench.Closure.SelectedAt = "nod_gate", "2026-10-23T10:02:00Z"
		}, "not a completed review"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := candidateRead(t)
			c.change(&rec)
			if _, _, err := benchInputs(rec, reg, true); err == nil || !strings.Contains(err.Error(), c.why) {
				t.Errorf("want %q: %v", c.why, err)
			}
		})
	}
}

// Astra #432 M1: registry-live.example.json's local builder has no price
// (the registry requires none of a local route): placed on that entry
// with no price, it records at an explicit zero price. A local lease placed
// at a price, and the cash reviewer placed with none, are refused.
func TestBenchInputsLocalRoute(t *testing.T) {
	reg, err := registry.Load("../../fixtures/m1/config/registry-live.example.json")
	if err != nil {
		t.Fatal(err)
	}
	const local, cash = types.ExecutorID("exe_01m4tm99haemrs8zjs56wjm467"), types.ExecutorID("exe_01m4tm99haemsbebz0rbxsgv6r")
	live := func() types.TaskRead {
		r := candidateRead(t)
		l := r.Bench.Leases
		l[0].Executor, l[0].Price, l[0].RegistryEntry = local, nil, entryOf(reg, local)
		l[1].Executor, l[1].Price, l[1].RegistryEntry = cash, priceOf(reg, cash), entryOf(reg, cash)
		return r
	}
	m, _, err := benchInputs(live(), reg, false)
	if p, ok := m.Prices[local]; err != nil || !ok || p != (types.RegistryPrice{}) || m.Prices[cash] != *priceOf(reg, cash) || len(m.Routes) != 2 {
		t.Fatalf("the local route: %+v %v", m, err)
	}
	priced := live()
	priced.Bench.Leases[0].Price = &types.RegistryPrice{}
	unpriced := live()
	unpriced.Bench.Leases[1].Price = nil
	for why, r := range map[string]types.TaskRead{"has no registry price": priced, "another price": unpriced} {
		var refused errRefused
		if _, _, err := benchInputs(r, reg, false); !errors.As(err, &refused) || !strings.Contains(err.Error(), why) {
			t.Errorf("want %q: %v", why, err)
		}
	}
}
