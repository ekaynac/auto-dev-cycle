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

// Recorded measurements, from two attempts, are listed per call (and only
// that call's), survive a rebuild and reconcile like any reports.
func TestRecordedMeasurements(t *testing.T) {
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
	const other = types.CallID("cal_01m3250vm7f6ktw39dszqnyddr")
	ms := []types.UsageMeasurement{m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 100), m("att_01m3250vnfeyrv67x6w0ddqr6b", 1, 50), m("att_01m3250vnfeyrv67x6w0ddqr6a", 0, 7)}
	ms[2].Call = other
	cmd, err := ids.New("cmd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, eventstore.Command{ID: cmd, Actor: "act_01m3250v0aer9ss2097svmefj8", IdempotencyKey: cmd, Input: []byte(`{}`)},
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
		}); err != nil {
		t.Fatal(err)
	}
	if diffs, err := s.RebuildProjections(ctx); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
	var got []types.UsageMeasurement
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		got, err = Measurements(tx, ms[0].Call)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d measurements of the call, want 2", len(got))
	}
	calls, err := Reconcile(got)
	if err != nil || calls[ms[0].Call].Usage.In != 150 {
		t.Fatalf("reconciled: %+v %v", calls, err)
	}
}

func m(att types.AttemptID, seq, in int64) types.UsageMeasurement {
	id, _ := ids.New("msr")
	return types.UsageMeasurement{ID: types.MeasurementID(id), Call: "cal_01m3250v9xft08ssr4ygq6g2np", Attempt: att, Lease: "lea_01m3250v57fhm9v1z9xd1wk8gw",
		Kind: types.UsageMeasurementKindDelta, Sequence: seq, Tokens: types.UsageMeasurementTokens{In: types.Amount(in)},
		Cost: types.UsageMeasurementCost{Currency: "USD", Basis: types.UsageMeasurementCostBasisCash}, SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
}
