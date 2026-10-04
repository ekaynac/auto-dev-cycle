// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

var testEffects = EffectKinds{"model_call": {Idempotent: false}, "notification": {Idempotent: true}}

// openEffectsAt opens (or reopens) a store with effects in dir.
func openEffectsAt(t *testing.T, dir string) *env {
	t.Helper()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs, Hooks: Hooks{Authorize: allow}, Effects: testEffects})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	t.Cleanup(e.close)
	return e
}

// intend submits a command that intends one effect of kind and returns its id.
func intend(t *testing.T, e *env, kind string) string {
	t.Helper()
	id := newID(t, "eff")
	_, err := e.s.Submit(context.Background(), command(t, "k-"+id, `{}`), func(tx *Tx) ([]Event, json.RawMessage, error) {
		return nil, json.RawMessage(`{}`), tx.IntendEffect(id, kind, "provider-a", json.RawMessage(`{"prompt": "x"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func state(t *testing.T, e *env, id string) EffectState {
	t.Helper()
	eff, err := e.s.LoadEffect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return eff.State
}

func TestIntentCommitsWithTheCommand(t *testing.T) {
	e := openEffectsAt(t, stateDir(t))
	ctx := context.Background()
	id := newID(t, "eff")
	c := command(t, "k", `{}`)
	apply := func(tx *Tx) ([]Event, json.RawMessage, error) {
		if err := tx.IntendEffect(id, "model_call", "provider-a", json.RawMessage(`{"prompt": "x"}`)); err != nil {
			return nil, nil, err
		}
		return []Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
	}
	out, err := e.s.Submit(ctx, c, apply)
	if err != nil {
		t.Fatal(err)
	}
	if out.FirstSeq != 1 || out.LastSeq != 2 || len(out.Effects) != 1 || out.Effects[0].State != EffectPending || out.Effects[0].Command != c.ID || out.Effects[0].Attempts != 1 {
		t.Fatalf("outcome %+v", out)
	}
	// A replay returns the effect's current state, so unfinished work stays visible.
	if _, err := e.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "observed", Observation: "200 OK"}); err != nil {
		t.Fatal(err)
	}
	again, err := e.s.Submit(ctx, command(t, "k", `{}`), apply)
	if err != nil || !again.Replayed || len(again.Effects) != 1 || again.Effects[0].State != EffectCompleted {
		t.Fatalf("replay %+v, %v", again, err)
	}
	if _, err := e.s.Verify(ctx, nil); err != nil {
		t.Fatalf("the log with effect events: %v", err)
	}
}

func TestIntendEffectRefusals(t *testing.T) {
	e := openEffectsAt(t, stateDir(t))
	cases := map[string]func(tx *Tx) error{
		"unregistered kind": func(tx *Tx) error { return tx.IntendEffect(newID(t, "eff"), "deploy", "target", json.RawMessage(`{}`)) },
		"bad id":            func(tx *Tx) error { return tx.IntendEffect("eff_x", "model_call", "target", json.RawMessage(`{}`)) },
		"no target":         func(tx *Tx) error { return tx.IntendEffect(newID(t, "eff"), "model_call", "", json.RawMessage(`{}`)) },
		"request not JSON": func(tx *Tx) error {
			return tx.IntendEffect(newID(t, "eff"), "model_call", "target", json.RawMessage(`{`))
		},
	}
	for name, intendBad := range cases {
		_, err := e.s.Submit(context.Background(), command(t, "k-"+name, `{}`), func(tx *Tx) ([]Event, json.RawMessage, error) {
			return nil, json.RawMessage(`{}`), intendBad(tx)
		})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if head(t, e) != 0 {
		t.Fatal("a refused intent left events")
	}
	if err := (&Tx{}).IntendEffect(newID(t, "eff"), "model_call", "t", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an effect intended outside a command")
	}
}

// The state machine of note 03, every state against every transition.
func TestEffectStateMachine(t *testing.T) {
	ctx := context.Background()
	e := openEffectsAt(t, stateDir(t))
	// reach puts a fresh effect of kind into state.
	reach := func(kind string, st EffectState) string {
		id := intend(t, e, kind)
		steps := map[EffectState][]Transition{
			EffectPending:         nil,
			EffectCompleted:       {{Kind: "observed"}},
			EffectFailedConfirmed: {{Kind: "failed", Observation: "connection refused before send"}},
			EffectUncertain:       {{Kind: "uncertain", Observation: "timeout"}},
		}
		for _, tr := range steps[st] {
			if _, err := e.s.RecordEffect(ctx, sysActor, id, tr); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	transitions := []Transition{
		{Kind: "observed"}, {Kind: "failed"}, {Kind: "uncertain"}, {Kind: "retried"},
		{Kind: "reconciled", Outcome: "completed"}, {Kind: "reconciled", Outcome: "failed_confirmed"}, {Kind: "reconciled", Outcome: "still_uncertain"},
		{Kind: "reconciled", Outcome: "maybe"}, {Kind: "vanished"},
	}
	want := map[EffectState]map[string]EffectState{
		EffectPending:   {"observed": EffectCompleted, "failed": EffectFailedConfirmed, "uncertain": EffectUncertain},
		EffectUncertain: {"reconciled/completed": EffectCompleted, "reconciled/failed_confirmed": EffectFailedConfirmed, "reconciled/still_uncertain": EffectUncertain, "retried": EffectPending},
	}
	for _, kind := range []string{"model_call", "notification"} {
		for _, from := range []EffectState{EffectPending, EffectCompleted, EffectFailedConfirmed, EffectUncertain} {
			for _, tr := range transitions {
				name := tr.Kind
				if tr.Outcome != "" {
					name += "/" + tr.Outcome
				}
				to, allowed := want[from][name]
				if name == "retried" && kind == "model_call" {
					allowed = false // never blindly retried: reconcile first
				}
				id := reach(kind, from)
				got, err := e.s.RecordEffect(ctx, sysActor, id, tr)
				switch {
				case allowed && err != nil:
					t.Errorf("%s %s --%s--> refused: %v", kind, from, name, err)
				case allowed && got.State != to:
					t.Errorf("%s %s --%s--> %s, want %s", kind, from, name, got.State, to)
				case !allowed && !errors.Is(err, ErrEffectTransition):
					t.Errorf("%s %s --%s--> %v, want a refused transition", kind, from, name, err)
				case !allowed && state(t, e, id) != from:
					t.Errorf("%s %s --%s--> a refused transition changed the state", kind, from, name)
				}
			}
		}
	}
}

func TestRetryCountsAttempts(t *testing.T) {
	ctx := context.Background()
	e := openEffectsAt(t, stateDir(t))
	id := intend(t, e, "notification")
	for _, tr := range []Transition{{Kind: "uncertain"}, {Kind: "retried"}, {Kind: "observed", Observation: "delivered"}} {
		if _, err := e.s.RecordEffect(ctx, sysActor, id, tr); err != nil {
			t.Fatal(err)
		}
	}
	eff, err := e.s.LoadEffect(ctx, id)
	if err != nil || eff.Attempts != 2 || eff.State != EffectCompleted || eff.LastObservation != "delivered" {
		t.Fatalf("%+v, %v", eff, err)
	}
}

func TestResultRefIsPublishedFirst(t *testing.T) {
	ctx := context.Background()
	e := openEffectsAt(t, stateDir(t))
	id := intend(t, e, "model_call")
	unpublished := "sha256:" + strings.Repeat("e", 64)
	if _, err := e.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "observed", ResultRef: &unpublished}); err == nil {
		t.Fatal("an unpublished result was referenced")
	}
	d, err := e.blobs.Put(strings.NewReader("result"), "sha256:f6a214f7a5fcda0c2cee9660b7fc29f5649e3c68aad48e20e950137c98913a68")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "observed", ResultRef: &d})
	if err != nil || got.ResultRef == nil || *got.ResultRef != d {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestEffectEventKindsAreReserved(t *testing.T) {
	dir := stateDir(t)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := Kinds{"effect.intent/v1": {Schema: "common/defs"}}
	if s, err := Open(context.Background(), lock, Config{Kinds: kinds, Blobs: blobs}); err == nil {
		s.Close()
		t.Fatal("a caller registered an effect event kind")
	}
	if s, err := Open(context.Background(), lock, Config{Blobs: blobs, Effects: EffectKinds{"Bad Kind": {}}}); err == nil {
		s.Close()
		t.Fatal("an invalid effect kind name")
	}
}

// The crash fixtures of note 03 (M0 defines the dispositions; M1 runs the
// reconciliation procedures). A crash is a close without the effect's
// observation; restart applies the rule that pending becomes uncertain.
func TestCrashFixtures(t *testing.T) {
	ctx := context.Background()
	t.Run("after intent commit", func(t *testing.T) {
		dir := stateDir(t)
		e := openEffectsAt(t, dir)
		id := intend(t, e, "model_call")
		e.close() // crash: the call may never have been sent
		again := openEffectsAt(t, dir)
		if n, err := again.s.MarkPendingUncertain(ctx, sysActor); err != nil || n != 1 {
			t.Fatalf("restart: %d, %v", n, err)
		}
		if state(t, again, id) != EffectUncertain {
			t.Fatal("a pending effect is not uncertain after restart")
		}
		// Reconciliation finds no request id at the provider: never sent.
		if got, err := again.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "reconciled", Outcome: "failed_confirmed", Observation: "no request id at provider"}); err != nil || got.State != EffectFailedConfirmed {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("after send, before observation", func(t *testing.T) {
		dir := stateDir(t)
		e := openEffectsAt(t, dir)
		id := intend(t, e, "model_call")
		e.close() // crash: sent, reply lost
		again := openEffectsAt(t, dir)
		if _, err := again.s.MarkPendingUncertain(ctx, sysActor); err != nil {
			t.Fatal(err)
		}
		// Non-idempotent: never retried blindly; reconciled from the provider.
		if _, err := again.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "retried"}); !errors.Is(err, ErrEffectTransition) {
			t.Fatalf("a non-idempotent effect was retried: %v", err)
		}
		if got, err := again.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "reconciled", Outcome: "completed", Observation: "usage found by request id"}); err != nil || got.State != EffectCompleted {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("after observation, before projection", func(t *testing.T) {
		dir := stateDir(t)
		e := openEffectsAt(t, dir)
		id := intend(t, e, "model_call")
		// The observation and the new state commit in one transaction, so no
		// crash can separate them.
		if _, err := e.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "observed", Observation: "200 OK"}); err != nil {
			t.Fatal(err)
		}
		e.close()
		again := openEffectsAt(t, dir)
		if n, err := again.s.MarkPendingUncertain(ctx, sysActor); err != nil || n != 0 {
			t.Fatalf("restart changed %d effect(s), %v", n, err)
		}
		if state(t, again, id) != EffectCompleted {
			t.Fatal("an observed effect is not completed after restart")
		}
	})
}
