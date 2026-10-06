// SPDX-License-Identifier: Apache-2.0

// Package daemon assembles the controller daemon, adcd (docs/design/m0/03,
// 08): the state-directory lock, the blob and event stores, the writer's
// hooks and the startup checks, in the order the design requires. It opens
// no listener yet, and the writer refuses every new command.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Daemon is a started controller: it holds the lock and the store until Close.
type Daemon struct {
	lock     *eventstore.Lock
	store    *eventstore.Store
	head     eventstore.Head    // verified at start
	system   identity.Principal // the system actor's, held only here
	once     sync.Once
	closeErr error
}

// Start brings the daemon up (03): validate the configuration and the
// state directory; take the lock before the store is opened; verify the
// full hash chain, against the trusted head if one is given; run
// capacity.CheckHistory on the existing projections; refuse event kinds
// this build does not read; replay the log and refuse projections that
// differ from the replay (nothing is repaired); load or create the
// system actor; mark pending effects uncertain, unless this start created
// the store. A refusal releases what
// was taken and is a *StartError (a configuration refusal, ErrConfig).
func Start(ctx context.Context, cfg Config) (d *Daemon, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := checkStateDir(cfg.StateDir, true); err != nil {
		return nil, err
	}
	reservations, err := capacity.NewReservations(cfg.Groups)
	if err != nil {
		return nil, configError("the resource groups break a capacity rule or use one not wired yet")
	}
	reg, err := vocab.Load()
	kinds, kerr := eventKinds()
	if err = errors.Join(err, kerr); err != nil {
		return nil, couldNotRun(vocab.ReasonCheckUnloadable, "the vocabulary or the event kinds cannot be loaded", err)
	}
	lock, err := eventstore.AcquireLock(cfg.StateDir)
	if errors.Is(err, eventstore.ErrAlreadyRunning) {
		return nil, couldNotRun(vocab.ReasonControllerAlreadyRunning, "another adcd holds the state directory lock", err)
	} else if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the state directory lock cannot be taken", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Release())
		}
	}()
	if err := checkStateDir(lock.Dir(), false); err != nil {
		return nil, err
	}
	blobs, err := blobstore.Open(lock.Dir())
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the blob store cannot be opened", err)
	}
	store, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: hooks(reg, projector(blobs, reg.Holds, reservations))})
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the event store cannot be opened", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	head, err := store.Verify(ctx, cfg.TrustedHead)
	if errors.Is(err, eventstore.ErrChainBroken) {
		return nil, &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonLogIntegrityBroken, Detail: "the event log fails verification; adc dev verify-log reports the finding", cause: err}
	} else if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the event log cannot be read for verification", err)
	}
	// Capacity history is checked on the existing projections before any
	// replay could manufacture it; then the log's kinds and a full replay.
	for _, check := range []func(context.Context, *eventstore.Store) error{checkCapacityHistory, checkKinds, checkProjections} {
		if err := check(ctx, store); err != nil {
			return nil, err
		}
	}
	sys, created, err := systemActor(ctx, store)
	if err != nil {
		return nil, err
	}
	// A store this start bootstrapped holds one event and no effect, so it
	// skips the restart transition: a fresh start that appends is never
	// refused after.
	if !created {
		if _, err := markPending(store, ctx, string(sys)); err != nil {
			return nil, couldNotRun(vocab.ReasonStoreUnreadable, "pending effects cannot be marked uncertain", err)
		}
	}
	var system identity.Principal
	if err := store.View(ctx, func(tx *eventstore.Tx) (err error) {
		system, err = identity.SystemPrincipal(tx, sys)
		return err
	}); err != nil {
		return nil, couldNotRun(vocab.ReasonAmbiguousState, "the system actor is not live", err)
	}
	return &Daemon{lock: lock, store: store, head: head, system: system}, nil
}

// Head is the event log's head verified at start.
func (d *Daemon) Head() eventstore.Head { return d.head }

// Close closes the event store and then releases the lock, once.
func (d *Daemon) Close() error {
	d.once.Do(func() { d.closeErr = errors.Join(d.store.Close(), d.lock.Release()) })
	return d.closeErr
}

// Run starts the daemon and blocks until ctx is done, then closes it.
// ready is the readiness point: it is called once every startup check has
// passed, and a listener starts from it, never before. logw
// never receives a path or a secret.
func Run(ctx context.Context, cfg Config, logw io.Writer, ready func(*Daemon)) error {
	d, err := Start(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(logw, "adcd: started; event log verified to seq %d; no listener in this build\n", d.head.Seq)
	if ready != nil {
		ready(d)
	}
	<-ctx.Done()
	if err := d.Close(); err != nil {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event store did not close cleanly", err)
	}
	fmt.Fprintln(logw, "adcd: stopped")
	return nil
}
