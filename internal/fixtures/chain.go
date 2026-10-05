// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// hashChainEditAndTruncation: a log of four events verifies against its
// trusted head; one with an event edited (in the middle or at the tail),
// a middle event deleted, or its tail truncated does not (11 "a hash-chain
// edit and truncation"; 03 rule 3). Each log is written by the real store
// and checked as `adc dev verify-log` checks it, read-only under a probe
// lock; the tamper keeps the store's append-only triggers as they were.
func hashChainEditAndTruncation(ctx context.Context, dir string) (steps []Step, violations []string, err error) {
	var o observe
	for i, tamper := range []string{
		"",
		`UPDATE events SET payload = '{"edited":true}' WHERE seq = 2`,
		`UPDATE events SET payload = '{"edited":true}' WHERE seq = 4`,
		`DELETE FROM events WHERE seq = 2`,
		`DELETE FROM events WHERE seq >= 3`,
	} {
		state := filepath.Join(dir, fmt.Sprint(i), "state")
		trusted, err := fourEvents(ctx, filepath.Dir(state))
		if err != nil {
			return nil, nil, err
		}
		if tamper != "" {
			if err := rewrite(ctx, filepath.Join(state, eventstore.DBFile), tamper); err != nil {
				return nil, nil, err
			}
		}
		lock, err := eventstore.ProbeLock(state)
		if err != nil {
			return nil, nil, err
		}
		h, err := eventstore.VerifyExisting(ctx, lock, &trusted)
		if err = errors.Join(err, lock.Release()); errors.Is(err, eventstore.ErrChainBroken) {
			o.steps = append(o.steps, Step{vocab.Failed, vocab.ReasonLogIntegrityBroken.String()})
			continue
		} else if err != nil {
			return nil, nil, err
		}
		o.steps = append(o.steps, Step{vocab.Empty, vocab.ReasonCompletedClean.String()})
		o.check(h == trusted, "the verified head is %+v, want the trusted %+v", h, trusted)
	}
	return o.steps, o.violations, nil
}

// fourEvents writes a store of four events in dir and returns its head.
func fourEvents(ctx context.Context, dir string) (eventstore.Head, error) {
	f, done, err := open(ctx, dir)
	if err != nil {
		return eventstore.Head{}, err
	}
	defer done()
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		if _, err := f.submit(k, `{}`); err != nil {
			return eventstore.Head{}, err
		}
	}
	return f.s.Head(ctx)
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
