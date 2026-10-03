// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// sysActor is the system actor of the test events.
const sysActor = "act_01m3250v0aer9ss2097svmefj8"

// testKinds: a test event kind whose payload schema accepts any JSON
// (common/defs has no root constraints).
var testKinds = Kinds{"test.appended/v1": "common/defs", "test.typed/v1": "records/instrument-contract"}

type env struct {
	dir   string
	s     *Store
	lock  *Lock
	blobs *blobstore.Store
}

// openEnv acquires the lock and opens the blob store and the event store,
// as adcd does.
func openEnv(t *testing.T, dir string) *env {
	t.Helper()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	t.Cleanup(e.close)
	return e
}

func (e *env) close() {
	if e.s != nil {
		e.s.Close()
		e.s = nil
	}
	e.lock.Release()
}

// stateDir is a not-yet-existing private state directory; AcquireLock
// creates it 0700 (t.TempDir itself may be group-accessible).
func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

func newID(t *testing.T, prefix string) string {
	t.Helper()
	id, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// appendRaw appends ev (filling the id and actor) in its own transaction on
// the writer connection, as the writer will.
func (e *env) appendRaw(t *testing.T, ev Event) (Event, error) {
	t.Helper()
	if ev.ID == "" {
		ev.ID = newID(t, "evt")
	}
	if ev.Actor == "" {
		ev.Actor = sysActor
	}
	ctx := context.Background()
	tx, err := e.s.w.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	out, err := e.s.append(ctx, tx, ev)
	if err != nil {
		tx.Rollback()
		return Event{}, err
	}
	return out, tx.Commit()
}

func (e *env) appendN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ev := Event{Kind: "test.appended/v1", Subject: "prj_x", Payload: []byte(fmt.Sprintf(`{"n": %d, "b": "x", "a": [1,2]}`, i))}
		if _, err := e.appendRaw(t, ev); err != nil {
			t.Fatal(err)
		}
	}
}

// raw opens a second connection, as an attacker with write access would.
func raw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
