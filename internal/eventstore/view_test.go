// SPDX-License-Identifier: Apache-2.0

package eventstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// A view reads the live projections and the log in one snapshot, and
// changes nothing.
func TestView(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	ctx := context.Background()
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hooks := eventstore.Hooks{
		Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
		Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
			for _, e := range evs {
				if err := tx.Put("seen", e.ID, json.RawMessage(`{"n":1}`)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{
		Kinds: eventstore.Kinds{"test.appended/v1": {Schema: "common/defs"}}, Blobs: blobs, Hooks: hooks,
		Effects: eventstore.EffectKinds{"notification": {Idempotent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	actor, subject := id(t, "act"), id(t, "prj")
	submit := func(key string) string {
		ev := id(t, "evt")
		if _, err := s.Submit(ctx, eventstore.Command{ID: id(t, "cmd"), Actor: actor, IdempotencyKey: key, Input: []byte(`{}`)},
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return []eventstore.Event{{ID: ev, Kind: "test.appended/v1", Subject: subject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
			}); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	first := submit("one")
	err = s.View(ctx, func(tx *eventstore.Tx) error {
		if v, ok, err := tx.Get("seen", first); err != nil || !ok || string(v) != `{"n":1}` {
			t.Errorf("get: %s %v %v", v, ok, err)
		}
		if v, err := tx.SubjectVersion(subject); err != nil || v == 0 {
			t.Errorf("subject version %d, %v", v, err)
		}
		if !errors.Is(tx.Put("seen", "x", json.RawMessage(`{}`)), eventstore.ErrReadOnly) || !errors.Is(tx.Delete("seen", first), eventstore.ErrReadOnly) {
			t.Error("a view changed a projection")
		}
		if tx.IntendEffect(id(t, "eff"), "notification", "t", json.RawMessage(`{}`)) == nil { // a registered kind
			t.Error("a view intended an effect")
		}
		// A command committed meanwhile is not in this snapshot.
		submit("two")
		if keys, err := tx.Keys("seen", ""); err != nil || len(keys) != 1 {
			t.Errorf("inside the view: %v %v", keys, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys("seen", "")
		if len(keys) != 2 {
			t.Errorf("a later view: %v", keys)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The snapshot is View's: a command committed before fn's first read is
	// not in it either.
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		submit("three")
		keys, err := tx.Keys("seen", "")
		if len(keys) != 2 {
			t.Errorf("a commit before the first read is visible: %v", keys)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// fn's error is View's.
	boom := errors.New("boom")
	if err := s.View(ctx, func(*eventstore.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(*eventstore.Tx) error { return nil }); !errors.Is(err, eventstore.ErrClosed) {
		t.Errorf("a view on a closed store: %v", err)
	}
}
