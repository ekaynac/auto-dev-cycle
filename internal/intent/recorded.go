// SPDX-License-Identifier: Apache-2.0

package intent

// The builder's recorded manifest.intent (P22a-5c-2b-1; I18, I10
// consumption; W17): the controller records it in the launch command, after
// the attempt.started it binds, so no build attempt starts without one, and
// the fold checks it as it lands:
//   - a records/manifest-intent (the event's schema) whose refs are all
//     checked refs, each input named once, included or omitted
//     (knowledge.CheckIntent);
//   - for its own task and its own attempt, stated in the consumer's role
//     its attempt's node gives (a build's: builder), every ref it lists
//     walked by the I18 walk over the task's records (CheckRecorded): an
//     input derived from the acceptance, or a plan ref that is not the
//     task's own brief, is refused;
//   - one per attempt (RecordedProjection).
//
// A refused intent refuses its command: the launch starts nothing.

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// The recorded intent's event kind and projection.
const (
	Recorded           = "manifest.intent/v1"
	RecordedProjection = "intent_recorded" // attempt id → the intent it recorded
	recordSchema       = "records/manifest-intent"
)

// Kinds are the recorded intent's events.
func Kinds() eventstore.Kinds {
	return eventstore.Kinds{Recorded: {Schema: recordSchema}}
}

// Record is the event recording mi, its subject its manifest id.
func Record(mi types.ManifestIntent) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(mi)
	return eventstore.Event{ID: id, Kind: Recorded, Subject: string(mi.ID), Payload: raw}, err
}

// Projector folds the recorded intents, checking each against Blobs (the
// declassified outputs CheckRecorded's walk verifies).
type Projector struct{ Blobs receipt.Blobs }

// Project folds evs' recorded intents (the file's comment).
func (p Projector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind != Recorded {
			continue
		}
		var mi types.ManifestIntent
		if err := json.Unmarshal(ev.Payload, &mi); err != nil {
			return err
		}
		switch {
		case ev.Subject != string(mi.ID) || mi.Task == nil || mi.Attempt == nil:
			return fmt.Errorf("intent: a recorded intent is under its manifest id and names its task and attempt")
		case p.Blobs == nil:
			return fmt.Errorf("intent: no blobs to check a recorded intent")
		}
		if err := knowledge.CheckIntent(mi); err != nil {
			return err
		}
		if _, dup, err := tx.Get(RecordedProjection, string(*mi.Attempt)); err != nil || dup {
			return errors.Join(err, fmt.Errorf("intent: attempt %s already recorded its intent", *mi.Attempt))
		}
		if err := CheckRecorded(tx, p.Blobs, *mi.Task, mi); err != nil {
			return err
		}
		raw, err := json.Marshal(mi)
		if err == nil {
			err = tx.Put(RecordedProjection, string(*mi.Attempt), raw)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RecordedOf is the intent attempt recorded, if any.
func RecordedOf(tx *eventstore.Tx, attempt types.AttemptID) (types.ManifestIntent, bool, error) {
	raw, ok, err := tx.Get(RecordedProjection, string(attempt))
	var mi types.ManifestIntent
	if err == nil && ok {
		err = json.Unmarshal(raw, &mi)
	}
	return mi, ok, err
}
