// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// hashChainEditAndTruncation: a log of four events verifies against its
// trusted head; one with an event edited (in the middle or at the tail), a
// middle event deleted, its tail truncated (to two events, by its head row
// only, or to nothing), or rewritten from the middle on as a consistent
// chain does not (11 "a hash-chain edit and truncation"; 03 rule 3).
// Without the anchor, a consistent prefix verifies clean by design: only
// the trusted head catches truncation and a consistent rewrite. Each log is
// written by the real store and checked as `adc dev verify-log` checks it,
// read-only under a probe lock; a tamper keeps the store's append-only
// triggers as they were.
func hashChainEditAndTruncation(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	var o observe
	for i, c := range []struct {
		tamper   string
		anchored bool
	}{
		{"", true},
		{`UPDATE events SET payload = '{"edited":true}' WHERE seq = 2`, true},
		{`UPDATE events SET payload = '{"edited":true}' WHERE seq = 4`, true},
		{`DELETE FROM events WHERE seq = 2`, true},
		{`DELETE FROM events WHERE seq >= 3`, true},
		{`DELETE FROM events WHERE seq = 4`, true},
		{`DELETE FROM events`, true},
		{`DELETE FROM events WHERE seq >= 3`, false},
	} {
		state := filepath.Join(dir, fmt.Sprint(i), "state")
		trusted, err := events(ctx, filepath.Dir(state), "k1", "k2", "k3", "k4")
		if err != nil {
			return nil, nil, err
		}
		if c.tamper != "" {
			if err := rewrite(ctx, filepath.Join(state, eventstore.DBFile), c.tamper); err != nil {
				return nil, nil, err
			}
		}
		anchor, want := &trusted, trusted
		if !c.anchored {
			anchor, want = nil, eventstore.Head{Seq: 2}
		}
		h, err := o.verified(ctx, state, anchor)
		if err != nil {
			return nil, nil, err
		}
		o.check(h == nil || h.Seq == want.Seq && (!c.anchored || *h == want), "the verified head is %+v, want %+v", h, want)
	}
	// A consistent rewrite: a copy of the log after its first event, carried
	// on by the real writer with other events, so seq 2 on are re-hashed and
	// re-linked; it verifies by itself, and fails against the trusted head.
	orig, fork := filepath.Join(dir, "orig"), filepath.Join(dir, "fork")
	if _, err := events(ctx, orig, "k1"); err != nil {
		return nil, nil, err
	}
	for _, f := range []string{eventstore.DBFile, eventstore.DBFile + "-wal"} {
		if err := copyOver(filepath.Join(orig, "state", f), filepath.Join(fork, "state", f)); err != nil {
			return nil, nil, err
		}
	}
	trusted, err := events(ctx, orig, "k2", "k3", "k4")
	if err != nil {
		return nil, nil, err
	}
	if _, err := events(ctx, fork, "x2", "x3", "x4"); err != nil {
		return nil, nil, err
	}
	var free observe
	h, err := free.verified(ctx, filepath.Join(fork, "state"), nil)
	if err != nil {
		return nil, nil, err
	}
	o.check(h != nil && h.Seq == 4 && h.Hash != trusted.Hash, "the rewritten log does not verify by itself as another four-event chain: %+v", h)
	if _, err := o.verified(ctx, filepath.Join(fork, "state"), &trusted); err != nil {
		return nil, nil, err
	}
	return o.steps, o.violations, nil
}

// verified checks the store in state against trusted (none: unanchored)
// as one step, and returns the verified head, nil when it failed.
func (o *observe) verified(ctx context.Context, state string, trusted *eventstore.Head) (*eventstore.Head, error) {
	lock, err := eventstore.ProbeLock(state)
	if err != nil {
		return nil, err
	}
	h, err := eventstore.VerifyExisting(ctx, lock, trusted)
	if err = errors.Join(err, lock.Release()); errors.Is(err, eventstore.ErrChainBroken) {
		o.steps = append(o.steps, Step{vocab.Failed, vocab.ReasonLogIntegrityBroken.String()})
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
	return &h, nil
}

// events appends one event per key to the store in dir (made if absent)
// and returns its head.
func events(ctx context.Context, dir string, keys ...string) (eventstore.Head, error) {
	f, done, err := open(ctx, dir)
	if err != nil {
		return eventstore.Head{}, err
	}
	defer done()
	for _, k := range keys {
		if _, err := f.submit(k, `{}`); err != nil {
			return eventstore.Head{}, err
		}
	}
	return f.s.Head(ctx)
}

// copyOver copies src to dst, making dst's directory; a missing src is
// skipped.
func copyOver(src, dst string) error {
	b, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// rewrite runs stmt on the closed store's database, as someone with write
// access to the file would: the events triggers are dropped around it and
// recreated exactly, so only the log differs.
func rewrite(ctx context.Context, file, stmt string) (err error) {
	db, err := sql.Open("sqlite", "file:"+file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // after a commit, a no-op
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'events'`)
	if err != nil {
		return err
	}
	var names, ddl []string
	for rows.Next() {
		var n, d string
		if err := rows.Scan(&n, &d); err != nil {
			return errors.Join(err, rows.Close())
		}
		names, ddl = append(names, n), append(ddl, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var stmts []string
	for _, n := range names {
		stmts = append(stmts, `DROP TRIGGER "`+n+`"`)
	}
	for _, s := range append(append(stmts, stmt), ddl...) {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return tx.Commit()
}
