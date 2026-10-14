// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gateway"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
	"github.com/ekaynac/auto-dev-cycle/internal/recovery"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	actor   = types.ActorID("act_01m3250v0aer9ss2097svmefj8")
	quota   = types.ResourceGroupID("rg_01m3250vm7f6ktw39dszqnyddr")
	reserve = 400 // the lease's reservation, in tokens
	child   = "ADC_RECOVERY_KILLED_CHILD"
)

var base = types.AttemptBase{Tree: "git:cccccccccccccccccccccccccccccccccccccccc", ContractRevision: "7"}

// units binds every unit to one project and task, and lets attempts start.
type units struct{}

func (units) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "prj_01m3250vm7f6ktw39dszqnydd0", "tsk_01m3250vm7f6ktw39dszqnydd0", true, nil
}

func (units) Startable(*eventstore.Tx, types.UnitID) error { return nil }

// world is a store composed as P22a will compose adcd's (leases with capacity, model-call bindings, the usage
// ledger), over a state directory a killed controller may have left.
type world struct {
	t     *testing.T
	s     *eventstore.Store
	blobs *blobstore.Store
	dir   string
}

func open(t testing.TB, dir string) (*eventstore.Store, *blobstore.Store, func()) {
	reg, err := vocab.Load()
	r, rerr := capacity.NewReservations([]types.ResourceGroup{{ID: quota, Kind: types.ResourceGroupKindAccountQuota,
		ReleaseRule: types.ResourceGroupReleaseRuleOnSettle, Members: []types.ExecutorID{},
		CapacityModel: types.ResourceGroupCapacityModel{Unit: types.CapacityUnitTokens, Limit: ptr(int64(1000)), QuotaState: types.ResourceGroupCapacityModelQuotaStateKnown}}})
	lock, lerr := eventstore.AcquireLock(filepath.Join(dir, "state"))
	if err = errors.Join(err, rerr, lerr); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := lease.Kinds()
	maps.Copy(kinds, modelcall.Kinds())
	maps.Copy(kinds, ledger.Kinds())
	ok := func(*eventstore.Tx, eventstore.Command) error { return nil }
	lp := lease.Projector{Blobs: blobs, Holds: reg.Holds, Units: units{}, Groups: r}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Effects: modelcall.Effects(),
		Hooks: eventstore.Hooks{Identify: ok, Authorize: ok, Project: eventstore.ProjectEach(capacity.Hook(lp, r), modelcall.Project, ledger.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	return s, blobs, func() { s.Close(); lock.Release() }
}

func ptr[T any](v T) *T { return &v }

// passing is a placement whose every check passed.
func passing() types.LeasePlacementChecks {
	pass := types.PlacementCheck{Outcome: types.Outcome(vocab.Empty), Reason: vocab.ReasonCompletedClean}
	return types.LeasePlacementChecks{DataFloor: pass, Destinations: pass, Capability: pass, Independence: pass,
		PaidAuthority: pass, Budget: pass, FallbackScope: pass}
}

// restarted opens dir's store as a restarted controller does: the restart rule first.
func restarted(t *testing.T, dir string) *world {
	t.Helper()
	s, blobs, done := open(t, dir)
	t.Cleanup(done)
	if _, err := s.MarkPendingUncertain(context.Background(), string(actor)); err != nil {
		t.Fatal(err)
	}
	return &world{t: t, s: s, blobs: blobs, dir: dir}
}

func (w *world) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	cmd, err := ids.New("cmd")
	if err != nil {
		return err
	}
	_, err = w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(actor), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)},
		eventstore.Applies(func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (w *world) must(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if err := w.do(fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) view(fn func(tx *eventstore.Tx) error) {
	w.t.Helper()
	if err := w.s.View(context.Background(), fn); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) recovery() recovery.Recovery {
	return recovery.Recovery{Store: w.s, Actor: actor, Units: units{}, Blobs: w.blobs, Exchanges: filepath.Join(w.dir, "exchanges")}
}

func (w *world) run() {
	w.t.Helper()
	if err := w.recovery().Run(context.Background()); err != nil {
		w.t.Fatal(err)
	}
}

// cut is what the killed controller was running: L1, A1 and A1's call.
type cut struct {
	Lease   types.LeaseID
	Attempt types.AttemptID
	Call    types.CallID
	Unit    types.UnitID
}

func newID(t testing.TB, prefix string) string {
	id, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// midCall records, in w, what a controller has durable mid-call (X3's barrier-builder scenario): L1 granted with its
// reservation, A1 running under it, A1's call intended, its request's floor recorded before the send (the gateway
// sent it), and two usage deltas; and A1's exchange root, as the adapter host made it.
func (w *world) midCall(t testing.TB) cut {
	c := cut{Lease: types.LeaseID(newID(t, "lea")), Attempt: types.AttemptID(newID(t, "att")), Call: types.CallID(newID(t, "cal")),
		Unit: types.UnitID(newID(t, "unt"))}
	l := types.Lease{ID: c.Lease, Unit: c.Unit, Role: "builder", Executor: types.ExecutorID(newID(t, "exe")), State: types.LeaseStateRequested,
		Reservations: []types.LeaseReservationsItem{{Group: quota, Amount: reserve, Unit: types.CapacityUnitTokens}},
		Footprint:    types.LeaseFootprint{ContextTokens: 1, ExpectedTurns: 1}, Deadline: "2026-10-04T14:00:00Z", PlacementChecks: passing()}
	steps := []func(tx *eventstore.Tx) ([]eventstore.Event, error){
		func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestGranted(tx, l) },
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.StartAttempt(tx, c.Lease, 2, c.Attempt, base)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return modelcall.Intend(tx, gateway.Binding{Lease: c.Lease, Attempt: c.Attempt, Call: c.Call}, "stub")
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var evs []eventstore.Event
			for seq, n := range [][2]int64{{200, 100}, {150, 100}} {
				ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: c.Call, Attempt: c.Attempt, Lease: c.Lease,
					Kind: types.UsageMeasurementKindDelta, Sequence: int64(seq + 1), Tokens: types.UsageMeasurementTokens{In: n[0], Out: n[1]},
					Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
					SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
				if err != nil {
					return nil, err
				}
				evs = append(evs, ev)
			}
			return evs, nil
		},
	}
	for _, step := range steps {
		if err := w.do(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := (modelcall.Recorder{Store: w.s, Actor: string(actor)}).Count(context.Background(),
		gateway.Binding{Lease: c.Lease, Attempt: c.Attempt, Call: c.Call}, 0, gateway.Count{In: 4, Out: 64}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.exchange(c.Attempt), "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	return c
}

func (w *world) exchange(a types.AttemptID) string {
	return filepath.Join(w.dir, "exchanges", "exchange-"+string(a)+"-0123")
}

// The killed controller: it records a call in flight, reports it, and waits for its kill -9.
func killedChild(dir string) {
	s, blobs, _ := open(fatal{}, dir)
	if _, err := s.MarkPendingUncertain(context.Background(), string(actor)); err != nil {
		panic(err)
	}
	w := &world{s: s, blobs: blobs, dir: dir}
	c := w.midCall(fatal{})
	raw, _ := json.Marshal(c)
	fmt.Println(string(raw))
	time.Sleep(time.Hour)
}

// fatal is a testing.TB for the child, which has no test to fail: it panics.
type fatal struct{ testing.TB }

func (fatal) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

// killMidCall runs a controller in a child process until its call is in flight, kills it with SIGKILL, and lets its
// adapter, which outlives it, write partial output into A1's exchange root.
func killMidCall(t *testing.T) (string, cut) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillMidCall$")
	cmd.Env = append(os.Environ(), child+"="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var c cut
	line, err := bufio.NewReader(out).ReadString('\n')
	if err == nil {
		err = json.Unmarshal([]byte(line), &c)
	}
	if err != nil {
		t.Fatalf("the child reported %q: %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("the child ended %v, not killed", err)
	}
	partial := filepath.Join(dir, "exchanges", "exchange-"+string(c.Attempt)+"-0123", "out", "partial.patch")
	if err := os.WriteFile(partial, []byte("diff --git a/x b/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

// events is every event of the store, in order.
func (w *world) events() []eventstore.Event {
	w.t.Helper()
	var evs []eventstore.Event
	w.view(func(tx *eventstore.Tx) (err error) { evs, err = tx.EventsAfter(0, 10000); return err })
	return evs
}

// payloadOf decodes a lease event's payload.
type payloadOf struct {
	Transition string         `json:"transition"`
	Reason     *string        `json:"reason"`
	Lease      *types.Lease   `json:"lease"`
	Attempt    *types.Attempt `json:"attempt"`
	Successor  *types.Attempt `json:"successor"`
}

// recoverySteps is X3's recovery sequence as the log records it after the kill: one line per event.
func recoverySteps(w *world, after int64) []string {
	var got []string
	for _, ev := range w.events() {
		if ev.Seq <= after {
			continue
		}
		var p payloadOf
		_ = json.Unmarshal(ev.Payload, &p)
		switch {
		case ev.Kind == lease.OutputRefused:
			var r types.AttemptOutputRefusal
			_ = json.Unmarshal(ev.Payload, &r)
			got = append(got, fmt.Sprintf("%s %s %s", ev.Kind, r.Handle.Handle, r.Check))
		case p.Lease != nil && p.Attempt == nil:
			reason := ""
			if p.Reason != nil {
				reason = " " + *p.Reason
			}
			got = append(got, fmt.Sprintf("%s %s%s", ev.Kind, p.Lease.State, reason))
		case p.Attempt != nil && p.Successor != nil:
			got = append(got, fmt.Sprintf("%s %s token %d", ev.Kind, p.Successor.Status, p.Successor.FencingToken))
		case p.Attempt != nil:
			got = append(got, fmt.Sprintf("%s %s", ev.Kind, p.Attempt.Status))
		default:
			got = append(got, ev.Kind)
		}
	}
	return got
}

func head(w *world) int64 {
	evs := w.events()
	return evs[len(evs)-1].Seq
}

// F (P21, X3): kill -9 mid-call. A controller is killed while A1's call is in flight (its floor and two deltas
// durable); A1's adapter then writes partial output. After the restart, recovery runs X3's steps 1 to 4, each its own
// command, in order; the partial output is refused after step 1 and never ingested; the lease settles the call as a
// liability of max(reservation, deltas); placement may then start A2 under L2, superseding A1 with token 2. A second
// restart changes nothing, and projections equal replay.
func TestKillMidCall(t *testing.T) {
	if dir := os.Getenv(child); dir != "" {
		killedChild(dir)
		return
	}
	dir, c := killMidCall(t)
	w := restarted(t, dir)
	before := head(w)
	w.run()
	want := []string{
		"attempt.lost/v1 lost",
		"attempt.output_refused/v1 partial.patch stale_attempt",
		"lease.revoked/v1 settling controller_restart",
		"attempt.sealed/v1 sealed",
		"lease.settled/v1 settled",
	}
	if got := recoverySteps(w, before); !slices.Equal(got, want) {
		t.Fatalf("the recovery sequence is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(w.exchange(c.Attempt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("A1's exchange root is left: %v", err)
	}
	var l1 types.Lease
	var liable, outstanding int64
	w.view(func(tx *eventstore.Tx) error {
		var err, lerr, oerr error
		l1, _, _, err = lease.LeaseOf(tx, c.Lease)
		liable, lerr = capacity.Liable(tx, quota)
		outstanding, oerr = capacity.Outstanding(tx, quota)
		return errors.Join(err, lerr, oerr)
	})
	// The call's usage is unsettled (no final, a floor alone): its liability is the larger of the reservation (400)
	// and its deltas (350 in, 200 out); the reservation returned.
	if s := l1.Settlement; s == nil || len(s.Calls) != 0 || !slices.Equal(s.Liabilities, []types.CallID{c.Call}) || liable != 550 || outstanding != 0 {
		t.Fatalf("L1 settled %+v, liable %d, outstanding %d; want the call a liability of 550, nothing reserved", l1.Settlement, liable, outstanding)
	}
	if e, err := w.s.LoadEffect(context.Background(), modelcall.EffectOf(c.Call)); err != nil || e.State != eventstore.EffectUncertain {
		t.Fatalf("A1's call effect is %+v, %v; want uncertain", e, err)
	}

	// A second restart changes no figure.
	mark := head(w)
	w.run()
	var again int64
	w.view(func(tx *eventstore.Tx) (err error) { again, err = capacity.Liable(tx, quota); return err })
	if head(w) != mark || again != liable {
		t.Fatalf("a second recovery recorded %v, liable %d", recoverySteps(w, mark), again)
	}

	// Steps 5 and 6: L2 placed and granted, A2 started under it, superseding A1.
	l2 := l1
	l2.ID, l2.State, l2.Attempt, l2.Settlement = types.LeaseID(newID(t, "lea")), types.LeaseStateRequested, nil, nil
	a2 := types.AttemptID(newID(t, "att"))
	mark = head(w)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestGranted(tx, l2) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartNext(tx, l2.ID, 2, a2, base) })
	if got := recoverySteps(w, mark); !slices.Equal(got[2:], []string{"attempt.superseded/v1 running token 2"}) {
		t.Fatalf("the replacement: %v", got)
	}

	// The old attempt publishing after its replacement started is refused.
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SubmitResult(tx, c.Attempt, 1, types.SubmittedResult{Call: c.Call, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
	}); !errors.As(err, new(*lease.Stale)) {
		t.Fatalf("A1's late result: %v; want stale_attempt", err)
	}

	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("projections differ from replay: %v %+v", err, diffs)
	}
}

// replaced is a world after a controller died mid-call, recovery ran, and the replacement started: A2 running
// under L2, superseding A1.
func replaced(t *testing.T) (*world, cut, types.AttemptID) {
	w := restarted(t, t.TempDir())
	c := w.midCall(t)
	w.run()
	var l1 types.Lease
	w.view(func(tx *eventstore.Tx) (err error) { l1, _, _, err = lease.LeaseOf(tx, c.Lease); return err })
	l2 := l1
	l2.ID, l2.State, l2.Attempt, l2.Settlement = types.LeaseID(newID(t, "lea")), types.LeaseStateRequested, nil, nil
	a2 := types.AttemptID(newID(t, "att"))
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestGranted(tx, l2) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartNext(tx, l2.ID, 2, a2, base) })
	return w, c, a2
}

const emptyDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// F (P21): the old attempt publishing after its replacement starts. A1's adapter, which outlived the controller,
// leaves output after A2 started under L2: a file, a link to a file outside, a FIFO and a directory. At the next
// start each is refused by the fence (A1 is not the unit's current attempt), recorded as stale_attempt, and never
// ingested: nothing is published, the link is not followed, the FIFO is not opened. A2, cut by that restart, is lost
// and its lease settled; A1 stays superseded.
func TestTheOldAttemptPublishingAfterItsReplacementStarts(t *testing.T) {
	w, c, a2 := replaced(t)
	out := filepath.Join(w.exchange(c.Attempt), "out")
	outside := filepath.Join(w.dir, "outside")
	if err := errors.Join(os.MkdirAll(out, 0o755), os.WriteFile(filepath.Join(out, "result.json"), []byte(`{"late":true}`), 0o644),
		os.WriteFile(outside, []byte("not the adapter's"), 0o600), os.Symlink(outside, filepath.Join(out, "link")),
		unix.Mkfifo(filepath.Join(out, "fifo"), 0o644), os.Mkdir(filepath.Join(out, "dir"), 0o755)); err != nil {
		t.Fatal(err)
	}
	mark := head(w)
	done := make(chan error, 1)
	go func() { done <- w.recovery().Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovery blocked on an orphaned entry")
	}
	var names []string
	for _, ev := range w.events() {
		if ev.Seq <= mark || ev.Kind != lease.OutputRefused {
			continue
		}
		var r types.AttemptOutputRefusal
		if err := json.Unmarshal(ev.Payload, &r); err != nil {
			t.Fatal(err)
		}
		names = append(names, r.Handle.Handle)
		if r.Attempt != c.Attempt || r.Check != types.AttemptOutputRefusalCheckStaleAttempt || !strings.Contains(r.Detail, "is not the unit's current attempt") {
			t.Errorf("refusal %+v", r)
		}
		if has, err := w.blobs.Has(string(r.Handle.Digest)); err != nil || has || !strings.HasSuffix(r.Detail, "; not read") || r.Handle.Digest != emptyDigest {
			t.Errorf("%s was read or ingested: %+v, %v", r.Handle.Handle, r, err)
		}
	}
	if slices.Sort(names); !slices.Equal(names, []string{"dir", "fifo", "link", "result.json"}) {
		t.Fatalf("refused %v", names)
	}
	if got := recoverySteps(w, mark); got[0] != "attempt.lost/v1 lost" || got[len(got)-1] != "lease.settled/v1 settled" {
		t.Fatalf("the second restart: %v", got)
	}
	w.view(func(tx *eventstore.Tx) error {
		a1, _, _, err := lease.AttemptAt(tx, c.Attempt)
		cur, _, cerr := lease.CurrentAttempt(tx, c.Unit)
		if a1.Status != types.AttemptStatusSuperseded || cur != a2 {
			t.Errorf("A1 is %s, the unit's attempt %s", a1.Status, cur)
		}
		return errors.Join(err, cerr)
	})
}

// A crash between any two steps: recovery resumes from the records, and the sequence is recorded once.
func TestRecoveryResumesAfterACrashBetweenSteps(t *testing.T) {
	type step func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error)
	steps := []step{
		func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.RecoverAfterRestart
		},
		func(_ *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.ChangeLease(tx, c.Lease, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "controller_restart"}, nil)
			}
		},
		func(w *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				return lease.SealMinimal(tx, c.Attempt, 2, units{}, w.blobs, nil, actor)
			}
		},
	}
	want := []string{"attempt.lost/v1 lost", "lease.revoked/v1 settling controller_restart", "attempt.sealed/v1 sealed", "lease.settled/v1 settled"}
	for crash := range len(steps) + 1 {
		t.Run(fmt.Sprintf("after step %d", crash), func(t *testing.T) {
			w := restarted(t, t.TempDir())
			c := w.midCall(t)
			mark := head(w)
			for _, s := range steps[:crash] {
				w.must(s(w, c))
			}
			w.run()
			if got := recoverySteps(w, mark); !slices.Equal(got, want) {
				t.Fatalf("recorded %v", got)
			}
		})
	}
}
