// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
)

func allow(*Tx, Command) error { return nil }

// openWriter opens a store with the given hooks.
func openWriter(t *testing.T, hooks Hooks) *env {
	t.Helper()
	dir := stateDir(t)
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), lock, Config{Kinds: testKinds, Blobs: blobs, Hooks: hooks})
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	e := &env{dir: dir, s: s, lock: lock, blobs: blobs}
	t.Cleanup(e.close)
	return e
}

func command(t *testing.T, key, input string, expected ...Expectation) Command {
	return Command{ID: newID(t, "cmd"), Actor: sysActor, IdempotencyKey: key, Input: []byte(input), Expected: expected}
}

// appendTo returns an Apply that appends n events about subject.
func appendTo(t *testing.T, subject string, n int) Apply {
	return func(*Tx) ([]Event, json.RawMessage, error) {
		var evs []Event
		for i := 0; i < n; i++ {
			evs = append(evs, Event{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: subject, Payload: []byte(`{}`)})
		}
		return evs, json.RawMessage(`{"ok": true}`), nil
	}
}

func head(t *testing.T, e *env) int64 {
	t.Helper()
	h, err := e.s.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return h.Seq
}

func TestSubmitAppliesAndReplays(t *testing.T) {
	var projected []Event
	e := openWriter(t, Hooks{Authorize: allow, Project: func(_ *Tx, evs []Event) error { projected = append(projected, evs...); return nil }})
	ctx := context.Background()
	c := command(t, "k1", `{"a": 1}`)
	out, err := e.s.Submit(ctx, c, appendTo(t, testSubject, 2))
	if err != nil {
		t.Fatal(err)
	}
	if out.Replayed || out.FirstSeq != 1 || out.LastSeq != 2 || string(out.Result) != `{"ok":true}` {
		t.Fatalf("outcome %+v", out)
	}
	if len(projected) != 2 || projected[0].Seq != 1 || projected[0].Command == nil || *projected[0].Command != c.ID || projected[0].Actor != sysActor {
		t.Fatalf("projected %+v", projected)
	}
	// The same command again (same actor, key and input, even reformatted)
	// replays the stored result and runs nothing.
	again := command(t, "k1", `{ "a" : 1 }`)
	out2, err := e.s.Submit(ctx, again, func(*Tx) ([]Event, json.RawMessage, error) {
		t.Fatal("a replayed command ran again")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.Replayed || out2.CommandID != c.ID || out2.FirstSeq != 1 || out2.LastSeq != 2 || string(out2.Result) != `{"ok":true}` {
		t.Fatalf("replay %+v", out2)
	}
	if head(t, e) != 2 {
		t.Fatal("a replay appended events")
	}
}

func TestIdempotencyKeyReuseIsRefused(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	if _, err := e.s.Submit(ctx, command(t, "k", `{"a": 1}`), appendTo(t, testSubject, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Submit(ctx, command(t, "k", `{"a": 2}`), appendTo(t, testSubject, 1)); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("a reused key: %v", err)
	}
	if head(t, e) != 1 {
		t.Fatal("a refused command appended events")
	}
	other := command(t, "k", `{"a": 2}`)
	other.Actor = newID(t, "act")
	if _, err := e.s.Submit(ctx, other, appendTo(t, testSubject, 1)); err != nil {
		t.Fatalf("the same key for another actor: %v", err)
	}
}

func TestStaleExpectedStateIsRefused(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	if _, err := e.s.Submit(ctx, command(t, "a", `{}`, Expectation{testSubject, 0}), appendTo(t, testSubject, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Submit(ctx, command(t, "b", `{}`, Expectation{testSubject, 0}), appendTo(t, testSubject, 1)); !errors.Is(err, ErrStaleExpectedState) {
		t.Fatalf("a stale expectation: %v", err)
	}
	if _, err := e.s.Submit(ctx, command(t, "c", `{}`, Expectation{testSubject, 1}), appendTo(t, testSubject, 1)); err != nil {
		t.Fatalf("the current version: %v", err)
	}
}

// Concurrent conflicting commands: exactly one applies.
func TestConcurrentConflictingCommands(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		c := command(t, "conflict-"+string(rune('a'+i)), `{}`, Expectation{testSubject, 0})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.s.Submit(ctx, c, appendTo(t, testSubject, 1))
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrStaleExpectedState):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 || head(t, e) != 1 {
		t.Fatalf("%d commands applied, head %d; exactly one must", won, head(t, e))
	}
}

// Every refusal or failure inside the transaction rolls all of it back,
// and leaves the idempotency key free.
func TestFailuresRollBack(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		hooks Hooks
		apply func(t *testing.T) Apply
	}{
		"authorization": {Hooks{Authorize: func(*Tx, Command) error { return boom }}, func(t *testing.T) Apply { return appendTo(t, testSubject, 1) }},
		"reservation":   {Hooks{Authorize: allow, Reserve: func(*Tx, Command) error { return boom }}, func(t *testing.T) Apply { return appendTo(t, testSubject, 1) }},
		"projection":    {Hooks{Authorize: allow, Project: func(*Tx, []Event) error { return boom }}, func(t *testing.T) Apply { return appendTo(t, testSubject, 1) }},
		"apply": {Hooks{Authorize: allow}, func(*testing.T) Apply {
			return func(*Tx) ([]Event, json.RawMessage, error) { return nil, nil, boom }
		}},
		"invalid event": {Hooks{Authorize: allow}, func(t *testing.T) Apply {
			return func(*Tx) ([]Event, json.RawMessage, error) {
				return []Event{{ID: newID(t, "evt"), Kind: "unregistered/v1", Subject: testSubject, Payload: []byte(`{}`)}}, json.RawMessage(`{}`), nil
			}
		}},
		"invalid result": {Hooks{Authorize: allow}, func(t *testing.T) Apply {
			return func(*Tx) ([]Event, json.RawMessage, error) { return nil, json.RawMessage(`not json`), nil }
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := openWriter(t, c.hooks)
			ctx := context.Background()
			if _, err := e.s.Submit(ctx, command(t, "k", `{}`), c.apply(t)); err == nil {
				t.Fatal("the command applied")
			}
			if head(t, e) != 0 {
				t.Fatal("events survived the failure")
			}
			var n int
			if err := raw(t, e.dir).QueryRow(`SELECT count(*) FROM commands`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("command rows survived the failure: %d, %v", n, err)
			}
		})
	}
}

func TestNoAuthorizerFailsClosed(t *testing.T) {
	e := openWriter(t, Hooks{})
	if _, err := e.s.Submit(context.Background(), command(t, "k", `{}`), appendTo(t, testSubject, 1)); err == nil || !strings.Contains(err.Error(), "authorizer") {
		t.Fatalf("a command without an authorizer: %v", err)
	}
}

// Astra #39: idempotency comes first: reopened without an authorizer, an
// accepted command still replays and a changed one is key reuse; only new
// commands are refused.
func TestReplayPrecedesTheAuthorizerCheck(t *testing.T) {
	dir := stateDir(t)
	ctx := context.Background()
	open := func(h Hooks) (*Store, *Lock) {
		lock, err := AcquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		blobs, err := blobstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(ctx, lock, Config{Kinds: testKinds, Blobs: blobs, Hooks: h})
		if err != nil {
			t.Fatal(err)
		}
		return s, lock
	}
	s, lock := open(Hooks{Authorize: allow})
	c := command(t, "k", `{"a": 1}`)
	if _, err := s.Submit(ctx, c, appendTo(t, testSubject, 1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	lock.Release()
	s, lock = open(Hooks{})
	defer func() { s.Close(); lock.Release() }()
	if out, err := s.Submit(ctx, command(t, "k", `{"a": 1}`), appendTo(t, testSubject, 1)); err != nil || !out.Replayed {
		t.Fatalf("a replay without an authorizer: %+v, %v", out, err)
	}
	if _, err := s.Submit(ctx, command(t, "k", `{"a": 2}`), appendTo(t, testSubject, 1)); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("a changed input without an authorizer: %v", err)
	}
	if _, err := s.Submit(ctx, command(t, "new", `{}`), appendTo(t, testSubject, 1)); err == nil {
		t.Fatal("a new command without an authorizer")
	}
}

// Astra #39: the expectations are part of the request.
func TestExpectationsArePartOfTheRequest(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	if _, err := e.s.Submit(ctx, command(t, "k", `{}`, Expectation{testSubject, 0}), appendTo(t, testSubject, 1)); err != nil {
		t.Fatal(err)
	}
	for name, exp := range map[string][]Expectation{
		"another version": {{testSubject, 1}},
		"another subject": {{"prj_01m3250vm7f6ktw39dszqnyddr", 0}},
		"none":            nil,
	} {
		if _, err := e.s.Submit(ctx, command(t, "k", `{}`, exp...), appendTo(t, testSubject, 1)); !errors.Is(err, ErrIdempotencyKeyReused) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Astra #39: a projector that reorders its events cannot change the
// recorded range.
func TestProjectorCannotChangeTheRange(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow, Project: func(_ *Tx, evs []Event) error {
		evs[0], evs[1] = evs[1], evs[0]
		return nil
	}})
	out, err := e.s.Submit(context.Background(), command(t, "k", `{}`), appendTo(t, testSubject, 2))
	if err != nil || out.FirstSeq != 1 || out.LastSeq != 2 {
		t.Fatalf("%+v, %v", out, err)
	}
	var first, last int64
	if err := raw(t, e.dir).QueryRow(`SELECT first_seq, last_seq FROM commands`).Scan(&first, &last); err != nil || first != 1 || last != 2 {
		t.Fatalf("recorded range %d..%d, %v", first, last, err)
	}
}

// Astra #39: hooks write only projections through the transaction.
func TestHookWritesAreLimitedToProjections(t *testing.T) {
	refused := []string{
		`INSERT INTO events(seq) VALUES (99)`,
		`INSERT INTO commands VALUES ('a','b','c','d','e','{}',NULL,NULL)`,
		`DELETE FROM meta`,
		`INSERT INTO proj_x VALUES (1); DELETE FROM events`,
		`UPDATE proj_x SET a = 1; INSERT INTO events(seq) VALUES (1)`,
		`DROP TABLE proj_x`,
		`INSERT INTO proj_x SELECT * FROM events; INSERT INTO events DEFAULT VALUES`,
	}
	e := openWriter(t, Hooks{Authorize: func(tx *Tx, _ Command) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS proj_x (a INTEGER)`); err != nil {
			return err
		}
		// A second statement after a valid projection write never runs.
		if _, err := tx.Exec(`INSERT INTO proj_x VALUES (1); CREATE TABLE sneaky (a)`); err == nil {
			t.Error("a chained statement was accepted")
		}
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'sneaky'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("a chained statement ran: %d, %v", n, err)
		}
		for _, q := range refused {
			if _, err := tx.Exec(q); err == nil {
				t.Errorf("%s: ran", q)
			}
		}
		return nil
	}})
	if _, err := e.s.Submit(context.Background(), command(t, "k", `{}`), appendTo(t, testSubject, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitValidatesTheCommand(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	cases := map[string]func(c *Command){
		"bad command id": func(c *Command) { c.ID = "cmd_x" },
		"bad actor":      func(c *Command) { c.Actor = testSubject },
		"empty key":      func(c *Command) { c.IdempotencyKey = "" },
		"long key":       func(c *Command) { c.IdempotencyKey = strings.Repeat("k", 257) },
		"input not JSON": func(c *Command) { c.Input = []byte("{") },
	}
	for name, mutate := range cases {
		c := command(t, "k-"+name, `{}`)
		mutate(&c)
		// No events: the command itself must be checked, not only its events.
		if _, err := e.s.Submit(context.Background(), c, appendTo(t, testSubject, 0)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCommandWithoutEvents(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	out, err := e.s.Submit(context.Background(), command(t, "noop", `{}`), appendTo(t, testSubject, 0))
	if err != nil || out.FirstSeq != 0 || out.LastSeq != 0 {
		t.Fatalf("%+v, %v", out, err)
	}
}

// The commands table is append-only, like the log.
func TestCommandsAreAppendOnly(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	c := command(t, "k", `{}`)
	if _, err := e.s.Submit(context.Background(), c, appendTo(t, testSubject, 1)); err != nil {
		t.Fatal(err)
	}
	db := raw(t, e.dir)
	for _, q := range []string{
		`UPDATE commands SET result_json = '{}'`,
		`DELETE FROM commands`,
		`INSERT OR REPLACE INTO commands SELECT id, actor, idempotency_key, 'sha256:x', disposition, result_json, first_seq, last_seq FROM commands`,
		`INSERT INTO commands VALUES ('cmd_other', '` + sysActor + `', 'k', 'd', 'applied', '{}', NULL, NULL)`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: ran", q)
		}
	}
}
