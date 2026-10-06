// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// refused asserts the start is refused with reason and a detail containing
// want, and that the refusal released the lock.
func refused(t *testing.T, dir string, cfg Config, outcome vocab.Outcome, reason vocab.Reason, want string) {
	t.Helper()
	_, err := Start(context.Background(), cfg)
	startErr(t, err, outcome, reason)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refused for another reason: %v", err)
	}
	lockFree(t, dir)
}

// lastSeq is the closed store's last event seq.
func lastSeq(t *testing.T, dir string) (seq int64) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func leaseStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.grantedLease(sys.ID, 4)
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
}

// execSQL changes the closed store's database behind the store's back.
func execSQL(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// A trusted head that is not a non-negative seq and a sha256 hash is
// refused by a direct Start too, before the state directory is created.
func TestStartValidatesTrustedHead(t *testing.T) {
	for _, h := range []eventstore.Head{{Seq: -1, Hash: "sha256:" + strings.Repeat("a", 64)}, {Seq: 0, Hash: "invalid"}} {
		dir := stateDir(t)
		if _, err := Start(context.Background(), Config{StateDir: dir, TrustedHead: &h}); !errors.Is(err, ErrConfig) {
			t.Fatalf("trusted head %+v accepted: %v", h, err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state directory was created: %v", err)
		}
	}
}

// A lease projection that cannot be decoded is store_unreadable, never
// reported as a grant from before capacity was wired.
func TestMalformedLeaseProjectionUnreadable(t *testing.T) {
	dir, cfg := leaseStore(t)
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('`+lease.LeaseProjection+`', 'lea_malformed', '{"record":1}')`)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "projections cannot be read")
}

// A store with events but no actor gets no system actor: Bootstrap writes
// only an empty store's first event.
func TestEventsWithoutActorRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t))
	w.do(types.ActorID(newID(t, "act")), func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		e, err := ledger.Measure(types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: types.CallID(newID(t, "cal")),
			Attempt: types.AttemptID(newID(t, "att")), Lease: types.LeaseID(newID(t, "lea")), Kind: types.UsageMeasurementKindDelta, Sequence: 1,
			Tokens: types.UsageMeasurementTokens{In: 1, Out: 1}, Turns: 1, Milliseconds: 1,
			Cost:          types.UsageMeasurementCost{AmountMicros: 1, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
			SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport})
		return []eventstore.Event{e}, err
	})
	w.done()
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "no actor")
}

// An otherwise empty store holding an orphan effect gets no system actor:
// Bootstrap refuses it, and the refused start appends nothing.
func TestOrphanEffectRefused(t *testing.T) {
	dir := stateDir(t)
	seed(t, dir, daemonProjection(t)).done()
	execSQL(t, dir, `INSERT INTO effects(id, kind, command, target, request_digest, state, attempts, last_observation)
		VALUES ('eff_orphan', 'model_call', 'cmd_orphan', 'target', 'sha256:x', 'pending', 0, '')`)
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "no actor")
	if lastSeq(t, dir) != 0 {
		t.Fatal("a refused start appended to the log")
	}
}

func TestTrustedHeadChecked(t *testing.T) {
	dir, cfg := leaseStore(t)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	head := d.Head()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.TrustedHead = &head
	d, err = Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the trusted head was refused: %v", err)
	}
	d.Close()
	cfg.TrustedHead = &eventstore.Head{Seq: head.Seq + 5, Hash: head.Hash}
	refused(t, dir, cfg, vocab.Failed, vocab.ReasonLogIntegrityBroken, "verification")
}

// A store whose projections match the log starts, and starts again.
func TestCleanStoreStarts(t *testing.T) {
	_, cfg := leaseStore(t)
	for range 2 {
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatalf("a clean store was refused: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A persisted projection that differs from a replay of the log, by a forged,
// a changed or a missing record, is refused before the system actor is
// recorded, and is not repaired.
func TestProjectionDriftRefused(t *testing.T) {
	for name, stmt := range map[string]string{
		"forged":  `INSERT INTO proj_kv(projection, key, value) VALUES ('capacity_reserved', 'forged', '{}')`,
		"changed": `UPDATE proj_kv SET value = '{}' WHERE projection = 'leases'`,
		"missing": `DELETE FROM proj_kv WHERE projection = 'capacity_reserved'`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, cfg := leaseStore(t)
			execSQL(t, dir, stmt)
			head := lastSeq(t, dir)
			for range 2 {
				refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
			}
			if lastSeq(t, dir) != head {
				t.Fatal("a refused start appended to the log")
			}
		})
	}
}

// A persisted event of a kind or version this build does not read is
// refused, not skipped.
func TestUnsupportedEventVersionRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t), "actor.created/v9")
	a := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	a.CreatedBy = a.ID
	w.do(a.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := identity.CreateActor(a)
		for i := range evs {
			evs[i].Kind = "actor.created/v9"
		}
		return evs, err
	})
	w.done()
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "does not read")
}

// CheckHistory runs on the existing projections before any replay: on a
// store with both a lease capacity never admitted and drifted projections,
// its refusal is the one reported.
func TestCapacityHistoryCheckedBeforeReplay(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, leasesOnly)
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	execSQL(t, dir, `INSERT INTO proj_kv(projection, key, value) VALUES ('stray', 'forged', '{}')`)
	refused(t, dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "before capacity was wired")
}

// A failed restart transition refuses the start and appends nothing. A
// fresh store, which this start bootstraps, has no effect to mark: the
// transition is skipped rather than refused after the append.
func TestFailedRestartTransitionRefused(t *testing.T) {
	markPending = func(*eventstore.Store, context.Context, string) (int, error) { return 0, errors.New("injected") }
	t.Cleanup(func() { markPending = (*eventstore.Store).MarkPendingUncertain })
	dir, cfg := leaseStore(t)
	head := lastSeq(t, dir)
	refused(t, dir, cfg, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "marked uncertain")
	if lastSeq(t, dir) != head {
		t.Fatal("a refused start appended to the log")
	}
	fresh := stateDir(t)
	d, err := Start(context.Background(), Config{StateDir: fresh})
	if err != nil {
		if lastSeq(t, fresh) != 0 {
			t.Fatalf("a refused fresh start appended to the log: %v", err)
		}
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// A replay the projector refuses is ambiguous_state; one that cannot run,
// here on a cancelled context, is store_unreadable.
func TestReplayErrorsClassified(t *testing.T) {
	dir := stateDir(t)
	refuse := false
	w := seed(t, dir, func(blobs lease.Blobs, holds map[string]types.Hold) func(*eventstore.Tx, []eventstore.Event) error {
		project := daemonProjection(t, tokens(10))(blobs, holds)
		return func(tx *eventstore.Tx, evs []eventstore.Event) error {
			if refuse {
				return errors.New("injected refusal")
			}
			return project(tx, evs)
		}
	})
	defer w.done()
	fullHistory(t, w)
	refuse = true
	startErr(t, checkProjections(context.Background(), w.s), vocab.CouldNotRun, vocab.ReasonAmbiguousState)
	refuse = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startErr(t, checkProjections(ctx, w.s), vocab.CouldNotRun, vocab.ReasonStoreUnreadable)
}

// fullHistory leaves a record in every projection the daemon composes, the
// waiting lease holding its hold; a change to any of them is drift.
func TestFullHistoryProjected(t *testing.T) {
	dir, _ := fullStore(t)
	held := `(SELECT key FROM proj_kv WHERE projection = '` + lease.LeaseProjection + `' AND value LIKE '%"quota_reset"%')`
	for projection, want := range map[string]int{identity.ActorProjection: 3, authority.GrantProjection: 1, lease.LeaseProjection: 2,
		capacity.ReservedProjection: 1, capacity.GrantedProjection: 1, ledger.UsageProjection: 1, ledger.MeasurementProjection: 1, "held": 1} {
		q := `SELECT count(*) FROM proj_kv WHERE projection = '` + projection + `'`
		if projection == "held" {
			q = `SELECT count(*) FROM ` + held + ``
		}
		if got := count(t, dir, q); got != want {
			t.Errorf("%s: %d records, want %d", projection, got, want)
		}
	}
	for _, projection := range []string{identity.ActorProjection, authority.GrantProjection, ledger.UsageProjection, "held"} {
		t.Run(projection, func(t *testing.T) {
			sub, cfg := fullStore(t)
			key := `(SELECT min(key) FROM proj_kv WHERE projection = '` + projection + `')`
			if projection == "held" {
				projection, key = lease.LeaseProjection, held
			}
			execSQL(t, sub, `UPDATE proj_kv SET value = json_set(value, '$.forged', 1) WHERE projection = '`+projection+`' AND key = `+key)
			refused(t, sub, cfg, vocab.CouldNotRun, vocab.ReasonAmbiguousState, "differ from a replay")
		})
	}
}

func fullStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	fullHistory(t, w)
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
}

func count(t *testing.T, dir, q string) (n int) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStateDirPermissionsRefused(t *testing.T) {
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), Config{StateDir: dir})
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonStoreUnreadable)
	if !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("refused for another reason: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(stateDir(t), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Config{StateDir: link}); err == nil {
		t.Fatal("a symlinked state directory was accepted")
	}
}

// A fresh store gets one persisted system actor, which later starts load.
func TestSystemActorPersisted(t *testing.T) {
	cfg := Config{StateDir: stateDir(t)}
	var heads []int64
	for range 2 {
		d, err := Start(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, d.Head().Seq)
		var system int
		if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
			keys, err := tx.Keys(identity.ActorProjection, "")
			system = len(keys)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if system != 1 {
			t.Fatalf("%d actors after start", system)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if heads[0] != 0 || heads[1] != 1 {
		t.Fatalf("the system actor was not created once: heads %v", heads)
	}
}

// The system actor is created through the trusted startup path; every
// ordinary submission after it, even one naming the system actor, is still
// refused.
func TestRefuseAllAfterSystemActor(t *testing.T) {
	d, err := Start(context.Background(), Config{StateDir: stateDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var sys string
	if err := d.store.View(context.Background(), func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(identity.ActorProjection, "")
		if len(keys) == 1 {
			sys = keys[0]
		}
		return err
	}); err != nil || sys == "" {
		t.Fatalf("no system actor after start: %v", err)
	}
	// Without a principal nobody gets past the identity recheck; the
	// system principal does, and no action is wired for it.
	noop := eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		return nil, json.RawMessage(`{}`), nil
	})
	for _, actor := range []string{sys, newID(t, "act")} {
		cmd := newID(t, "cmd")
		_, err := d.store.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: actor, Action: "test.command", IdempotencyKey: "adcd-system-actor", Input: []byte(`{}`)}, noop)
		if !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("a submission as %s without a principal was not refused: %v", actor, err)
		}
	}
	// Startup took the store's one system claim: nothing after it can.
	if _, err := d.store.ClaimSystem(); !errors.Is(err, eventstore.ErrSystemClaimed) {
		t.Fatalf("the system claim is still free after start: %v", err)
	}
	if d.system.Actor() != types.ActorID(sys) {
		t.Fatalf("the daemon holds %s's principal, not the system actor's", d.system.Actor())
	}
	cmd := newID(t, "cmd")
	if _, err := d.store.SubmitApplied(context.Background(), d.system.Command(cmd, "note.add", cmd, []byte(`{}`), nil), noop); !errors.Is(err, ErrCommandsRefused) {
		t.Fatalf("the system principal's command was not refused: %v", err)
	}
}
