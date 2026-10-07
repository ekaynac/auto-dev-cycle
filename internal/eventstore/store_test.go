// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

func TestAppendAndVerify(t *testing.T) {
	e := openEnv(t, stateDir(t))
	e.appendN(t, 5)
	head, err := e.s.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if head.Seq != 5 || !strings.HasPrefix(head.Hash, "sha256:") {
		t.Fatalf("head = %+v", head)
	}
	if _, err := e.s.Verify(context.Background(), &head); err != nil {
		t.Fatalf("verify against its own head: %v", err)
	}
}

func TestAppendOnlyAndImmutableMeta(t *testing.T) {
	e := openEnv(t, stateDir(t))
	e.appendN(t, 1)
	for _, q := range []string{`UPDATE events SET kind = 'x.y/v1'`, `DELETE FROM events`,
		`UPDATE meta SET value = 'x' WHERE key = 'genesis_hash'`, `DELETE FROM meta`} {
		if _, err := e.s.w.Exec(q); err == nil || !(strings.Contains(err.Error(), "append-only") || strings.Contains(err.Error(), "immutable")) {
			t.Errorf("%s: want a refusal, got %v", q, err)
		}
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	ctx := context.Background()
	drop := []string{`DROP TRIGGER events_append_only_update`, `DROP TRIGGER events_append_only_delete`,
		`DROP TRIGGER meta_immutable_update`}
	for _, c := range []struct {
		name string
		sql  func(e *env) string
		want string
	}{
		{"edited payload", func(*env) string { return `UPDATE events SET payload = '{"n":99}' WHERE seq = 2` }, "seq 2 was edited"},
		{"edited actor", func(*env) string { return `UPDATE events SET actor = 'act_other' WHERE seq = 3` }, "seq 3 was edited"},
		{"deleted middle row", func(*env) string { return `DELETE FROM events WHERE seq = 2` }, "gap or reorder at seq 3"},
		{"truncated tail", func(*env) string { return `DELETE FROM events WHERE seq >= 4` }, "truncated"},
		{"consistently rewritten tail", func(e *env) string {
			// Re-hash seq 4 so the chain itself is intact: only the trusted head catches it.
			var row Event
			var payload []byte
			if err := e.s.r.QueryRow(`SELECT seq, id, at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash FROM events WHERE seq = 4`).
				Scan(&row.Seq, &row.ID, &row.At, &row.Kind, &row.Subject, &row.Project, &row.Actor, &row.Command, &row.Causation, &row.SchemaSet, &payload, &row.PrevHash); err != nil {
				panic(err)
			}
			row.Payload = []byte(`{"n":42}`)
			h, err := rowHash(row)
			if err != nil {
				panic(err)
			}
			return fmt.Sprintf(`UPDATE events SET payload = '{"n":42}', hash = '%s' WHERE seq = 4`, h)
		}, "seq 4 differs from the trusted head"},
		{"edited genesis", func(*env) string { return `UPDATE meta SET value = '{"format":"x"}' WHERE key = 'genesis_record'` }, "genesis record does not match"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := openEnv(t, stateDir(t))
			e.appendN(t, 4)
			trusted, err := e.s.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, raw(t, e.dir), append(drop, c.sql(e))...)
			_, err = e.s.Verify(ctx, &trusted)
			if !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrChainBroken containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestOpenRefusesAnotherFormat(t *testing.T) {
	dir := stateDir(t)
	e := openEnv(t, dir)
	e.close()
	mustExec(t, raw(t, dir), `DROP TRIGGER meta_immutable_update`, `UPDATE meta SET value = 'adc.eventstore/v99' WHERE key = 'format'`)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, _ := blobstore.Open(dir)
	if _, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs}); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("want a format refusal, got %v", err)
	}
}

func TestAppendRefuses(t *testing.T) {
	e := openEnv(t, stateDir(t))
	ok := Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}
	other := "sha256:" + strings.Repeat("1", 64)
	cases := map[string]func(*Event){
		"unregistered kind":           func(ev *Event) { ev.Kind = "a.b/v1" },
		"bad id":                      func(ev *Event) { ev.ID = "evt_x" },
		"integer beyond 2^53":         func(ev *Event) { ev.Payload = []byte(`{"n": 9007199254740993}`) },
		"float":                       func(ev *Event) { ev.Payload = []byte(`{"n": 1.5}`) },
		"not JSON":                    func(ev *Event) { ev.Payload = []byte(`{`) },
		"bad project id":              func(ev *Event) { p := "proj-1"; ev.Project = &p },
		"unpublished blob":            func(ev *Event) { ev.Kind, ev.Payload = "test.typed/v1", typedPayload(other) },
		"bad actor":                   func(ev *Event) { ev.Actor = "not-an-actor" },
		"actor of another kind":       func(ev *Event) { ev.Actor = testSubject },
		"bad subject":                 func(ev *Event) { ev.Subject = "not-an-id" },
		"unregistered subject prefix": func(ev *Event) { ev.Subject = "zzz_01m3250vkwffc979nzjw0g2sf9" },
		"negative zero":               func(ev *Event) { ev.Payload = []byte(`{"n": -0}`) },
		"invalid UTF-8 kind":          func(ev *Event) { ev.Kind = "test.appended/v1\xff" },
		"no subject":                  func(ev *Event) { ev.Subject = "" },
		"bad command id":              func(ev *Event) { c := "cmd_x"; ev.Command = &c },
		"bad causation id":            func(ev *Event) { c := "evt_x"; ev.Causation = &c },
		"payload off schema":          func(ev *Event) { ev.Kind = "test.typed/v1"; ev.Payload = []byte(`{"id": 1}`) },
	}
	for name, mut := range cases {
		ev := ok
		mut(&ev)
		if _, err := e.appendRaw(t, ev); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if head, _ := e.s.Head(context.Background()); head.Seq != 0 {
		t.Fatalf("refused appends left events behind: seq %d", head.Seq)
	}
	// A published blob may be referenced.
	d, err := e.blobs.Put(strings.NewReader("evidence"), "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ok
	ev.Kind, ev.Payload = "test.typed/v1", typedPayload(d)
	if _, err := e.appendRaw(t, ev); err != nil {
		t.Fatalf("published blob reference refused: %v", err)
	}
}

func TestSecondLockRefused(t *testing.T) {
	dir := stateDir(t)
	e := openEnv(t, dir)
	if _, err := AcquireLock(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock: want ErrAlreadyRunning, got %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, LockFile))
	if err != nil || !strings.Contains(string(b), fmt.Sprintf("pid %d\n", os.Getpid())) || !strings.Contains(string(b), "process_start_ticks ") {
		t.Fatalf("lock record damaged: %q, %v", b, err)
	}
	e.close()
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again.Release()
}

func TestLockRefusesAnOpenStateDirAndSymlinks(t *testing.T) {
	open := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(open); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("group-readable state dir: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "s2")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, LockFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); err == nil {
		t.Fatal("a symlinked lock file was followed")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("the symlink target was truncated")
	}
}

func TestOpenNeedsTheLockAndABlobChecker(t *testing.T) {
	dir := stateDir(t)
	if _, err := Open(context.Background(), nil, Config{}); err == nil {
		t.Fatal("Open without the lock succeeded")
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := Open(context.Background(), lock, Config{Kinds: testKinds}); err == nil {
		t.Fatal("Open without a blob checker succeeded")
	}
}

// typedPayload is an instrument contract whose payload_schema is the given
// blob digest.
func typedPayload(digest string) []byte {
	return []byte(`{"name":"go-test","version":"1","digest":"sha256:` + strings.Repeat("a", 64) + `","subject_type":"tree",` +
		`"required_predicates":[{"name":"tests","evidence_required":[]}],"optional_predicates":[],"preconditions":[],` +
		`"payload_schema":"` + digest + `","artifact_classes":{}}`)
}

// Astra #25: a state path with URI syntax opens its own database, and the
// lock of one directory never guards another's database.
func TestStatePathsWithURISyntax(t *testing.T) {
	base := t.TempDir()
	var envs []*env
	for _, name := range []string{"state?a", "state?b", "st#ate", "st%41te"} {
		envs = append(envs, openEnv(t, filepath.Join(base, name)))
	}
	if _, err := envs[0].appendRaw(t, Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	for i, e := range envs {
		h, err := e.s.Head(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(0); i == 0 {
			want = 1
			if h.Seq != want {
				t.Errorf("%s: head seq %d, want %d", e.dir, h.Seq, want)
			}
		} else if h.Seq != want {
			t.Errorf("%s sees another directory's events (seq %d)", e.dir, h.Seq)
		}
		if _, err := os.Stat(filepath.Join(e.dir, DBFile)); err != nil {
			t.Errorf("%s: database not inside its directory: %v", e.dir, err)
		}
	}
}

// Astra #25: INSERT OR REPLACE cannot overwrite an event or a meta key.
func TestReplacingInsertsAreRefused(t *testing.T) {
	e := openEnv(t, stateDir(t))
	e.appendN(t, 1)
	db := raw(t, e.dir)
	var id string
	if err := db.QueryRow(`SELECT id FROM events WHERE seq = 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	other := newID(t, "evt")
	for _, q := range []string{
		`INSERT OR REPLACE INTO events SELECT 1, '` + other + `', at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash, hash FROM events WHERE seq = 1`,
		`INSERT OR REPLACE INTO events SELECT 2, id, at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash, hash FROM events WHERE seq = 1`,
		`REPLACE INTO meta(key, value) VALUES ('format', 'adc.eventstore/v99')`,
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('genesis_hash', 'x')`,
	} {
		if _, err := db.Exec(q); err == nil || !(strings.Contains(err.Error(), "append-only") || strings.Contains(err.Error(), "immutable")) {
			t.Errorf("%s: want a refusal, got %v", q, err)
		}
	}
	var got string
	if err := db.QueryRow(`SELECT id FROM events WHERE seq = 1`).Scan(&got); err != nil || got != id {
		t.Fatalf("event 1 changed: %q, %v", got, err)
	}
}

// Astra #25: an edited text column that is not valid UTF-8 breaks the chain;
// it never shares an encoding with the original.
func TestVerifyDetectsInvalidUTF8Edits(t *testing.T) {
	e := openEnv(t, stateDir(t))
	e.appendN(t, 2)
	trusted, err := e.s.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw(t, e.dir), `DROP TRIGGER events_append_only_update`, `UPDATE events SET subject = CAST(X'70726A5FFF' AS TEXT) WHERE seq = 1`)
	if _, err := e.s.Verify(context.Background(), &trusted); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("an invalid UTF-8 edit verified: %v", err)
	}
}

// Astra #25: a stored payload must be in its canonical form.
func TestVerifyRefusesNonCanonicalPayloads(t *testing.T) {
	e := openEnv(t, stateDir(t))
	if _, err := e.appendRaw(t, Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{"n": 0}`)}); err != nil {
		t.Fatal(err)
	}
	trusted, err := e.s.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw(t, e.dir), `DROP TRIGGER events_append_only_update`, `UPDATE events SET payload = '{ "n" : -0 }' WHERE seq = 1`)
	if _, err := e.s.Verify(context.Background(), &trusted); !errors.Is(err, ErrChainBroken) || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("a non-canonical payload verified: %v", err)
	}
}

// Astra #25: an empty store's head anchors its genesis.
func TestEmptyHeadAnchorsGenesis(t *testing.T) {
	dir := stateDir(t)
	e := openEnv(t, dir)
	trusted, err := e.s.Head(context.Background())
	if err != nil || trusted.Seq != 0 || !strings.HasPrefix(trusted.Hash, "sha256:") {
		t.Fatalf("empty head %+v, %v", trusted, err)
	}
	if _, err := e.s.Verify(context.Background(), &trusted); err != nil {
		t.Fatalf("verify against its own empty head: %v", err)
	}
	e.close()
	record := `{"created_at":"2026-10-04T00:00:00Z","format":"adc.eventstore/v1","store_id":"00"}`
	h, err := digest([]byte(record))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw(t, dir), `DROP TRIGGER meta_immutable_update`,
		`UPDATE meta SET value = '`+record+`' WHERE key = 'genesis_record'`,
		`UPDATE meta SET value = '`+h+`' WHERE key = 'genesis_hash'`)
	again := openEnv(t, dir)
	if _, err := again.s.Verify(context.Background(), &trusted); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a replaced genesis verified against the original empty head: %v", err)
	}
}

// Astra #25: a hardlinked lock file is refused and its target left intact.
func TestLockRefusesAHardlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(dir, LockFile)); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if _, err := AcquireLock(dir); err == nil || !strings.Contains(err.Error(), "one link") {
		t.Fatalf("a hardlinked lock file: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("the hardlink target was overwritten")
	}
}

// Astra #25: one store per lock, and the lock cannot be released under it.
func TestLockOwnsOneStore(t *testing.T) {
	e := openEnv(t, stateDir(t))
	if _, err := Open(context.Background(), e.lock, Config{Kinds: testKinds, Blobs: e.blobs}); err == nil {
		t.Fatal("a second store opened on the same lock")
	}
	if err := e.lock.Release(); err == nil {
		t.Fatal("the lock was released under an open store")
	}
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
	e.s = nil
	if err := e.lock.Release(); err != nil {
		t.Fatalf("release after close: %v", err)
	}
}

// Astra #25 round 2: meta has no rowid to replace a guarded row through, and
// verify accepts exactly the three meta keys.
func TestMetaHasNoRowid(t *testing.T) {
	e := openEnv(t, stateDir(t))
	e.appendN(t, 1)
	trusted, err := e.s.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db := raw(t, e.dir)
	for _, col := range []string{"rowid", "_rowid_", "oid"} {
		q := `INSERT OR REPLACE INTO meta(` + col + `, key, value) SELECT ` + col + `, 'moved_format', 'x' FROM meta WHERE key = 'format'`
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: a replacement through the row id ran", col)
		}
	}
	var format string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil || format != Format {
		t.Fatalf("format %q, %v", format, err)
	}
	mustExec(t, db, `INSERT INTO meta(key, value) VALUES ('extra', 'x')`)
	if _, err := e.s.Verify(context.Background(), &trusted); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("an extra meta key verified: %v", err)
	}
}

// Astra #25 round 2: a database whose meta table has a rowid is refused.
func TestOpenRefusesARowidMeta(t *testing.T) {
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustExec(t, raw(t, dir), `CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs}); err == nil || !strings.Contains(err.Error(), "WITHOUT ROWID") {
		t.Fatalf("a rowid meta table opened: %v", err)
	}
	if lock.owner != 0 {
		t.Fatal("a failed open left its claim on the lock")
	}
}

// Astra #25 round 2: a database shared with another state directory, by a
// symlink or a hardlink, is refused; so is a database replaced after Open.
func TestDatabaseAliasesAreRefused(t *testing.T) {
	a := openEnv(t, stateDir(t))
	a.appendN(t, 1)
	for name, link := range map[string]func(string, string) error{"symlink": os.Symlink, "hardlink": os.Link} {
		dir := stateDir(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := link(filepath.Join(a.dir, DBFile), filepath.Join(dir, DBFile)); err != nil {
			t.Fatal(err)
		}
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		blobs, err := blobstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs}); err == nil || !strings.Contains(err.Error(), "one link") {
			if err == nil {
				s.Close()
			}
			t.Errorf("%s: a database of another state directory: %v", name, err)
		}
		lock.Release()
		if name == "hardlink" {
			os.Remove(filepath.Join(dir, DBFile))
		}
	}
	other := filepath.Join(a.dir, "replacement")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(a.dir, DBFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.Head(context.Background()); err == nil || !strings.Contains(err.Error(), "no longer the database") {
		t.Fatalf("a replaced database was used: %v", err)
	}
	if _, err := a.appendRaw(t, Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}); err == nil {
		t.Fatal("appended to a replaced database")
	}
}

// Astra #25 round 2: a stale Close never frees the lock under a successor.
func TestStaleCloseKeepsTheSuccessorsClaim(t *testing.T) {
	e := openEnv(t, stateDir(t))
	first := e.s
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), e.lock, Config{Kinds: testKinds, Blobs: e.blobs})
	if err != nil {
		t.Fatal(err)
	}
	e.s = second
	if err := first.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if err := e.lock.Release(); err == nil {
		t.Fatal("a stale Close freed the lock under the open successor")
	}
	if _, err := first.Head(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed store answered: %v", err)
	}
}

// Astra #25 round 2: Close waits for a running transaction, and nothing
// runs after it.
func TestCloseWaitsForTransactions(t *testing.T) {
	e := openEnv(t, stateDir(t))
	ctx := context.Background()
	inside, release := make(chan struct{}), make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- e.s.inTx(ctx, func(tx *sql.Tx) error {
			close(inside)
			<-release
			_, err := e.s.append(ctx, tx, Event{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Actor: sysActor, Payload: []byte(`{}`)})
			return err
		})
	}()
	<-inside
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a transaction was running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-txDone; err != nil {
		t.Fatalf("the running transaction: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := e.s.inTx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("a transaction after Close: %v", err)
	}
	e.s = nil
}

// Astra #25 round 2: the caller's kinds cannot change after Open.
func TestKindsAreCopied(t *testing.T) {
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
	fields := []string{"payload_schema"}
	s, err := Open(context.Background(), lock, Config{Kinds: Kinds{"test.typed/v1": {Schema: "records/instrument-contract", BlobFields: fields}}, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fields[0] = "nothing"
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	unpublished := "sha256:" + strings.Repeat("b", 64)
	if _, err := e.appendRaw(t, Event{Kind: "test.typed/v1", Subject: testSubject, Payload: typedPayload(unpublished)}); err == nil {
		t.Fatal("changing the caller's BlobFields disabled publish-before-reference")
	}
}

// Astra #25 round 2: a dangling database symlink is refused before SQLite
// opens it, so nothing is created at its target.
func TestDanglingDatabaseSymlinkCreatesNothing(t *testing.T) {
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.Symlink(target, filepath.Join(dir, DBFile)); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs}); err == nil {
		t.Fatal("a dangling database symlink opened")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SQLite created the symlink's target: %v", err)
	}
}

// Astra #25 round 2: a stale claim never ends its successor's.
func TestStaleClaimIsIgnored(t *testing.T) {
	e := openEnv(t, stateDir(t))
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
	e.s = nil
	first, err := e.lock.claim()
	if err != nil {
		t.Fatal(err)
	}
	e.lock.unclaim(first)
	second, err := e.lock.claim()
	if err != nil {
		t.Fatal(err)
	}
	e.lock.unclaim(first)
	if err := e.lock.Release(); err == nil {
		t.Fatal("a stale claim freed the lock")
	}
	e.lock.unclaim(second)
}

// Astra #25 round 3: a read inside a transaction while Close waits is
// refused, never deadlocked, and Close then completes.
func TestNestedReadDuringCloseDoesNotDeadlock(t *testing.T) {
	e := openEnv(t, stateDir(t))
	ctx := context.Background()
	closed := make(chan error, 1)
	nested := make(chan error, 1)
	err := e.s.inTx(ctx, func(*sql.Tx) error {
		go func() { closed <- e.s.Close() }()
		for {
			e.s.mu.Lock()
			closing := e.s.closing
			e.s.mu.Unlock()
			if closing {
				break
			}
			time.Sleep(time.Millisecond)
		}
		go func() {
			_, err := e.s.Head(ctx)
			nested <- err
		}()
		select {
		case err := <-nested:
			if !errors.Is(err, ErrClosed) {
				return fmt.Errorf("a nested read during Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			return errors.New("a nested read during Close blocked")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not complete after the transaction")
	}
	e.s = nil
}

// Astra #25 round 3: a panicking callback's transaction is rolled back
// before the operation ends; the writer stays usable.
func TestPanickingTransactionIsRolledBack(t *testing.T) {
	e := openEnv(t, stateDir(t))
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not propagate")
			}
		}()
		_ = e.s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := e.s.append(ctx, tx, Event{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Actor: sysActor, Payload: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			panic("callback failure")
		})
	}()
	done := make(chan error, 1)
	go func() {
		_, err := e.appendRaw(t, Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the writer is still held by the panicked transaction")
	}
	if h, err := e.s.Head(ctx); err != nil || h.Seq != 1 {
		t.Fatalf("head %+v, %v: the panicked append must not be committed", h, err)
	}
}

// Config.Existing opens only an initialized store: with no database, or
// with an empty one, it is ErrNoStore and creates no file, schema or
// genesis; an existing store opens as usual.
func TestOpenExistingCreatesNothing(t *testing.T) {
	dir := stateDir(t)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	open := func() error {
		s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs, Existing: true})
		if err == nil {
			err = s.Close()
		}
		return err
	}
	file := filepath.Join(dir, DBFile)
	if err := open(); !errors.Is(err, ErrNoStore) {
		t.Fatalf("no database: %v", err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a database was created: %v", err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := open(); err == nil {
		t.Fatal("an empty database opened")
	}
	if st, err := os.Stat(file); err != nil || st.Size() != 0 {
		t.Fatalf("the empty database was written: %v", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// An SQLite database without the store's schema gets none.
	db := raw(t, dir)
	mustExec(t, db, `PRAGMA journal_mode=WAL`, `CREATE TABLE other(x)`)
	if err := open(); err == nil {
		t.Fatal("a database without the schema opened")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'events'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the schema was created: %d %v", n, err)
	}
	// The store's schema without its genesis gets no genesis.
	mustExec(t, db, ddl)
	if err := open(); !errors.Is(err, ErrNoStore) {
		t.Fatalf("a store without its genesis: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a genesis was created: %d %v", n, err)
	}
	db.Close()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(file + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	lock.Release()
	openEnv(t, dir).close()
	if lock, err = AcquireLock(dir); err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := open(); err != nil {
		t.Fatalf("an existing store: %v", err)
	}
}
