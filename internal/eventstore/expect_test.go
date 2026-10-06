// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A command expecting a state of its object fails closed without an
// object derivation, without a derived object, and, for a digest, without
// a digest definition; none of those is stale, and none commits anything.
func TestExpectFailsClosed(t *testing.T) {
	v := int64(0)
	d := "sha256:" + strings.Repeat("0", 64)
	object := func(o Object) func(*Tx, Command) (Object, error) {
		return func(*Tx, Command) (Object, error) { return o, nil }
	}
	for name, tc := range map[string]struct {
		hook   func(*Tx, Command) (Object, error)
		expect State
		want   string
	}{
		"no derivation":        {nil, State{Version: &v}, "no object derivation"},
		"no object":            {object(Object{}), State{Version: &v}, "derived no object"},
		"no digest definition": {object(Object{Subject: testSubject}), State{Digest: &d}, "no digest definition"},
	} {
		e := openWriter(t, Hooks{Identify: allow, Authorize: allow, Object: tc.hook})
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
