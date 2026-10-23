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

	_ "modernc.org/sqlite"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
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

// A history written through the daemon's projector (fullHistory) rebuilds
// to the
// same projections, and the daemon starts on it and refuses commands.
func TestProjectorOrderMatchesRebuild(t *testing.T) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	op := fullHistory(t, w)
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
	_, err = d.store.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(op), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return nil, json.RawMessage(`{}`), nil
		}))
	if !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("a command without a principal was not refused: %v", err)
	}
	_, err = d.store.SubmitApplied(context.Background(), d.system.Command(cmd, "review.request", cmd, []byte(`{}`), nil), eventstore.Applies(
		func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			return nil, json.RawMessage(`{}`), nil
		}))
	if !errors.Is(err, ErrCommandsRefused) {
		t.Fatalf("a command was not refused: %v", err)
	}
	// The seat and admin actions are wired: seat.take reaches its logic, which
	// refuses the system actor (not a supervisor) with a recorded reason.
	if hs := handlers(); len(hs) != 18 || hs[authority.TaskRaiseRailAction] == nil || hs[authority.InputAnswerAction] == nil || hs[authority.ProjectCreateAction] == nil || hs[authority.TaskCreateAction] == nil || hs[authority.TaskCancelAction] == nil || hs[authority.TokenIssueAction] == nil || hs[authority.TokenRevokeAction] == nil || hs[authority.TokenReplaceAction] == nil || hs[authority.ConfirmAction] == nil || hs["seat.take"] == nil || hs["seat.release"] == nil || hs[authority.EnrolAction] == nil || hs[authority.RevokeActorAction] == nil || hs[authority.WithdrawAction] == nil || hs[authority.NoteAction] == nil ||
		hs[authority.GrantCreateAction] == nil || hs[authority.GrantRevokeAction] == nil || hs[authority.GrantExtendAction] == nil {
		t.Fatalf("the wired handlers: %v", hs)
	}
	take := newID(t, "cmd")
	c := d.system.Command(take, "seat.take", take, []byte(`{}`), nil)
	out, err := d.store.Submit(context.Background(), c, handlers()["seat.take"].(authority.SeatHandler).Logic(c))
	if err != nil || out.Disposition != eventstore.DispositionRefused || !strings.Contains(string(out.Result), "seat_unavailable") {
		t.Fatalf("seat.take is not wired: %+v %v", out, err)
	}
	// proposal.withdraw is wired: its handler derives the proposal, and an
	// unknown one is refused as not open, not as an unwired action.
	w2 := newID(t, "cmd")
	c = d.system.Command(w2, authority.WithdrawAction, w2, []byte(`{"proposal":"`+newID(t, "prp")+`"}`), nil)
	_, err = d.store.Submit(context.Background(), c, authority.Authorizer{}.Withdraw(c))
	var r *authority.Refusal
	if !errors.As(err, &r) || r.Reason != vocab.ReasonProposalNotOpen {
		t.Fatalf("proposal.withdraw is not wired: %v", err)
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
	// The private socket listens from the readiness point.
	if st := lstat(t, filepath.Join(dir, privateSocket)); st.Mode&0o777 != 0o600 {
		t.Errorf("private socket mode %o", st.Mode)
	}
	// While it runs, a second daemon is refused.
	_, err := Start(context.Background(), Config{StateDir: dir})
	startErr(t, err, vocab.CouldNotRun, vocab.ReasonControllerAlreadyRunning)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lockFree(t, dir)
	gone(t, filepath.Join(dir, privateSocket))
	if out := logs.String(); !strings.Contains(out, "adcd: stopped") || strings.Contains(out, dir) {
		t.Fatalf("log: %q", out)
	}
}
