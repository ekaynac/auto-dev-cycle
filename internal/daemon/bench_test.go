// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
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
		rec := taskOneAs(t, w, w.op, running)
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
	b := taskOneAs(t, w, w.op, taskOf(w, node)).Bench
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

// measuredTask records, through w, a new task whose plan ran one attempt
// under a lease of its own with one model call reporting n deltas; the
// plan completed by the attempt and its lease settled when done (the task
// then endable for the next one's admission). It returns the task.
func measuredTask(t *testing.T, w *seeder, n int, done bool) types.TaskID {
	t.Helper()
	plan := plannedIn(t, w, graphProject)
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan, map[string]graph.Resolved{}, 1)
	})
	ls := w.lease(4)
	ls.Unit = graph.UnitOf(plan)
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestLease(tx, ls) })
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.ChangeLease(tx, ls.ID, 1, lease.LeaseEvent{Kind: lease.Granted}, w.holds)
	})
	attempt := types.AttemptID(newID(t, "att"))
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartAttempt(tx, ls.ID, 2, attempt, types.AttemptBase{Tree: "git:4b825dc642cb6eb9a060e54bf8d69288fbee4904", ContractRevision: "1"})
	})
	call := types.CallID(newID(t, "cal"))
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return modelcall.Intend(tx, gateway.Binding{Lease: ls.ID, Attempt: attempt, Call: call}, "stub")
	})
	w.do(w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		var evs []eventstore.Event
		for i := range n {
			ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: call, Attempt: attempt, Lease: ls.ID,
				Kind: types.UsageMeasurementKindDelta, Sequence: int64(i + 1), Tokens: types.UsageMeasurementTokens{In: 3, Out: 2}, Turns: 1, Milliseconds: 5,
				Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
				SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		}
		return evs, nil
	})
	if done {
		commitPlan(t, w, plan, &attempt)
		w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			_, rev, _, err := lease.AttemptAt(tx, attempt)
			if err != nil {
				return nil, err
			}
			return lease.SealMinimal(tx, attempt, rev, graph.Units{Tasks: authority.Tasks{}}, w.blobs, nil, w.sys)
		})
		ended(w, w.sys, attempt)
	}
	return taskOf(w, plan)
}

// Opus #430 H1: the benchmark facts are the one task's read alone. Beside
// four tasks whose facts are each about a third of a mebibyte (together
// past the client's 1 MiB response bound, which the list carrying them
// would pass), the operator's adc task show of a small task, through
// adcd's socket, prints its record with its facts, far under the bound;
// the tasks list carries no task's facts, and reads.
func TestTheOneTaskReadIsBounded(t *testing.T) {
	var big []types.TaskID
	dir := shortDir(t)
	gs := []types.ResourceGroup{tokens(1 << 40)}
	w := seed(t, dir, daemonProjection(t, gs...))
	fullHistory(t, w)
	for range 4 {
		big = append(big, measuredTask(t, w, 900, true))
	}
	mine := measuredTask(t, w, 1, false)
	op := w.op
	w.done()
	r := startOn(t, dir, gs, op)
	c, tok := r.c, r.tok
	sum := 0
	for _, b := range big {
		code, out, errb := c.adc(tok, "task", "show", string(b), "--json")
		var rec types.TaskRead
		if code != entry.ExitOK || json.Unmarshal([]byte(out), &rec) != nil || rec.Bench == nil || len(rec.Bench.Leases) != 1 || len(rec.Bench.Leases[0].Calls[0].Measurements) != 900 {
			t.Fatalf("a large task's show: %d %.300s %s", code, out, errb)
		}
		sum += len(out)
	}
	if sum <= 1<<20 {
		t.Fatalf("the large tasks' facts are %d bytes in all, under the bound this row is about", sum)
	}
	code, out, errb := c.adc(tok, "task", "show", string(mine), "--json")
	var rec types.TaskRead
	if code != entry.ExitOK || json.Unmarshal([]byte(out), &rec) != nil || rec.ID != mine || rec.Bench == nil || len(out) > 16<<10 {
		t.Fatalf("the small task's show beside them: %d, %d bytes, %s", code, len(out), errb)
	}
	env := map[string]string{cli.SocketEnv: filepath.Join(c.dir, privateSocket), client.TokenEnv: tok}
	raw, err := cli.ClientFromEnv(func(k string) string { return env[k] }).(cli.Reader).Read(context.Background(), "tasks")
	var list struct{ Items []types.TaskRead }
	if err != nil || json.Unmarshal(raw, &list) != nil || len(list.Items) < 5 {
		t.Fatalf("the tasks list: %v, %d items", err, len(list.Items))
	}
	for _, r := range list.Items {
		if r.Bench != nil {
			t.Errorf("the list carries %s's benchmark facts", r.ID)
		}
	}
}

// Opus #430 M1: one task's unreadable benchmark facts (a node's claim
// time that is no time) fail that task's one-task read alone: the list
// reads both tasks, and the other task's one-task read gives its facts.
func TestATasksDamagedFactsAreItsOwn(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	running, done, _, _ := startedTask(t, w, w.lease(4).Reservations, true)
	damaged := templateNodes(w, done)[types.NodeKindPlan]
	op := w.op
	w.done()
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+graph.TimesProjection+`', '`+string(damaged)+`', '{"claimed":"not a time","selected":""}')`)
	w = seed(t, dir, daemonProjection(t, tokens(10)))
	t.Cleanup(w.done)
	w.op = op
	if a, b := taskReadAs(t, w, op, running), taskReadAs(t, w, op, done); a.ID != running || b.ID != done {
		t.Fatalf("the list: %+v %+v", a, b)
	}
	if rec := taskOneAs(t, w, op, running); rec.Bench == nil {
		t.Errorf("the undamaged task's facts: %+v", rec)
	}
	w.view(func(tx *eventstore.Tx) error {
		var a types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(op))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		scope, serr := authority.ReadScopeOf(tx, a, time.Now())
		if err = errors.Join(err, serr); err != nil {
			return err
		}
		if _, _, err := api.ReadTask(tx, api.ReadAt{Now: time.Now(), Scope: scope}, done, true); err == nil {
			t.Error("the damaged task's one-task read")
		}
		return nil
	})
}

// Opus #432 L1: adc task wait polls the one task's read without its
// benchmark facts (without=bench). A held task whose facts alone pass the
// client's 1 MiB bound (3,000 measurements): adc task show fails, could
// not run, past the bound; adc task wait --until held reads it and prints
// its record, bench null. Any other query on the one task's read is 400.
func TestATaskWaitReadsNoLedger(t *testing.T) {
	dir := shortDir(t)
	gs := []types.ResourceGroup{tokens(1 << 40)}
	w := seed(t, dir, daemonProjection(t, gs...))
	fullHistory(t, w)
	huge := measuredTask(t, w, 3000, false)
	build := templateNodes(w, huge)[types.NodeKindBuild]
	w.do(w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		h, err := graph.RailHold(graph.RailRounds)
		if err != nil {
			return nil, err
		}
		return graph.Raise(tx, build, h, nil)
	})
	op := w.op
	w.done()
	r := startOn(t, dir, gs, op)
	if code, _, errb := r.c.adc(r.tok, "task", "show", string(huge), "--json"); code != entry.ExitCouldNotRun || !strings.Contains(errb, "exceeds 1048576 bytes") {
		t.Fatalf("task show of a task past the bound: %d %s", code, errb)
	}
	code, out, errb := r.c.adcWithin(10*time.Second, r.tok, "task", "wait", string(huge), "--until", "held", "--timeout", "5s", "--every", "100ms", "--json")
	var rec types.TaskRead
	if code != entry.ExitOK || json.Unmarshal([]byte(out), &rec) != nil || rec.ID != huge || rec.Bench != nil || len(out) > 64<<10 {
		t.Fatalf("task wait of the same task: %d, %d bytes, %s", code, len(out), errb)
	}
	req, err := http.NewRequest(http.MethodGet, "http://adcd/v1/tasks/"+string(huge)+"?without=card", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.tok)
	resp, err := unixClient(filepath.Join(r.c.dir, privateSocket)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another query on the one task's read: %d", resp.StatusCode)
	}
}

// Opus #432 L2: the one task's read checks the task's board scope before
// it reads the task's nodes. With the task's node index damaged (a node it
// names is no node), a supervisor reading another project's board gets no
// record and no error, as for any task it does not read; the operator,
// who reads the task, gets the damage as the read's failure.
func TestTheOneTaskReadScopesFirst(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	op := fullHistory(t, w)
	running, _, _, _ := startedTask(t, w, w.lease(4).Reservations, true)
	now := time.Now().UTC()
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op}
	g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{other}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"board", "attempt", "model_call"}},
		ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		a, err := identity.CreateActor(sup)
		if err != nil {
			return nil, err
		}
		gr, err := authority.CreateGrant(tx, g)
		return append(a, gr...), err
	})
	w.done()
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+graph.TaskNodeProjection+`', '`+string(running)+`/`+newID(t, "nod")+`', 'true')`)
	w = seed(t, dir, daemonProjection(t, tokens(10)))
	t.Cleanup(w.done)
	read := func(actor types.ActorID) (bool, error) {
		var ok bool
		var rerr error
		w.view(func(tx *eventstore.Tx) error {
			var a types.Actor
			raw, _, err := tx.Get(identity.ActorProjection, string(actor))
			if err == nil {
				err = json.Unmarshal(raw, &a)
			}
			scope, serr := authority.ReadScopeOf(tx, a, time.Now())
			if err = errors.Join(err, serr); err != nil {
				return err
			}
			_, ok, rerr = api.ReadTask(tx, api.ReadAt{Now: time.Now(), Scope: scope}, running, true)
			return nil
		})
		return ok, rerr
	}
	if ok, err := read(sup.ID); ok || err != nil {
		t.Errorf("a reader of another project: %v %v", ok, err)
	}
	if _, err := read(op); err == nil {
		t.Error("the operator's read of the damaged task")
	}
}

// opRead is api.ReadTask of task for actor, with its benchmark facts.
func opRead(t *testing.T, w *seeder, actor types.ActorID, task types.TaskID) (rec types.TaskRead, ok bool, rerr error) {
	t.Helper()
	w.view(func(tx *eventstore.Tx) error {
		var a types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(actor))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		scope, serr := authority.ReadScopeOf(tx, a, time.Now())
		if err = errors.Join(err, serr); err != nil {
			return err
		}
		rec, ok, rerr = api.ReadTask(tx, api.ReadAt{Now: time.Now(), Scope: scope}, task, true)
		return nil
	})
	return rec, ok, rerr
}

// Astra #432 H1: task A's node index names task B's node, of another
// project, which has an attempt under a lease. A supervisor reading A's
// project alone, with the board, attempt and model_call read kinds, reads
// A's card unplaced damaged_record, with no first attempt and no benchmark
// facts: nothing of B's node, attempt or lease is in the read.
func TestATaskIndexNamingAnotherProjectsNode(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	op := fullHistory(t, w)
	a := taskOf(w, plannedIn(t, w, graphProject))
	nodeB := plannedIn(t, w, other)
	attemptsOn(t, w, nodeB, 1)
	var attemptB types.AttemptID
	var leaseB types.LeaseID
	w.view(func(tx *eventstore.Tx) error {
		var err, aerr error
		attemptB, _, err = lease.CurrentAttempt(tx, graph.UnitOf(nodeB))
		at, _, _, aerr := lease.AttemptAt(tx, attemptB)
		leaseB = at.Lease
		return errors.Join(err, aerr)
	})
	now := time.Now().UTC()
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op}
	g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{graphProject}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"board", "attempt", "model_call"}},
		ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op}
	w.do(op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		ac, err := identity.CreateActor(sup)
		if err != nil {
			return nil, err
		}
		gr, err := authority.CreateGrant(tx, g)
		return append(ac, gr...), err
	})
	w.done()
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+graph.TaskNodeProjection+`', '`+string(a)+`/`+string(nodeB)+`', 'true')`)
	w = seed(t, dir, daemonProjection(t, tokens(10)))
	t.Cleanup(w.done)
	rec, ok, err := opRead(t, w, sup.ID, a)
	raw, _ := json.Marshal(rec)
	card, unplaced := rec.Card.Variant.(*types.BoardUnplacedCard)
	if err != nil || !ok || !unplaced || card.Reason != types.BoardUnplacedCardReasonDamagedRecord || rec.FirstAttempt != nil || rec.Bench != nil ||
		strings.Contains(string(raw), string(nodeB)) || strings.Contains(string(raw), string(attemptB)) || strings.Contains(string(raw), string(leaseB)) {
		t.Errorf("A's read through B's node: %v %v %s", ok, err, raw)
	}
	if l := taskReadAs(t, w, sup.ID, a); l.FirstAttempt != nil || l.Bench != nil {
		t.Errorf("A in the list: %+v", l)
	}
}

// Astra #432 H2: a claimed node's times row gone (damage after start): the
// claim is evidence of a time the row lacks, so the task's one-task read
// with its benchmark facts fails (graph.ErrTimesMissing), never reading
// the node as unclaimed.
func TestAClaimWithNoTimeIsDamage(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	op := fullHistory(t, w)
	running, _, plan, _ := startedTask(t, w, w.lease(4).Reservations, true)
	if rec, ok, err := opRead(t, w, op, running); err != nil || !ok || rec.Bench == nil {
		t.Fatalf("the intact read: %v %v", ok, err)
	}
	w.done()
	execSQL(t, dir, `DELETE FROM proj_kv WHERE projection = '`+graph.TimesProjection+`' AND key = '`+string(plan)+`'`)
	w = seed(t, dir, daemonProjection(t, tokens(10)))
	t.Cleanup(w.done)
	if _, _, err := opRead(t, w, op, running); !errors.Is(err, graph.ErrTimesMissing) {
		t.Errorf("the read of a claim with no time: %v", err)
	}
}

// hopWorld is a store for TestEveryHopIsChecked: startedTask's running task
// (its plan's attempt, lease, call and measurement) and done task (its plan
// with no attempt, which asked an owed input); a task whose node has two
// attempts, the second superseding the first; and, for the crossing case,
// a task of graphProject with no attempt beside a task of another project
// with one, and a supervisor reading graphProject alone.
type hopWorld struct {
	op, sup                          types.ActorID
	running, done, chained, mine     types.TaskID
	plan, donePlan, chainNode, mineP types.NodeID
	attempt, first, second, theirs   types.AttemptID
	build                            types.NodeID
	result                           types.ResultID
	lease                            types.LeaseID
	call                             types.CallID
	msr                              types.MeasurementID
	owed                             types.OwedInputID
}

// Opus #432 r2 H1′: every hop of the one task's read is checked against
// its source, from the node to the last measurement, and the answers and
// rail raises. One damaged row per hop (another task's or another
// record's, never a missing one): the damaged task's one-task read fails;
// the other task's reads; the list reads on, showing the damaged task's
// card damaged_record when the hop is the first attempt's (the list's own
// facts). Across projects, a supervisor reading graphProject alone gets an
// error for its task, never the other project's attempt or lease. A node of
// the task recorded in another project keeps the board's reason,
// node_in_another_project (Opus #432 r2 L3).
func TestEveryHopIsChecked(t *testing.T) {
	const other = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	str := func(s string) string { return `'"` + s + `"'` }
	set := func(projection, key, path, value string) string {
		return `UPDATE proj_kv SET value = json_set(CAST(value AS TEXT), '` + path + `', '` + value + `') WHERE projection = '` + projection + `' AND key = '` + key + `'`
	}
	put := func(projection, key, value string) string {
		return `INSERT INTO proj_kv(projection, key, value) VALUES ('` + projection + `', '` + key + `', ` + value + `)`
	}
	strangerUnit := string(graph.UnitOf(types.NodeID(newID(t, "nod"))))
	for _, c := range []struct {
		name    string
		world   string // started, chained or crossed
		damaged func(w hopWorld) types.TaskID
		first   bool // a first-attempt hop: the list shows the card damaged_record
		damage  func(w hopWorld) []string
		reason  types.BoardUnplacedCardReason // the read gives an unplaced card, no error
		stops   bool                          // with reason: no first attempt and no facts are read
		listErr bool                          // the list fails: the node table's own damage (a scan)
		check   func(w hopWorld, rec types.TaskRead) bool
	}{
		{"the unit's index names another task's attempt (probe A)", "started", func(w hopWorld) types.TaskID { return w.done }, false, func(w hopWorld) []string {
			return []string{put(lease.UnitProjection, string(graph.UnitOf(w.donePlan)), str(string(w.attempt)))}
		}, "", false, false, nil},
		{"the unit's first index names another task's attempt", "started", func(w hopWorld) types.TaskID { return w.done }, true, func(w hopWorld) []string {
			return []string{put(lease.FirstProjection, string(graph.UnitOf(w.donePlan)), str(string(w.attempt)))}
		}, "", false, false, nil},
		{"the superseded attempt names another successor", "chained", func(w hopWorld) types.TaskID { return w.chained }, false, func(w hopWorld) []string {
			return []string{set(lease.AttemptProjection, string(w.first), "$.record.superseded_by", newID(t, "att"))}
		}, "", false, false, nil},
		{"the current attempt is another unit's", "chained", func(w hopWorld) types.TaskID { return w.chained }, false, func(w hopWorld) []string {
			return []string{set(lease.AttemptProjection, string(w.second), "$.record.unit", strangerUnit)}
		}, "", false, false, nil},
		{"the attempt's lease is another unit's", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			return []string{set(lease.LeaseProjection, string(w.lease), "$.record.unit", strangerUnit)}
		}, "", false, false, nil},
		{"the attempt's result index names no result of it", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			return []string{put(graph.AttemptResultProjection, string(w.attempt), str(newID(t, "res")))}
		}, "", false, false, nil},
		{"the lease's call is bound to another lease", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			return []string{set(ledger.BindingProjection, string(w.call), "$.lease", newID(t, "lea"))}
		}, "", false, false, nil},
		{"the call's index names another call's measurement", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			m := newID(t, "msr")
			return []string{`INSERT INTO proj_kv(projection, key, value) SELECT projection, '` + string(w.call) + `/` + m + `', json_set(CAST(value AS TEXT), '$.id', '` + m + `', '$.call', '` + newID(t, "cal") + `') FROM proj_kv WHERE projection = '` +
				ledger.UsageProjection + `' AND key = '` + string(w.call) + `/` + string(w.msr) + `'`}
		}, "", false, false, nil},
		{"a measurement of the call names another lease", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			return []string{set(ledger.UsageProjection, string(w.call)+"/"+string(w.msr), "$.lease", newID(t, "lea"))}
		}, "", false, false, nil},
		{"the call's index holds a measurement under another id's key (r3)", "started", func(w hopWorld) types.TaskID { return w.running }, true, func(w hopWorld) []string {
			return []string{`INSERT INTO proj_kv(projection, key, value) SELECT projection, '` + string(w.call) + `/` + newID(t, "msr") + `', value FROM proj_kv WHERE projection = '` +
				ledger.UsageProjection + `' AND key = '` + string(w.call) + `/` + string(w.msr) + `'`}
		}, "", false, false, nil},
		{"a node's selected result is another task's node's (r3 H1″)", "resulted", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{set(graph.NodeProjection, string(w.plan), "$.state.result", string(w.result))}
		}, types.BoardUnplacedCardReasonDamagedRecord, false, false, func(w hopWorld, rec types.TaskRead) bool {
			for _, n := range rec.Bench.Nodes {
				if n.ID == w.plan {
					return n.Outcome == nil && n.Reason == nil
				}
			}
			return false
		}},
		{"a node's record is under another node's key (r3)", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{`UPDATE proj_kv SET value = (SELECT value FROM proj_kv WHERE projection = '` + graph.NodeProjection + `' AND key = '` + string(w.donePlan) +
				`') WHERE projection = '` + graph.NodeProjection + `' AND key = '` + string(w.plan) + `'`}
		}, types.BoardUnplacedCardReasonDamagedRecord, true, true, nil},
		{"the unit's open index names another unit's attempt and lease (r3)", "abandoned", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			from, to := string(graph.UnitOf(w.plan))+"/", string(graph.UnitOf(w.build))+"/"
			return []string{`UPDATE proj_kv SET key = '` + to + `' || substr(key, ` + fmt.Sprint(len(from)+1) + `) WHERE projection = '` + lease.OpenProjection +
				`' AND substr(key, 1, ` + fmt.Sprint(len(from)) + `) = '` + from + `'`}
		}, types.BoardUnplacedCardReasonDamagedRecord, false, false, func(w hopWorld, rec types.TaskRead) bool { return rec.Bench != nil }},
		{"the unit's open index names another unit's lease (r3)", "abandoned", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			from := string(graph.UnitOf(w.plan)) + "/"
			move := func(prefix, to string) string {
				return `UPDATE proj_kv SET key = '` + to + `' || substr(key, ` + fmt.Sprint(len(from)+1) + `) WHERE projection = '` + lease.OpenProjection +
					`' AND substr(key, 1, ` + fmt.Sprint(len(from+prefix)) + `) = '` + from + prefix + `'`
			}
			return []string{move("lea_", string(graph.UnitOf(w.build))+"/"), move("att_", strangerUnit+"/")}
		}, types.BoardUnplacedCardReasonDamagedRecord, false, false, func(w hopWorld, rec types.TaskRead) bool { return rec.Bench != nil }},
		{"the asker index names an input another node asked", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{put(authority.OwedProjection, "asker/"+string(w.plan)+"/"+string(w.owed), str(string(w.owed)))}
		}, "", false, false, nil},
		{"the raise index holds another task's raise", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{put(authority.RaiseProjection, string(w.running)+"/99999999999999999999", `'{"task":"`+string(w.done)+`","rail":"rounds_per_task","command":null}'`)}
		}, "", false, false, nil},
		{"a raise's command cleared a hold of another rail", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{put(authority.RaiseProjection, string(w.running)+"/99999999999999999999", `'{"task":"`+string(w.running)+`","rail":"rounds_per_task","command":"cmd_hop"}'`),
				put(graph.RailClearProjection, "cmd_hop/"+string(w.plan), str(graph.RailSignal("attempts_per_round")))}
		}, "", false, false, nil},
		{"a raise's command cleared a hold on another task's node", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{put(authority.RaiseProjection, string(w.running)+"/99999999999999999999", `'{"task":"`+string(w.running)+`","rail":"rounds_per_task","command":"cmd_hop"}'`),
				put(graph.RailClearProjection, "cmd_hop/"+string(w.donePlan), str(graph.RailSignal("rounds_per_task")))}
		}, "", false, false, nil},
		{"across projects: the unit's index names another project's attempt", "crossed", func(w hopWorld) types.TaskID { return w.mine }, false, func(w hopWorld) []string {
			return []string{put(lease.UnitProjection, string(graph.UnitOf(w.mineP)), str(string(w.theirs)))}
		}, "", false, false, nil},
		{"the task's node recorded in another project (L3)", "started", func(w hopWorld) types.TaskID { return w.running }, false, func(w hopWorld) []string {
			return []string{set(graph.NodeProjection, string(w.plan), "$.recorded.project", string(other))}
		}, types.BoardUnplacedCardReasonNodeInAnotherProject, true, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t)
			s := seed(t, dir, daemonProjection(t, tokens(1<<40)))
			w := hopWorld{op: fullHistory(t, s)}
			w.sup = w.op
			switch c.world {
			case "resulted":
				w.running, w.done = measuredTask(t, s, 1, true), measuredTask(t, s, 1, true)
				w.plan, w.donePlan = templateNodes(s, w.running)[types.NodeKindPlan], templateNodes(s, w.done)[types.NodeKindPlan]
				s.view(func(tx *eventstore.Tx) error {
					v, err := graph.ViewOf(tx, w.donePlan)
					if err == nil && v.State.Result == nil {
						err = errors.New("the other task's plan has no selected result")
					}
					if err == nil {
						w.result = *v.State.Result
					}
					return err
				})
			case "started", "abandoned":
				w.running, w.done, w.plan, w.attempt = startedTask(t, s, s.lease(4).Reservations, true)
				w.donePlan = templateNodes(s, w.done)[types.NodeKindPlan]
				w.owed = types.OwedInputID(newID(t, "own"))
				o := types.OwedInput{ID: w.owed, Project: graphProject, RequestedBy: w.donePlan, Owner: w.op, Kind: types.OwedInputKindRuling, Question: "q",
					Subject: types.ObjectRef{Type: "node", ID: string(w.donePlan), Version: new(int64)}, Revision: 1}
				s.do(w.op, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.RecordOwed(o) })
				s.view(func(tx *eventstore.Tx) error {
					a, _, _, err := lease.AttemptAt(tx, w.attempt)
					w.lease = a.Lease
					calls, cerr := ledger.BoundTo(tx, w.lease)
					if err = errors.Join(err, cerr); err != nil || len(calls) != 1 {
						return errors.Join(err, errors.New("the running task's one call"))
					}
					w.call = calls[0]
					ms, err := ledger.Measurements(tx, w.call)
					if err == nil && len(ms) == 1 {
						w.msr = ms[0].ID
					}
					return err
				})
				w.build = templateNodes(s, w.running)[types.NodeKindBuild]
				if c.world == "abandoned" { // the approver revoked: the task ends abandoned, its open units read (lease.Unended)
					s.do(w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return identity.RevokeActor(tx, w.op, time.Now()) })
				}
			case "chained":
				w.chainNode = plannedIn(t, s, graphProject)
				w.chained = taskOf(s, w.chainNode)
				attemptsOn(t, s, w.chainNode, 2)
				s.view(func(tx *eventstore.Tx) error {
					var err, ferr error
					w.second, _, err = lease.CurrentAttempt(tx, graph.UnitOf(w.chainNode))
					w.first, _, ferr = lease.FirstOn(tx, graph.UnitOf(w.chainNode))
					return errors.Join(err, ferr)
				})
			case "crossed":
				w.mineP = plannedIn(t, s, graphProject)
				w.mine = taskOf(s, w.mineP)
				theirs := plannedIn(t, s, other)
				attemptsOn(t, s, theirs, 1)
				s.view(func(tx *eventstore.Tx) error {
					var err error
					w.theirs, _, err = lease.CurrentAttempt(tx, graph.UnitOf(theirs))
					return err
				})
				now := time.Now().UTC()
				sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: w.op}
				g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{graphProject}},
					Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{"board", "attempt", "model_call"}},
					ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
					ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: w.op}
				s.do(w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
					a, err := identity.CreateActor(sup)
					if err != nil {
						return nil, err
					}
					gr, err := authority.CreateGrant(tx, g)
					return append(a, gr...), err
				})
				w.sup = sup.ID
			}
			damaged := c.damaged(w)
			if _, ok, err := opRead(t, s, w.sup, damaged); err != nil || !ok {
				t.Fatalf("the intact read: %v %v", ok, err)
			}
			s.done()
			execSQL(t, dir, c.damage(w)...)
			s = seed(t, dir, daemonProjection(t, tokens(1<<40)))
			t.Cleanup(s.done)
			rec, ok, err := opRead(t, s, w.sup, damaged)
			raw, _ := json.Marshal(rec)
			switch card, unplaced := rec.Card.Variant.(*types.BoardUnplacedCard); {
			case c.reason != "" && (err != nil || !ok || !unplaced || card.Reason != c.reason):
				t.Errorf("want the card unplaced %s: %v %v %s", c.reason, ok, err, raw)
			case c.reason != "" && c.stops && (rec.FirstAttempt != nil || rec.Bench != nil):
				t.Errorf("want no facts read past the card: %s", raw)
			case c.check != nil && !c.check(w, rec):
				t.Errorf("the read's facts: %s", raw)
			case c.reason == "" && err == nil:
				t.Errorf("the damaged task's one-task read: %s", raw)
			}
			if w.theirs != "" && (strings.Contains(string(raw), string(w.theirs)) || err != nil && strings.Contains(err.Error(), "lea_")) {
				t.Errorf("the other project's records reached: %s %v", raw, err)
			}
			if w.done != "" && damaged == w.running || w.running != "" && damaged == w.done {
				intact := map[types.TaskID]types.TaskID{w.running: w.done, w.done: w.running}[damaged]
				if r, ok, err := opRead(t, s, w.op, intact); err != nil || !ok || r.Bench == nil {
					t.Errorf("the other task's read: %v %v", ok, err)
				}
			}
			if c.listErr {
				var lerr error
				s.view(func(tx *eventstore.Tx) error {
					_, lerr = api.Registered()["tasks"].List(tx, api.ReadAt{Now: time.Now(), Scope: authority.ReadScope{}})
					return nil
				})
				if !errors.Is(lerr, graph.ErrIndexDamaged) {
					t.Errorf("the list over a damaged node table: %v", lerr)
				}
				return
			}
			l := taskReadAs(t, s, w.sup, damaged)
			card, unplaced := l.Card.Variant.(*types.BoardUnplacedCard)
			if l.ID != damaged || c.first && (!unplaced || card.Reason != types.BoardUnplacedCardReasonDamagedRecord || l.FirstAttempt != nil) {
				t.Errorf("the list's record of the damaged task: %+v", l)
			}
		})
	}
}
