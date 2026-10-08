// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

const leaB = types.LeaseID("lea_01m3250vm7f6ktw39dszqnyddr")

type world struct {
	t     *testing.T
	dir   string
	lock  *eventstore.Lock
	s     *eventstore.Store
	blobs *blobstore.Store
	h     map[string]types.Hold
	units Units
}

// testUnits binds every unit to the project and task the test records name.
type testUnits struct{}

func (testUnits) ProjectTask(*eventstore.Tx, types.UnitID) (types.ProjectID, types.TaskID, bool, error) {
	return "prj_01m3250vkwffc979nzjw0g2sf9", "tsk_01m3250v9xft08ssr4ygq6g2np", true, nil
}

func openWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: filepath.Join(t.TempDir(), "state"), h: holds(t), units: testUnits{}}
	w.open()
	return w
}

func (w *world) open() {
	w.t.Helper()
	lock, err := eventstore.AcquireLock(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	blobs, err := blobstore.Open(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: Projector{Blobs: blobs, Holds: w.h, Units: w.units}.Project}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.lock, w.s, w.blobs = lock, s, blobs
	w.t.Cleanup(w.close)
}

func (w *world) close() {
	if w.s != nil {
		w.s.Close()
		w.lock.Release()
		w.s = nil
	}
}

func (w *world) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	w.t.Helper()
	cmd, err := ids.New("cmd")
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
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

// raw appends one hand-made event, bypassing the helpers.
func (w *world) raw(subject string, p payload) error {
	w.t.Helper()
	return w.rawKind(p.Transition+"/v1", subject, p)
}

// rawKind is raw with an event kind of the caller's choosing.
func (w *world) rawKind(kind, subject string, p payload) error {
	w.t.Helper()
	return w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		e, err := newEvent(subject, p)
		e.Kind = kind
		return []eventstore.Event{e}, err
	})
}

func (w *world) put(b []byte) types.Digest {
	w.t.Helper()
	d, err := w.blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		w.t.Fatal(err)
	}
	return types.Digest(d)
}

// record publishes a valid AttemptRecord for a, with its named blobs.
func (w *world) record(a types.Attempt, edit func(*types.AttemptRecord)) types.Digest {
	w.t.Helper()
	r := types.AttemptRecord{Attempt: a.ID, Project: "prj_01m3250vkwffc979nzjw0g2sf9", Task: "tsk_01m3250v9xft08ssr4ygq6g2np",
		ContractRevision: a.Base.ContractRevision, FencingToken: a.FencingToken, BaseTree: a.Base.Tree, FailingTests: []string{},
		Trail: w.put([]byte("trail")), EvidenceManifest: w.put([]byte("manifest")),
		Seal: types.AttemptRecordSeal{Reason: "drain", At: "2026-10-05T12:00:00Z", SealedBy: "act_01m3250v0aer9ss2097svmefj8"}}
	if edit != nil {
		edit(&r)
	}
	b, err := contract.Encode("records/attempt-record", r)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.put(b)
}

func get[T any](w *world, projection, id string) (T, int64) {
	w.t.Helper()
	var rec T
	var rev int64
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var ok bool
		var err error
		rec, rev, ok, err = load[T](tx, projection, id)
		if err == nil && !ok {
			err = errors.New("no record")
		}
		return nil, err
	})
	return rec, rev
}

// granted requests and grants a lease (revision 2).
func (w *world) granted(l types.Lease) {
	w.t.Helper()
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, l) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, l.ID, 1, event(Granted), w.h)
	})
}

func isStale(err error) bool {
	var s *StaleRecord
	return errors.As(err, &s)
}

func ptrA(a types.AttemptID) *types.AttemptID { return &a }

func TestWiredLifecycle(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) }); err == nil {
		t.Error("the same lease requested twice")
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Expired), w.h)
	}); !isStale(err) {
		t.Errorf("a stale revision: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, event(Revoked), w.h)
	}); !isStale(err) {
		t.Errorf("the pre-start lease expectation after activation: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) }); !errors.As(err, new(*Stale)) {
		t.Errorf("a stopped attempt's output: %v", err)
	}
	stopped, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	cp := w.record(stopped, nil)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: cp})
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Revoked), w.h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 4, event(Settled), w.h)
	})
	next := requested()
	next.ID = leaB
	w.granted(next)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, leaB, 2, attB, base)
	})
	if a, _ := get[types.Attempt](w, AttemptProjection, string(attB)); a.FencingToken != 2 || *a.Supersedes != attA {
		t.Fatalf("the successor: %+v", a)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) }); !errors.As(err, new(*Stale)) {
		t.Errorf("the superseded attempt's output: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attB, 2) })
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v, %+v", err, diffs)
	}
}

// Astra #59, findings 1 and 3: starts and supersessions are minted only as
// the state machines would, bound to their lease and unit.
func TestMinting(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	second := requested()
	second.ID = leaB
	w.granted(second)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	l2, _ := get[types.Lease](w, LeaseProjection, string(leaB))
	activeB, err := ApplyLease(l2, LeaseEvent{Kind: AttemptStarted, Attempt: ptrA(attB)}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := Start(attB, untID, leaB, base)
	token2 := first
	token2.FencingToken = 2
	otherUnit := first
	otherUnit.Unit = "unt_01m3250v57ft7aehkqwq0yrq3n"
	otherLease := first
	otherLease.Lease = leaID
	mismatched := activeB
	mismatched.Attempt = ptrA(attA)
	for name, p := range map[string]payload{
		"a second token-1 attempt for the unit": {Transition: AttemptStarted, Lease: &activeB, Attempt: &first},
		"a start at token 2":                    {Transition: AttemptStarted, Lease: &activeB, Attempt: &token2},
		"a start for another unit":              {Transition: AttemptStarted, Lease: &activeB, Attempt: &otherUnit},
		"a start under another lease":           {Transition: AttemptStarted, Lease: &activeB, Attempt: &otherLease},
		"a lease naming another attempt":        {Transition: AttemptStarted, Lease: &mismatched, Attempt: &first},
	} {
		if err := w.raw(string(p.Attempt.ID), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaB, 2, attB, base) }); err == nil {
		t.Error("a second first attempt through the helper")
	}
	// Supersession: only from the unit's current sealed attempt, with the
	// next token, in one event.
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(Lost))
	})
	lost, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	cp := w.record(lost, nil)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: cp})
	})
	sealed, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	retired, next, err := Supersede(sealed, attB, leaB, base)
	if err != nil {
		t.Fatal(err)
	}
	skipped := next
	skipped.FencingToken = 5
	for name, p := range map[string]payload{
		"a skipped token":         {Transition: Superseded, Lease: &activeB, Attempt: &retired, Successor: &skipped},
		"a predecessor unchanged": {Transition: Superseded, Lease: &activeB, Attempt: &sealed, Successor: &next},
	} {
		if err := w.raw(string(attA), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := requested()
	other.ID, other.Unit = "lea_01m3250vnfeyrv67x6w0ddqr6a", "unt_01m3250v57ft7aehkqwq0yrq3n"
	w.granted(other)
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, other.ID, 2, attB, base)
	}); err == nil {
		t.Error("superseding under another unit's lease")
	}
	// A first attempt at token 2 for a unit with none, and a successor
	// reusing another unit's attempt id.
	attC := types.AttemptID("att_01m3250vm7f6ktw39dszqnyddr")
	o, _ := get[types.Lease](w, LeaseProjection, string(other.ID))
	activeO, err := ApplyLease(o, LeaseEvent{Kind: AttemptStarted, Attempt: ptrA(attC)}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstO := Start(attC, other.Unit, other.ID, base)
	firstO.FencingToken = 2
	if err := w.raw(string(attC), payload{Transition: AttemptStarted, Lease: &activeO, Attempt: &firstO}); err == nil {
		t.Error("a unit's first attempt at token 2: accepted")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, other.ID, 2, attC, base) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, leaB, 2, attC, base)
	}); err == nil {
		t.Error("a successor reusing another unit's attempt id")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, leaB, 2, attB, base)
	})
}

// Astra #59, finding 2: a lease leaves active only once its attempt stopped
// publishing, and completes only after its attempt completed.
func TestLeaseWaitsForItsAttempt(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	for _, k := range []string{Revoked, Preempted, Expired, Completed} {
		if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return ChangeLease(tx, leaID, 3, event(k), w.h) }); err == nil {
			t.Errorf("%s while the attempt publishes", k)
		}
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Completed), w.h)
	}); err == nil {
		t.Error("completed before the attempt completed")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Preempted), w.h)
	})
}

// Astra #59, findings 4, 5 and 8: every carried record must be exactly the
// state machine's; restore and publication are refused until wired.
func TestRawRecordsAreDerived(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	l, _ := get[types.Lease](w, LeaseProjection, string(leaID))
	expired, err := ApplyLease(l, event(Expired), false, w.h)
	if err != nil {
		t.Fatal(err)
	}
	moreRes := expired
	moreRes.Reservations = append(append([]types.LeaseReservationsItem{}, moreRes.Reservations...),
		types.LeaseReservationsItem{Group: "rg_01m3250vnfeyrv67x6w0ddqr6a", Amount: 9, Unit: types.CapacityUnitTokens})
	settledFake := l
	settledFake.State = types.LeaseStateSettled
	settledFake.Settlement = &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}
	for name, p := range map[string]payload{
		"an expiry rewriting reservations": {Transition: Expired, Lease: &moreRes},
		"a jump to settled":                {Transition: Settled, Lease: &settledFake},
		"a kind against its payload":       {Transition: Granted, Lease: &expired},
	} {
		if err := w.raw(string(leaID), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := w.rawKind(Granted+"/v1", string(leaID), payload{Transition: Expired, Lease: &expired}); err == nil {
		t.Error("an event of one kind carrying another transition: accepted")
	}
	if err := w.raw(string(leaB), payload{Transition: Expired, Lease: &expired}); err == nil {
		t.Error("an event about another subject: accepted")
	}
	if err := w.raw(string(leaID), payload{Transition: Expired, Lease: &expired}); err != nil {
		t.Errorf("a derived expiry: %v", err)
	}
	// A first request must be the fresh, requested lease.
	newGranted := requested()
	newGranted.ID, newGranted.State = leaB, types.LeaseStateGranted
	if err := w.raw(string(leaB), payload{Transition: Requested, Lease: &newGranted}); err == nil {
		t.Error("a first request carrying a granted lease: accepted")
	}
	// An unregistered hold, and a re-request rewriting the lease.
	w2 := openWorld(t)
	w2.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	coffee := requested()
	coffee.State = types.LeaseStateWaiting
	coffee.Hold = &types.LeaseHoldValue{Hold: "coffee_break", SignalInstance: "x"}
	if err := w2.raw(string(leaID), payload{Transition: Waiting, Lease: &coffee}); err == nil {
		t.Error("an unregistered hold: accepted")
	}
	w2.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Waiting), w2.h)
	})
	rewritten := requested()
	rewritten.Priority = 999
	if err := w2.raw(string(leaID), payload{Transition: Requested, Lease: &rewritten}); err == nil {
		t.Error("a re-request rewriting the lease: accepted")
	}
	if err := w2.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, rewritten) }); err == nil {
		t.Error("RequestLease over an existing lease")
	}
	// Astra #59 round 2: an unchanged waiting lease is not re-requested
	// without its revision either.
	if err := w2.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) }); err == nil {
		t.Error("RequestLease re-requesting an unchanged waiting lease")
	}
	w2.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, event(Requested), w2.h)
	})
	// Restore transitions and publication wait for their wiring.
	w3 := openWorld(t)
	w3.granted(requested())
	w3.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	w3.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	w3.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Revoked), w3.h)
	})
	settling, _ := get[types.Lease](w3, LeaseProjection, string(leaID))
	restoring, err := ApplyLease(settling, event(RestoreStarted), true, w3.h)
	if err != nil {
		t.Fatal(err)
	}
	if err := w3.raw(string(leaID), payload{Transition: RestoreStarted, Lease: &restoring}); err == nil {
		t.Error("a restore before the evidence-target wiring: accepted")
	}
	published := settling
	published.State = types.LeaseStateSettled
	res := types.ResultID("res_01m3250vm7f6ktw39dszqnyddr")
	rcp := types.ReceiptID("rcp_01m3250v57fhm9v1z9xd1wk8gw")
	published.RestoreReceipt = &rcp
	published.Settlement = &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}, Published: &res}
	if err := w3.raw(string(leaID), payload{Transition: Settled, Lease: &published}); err == nil {
		t.Error("a settlement with an invented receipt and publication: accepted")
	}
}

// Astra #59, findings 6 and 7: a result submission waits for its companion
// record; a seal names a valid AttemptRecord of its attempt.
func TestSubmissionsAndSeals(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(ResultSubmitted))
	}); err == nil {
		t.Error("a result submission without its result")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	a, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	seal := func(d types.Digest) error {
		return w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: d})
		})
	}
	unpublished := types.Digest("sha256:" + strings.Repeat("e", 64))
	for name, d := range map[string]types.Digest{
		"bytes that are not a record":    w.put([]byte("not json")),
		"another attempt's record":       w.record(a, func(r *types.AttemptRecord) { r.Attempt = attB }),
		"another token's record":         w.record(a, func(r *types.AttemptRecord) { r.FencingToken = 2 }),
		"another base's record":          w.record(a, func(r *types.AttemptRecord) { r.ContractRevision = "8" }),
		"a record naming a missing blob": w.record(a, func(r *types.AttemptRecord) { r.PartialDiff = &unpublished }),
	} {
		if err := seal(d); err == nil {
			t.Errorf("a seal with %s: accepted", name)
		}
	}
	// Astra #59 round 2: the record binds the unit's project and task.
	for name, d := range map[string]types.Digest{
		"another project's record": w.record(a, func(r *types.AttemptRecord) { r.Project = "prj_01m3250v57fhm9v1z9xd1wk8gw" }),
		"another task's record":    w.record(a, func(r *types.AttemptRecord) { r.Task = "tsk_01m3250vm7f6ktw39dszqnyddr" }),
	} {
		if err := seal(d); err == nil {
			t.Errorf("a seal with %s: accepted", name)
		}
	}
	if err := seal(w.record(a, nil)); err != nil {
		t.Fatalf("a seal with its record: %v", err)
	}
}

// Without the unit binding, no seal is admitted.
func TestSealNeedsUnits(t *testing.T) {
	w := &world{t: t, dir: filepath.Join(t.TempDir(), "state"), h: holds(t)}
	w.open()
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	a, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	cp := w.record(a, nil)
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: cp})
	}); err == nil {
		t.Error("a seal without the unit binding: accepted")
	}
}

// Astra #59 round 2: a lease reserving slots may owe a restore, so it does
// not settle while that is unknown.
func TestSlotLeasesDoNotSettleYet(t *testing.T) {
	w := openWorld(t)
	slots := requested()
	slots.Reservations = []types.LeaseReservationsItem{{Group: "rg_01m3250vkwffc979nzjw0g2sf9", Amount: 1, Unit: types.CapacityUnitSlots}}
	w.granted(slots)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Revoked), w.h)
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 4, event(Settled), w.h)
	}); err == nil {
		t.Error("a slot lease settled while its restore obligation is unknown")
	}
}

// Astra #59 round 2: replay folds the log alone. Purging a seal's named blob
// or retiring a hold never makes recorded history unrebuildable.
func TestReplayNeedsOnlyTheLog(t *testing.T) {
	w := openWorld(t)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Waiting), w.h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, event(Requested), w.h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Granted), w.h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 4, attA, base) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, attemptEvent(StopRequested))
	})
	a, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	trail := w.put([]byte("a trail to purge"))
	cp := w.record(a, func(r *types.AttemptRecord) { r.Trail = trail })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: cp})
	})
	hex := strings.TrimPrefix(string(trail), "sha256:")
	if err := os.Remove(filepath.Join(w.dir, "blobs", "sha256", hex[:2], hex[2:4], hex)); err != nil {
		t.Fatal(err)
	}
	w.close()
	w.h = map[string]types.Hold{} // the hold is retired
	w.open()
	if diffs, err := w.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild after a purge and a retired hold: %v, %+v", err, diffs)
	}
}

// Astra #59, finding 9: after a restart, a running attempt is lost before
// anything else runs, and its output is fenced out.
func TestRecoverAfterRestart(t *testing.T) {
	w := openWorld(t)
	w.granted(requested())
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	w.close()
	w.open()
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RecoverAfterRestart(tx) })
	a, _ := get[types.Attempt](w, AttemptProjection, string(attA))
	if a.Status != types.AttemptStatusLost || *a.LostReason != types.AttemptLostReasonValueControllerRestart {
		t.Fatalf("after recovery: %s", a.Status)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) }); !errors.As(err, new(*Stale)) {
		t.Errorf("a pre-restart attempt's output: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := RecoverAfterRestart(tx)
		if err == nil && len(evs) != 0 {
			err = errors.New("recovered twice")
		}
		return evs, err
	})
}

// A grant's targets are its own reserved groups, each once; the projector
// takes no other, whether or not capacity is wired.
func TestGrantTargetsAreReservedGroups(t *testing.T) {
	own := requested().Reservations[0].Group
	for name, targets := range map[string][]types.ResourceGroupID{
		"a group it does not reserve": {"rg_01m3250vnfeyrv67x6w0ddqr6a"},
		"its group twice":             {own, own},
	} {
		w := openWorld(t)
		w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
		if w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			return ChangeLease(tx, leaID, 1, LeaseEvent{Kind: Granted, Targets: targets}, w.h)
		}) == nil {
			t.Errorf("%s: granted", name)
		}
	}
	w := openWorld(t)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, LeaseEvent{Kind: Granted, Targets: []types.ResourceGroupID{own}}, w.h)
	})
}
