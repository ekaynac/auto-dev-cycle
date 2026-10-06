// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"

	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// 14 P3: the writer checks the version and the content digest a request
// expects of the object its handler derives, inside the transaction and
// before the command's logic runs.

func version(v int64) *eventstore.State { return &eventstore.State{Version: &v} }

func digestOf(d string) *eventstore.State { return &eventstore.State{Digest: &d} }

func both(v int64, d string) *eventstore.State { return &eventstore.State{Version: &v, Digest: &d} }

// edit submits note.add of obj under key, expecting expect, whose logic sets
// the note's record to record ("": deletes it); ran reports whether that
// logic ran.
func (b *boundary) edit(key, obj, record string, expect *eventstore.State) (out eventstore.Committed, ran bool, err error) {
	c := b.op.Command(newID(b.t, "cmd"), "note.add", key, []byte(`{"project":"`+string(prjA)+`","object":"`+obj+`"}`), nil)
	c.Expect = expect
	ev := eventstore.Event{ID: newID(b.t, "evt"), Kind: "test.noted/v1", Subject: obj, Payload: json.RawMessage(record)}
	if record == "" {
		ev.Kind, ev.Payload = "test.unnoted/v1", json.RawMessage(`{}`)
	}
	out, err = b.s.Submit(context.Background(), c, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		ran = true
		return []eventstore.Event{ev}, json.RawMessage(`{}`), nil
	}))
	return out, ran, err
}

// applied submits an edit that must apply, and returns the note's version.
func (b *boundary) applied(key, obj, record string, expect *eventstore.State) int64 {
	b.t.Helper()
	out, _, err := b.edit(key, obj, record, expect)
	if err != nil || out.Disposition != eventstore.DispositionApplied || out.Replayed {
		b.t.Fatalf("edit %s: %+v %v", key, out, err)
	}
	return out.LastSeq
}

// stale submits an edit that must be refused as stale, before its logic
// runs and committing nothing: the head stays, and the same key is free.
func (b *boundary) stale(name, key, obj string, expect *eventstore.State) {
	b.t.Helper()
	h := b.head()
	out, ran, err := b.edit(key, obj, `{"x":1}`, expect)
	if !errors.Is(err, eventstore.ErrStaleExpectedState) || out.CommandID != "" {
		b.t.Errorf("%s: %+v %v", name, out, err)
	}
	if ran || b.head() != h {
		b.t.Errorf("%s: the logic ran (%v) or the log moved", name, ran)
	}
}

func contentDigest(t *testing.T, record string) string {
	t.Helper()
	d, err := eventstore.ContentDigest(json.RawMessage(record))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestExpectedVersionAndDigest(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	// A missing object matches version 0 only; any other expectation of it
	// is stale, not an error.
	b.stale("version 1 of a missing object", "a", obj, version(1))
	b.stale("a digest of a missing object", "a", obj, digestOf(contentDigest(t, `{}`)))
	v := b.applied("a", obj, `{"a":1,"b":[2,3]}`, version(0))
	// The digest is the canonical record's: field order and spacing do not
	// change it.
	d := contentDigest(t, `{ "b": [2, 3], "a": 1 }`)
	b.stale("version 0 of an existing object", "b", obj, version(0))
	b.stale("a stale version", "b", obj, version(v-1))
	b.stale("a stale digest", "b", obj, digestOf(contentDigest(t, `{"a":2,"b":[2,3]}`)))
	b.stale("a stale digest, the version current", "b", obj, both(v, contentDigest(t, `{"a":2}`)))
	b.stale("a stale version, the digest current", "b", obj, both(v-1, d))
	v = b.applied("b", obj, `{"a":2}`, digestOf(d))
	v = b.applied("c", obj, `{"a":3}`, version(v))
	d = contentDigest(t, `{"a":3}`)
	b.applied("d", obj, `{"a":4}`, both(v, d))
}

// Two callers inspect the same state; the first one's change makes the
// second one stale.
func TestConcurrentChangeIsStale(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	v := b.applied("seed", obj, `{"n":0}`, nil)
	d := contentDigest(t, `{"n":0}`)
	for _, inspect := range []func(v int64) *eventstore.State{version, func(int64) *eventstore.State { return digestOf(d) }, func(v int64) *eventstore.State { return both(v, d) }} {
		expect := inspect(v)
		b.applied("first-"+expectKey(expect), obj, `{"n":1}`, expect)
		b.stale("the second caller", "second-"+expectKey(expect), obj, expect)
		// Back to the inspected record, under a new version.
		v = b.applied("reset-"+expectKey(expect), obj, `{"n":0}`, nil)
	}
}

func expectKey(s *eventstore.State) string {
	var k []string
	if s.Version != nil {
		k = append(k, "v")
	}
	if s.Digest != nil {
		k = append(k, "d")
	}
	return strings.Join(k, "")
}

// The expectations are part of the request (14 N1): the same key with
// another expectation is refused, the identical request replays.
func TestExpectationIsPartOfTheRequest(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	b.applied("k", obj, `{"n":0}`, version(0))
	d := contentDigest(t, `{"n":0}`)
	for _, other := range []*eventstore.State{nil, version(1), digestOf(d), both(0, d)} {
		if _, ran, err := b.edit("k", obj, `{"n":0}`, other); !errors.Is(err, eventstore.ErrIdempotencyKeyReused) || ran {
			t.Errorf("another expectation %s under the key: %v", expectKey(orEmpty(other)), err)
		}
	}
	if out, ran, err := b.edit("k", obj, `{"n":0}`, version(0)); err != nil || !out.Replayed || ran {
		t.Errorf("the identical request: %+v %v", out, err)
	}
}

func orEmpty(s *eventstore.State) *eventstore.State {
	if s == nil {
		return &eventstore.State{}
	}
	return s
}

// An expected state needs a version or a digest, and the object a digest
// definition for a digest; a command whose principal fails the recheck is
// refused before any derivation.
func TestExpectationRefusals(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	if _, _, err := b.edit("k", obj, `{}`, &eventstore.State{}); err == nil || !strings.Contains(err.Error(), "a version (0 or more), a digest or both") {
		t.Errorf("an empty expected state: %v", err)
	}
	if _, _, err := b.edit("k", obj, `{}`, version(-1)); err == nil {
		t.Errorf("a negative version: accepted")
	}
	c := b.op.Command(newID(t, "cmd"), "note.add", "k", noteInput(prjA), nil)
	c.Expect = version(0)
	c.Principal = nil
	if _, err := b.note(c); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("no principal: %v", err)
	}
}

// Existence is the record's, whatever the subject's history: a deleted
// note is missing, so only version 0 without a digest matches it, and it
// can be created again.
func TestDeleteAndRecreate(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	v := b.applied("create", obj, `{"n":1}`, version(0))
	v = b.applied("delete", obj, "", version(v))
	b.stale("the deletion's version of a deleted note", "a", obj, version(v))
	b.stale("version 0 and a digest of a deleted note", "a", obj, both(0, contentDigest(t, `{"n":1}`)))
	b.stale("the old digest of a deleted note", "a", obj, digestOf(contentDigest(t, `{"n":1}`)))
	v = b.applied("recreate", obj, `{"n":2}`, version(0))
	b.stale("version 0 of the recreated note", "b", obj, version(0))
	b.applied("b", obj, `{"n":3}`, both(v, contentDigest(t, `{"n":2}`)))
}

// Competing submissions inspected the same state: exactly one applies,
// and every loser is stale, its logic never run.
func TestCompetingSubmissions(t *testing.T) {
	b := openBoundary(t)
	obj := newID(t, "prj")
	v := b.applied("seed", obj, `{"n":0}`, nil)
	d := contentDigest(t, `{"n":0}`)
	for _, inspect := range []func(v int64) *eventstore.State{version, func(int64) *eventstore.State { return digestOf(d) }, func(v int64) *eventstore.State { return both(v, d) }} {
		expect := inspect(v)
		const n = 8
		var wg sync.WaitGroup
		var applied, ranStale, stale atomic.Int32
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, ran, err := b.edit(expectKey(expect)+string(rune('a'+i)), obj, `{"n":1}`, expect)
				switch {
				case err == nil && out.Disposition == eventstore.DispositionApplied:
					applied.Add(1)
				case errors.Is(err, eventstore.ErrStaleExpectedState):
					stale.Add(1)
					if ran {
						ranStale.Add(1)
					}
				default:
					t.Errorf("%s: %+v %v", expectKey(expect), out, err)
				}
			}()
		}
		wg.Wait()
		if applied.Load() != 1 || stale.Load() != n-1 || ranStale.Load() != 0 {
			t.Fatalf("%s: %d applied, %d stale, %d stale ran", expectKey(expect), applied.Load(), stale.Load(), ranStale.Load())
		}
		v = b.applied("reset-"+expectKey(expect), obj, `{"n":0}`, nil)
	}
}

// flipHandler derives an existing note first and a missing one after:
// whatever a second derivation would say, the object authorized is the one
// checked.
type flipHandler struct {
	first, then string
	calls       atomic.Int32
}

func (h *flipHandler) Derive(tx *eventstore.Tx, _ types.Actor, _ json.RawMessage) (authority.Derived, error) {
	obj := h.first
	if h.calls.Add(1) > 1 {
		obj = h.then
	}
	p := prjA
	return authority.Derived{Object: obj, Project: &p, Record: func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(notes, obj) }}, nil
}

func TestObjectIsDerivedOnce(t *testing.T) {
	b := openBoundary(t)
	existing, missing := newID(t, "prj"), newID(t, "prj")
	v := b.applied("seed", existing, `{"n":0}`, nil)
	for i, tc := range []struct {
		expect *eventstore.State
		stale  bool
	}{{version(0), true}, {version(v), false}} {
		h := &flipHandler{first: existing, then: missing}
		b.az.Handlers = map[string]authority.Handler{"note.add": h}
		_, ran, err := b.edit(string(rune('0'+i)), missing, `{"n":1}`, tc.expect)
		if errors.Is(err, eventstore.ErrStaleExpectedState) != tc.stale || ran == tc.stale || (!tc.stale && err != nil) {
			t.Errorf("expecting %d of the authorized note: ran %v, %v", *tc.expect.Version, ran, err)
		}
		if n := h.calls.Load(); n != 1 {
			t.Errorf("%d derivations", n)
		}
	}
}
