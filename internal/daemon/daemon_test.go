// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// The lock is free again: a daemon that stopped or was refused holds nothing.
func lockFree(t *testing.T, dir string) {
	t.Helper()
	l, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatalf("the state directory lock is still held: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	cfg := Config{StateDir: stateDir(t)}
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Start(context.Background(), cfg)
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonControllerAlreadyRunning)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a daemon after a clean close was refused: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// A store with a lease granted before capacity was wired is refused, and
// the refused start releases the lock.
func TestPreCapacityLeaseRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, leasesOnly)
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	_, err := Start(context.Background(), Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}})
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonAmbiguousState)
	if !strings.Contains(err.Error(), "before capacity was wired") {
		t.Fatalf("refused for another reason: %v", err)
	}
	lockFree(t, dir)
}

func TestBrokenChainRefused(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, eventstore.DBFile)}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TRIGGER events_append_only_update`, `UPDATE events SET at = '2020-01-01T00:00:00Z' WHERE seq = 1`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Start(context.Background(), Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}})
	startErr(t, err, vocab.Failed, vocab.ReasonLogIntegrityBroken)
	if !errors.Is(err, eventstore.ErrChainBroken) {
		t.Fatalf("the cause is not kept: %v", err)
	}
	lockFree(t, dir)
}

// A history written through the daemon's projector (a supervisor and its
// grant in one command, a granted lease, a usage report) rebuilds to the
// same projections, and the daemon starts on it and refuses commands.
func TestProjectorOrderMatchesRebuild(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	sys := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	sys.CreatedBy = sys.ID
	op := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: sys.ID}
	sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: op.ID}
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(sys) })
	w.do(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(op) })
	now := time.Now().UTC()
	g := types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{"prj_01m3250vkwffc979nzjw0g2sa1"}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: []types.Name{}},
		ValidFrom: types.Time(now.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(now.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: op.ID, Approval: types.ApprovalID(newID(t, "apr"))}
	// The grantee and its grant in one command: admitted only if each event
	// is projected through every projector before the next one.
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		a, err := identity.CreateActor(sup)
		if err != nil {
			return nil, err
		}
		gr, err := authority.CreateGrant(g)
		return append(a, gr...), err
	})
	w.grantedLease(op.ID, 4)
	m := types.UsageMeasurement{ID: types.MeasurementID(newID(t, "msr")), Call: types.CallID(newID(t, "cal")), Attempt: types.AttemptID(newID(t, "att")),
		Lease: types.LeaseID(newID(t, "lea")), Kind: types.UsageMeasurementKindDelta, Sequence: 1,
		Tokens: types.UsageMeasurementTokens{In: 10, Out: 1}, Turns: 1, Milliseconds: 20,
		Cost:          types.UsageMeasurementCost{AmountMicros: 30, Currency: "USD", Basis: types.UsageMeasurementCostBasisCash, Estimate: true},
		SourceOfTruth: types.UsageMeasurementSourceOfTruthAdapterReport}
	w.do(op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		e, err := ledger.Measure(m)
		return []eventstore.Event{e}, err
	})
	diffs, err := w.s.RebuildProjections(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("the daemon's projector does not rebuild: %v %v", diffs, err)
	}
	w.done()

	d, err := Start(context.Background(), Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Head().Seq == 0 {
		t.Fatal("the verified head is empty")
	}
	cmd := newID(t, "cmd")
	_, err = d.store.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: string(op.ID), IdempotencyKey: cmd, Input: []byte(`{}`)},
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return nil, json.RawMessage(`{}`), nil
		})
	if !errors.Is(err, ErrCommandsRefused) {
		t.Fatalf("a command was not refused: %v", err)
	}
}

// logBuffer is a log writer the test reads while Run writes it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCleanShutdownReleasesLock(t *testing.T) {
	dir := stateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	logs := &logBuffer{}
	done := make(chan error, 1)
	ready := make(chan *Daemon, 1)
	go func() { done <- Run(ctx, Config{StateDir: dir}, logs, func(d *Daemon) { ready <- d }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the daemon did not start: %v", err)
	}
	// While it runs, a second daemon is refused.
	_, err := Start(context.Background(), Config{StateDir: dir})
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonControllerAlreadyRunning)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lockFree(t, dir)
	if out := logs.String(); !strings.Contains(out, "adcd: stopped") || strings.Contains(out, dir) {
		t.Fatalf("log: %q", out)
	}
}
