// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// issueCommand is a token issuance as the request layer runs it (14 N3):
// the token is minted before Submit, the command's logic records only its
// public id and hash, and the secret is delivered only from a fresh applied
// commit. The command's input is the client's request; the mint is not in
// it, so a retry under the same key replays rather than conflicting.
func (f *fixture) issueCommand(key string, disposition eventstore.Disposition) (identity.Issued, eventstore.Committed, identity.Secret, bool) {
	f.t.Helper()
	issued, err := identity.Mint(f.op.ID, f.op.ID, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	c := eventstore.Command{ID: id(f.t, "cmd"), Actor: string(f.op.ID), Action: "token.issue", IdempotencyKey: key, Input: []byte(`{"actor":"` + string(f.op.ID) + `"}`)}
	res, err := f.s.Submit(context.Background(), c, func(*eventstore.Tx) (eventstore.Decision, error) {
		if disposition != eventstore.DispositionApplied {
			return eventstore.Decision{Disposition: disposition, Result: json.RawMessage(`{"outcome":"failed","reason":"precondition_failed","detail":"d"}`)}, nil
		}
		evs, err := issued.Events()
		return eventstore.Decision{Disposition: eventstore.DispositionApplied, Events: evs,
			Result: json.RawMessage(`{"token":"` + string(issued.Token.ID) + `"}`)}, err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	secret, ok := issued.Deliver(res)
	return issued, res, secret, ok
}

// stored reports whether any file of the state directory (the database,
// its WAL included) holds s.
func (f *fixture) stored(s string) bool {
	f.t.Helper()
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

func secretPart(s identity.Secret) string {
	_, v, _ := strings.Cut(s.Reveal(), ".")
	return v
}

// Only the hash reaches the store; the secret is delivered once, from the
// fresh applied commit, and authenticates.
func TestIssuedTokenDeliversOnceAndStoresOnlyTheHash(t *testing.T) {
	f := open(t)
	issued, res, secret, ok := f.issueCommand("k", eventstore.DispositionApplied)
	if !ok || res.Replayed || !strings.HasPrefix(secret.Reveal(), string(issued.Token.ID)+".") {
		t.Fatalf("delivered %v, replayed %v", ok, res.Replayed)
	}
	if f.stored(secretPart(secret)) || !f.stored(issued.Token.Hash) {
		t.Fatal("the store holds the secret, or not the hash")
	}
	if a, err := f.authenticate(secret.Reveal()); err != nil || a.ID != f.op.ID {
		t.Fatalf("authenticate: %v", err)
	}
	// A retry under the same key replays: no delivery, and the retry's
	// own mint is neither recorded nor delivered.
	again, res2, secret2, ok2 := f.issueCommand("k", eventstore.DispositionApplied)
	if ok2 || secret2.Reveal() != "" || !res2.Replayed || res2.CommandID != res.CommandID {
		t.Fatalf("replay delivered %v, replayed %v", ok2, res2.Replayed)
	}
	if f.projected(identity.TokenProjection, string(again.Token.ID)) || f.stored(again.Token.Hash) {
		t.Fatal("a replay recorded a new token")
	}
}

// A command that does not apply delivers nothing, live or replayed.
func TestIssuedTokenIsNotDeliveredUnlessApplied(t *testing.T) {
	for _, d := range []eventstore.Disposition{eventstore.DispositionRefused, eventstore.DispositionProposed} {
		t.Run(string(d), func(t *testing.T) {
			f := open(t)
			for _, replay := range []bool{false, true} {
				issued, res, secret, ok := f.issueCommand("k", d)
				if ok || secret.Reveal() != "" || res.Replayed != replay || res.Disposition != d {
					t.Fatalf("replay %v: delivered %v, %+v", replay, ok, res)
				}
				if f.projected(identity.TokenProjection, string(issued.Token.ID)) || f.stored(issued.Token.Hash) {
					t.Fatal("a token was recorded")
				}
			}
		})
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
