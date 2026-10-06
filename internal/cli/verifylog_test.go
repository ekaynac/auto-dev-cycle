// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/entry"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// noBlobs: the fixture's events reference no blob.
type noBlobs struct{}

func (noBlobs) Has(string) (bool, error) { return false, nil }

func newID(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// fixtureStore makes a state directory with n events and returns it and the
// head.
func fixtureStore(t *testing.T, n int) (string, eventstore.Head) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{
		Kinds: eventstore.Kinds{"test.appended/v1": {Schema: "common/defs"}}, Blobs: noBlobs{},
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range n {
		_, err := s.SubmitApplied(context.Background(), eventstore.Command{ID: newID(t, "cmd"), Actor: newID(t, "act"), Action: "test.command", IdempotencyKey: fmt.Sprint(i), Input: []byte(`{}`)}, eventstore.Applies(
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return []eventstore.Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: newID(t, "prj"), Payload: []byte(`{"n":1}`)}}, json.RawMessage(`{}`), nil
			}))
		if err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return dir, h
}

// tamper runs statements on the database behind the controller's back.
func tamper(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, eventstore.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// tamperQuietly edits around an append-only trigger and restores it, so the
// schema is the format's again: only the chain can tell.
func tamperQuietly(t *testing.T, dir, trigger, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, eventstore.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	var ddl string
	err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, trigger).Scan(&ddl)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	tamper(t, dir, "DROP TRIGGER "+trigger, stmt, ddl)
}

// flipIndexByte checkpoints the store into its main file, then changes the
// last byte of index's root page (a leaf in a small store: the end of its
// last entry, the row it points at), behind SQLite's back.
func flipIndexByte(t *testing.T, dir, index string) {
	t.Helper()
	tamper(t, dir, "PRAGMA wal_checkpoint(TRUNCATE)")
	db, err := sql.Open("sqlite", filepath.Join(dir, eventstore.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	var root, size int64
	err = db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = ?`, index).Scan(&root)
	if err == nil {
		err = db.QueryRow(`PRAGMA page_size`).Scan(&size)
	}
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, eventstore.DBFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	at := root*size - 1
	if _, err := f.ReadAt(b, at); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
}

func verifyCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), append([]string{"dev", "verify-log"}, args...), &out, &errb, NoTransport{})
	return code, out.String(), errb.String()
}

// verifyJSON runs with --json and requires exactly one result document on
// stdout.
func verifyJSON(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	code, out, errs := verifyCmd(append(args, "--json")...)
	var doc struct{ Outcome, Reason, Detail string }
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&doc); err != nil || dec.More() || doc.Outcome == "" || doc.Reason == "" {
		t.Fatalf("%v: stdout is not one result document: %q (stderr %q)", args, out, errs)
	}
	return code, doc.Outcome + "/" + doc.Reason, doc.Detail
}

// fingerprint is every file in the state directory with its content,
// SQLite's -shm index included: verification reads a private copy, so even
// a reader's marks in the index would show that it opened the store.
func fingerprint(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = string(b)
	}
	return m
}

func TestVerifyLog(t *testing.T) {
	dir, head := fixtureStore(t, 3)
	// Verification's private copies go here, and must be gone afterwards.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Cleanup(func() {
		if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
			t.Errorf("verification left %d temporary entries (%v)", len(left), err)
		}
	})
	trusted := fmt.Sprintf("%d:%s", head.Seq, head.Hash)
	before := fingerprint(t, dir)
	code, out, errs := verifyCmd("--state", dir, "--trusted", trusted, "--json")
	var doc struct {
		Outcome, Reason, Detail string
		Head                    *eventstore.Head
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != entry.ExitOK || doc.Outcome != "empty" || doc.Head == nil || *doc.Head != head {
		t.Fatalf("intact: %d %q %q", code, out, errs)
	}
	if code, out, _ := verifyCmd("--state", dir); code != entry.ExitOK || !strings.Contains(out, head.Hash) {
		t.Fatalf("intact, text: %d %q", code, out)
	}
	if after := fingerprint(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("verification changed the state directory (the store or the lock record)")
	}
	// Truncation, caught against the trusted head (and not without it).
	tamperQuietly(t, dir, "events_append_only_delete", fmt.Sprintf(`DELETE FROM events WHERE seq = %d`, head.Seq))
	if code, res, _ := verifyJSON(t, "--state", dir, "--trusted", trusted); code != entry.ExitRefused || res != "failed/log_integrity_broken" {
		t.Fatalf("truncated: %d %s", code, res)
	}
	if code, _, _ := verifyCmd("--state", dir); code != entry.ExitOK {
		t.Fatalf("a consistent prefix with no trusted head: %d", code)
	}
	// An edit, caught without a trusted head.
	tamperQuietly(t, dir, "events_append_only_update", `UPDATE events SET payload = '{"n":2}' WHERE seq = 1`)
	if code, out, _ := verifyCmd("--state", dir); code != entry.ExitRefused || !strings.Contains(out, "failed/log_integrity_broken") {
		t.Fatalf("edited: %d %q", code, out)
	}
}

// A database that is not a complete store of the format fails, and the
// check leaves it exactly as it found it: verification never creates,
// migrates or repairs.
func TestVerifyLogNeverRepairs(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string){
		"a dropped trigger": func(t *testing.T, dir string) { tamper(t, dir, "DROP TRIGGER events_append_only_delete") },
		"a rewritten trigger": func(t *testing.T, dir string) {
			tamper(t, dir, "DROP TRIGGER events_append_only_update", "CREATE TRIGGER events_append_only_update BEFORE UPDATE ON events BEGIN SELECT 1; END")
		},
		"an extra trigger": func(t *testing.T, dir string) {
			tamper(t, dir, "CREATE TRIGGER extra AFTER INSERT ON events BEGIN SELECT 1; END")
		},
		"a dropped automatic index": func(t *testing.T, dir string) {
			tamper(t, dir, "PRAGMA writable_schema=ON", "DELETE FROM sqlite_master WHERE name = 'sqlite_autoindex_events_1'")
		},
		"an index on another index's pages": func(t *testing.T, dir string) {
			// The schema text is the format's; SQLite itself finds the schema
			// malformed (SQLITE_CORRUPT), which is a verdict, not a failure
			// to run.
			tamper(t, dir, "PRAGMA writable_schema=ON", "UPDATE sqlite_master SET rootpage = (SELECT rootpage FROM sqlite_master WHERE name = 'events_subject') WHERE name = 'events_command'")
		},
		"an index entry that disagrees with its row": func(t *testing.T, dir string) {
			// Rows, hashes and schema are intact; only SQLite's integrity
			// check sees the index name another row.
			flipIndexByte(t, dir, "events_subject")
		},
		"no meta table": func(t *testing.T, dir string) {
			tamper(t, dir, "DROP TRIGGER meta_immutable_update", "DROP TRIGGER meta_immutable_delete", "DROP TRIGGER meta_no_replace", "DROP TABLE meta")
		},
		"an empty file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, eventstore.DBFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"not a database": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, eventstore.DBFile), []byte("not a database, but long enough to be read as a header"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"an unrelated database": func(t *testing.T, dir string) {
			// Another (rollback-journal) database moved in beside the
			// fixture's write-ahead log.
			other := t.TempDir()
			tamper(t, other, "CREATE TABLE notes (body TEXT)", "INSERT INTO notes VALUES ('keep')")
			if err := os.Rename(filepath.Join(other, eventstore.DBFile), filepath.Join(dir, eventstore.DBFile)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := fixtureStore(t, 2)
			setup(t, dir)
			before := fingerprint(t, dir)
			// SQLite drops a write-ahead log it takes as stale: where the
			// setup left the fixture's log (no tamper connection checkpointed
			// it), it must survive the check.
			if _, ok := before[eventstore.DBFile+"-wal"]; !ok && (name == "an empty file" || name == "not a database" || name == "an unrelated database") {
				t.Fatal("the fixture left no write-ahead log to protect")
			}
			if code, res, detail := verifyJSON(t, "--state", dir); code != entry.ExitRefused || res != "failed/log_integrity_broken" {
				t.Fatalf("%d %s %q", code, res, detail)
			}
			if after := fingerprint(t, dir); !reflect.DeepEqual(after, before) {
				t.Fatal("verification changed the state directory")
			}
		})
	}
}

// A store past the copy bound is not checked: a database grown sparse to
// 2 TiB (where SQLite's page count wraps and it would drop the log as stale)
// is refused before anything is copied, and the store's log is kept.
func TestVerifyLogRefusesAnOversizedStore(t *testing.T) {
	dir, _ := fixtureStore(t, 2)
	file := filepath.Join(dir, eventstore.DBFile)
	wal, err := os.ReadFile(file + "-wal")
	if err != nil || len(wal) == 0 {
		t.Fatalf("the fixture left no write-ahead log to protect: %v", err)
	}
	const size = 2 << 40
	if err := os.Truncate(file, size); err != nil {
		t.Skipf("no sparse file of %d bytes here: %v", size, err)
	}
	if code, res, detail := verifyJSON(t, "--state", dir); code != entry.ExitCouldNotRun || res != "could_not_run/store_unreadable" || !strings.Contains(detail, "more than") {
		t.Fatalf("%d %s %q", code, res, detail)
	}
	if b, err := os.ReadFile(file + "-wal"); err != nil || !bytes.Equal(b, wal) {
		t.Fatalf("the write-ahead log changed: %v", err)
	}
	if st, err := os.Stat(file); err != nil || st.Size() != size {
		t.Fatalf("the database changed: %v", err)
	}
}

// A state directory nobody may write still verifies, unchanged: nothing in
// it is opened for writing, created or removed.
func TestVerifyLogReadOnlyState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir, head := fixtureStore(t, 2)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(dir, e.Name()), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	before := fingerprint(t, dir)
	code, out, errs := verifyCmd("--state", dir, "--trusted", fmt.Sprintf("%d:%s", head.Seq, head.Hash))
	if code != entry.ExitOK {
		t.Fatalf("a read-only state directory: %d %q %q", code, out, errs)
	}
	if after := fingerprint(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("verification changed the state directory")
	}
}

func TestVerifyLogCouldNotRun(t *testing.T) {
	// Another holder: refused before the database is read, its record kept.
	dir, _ := fixtureStore(t, 1)
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := os.ReadFile(filepath.Join(dir, eventstore.LockFile))
	code, res, _ := verifyJSON(t, "--state", dir)
	lock.Release()
	if code != entry.ExitCouldNotRun || res != "could_not_run/controller_already_running" {
		t.Fatalf("a held lock: %d %s", code, res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, eventstore.LockFile)); string(b) != string(record) {
		t.Fatal("the holder's lock record changed")
	}
	// A store with no lock file: never created by the probe.
	nolock, _ := fixtureStore(t, 1)
	os.Remove(filepath.Join(nolock, eventstore.LockFile))
	if code, res, _ := verifyJSON(t, "--state", nolock); code != entry.ExitCouldNotRun || res != "could_not_run/store_unreadable" {
		t.Fatalf("no lock file: %d %s", code, res)
	}
	if _, err := os.Stat(filepath.Join(nolock, eventstore.LockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the probe created a lock file")
	}
	// Access denied, to the database or to the state directory: the check
	// cannot run (never "no store there"), and says so in JSON.
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	unreadable, _ := fixtureStore(t, 1)
	if err := os.Chmod(filepath.Join(unreadable, eventstore.DBFile), 0); err != nil {
		t.Fatal(err)
	}
	if code, res, detail := verifyJSON(t, "--state", unreadable); code != entry.ExitCouldNotRun || res != "could_not_run/store_unreadable" || detail == "" {
		t.Fatalf("an unreadable database: %d %s %q", code, res, detail)
	}
	shut, _ := fixtureStore(t, 1)
	if err := os.Chmod(shut, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shut, 0o700) })
	if code, res, _ := verifyJSON(t, "--state", shut); code != entry.ExitCouldNotRun || res != "could_not_run/store_unreadable" {
		t.Fatalf("an inaccessible state directory: %d %s", code, res)
	}
}

func TestVerifyLogUsage(t *testing.T) {
	dir, head := fixtureStore(t, 1)
	if _, _, errs := verifyCmd(); !strings.Contains(errs, "usage:") {
		t.Errorf("no --state: %q", errs)
	}
	hex := strings.TrimPrefix(head.Hash, "sha256:")
	empty := filepath.Join(t.TempDir(), "nothing")
	for name, args := range map[string][]string{
		"no --state":            {},
		"no store there":        {"--state", empty},
		"an empty trusted head": {"--state", dir, "--trusted", ""},
		"a malformed head":      {"--state", dir, "--trusted", "abc"},
		"a negative seq":        {"--state", dir, "--trusted", "-1:" + head.Hash},
		"a signed seq":          {"--state", dir, "--trusted", "+1:" + head.Hash},
		"a leading zero":        {"--state", dir, "--trusted", "01:" + head.Hash},
		"a seq past int64":      {"--state", dir, "--trusted", "9223372036854775808:" + head.Hash},
		"no hash":               {"--state", dir, "--trusted", "1:"},
		"a bare digest":         {"--state", dir, "--trusted", "1:" + hex},
		"another algorithm":     {"--state", dir, "--trusted", "1:sha512:" + hex},
		"uppercase hex":         {"--state", dir, "--trusted", "1:sha256:" + strings.ToUpper(hex)},
		"a short digest":        {"--state", dir, "--trusted", "1:sha256:" + hex[:63]},
		"a non-hex digest":      {"--state", dir, "--trusted", "1:sha256:" + hex[:63] + "g"},
		"an unknown flag":       {"--state", dir, "--force"},
		"an extra argument":     {"--state", dir, "x"},
	} {
		if code, _, _ := verifyCmd(args...); code != entry.ExitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Error("something was created where there was no store")
	}
}
