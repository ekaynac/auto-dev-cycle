// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const refusal = `{"outcome":"failed","reason":"precondition_failed","detail":"d"}`

// decided returns a Decide that commits disposition d with n events and
// result, counting its calls.
func decided(t *testing.T, d Disposition, n int, result string, calls *int) Decide {
	return func(tx *Tx) (Decision, error) {
		*calls++
		evs, _, err := appendTo(t, testSubject, n)(tx)
		return Decision{Disposition: d, Events: evs, Result: json.RawMessage(result)}, err
	}
}

func storedDisposition(t *testing.T, e *env, id string) string {
	t.Helper()
	var d string
	if err := raw(t, e.dir).QueryRow(`SELECT disposition FROM commands WHERE id = ?`, id).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// N1: the stored digest covers the action, the input and the expectations.
// The same actor and key with any of them changed is refused; the same
// request replays.
func TestIdempotencyCoversActionInputAndExpectations(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	first := command(t, "k", `{"a": 1}`, Expectation{testSubject, 0})
	if _, err := e.s.SubmitApplied(ctx, first, Applies(appendTo(t, testSubject, 1))); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(c *Command){
		"different input":     func(c *Command) { c.Input = []byte(`{"a": 2}`) },
		"cross-action":        func(c *Command) { c.Action = "test.other" },
		"changed expectation": func(c *Command) { c.Expected = []Expectation{{testSubject, 1}} },
		"dropped expectation": func(c *Command) { c.Expected = nil },
		"another subject":     func(c *Command) { c.Expected = []Expectation{{"prj_01m3250vz2ezn81cgvbkcn17jd", 0}} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := command(t, "k", `{"a": 1}`, Expectation{testSubject, 0})
			change(&c)
			if _, err := e.s.Submit(ctx, c, func(*Tx) (Decision, error) {
				t.Fatal("a reused key ran")
				return Decision{}, nil
			}); !errors.Is(err, ErrIdempotencyKeyReused) {
				t.Fatalf("got %v, want idempotency_key_reused", err)
			}
		})
	}
	again := command(t, "k", `{ "a" : 1 }`, Expectation{testSubject, 0})
	if out, err := e.s.SubmitApplied(ctx, again, Applies(appendTo(t, testSubject, 1))); err != nil || !out.Replayed {
		t.Fatalf("the identical request: %+v %v", out, err)
	}
	if head(t, e) != 1 {
		t.Fatal("a refused or replayed command appended events")
	}
}

func TestActionIsCanonical(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	for _, a := range []string{"", "grant", "Grant.create", "grant.", ".create", "grant create", "grant.create/v1", strings.Repeat("a.", 64) + "b"} {
		c := command(t, "k", `{}`)
		c.Action = a
		if _, err := e.s.SubmitApplied(context.Background(), c, Applies(appendTo(t, testSubject, 1))); err == nil {
			t.Errorf("action %q was accepted", a)
		}
	}
}

// N2: every disposition is committed with its events, and replays as
// itself; only an applied one passes SubmitApplied.
func TestEveryDispositionIsCommitted(t *testing.T) {
	for _, d := range []Disposition{DispositionApplied, DispositionProposed, DispositionInvalidated, DispositionRefused} {
		t.Run(string(d), func(t *testing.T) {
			e := openWriter(t, Hooks{Authorize: allow})
			ctx := context.Background()
			calls := 0
			c := command(t, "k", `{}`)
			res, err := e.s.Submit(ctx, c, decided(t, d, 1, refusal, &calls))
			if err != nil || res.Disposition != d || res.Replayed || res.FirstSeq != 1 || res.CommandID != c.ID {
				t.Fatalf("submit: %+v %v", res, err)
			}
			if got := storedDisposition(t, e, c.ID); got != string(d) {
				t.Fatalf("stored %q", got)
			}
			_, liveErr := res.Applied()
			out, err := e.s.SubmitApplied(ctx, command(t, "k", `{}`), decided(t, d, 1, refusal, &calls))
			if d == DispositionApplied {
				if liveErr != nil || err != nil || !out.Replayed || out.CommandID != c.ID {
					t.Fatalf("applied: %v, replay %+v %v", liveErr, out, err)
				}
			} else if !errors.Is(liveErr, ErrNotApplied) || !errors.Is(err, ErrNotApplied) || out.CommandID != "" {
				t.Fatalf("%s passed as applied: live %v, replay %+v %v", d, liveErr, out, err)
			}
			if calls != 1 || head(t, e) != 1 {
				t.Fatalf("calls %d, head %d", calls, head(t, e))
			}
		})
	}
}

// A decision the writer cannot store as one of the dispositions is refused
// and commits nothing.
func TestMalformedDecisionsCommitNothing(t *testing.T) {
	cases := map[string]func(t *testing.T, tx *Tx) Decision{
		"zero": func(*testing.T, *Tx) Decision { return Decision{Result: json.RawMessage(`{}`)} },
		"unknown": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: "deferred", Result: json.RawMessage(`{}`)}
		},
		"refused without a reason": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionRefused, Result: json.RawMessage(`{"outcome":"failed"}`)}
		},
		"invalidated without a reason": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionInvalidated, Result: json.RawMessage(`{}`)}
		},
		"proposed with an effect": func(t *testing.T, tx *Tx) Decision {
			if err := tx.IntendEffect(newID(t, "eff"), "model_call", "provider-a", json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			return Decision{Disposition: DispositionProposed, Result: json.RawMessage(`{}`)}
		},
		"a credential when refused": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionRefused, Result: json.RawMessage(refusal), Credential: NewIssuedCredential("tok_x", testSecret)}
		},
		"a credential without a secret": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionApplied, Result: json.RawMessage(`{}`), Credential: &IssuedCredential{id: "tok_x"}}
		},
		"a credential without an id": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionApplied, Result: json.RawMessage(`{}`), Credential: NewIssuedCredential("", testSecret)}
		},
		"a short secret": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionApplied, Result: json.RawMessage(`{}`), Credential: NewIssuedCredential("tok_x", "short")}
		},
		"a secret that JSON would escape": func(*testing.T, *Tx) Decision {
			return Decision{Disposition: DispositionApplied, Result: json.RawMessage(`{}`), Credential: NewIssuedCredential("tok_x", `0123456789abcdef"`)}
		},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			e := openEffectsAt(t, stateDir(t))
			if _, err := e.s.Submit(context.Background(), command(t, "k", `{}`), func(tx *Tx) (Decision, error) { return d(t, tx), nil }); err == nil {
				t.Fatal("committed")
			}
			var n int
			if err := raw(t, e.dir).QueryRow(`SELECT count(*) FROM commands`).Scan(&n); err != nil || n != 0 || head(t, e) != 0 {
				t.Fatalf("committed: %d commands, head %d, %v", n, head(t, e), err)
			}
		})
	}
}

const testSecret = "hunter2-0123456789abcdef"

// N3: an issued secret reaches no stored row, whichever path writes it.
// Each attempt is refused and commits nothing: no command, no event, no
// effect, no projection.
func TestIssuedSecretIsNeverStored(t *testing.T) {
	escaped := `{"token":"\u0068unter2-0123456789abcdef"}` // decodes to the secret
	cases := map[string]func(t *testing.T, tx *Tx) ([]Event, json.RawMessage){
		"in the result": func(*testing.T, *Tx) ([]Event, json.RawMessage) {
			return nil, json.RawMessage(`{"token":"` + testSecret + `"}`)
		},
		"in an event payload": func(t *testing.T, _ *Tx) ([]Event, json.RawMessage) {
			return []Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(`{"t":"` + testSecret + `"}`)}}, nil
		},
		"JSON-escaped in an event payload": func(t *testing.T, _ *Tx) ([]Event, json.RawMessage) {
			return []Event{{ID: newID(t, "evt"), Kind: "test.appended/v1", Subject: testSubject, Payload: []byte(escaped)}}, nil
		},
		"JSON-escaped in the result": func(*testing.T, *Tx) ([]Event, json.RawMessage) { return nil, json.RawMessage(escaped) },
		"in an effect target": func(t *testing.T, tx *Tx) ([]Event, json.RawMessage) {
			if err := tx.IntendEffect(newID(t, "eff"), "model_call", testSecret, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		},
		"in a projection value": func(t *testing.T, tx *Tx) ([]Event, json.RawMessage) {
			if err := tx.Put("test_tokens", "tok_1", json.RawMessage(`{"s":"`+testSecret+`"}`)); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		},
		"JSON-escaped in a projection value": func(t *testing.T, tx *Tx) ([]Event, json.RawMessage) {
			if err := tx.Put("test_tokens", "tok_1", json.RawMessage(escaped)); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		},
		"in a projection key": func(t *testing.T, tx *Tx) ([]Event, json.RawMessage) {
			if err := tx.Put("test_tokens", testSecret, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			e := openEffectsAt(t, stateDir(t))
			_, err := e.s.Submit(context.Background(), command(t, "k", `{}`), func(tx *Tx) (Decision, error) {
				evs, result := write(t, tx)
				if result == nil {
					result = json.RawMessage(`{}`)
				}
				return Decision{Disposition: DispositionApplied, Events: evs, Result: result, Credential: NewIssuedCredential("tok_1", testSecret)}, nil
			})
			if err == nil || !strings.Contains(err.Error(), "secret would be stored") {
				t.Fatalf("got %v", err)
			}
			var n int
			if err := raw(t, e.dir).QueryRow(`SELECT (SELECT count(*) FROM commands) + (SELECT count(*) FROM events) +
				(SELECT count(*) FROM effects) + (SELECT count(*) FROM proj_kv)`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%d rows committed, %v", n, err)
			}
		})
	}
}

// N3: an issued credential is delivered once. The stored result is free of
// it, and a replay returns none and mints nothing.
func TestCredentialIsDeliveredOnce(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx := context.Background()
	minted := 0
	issue := func(tx *Tx) (Decision, error) {
		minted++
		evs, _, err := appendTo(t, testSubject, 1)(tx)
		return Decision{Disposition: DispositionApplied, Events: evs, Result: json.RawMessage(`{"token":"tok_1"}`),
			Credential: NewIssuedCredential("tok_1", fmt.Sprintf("secret-%d-0123456789abcdef", minted))}, err
	}
	live, err := e.s.Submit(ctx, command(t, "k", `{}`), issue)
	if err != nil || live.Credential == nil || live.Credential.Reveal() != "secret-1-0123456789abcdef" {
		t.Fatalf("live: %+v %v", live, err)
	}
	var stored string
	if err := raw(t, e.dir).QueryRow(`SELECT result_json FROM commands`).Scan(&stored); err != nil || strings.Contains(stored, "secret") {
		t.Fatalf("stored result %q, %v", stored, err)
	}
	replay, err := e.s.Submit(ctx, command(t, "k", `{}`), issue)
	if err != nil || !replay.Replayed || replay.Credential != nil || string(replay.Result) != string(live.Result) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if minted != 1 || head(t, e) != 1 {
		t.Fatalf("minted %d, head %d", minted, head(t, e))
	}
}

func TestIssuedCredentialFormattingWithholdsTheSecret(t *testing.T) {
	c := NewIssuedCredential("tok_1", testSecret)
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%p", "%T", "%c", "%U", "%t", "%8.3v"} {
		for _, v := range []any{c, *c, Committed{Credential: c}, &Committed{Credential: c}} {
			if s := fmt.Sprintf(f, v); strings.Contains(s, "hunter2") || strings.Contains(s, "68756e74657232") {
				t.Errorf("%s of %T: %q", f, v, s)
			}
		}
	}
	for _, v := range []any{c, *c, Committed{Credential: c}, Decision{Credential: c}} {
		if b, err := json.Marshal(v); err == nil || strings.Contains(string(b), "hunter2") {
			t.Errorf("marshalled %T: %s, %v", v, b, err)
		}
	}
	if c.Reveal() != testSecret || c.ID() != "tok_1" || (&IssuedCredential{}).Reveal() != "" {
		t.Fatal("the accessors")
	}
}

// A decision made over a storage fault is not committed, even when the
// logic swallowed the error and decided a refusal.
func TestDecisionOverAStorageFaultIsNotCommitted(t *testing.T) {
	e := openWriter(t, Hooks{Authorize: allow})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.s.Submit(ctx, command(t, "k", `{}`), func(tx *Tx) (Decision, error) {
		cancel()
		_, _, _ = tx.Get("test", "key") // the fault, swallowed
		return Decision{Disposition: DispositionRefused, Result: json.RawMessage(refusal)}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "storage fault") {
		t.Fatalf("got %v", err)
	}
}
