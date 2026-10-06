// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
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

// checkCapacityHistory runs capacity.CheckHistory on the existing
// projections, before anything is replayed: a lease granted before
// capacity was wired is refused, never read as one capacity admitted.
func checkCapacityHistory(ctx context.Context, s *eventstore.Store) error {
	var refusal error
	if err := s.View(ctx, func(tx *eventstore.Tx) error { refusal = capacity.CheckHistory(tx); return nil }); err != nil {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the projections cannot be read", err)
	}
	if refusal != nil {
		return couldNotRun(vocab.ReasonAmbiguousState, "a lease was granted before capacity was wired; capacity cannot account for it", refusal)
	}
	return nil
}

// markPending is the restart rule's transition; a test makes it fail.
var markPending = (*eventstore.Store).MarkPendingUncertain

// systemActor loads the persisted system actor, or creates it in a store
// with no actor yet, through the trusted startup path (SubmitTrusted): no
// request reaches it and nothing outside Start names the identity. Actors
// without exactly one system actor are refused.
func systemActor(ctx context.Context, s *eventstore.Store) (types.ActorID, error) {
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
		return "", couldNotRun(vocab.ReasonStoreUnreadable, "the identity projection cannot be read", err)
	}
	switch {
	case len(system) == 1:
		return system[0], nil
	case actors > 0:
		return "", couldNotRun(vocab.ReasonAmbiguousState, "the store holds actors but not exactly one system actor", nil)
	}
	id, err := ids.New("act")
	cmd, cerr := ids.New("cmd")
	a := types.Actor{ID: types.ActorID(id), Kind: types.ActorKindSystem, Name: "system", CreatedBy: types.ActorID(id)}
	if err = errors.Join(err, cerr); err == nil {
		_, err = s.SubmitTrusted(ctx, eventstore.Command{ID: cmd, Actor: id, IdempotencyKey: cmd, Input: []byte(`{}`)},
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				evs, err := identity.CreateActor(a)
				return evs, json.RawMessage(`{}`), err
			})
	}
	if err != nil {
		return "", couldNotRun(vocab.ReasonStoreUnreadable, "the system actor cannot be recorded", err)
	}
	return a.ID, nil
}
