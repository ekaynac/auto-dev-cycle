// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

// counting projects each event kind's count and each subject's last seq:
// a fold over events alone, as a projector must be.
func counting(fail *bool) func(*Tx, []Event) error {
	return func(tx *Tx, evs []Event) error {
		for _, ev := range evs {
			n := 0
			if v, ok, err := tx.Get("kinds", ev.Kind); err != nil {
				return err
			} else if ok {
				if err := json.Unmarshal(v, &n); err != nil {
					return err
				}
			}
			if err := tx.Put("kinds", ev.Kind, json.RawMessage(strconv.Itoa(n+1))); err != nil {
				return err
			}
			if err := tx.Put("subjects", ev.Subject, json.RawMessage(strconv.FormatInt(ev.Seq, 10))); err != nil {
				return err
			}
			// recent keeps the last two events by zero-padded seq, through
			// Keys and Delete.
			if err := tx.Put("recent", fmt.Sprintf("%08d", ev.Seq), json.RawMessage(strconv.Quote(ev.Kind))); err != nil {
				return err
			}
			keys, err := tx.Keys("recent", "")
			if err != nil {
				return err
			}
			for _, k := range keys[:max(0, len(keys)-2)] {
				if err := tx.Delete("recent", k); err != nil {
					return err
				}
			}
		}
		if fail != nil && *fail {
			return errors.New("projection failed")
		}
		return nil
	}
}

func openProjected(t *testing.T, dir string, project func(*Tx, []Event) error) *env {
	t.Helper()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs, Effects: testEffects,
		Hooks: Hooks{Authorize: allow, Project: project}})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	t.Cleanup(e.close)
	return e
}

// populate submits commands with events and an effect, and moves the effect
// through restart and reconciliation, so every write path has projected.
func populate(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := e.s.Submit(ctx, command(t, fmt.Sprintf("p-%d", i), `{}`), func(*Tx) ([]Event, json.RawMessage, error) {
			return []Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	id := intend(t, e, "model_call")
	if _, err := e.s.MarkPendingUncertain(ctx, sysActor); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RecordEffect(ctx, sysActor, id, Transition{Kind: "reconciled", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
}

func liveRows(t *testing.T, dir string) map[string]string {
	t.Helper()
	rows, err := raw(t, dir).Query(`SELECT projection || '/' || key, value FROM proj_kv`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// 03: the log replayed into fresh tables matches the live projections, and
// the rebuild changes nothing; it can run again.
func TestRebuildMatchesLive(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	before := liveRows(t, dir)
	if len(before) == 0 {
		t.Fatal("nothing was projected")
	}
	defer func(n int) { rebuildBatch = n }(rebuildBatch)
	for i, batch := range []int{rebuildBatch, 2} { // one batch, then several
		rebuildBatch = batch
		diffs, err := e.s.RebuildProjections(context.Background())
		if err != nil || len(diffs) != 0 {
			t.Fatalf("rebuild %d: %v, %+v", i, err, diffs)
		}
	}
	after := liveRows(t, dir)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the rebuild changed the live projections:\n%v\n%v", before, after)
	}
	if n := len(filter(after, "recent/")); n != 2 {
		t.Fatalf("recent holds %d records", n)
	}
	// Live writes after a rebuild still reach the live table.
	_, err := e.s.Submit(context.Background(), command(t, "after-rebuild", `{}`), func(*Tx) ([]Event, json.RawMessage, error) {
		return []Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if liveRows(t, dir)["kinds/test.appended/v1"] != "4" {
		t.Fatalf("a submit after the rebuild missed the live table: %v", liveRows(t, dir))
	}
	if diffs, err := e.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("after a further submit: %v, %+v", err, diffs)
	}
}

func filter(rows map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range rows {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out[k] = v
		}
	}
	return out
}

// A tampered, missing or extra live record is reported, sorted, with both
// sides.
func TestRebuildReportsDrift(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	mustExec(t, raw(t, dir),
		`UPDATE proj_kv SET value = '99' WHERE projection = 'kinds' AND key = 'test.appended/v1'`,
		`DELETE FROM proj_kv WHERE projection = 'kinds' AND key = 'effect.intent/v1'`,
		`INSERT INTO proj_kv(projection, key, value) VALUES ('stray', 'x', '1')`)
	diffs, err := e.s.RebuildProjections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 3 {
		t.Fatalf("diffs: %+v", diffs)
	}
	missing, changed, extra := diffs[0], diffs[1], diffs[2]
	if missing.Key != "effect.intent/v1" || missing.Live != nil || *missing.Rebuilt != "1" {
		t.Errorf("missing: %+v", missing)
	}
	if changed.Key != "test.appended/v1" || *changed.Live != "99" || *changed.Rebuilt != "3" {
		t.Errorf("changed: %+v", changed)
	}
	if extra.Projection != "stray" || *extra.Live != "1" || extra.Rebuilt != nil {
		t.Errorf("extra: %+v", extra)
	}
}

// Without a projector, any live record is drift; a broken chain or a failing
// projector refuses the rebuild and leaves the store as it was.
func TestRebuildRefusals(t *testing.T) {
	dir := stateDir(t)
	fail := false
	e := openProjected(t, dir, counting(&fail))
	populate(t, e)
	before := liveRows(t, dir)
	fail = true
	if _, err := e.s.RebuildProjections(context.Background()); err == nil {
		t.Error("a failing projector")
	}
	if fmt.Sprint(liveRows(t, dir)) != fmt.Sprint(before) {
		t.Fatal("a failed rebuild changed the live projections")
	}
	fail = false
	if diffs, err := e.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("after a failed rebuild: %v, %+v", err, diffs)
	}
	e.close()

	bare := openEnv(t, dir)
	if diffs, err := bare.s.RebuildProjections(context.Background()); err != nil || len(diffs) != len(before) {
		t.Fatalf("no projector: %v, %d diffs for %d records", err, len(diffs), len(before))
	}
	mustExec(t, raw(t, dir), `DROP TRIGGER events_append_only_update`, `UPDATE events SET payload = '{"x":1}' WHERE seq = 1`)
	if _, err := bare.s.RebuildProjections(context.Background()); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a broken chain: %v", err)
	}
}

// UnreadKinds names the persisted kinds the store was not opened with; the
// store's own effect kinds are always read.
func TestUnreadKinds(t *testing.T) {
	dir := stateDir(t)
	e := openProjected(t, dir, counting(nil))
	populate(t, e)
	if unread, err := e.s.UnreadKinds(context.Background()); err != nil || len(unread) != 0 {
		t.Fatalf("every kind is registered: %v, %v", err, unread)
	}
	e.close()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), lock, Config{Kinds: Kinds{"test.typed/v1": testKinds["test.typed/v1"]}, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if unread, err := s.UnreadKinds(context.Background()); err != nil || fmt.Sprint(unread) != "[test.appended/v1]" {
		t.Fatalf("an unregistered kind: %v, %v", err, unread)
	}
}
