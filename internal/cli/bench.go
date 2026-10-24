// SPDX-License-Identifier: Apache-2.0

package cli

// adc dev bench record (M1 slice plan §2.8; P25): one task's benchmark run,
// records/benchmark-run, written through internal/bench.Record.
//
//	adc dev bench record --task TSK --out FILE --registry REGISTRY
//	  [--routes stub|live] [--until candidate] [--root DIR]
//	  [--fixture PATH]... [--json]
//
//   - It reads the task over the controller's API, as adc task show does
//     (the one task's read, GET /v1/tasks/{task}, under the caller's
//     token; never the whole list: Opus #430 H1): its card and its
//     benchmark facts (TaskRead.bench, read under the attempt and
//     model_call read kinds), never the store. The registry file is the
//     one adcd ran with: each route's role, family and price. Every
//     lease's placed price must equal its entry's, and the registry entry
//     the lease was placed on (its digest, dispatch.EntryDigestOf) must be
//     the file's entry (dispatch.EntryDigest), so a file that keeps an
//     id, role and price but changes its family or destination is refused
//     (Opus #430 M3).
//   - --until candidate is checkpoint (a): stages up to review only, so a
//     node past review (rebase, merge, …) is refused. A stage's round is
//     its candidate's (graph.CandidateRound, the read's round; Opus #408
//     L1). The plan and the review's requirement are no stage: refused
//     only when one has an attempt.
//   - --routes stub: every route the task ran on is the stub provider's
//     (destination stub); live: none is.
//   - The task is closed as a candidate (its closing review's
//     result.selected, outcome and reason) or held (the earliest hold of
//     its card); an abandoned task, or one neither closed nor held, is
//     refused.
//   - Record's inputs, as it trusts them (internal/bench): every call the
//     ledger binds to each lease, each with its binding's attempt and
//     every measurement the ledger holds of it; a lease's usd_micros
//     liability once, on the first of its calls not settled
//     (ledger.StandingOf), zero on the others. A positive liability with
//     no unsettled call to stand on (no call yet, or the lease not settled
//     though its calls are) is refused, never dropped (Opus #430 M2):
//     record the task once its leases settle. Each answer counts at its
//     asking node's stage, each rail raise at the stage of the first node
//     its command released, or build when it released none.
//   - Interventions at (a) (Opus #430 L1, L2): answers and rail raises.
//     No approval is passed: the task's own approval.recorded (task.create)
//     starts its wall time and is no intervention, and no other approval
//     reaches checkpoint (a) (the merge approval is (b)'s, P28). An answer
//     counts only at its owed input's current revision (a new revision
//     drops the answer to the one before), and no production command
//     records an owed input yet (authority.RecordOwed has no caller
//     outside tests), so answers are 0 in (a) today.
//   - Routes are the executors of the task's attempts, with their
//     registry role and family; fixtures are each --fixture path under
//     --root, digested by sandbox.ClosureDigest. The build commit and
//     schema set are adc's own (buildinfo): a build with no commit is
//     refused.
//   - --out is written once: refused when it exists; a temporary file in
//     its directory is linked to it, so a file another writer made first
//     is never replaced. stdout is bench.Summary, or with --json the record.
//   - Exit: 0 written; 1 refused; 2 could not run (a read, a fixture, the
//     registry or the write); 64 usage.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/bench"
	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// stubDestination is the stub provider's destination id (the stub
// registries', fixtures/m1/config/registry-stub.json).
const stubDestination = "stub"

// candidateStages are the stages checkpoint (a) records, by node kind.
var candidateStages = map[types.BenchNodeKind]types.BenchmarkStageStage{
	types.BenchNodeKindBuild: "build", types.BenchNodeKindAccept: "accept", types.BenchNodeKindGate: "gate", types.BenchNodeKindReview: "review"}

// noStage are the node kinds that are no stage of a candidate's.
var noStage = []types.BenchNodeKind{types.BenchNodeKindPlan, types.BenchNodeKindReviewRequirement}

// gitCommit is a released build's commit (buildinfo.Commit, -ldflags).
var gitCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// errRefused marks a refusal (exit 1), apart from what could not run.
type errRefused struct{ error }

func refusedf(format string, a ...any) error { return errRefused{fmt.Errorf(format, a...)} }

type fixtureFlags []string

func (f *fixtureFlags) String() string     { return strings.Join(*f, ",") }
func (f *fixtureFlags) Set(v string) error { *f = append(*f, v); return nil }

func benchRecord(ctx context.Context, args []string, stdout, stderr io.Writer, c Client) int {
	const name = "adc dev bench record"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	task := fs.String("task", "", "the task (tsk_...) (required)")
	out := fs.String("out", "", "the records/benchmark-run file to write; refused when it exists (required)")
	reg := fs.String("registry", "", "the executor registry adcd ran with (required)")
	routes := fs.String("routes", "live", "stub: every route is the stub provider's; live: none is")
	until := fs.String("until", "candidate", "candidate: checkpoint (a), stages up to review")
	root := fs.String("root", ".", "the directory the fixtures are relative to")
	var fixtures fixtureFlags
	fs.Var(&fixtures, "fixture", "a fixture file or directory under --root, recorded with its digest (repeatable)")
	asJSON := fs.Bool("json", false, "print the record as one JSON document, not its summary")
	if code, ok := parse(fs, args, stderr); !ok {
		return code
	}
	switch {
	case fs.NArg() > 0 || *out == "" || *reg == "" || !strings.HasPrefix(*task, "tsk_"):
		say(stderr, "%s: usage: %s --task TSK --out FILE --registry REGISTRY [--routes stub|live] [--until candidate] [--root DIR] [--fixture PATH]... [--json]\n", name, name)
		return entry.ExitUsage
	case *routes != "stub" && *routes != "live":
		say(stderr, "%s: --routes %s: stub or live\n", name, *routes)
		return entry.ExitUsage
	case *until != "candidate":
		say(stderr, "%s: --until %s: only candidate, checkpoint (a), is recorded in this build\n", name, *until)
		return entry.ExitUsage
	}
	for _, f := range fixtures {
		if !filepath.IsLocal(f) || filepath.Clean(f) != f || filepath.ToSlash(f) != f {
			say(stderr, "%s: --fixture %s: a clean relative path under --root\n", name, f)
			return entry.ExitUsage
		}
	}
	run, err := benchRun(ctx, types.TaskID(*task), *out, *reg, *routes == "stub", *root, fixtures, stderr, c)
	if code, ok := benchFailed(stderr, name, err); !ok {
		return code
	}
	raw, err := json.MarshalIndent(run, "", "  ")
	if err == nil {
		err = writeOnce(*out, append(raw, '\n'))
	}
	if code, ok := benchFailed(stderr, name, err); !ok {
		return code
	}
	if *asJSON {
		err = sayJSON(stdout, raw)
	} else {
		_, err = io.WriteString(stdout, bench.Summary(run))
	}
	if err != nil {
		say(stderr, "%s: could_not_run: writing the result: %s\n", name, err)
		return entry.ExitCouldNotRun
	}
	return entry.ExitOK
}

// benchFailed reports err with its exit code; ok when there is none.
func benchFailed(stderr io.Writer, name string, err error) (int, bool) {
	var refused errRefused
	var code exitError
	switch {
	case err == nil:
		return entry.ExitOK, true
	case errors.As(err, &code):
		return int(code), false
	case errors.As(err, &refused):
		say(stderr, "%s: refused: %s\n", name, err)
		return entry.ExitRefused, false
	}
	say(stderr, "%s: could_not_run: %s\n", name, err)
	return entry.ExitCouldNotRun, false
}

// exitError is a read's exit code, its message already said.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// benchRun reads task over c and records its run.
func benchRun(ctx context.Context, task types.TaskID, out, regPath string, stub bool, root string, fixtures []string, stderr io.Writer, c Client) (types.BenchmarkRun, error) {
	if _, err := os.Lstat(out); err == nil {
		return types.BenchmarkRun{}, refusedf("%s exists: a benchmark file is written once", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return types.BenchmarkRun{}, err
	}
	info := buildinfo.Get("adc")
	if !gitCommit.MatchString(info.Commit) {
		return types.BenchmarkRun{}, refusedf("this adc is an unreleased build (commit %s): a benchmark names its build's commit", info.Commit)
	}
	reg, err := registry.Load(regPath)
	if err != nil {
		return types.BenchmarkRun{}, fmt.Errorf("--registry %s: %w", regPath, err)
	}
	fs := []types.BenchmarkFixture{}
	for _, f := range fixtures {
		d, err := sandbox.ClosureDigest(root, []string{f})
		if err != nil {
			return types.BenchmarkRun{}, fmt.Errorf("--fixture %s: %w", f, err)
		}
		fs = append(fs, types.BenchmarkFixture{Path: f, Digest: types.Digest(d)})
	}
	raw, code, ok := taskRecord(ctx, "adc dev bench record", task, stderr, c)
	if !ok {
		return types.BenchmarkRun{}, exitError(code)
	}
	rec, err := contract.Decode[types.TaskRead]("api/task", raw)
	if err != nil {
		return types.BenchmarkRun{}, fmt.Errorf("the task read: %w", err)
	}
	m, t, err := benchInputs(rec, reg, stub)
	if err != nil {
		return types.BenchmarkRun{}, err
	}
	m.Checkpoint, m.RecordedAt, m.Arch = "a", time.Now(), runtime.GOOS+"/"+runtime.GOARCH
	m.Commit, m.SchemaSet, m.Fixtures = types.GitID("git:"+info.Commit), types.Digest(info.SchemaSet), fs
	run, err := bench.Record(m, []bench.Task{t})
	if err != nil {
		return run, errRefused{err}
	}
	return run, nil
}

// benchInputs are rec's routes and prices, and its task as bench.Record
// takes it, at checkpoint (a), against the registry reg.
func benchInputs(rec types.TaskRead, reg types.ExecutorRegistry, stub bool) (bench.Meta, bench.Task, error) {
	var m bench.Meta
	b := rec.Bench
	if b == nil {
		return m, bench.Task{}, refusedf("the task read holds no benchmark facts: read attempts and model calls in project %s", rec.Project)
	}
	t := bench.Task{ID: rec.ID}
	if b.ApprovedAt != nil {
		at, err := time.Parse(time.RFC3339Nano, string(*b.ApprovedAt))
		if err != nil {
			return m, t, err
		}
		t.Approved = at
	}
	if err := ending(rec, &t); err != nil {
		return m, t, err
	}
	stage := map[types.NodeID]types.BenchmarkStageStage{}
	executor := map[types.LeaseID]types.ExecutorID{}
	for _, l := range b.Leases {
		executor[l.ID] = l.Executor
	}
	for _, n := range b.Nodes {
		s, ok := candidateStages[n.Kind]
		switch {
		case !ok && slices.Contains(noStage, n.Kind) && len(n.Attempts) == 0:
			continue
		case !ok:
			return m, t, refusedf("node %s is a %s: no stage of --until candidate's (build, accept, gate, review)", n.ID, n.Kind)
		}
		stage[n.ID] = s
		node := bench.Node{Stage: s, Round: int(n.Round)}
		for _, f := range []struct {
			at  *types.Time
			dst *time.Time
		}{{n.ClaimedAt, &node.Claimed}, {n.SelectedAt, &node.Done}} {
			if f.at == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, string(*f.at))
			if err != nil {
				return m, t, err
			}
			*f.dst = at
		}
		for _, a := range n.Attempts {
			e, ok := executor[a.Lease]
			if !ok {
				return m, t, fmt.Errorf("the task read lists no lease %s of attempt %s", a.Lease, a.ID)
			}
			node.Attempts = append(node.Attempts, bench.Attempt{ID: a.ID, Executor: e})
		}
		t.Nodes = append(t.Nodes, node)
	}
	if err := ledgerOf(b, &t); err != nil {
		return m, t, err
	}
	for _, n := range b.Answers {
		s, ok := stage[n]
		if !ok {
			return m, t, refusedf("an answer to node %s, which is no stage", n)
		}
		t.Interventions = append(t.Interventions, bench.Intervention{Stage: s, Kind: bench.Answer})
	}
	for _, r := range b.RailRaises {
		s := types.BenchmarkStageStage("build") // a raise that released no node: the task's first stage
		if len(r.Nodes) > 0 {
			var ok bool
			if s, ok = stage[r.Nodes[0]]; !ok {
				return m, t, refusedf("a raise of %s released node %s, which is no stage", r.Rail, r.Nodes[0])
			}
		}
		t.Interventions = append(t.Interventions, bench.Intervention{Stage: s, Kind: bench.RailRaise})
	}
	var err error
	m.Routes, m.Prices, err = routesOf(b, reg, stub)
	return m, t, err
}

// ending sets t's closure, from rec's closing review, or its hold, from
// the earliest hold on rec's card.
func ending(rec types.TaskRead, t *bench.Task) error {
	b := rec.Bench
	switch {
	case b.DoneAs != nil && *b.DoneAs == types.TaskBenchDoneAsValueAbandoned:
		return refusedf("task %s was abandoned: it ended with no candidate", rec.ID)
	case b.DoneAs != nil && b.Closure == nil:
		return fmt.Errorf("the task read gives task %s, done as a candidate, no closing review", rec.ID)
	case b.Closure != nil:
		at, err := time.Parse(time.RFC3339Nano, string(b.Closure.SelectedAt))
		t.Closed, t.Outcome, t.Reason = at, b.Closure.Outcome, b.Closure.Reason
		return err
	}
	placed, ok := rec.Card.Variant.(*types.BoardPlacedCard)
	if !ok {
		return refusedf("task %s's card is unplaced: its records determine no column", rec.ID)
	}
	var first *types.BoardHoldBadge
	var since time.Time
	for _, n := range placed.Nodes {
		for i, h := range n.Holds {
			at, err := time.Parse(time.RFC3339Nano, string(h.Since))
			if err != nil {
				return err
			}
			if first == nil || at.Before(since) || at.Equal(since) && h.ID < first.ID {
				first, since = &n.Holds[i], at
			}
		}
	}
	if first == nil {
		return refusedf("task %s is neither closed nor held: record it once it is", rec.ID)
	}
	t.Hold = string(first.Hold)
	return nil
}

// ledgerOf sets t's usage and calls from b's leases: every bound call with
// its attempt, and each lease's liability once, on its first call not
// settled.
func ledgerOf(b *types.TaskBench, t *bench.Task) error {
	for _, l := range b.Leases {
		if l.LiabilityUsdMicros == nil {
			return refusedf("lease %s's liability is inconsistent or saturated: no figure bounds it", l.ID)
		}
		owed := *l.LiabilityUsdMicros
		for _, c := range l.Calls {
			call := bench.Call{ID: c.ID, Attempt: c.Attempt}
			if !ledger.StandingOf(c.Measurements, c.ID).Settled {
				call.Liability, owed = owed, 0
			}
			t.Calls, t.Usage = append(t.Calls, call), append(t.Usage, c.Measurements...)
		}
		if owed > 0 { // never dropped (Opus #430 M2)
			return refusedf("lease %s is liable for %d usd_micros with no unsettled call to stand on: record the task once its leases settle", l.ID, owed)
		}
	}
	return nil
}

// routesOf are the routes of b's leases, with their registry role, family
// and price: every lease's executor is a registry executor of its role,
// its placed price the registry's, and its destination the stub's exactly
// when stub.
func routesOf(b *types.TaskBench, reg types.ExecutorRegistry, stub bool) ([]types.BenchmarkRoute, map[types.ExecutorID]types.RegistryPrice, error) {
	byID := map[types.ExecutorID]types.RegistryExecutor{}
	for _, e := range reg.Executors {
		byID[e.ID] = e
	}
	routes, prices := []types.BenchmarkRoute{}, map[types.ExecutorID]types.RegistryPrice{}
	for _, l := range b.Leases {
		e, ok := byID[l.Executor]
		switch {
		case !ok:
			return nil, nil, refusedf("lease %s ran on executor %s, which the registry does not hold", l.ID, l.Executor)
		case string(l.Role) != string(e.Role):
			return nil, nil, refusedf("lease %s is a %s's, its executor %s the registry's %s", l.ID, l.Role, e.ID, e.Role)
		case e.Price == nil:
			return nil, nil, refusedf("executor %s has no registry price", e.ID)
		case l.Price == nil || *l.Price != *e.Price:
			return nil, nil, refusedf("lease %s was placed at another price than executor %s's in the registry", l.ID, e.ID)
		case l.RegistryEntry == nil || string(*l.RegistryEntry) != dispatch.EntryDigest(e):
			return nil, nil, refusedf("lease %s was placed on another registry entry than executor %s's in --registry (its family, destination or another field differs)", l.ID, e.ID)
		case (e.Destination.ID == stubDestination) != stub:
			return nil, nil, refusedf("executor %s's destination is %s, against --routes %s", e.ID, e.Destination.ID, map[bool]string{true: "stub", false: "live"}[stub])
		}
		if _, ok := prices[e.ID]; !ok {
			routes = append(routes, types.BenchmarkRoute{Executor: e.ID, Role: types.BenchmarkRouteRole(e.Role), Family: types.Name(e.Family)})
			prices[e.ID] = *e.Price
		}
	}
	return routes, prices, nil
}

// writeOnce writes data to path, never over a file: a temporary file in
// its directory, linked to path (refused when path exists), then removed.
func writeOnce(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bench-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Chmod(0o644), tmp.Sync(), tmp.Close())
	if err == nil {
		if err = os.Link(tmp.Name(), path); errors.Is(err, os.ErrExist) {
			return refusedf("%s exists: a benchmark file is written once", path)
		}
	}
	return err
}
