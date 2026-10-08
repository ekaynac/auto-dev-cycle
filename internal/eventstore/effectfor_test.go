// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// within runs fn in a command's transaction.
func within(t *testing.T, e *env, fn func(tx *Tx) error) {
	t.Helper()
	_, err := e.s.SubmitApplied(context.Background(), command(t, "k-"+newID(t, "cmd"), `{}`), Applies(func(tx *Tx) ([]Event, json.RawMessage, error) {
		return nil, json.RawMessage(`{}`), fn(tx)
	}))
	if err != nil {
		t.Fatal(err)
	}
}

// EffectFor finds the effect intended for a request, by kind and the
// request's canonical digest: one the command intends, or one recorded; two
// recorded are ambiguous.
func TestEffectFor(t *testing.T) {
	e := openEffectsAt(t, stateDir(t))
	find := func(kind, request string) (eff Effect, ok bool, err error) {
		within(t, e, func(tx *Tx) error { eff, ok, err = tx.EffectFor(kind, json.RawMessage(request)); return nil })
		return eff, ok, err
	}
	if _, ok, err := find("notification", `{"prompt": "x"}`); ok || err != nil {
		t.Fatalf("an empty store found an effect (%v)", err)
	}
	id := intend(t, e, "notification")
	got, ok, err := find("notification", `{ "prompt":"x" }`) // the same canonical request
	if err != nil || !ok || got.ID != id || got.State != EffectPending {
		t.Fatalf("got %+v, %v, %v; want %s pending", got, ok, err, id)
	}
	if _, ok, err := find("model_call", `{"prompt": "x"}`); ok || err != nil {
		t.Fatalf("another kind found the effect (%v)", err)
	}
	if _, ok, err := find("notification", `{"prompt": "y"}`); ok || err != nil {
		t.Fatalf("another request found the effect (%v)", err)
	}

	// Within the command that intends it, by kind too.
	other := newID(t, "eff")
	within(t, e, func(tx *Tx) error {
		if err := tx.IntendEffect(other, "model_call", "provider-a", json.RawMessage(`{"q": 1}`)); err != nil {
			return err
		}
		if eff, ok, err := tx.EffectFor("model_call", json.RawMessage(`{"q": 1}`)); err != nil || !ok || eff.ID != other {
			t.Errorf("the command's own intent: %+v, %v, %v", eff, ok, err)
		}
		if _, ok, err := tx.EffectFor("notification", json.RawMessage(`{"q": 1}`)); ok || err != nil {
			t.Errorf("the command's own intent of another kind was found (%v)", err)
		}
		return nil
	})

	intend(t, e, "notification")
	if _, _, err := find("notification", `{"prompt": "x"}`); !errors.Is(err, ErrAmbiguousEffect) {
		t.Fatalf("two effects for one request: %v", err)
	}
	if _, _, err := find("notification", `{"prompt":`); err == nil {
		t.Fatal("a request that is not JSON was accepted")
	}
}

// LiveEffect finds the pending or uncertain effect of a kind on a target.
func TestLiveEffect(t *testing.T) {
	e := openEffectsAt(t, stateDir(t))
	live := func(kind, target string) (eff Effect, ok bool, err error) {
		within(t, e, func(tx *Tx) error { eff, ok, err = tx.LiveEffect(kind, target); return nil })
		return eff, ok, err
	}
	id := intend(t, e, "notification") // on provider-a
	if got, ok, err := live("notification", "provider-a"); err != nil || !ok || got.ID != id {
		t.Fatalf("a pending effect: %+v, %v, %v", got, ok, err)
	}
	for _, c := range [][2]string{{"notification", "provider-b"}, {"model_call", "provider-a"}} {
		if _, ok, err := live(c[0], c[1]); ok || err != nil {
			t.Fatalf("%v found the effect (%v)", c, err)
		}
	}
	if _, err := e.s.MarkPendingUncertain(context.Background(), newID(t, "act")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := live("notification", "provider-a"); !ok {
		t.Fatal("an uncertain effect is not live")
	}
	if _, err := e.s.RecordEffect(context.Background(), newID(t, "act"), id, Transition{Kind: "reconciled", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := live("notification", "provider-a"); ok || err != nil {
		t.Fatalf("a completed effect is live (%v)", err)
	}
	within(t, e, func(tx *Tx) error {
		if err := tx.IntendEffect(newID(t, "eff"), "notification", "provider-a", json.RawMessage(`{}`)); err != nil {
			return err
		}
		if _, ok, err := tx.LiveEffect("notification", "provider-a"); !ok || err != nil {
			t.Errorf("the command's own intent is not live (%v)", err)
		}
		if _, ok, err := tx.LiveEffect("model_call", "provider-a"); ok || err != nil {
			t.Errorf("the command's own intent of another kind is live (%v)", err)
		}
		return nil
	})
	intend(t, e, "notification")
	if _, _, err := live("notification", "provider-a"); !errors.Is(err, ErrAmbiguousEffect) {
		t.Fatalf("two live effects on one target: %v", err)
	}
	if k, ok := e.s.EffectKind("notification"); !ok || !k.Idempotent {
		t.Fatalf("EffectKind(notification) = %+v, %v", k, ok)
	}
	if _, ok := e.s.EffectKind("deploy"); ok {
		t.Fatal("an unregistered kind is registered")
	}
}
