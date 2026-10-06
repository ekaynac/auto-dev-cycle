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
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

func id(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A hook package (authority, capacity, projections) works through the
// writer transaction: the authorizer reads a projection, the projector
// writes projections (a reservation among them), and a failure rolls all of
// it back.
func TestHooksShareTheWriterTransaction(t *testing.T) {
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
	fail := false
	free := func(tx *eventstore.Tx) (int, error) {
		v, ok, err := tx.Get("units", "gpu")
		if err != nil || !ok {
			return 1, err // one unit until the first reservation
		}
		var u struct{ Free int }
		return u.Free, json.Unmarshal(v, &u)
	}
	hooks := eventstore.Hooks{
		Identify: func(*eventstore.Tx, eventstore.Command) error { return nil },
		Authorize: func(tx *eventstore.Tx, c eventstore.Command) error {
			n, err := free(tx)
			if err != nil {
				return err
			}
			if n < 1 {
				return errors.New("no capacity")
			}
			return nil
		},
		Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
			// The reservation is part of the projection (internal/capacity).
			n, err := free(tx)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(map[string]int{"free": n - 1})
			if err := tx.Put("units", "gpu", b); err != nil {
				return err
			}
			for _, e := range evs {
				if err := tx.Put("seen", e.ID, json.RawMessage(`{}`)); err != nil {
					return err
				}
			}
			if fail {
				return errors.New("projection failed")
			}
			return nil
		},
	}
	s, err := eventstore.Open(ctx, lock, eventstore.Config{
		Kinds: eventstore.Kinds{"test.appended/v1": {Schema: "common/defs"}}, Blobs: blobs, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := id(t, "act")
	subject := id(t, "prj")
	var lastEvent string
	var check func(*eventstore.Tx) error
	submit := func(key string) error {
		lastEvent = id(t, "evt")
		_, err := s.SubmitApplied(ctx, eventstore.Command{ID: id(t, "cmd"), Actor: actor, Action: "test.command", IdempotencyKey: key, Input: []byte(`{}`)}, eventstore.Applies(
			func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				if check != nil {
					if err := check(tx); err != nil {
						return nil, nil, err
					}
				}
				return []eventstore.Event{{ID: lastEvent, Kind: "test.appended/v1", Subject: subject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
			}))
		return err
	}
	// A failing projection rolls back the reservation and the projection.
	fail = true
	if err := submit("first"); err == nil {
		t.Fatal("the failing command applied")
	}
	failed := lastEvent
	fail = false
	// The next command sees neither: its apply runs before it projects.
	checked := false
	check = func(tx *eventstore.Tx) error {
		if _, ok, err := tx.Get("seen", failed); err != nil || ok {
			t.Errorf("the failed command's projection record survived the rollback (%v)", err)
		}
		checked = true
		return nil
	}
	// The unit is still free, so this one reserves it…
	if err := submit("second"); err != nil {
		t.Fatalf("the reservation was not rolled back: %v", err)
	}
	if !checked {
		t.Fatal("the projection rollback was not checked")
	}
	check = nil
	// …and the next one finds none, through the same transaction's reads.
	if err := submit("third"); err == nil {
		t.Fatal("a reservation beyond capacity applied")
	}
}
