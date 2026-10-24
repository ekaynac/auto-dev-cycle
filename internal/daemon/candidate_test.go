// SPDX-License-Identifier: Apache-2.0

package daemon

// P22a-7: the in-process candidate fixture (slice plan §2.3, checkpoint
// (a) in stub mode). Each case runs the reconcile loop and adcd's runner
// with a scripted stub peer (inproc), then restarts adcd on the case's
// state: the start compares every projection with a replay of the log and
// refuses any difference, and the reads, through the installed commands,
// are the restarted controller's. After it stops, verify-log exits 0.
//
//   - candidate: build found, accept, the gate empty/completed_clean, the
//     review GO by an independent roster; no merge node; the board's card
//     Done (candidate); task show's first-attempt metrics; task wait
//     --until closed.
//   - admission-held: task A held (its review: no independent reviewer),
//     task B created stays Ready, never admitted; A resumes once a
//     reviewer is, runs to its end, and only then is B admitted.
//   - launch-missing-credential, launch-missing-image: the launch is
//     refused, nothing starts, the lease is released, the build held on
//     what is missing; task wait --until held.
//   - a revoked approval: its task's operator revoked before the first
//     claim, no node is claimed, no attempt starts, and the task is
//     abandoned (Done, abandoned).
//
// No read holds the acceptance digest or the canary (the hidden test's
// name, which only the gate's stream carries, passing or failing): every
// read the CLI has, the event stream included (Opus #408 L2, L3), as the
// operator.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/cli"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// acceptBody is the acceptance attachment readyTask records at
// accept_hidden_test.go, published in the world's blob store: a hidden Go
// test of the root package, TestA, which every gate world's stream reports
// (a gate expects the acceptance's tests: Opus #429 M1); acceptDigest is
// its digest: restricted, withheld from the builder and the reviewer.
const acceptBody = "package accept\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"

var acceptDigest = blobDigest([]byte(acceptBody))

// canary is the hidden acceptance test's name, which only the gate's
// stream (a restricted artifact) carries.
const canary = "TestHiddenCanaryTransfer"

// stubGroups are the fixture's resource groups, the run world's.
func stubGroups(t *testing.T) []types.ResourceGroup {
	t.Helper()
	var gs []types.ResourceGroup
	raw, err := os.ReadFile("../../fixtures/m1/config/groups.json")
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

// restarted is adcd restarted on rw's state, its operator's token, and
// the controller's log, once rw's runs have ended and its store closed.
// The state moves to a short directory first (the private socket's path).
type restarted struct {
	c   *commanded
	tok string
}

// restartOn stops rw and starts adcd on its state, its reader rw.w.op; Run refuses a start
// whose projections differ from a replay of the log (Start's check). Its
// cleanup stops adcd and then requires verify-log to exit 0.
func restartOn(t *testing.T, rw *runWorld) restarted {
	t.Helper()
	rw.r.Wait()
	rw.w.done()
	dir := shortDir(t)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rw.w.dir, dir); err != nil {
		t.Fatal(err)
	}
	gs := stubGroups(t)
	w := seed(t, dir, daemonProjection(t, gs...))
	var tok identity.Secret
	w.do(rw.w.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, s, err := identity.IssueToken(rw.w.op, rw.w.op, time.Now().UTC())
		tok = s
		return evs, err
	})
	w.done()
	c := &commanded{dir: dir, logs: &logBuffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan *Daemon, 1)
	go func() {
		done <- Run(ctx, Config{StateDir: dir, Groups: gs, reconcileEvery: time.Hour}, c.logs, func(d *Daemon) { ready <- d })
	}()
	select {
	case c.d = <-ready:
	case err := <-done:
		t.Fatalf("the restart: %v\n%s", err, c.logs.String())
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		var out, errb bytes.Buffer
		if code := cli.Main(context.Background(), []string{"dev", "verify-log", "--state", dir}, &out, &errb, nil); code != entry.ExitOK {
			t.Errorf("verify-log: %d %s %s", code, out.String(), errb.String())
		}
	})
	return restarted{c: c, tok: tok.Reveal()}
}

// show is adc task show task --json, validated against its schema.
func (r restarted) show(t *testing.T, task types.TaskID) types.TaskRead {
	t.Helper()
	code, out, errb := r.c.adc(r.tok, "task", "show", string(task), "--json")
	var rec types.TaskRead
	if code != entry.ExitOK || contract.Validate("api/task", []byte(out)) != nil || json.Unmarshal([]byte(out), &rec) != nil {
		t.Fatalf("task show: %d %s %s", code, out, errb)
	}
	return rec
}

// wait is adc task wait task --until state's exit code.
func (r restarted) wait(t *testing.T, task types.TaskID, state string) int {
	t.Helper()
	code, _, _ := r.c.adcWithin(10*time.Second, r.tok, "task", "wait", string(task), "--until", state, "--timeout", "2s", "--every", "100ms")
	return code
}

// noLeak requires that no read the CLI has (every one but the event
// stream), as the operator, holds the acceptance digest or the canary,
// in task's show and every other read.
func (r restarted) noLeak(t *testing.T, task types.TaskID, canaries ...string) {
	t.Helper()
	for resource, line := range cli.ReadCommands() {
		args := append(strings.Fields(strings.TrimPrefix(line, "adc ")), "--json")
		if resource == "tasks" {
			args = []string{"task", "show", string(task), "--json"}
		}
		var code int
		var out, errb string
		if resource == "events" { // the stream from its start, read for a bounded time: it has no end (Opus #408 L2)
			code, out, errb = r.c.adcWithin(3*time.Second, r.tok, "events", "--follow", "--after", "0", "--json")
			if !strings.Contains(out, `"seq":1,`) {
				t.Errorf("the event stream read nothing: %d %s", code, errb)
			}
		} else if code, out, errb = r.c.adc(r.tok, args...); code != entry.ExitOK {
			t.Errorf("%s: %d %s", line, code, errb)
		}
		for _, secret := range append([]string{acceptDigest, strings.TrimPrefix(acceptDigest, "sha256:"), canary}, canaries...) {
			if strings.Contains(out, secret) {
				t.Errorf("%s holds %q: %s", line, secret, out)
			}
		}
	}
}

// card is task's placed card on the restarted board.
func (r restarted) card(t *testing.T, task types.TaskID) types.BoardPlacedCard {
	t.Helper()
	placed, ok := r.show(t, task).Card.Variant.(*types.BoardPlacedCard)
	if !ok {
		t.Fatalf("the task's card is not placed: %+v", r.show(t, task).Card)
	}
	return *placed
}

// candidateRun is a run world whose task's build, accept and gate ran (the
// gate green, the canary among its passing tests) and whose review, run
// by reviewer under reg, decides. It returns once the review leaves claimed.
func candidateRun(t *testing.T, held func(*runWorld)) *runWorld {
	t.Helper()
	rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"gate-events.json": goEvents("TestA=pass", canary+"=pass")}, nil)
	if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted || v.Outcome == nil || *v.Outcome != types.OutcomeEmpty {
		t.Fatalf("the gate: %s %v, holds %+v", v.State.State, v.Outcome, v.State.Holds)
	}
	if held != nil {
		held(rw)
	}
	rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(goDecision, false, nil))
	for range 12 {
		rw.passesFor(t, 1, 0)
		if reviewNodes(rw)[types.NodeKindReview].State.State == types.NodeStateStateCompleted {
			break
		}
	}
	return rw
}

// The candidate case.
func TestCandidateFixture(t *testing.T) {
	rw := candidateRun(t, nil)
	nodes := nodesOf(rw.w, rw.task)
	for kind, want := range map[types.NodeKind]types.Outcome{types.NodeKindBuild: types.OutcomeFound, types.NodeKindAccept: types.OutcomeEmpty,
		types.NodeKindGate: types.OutcomeEmpty, types.NodeKindReview: types.OutcomeEmpty} {
		v := nodes[kind]
		if kind == types.NodeKindReview {
			v = reviewNodes(rw)[kind]
		}
		if v.State.State != types.NodeStateStateCompleted || v.Outcome == nil || *v.Outcome != want {
			t.Errorf("the %s: %s %v, holds %+v", kind, v.State.State, v.Outcome, v.State.Holds)
		}
	}
	if _, merge := nodes[types.NodeKind("merge")]; merge {
		t.Error("a merge node is recorded")
	}
	r := restartOn(t, rw)
	approvedBeforeClaim(t, r.c.dir)
	if c := r.card(t, rw.task); c.Column != types.BoardPlacedCardColumnDoneCandidate {
		t.Errorf("the card: %+v", c)
	}
	f := r.show(t, rw.task).FirstAttempt
	if f == nil || f.Node != rw.build || f.Outcome == nil || *f.Outcome != types.OutcomeFound || f.SecondsAfterApproval == nil {
		t.Errorf("the first attempt: %+v", f)
	}
	if code := r.wait(t, rw.task, "closed"); code != entry.ExitOK {
		t.Errorf("task wait --until closed: %d", code)
	}
	r.noLeak(t, rw.task)
}

// approvedBeforeClaim requires, in the log under dir, the approval of a
// task recorded before the first node.claimed (§2.3 assertion 1).
func approvedBeforeClaim(t *testing.T, dir string) {
	t.Helper()
	approved := count(t, dir, `SELECT COALESCE(MIN(seq), 0) FROM events WHERE kind = '`+authority.ApprovalRecorded+`'`)
	claimed := count(t, dir, `SELECT COALESCE(MIN(seq), 0) FROM events WHERE kind = '`+graph.NodeClaimed+`'`)
	if approved == 0 || claimed == 0 || approved > claimed {
		t.Errorf("the approval at seq %d, the first claim at %d", approved, claimed)
	}
}

// The admission-held case: A's review held with no independent reviewer
// (the build's executor unknown to the registry); B created stays Ready.
// A resumes once its builder is known again, runs to its end, and only
// then is B admitted.
func TestAdmissionHeldFixture(t *testing.T) {
	var b types.TaskID
	rw := candidateRun(t, func(rw *runWorld) {
		rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{types.ExecutorID(newID(t, "exe")): "stub-builder"}, nil))
		rw.passesFor(t, 8, 0)
		if v := reviewNodes(rw)[types.NodeKindReview]; v.State.State != types.NodeStateStateHeld {
			t.Fatalf("A's review: %s, holds %+v", v.State.State, v.State.Holds)
		}
		_, b = readyTask(t, rw.w, rw.w.sys)
		rw.passesFor(t, 4, 0)
		if n := nodesOf(rw.w, b); len(n) != 0 {
			t.Fatalf("B, created while A is held, has nodes: %v", n)
		}
	})
	if v := reviewNodes(rw)[types.NodeKindReview]; v.State.State != types.NodeStateStateCompleted {
		t.Fatalf("A's review after its reviewer is known: %s, holds %+v", v.State.State, v.State.Holds)
	}
	if n := nodesOf(rw.w, b); len(n) == 0 {
		rw.passesFor(t, 2, 0)
	}
	if n := nodesOf(rw.w, b); n[types.NodeKindPlan].Node.ID == "" {
		t.Fatalf("B after A's end is not admitted: %v", n)
	}
	r := restartOn(t, rw)
	if c := r.card(t, rw.task); c.Column != types.BoardPlacedCardColumnDoneCandidate {
		t.Errorf("A's card: %+v", c)
	}
	r.noLeak(t, rw.task)
}

// The admission-held case's first half, as the board shows it: B, Ready
// while A is held, is unadmitted, with no claim and no lease; task wait
// --until held of A exits 0.
func TestAdmissionHeldBoard(t *testing.T) {
	var b types.TaskID
	var rw *runWorld
	rw = gateCompleted(t)
	rw.reviewing(stubRegistry(t, map[types.ExecutorID]types.Name{types.ExecutorID(newID(t, "exe")): "stub-builder"}, nil))
	rw.passesFor(t, 8, 0)
	_, b = readyTask(t, rw.w, rw.w.sys)
	rw.passesFor(t, 4, 0)
	r := restartOn(t, rw)
	if code := r.wait(t, rw.task, "held"); code != entry.ExitOK {
		t.Errorf("task wait --until held of A: %d", code)
	}
	rec := r.show(t, b)
	if rec.FirstAttempt != nil {
		t.Errorf("B has a first attempt: %+v", rec.FirstAttempt)
	}
	raw, _ := json.Marshal(rec.Card)
	if !bytes.Contains(raw, []byte(`"admitted":false`)) && !bytes.Contains(raw, []byte(`"column":"ready"`)) {
		t.Errorf("B's card while A is held: %s", raw)
	}
}

// The launch-missing cases: the launch is refused, nothing starts, the
// lease is released and the build held on what is missing, naming it;
// task wait --until held exits 0.
func TestLaunchMissingFixtures(t *testing.T) {
	for name, c := range map[string]struct {
		edit         func(*runWorld)
		hold, signal string
	}{
		"launch-missing-credential": {func(rw *runWorld) { rw.r.place.d.Credentials = noCredentials{} }, vocab.HoldSourceInvalidConfig, "destination stub"},
		"launch-missing-image":      {func(rw *runWorld) { rw.r.place.d.Images = fakeImages{false} }, vocab.HoldWrongOwner, "localhost/adc/py-mini@sha256:"},
	} {
		t.Run(name, func(t *testing.T) {
			rw := newRunWorld(t, true, builder(nil, false, nil))
			c.edit(rw)
			rw.untilRun(t)
			if _, ok, err := firstAttempt(rw); err != nil || ok {
				t.Fatalf("an attempt started: %v %v", ok, err)
			}
			st := nodesOf(rw.w, rw.task)[types.NodeKindBuild].State
			if len(st.Holds) != 1 || st.Holds[0].Hold != c.hold || !strings.Contains(st.Holds[0].SignalInstance, c.signal) {
				t.Fatalf("the build: %+v", st)
			}
			if _, live := leaseOn(rw.w, rw.build); live {
				t.Error("the refused launch left its lease live")
			}
			r := restartOn(t, rw)
			if code := r.wait(t, rw.task, "held"); code != entry.ExitOK {
				t.Errorf("task wait --until held: %d", code)
			}
			if f := r.show(t, rw.task).FirstAttempt; f != nil {
				t.Errorf("a first attempt: %+v", f)
			}
			r.noLeak(t, rw.task)
		})
	}
}

// A revoked approval: the task's operator revoked before the first claim;
// no node is claimed and no attempt starts; the task is abandoned (Done,
// abandoned) and task wait --until closed exits 0.
func TestRevokedApprovalFixture(t *testing.T) {
	rw := newRunWorldWith(t, true, builder(nil, false, nil), nil)
	if ran, err := rw.l.Pass(context.Background()); err != nil || ran != 1 { // the admission, before any claim
		t.Fatalf("the admitting pass: %d, %v", ran, err)
	}
	if n := nodesOf(rw.w, rw.task); n[types.NodeKindPlan].State.State != types.NodeStateStateRunnable && n[types.NodeKindPlan].State.State != types.NodeStateStateWaiting {
		t.Fatalf("the plan after admission: %s", n[types.NodeKindPlan].State.State)
	}
	reader := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "reader", CreatedBy: rw.w.op}
	rw.w.do(rw.w.op, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(reader) })
	rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, rw.w.op, time.Now())
	})
	rw.w.op = reader.ID // the restarted controller's reader: the task's operator is revoked
	rw.passesFor(t, 4, 0)
	for kind, v := range nodesOf(rw.w, rw.task) {
		if v.State.State == types.NodeStateStateClaimed || v.State.State == types.NodeStateStateCompleted {
			t.Errorf("the %s ran: %s", kind, v.State.State)
		}
		rw.w.view(func(tx *eventstore.Tx) error {
			if _, started, err := lease.FirstOn(tx, graph.UnitOf(v.Node.ID)); err != nil || started {
				t.Errorf("an attempt started on the %s: %v", kind, err)
			}
			return nil
		})
	}
	r := restartOn(t, rw)
	if c := r.card(t, rw.task); c.Column != types.BoardPlacedCardColumnDone || !c.Abandoned {
		t.Errorf("the card: %+v", c)
	}
	if code := r.wait(t, rw.task, "closed"); code != entry.ExitOK {
		t.Errorf("task wait --until closed: %d", code)
	}
}

// taskState is task's record's state and done-as.
func taskState(t *testing.T, w *seeder, task types.TaskID) (string, string) {
	t.Helper()
	var r authority.TaskRecord
	w.view(func(tx *eventstore.Tx) error {
		raw, _, err := authority.TaskRecordOf(task)(tx)
		if err == nil {
			err = json.Unmarshal(raw, &r)
		}
		return err
	})
	return r.State, r.DoneAs
}

// 22a-7: the loop records the admitted task's task.done as a candidate
// once its candidate is decided (graph.CandidateGo, the board's
// done_candidate rule) and none of its nodes is claimed or held:
//   - while a node of it is claimed (a plan recorded in its round and
//     claimed), the snapshot decides no Done; once it settles, it is done;
//   - a Done expecting the task's version a later event moved is refused
//     stale, recording nothing; the next pass decides it again, and it is
//     recorded once.
func TestTheLoopEndsADecidedCandidate(t *testing.T) {
	rw := candidateRun(t, nil)
	if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
		t.Fatal("the task ended before its review's GO was observed")
	}
	var stale reconcile.Versioned
	rw.w.view(func(tx *eventstore.Tx) (err error) {
		stale.ID = string(rw.task)
		stale.Version, err = tx.SubjectVersion(stale.ID)
		stale.Version--
		return err
	})
	head := rw.w.head()
	if err := rw.x.Run(context.Background(), reconcile.Done{Task: stale}); !errors.Is(err, eventstore.ErrStaleExpectedState) || rw.w.head() != head {
		t.Fatalf("a Done at a moved version: %v (head %d → %d)", err, head, rw.w.head())
	}
	// A node of the task claimed (a plan recorded and claimed in the
	// candidate's round): the candidate is decided, but not settled.
	plan := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	rw.w.do(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: rw.task, Project: graphProject, Node: plan})
	})
	rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan.ID, map[string]graph.Resolved{}, 1)
	})
	noDone := func(what string) {
		rw.w.view(func(tx *eventstore.Tx) error {
			s, err := reconcile.Observe(tx, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true})
			if err != nil || s.Candidate || s.Ending != nil || slices.ContainsFunc(reconcile.Decide(s), func(a reconcile.Action) bool {
				_, done := a.(reconcile.Done)
				return done
			}) {
				t.Errorf("a decided candidate with a node %s: candidate %v, ending %v, %v", what, s.Candidate, s.Ending, err)
			}
			return nil
		})
	}
	noDone("claimed")
	// Opus #408 M1 (E2): a held node, likewise.
	hid := types.HoldID(newID(t, "hld"))
	rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Raise(tx, plan.ID, types.NodeHold{ID: hid, Hold: vocab.HoldReviewerUnavailable, SignalInstance: "test:held", Owner: types.OwnerSystem,
			Pages: []types.NodeHoldPagesItem{}}, nil)
	})
	noDone("held")
	rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return graph.Clear(tx, plan.ID, hid, "test:held") })
	rw.passesFor(t, 6, 0) // the loop records the plan, then ends the task
	rw.w.view(func(tx *eventstore.Tx) error {
		v, err := graph.ViewOf(tx, plan.ID)
		if err != nil || v.State.State != types.NodeStateStateCompleted {
			t.Fatalf("the extra plan did not settle: %+v, %v", v.State, err)
		}
		return nil
	})
	if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
		t.Fatalf("the decided candidate: %s %s", st, as)
	}
	if n := count(t, rw.w.dir, `SELECT count(*) FROM events WHERE kind = '`+authority.TaskDoneEvent+`'`); n != 1 {
		t.Errorf("%d task.done recorded", n)
	}
}

// 22a-7 (Main): the loop's task.done takes the last build round's review
// only (graph.LastBuildRound, 6d's rule, CandidateGo's):
//   - round 1 no_go, then round 2 (the repair) go: the task is not done
//     after round 1, and is done once round 2's review is, once;
//   - round 1 no_go with the rounds rail at one round: round 2's build is
//     held on the rail, and the task is never done.
func TestADoneCandidateIsTheLastRounds(t *testing.T) {
	t.Run("no_go, then go", func(t *testing.T) {
		rw, _ := reviewRun(t, noGoDecision)
		rw.passesFor(t, 1, 0) // the repair: round 2 recorded
		if roundOf(t, rw, 2).build.Node.ID == "" {
			t.Fatal("round 1's no_go recorded no repair")
		}
		if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
			t.Fatal("the task is done after a no_go")
		}
		rw.r.adapters = &peerStarter{peer: reviewer(goDecision, false, nil)}
		for range 40 {
			if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
				break
			}
			rw.passesFor(t, 1, 0)
		}
		two := roundOf(t, rw, 2)
		if two.review.State.State != types.NodeStateStateCompleted || two.review.Outcome == nil || *two.review.Outcome != types.OutcomeEmpty {
			t.Fatalf("round 2's review: %s %v", two.review.State.State, two.review.Outcome)
		}
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
			rw.w.view(func(tx *eventstore.Tx) error {
				nodes, _ := graph.TaskNodes(tx, rw.task)
				for _, n := range nodes {
					t.Logf("%s r%d %s %v holds %+v", n.Node.Kind, n.Node.Round, n.State.State, n.Outcome, n.State.Holds)
				}
				return nil
			})
			t.Fatalf("after round 2's go: %s %s", st, as)
		}
		if n := count(t, rw.w.dir, `SELECT count(*) FROM events WHERE kind = '`+authority.TaskDoneEvent+`'`); n != 1 {
			t.Errorf("%d task.done recorded", n)
		}
	})
	t.Run("no_go, the rounds rail held", func(t *testing.T) {
		rw := gateWorld(t, goEvents("TestA=pass"), map[string]string{"gate-events.json": goEvents("TestA=pass", hiddenTest+"=fail")}, nil)
		if v := rw.gating(t); v.State.State != types.NodeStateStateCompleted {
			t.Fatalf("the gate: %s", v.State.State)
		}
		rails := rw.x.Env.Rails
		oneRound := func(tx *eventstore.Tx, task types.TaskID) (graph.Caps, error) {
			c, err := rails(tx, task)
			c.RoundsPerTask = 1
			return c, err
		}
		rw.x.Env.Rails, rw.r.env.Rails = oneRound, oneRound
		rw.reviewRunning(stubRegistry(t, nil, nil), reviewer(noGoDecision, false, nil))
		var two round
		for range 24 {
			if two = roundOf(t, rw, 2); slices.ContainsFunc(two.build.State.Holds, func(h types.NodeHold) bool { return h.Hold == vocab.HoldBudgetRail }) {
				break
			}
			rw.passesFor(t, 1, 0)
		}
		if two.build.Node.ID == "" || len(two.build.State.Holds) == 0 {
			t.Fatalf("round 2's build: %+v", two.build.State)
		}
		rw.passesFor(t, 4, 0)
		if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
			t.Error("the task is done with its last round held")
		}
		// Opus #408 L3: a gate red on the hidden test keeps its name off
		// every read, the hold badges and task show included.
		restartOn(t, rw).noLeak(t, rw.task, hiddenTest)
	})
}

// unsealedReview is candidateRun whose review's go result is committed
// and whose seal fails (sealWith) from the review's run on, failing at
// most fails seals (-1: every one): its attempt left finishing, its
// lease active, as a failed blob publish leaves it (Opus #408 H1). The
// runner's log is returned; retryBase is a minute, so a pass never
// retries twice in the rows' time.
func unsealedReview(t *testing.T, fails int) (*runWorld, *logBuffer) {
	t.Helper()
	var n atomic.Int64
	log := &logBuffer{}
	rw := candidateRun(t, func(rw *runWorld) {
		rw.r.log = log
		sealWith = func(tx *eventstore.Tx, a types.AttemptID, rev int64, u lease.Units, p lease.Publisher, d *types.Digest, ig []string, by types.ActorID) ([]eventstore.Event, error) {
			if fails < 0 || n.Add(1) <= int64(fails) {
				return nil, errors.New("the seal's blob could not be published under /var/lib/adc/blobs")
			}
			return lease.SealWith(tx, a, rev, u, p, d, ig, by)
		}
	})
	t.Cleanup(func() { sealWith = lease.SealWith })
	return rw, log
}

// endingCard is the task read's card for task, as the operator, through
// the API (no restart: the restart's sweep would end what still runs).
func endingCard(t *testing.T, rw *runWorld) types.BoardPlacedCard {
	t.Helper()
	placed, ok := taskReadAs(t, rw.w, rw.w.op, rw.task).Card.Variant.(*types.BoardPlacedCard)
	if !ok {
		t.Fatal("the task's card is not placed")
	}
	return *placed
}

// Opus #408 H1 (#226 entry 166): a decided candidate whose review attempt
// did not end is ended at runtime, by the runner's End (endOne), never by
// a restart:
//   - its seal failing once: the task ends at runtime;
//   - its seal never landing: End retries with backoff (one try per
//     retryBase, however many passes), the cause logged once; the card
//     says ending_blocked, with the unit and the controller's phrase (one
//     of authority.EndingCauses, never the error's text); the task is not
//     done; once the fault clears and the retry is due, the task ends with
//     no restart.
func TestADecidedCandidateEndsAtRuntime(t *testing.T) {
	t.Run("the seal fails once", func(t *testing.T) {
		rw, _ := unsealedReview(t, 1)
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
			t.Fatalf("the task after one failed seal: %s %s", st, as)
		}
	})
	t.Run("the seal never lands, then does", func(t *testing.T) {
		rw, log := unsealedReview(t, -1)
		review := reviewNodes(rw)[types.NodeKindReview]
		if review.State.State != types.NodeStateStateCompleted {
			t.Fatalf("the review: %s", review.State.State)
		}
		rw.passesFor(t, 12, 0)
		rw.r.mu.Lock()
		tries := rw.r.ending[review.Node.ID].tries
		rw.r.mu.Unlock()
		if tries != 1 {
			t.Errorf("End tried %d times in 12 passes within retryBase, want 1", tries)
		}
		if n := strings.Count(log.String(), "could not be ended"); n != 1 {
			t.Errorf("the cause logged %d times: %s", n, log.String())
		}
		rw.r.mu.Lock()
		e := rw.r.ending[review.Node.ID]
		e.next = time.Now() // a second try due, the same cause: not logged again
		rw.r.ending[review.Node.ID] = e
		rw.r.mu.Unlock()
		rw.passesFor(t, 4, 0)
		rw.r.mu.Lock()
		tries = rw.r.ending[review.Node.ID].tries
		rw.r.mu.Unlock()
		if n := strings.Count(log.String(), "could not be ended"); tries != 2 || n != 1 {
			t.Errorf("a second try: %d tries, the cause logged %d times: %s", tries, n, log.String())
		}
		if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
			t.Fatal("the task is done with its review's attempt unsealed")
		}
		c := endingCard(t, rw)
		if c.EndingBlocked == nil || c.EndingBlocked.Unit != graph.UnitOf(review.Node.ID) || string(c.EndingBlocked.Cause) != authority.EndingSeal ||
			c.Column != types.BoardPlacedCardColumnDoneCandidate {
			t.Fatalf("the card: %+v (blocked %+v)", c, c.EndingBlocked)
		}
		raw, _ := json.Marshal(c)
		if !slices.Contains(authority.EndingCauses, string(c.EndingBlocked.Cause)) || bytes.Contains(raw, []byte("/var/lib")) || bytes.Contains(raw, []byte("blob")) {
			t.Errorf("the card's cause is not one of the controller's phrases: %s", raw)
		}
		sealWith = lease.SealWith // the fault clears
		rw.r.mu.Lock()
		e = rw.r.ending[review.Node.ID]
		e.next = time.Now() // the retry is due (retryBase passed)
		rw.r.ending[review.Node.ID] = e
		rw.r.mu.Unlock()
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
			t.Fatalf("the task once the fault cleared: %s %s", st, as)
		}
		if c := endingCard(t, rw); c.EndingBlocked != nil {
			t.Errorf("the ended task's card: %+v", c.EndingBlocked)
		}
	})
}

// Opus #408 re-check M1: an open-index entry End cannot act on (here the
// build's own completed attempt, its lease settled, re-planted in the open
// index: a shape adcd never writes, which stands for any entry endEntry
// does not cover; Mythos #408 L7) is a failed try: one try, one
// log line and no nudge over 10 passes, the task still admitted, its card
// ending_blocked. Never a hot loop.
func TestAnEntryEndCannotEndBacksOff(t *testing.T) {
	log := &logBuffer{}
	rw := candidateRun(t, func(rw *runWorld) {
		rw.r.log = log
		a, ok, err := firstAttempt(rw)
		if err != nil || !ok {
			t.Fatalf("the build's attempt: %v %v", ok, err)
		}
		execSQL(t, rw.w.dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+lease.OpenProjection+`', '`+string(graph.UnitOf(rw.build))+`/`+string(a)+`', 'true')`)
	})
	nudged := rw.nudged.Load()
	rw.passesFor(t, 10, 0)
	rw.r.mu.Lock()
	tries := rw.r.ending[rw.build].tries
	rw.r.mu.Unlock()
	if tries != 1 || rw.nudged.Load() != nudged {
		t.Errorf("10 passes: %d tries, %d nudges", tries, rw.nudged.Load()-nudged)
	}
	if n := strings.Count(log.String(), "could not be ended"); n != 1 {
		t.Errorf("logged %d times: %s", n, log.String())
	}
	if st, _ := taskState(t, rw.w, rw.task); st != authority.TaskAdmitted {
		t.Errorf("the task: %s", st)
	}
	if c := endingCard(t, rw); c.EndingBlocked == nil || c.EndingBlocked.Unit != graph.UnitOf(rw.build) {
		t.Errorf("the card: %+v", c.EndingBlocked)
	}
}

// Opus #408 re-check M2: a decided candidate still ending whose approval
// is then revoked is ended at runtime too (Ending before Abandon): once the
// fault clears, its unit ends and the abandonment records, with no restart.
func TestARevokedTaskStillEndingIsEnded(t *testing.T) {
	rw, _ := unsealedReview(t, -1)
	review := reviewNodes(rw)[types.NodeKindReview]
	rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, rw.w.op, time.Now())
	})
	rw.passesFor(t, 4, 0)
	if st, _ := taskState(t, rw.w, rw.task); st != authority.TaskAdmitted {
		t.Fatalf("the revoked task with its review's seal failing: %s", st)
	}
	sealWith = lease.SealWith // the fault clears
	rw.r.mu.Lock()
	e := rw.r.ending[review.Node.ID]
	e.next = time.Now()
	rw.r.ending[review.Node.ID] = e
	rw.r.mu.Unlock()
	rw.passesFor(t, 4, 0)
	if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
		t.Fatalf("the revoked task once its unit ended: %s %s", st, as)
	}
}

// Opus #408 re-check L1: endable sees a claimed node before an open unit
// on an earlier node: with the review's attempt left finishing and a plan
// claimed after it, the loop decides no Ending (nor Done): the claimed
// plan is evaluated first.
func TestAClaimedNodeComesBeforeAnOpenUnit(t *testing.T) {
	rw, _ := unsealedReview(t, -1)
	plan := types.Node{ID: types.NodeID(newID(t, "nod")), Kind: types.NodeKindPlan, DefinitionVersion: "1", Round: 1, Params: json.RawMessage(`{}`),
		Inputs: []types.InputSpec{}, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}
	rw.w.do(rw.w.sys, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Record(types.NodeRecorded{Task: rw.task, Project: graphProject, Node: plan})
	})
	rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return graph.Claim(tx, plan.ID, map[string]graph.Resolved{}, 1)
	})
	rw.w.view(func(tx *eventstore.Tx) error {
		s, err := reconcile.Observe(tx, reconcile.Gates{Admits: true, Evaluates: true, Reviews: true})
		if err != nil || s.Candidate || s.Ending != nil {
			t.Errorf("a claimed plan and an open unit: candidate %v, ending %+v, %v", s.Candidate, s.Ending, err)
		}
		return nil
	})
}

// Opus #408 re-check M2, an uncandidated task: a build whose seal did not
// land, its approval then revoked, is ended at runtime and abandoned.
func TestARevokedBuildStillEndingIsEnded(t *testing.T) {
	rw, a, _ := unsealedRun(t)
	rw.x.Acceptor = rw.r // adcd's: its runner (wiring.go)
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, rw.w.op, time.Now())
	})
	rw.passesFor(t, 4, 0)
	if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
		t.Fatalf("the revoked task with its build's attempt %s left finishing: %s %s", a.ID, st, as)
	}
}

// Opus #408 re-check M1: a try that ends what was named, after which
// another entry runs, is progress (it nudges, no backoff); the next, which
// End cannot end, backs off.
func TestATryThatEndsSomethingIsProgress(t *testing.T) {
	rw, log := unsealedReview(t, 1)
	review := reviewNodes(rw)[types.NodeKindReview]
	execSQL(t, rw.w.dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+lease.OpenProjection+`', '`+string(graph.UnitOf(review.Node.ID))+`/att_7zzzzzzzzzzzzzzzzzzzzzzzzz', 'true')`)
	nudged := rw.nudged.Load()
	rw.passesFor(t, 6, 0)
	rw.r.mu.Lock()
	tries := rw.r.ending[review.Node.ID].tries
	rw.r.mu.Unlock()
	if tries != 1 || rw.nudged.Load()-nudged != 1 || strings.Count(log.String(), "could not be ended") != 1 {
		t.Errorf("a try ending the review's attempt, then one ending nothing: %d tries, %d nudges: %s", tries, rw.nudged.Load()-nudged, log.String())
	}
}

// noRuns starts no run: a granted lease stays granted, never launched.
type noRuns struct{}

func (noRuns) Run(types.NodeID, types.LeaseID) bool { return false }

// Mythos chunk 5a H1: an open entry that is a lease is ended too:
//   - the review's attempt completed, its lease's end failing (the run's
//     own end and End's first try): the open entry is the lease; End's
//     next due try ends it by its attempt (endOne's completed branch), and
//     the task ends at runtime;
//   - a revoked task whose build's lease was granted and never launched:
//     End ends the lease as one never launched (endLease: refused and
//     released), and the abandonment records.
func TestALeaseEntryIsEnded(t *testing.T) {
	t.Run("the lease's end fails, then works", func(t *testing.T) {
		var n atomic.Int64
		rw := candidateRun(t, func(rw *runWorld) {
			changeLease = func(tx *eventstore.Tx, id types.LeaseID, rev int64, e lease.LeaseEvent, h map[string]types.Hold) ([]eventstore.Event, error) {
				if e.Kind == lease.Completed && n.Add(1) <= 2 {
					return nil, errors.New("the store is busy")
				}
				return lease.ChangeLease(tx, id, rev, e, h)
			}
		})
		t.Cleanup(func() { changeLease = lease.ChangeLease })
		review := reviewNodes(rw)[types.NodeKindReview]
		rw.passesFor(t, 3, 0)
		if c := endingCard(t, rw); c.EndingBlocked == nil || string(c.EndingBlocked.Cause) != authority.EndingLease {
			t.Fatalf("the card with the review's lease not ended: %+v", c.EndingBlocked)
		}
		if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
			t.Fatal("the task is done with its review's lease active")
		}
		rw.r.mu.Lock()
		e := rw.r.ending[review.Node.ID]
		e.next = time.Now() // the retry is due
		rw.r.ending[review.Node.ID] = e
		rw.r.mu.Unlock()
		rw.passesFor(t, 3, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsCandidate) {
			t.Fatalf("the task at the next due try: %s %s", st, as)
		}
	})
	t.Run("a revoked task's lease never launched", func(t *testing.T) {
		rw := newRunWorld(t, true, builder(nil, false, nil))
		rw.x.Runner, rw.x.Acceptor = noRuns{}, rw.r
		rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
		for range 12 {
			if rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]; rw.build != "" {
				if _, live := leaseOn(rw.w, rw.build); live {
					break
				}
			}
			rw.passesFor(t, 1, 0)
		}
		l, live := leaseOn(rw.w, rw.build)
		if !live || l.State != types.LeaseStateGranted {
			t.Fatalf("the build's lease: %s (live %v)", l.State, live)
		}
		rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return identity.RevokeActor(tx, rw.w.op, time.Now())
		})
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with a granted lease: %s %s", st, as)
		}
	})
}

// sealOnceFails fails sealWith's first call, as H1's own fixture fails it
// (the blob publish), and lets every later one through.
func sealOnceFails(t *testing.T) {
	t.Helper()
	var n atomic.Int64
	sealWith = func(tx *eventstore.Tx, a types.AttemptID, rev int64, u lease.Units, p lease.Publisher, d *types.Digest, ig []string, by types.ActorID) ([]eventstore.Event, error) {
		if n.Add(1) == 1 {
			return nil, errors.New("the seal's blob could not be published under /var/lib/adc/blobs")
		}
		return lease.SealWith(tx, a, rev, u, p, d, ig, by)
	}
	t.Cleanup(func() { sealWith = lease.SealWith })
}

// lostBuild is a run world whose build's agent disconnected (no result
// submitted) and whose seal then failed once: record's other steps landed,
// so the attempt is lost and unsealed, its lease revoked and settled, its
// node's could_not_run recorded with no attempt. The task's approval is
// then revoked, with adcd's runner as the loop's acceptor.
func lostBuild(t *testing.T) (*runWorld, types.AttemptID) {
	t.Helper()
	sealOnceFails(t)
	rw := newRunWorld(t, true, builder(nil, true, nil))
	rw.untilRun(t)
	a, l := rw.attemptOf(t)
	if a.Status != types.AttemptStatusLost || l.State != types.LeaseStateSettled {
		t.Fatalf("the build after a disconnect and a failed seal: attempt %s, lease %s", a.Status, l.State)
	}
	rw.x.Acceptor = rw.r // adcd's: its runner (wiring.go)
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, rw.w.op, time.Now())
	})
	return rw, a.ID
}

// attemptAt is attempt's record.
func attemptAt(w *seeder, attempt types.AttemptID) types.Attempt {
	var a types.Attempt
	w.view(func(tx *eventstore.Tx) (err error) {
		a, _, _, err = lease.AttemptAt(tx, attempt)
		return err
	})
	return a
}

// rawAttempt is a run world whose build's lease was granted under a runner
// that runs nothing, an attempt then started on it raw (no run of it goes
// on), and the task's approval revoked after edit (nil: none) acts on it.
func rawAttempt(t *testing.T, edit func(*runWorld, types.AttemptID)) (*runWorld, types.AttemptID) {
	t.Helper()
	rw := newRunWorld(t, true, builder(nil, false, nil))
	rw.x.Runner, rw.x.Acceptor = noRuns{}, rw.r
	rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
	for range 12 {
		if rw.build = templateNodes(rw.w, rw.task)[types.NodeKindBuild]; rw.build != "" {
			if _, live := leaseOn(rw.w, rw.build); live {
				break
			}
		}
		rw.passesFor(t, 1, 0)
	}
	l, live := leaseOn(rw.w, rw.build)
	if !live || l.State != types.LeaseStateGranted {
		t.Fatalf("the build's lease: %s (live %v)", l.State, live)
	}
	a := types.AttemptID(newID(t, "att"))
	rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		_, rev, _, err := lease.LeaseOf(tx, l.ID)
		if err != nil {
			return nil, err
		}
		return lease.StartAttempt(tx, l.ID, rev, a, emptyBase)
	})
	if edit != nil {
		edit(rw, a)
	}
	rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, rw.w.op, time.Now())
	})
	return rw, a
}

// Mythos #408 H1′: an attempt left lost or stopping, unsealed (an agent
// disconnect, or a review superseded while it runs, plus a failed seal),
// its lease already settled, is ended: sealed by End at runtime (a lost
// build whose seal failed once; a stopping review whose seal fails until
// the fault clears, then at End's next due try), or by the start sweep
// (endLeft) at a restart; the revoked task is then abandoned. A cut
// attempt's end records no result (record's could_not_run is the one; as
// recovery's Seal at a restart). A sealed attempt whose lease's revoke
// failed has its lease revoked and settled. An attempt running with no run
// (the host's finish and record's loss both failed; started raw here) is
// lost and sealed, nothing recorded, its lease revoked and settled; one
// finishing with no result recorded (record's CommitAttempt failed) is
// sealed with could_not_run/ambiguous_state recorded over its claim.
func TestALostOrStoppingAttemptIsEnded(t *testing.T) {
	t.Run("a lost build, at runtime", func(t *testing.T) {
		rw, a := lostBuild(t)
		before := len(resultsOn(t, rw.w, rw.build))
		rw.passesFor(t, 4, 0)
		if n := len(resultsOn(t, rw.w, rw.build)); before != 1 || n != before {
			t.Errorf("the build's results: %d, then %d after End (record's could_not_run is the one)", before, n)
		}
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with its build lost and unsealed: %s %s", st, as)
		}
		if got := attemptAt(rw.w, a); got.Status != types.AttemptStatusSealed || got.LostReason == nil {
			t.Errorf("the lost attempt: %s (lost %v)", got.Status, got.LostReason)
		}
		rw.r.mu.Lock()
		defer rw.r.mu.Unlock()
		if len(rw.r.ending) != 0 {
			t.Errorf("End failed a try: %+v", rw.r.ending)
		}
	})
	t.Run("a lost build, at a restart", func(t *testing.T) {
		rw, a := lostBuild(t)
		removeStale = func(*runner, context.Context) ([]string, error) { return nil, nil }
		t.Cleanup(func() {
			removeStale = func(r *runner, ctx context.Context) ([]string, error) {
				return r.box.RemoveStale(ctx, func(string) bool { return false })
			}
		})
		before := len(resultsOn(t, rw.w, rw.build))
		if err := rw.r.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := attemptAt(rw.w, a); got.Status != types.AttemptStatusSealed {
			t.Fatalf("the lost attempt after the start sweep: %s", got.Status)
		}
		if n := len(resultsOn(t, rw.w, rw.build)); n != before {
			t.Errorf("the sweep recorded over the lost attempt's claim: %d results, %d before", n, before)
		}
		rw.passesFor(t, 2, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task after the sweep: %s %s", st, as)
		}
		rw.r.mu.Lock()
		defer rw.r.mu.Unlock()
		if len(rw.r.ending) != 0 {
			t.Errorf("End tried after the sweep ended it: %+v", rw.r.ending)
		}
	})
	t.Run("a sealed build whose lease's revoke failed", func(t *testing.T) {
		var n atomic.Int64
		changeLease = func(tx *eventstore.Tx, id types.LeaseID, rev int64, e lease.LeaseEvent, h map[string]types.Hold) ([]eventstore.Event, error) {
			if e.Kind == lease.Revoked && n.Add(1) == 1 {
				return nil, errors.New("the store is busy")
			}
			return lease.ChangeLease(tx, id, rev, e, h)
		}
		t.Cleanup(func() { changeLease = lease.ChangeLease })
		rw := newRunWorld(t, true, builder(nil, true, nil))
		rw.untilRun(t)
		if a, l := rw.attemptOf(t); a.Status != types.AttemptStatusSealed || l.State != types.LeaseStateActive {
			t.Fatalf("the build after a disconnect and a failed revoke: attempt %s, lease %s", a.Status, l.State)
		}
		before := len(resultsOn(t, rw.w, rw.build))
		rw.x.Acceptor = rw.r
		rw.l = reconcile.NewLoop(rw.x, reconcile.Gates{Admits: true, Evaluates: true}, time.Hour, io.Discard)
		rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return identity.RevokeActor(tx, rw.w.op, time.Now())
		})
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with its sealed build's lease active: %s %s", st, as)
		}
		if _, l := rw.attemptOf(t); l.State != types.LeaseStateSettled {
			t.Errorf("the lease: %s", l.State)
		}
		if n := len(resultsOn(t, rw.w, rw.build)); n != before {
			t.Errorf("End recorded over the cut attempt's claim: %d results, %d before", n, before)
		}
	})
	t.Run("an attempt running with no run", func(t *testing.T) { // the host's finish and record's loss both failed: here started raw
		rw, a := rawAttempt(t, nil)
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with an attempt running and no run: %s %s", st, as)
		}
		if got := attemptAt(rw.w, a); got.Status != types.AttemptStatusSealed || got.LostReason == nil {
			t.Errorf("the attempt: %s (lost %v)", got.Status, got.LostReason)
		}
		if l, _ := leaseOn(rw.w, rw.build); l.State == types.LeaseStateActive {
			t.Errorf("its lease: %s", l.State)
		}
		if n := len(resultsOn(t, rw.w, rw.build)); n != 0 {
			t.Errorf("a cut attempt's End recorded %d results; its claim is matched again", n)
		}
	})
	t.Run("a finishing attempt with no result recorded", func(t *testing.T) { // record's CommitAttempt failed
		rw, a := rawAttempt(t, func(rw *runWorld, a types.AttemptID) {
			rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				at, _, _, err := lease.AttemptAt(tx, a)
				if err != nil {
					return nil, err
				}
				return lease.SubmitResult(tx, a, at.FencingToken, types.SubmittedResult{Call: types.CallID(newID(t, "cal")), Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
			})
		})
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with an attempt finishing: %s %s", st, as)
		}
		res := resultsOn(t, rw.w, rw.build)
		if got := attemptAt(rw.w, a); got.Status != types.AttemptStatusSealed || len(res) != 1 || res[0].Result.Outcome != types.OutcomeCouldNotRun || res[0].Result.Reason != vocab.ReasonAmbiguousState {
			t.Errorf("the finishing attempt: %s, its node's results %+v", got.Status, res)
		}
	})
	t.Run("a superseded review left stopping", func(t *testing.T) {
		var stopped atomic.Pointer[types.NodeID]
		var fails atomic.Bool
		fails.Store(true)
		rw := candidateRun(t, func(rw *runWorld) {
			sealWith = func(tx *eventstore.Tx, a types.AttemptID, rev int64, u lease.Units, p lease.Publisher, d *types.Digest, ig []string, by types.ActorID) ([]eventstore.Event, error) {
				if fails.Load() {
					return nil, errors.New("the seal's blob could not be published under /var/lib/adc/blobs")
				}
				return lease.SealWith(tx, a, rev, u, p, d, ig, by)
			}
			t.Cleanup(func() { sealWith = lease.SealWith })
			other := stubRegistry(t, nil, map[types.ExecutorID]types.Name{stubReviewer: "fam-b"})
			roster, err := graph.Roster([]types.ExecutorID{stubReviewer}, other)
			if err != nil {
				t.Fatal(err)
			}
			next, err := reviews{reg: other}.requirement(rw.task, graphProject, roster, 2)
			if err != nil {
				t.Fatal(err)
			}
			reviewerRuns = func() bool { // preempted while it runs, then its session ends
				if review := reviewNodes(rw)[types.NodeKindReview].Node.ID; stopped.CompareAndSwap(nil, &review) {
					rw.w.do(rw.w.sys, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
						return graph.SupersedeOnto(tx, review, next, other)
					})
					rw.w.do(rw.w.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
						return identity.RevokeActor(tx, rw.w.op, time.Now())
					})
				}
				return true
			}
		})
		t.Cleanup(func() { reviewerRuns = nil })
		review := stopped.Load()
		if review == nil {
			t.Fatal("no review ran")
		}
		var a types.Attempt
		var l types.Lease
		rw.w.view(func(tx *eventstore.Tx) error {
			first, _, err := lease.FirstOn(tx, graph.UnitOf(*review))
			if err == nil {
				a, _, _, err = lease.AttemptAt(tx, first)
			}
			if err == nil {
				l, _, _, err = lease.LeaseOf(tx, a.Lease)
			}
			return err
		})
		if a.Status != types.AttemptStatusStopping || l.State != types.LeaseStateSettled {
			t.Fatalf("the preempted review with its seal failing: attempt %s, lease %s", a.Status, l.State)
		}
		if st, _ := taskState(t, rw.w, rw.task); st == authority.TaskDoneState {
			t.Fatal("the task is done with its stopping attempt unsealed")
		}
		before := len(resultsOn(t, rw.w, *review))
		fails.Store(false) // the fault clears
		rw.r.mu.Lock()
		e := rw.r.ending[*review]
		e.next = time.Now() // End's retry is due
		rw.r.ending[*review] = e
		rw.r.mu.Unlock()
		rw.passesFor(t, 4, 0)
		if st, as := taskState(t, rw.w, rw.task); st != authority.TaskDoneState || as != string(types.TaskDoneAsAbandoned) {
			t.Fatalf("the revoked task with its superseded review left stopping: %s %s", st, as)
		}
		if got := attemptAt(rw.w, a.ID); got.Status != types.AttemptStatusSealed || got.StopReason == nil {
			t.Errorf("the stopped review's attempt: %s (stop %v)", got.Status, got.StopReason)
		}
		if n := len(resultsOn(t, rw.w, *review)); n != before {
			t.Errorf("End recorded over the superseded review: %d results, %d before", n, before)
		}
		rw.r.mu.Lock()
		defer rw.r.mu.Unlock()
		if e, failed := rw.r.ending[*review]; failed {
			t.Errorf("End's try once the fault cleared failed: %+v", e)
		}
	})
}
