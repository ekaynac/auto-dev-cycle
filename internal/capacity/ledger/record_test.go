// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

type ledgerStore struct {
	t *testing.T
	s *eventstore.Store
}

func openLedger(t *testing.T) *ledgerStore {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: Project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	return &ledgerStore{t: t, s: s}
}

// record appends one event per measurement in one command.
func (l *ledgerStore) record(ms ...types.UsageMeasurement) error {
	l.t.Helper()
	cmd, err := ids.New("cmd")
	if err != nil {
		l.t.Fatal(err)
	}
	_, err = l.s.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", IdempotencyKey: cmd, Input: []byte(`{}`)},
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			var evs []eventstore.Event
			for _, x := range ms {
				ev, err := Measure(x)
				if err != nil {
					return nil, nil, err
				}
				evs = append(evs, ev)
			}
			return evs, json.RawMessage(`{}`), nil
		})
	return err
}

func (l *ledgerStore) reconciled(call types.CallID) (int, Call) {
	l.t.Helper()
	if diffs, err := l.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		l.t.Fatalf("rebuild: %v %v", diffs, err)
	}
	var got []types.UsageMeasurement
	if err := l.s.View(context.Background(), func(tx *eventstore.Tx) (err error) {
		got, err = Measurements(tx, call)
		return err
	}); err != nil {
		l.t.Fatal(err)
	}
	calls, err := Reconcile(got)
	if err != nil {
		l.t.Fatal(err)
	}
	return len(got), calls[call]
}

// Recorded measurements, from two attempts, are listed per call (and only
// that call's), survive a rebuild and reconcile like any reports.
func TestRecordedMeasurements(t *testing.T) {
	l := openLedger(t)
	other := m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 7)
	other.Call = calB
	if err := l.record(m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 100), m("att_01m3250vnfeyrv67x6w0ddqr6b", 1, 50), other); err != nil {
		t.Fatal(err)
	}
	if n, c := l.reconciled(calA); n != 2 || c.Usage.In != 150 {
		t.Fatalf("%d measurements, usage %+v; want 2 and 150", n, c.Usage)
	}
}

// Astra #107 finding 4: a measurement id is immutable. An identical replay
// changes nothing; a conflicting reuse (other figures, or another call) is
// refused, so a later, lower report never replaces the first. Distinct ids
// for one (call, sequence) are kept and reconciled: the larger is kept.
func TestMeasurementIDsAreImmutable(t *testing.T) {
	l := openLedger(t)
	first := m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 100)
	if err := l.record(first); err != nil {
		t.Fatal(err)
	}
	if err := l.record(first); err != nil {
		t.Fatalf("an identical replay: %v", err)
	}
	lower := first
	lower.Tokens.In = 50
	if err := l.record(lower); err == nil {
		t.Fatal("a reused id with lower figures: accepted")
	}
	moved := first
	moved.Call = calB
	if err := l.record(moved); err == nil {
		t.Fatal("a reused id under another call: accepted")
	}
	if n, c := l.reconciled(calA); n != 1 || c.Usage.In != 100 {
		t.Fatalf("%d measurements, usage %+v; want 1 and 100", n, c.Usage)
	}
	if err := l.record(m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 50)); err != nil {
		t.Fatal(err)
	}
	if n, c := l.reconciled(calA); n != 2 || c.Usage.In != 100 || !c.Inconsistent {
		t.Fatalf("%d measurements, %+v; want 2, the larger kept and flagged", n, c)
	}
}

func m(att types.AttemptID, seq, in int64) types.UsageMeasurement {
	id, _ := ids.New("msr")
	return types.UsageMeasurement{ID: types.MeasurementID(id), Call: calA, Attempt: att, Lease: "lea_01m3250v57fhm9v1z9xd1wk8gw",
		Kind: types.UsageMeasurementKindDelta, Sequence: seq, Tokens: types.UsageMeasurementTokens{In: types.Amount(in)},
		Cost: types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash}, SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
}
