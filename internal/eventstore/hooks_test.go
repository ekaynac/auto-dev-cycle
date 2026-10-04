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
// writer transaction: the authorizer reads a projection, the reservation and
// the projector write projections, and a later failure rolls all of it back.
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
	hooks := eventstore.Hooks{
		Authorize: func(tx *eventstore.Tx, c eventstore.Command) error {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS proj_units (name TEXT PRIMARY KEY, free INTEGER NOT NULL)`); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO proj_units VALUES ('gpu', 1)`); err != nil {
				return err
			}
			var free int
			if err := tx.QueryRow(`SELECT free FROM proj_units WHERE name = 'gpu'`).Scan(&free); err != nil {
				return err
			}
			if free < 1 {
				return errors.New("no capacity")
			}
			return nil
		},
		Reserve: func(tx *eventstore.Tx, c eventstore.Command) error {
			_, err := tx.Exec(`UPDATE proj_units SET free = free - 1 WHERE name = 'gpu'`)
			return err
		},
		Project: func(tx *eventstore.Tx, evs []eventstore.Event) error {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS proj_seen (seq INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			for _, e := range evs {
				if _, err := tx.Exec(`INSERT INTO proj_seen VALUES (?)`, e.Seq); err != nil {
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
	submit := func(key string) error {
		_, err := s.Submit(ctx, eventstore.Command{ID: id(t, "cmd"), Actor: actor, IdempotencyKey: key, Input: []byte(`{}`)},
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return []eventstore.Event{{ID: id(t, "evt"), Kind: "test.appended/v1", Subject: subject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
			})
		return err
	}
	// A failing projection rolls back the reservation and the projection.
	fail = true
	if err := submit("first"); err == nil {
		t.Fatal("the failing command applied")
	}
	fail = false
	// The unit is still free, so this one reserves it…
	if err := submit("second"); err != nil {
		t.Fatalf("the reservation was not rolled back: %v", err)
	}
	// …and the next one finds none, through the same transaction's reads.
	if err := submit("third"); err == nil {
		t.Fatal("a reservation beyond capacity applied")
	}
}
