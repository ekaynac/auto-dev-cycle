// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

// stepClock is a store clock a test sets.
type stepClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stepClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *stepClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.at = t }

// 03 "Event time": an event is recorded at max(clock, the previous event's
// time), command and effect events alike, so a backward clock step never
// makes recorded times decrease, and the chain still verifies.
func TestEventTimeNeverDecreases(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := &stepClock{at: t0}
	dir := stateDir(t)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, lock, Config{Kinds: testKinds, Blobs: blobs, Hooks: Hooks{Identify: allow, Authorize: allow}, Effects: testEffects, Now: clock.now})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	t.Cleanup(e.close)
	// command runs a command of two events and returns its Tx.Now.
	command2 := func(key string) time.Time {
		var now time.Time
		if _, err := s.SubmitApplied(ctx, command(t, key, `{}`), Applies(func(tx *Tx) ([]Event, json.RawMessage, error) {
			now = tx.Now()
			return appendTo(t, testSubject, 2)(tx)
		})); err != nil {
			t.Fatal(err)
		}
		return now
	}
	id := intend(t, e, "model_call") // a command with its effect intent, at t0
	clock.set(t0.Add(-time.Hour))
	if _, err := s.RecordEffect(ctx, sysActor, id, Transition{Kind: "uncertain", Observation: "timeout"}); err != nil {
		t.Fatal(err)
	}
	clock.set(t0.Add(-2 * time.Hour))
	if now := command2("back"); !now.Equal(t0) {
		t.Errorf("a command behind the head's time runs at %s, want %s", now, t0)
	}
	if _, err := s.RecordEffect(ctx, sysActor, id, Transition{Kind: "reconciled", Outcome: "still_uncertain"}); err != nil {
		t.Fatal(err)
	}
	clock.set(t0.Add(time.Hour))
	if now := command2("ahead"); !now.Equal(t0.Add(time.Hour)) {
		t.Errorf("a command ahead of the head's time runs at %s", now)
	}
	if _, err := s.Verify(ctx, nil); err != nil {
		t.Fatalf("the history does not verify: %v", err)
	}
	rows, err := raw(t, dir).Query(`SELECT seq, at, kind FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev time.Time
	var kinds []string
	for rows.Next() {
		var seq int64
		var at, kind string
		if err := rows.Scan(&seq, &at, &kind); err != nil {
			t.Fatal(err)
		}
		a, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || a.Before(prev) || a.After(t0.Add(time.Hour)) {
			t.Errorf("event %d (%s) at %s after %s", seq, kind, at, prev)
		}
		prev, kinds = a, append(kinds, kind)
	}
	if got := strings.Join(kinds, " "); !strings.Contains(got, "effect.uncertain/v1 test.appended/v1 test.appended/v1 effect.reconciled/v1") {
		t.Errorf("the mixed history: %s", got)
	}
	// An event that names a time before the previous event's is refused.
	if _, err := e.appendRaw(t, Event{Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{}`), At: t0.Format(time.RFC3339Nano)}); err == nil {
		t.Error("an event recorded before the previous one")
	}
}

// A command reads the clock once: under a clock that advances an hour on
// every read, its events and its effect intents all carry its Tx.Now.
func TestCommandReadsTheClockOnce(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	reads, base := 0, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	advancing := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return base.Add(time.Duration(reads) * time.Hour)
	}
	dir := stateDir(t)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var projected []Event
	s, err := Open(ctx, lock, Config{Kinds: testKinds, Blobs: blobs, Effects: testEffects, Now: advancing,
		Hooks: Hooks{Identify: allow, Authorize: allow, Project: func(_ *Tx, evs []Event) error { projected = append(projected, evs...); return nil }}})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	before := reads
	var now time.Time
	if _, err := s.SubmitApplied(ctx, command(t, "k", `{}`), Applies(func(tx *Tx) ([]Event, json.RawMessage, error) {
		now = tx.Now()
		if err := tx.IntendEffect(newID(t, "eff"), "model_call", "provider-a", json.RawMessage(`{}`)); err != nil {
			return nil, nil, err
		}
		return appendTo(t, testSubject, 2)(tx)
	})); err != nil {
		t.Fatal(err)
	}
	if len(projected) != 3 || reads-before != 1 {
		t.Fatalf("%d events, %d clock reads", len(projected), reads-before)
	}
	for _, ev := range projected {
		if at, err := time.Parse(time.RFC3339Nano, ev.At); err != nil || !at.Equal(now) {
			t.Errorf("%s at %s, the command's time %s", ev.Kind, ev.At, now)
		}
	}
}
