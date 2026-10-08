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
	"github.com/ekaynac/auto-dev-cycle/internal/testguard"
)

// sysActor is the system actor of the test events.
const sysActor = "act_01m3250v0aer9ss2097svmefj8"

// testKinds: test.appended accepts any JSON (common/defs has no root
// constraints); test.typed is an instrument contract, whose payload_schema
// field holds a blob digest.
var testKinds = Kinds{
	"test.appended/v1": {Schema: "common/defs"},
	"test.typed/v1":    {Schema: "records/instrument-contract", BlobFields: []string{"payload_schema"}},
}

// testSubject is a project id used as the subject of test events.
const testSubject = "prj_01m3250vkwffc979nzjw0g2sf9"

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
	return testguard.StateDir(t)
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
	var out Event
	err := e.s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = e.s.append(ctx, tx, ev)
		return err
	})
	return out, err
}

func (e *env) appendN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ev := Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(fmt.Sprintf(`{"n": %d, "b": "x", "a": [1,2]}`, i))}
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
