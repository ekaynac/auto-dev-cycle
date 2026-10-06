// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
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

func leaseStore(t *testing.T) (string, Config) {
	dir := stateDir(t)
	w := seed(t, dir, daemonProjection(t, tokens(10)))
	w.grantedLease(types.ActorID(newID(t, "act")), 4)
	w.done()
	return dir, Config{StateDir: dir, Groups: []types.ResourceGroup{tokens(10)}}
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

func TestFailedRestartTransitionRefused(t *testing.T) {
	dir := stateDir(t)
	markPending = func(*eventstore.Store, context.Context, string) (int, error) { return 0, errors.New("injected") }
	t.Cleanup(func() { markPending = (*eventstore.Store).MarkPendingUncertain })
	refused(t, dir, Config{StateDir: dir}, vocab.CouldNotRun, vocab.ReasonStoreUnreadable, "marked uncertain")
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
	for _, actor := range []string{sys, newID(t, "act")} {
		cmd := newID(t, "cmd")
		_, err := d.store.Submit(context.Background(), eventstore.Command{ID: cmd, Actor: actor, IdempotencyKey: "adcd-system-actor", Input: []byte(`{}`)},
			func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
				return nil, json.RawMessage(`{}`), nil
			})
		if !errors.Is(err, ErrCommandsRefused) {
			t.Fatalf("a submission as %s was not refused: %v", actor, err)
		}
	}
}
