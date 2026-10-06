// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"encoding/json"
	"fmt"

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
		if err := tx.Put(UsageProjection, string(m.Call)+"/"+string(m.ID), ev.Payload); err != nil {
			return err
		}
	}
	return nil
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
