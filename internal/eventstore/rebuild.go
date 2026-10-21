// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ProjectionDiff is one projection record on which the live tables and a
// rebuild from the log disagree. A nil side is a record that side lacks.
type ProjectionDiff struct {
	Projection, Key string
	Live, Rebuilt   *string
}

// ErrReplayRefused: the Project hook refused an event of the log on replay,
// with no storage error behind it.
var ErrReplayRefused = errors.New("eventstore: rebuild: the projector refused the log")

// rebuildBatch bounds the events held in memory at once during a rebuild.
var rebuildBatch = 512

// RebuildProjections replays the whole log through the Project hook into
// fresh projection tables and compares them with the live ones (03; no `adc
// dev rebuild-projections` command wires it yet). It verifies the full
// chain first and changes nothing: the rebuild runs in a writer transaction
// that is always rolled back. An empty result means the live projections
// are exactly what the log derives.
//
// The fresh table is a TEMP proj_rebuild, which the hook's Tx reads and
// writes in place of the live proj_kv. The hook is replayed one event at a
// time, in log order: a projector that is not a fold (see Hooks.Project)
// shows up as drift. A projector's error with no storage error behind it is
// ErrReplayRefused; a storage error or cancellation is returned as itself.
//
// The store's record of built projections (build.go) is no projection: it
// is compared only when the store was opened with Config.Projections. Then
// a record of a projection not registered there is a difference, as at
// start (ProjectionDiff.BuildRecord), and a record that does not decode or
// records a later version than registered is ErrBuildRecord, as at start.
func (s *Store) RebuildProjections(ctx context.Context) ([]ProjectionDiff, error) {
	var diffs []ProjectionDiff
	err := s.do(func() error {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		rolledBack := false
		defer func() {
			if !rolledBack {
				_ = tx.Rollback() // only on a panic, which keeps unwinding
			}
		}()
		diffs, err = s.rebuild(ctx, tx)
		rolledBack = true
		// Always rolled back: a rebuild never changes the store.
		return errors.Join(err, tx.Rollback())
	})
	if err != nil {
		return nil, err
	}
	return diffs, nil
}

func (s *Store) rebuild(ctx context.Context, tx *sql.Tx) ([]ProjectionDiff, error) {
	if err := s.replayFresh(ctx, tx); err != nil {
		return nil, err
	}
	diffs, err := compareLive(ctx, tx)
	if err != nil || s.projections == nil {
		return diffs, err
	}
	recorded, err := builtProjections(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := laterRecords(recorded, s.projections); err != nil {
		return nil, err
	}
	// _built sorts before every projection name: the order stays compare's.
	return append(unknownRecords(recorded, s.projections), diffs...), nil
}

// replayFresh verifies the chain and replays it into a fresh scratch table,
// in tx.
func (s *Store) replayFresh(ctx context.Context, tx *sql.Tx) error {
	// The chain is verified inside this transaction, so the log replayed is
	// exactly the log verified (03 rule 3).
	if _, err := s.verify(ctx, tx, nil); err != nil {
		return err
	}
	// A fresh scratch table; one left by a failed rollback is dropped first.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+rebuildProjections); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE proj_rebuild (
		projection TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (projection, key)) WITHOUT ROWID`); err != nil {
		return err
	}
	return s.replay(ctx, tx)
}

// compareLive compares the live projections, the store's record of the
// projections it built aside (build.go), with the scratch table.
func compareLive(ctx context.Context, tx *sql.Tx) ([]ProjectionDiff, error) {
	live, err := projections(ctx, tx, `SELECT projection, key, value FROM `+liveProjections+` WHERE projection <> '`+builtRecord+`'`)
	if err != nil {
		return nil, err
	}
	rebuilt, err := projections(ctx, tx, `SELECT projection, key, value FROM `+rebuildProjections)
	if err != nil {
		return nil, err
	}
	return compare(live, rebuilt), nil
}

// replay feeds every event, in seq order and in bounded batches, to the
// Project hook.
func (s *Store) replay(ctx context.Context, tx *sql.Tx) error {
	if s.hooks.Project == nil {
		return nil
	}
	for after := int64(0); ; {
		batch, err := eventsAfter(ctx, tx, after, rebuildBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, e := range batch {
			pt := &Tx{ctx: ctx, tx: tx, rebuild: true}
			if err := s.hooks.Project(pt, []Event{e}); err != nil && (pt.fault != nil || ctx.Err() != nil) {
				return fmt.Errorf("eventstore: rebuild: projecting seq %d: %w", e.Seq, err)
			} else if err != nil {
				return fmt.Errorf("%w: projecting seq %d: %w", ErrReplayRefused, e.Seq, err)
			}
		}
		after = batch[len(batch)-1].Seq
	}
}

func eventsAfter(ctx context.Context, tx *sql.Tx, after int64, limit int) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq, id, at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash, hash
		FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.ID, &e.At, &e.Kind, &e.Subject, &e.Project, &e.Actor, &e.Command, &e.Causation, &e.SchemaSet, &payload, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		e.Payload = payload
		out = append(out, e)
	}
	return out, rows.Err()
}

type projKey struct{ projection, key string }

func projections(ctx context.Context, tx *sql.Tx, query string) (map[projKey]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[projKey]string{}
	for rows.Next() {
		var k projKey
		var v string
		if err := rows.Scan(&k.projection, &k.key, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// compare lists every record on which live and rebuilt differ, sorted.
func compare(live, rebuilt map[projKey]string) []ProjectionDiff {
	var diffs []ProjectionDiff
	for k, v := range live {
		if r, ok := rebuilt[k]; !ok || r != v {
			d := ProjectionDiff{Projection: k.projection, Key: k.key, Live: &v}
			if ok {
				d.Rebuilt = &r
			}
			diffs = append(diffs, d)
		}
	}
	for k, r := range rebuilt {
		if _, ok := live[k]; !ok {
			diffs = append(diffs, ProjectionDiff{Projection: k.projection, Key: k.key, Rebuilt: &r})
		}
	}
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].Projection != diffs[j].Projection {
			return diffs[i].Projection < diffs[j].Projection
		}
		return diffs[i].Key < diffs[j].Key
	})
	return diffs
}

// EventsAfter reads up to limit events after seq, in order, in t's
// snapshot: a reader that decides authority in the same view never sees an
// event its decision did not see.
func (t *Tx) EventsAfter(after int64, limit int) ([]Event, error) {
	evs, err := eventsAfter(t.ctx, t.tx, after, limit)
	return evs, t.io(err)
}

// UnreadKinds lists, sorted, the kinds of persisted events this store was
// not opened to read. A projector skips such an event, so a reader that does
// not handle every version ever written (03 rule 4) must refuse the log,
// never ignore the event.
func (s *Store) UnreadKinds(ctx context.Context) ([]string, error) {
	var out []string
	err := s.View(ctx, func(t *Tx) error {
		rows, err := t.tx.QueryContext(ctx, `SELECT DISTINCT kind FROM events ORDER BY kind`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			if _, ok := s.kinds[k]; !ok {
				out = append(out, k)
			}
		}
		return rows.Err()
	})
	return out, err
}
