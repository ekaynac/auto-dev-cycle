// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// Measured is the event kind of one UsageMeasurement, subject its call. It
// is never fenced: a stale attempt's usage is recorded like any other (05).
const (
	Measured        = "usage.measured/v1"
	MeasuredSchema  = "records/usage-measurement"
	UsageProjection = "usage_measurements"
	// MeasurementProjection holds each measurement under its id alone, so
	// a reused id is seen whatever call it names.
	MeasurementProjection = "usage_measurement_ids"
)

// Kinds registers Measured with the event store.
func Kinds() eventstore.Kinds {
	return eventstore.Kinds{Measured: {Schema: MeasuredSchema}}
}

// Measure builds the event recording m.
func Measure(m types.UsageMeasurement) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(m)
	return eventstore.Event{ID: id, Kind: Measured, Subject: string(m.Call), Payload: raw}, err
}

// Project is an event store Project hook: it keeps each measurement under
// its call, so Measurements can reconcile a call from the projections.
func Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != Measured {
			continue
		}
		var m types.UsageMeasurement
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return fmt.Errorf("ledger: event %s: %w", ev.ID, err)
		}
		if ev.Subject != string(m.Call) {
			return fmt.Errorf("ledger: event %s: subject %s is not its call %s", ev.ID, ev.Subject, m.Call)
		}
		// A bound call's report names the call's own attempt and lease
		// (#226 entry 61), stale or not, so its charge lands where the
		// binding put it.
		if b, bound, err := Binding(tx, m.Call); err != nil {
			return err
		} else if bound && (b.Attempt != m.Attempt || b.Lease != m.Lease) {
			return fmt.Errorf("ledger: event %s: call %s is bound to %s under %s, not %s under %s", ev.ID, m.Call, b.Attempt, b.Lease, m.Attempt, m.Lease)
		}
		// A measurement id is immutable: an identical replay changes
		// nothing; another report under a used id is refused, never
		// written over the first (which could lower usage).
		prev, seen, err := tx.Get(MeasurementProjection, string(m.ID))
		if err != nil {
			return err
		}
		if seen {
			var first types.UsageMeasurement
			if err := json.Unmarshal(prev, &first); err != nil {
				return fmt.Errorf("ledger: measurement %s: %w", m.ID, err)
			}
			if !reflect.DeepEqual(first, m) {
				return fmt.Errorf("ledger: event %s: measurement %s is already recorded with other figures", ev.ID, m.ID)
			}
			continue
		}
		if err := errors.Join(tx.Put(MeasurementProjection, string(m.ID), ev.Payload),
			tx.Put(UsageProjection, string(m.Call)+"/"+string(m.ID), ev.Payload)); err != nil {
			return err
		}
	}
	return nil
}

// The model_call binding's projections, kept by internal/modelcall, which
// this package cannot import (it imports the gateway, which imports the
// registry, whose tests import capacity). internal/capacity's liability
// tests bind calls through internal/modelcall, so they fail if these drift.
const (
	bindingProjection      = "model_calls"       // call → binding
	leaseBindingProjection = "model_call_leases" // lease/call → binding
)

// Binding is call's model_call binding (records/model-call), if it is bound.
func Binding(tx *eventstore.Tx, call types.CallID) (types.ModelCall, bool, error) {
	var m types.ModelCall
	raw, ok, err := tx.Get(bindingProjection, string(call))
	if err == nil && ok {
		err = json.Unmarshal(raw, &m)
	}
	return m, ok && err == nil, err
}

// BoundTo lists the calls bound to lease, sorted.
func BoundTo(tx *eventstore.Tx, lease types.LeaseID) ([]types.CallID, error) {
	keys, err := tx.Keys(leaseBindingProjection, string(lease)+"/")
	out := make([]types.CallID, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.CallID(strings.TrimPrefix(k, string(lease)+"/")))
	}
	slices.Sort(out)
	return out, err
}

// Measurements lists a call's recorded measurements, from every attempt.
func Measurements(tx *eventstore.Tx, call types.CallID) ([]types.UsageMeasurement, error) {
	keys, err := tx.Keys(UsageProjection, string(call)+"/")
	if err != nil {
		return nil, err
	}
	out := make([]types.UsageMeasurement, 0, len(keys))
	for _, k := range keys {
		raw, _, err := tx.Get(UsageProjection, k)
		if err != nil {
			return nil, err
		}
		var m types.UsageMeasurement
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("ledger: measurement %s: %w", k, err)
		}
		out = append(out, m)
	}
	return out, nil
}
