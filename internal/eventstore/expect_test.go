// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A command expecting a state of its object fails closed when Authorize
// bound no object, or one without a record definition (for a version too),
// or tried to bind twice; none of those is stale, and none commits anything.
func TestExpectFailsClosed(t *testing.T) {
	v := int64(0)
	d := "sha256:" + strings.Repeat("0", 64)
	bind := func(objects ...Object) func(*Tx, Command) error {
		return func(tx *Tx, _ Command) error {
			for _, o := range objects {
				if err := tx.BindObject(o); err != nil {
					return err
				}
			}
			return nil
		}
	}
	record := func(*Tx) (json.RawMessage, bool, error) { return nil, false, nil }
	for name, tc := range map[string]struct {
		authorize func(*Tx, Command) error
		expect    State
		want      string
	}{
		"nothing bound":        {allow, State{Version: &v}, "no object was bound"},
		"no subject":           {bind(Object{Record: record}), State{Version: &v}, "needs a subject"},
		"no record, a version": {bind(Object{Subject: testSubject}), State{Version: &v}, "no record definition"},
		"no record, a digest":  {bind(Object{Subject: testSubject}), State{Digest: &d}, "no record definition"},
		"bound twice":          {bind(Object{Subject: testSubject, Record: record}, Object{Subject: testSubject, Record: record}), State{Version: &v}, "already bound"},
	} {
		e := openWriter(t, Hooks{Identify: allow, Authorize: tc.authorize})
		c := command(t, "k", `{}`)
		c.Expect = &tc.expect
		_, err := e.s.Submit(context.Background(), c, Applies(appendTo(t, testSubject, 1)))
		if err == nil || errors.Is(err, ErrStaleExpectedState) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if h := head(t, e); h != 0 {
			t.Errorf("%s: head %d", name, h)
		}
	}
	// Only a command's transaction binds.
	e := openWriter(t, Hooks{Identify: allow, Authorize: allow})
	if err := e.s.View(context.Background(), func(tx *Tx) error {
		return tx.BindObject(Object{Subject: testSubject, Record: record})
	}); err == nil || !strings.Contains(err.Error(), "only a command's transaction") {
		t.Errorf("a view bound an object: %v", err)
	}
}

// A request without an expected state keeps the digest it had before
// expected states existed, so its stored commands still replay.
func TestRequestDigestWithoutExpect(t *testing.T) {
	e := openWriter(t, Hooks{Identify: allow, Authorize: allow})
	c := command(t, "k", `{"a": 1}`)
	if _, err := e.s.SubmitApplied(context.Background(), c, Applies(appendTo(t, testSubject, 1))); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := raw(t, e.dir).QueryRow(`SELECT input_digest FROM commands WHERE id = ?`, c.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want, err := digest(json.RawMessage(`{"action":"test.command","input":{"a":1},"expected":[]}`))
	if err != nil || got != want {
		t.Fatalf("input digest %s, want %s (%v)", got, want, err)
	}
}
