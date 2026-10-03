// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	ok := Event{Kind: "test.appended/v1", Subject: "s", Payload: []byte(`{}`)}
	other := "sha256:" + strings.Repeat("1", 64)
	cases := map[string]func(*Event){
		"unregistered kind":   func(ev *Event) { ev.Kind = "a.b/v1" },
		"bad id":              func(ev *Event) { ev.ID = "evt_x" },
		"integer beyond 2^53": func(ev *Event) { ev.Payload = []byte(`{"n": 9007199254740993}`) },
		"float":               func(ev *Event) { ev.Payload = []byte(`{"n": 1.5}`) },
		"not JSON":            func(ev *Event) { ev.Payload = []byte(`{`) },
		"bad project id":      func(ev *Event) { p := "proj-1"; ev.Project = &p },
		"unpublished blob":    func(ev *Event) { ev.Blobs = []string{other} },
		"no subject":          func(ev *Event) { ev.Subject = "" },
		"bad command id":      func(ev *Event) { c := "cmd_x"; ev.Command = &c },
		"bad causation id":    func(ev *Event) { c := "evt_x"; ev.Causation = &c },
		"payload off schema":  func(ev *Event) { ev.Kind = "test.typed/v1"; ev.Payload = []byte(`{"id": 1}`) },
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
	ev.Blobs = []string{d}
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
