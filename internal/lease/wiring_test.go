// SPDX-License-Identifier: Apache-2.0

package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

type world struct {
	t     *testing.T
	s     *eventstore.Store
	blobs *blobstore.Store
}

func openWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: Project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &world{t, s, blobs}
}

// do runs one command whose events fn returns.
func (w *world) do(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) error {
	w.t.Helper()
	cmd, err := ids.New("cmd")
	if err != nil {
		w.t.Fatal(err)
	}
	_, err = w.s.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", IdempotencyKey: cmd, Input: []byte(`{}`)},
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := fn(tx)
			return evs, json.RawMessage(`{}`), err
		})
	return err
}

func (w *world) must(fn func(tx *eventstore.Tx) ([]eventstore.Event, error)) {
	w.t.Helper()
	if err := w.do(fn); err != nil {
		w.t.Fatal(err)
	}
}

// record publishes a valid AttemptRecord for attempt a and returns its digest.
func (w *world) record(a types.Attempt) types.Digest {
	w.t.Helper()
	b, err := contract.Encode("records/attempt-record", types.AttemptRecord{Attempt: a.ID, Project: "prj_01m3250vkwffc979nzjw0g2sf9",
		Task: "tsk_01m3250v9xft08ssr4ygq6g2np", ContractRevision: "7", FencingToken: a.FencingToken, BaseTree: a.Base.Tree, FailingTests: []string{},
		Trail: types.Digest("sha256:" + strings.Repeat("b", 64)), EvidenceManifest: types.Digest("sha256:" + strings.Repeat("c", 64)),
		Seal: types.AttemptRecordSeal{Reason: "drain", At: "2026-10-05T12:00:00Z", SealedBy: "act_01m3250v0aer9ss2097svmefj8"}})
	if err != nil {
		w.t.Fatal(err)
	}
	d, err := w.blobs.Put(bytes.NewReader(b), "")
	if err != nil {
		w.t.Fatal(err)
	}
	return types.Digest(d)
}

func (w *world) lease(id types.LeaseID) (types.Lease, int64) {
	w.t.Helper()
	var l types.Lease
	var rev int64
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var ok bool
		var err error
		l, rev, ok, err = load[types.Lease](tx, LeaseProjection, string(id))
		if err == nil && !ok {
			err = errors.New("no lease")
		}
		return nil, err
	})
	return l, rev
}

const leaB = types.LeaseID("lea_01m3250vm7f6ktw39dszqnyddr")

func TestWiredLifecycle(t *testing.T) {
	w := openWorld(t)
	h := holds(t)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) }); err == nil {
		t.Error("the same lease requested twice")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Granted), false, h)
	})
	// Astra #53: commands expect a revision; a stale one is refused.
	err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Expired), false, h)
	})
	var stale *StaleRecord
	if !errors.As(err, &stale) {
		t.Fatalf("a stale expectation: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	// attempt.started changed the lease too: its revision moved, so a
	// command still expecting the granted lease is refused.
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 2, event(Expired), false, h)
	}); !errors.As(err, &stale) {
		t.Errorf("a pre-start expectation after activation: %v", err)
	}
	if l, rev := w.lease(leaID); l.State != types.LeaseStateActive || rev != 3 || *l.Attempt != attA {
		t.Fatalf("lease after start: %s rev %d", l.State, rev)
	}
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 3, attB, base) }); err == nil {
		t.Error("a second first attempt for the unit")
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, AttemptEvent{Kind: StopRequested, StopReason: types.AttemptStopReasonValueRevoke})
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) }); !errors.As(err, new(*Stale)) {
		t.Errorf("a revoked attempt's output: %v", err)
	}
	stopped := Start(attA, untID, leaID, base)
	checkpoint := w.record(stopped)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: checkpoint})
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 3, event(Revoked), false, h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 4, event(Settled), false, h)
	})

	// Supersession under a new lease: the next token, in one command.
	next := requested()
	next.ID = leaB
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, next) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaB, 1, event(Granted), false, h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, leaB, 2, attB, base)
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attA, 1) }); !errors.As(err, new(*Stale)) {
		t.Errorf("the superseded attempt's output: %v", err)
	}
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return nil, FenceOutput(tx, untID, attB, 2) })

	// The projections are exactly what the log derives (#50).
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v, %+v", err, diffs)
	}
}

// Raw events cannot write a record the state machines would not: the
// projector checks each one.
func TestRawEventsAreChecked(t *testing.T) {
	w := openWorld(t)
	h := holds(t)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	raw := func(kind, subject, transition string, l types.Lease) error {
		return w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
			e, err := newEvent(transition, subject, "", &l, nil)
			e.Kind = kind
			return []eventstore.Event{e}, err
		})
	}
	settled := requested()
	settled.State = types.LeaseStateSettled
	settled.Settlement = &types.LeaseSettlementValue{Calls: []types.CallID{}, Liabilities: []types.CallID{}}
	granted, _ := ApplyLease(requested(), event(Granted), false, h)
	for name, err := range map[string]error{
		"a jump to settled":      raw(Settled+"/v1", string(leaID), Settled, settled),
		"a kind against payload": raw(Expired+"/v1", string(leaID), Granted, granted),
		"another subject":        raw(Granted+"/v1", string(leaB), Granted, granted),
		"another unit": func() error {
			g := granted
			g.Unit = "unt_01m3250v57ft7aehkqwq0yrq3n"
			return raw(Granted+"/v1", string(leaID), Granted, g)
		}(),
		"an unrequested lease": func() error { g := granted; g.ID = leaB; return raw(Granted+"/v1", string(leaB), Granted, g) }(),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := raw(Granted+"/v1", string(leaID), Granted, granted); err != nil {
		t.Errorf("a legitimate grant: %v", err)
	}
}

// A unit's first attempt is minted once (token 1 never twice); a successor
// comes only from supersession under a lease for the same unit; a raw
// attempt.started cannot restart an existing attempt.
func TestAttemptMinting(t *testing.T) {
	w := openWorld(t)
	h := holds(t)
	second := requested()
	second.ID = leaB
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, requested()) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, second) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaID, 1, event(Granted), false, h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, leaB, 1, event(Granted), false, h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaID, 2, attA, base) })
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return StartAttempt(tx, leaB, 2, attB, base) }); err == nil {
		t.Error("a second token-1 attempt for the unit under another lease")
	}
	// Superseding under a lease for another unit.
	other := requested()
	other.ID, other.Unit = "lea_01m3250vnfeyrv67x6w0ddqr6a", "unt_01m3250v57ft7aehkqwq0yrq3n"
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) { return RequestLease(tx, other) })
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeLease(tx, other.ID, 1, event(Granted), false, h)
	})
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 1, AttemptEvent{Kind: Lost, LostReason: types.AttemptLostReasonValueStreamDead})
	})
	lost := Start(attA, untID, leaID, base)
	checkpoint := w.record(lost)
	w.must(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return ChangeAttempt(tx, attA, 2, AttemptEvent{Kind: Sealed, Checkpoint: checkpoint})
	})
	if err := w.do(func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return SupersedeAttempt(tx, attA, 3, other.ID, 2, attB, base)
	}); err == nil {
		t.Error("superseding under another unit's lease")
	}
	// A raw attempt.started for the existing attempt, with the lease.
	restarted := Start(attA, untID, leaB, base)
	l, _ := w.lease(leaB)
	active, err := ApplyLease(l, LeaseEvent{Kind: AttemptStarted, Attempt: &restarted.ID}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.do(func(*eventstore.Tx) ([]eventstore.Event, error) {
		e, err := newEvent(AttemptStarted, string(attA), "", &active, &restarted)
		return []eventstore.Event{e}, err
	}); err == nil {
		t.Error("a raw restart of an existing attempt")
	}
}
