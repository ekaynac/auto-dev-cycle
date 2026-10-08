// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A logic's emitted events are recorded ahead of the ones it returns, at
// the command's time and under its id; only an applied command records
// them, and only a command's transaction emits.
func TestEmit(t *testing.T) {
	e := openWriter(t, Hooks{Identify: allow, Authorize: allow})
	emitted := Event{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{"emitted": true}`)}
	out, err := e.s.SubmitApplied(context.Background(), command(t, "a", `{}`), Applies(func(tx *Tx) ([]Event, json.RawMessage, error) {
		evs, res, _ := appendTo(t, testSubject, 1)(tx)
		return evs, res, tx.Emit(emitted)
	}))
	if err != nil || out.FirstSeq != 1 || out.LastSeq != 2 {
		t.Fatalf("applied: %+v %v", out, err)
	}
	// Hooks do not emit: neither before the logic nor once it decided.
	var before, after error
	h := openWriter(t, Hooks{Identify: allow, Authorize: func(tx *Tx, _ Command) error { before = tx.Emit(emitted); return nil },
		Project: func(tx *Tx, _ []Event) error { after = tx.Emit(emitted); return nil }})
	if _, err := h.s.SubmitApplied(context.Background(), command(t, "h", `{}`), Applies(appendTo(t, testSubject, 1))); err != nil || before == nil || after == nil {
		t.Fatalf("a hook emitted: %v, before %v, after %v", err, before, after)
	}
	if err := e.s.View(context.Background(), func(tx *Tx) error {
		evs, err := tx.EventsAfter(0, 10)
		if err == nil && (len(evs) != 2 || evs[0].ID != emitted.ID || *evs[0].Command != out.CommandID) {
			t.Errorf("the log: %+v", evs)
		}
		if tx.Emit(emitted) == nil {
			t.Error("a view emitted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Submit(context.Background(), command(t, "b", `{}`), func(tx *Tx) (Decision, error) {
		return Decision{Disposition: DispositionRefused, Result: json.RawMessage(refusal)}, tx.Emit(emitted)
	}); err == nil || !strings.Contains(err.Error(), "emits no event") || head(t, e) != 2 {
		t.Fatalf("a refused command emitted: %v", err)
	}
}
