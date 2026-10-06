// SPDX-License-Identifier: Apache-2.0

package eventstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

var errRefused = errors.New("refused")

// bootstrapStore is a store whose Authorize hook refuses everything.
func bootstrapStore(t *testing.T) *eventstore.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: identity.Kinds(), Blobs: blobs,
		Hooks: eventstore.Hooks{Authorize: func(*eventstore.Tx, eventstore.Command) error { return errRefused },
			Project: identity.Projector{}.Project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); lock.Release() })
	return s
}

func selfCreated(t *testing.T, kind types.ActorKind) (eventstore.Command, eventstore.Event) {
	t.Helper()
	a := types.Actor{ID: types.ActorID(id(t, "act")), Kind: kind, Name: string(kind)}
	a.CreatedBy = a.ID
	evs, err := identity.CreateActor(a)
	if err != nil {
		t.Fatal(err)
	}
	cmd := id(t, "cmd")
	return eventstore.Command{ID: cmd, Actor: string(a.ID), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, evs[0]
}

// Bootstrap writes an empty store's first event, by its own subject, past
// a refusing Authorize hook, and nothing else: not a second event, not an
// event about another subject, and not what the projectors refuse.
func TestBootstrapOnlyFirstEvent(t *testing.T) {
	ctx := context.Background()
	s := bootstrapStore(t)
	c, e := selfCreated(t, types.ActorKindSystem)
	other := e
	other.Subject = id(t, "act")
	if _, err := s.Bootstrap(ctx, c, other); !errors.Is(err, eventstore.ErrNotBootstrap) {
		t.Fatalf("an event about another subject was bootstrapped: %v", err)
	}
	// The identity projector still decides: an operator does not create itself.
	c2, e2 := selfCreated(t, types.ActorKindOperator)
	if _, err := s.Bootstrap(ctx, c2, e2); err == nil {
		t.Fatal("a self-created operator was admitted")
	}
	out, err := s.Bootstrap(ctx, c, e)
	if err != nil || out.FirstSeq != 1 || out.LastSeq != 1 {
		t.Fatalf("the first event was refused: %+v %v", out, err)
	}
	c3, e3 := selfCreated(t, types.ActorKindSystem)
	if _, err := s.Bootstrap(ctx, c3, e3); !errors.Is(err, eventstore.ErrNotBootstrap) {
		t.Fatalf("a second bootstrap was admitted: %v", err)
	}
	// Ordinary submission still goes through the Authorize hook.
	cmd := id(t, "cmd")
	if _, err := s.SubmitApplied(ctx, eventstore.Command{ID: cmd, Actor: c.Actor, Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return nil, json.RawMessage(`{}`), nil
		})); !errors.Is(err, errRefused) {
		t.Fatalf("Submit skipped the Authorize hook: %v", err)
	}
	if h, err := s.Head(ctx); err != nil || h.Seq != 1 {
		t.Fatalf("head %+v %v", h, err)
	}
}
