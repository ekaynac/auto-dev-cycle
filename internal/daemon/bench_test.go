// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// stubRegistryFile is the registry the run worlds' executors are
// (stubRegistry with no override).
const stubRegistryFile = "../../fixtures/m1/config/registry-stub.json"

// released makes adc a released build for the test: a benchmark names its
// build's commit.
func released(t *testing.T) {
	t.Helper()
	was := buildinfo.Commit
	buildinfo.Commit = strings.Repeat("ab", 20)
	t.Cleanup(func() { buildinfo.Commit = was })
}

// benchRecord is adc dev bench record of task into a new file, as the
// operator, with args added: its exit code, stdout, stderr and the file's
// bytes (nil when it was not written).
func (r restarted) benchRecord(t *testing.T, task types.TaskID, args ...string) (int, string, string, []byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "a.json")
	code, stdout, errb := r.c.adc(r.tok, append([]string{"dev", "bench", "record", "--task", string(task), "--out", out, "--registry", stubRegistryFile}, args...)...)
	raw, err := os.ReadFile(out)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return code, stdout, errb, raw
}

// stageOf is run's only task's stage s; false when it has none.
func stageOf(run types.BenchmarkRun, s types.BenchmarkStageStage) (types.BenchmarkStage, bool) {
	for _, st := range run.Tasks[0].Stages {
		if st.Stage == s {
			return st, true
		}
	}
	return types.BenchmarkStage{}, false
}

// P25 (§2.3 assertion 5, §2.8): adc dev bench record, as the operator, over
// adcd's API, records the candidate task's run, valid against
// records/benchmark-run: checkpoint (a), the task closed by its review's go
// (empty/completed_clean) with its wall time, the four stages up to review
// (one attempt each for build and review, none for the controller's accept
// and gate, each in round 1; this world's runs make no model call), the
// two stub routes with their families, the
// fixture with its digest, and no merge stage. Neither the file nor its
// output holds the acceptance digest, the canary or the hidden test (22a-7's
// noLeak, its reads too). A second record over the file is refused, and so
// are --routes live and a build with no commit.
func TestBenchRecordsTheCandidate(t *testing.T) {
	released(t)
	rw := candidateRun(t, nil)
	r := restartOn(t, rw)
	code, stdout, errb, raw := r.benchRecord(t, rw.task, "--routes", "stub", "--root", "../..", "--fixture", "fixtures/m1/profiles/go-mini.json", "--json")
	if code != entry.ExitOK || raw == nil {
		t.Fatalf("bench record: %d %s", code, errb)
	}
	run, err := contract.Decode[types.BenchmarkRun]("records/benchmark-run", raw)
	if err != nil {
		t.Fatalf("the record: %v: %s", err, raw)
	}
	var printed types.BenchmarkRun
	if json.Unmarshal([]byte(stdout), &printed) != nil || printed.RecordedAt != run.RecordedAt {
		t.Errorf("--json printed %s", stdout)
	}
	if run.Checkpoint != "a" || len(run.Tasks) != 1 || run.BuildCommit != types.GitID("git:"+buildinfo.Commit) || len(run.Fixtures) != 1 ||
		run.Fixtures[0].Path != "fixtures/m1/profiles/go-mini.json" || !strings.HasPrefix(string(run.Fixtures[0].Digest), "sha256:") {
		t.Fatalf("the run: %s", raw)
	}
	task := run.Tasks[0]
	if task.Task != rw.task || task.Outcome == nil || *task.Outcome != types.OutcomeEmpty || task.Reason == nil || task.Reason.String() != "completed_clean" ||
		task.Hold != nil || task.WallMs == nil {
		t.Errorf("the task: %s", raw)
	}
	families := map[types.BenchmarkRouteRole]types.Name{}
	for _, rt := range run.Routes {
		families[rt.Role] = rt.Family
	}
	if len(run.Routes) != 2 || families["builder"] != "stub-builder" || families["reviewer"] != "stub-reviewer" {
		t.Errorf("the routes: %+v", run.Routes)
	}
	for s, attempts := range map[types.BenchmarkStageStage]int64{"build": 1, "accept": 0, "gate": 0, "review": 1} {
		st, ok := stageOf(run, s)
		if !ok || st.Attempts != attempts || st.Rounds != 1 || st.Pending != 0 || st.Unfinished != 0 {
			t.Errorf("stage %s: %+v (%v)", s, st, ok)
		}
	}
	for _, s := range []types.BenchmarkStageStage{"rebase", "review_fresh", "gate_refresh", "merge", "closure"} {
		if _, ok := stageOf(run, s); ok {
			t.Errorf("a %s stage in checkpoint (a)", s)
		}
	}
	noAcceptance(t, "the benchmark file", string(raw), hiddenTest)
	noAcceptance(t, "adc dev bench record --json", stdout+errb, hiddenTest)
	r.noLeak(t, rw.task, hiddenTest)
	// Written once; the routes must be the stub's; a build names its commit.
	dir := t.TempDir()
	again := filepath.Join(dir, "a.json")
	if err := os.WriteFile(again, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.c.adc(r.tok, "dev", "bench", "record", "--task", string(rw.task), "--out", again, "--registry", stubRegistryFile, "--routes", "stub"); code != entry.ExitRefused || !strings.Contains(errb, "written once") {
		t.Errorf("a second record over the file: %d %s", code, errb)
	}
	if kept, _ := os.ReadFile(again); string(kept) != "kept\n" {
		t.Errorf("the file was replaced: %q", kept)
	}
	if code, _, errb, raw := r.benchRecord(t, rw.task, "--routes", "live"); code != entry.ExitRefused || raw != nil || !strings.Contains(errb, "against --routes live") {
		t.Errorf("--routes live of a stub run: %d %s", code, errb)
	}
	buildinfo.Commit = "unknown"
	if code, _, errb, raw := r.benchRecord(t, rw.task, "--routes", "stub"); code != entry.ExitRefused || raw != nil || !strings.Contains(errb, "unreleased build") {
		t.Errorf("an unreleased adc: %d %s", code, errb)
	}
}

// usdGroup is a USD rail's group for a lease's usd_micros reservation.
var usdGroup = types.ResourceGroup{ID: "rg_01m5nd2rzhe3wbmb5ygfh7pf4s", Kind: types.ResourceGroupKindAccountQuota, ReleaseRule: types.ResourceGroupReleaseRuleOnSettle,
	Members: []types.ExecutorID{}, CapacityModel: types.ResourceGroupCapacityModel{Unit: types.CapacityUnitUsdMicros, Limit: ptr(int64(1_000_000)),
		QuotaState: types.ResourceGroupCapacityModelQuotaStateKnown}}

func ptr[T any](v T) *T { return &v }

// P25: the task read's benchmark facts, as the operator reads them (valid
// against api/task): the approval's time; the plan claimed (its claim's
// time, no selection yet) with its attempt and that attempt's lease; the
// lease's executor, role, placed price and every bound call, with its
// attempt and its measurement as the ledger holds it; the lease's
// usd_micros liability, its reservation while it has not settled and the
// group's liability once it has (capacity.LiabilitiesOf: 0, its call
// settled by the adapter's final and the gateway's count), its three
// measurements; and the plan's
// result.selected time once it completes. A reader without the model_call
// read kind reads none (TestTaskReadKinds' scope).
func TestTaskReadBench(t *testing.T) {
	startedPrice = &types.RegistryPrice{InputUsdMicrosPerMtok: 2_000_000, OutputUsdMicrosPerMtok: 10_000_000}
	t.Cleanup(func() { startedPrice = nil })
	w := seed(t, stateDir(t), daemonProjection(t, tokens(1000), usdGroup))
	fullHistory(t, w)
	res := []types.LeaseReservationsItem{{Group: usdGroup.ID, Amount: 700, Unit: types.CapacityUnitUsdMicros}}
	running, _, plan, attempt := startedTask(t, w, res, true)
	read := func() *types.TaskBench {
		t.Helper()
		rec := taskReadAs(t, w, w.op, running)
		raw, err := json.Marshal(rec)
		if err != nil || contract.Validate("api/task", raw) != nil || rec.Bench == nil {
			t.Fatalf("the task read: %s %v", raw, err)
		}
		return rec.Bench
	}
	b := read()
	if b.ApprovedAt == nil || b.DoneAs != nil || b.Closure != nil || len(b.Answers) != 0 || len(b.RailRaises) != 0 || len(b.Leases) != 1 {
		t.Fatalf("the bench: %+v", b)
	}
	var n types.BenchNode
	for _, x := range b.Nodes {
		if x.ID == plan {
			n = x
		}
	}
	l := b.Leases[0]
	if n.Kind != types.BenchNodeKindPlan || n.ClaimedAt == nil || n.SelectedAt != nil || len(n.Attempts) != 1 || n.Attempts[0].ID != attempt || n.Attempts[0].Lease != l.ID {
		t.Errorf("the plan: %+v", n)
	}
	if l.Role != "builder" || l.Price == nil || *l.Price != *startedPrice || l.LiabilityUsdMicros == nil || *l.LiabilityUsdMicros != 700 ||
		len(l.Calls) != 1 || l.Calls[0].Attempt != attempt || len(l.Calls[0].Measurements) != 1 || l.Calls[0].Measurements[0].Tokens.In != 300 {
		t.Errorf("the lease: %+v", l)
	}
	for i, src := range []types.UsageMeasurementSourceOfTruth{types.UsageMeasurementSourceOfTruthGatewayCount, types.UsageMeasurementSourceOfTruthAdapterReport} {
		m := l.Calls[0].Measurements[0] // the adapter's final, the gateway's count bearing it out, settles the call
		m.ID, m.Kind, m.Sequence, m.SourceOfTruth = types.MeasurementID(newID(t, "msr")), []types.UsageMeasurementKind{types.UsageMeasurementKindDelta, types.UsageMeasurementKindFinal}[i], int64(2+i), src
		w.do(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
			ev, err := ledger.Measure(m)
			return []eventstore.Event{ev}, err
		})
	}
	commitPlan(t, w, plan, &attempt)
	for _, x := range read().Nodes {
		if x.ID == plan && (x.SelectedAt == nil || x.State != types.BenchNodeStateCompleted || x.Outcome == nil || *x.Outcome != types.OutcomeEmpty) {
			t.Errorf("the completed plan: %+v", x)
		}
	}
	ended(w, w.sys, attempt)
	var owed int64
	w.view(func(tx *eventstore.Tx) error {
		out, _, _, err := capacity.LiabilitiesOf(tx, l.ID, []types.ResourceGroupID{usdGroup.ID})
		owed = out[usdGroup.ID]
		return err
	})
	if l := read().Leases[0]; l.LiabilityUsdMicros == nil || *l.LiabilityUsdMicros != owed || owed == 700 || len(l.Calls[0].Measurements) != 3 {
		t.Errorf("the settled lease's liability %v, the group's %d", l.LiabilityUsdMicros, owed)
	}
}

// P25: a node's benchmark facts list every attempt on its unit, first to
// last, each superseding the one before under a lease of its own
// (lease.AttemptsOn), with each of those leases.
func TestTaskReadBenchAttempts(t *testing.T) {
	w := seed(t, stateDir(t), daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	node := plannedIn(t, w, graphProject)
	attemptsOn(t, w, node, 3)
	var first, last types.AttemptID
	w.view(func(tx *eventstore.Tx) error {
		var err, cerr error
		first, _, err = lease.FirstOn(tx, graph.UnitOf(node))
		last, _, cerr = lease.CurrentAttempt(tx, graph.UnitOf(node))
		return errors.Join(err, cerr)
	})
	b := taskReadAs(t, w, w.op, taskOf(w, node)).Bench
	var n types.BenchNode
	for _, x := range b.Nodes {
		if x.ID == node {
			n = x
		}
	}
	leases := map[types.LeaseID]bool{}
	for _, l := range b.Leases {
		leases[l.ID] = true
	}
	if len(n.Attempts) != 3 || n.Attempts[0].ID != first || n.Attempts[2].ID != last || len(leases) != 3 ||
		!leases[n.Attempts[0].Lease] || !leases[n.Attempts[1].Lease] || !leases[n.Attempts[2].Lease] {
		t.Errorf("the node's attempts %+v, first %s, last %s; leases %v", n.Attempts, first, last, leases)
	}
}
