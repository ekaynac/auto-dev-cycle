// SPDX-License-Identifier: Apache-2.0

// Package daemon assembles the controller daemon, adcd (docs/design/m0/03,
// 08): the state-directory lock, the blob and event stores, the writer's
// hooks and the startup checks, in the order the design requires; then,
// at the readiness point, the API's Unix sockets. The writer refuses every
// new command.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/dispatch"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Daemon is a started controller: it holds the lock and the store until Close.
type Daemon struct {
	lock     *eventstore.Lock
	store    *eventstore.Store
	blobs    *blobstore.Store
	captures *blobstore.Store // the egress captures' (api.PurgeCaptures)
	// capturing is held while a capture is kept and while captures are
	// purged, so a purge never removes bytes a capture is being kept on.
	// An exit run's capture waits for a whole purge: one commit for each
	// capture expired since the last purge (captures live 7 days, and the
	// purge runs every period), then one walk of the capture store, whose
	// files are the captures of the last 7 days and unnamed bytes younger
	// than api.CaptureGrace (Mythos #359 L2, Opus #322 r2 L4). A capture
	// itself holds it for at most its write and captureRecordBound.
	capturing sync.Mutex
	head      eventstore.Head    // verified at start
	system    identity.Principal // the system actor's, held only here
	boot      *bootstrapper
	clock     func() time.Time // the store clock (eventstore.Config.Now), for what is made outside a command
	note      string           // the bootstrap's startup note, fixed text
	az        authority.Authorizer
	gates     reconcile.Gates // the reconcile loop's, derived at Run's start (review's: reviewsAtStart)
	once      sync.Once
	closeErr  error
}

// Start brings the daemon up (03): validate the configuration and the
// state directory; take the lock before the store is opened; verify the
// full hash chain, against the trusted head if one is given; run
// capacity.CheckHistory and graph.CheckHistory on the existing
// projections; refuse event kinds
// this build does not read; replay the log and refuse projections that
// differ from the replay (nothing is repaired); load or create the
// system actor; mark pending effects uncertain (every start, #226 entry
// 64); run restart recovery (recoverAtStart). A refusal releases what
// was taken and is a *StartError (a configuration refusal, ErrConfig).
func Start(ctx context.Context, cfg Config) (d *Daemon, err error) { return start(ctx, cfg, false) }

// start is Start; offline (a recovery command, 14 R7, R8) runs every check
// on an existing store and writes nothing of its own: no system actor is
// created, no pending effect marked, no bootstrap secret issued.
func start(ctx context.Context, cfg Config, offline bool) (d *Daemon, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := checkStateDir(cfg.StateDir, !offline); err != nil {
		return nil, err
	}
	reservations, err := capacity.NewReservations(cfg.Groups)
	if err != nil {
		return nil, configError("the resource groups break a capacity rule or use one not wired yet")
	}
	reg, err := vocab.Load()
	kinds, kerr := eventKinds()
	effects, eerr := effectKinds()
	if err = errors.Join(err, kerr, eerr); err != nil {
		return nil, couldNotRun(vocab.ReasonCheckUnloadable, "the vocabulary, the event kinds or the effect kinds cannot be loaded", err)
	}
	acquire := eventstore.AcquireLock
	if offline {
		acquire = eventstore.ProbeLock // creates no lock file
	}
	lock, err := acquire(cfg.StateDir)
	if errors.Is(err, eventstore.ErrAlreadyRunning) {
		return nil, couldNotRun(vocab.ReasonControllerAlreadyRunning, "another adcd holds the state directory lock", err)
	} else if offline && errors.Is(err, fs.ErrNotExist) {
		return nil, errNoStore
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
	if offline {
		if err := existingStore(ctx, lock, cfg.TrustedHead); err != nil {
			return nil, err
		}
	}
	blobs, err := blobstore.Open(lock.Dir())
	var captures *blobstore.Store // a store of its own: a capture's purge removes bytes no other record shares
	if err == nil {
		captures, err = blobstore.Open(filepath.Join(lock.Dir(), "captures"))
	}
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the blob store cannot be opened", err)
	}
	clock := cfg.clock
	if clock == nil {
		clock = time.Now
	}
	az := authorizer(reg)
	store, err := eventstore.Open(ctx, lock, eventstore.Config{Kinds: kinds, Effects: effects, Blobs: blobs, Existing: offline, Now: clock, Projections: projections,
		Hooks: hooks(az, projector(blobs, reg.Holds, reservations))})
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the event store cannot be opened", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	// The system claim is taken first, before the store reaches anyone
	// else: only this start ever holds it (14 N4).
	claim, err := store.ClaimSystem()
	if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the system claim cannot be taken", err)
	}
	head, err := store.Verify(ctx, cfg.TrustedHead)
	if errors.Is(err, eventstore.ErrChainBroken) {
		return nil, &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonLogIntegrityBroken, Detail: "the event log fails verification; adc dev verify-log reports the finding", cause: err}
	} else if err != nil {
		return nil, couldNotRun(vocab.ReasonStoreUnreadable, "the event log cannot be read for verification", err)
	}
	// Capacity history and groups are checked on the existing projections
	// before any replay could manufacture them; then the log's kinds and a
	// full replay.
	for _, check := range []func(context.Context, *eventstore.Store) error{checkCapacityHistory, checkGraphHistory, checkCapacityGroups(reservations), checkKinds, checkProjections} {
		if err := check(ctx, store); err != nil {
			return nil, err
		}
	}
	sys, err := systemActor(ctx, store, !offline)
	if err != nil {
		return nil, err
	}
	// The restart rule runs at every non-offline start, a store this start created
	// included (it marks nothing there): the model calls' Recorder refuses a
	// store where it has not run (#226 entry 64). Then restart recovery
	// (P21a, X3 steps 1-4), before the writer admits any other command
	// (#226 entry 81).
	if !offline {
		if _, err := markPending(store, ctx, string(sys)); err != nil {
			return nil, couldNotRun(vocab.ReasonStoreUnreadable, "pending effects cannot be marked uncertain", err)
		}
	}
	var system identity.Principal
	if err := store.View(ctx, func(tx *eventstore.Tx) (err error) {
		system, err = identity.SystemPrincipal(tx, claim, sys)
		return err
	}); err != nil {
		return nil, couldNotRun(vocab.ReasonAmbiguousState, "the system actor is not live", err)
	}
	recovered, err := recoverAtStart(ctx, store, system, blobs, offline)
	if err != nil {
		return nil, err
	}
	boot, note, err := openBootstrap(ctx, recovered, system, lock.Dir(), clock, !offline)
	if err != nil {
		return nil, err
	}
	if recovered.note != "" {
		note = strings.TrimSpace(note + "\n" + recovered.note)
	}
	return &Daemon{lock: lock, store: store, blobs: blobs, captures: captures, head: head, system: system, boot: boot, clock: clock, note: note, az: az}, nil
}

// Head is the event log's head verified at start.
func (d *Daemon) Head() eventstore.Head { return d.head }

// Close closes the event store and then releases the lock, once.
func (d *Daemon) Close() error {
	d.once.Do(func() { d.closeErr = errors.Join(d.boot.dir.Close(), d.store.Close(), d.lock.Release()) })
	return d.closeErr
}

// Run starts the daemon, then at its readiness point (every startup check
// passed) opens the listeners: the private socket and, when configured,
// the shared one and the HTTPS listener, whose certificate and key are
// checked before the store opens. ready is called once they listen. Run
// blocks until ctx is done or a listener fails, then stops the server
// (streams closed, sockets removed) before the store closes. logw never receives a path or a secret.
func Run(ctx context.Context, cfg Config, logw io.Writer, ready func(*Daemon)) error {
	var tc *tls.Config
	if cfg.HTTPS != nil {
		// The certificate and key are read before Start takes the lock, from
		// paths Validate has checked; Start validates again for its own
		// callers.
		err := cfg.Validate()
		if err == nil {
			tc, err = cfg.HTTPS.load(time.Now(), os.Geteuid())
		}
		if err != nil {
			return err
		}
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		return err
	}
	var reg registry.Registry // Validate checked it against the groups
	if cfg.Registry != nil {
		if reg, err = registry.New(*cfg.Registry, cfg.Groups); err != nil {
			_ = d.Close()
			return err
		}
	}
	var images dispatch.Images // the pinned images the launch checks, in the sandbox's storage (none: every launch misses its image)
	if cfg.Sandbox != nil {
		images = *cfg.Sandbox
	}
	if cfg.Sandbox != nil { // one adcd a sandbox root: its start sweep removes every stale run there (Opus 22a-5c-2c M2)
		unlock, err := lockSandbox(cfg.Sandbox.Root)
		if err != nil {
			_ = d.Close()
			return err
		}
		defer unlock()
	}
	eg, err := newEgress(d, reg, cfg.CaptureEgress) // the one gateway (Opus #322 L5)
	var run *runner
	if err == nil {
		run, err = runnerOf(ctx, d, cfg, reg, eg, logw)
	}
	if err != nil {
		_ = d.Close()
		return err
	}
	if run != nil { // before the loop's first Run (sweep.go); its failures never block the start
		if err := run.sweep(ctx); err != nil {
			fmt.Fprintf(logw, "adcd: the start sweep left work for the next start: %v\n", err)
		}
	}
	eg.log = logw
	settling, stopSettle := context.WithTimeout(ctx, settleBound) // after recovery and the sweep, which end the attempts a crash cut (Opus #409 M1); bounded, the rest the passes' (Opus #411 L1)
	eg.settleEnded(settling)
	stopSettle()
	loop := reconciler(d, reg, images, eg, run, cfg.reconcileEvery, reviewsOf(reg, logw), logw) // its passes settle too
	d.gates = loop.Gates
	srv, err := serve(d.store, d.boot, d.lock.Dir(), cfg.Shared, defaultLimits, commands{store: d.store, reg: d.az.Registry, logic: logics(d.az), issue: issuers(d.az), request: requests(d.az, d.blobs), log: logw,
		blobs: d.blobs, clock: d.clock, probe: cfg.Sandbox, authenticated: cfg.authenticated, applied: loop.Nudge})
	if err == nil && tc != nil {
		if err = srv.listenHTTPS(d.store, cfg.HTTPS.Addr, tc); err != nil {
			_ = srv.close() // the refusal is err
		}
	}
	if err != nil {
		if cerr := d.Close(); cerr != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the event store did not close cleanly", errors.Join(err, cerr))
		}
		return err
	}
	fmt.Fprintf(logw, "adcd: started; event log verified to seq %d; listening on %d socket(s)\n", d.head.Seq, len(srv.sockets))
	if srv.tcp != nil {
		fmt.Fprintln(logw, "adcd: the HTTPS listener is open")
	}
	if d.note != "" {
		fmt.Fprintln(logw, d.note)
	}
	if ready != nil {
		ready(d)
	}
	purging, stopPurge := context.WithCancel(ctx)
	purged := make(chan struct{})
	go func() { defer close(purged); d.purgeUploads(purging, purgeEvery, logw) }()
	reconciled := make(chan struct{})
	go func() { defer close(reconciled); loop.Run(purging) }() // resynchronises now, then every reconcileEvery and on a nudge
	var failed error
	select {
	case <-ctx.Done():
	case failed = <-srv.failed:
	}
	stopPurge()
	<-purged
	<-reconciled
	eg.wait() // a settlement the last pass started, its context ended
	if run != nil {
		run.Wait() // ctx ended each run's conversation; its completion records under a context of its own (WithoutCancel)
	}
	serr := srv.close()
	if err := d.Close(); err != nil {
		return couldNotRun(vocab.ReasonStoreUnreadable, "the event store did not close cleanly", err)
	}
	if err := errors.Join(failed, serr); err != nil {
		return fmt.Errorf("adcd: the server stopped on an error: %w", err)
	}
	fmt.Fprintln(logw, "adcd: stopped")
	return nil
}

// purgeEvery is how often expired uploads and captures are purged.
const purgeEvery = time.Minute

// purgeUploads purges expired uploads and egress captures now and then
// every period until ctx ends; a failure is logged and retried at the next
// period.
func (d *Daemon) purgeUploads(ctx context.Context, period time.Duration, logw io.Writer) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		if _, err := api.PurgeExpired(ctx, d.store, d.blobs, d.system, d.clock(), logw); err != nil && ctx.Err() == nil {
			fmt.Fprintf(logw, "adcd: purging expired uploads: %v\n", err)
		}
		d.capturing.Lock()
		_, err := api.PurgeCaptures(ctx, d.store, d.captures, d.system, d.clock(), nil)
		d.capturing.Unlock()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(logw, "adcd: purging expired captures: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
