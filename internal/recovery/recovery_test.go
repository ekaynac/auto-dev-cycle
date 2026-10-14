// SPDX-License-Identifier: Apache-2.0

//go:build linux

package recovery_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	return recovery.Recovery{Store: w.s, Actor: actor, Units: units{}, Blobs: w.blobs}
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
// sent it, here 4 input and 64 output tokens), and two usage deltas (350 input, 200 output in all).
func (w *world) midCall(t testing.TB) cut { return w.midCallFloored(t, gateway.Count{In: 4, Out: 64}) }

// midCallFloored is midCall with the request's floor.
func (w *world) midCallFloored(t testing.TB, floor gateway.Count) cut {
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
		gateway.Binding{Lease: c.Lease, Attempt: c.Attempt, Call: c.Call}, 0, floor); err != nil {
		t.Fatal(err)
	}
	return c
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

// killMidCall runs a controller in a child process until its call is in flight, and kills it with SIGKILL.
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

// stale is A1's late result, refused by the fence (stale_attempt) or not.
func (w *world) stale(c cut) error {
	return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.SubmitResult(tx, c.Attempt, 1, types.SubmittedResult{Call: c.Call, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
	})
}

// liable is the quota group's liability and outstanding reservation.
func (w *world) liable() (liable, outstanding int64) {
	w.view(func(tx *eventstore.Tx) error {
		var lerr, oerr error
		liable, lerr = capacity.Liable(tx, quota)
		outstanding, oerr = capacity.Outstanding(tx, quota)
		return errors.Join(lerr, oerr)
	})
	return liable, outstanding
}

// F (P21, X3): kill -9 mid-call. A controller is killed while A1's call is in flight (its floor and two deltas
// durable). After the restart, recovery runs X3's steps 1 to 4, each its own command, in order. From step 1 on, A1's
// late output is refused (stale_attempt). The lease settles the call as a liability of the field-wise maximum of the
// deltas (350/200) and the floor (4/64), 550, over the reservation (400). Placement then starts A2 under L2,
// superseding A1 with token 2. A second restart changes nothing, and projections equal replay.
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
		"lease.revoked/v1 settling controller_restart",
		"attempt.sealed/v1 sealed",
		"lease.settled/v1 settled",
	}
	if got := recoverySteps(w, before); !slices.Equal(got, want) {
		t.Fatalf("the recovery sequence is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := w.stale(c); !errors.As(err, new(*lease.Stale)) {
		t.Fatalf("A1's late result after step 1: %v; want stale_attempt", err)
	}
	var l1 types.Lease
	w.view(func(tx *eventstore.Tx) (err error) { l1, _, _, err = lease.LeaseOf(tx, c.Lease); return err })
	liable, outstanding := w.liable()
	if s := l1.Settlement; s == nil || len(s.Calls) != 0 || !slices.Equal(s.Liabilities, []types.CallID{c.Call}) || liable != 550 || outstanding != 0 {
		t.Fatalf("L1 settled %+v, liable %d, outstanding %d; want the call a liability of 550, nothing reserved", l1.Settlement, liable, outstanding)
	}
	if e, err := w.s.LoadEffect(context.Background(), modelcall.EffectOf(c.Call)); err != nil || e.State != eventstore.EffectUncertain {
		t.Fatalf("A1's call effect is %+v, %v; want uncertain", e, err)
	}

	// A second restart changes no figure.
	mark := head(w)
	w.run()
	if again, _ := w.liable(); head(w) != mark || again != liable {
		t.Fatalf("a second recovery recorded %v, liable %d", recoverySteps(w, mark), again)
	}

	// Steps 5 and 6: L2 granted, A2 started under it, superseding A1.
	mark = head(w)
	a2 := w.replace(c)
	if got := recoverySteps(w, mark); !slices.Equal(got[2:], []string{"attempt.superseded/v1 running token 2"}) {
		t.Fatalf("the replacement %s: %v", a2, got)
	}
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("projections differ from replay: %v %+v", err, diffs)
	}
}

// replace grants L2, like L1, and starts A2 under it (lease.StartNext): steps 5 and 6.
func (w *world) replace(c cut) types.AttemptID {
	w.t.Helper()
	var l1 types.Lease
	w.view(func(tx *eventstore.Tx) (err error) { l1, _, _, err = lease.LeaseOf(tx, c.Lease); return err })
	l2 := l1
	l2.ID, l2.State, l2.Attempt, l2.Settlement = types.LeaseID(newID(w.t, "lea")), types.LeaseStateRequested, nil, nil
	a2 := types.AttemptID(newID(w.t, "att"))
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestGranted(tx, l2) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StartNext(tx, l2.ID, 2, a2, base) }); err != nil {
		w.t.Fatal(err)
	}
	return a2
}

// F (P21): the old attempt publishing after its replacement starts. After A2 started under L2, A1's adapter, which
// outlived the controller, publishes its result: the fence refuses it (stale_attempt), as it refuses an output of A1
// at A2's token. A2's own output lands. Usage is not fenced: A1's late usage report is recorded. A restart then cuts
// A2, whose lease settles, and A1 stays superseded.
func TestTheOldAttemptPublishingAfterItsReplacementStarts(t *testing.T) {
	w := restarted(t, t.TempDir())
	c := w.midCall(t)
	w.run()
	a2 := w.replace(c)
	if err := w.stale(c); !errors.As(err, new(*lease.Stale)) || !strings.Contains(err.Error(), "is not the unit's current attempt") {
		t.Fatalf("A1's result after A2 started: %v; want stale_attempt", err)
	}
	w.view(func(tx *eventstore.Tx) error {
		if err := lease.FenceAttempt(tx, c.Attempt, 2); !errors.As(err, new(*lease.Stale)) {
			t.Errorf("A1's output at A2's token: %v", err)
		}
		return lease.FenceAttempt(tx, a2, 2)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		ev, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: c.Call, Attempt: c.Attempt, Lease: c.Lease,
			Kind: types.UsageMeasurementKindDelta, Sequence: 3, Tokens: types.UsageMeasurementTokens{In: 500, Out: 0},
			Cost:          types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
			SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
		return []eventstore.Event{ev}, err
	})
	if liable, _ := w.liable(); liable != 1050 { // late usage recomputes L1's liability: 850 in, 200 out
		t.Fatalf("liable %d after A1's late usage; want 1050", liable)
	}
	mark := head(w)
	w.run()
	if got := recoverySteps(w, mark); !slices.Equal(got, []string{"attempt.lost/v1 lost", "lease.revoked/v1 settling controller_restart",
		"attempt.sealed/v1 sealed", "lease.settled/v1 settled"}) {
		t.Fatalf("the restart after the replacement: %v", got)
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

// M7 (Opus on #259): a replacement while the old attempt's lease settles is refused at admission, whatever path
// granted L2: a crash after the seal left L1 settling, and a direct grant does not go through dispatch.
func TestNoReplacementBeforeTheOldLeaseSettles(t *testing.T) {
	w := restarted(t, t.TempDir())
	c := w.midCall(t)
	for _, step := range []func(tx *eventstore.Tx) ([]eventstore.Event, error){
		lease.RecoverAfterRestart,
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.ChangeLease(tx, c.Lease, 3, lease.LeaseEvent{Kind: lease.Revoked, Reason: "controller_restart"}, nil)
		},
		func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.SealMinimal(tx, c.Attempt, 2, units{}, w.blobs, nil, actor)
		},
	} {
		w.must(step)
	}
	var l1 types.Lease
	w.view(func(tx *eventstore.Tx) (err error) { l1, _, _, err = lease.LeaseOf(tx, c.Lease); return err })
	l2 := l1
	l2.ID, l2.State, l2.Attempt, l2.Settlement = types.LeaseID(newID(t, "lea")), types.LeaseStateRequested, nil, nil
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.RequestGranted(tx, l2) })
	err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return lease.StartNext(tx, l2.ID, 2, types.AttemptID(newID(t, "att")), base)
	})
	if err == nil || !strings.Contains(err.Error(), "settles before its attempt is replaced") {
		t.Fatalf("a replacement while L1 settles: %v", err)
	}
}

// M5 (Opus on #259): the liability is the token total of the field-wise maximum of the deltas (350 in, 200 out) and
// the gateway's floors (8 in, 1064 out): 350 + 1064 = 1414, over both sums (550, 1072) and the reservation (400).
func TestTheLiabilityIsTheFieldWiseMaximum(t *testing.T) {
	w := restarted(t, t.TempDir())
	w.midCallFloored(t, gateway.Count{In: 8, Out: 1064})
	w.run()
	if liable, outstanding := w.liable(); liable != 1414 || outstanding != 0 {
		t.Fatalf("liable %d, outstanding %d; want 1414 and 0", liable, outstanding)
	}
}

// M4 (Opus on #259): a crash may leave an attempt stopping, lost for another reason, or sealed from either, with its
// lease still active. At start, recovery converges each: revoked, sealed (abrupt_loss, or drain for a drain stop),
// settled. A finishing attempt holds an admitted result: it is the completion path's (P22a), left alone.
func TestRecoveryConvergesEveryCutAttempt(t *testing.T) {
	stop := func(r types.AttemptStopReasonValue) func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return func(_ *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return func(tx *eventstore.Tx) ([]eventstore.Event, error) { return lease.StopRunning(tx, c.Attempt, r) }
		}
	}
	lose := func(_ *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.LoseRunning(tx, c.Attempt, types.AttemptLostReasonValueAgentDisconnect)
		}
	}
	seal := func(w *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.SealMinimal(tx, c.Attempt, 2, units{}, w.blobs, nil, actor)
		}
	}
	finish := func(_ *world, c cut) func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return lease.SubmitResult(tx, c.Attempt, 1, types.SubmittedResult{Call: c.Call, Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean})
		}
	}
	for name, k := range map[string]struct {
		crash []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error)
		seal  types.AttemptRecordSealReason // "": left alone
	}{
		"stopping (revoke)":       {crash: []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error){stop(types.AttemptStopReasonValueRevoke)}, seal: types.AttemptRecordSealReasonAbruptLoss},
		"stopping (drain)":        {crash: []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error){stop(types.AttemptStopReasonValueDrain)}, seal: types.AttemptRecordSealReasonDrain},
		"lost (agent_disconnect)": {crash: []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error){lose}, seal: types.AttemptRecordSealReasonAbruptLoss},
		"sealed after a loss":     {crash: []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error){lose, seal}, seal: types.AttemptRecordSealReasonAbruptLoss},
		"finishing":               {crash: []func(*world, cut) func(tx *eventstore.Tx) ([]eventstore.Event, error){finish}},
	} {
		t.Run(name, func(t *testing.T) {
			w := restarted(t, t.TempDir())
			c := w.midCall(t)
			for _, s := range k.crash {
				w.must(s(w, c))
			}
			w.run()
			var a types.Attempt
			var l types.Lease
			var reason types.AttemptRecordSealReason
			w.view(func(tx *eventstore.Tx) error {
				var err, lerr error
				a, _, _, err = lease.AttemptAt(tx, c.Attempt)
				l, _, _, lerr = lease.LeaseOf(tx, c.Lease)
				if err == nil && a.Checkpoint != nil {
					rc, oerr := w.blobs.Open(string(*a.Checkpoint))
					if oerr != nil {
						return oerr
					}
					defer rc.Close()
					var rec types.AttemptRecord
					err = json.NewDecoder(rc).Decode(&rec)
					reason = rec.Seal.Reason
				}
				return errors.Join(err, lerr)
			})
			if k.seal == "" {
				if a.Status != types.AttemptStatusFinishing || l.State != types.LeaseStateActive {
					t.Fatalf("a finishing attempt was touched: %s, lease %s", a.Status, l.State)
				}
				return
			}
			if a.Status != types.AttemptStatusSealed || reason != k.seal || l.State != types.LeaseStateSettled {
				t.Fatalf("attempt %s sealed %q, lease %s; want sealed %s, settled", a.Status, reason, l.State, k.seal)
			}
		})
	}
}

// failingOnce is the blob store, failing its first publication.
type failingOnce struct {
	lease.Publisher
	calls *int
}

func (f failingOnce) Put(r io.Reader, declared string) (string, error) {
	if *f.calls++; *f.calls == 1 {
		return "", errors.New("blob store unavailable")
	}
	return f.Publisher.Put(r, declared)
}

// L4 (Astra on #259): a failed seal stops that attempt only. Two attempts are cut; the blob store fails the first
// one's seal (Run takes the attempts in key order). The second still settles; Run reports the failure; the first
// stays revoked (settling) and settles at the next run.
func TestAFailedSealStopsThatAttemptOnly(t *testing.T) {
	w := restarted(t, t.TempDir())
	first, second := w.midCall(t), w.midCall(t)
	if first.Attempt > second.Attempt {
		first, second = second, first
	}
	r, calls := w.recovery(), 0
	r.Blobs = failingOnce{w.blobs, &calls}
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "blob store unavailable") {
		t.Fatalf("Run reported %v; want the failed seal", err)
	}
	states := func() (types.LeaseState, types.LeaseState) {
		var a, b types.Lease
		w.view(func(tx *eventstore.Tx) error {
			var err, berr error
			a, _, _, err = lease.LeaseOf(tx, first.Lease)
			b, _, _, berr = lease.LeaseOf(tx, second.Lease)
			return errors.Join(err, berr)
		})
		return a.State, b.State
	}
	if a, b := states(); a != types.LeaseStateSettling || b != types.LeaseStateSettled {
		t.Fatalf("leases %s and %s; want the failed one settling, the other settled", a, b)
	}
	w.run()
	if a, b := states(); a != types.LeaseStateSettled || b != types.LeaseStateSettled {
		t.Fatalf("after the next run, leases %s and %s; want both settled", a, b)
	}
}

// L5 (Opus on #259): Next acts only on its lease's own attempt.
func TestNextActsOnlyOnItsLeasesAttempt(t *testing.T) {
	a := lease.LoseOnRestart(lease.Start("att_01m3250vm7f6ktw39dszqnydd1", "unt_01m3250vm7f6ktw39dszqnydd1", "lea_01m3250vm7f6ktw39dszqnydd1", base))
	l := types.Lease{ID: a.Lease, State: types.LeaseStateActive, Attempt: ptr(a.ID)}
	if got := recovery.Next(a, l); got != recovery.Revoke {
		t.Fatalf("Next on its lease: %q", got)
	}
	other := l
	other.ID = "lea_01m3250vm7f6ktw39dszqnydd2"
	moved := l
	moved.Attempt = ptr(types.AttemptID("att_01m3250vm7f6ktw39dszqnydd2"))
	for name, l := range map[string]types.Lease{"another lease": other, "another attempt on the lease": moved, "no attempt": {ID: a.Lease, State: types.LeaseStateActive}} {
		if got := recovery.Next(a, l); got != recovery.Done {
			t.Errorf("%s: Next is %q", name, got)
		}
	}
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
