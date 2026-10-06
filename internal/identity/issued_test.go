// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// failAfterAppend is a test mode of issueCommand: the token event is
// appended, then the command fails and rolls back.
const failAfterAppend eventstore.Disposition = "fail after append"

// issuance is one run of issueCommand.
type issuance struct {
	issued    identity.Issued
	res       eventstore.Committed
	err       error
	secret    identity.Secret
	delivered bool
}

// issueCommand is a token issuance as the request layer runs it (14 N3):
// the token is minted before Submit, the command's logic records only its
// public id and hash, and the secret is delivered only from a fresh applied
// commit. The command's input is the client's request; the mint is not in
// it, so a retry under the same key replays rather than conflicting.
func (f *fixture) issueCommand(key string, mode eventstore.Disposition) issuance {
	f.t.Helper()
	issued, err := identity.Mint(f.op.ID, f.op.ID, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	c := eventstore.Command{ID: id(f.t, "cmd"), Actor: string(f.op.ID), Action: "token.issue", IdempotencyKey: key, Input: []byte(`{"actor":"` + string(f.op.ID) + `"}`)}
	res, err := f.s.Submit(context.Background(), c, func(*eventstore.Tx) (eventstore.Decision, error) {
		evs, err := issued.Events()
		switch mode {
		case eventstore.DispositionApplied:
			return eventstore.Decision{Disposition: mode, Events: evs, Result: json.RawMessage(`{"token":"` + string(issued.Token.ID) + `"}`)}, err
		case failAfterAppend: // the second event is unregistered: the first is appended, then the append fails
			bogus := eventstore.Event{ID: id(f.t, "evt"), Kind: "test.unregistered/v1", Subject: string(issued.Token.ID), Payload: []byte(`{}`)}
			return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: append(evs, bogus), Result: json.RawMessage(`{}`)}, err
		}
		return eventstore.Decision{Disposition: mode, Result: json.RawMessage(`{"outcome":"failed","reason":"precondition_failed","detail":"d"}`)}, nil
	})
	out := issuance{issued: issued, res: res, err: err}
	if err == nil {
		out.secret, out.delivered = issued.Deliver(res)
	}
	return out
}

// stored reports whether any file of the state directory (the database,
// its WAL included, which must exist) holds s.
func (f *fixture) stored(s string) bool {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.dir, "adc.db-wal")); err != nil {
		f.t.Fatalf("the WAL is not there to search: %v", err)
	}
	found := false
	err := filepath.WalkDir(f.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		found = found || bytes.Contains(b, []byte(s))
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func secretPart(plaintext string) string {
	_, v, _ := strings.Cut(plaintext, ".")
	return v
}

// leaked reports whether the run's minted plaintext is anywhere it must not
// be: the state directory or the returned error.
func (f *fixture) leaked(r issuance) bool {
	f.t.Helper()
	p := secretPart(identity.Plaintext(r.issued))
	if len(p) < 40 {
		f.t.Fatalf("no plaintext to search for: %d bytes", len(p))
	}
	return f.stored(p) || (r.err != nil && strings.Contains(r.err.Error(), p))
}

// Only the hash reaches the store; the secret is delivered once, from the
// fresh applied commit, and authenticates.
func TestIssuedTokenDeliversOnceAndStoresOnlyTheHash(t *testing.T) {
	f := open(t)
	r := f.issueCommand("k", eventstore.DispositionApplied)
	if r.err != nil || !r.delivered || r.res.Replayed || r.secret.Reveal() != identity.Plaintext(r.issued) {
		t.Fatalf("delivered %v, replayed %v, %v", r.delivered, r.res.Replayed, r.err)
	}
	if f.leaked(r) || !f.stored(r.issued.Token.Hash) {
		t.Fatal("the store holds the secret, or not the hash")
	}
	if a, err := f.authenticate(r.secret.Reveal()); err != nil || a.ID != f.op.ID {
		t.Fatalf("authenticate: %v", err)
	}
	// A retry under the same key replays: no delivery, and the retry's
	// own mint is neither recorded nor delivered.
	again := f.issueCommand("k", eventstore.DispositionApplied)
	if again.err != nil || again.delivered || again.secret.Reveal() != "" || !again.res.Replayed || again.res.CommandID != r.res.CommandID {
		t.Fatalf("replay delivered %v, replayed %v, %v", again.delivered, again.res.Replayed, again.err)
	}
	if f.leaked(again) || f.projected(identity.TokenProjection, string(again.issued.Token.ID)) || f.stored(again.issued.Token.Hash) {
		t.Fatal("a replay recorded a new token")
	}
}

// A command that does not apply delivers nothing, live or replayed, and
// its mint's plaintext is nowhere in the store.
func TestIssuedTokenIsNotDeliveredUnlessApplied(t *testing.T) {
	for _, d := range []eventstore.Disposition{eventstore.DispositionRefused, eventstore.DispositionProposed, eventstore.DispositionInvalidated} {
		t.Run(string(d), func(t *testing.T) {
			f := open(t)
			for _, replay := range []bool{false, true} {
				r := f.issueCommand("k", d)
				if r.err != nil || r.delivered || r.secret.Reveal() != "" || r.res.Replayed != replay || r.res.Disposition != d {
					t.Fatalf("replay %v: delivered %v, %+v, %v", replay, r.delivered, r.res, r.err)
				}
				if f.leaked(r) || f.projected(identity.TokenProjection, string(r.issued.Token.ID)) || f.stored(r.issued.Token.Hash) {
					t.Fatal("a token or its secret was recorded")
				}
			}
		})
	}
}

// A command that fails after its token event was appended commits no token,
// delivers nothing, and its plaintext is neither in the store (its WAL
// included) nor in the error.
func TestIssuedTokenFailingAfterAppendLeavesNothing(t *testing.T) {
	f := open(t)
	r := f.issueCommand("k", failAfterAppend)
	if r.err == nil || !strings.Contains(r.err.Error(), "unregistered event kind") || r.delivered || r.res.CommandID != "" {
		t.Fatalf("delivered %v, %+v, %v", r.delivered, r.res, r.err)
	}
	if f.leaked(r) || f.projected(identity.TokenProjection, string(r.issued.Token.ID)) {
		t.Fatal("the failed issuance left its token or secret")
	}
	// The key stays free: a retry issues normally.
	if ok := f.issueCommand("k", eventstore.DispositionApplied); ok.err != nil || !ok.delivered || ok.res.Replayed {
		t.Fatalf("the retry: %v", ok.err)
	}
	if errors.Is(r.err, eventstore.ErrNotApplied) {
		t.Fatal("a failure read as a committed disposition")
	}
}

// Deliver reads the disposition and the replay flag only.
func TestDeliverNeedsAFreshApplied(t *testing.T) {
	issued, err := identity.Mint(types.ActorID(id(t, "act")), types.ActorID(id(t, "act")), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []eventstore.Committed{
		{Disposition: eventstore.DispositionApplied, Replayed: true},
		{Disposition: eventstore.DispositionInvalidated},
		{},
	} {
		if _, ok := issued.Deliver(c); ok {
			t.Errorf("delivered for %+v", c)
		}
	}
	if s, ok := issued.Deliver(eventstore.Committed{Disposition: eventstore.DispositionApplied}); !ok || s.Reveal() == "" {
		t.Error("not delivered for a fresh applied commit")
	}
}
