// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// checkStateDir refuses a state directory that is not a real directory
// owned by this user with mode 0700. A missing one passes before the lock
// creates it; Start checks again once the lock is held.
func checkStateDir(dir string, mayBeMissing bool) error {
	var st syscall.Stat_t
	err := syscall.Lstat(dir, &st)
	if errors.Is(err, fs.ErrNotExist) && mayBeMissing {
		return nil
	}
	if err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || int(st.Uid) != os.Geteuid() || st.Mode&0o777 != 0o700 {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the state directory must be a directory owned by this user with mode 0700", err)
	}
	return nil
}

// errNoStore refuses an offline command on a state directory without an
// initialized store.
var errNoStore = couldNotRun(vocab.ReasonStoreUnreadable, "the state directory holds no initialized store; an offline command never creates one", nil)

// existingStore checks, under the lock and before anything is opened for
// writing, that the state directory holds an initialized store, verified
// read-only on a private copy: an offline command never creates the
// database, its schema or genesis, or the blob directories. The store is
// then opened with eventstore.Config.Existing, so a database removed in
// between is refused, never recreated. One window remains: blobstore.Open
// recreates, empty, a blob directory removed in between by a process of
// the daemon uid, which can change the state directory at will anyway.
func existingStore(ctx context.Context, lock *eventstore.Lock, trusted *eventstore.Head) error {
	_, err := eventstore.VerifyExisting(ctx, lock, trusted)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errNoStore
	case errors.Is(err, eventstore.ErrChainBroken):
		return &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonLogIntegrityBroken, Detail: "the event log fails verification; adc dev verify-log reports the finding", cause: err}
	case err != nil:
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event log cannot be read for verification", err)
	}
	for _, d := range []string{"blobs", "blobs/sha256", "blobs/tmp"} {
		if st, err := os.Lstat(filepath.Join(lock.Dir(), d)); err != nil || !st.IsDir() {
			return errNoStore
		}
	}
	return nil
}

// checkCapacityHistory runs capacity.CheckHistory on the existing
// projections, before anything is replayed: a lease granted before
// capacity was wired is refused, never read as one capacity admitted; a
// projection that cannot be read or decoded is store_unreadable.
func checkCapacityHistory(ctx context.Context, s *eventstore.Store) error {
	err := s.View(ctx, capacity.CheckHistory)
	switch {
	case errors.Is(err, capacity.ErrGrantedBeforeWiring):
		return couldNotRun(vocab.ReasonAmbiguousState, "a lease was granted before capacity was wired; capacity cannot account for it", err)
	case err != nil:
		return couldNotRun(vocab.ReasonStoreUnreadable, "the projections cannot be read", err)
	}
	return nil
}

// checkKinds refuses a log holding an event kind or version this build does
// not read: a projector would skip the event, which is never safe (03 rule
// 4). Nothing is ignored and nothing is repaired.
func checkKinds(ctx context.Context, s *eventstore.Store) error {
	unread, err := s.UnreadKinds(ctx)
	if err != nil {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event log cannot be read for its event kinds", err)
	}
	if len(unread) > 0 {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event log holds an event kind or version this build does not read", nil)
	}
	return nil
}

// checkProjections replays the log through the production projector into
// scratch tables and compares them with the persisted projections
// (eventstore.RebuildProjections, which always rolls back). Any difference,
// or a replay the projector refuses, is ambiguous_state; a replay that
// cannot run (storage, cancellation) is store_unreadable. Startup never
// repairs a projection, and one it has not matched is never trusted.
func checkProjections(ctx context.Context, s *eventstore.Store) error {
	diffs, err := s.RebuildProjections(ctx)
	switch {
	case errors.Is(err, eventstore.ErrChainBroken):
		return &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonLogIntegrityBroken, Detail: "the event log fails verification; adc dev verify-log reports the finding", cause: err}
	case errors.Is(err, eventstore.ErrReplayRefused):
		return couldNotRun(vocab.ReasonAmbiguousState, "the event log does not replay into the projections", err)
	case err != nil:
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event log cannot be replayed", err)
	}
	if len(diffs) > 0 {
		return couldNotRun(vocab.ReasonAmbiguousState, "the persisted projections differ from a replay of the event log", nil)
	}
	return nil
}

// markPending is the restart rule's transition; a test makes it fail.
var markPending = (*eventstore.Store).MarkPendingUncertain

// systemActor loads the persisted system actor, or creates it in a store
// whose log is still empty, through the store's one unauthorized
// operation (Bootstrap), which only ever writes an empty log's first event.
// No request reaches it and nothing outside Start names the identity. A
// store with actors but not exactly one system actor, or with events but
// no actor, is refused, and so is an empty one unless create. created
// reports a store this start bootstrapped.
func systemActor(ctx context.Context, s *eventstore.Store, create bool) (_ types.ActorID, created bool, err error) {
	var system []types.ActorID
	actors := 0
	if err := s.View(ctx, func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(identity.ActorProjection, "")
		for _, k := range keys {
			var a types.Actor
			raw, _, err := tx.Get(identity.ActorProjection, k)
			if err == nil {
				err = json.Unmarshal(raw, &a)
			}
			if err != nil {
				return err
			}
			if actors++; a.Kind == types.ActorKindSystem {
				system = append(system, a.ID)
			}
		}
		return err
	}); err != nil {
		return "", false, couldNotRun(vocab.ReasonStoreUnreadable, "the identity projection cannot be read", err)
	}
	switch {
	case len(system) == 1:
		return system[0], false, nil
	case actors > 0:
		return "", false, couldNotRun(vocab.ReasonAmbiguousState, "the store holds actors but not exactly one system actor", nil)
	case !create:
		return "", false, couldNotRun(vocab.ReasonAmbiguousState, "the store has no system actor; an offline command runs only on an existing store", nil)
	}
	id, ierr := ids.New("act")
	cmd, cerr := ids.New("cmd")
	a := types.Actor{ID: types.ActorID(id), Kind: types.ActorKindSystem, Name: "system", CreatedBy: types.ActorID(id)}
	evs, eerr := identity.CreateActor(a)
	if err = errors.Join(ierr, cerr, eerr); err == nil {
		_, err = s.Bootstrap(ctx, eventstore.Command{ID: cmd, Actor: id, Action: "actor.create_system", IdempotencyKey: cmd, Input: []byte(`{}`)}, evs[0])
	}
	if errors.Is(err, eventstore.ErrNotBootstrap) {
		return "", false, couldNotRun(vocab.ReasonAmbiguousState, "the store holds events or effects but no actor", err)
	}
	if err != nil {
		return "", false, couldNotRun(vocab.ReasonStoreUnreadable, "the system actor cannot be recorded", err)
	}
	return a.ID, true, nil
}
