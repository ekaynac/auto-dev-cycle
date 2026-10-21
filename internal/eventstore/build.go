// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Projection is a projection a build folds: its name, and the version of
// the fold that builds it.
type Projection struct {
	Name    string
	Version int
}

// builtRecord is the store's record of the projections it has built: one
// proj_kv row per projection, keyed by its name, valued {"version": N}. It
// is not a projection name (projectionName), so no projector reads or
// writes it, and a rebuild leaves it out of the comparison.
const builtRecord = "_built"

type buildRecord struct {
	Version int `json:"version"`
}

// BuildProjections replays the whole log, as RebuildProjections does, and
// builds in the live tables every registered projection the store has never
// built: one with no record of a build and no live record (#226 entry 107).
// A projection added by a later build is so built from the log the first
// time that build starts on an older store. A projection with a record of
// its build, or with a live record, is never built here: it is only
// compared. Then every projection is compared with the replay, as
// RebuildProjections does. On any difference nothing is kept: the
// transaction is rolled back and the differences are returned. Otherwise
// every registered projection is recorded as built at its version, and the
// transaction commits; built lists the projections built, in registration
// order.
//
// A store holding no record at all was written before the record existed:
// it is taken to have built the projections in assumed, which are recorded
// before anything is decided. Every other registered projection is built
// in it if it has no live record.
//
// Errors are RebuildProjections's: ErrChainBroken, ErrReplayRefused, or a
// storage error or cancellation as itself. An invalid registration is an
// error, before anything is read.
func (s *Store) BuildProjections(ctx context.Context, registered []Projection, assumed []string) (built []string, diffs []ProjectionDiff, err error) {
	if err := checkRegistered(registered, assumed); err != nil {
		return nil, nil, err
	}
	err = s.do(func() error {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		done := false
		defer func() {
			if !done {
				_ = tx.Rollback() // only on a panic, which keeps unwinding
			}
		}()
		built, diffs, err = s.build(ctx, tx, registered, assumed)
		done = true
		if err != nil || len(diffs) > 0 {
			built = nil
			return errors.Join(err, tx.Rollback())
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, nil, err
	}
	return built, diffs, nil
}

func checkRegistered(registered []Projection, assumed []string) error {
	seen := map[string]bool{}
	for _, p := range registered {
		if !projectionName.MatchString(p.Name) || p.Version < 1 || seen[p.Name] {
			return fmt.Errorf("eventstore: registered projection %q, version %d: a projection name, once, at a version from 1", p.Name, p.Version)
		}
		seen[p.Name] = true
	}
	for _, name := range assumed {
		if !seen[name] {
			return fmt.Errorf("eventstore: assumed projection %q is not registered", name)
		}
	}
	return nil
}

func (s *Store) build(ctx context.Context, tx *sql.Tx, registered []Projection, assumed []string) ([]string, []ProjectionDiff, error) {
	if err := s.replayFresh(ctx, tx); err != nil {
		return nil, nil, err
	}
	recorded, err := builtProjections(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if len(recorded) == 0 {
		versions := map[string]int{}
		for _, p := range registered {
			versions[p.Name] = p.Version
		}
		for _, name := range assumed {
			if err := record(ctx, tx, name, versions[name]); err != nil {
				return nil, nil, err
			}
			recorded[name] = true
		}
	}
	var built []string
	for _, p := range registered {
		if recorded[p.Name] {
			continue
		}
		var held bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+liveProjections+` WHERE projection = ?)`, p.Name).Scan(&held); err != nil {
			return nil, nil, err
		}
		if held {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+liveProjections+`(projection, key, value)
			SELECT projection, key, value FROM `+rebuildProjections+` WHERE projection = ?`, p.Name); err != nil {
			return nil, nil, err
		}
		built = append(built, p.Name)
	}
	diffs, err := compareLive(ctx, tx)
	if err != nil || len(diffs) > 0 {
		return nil, diffs, err
	}
	for _, p := range registered {
		if err := record(ctx, tx, p.Name, p.Version); err != nil {
			return nil, nil, err
		}
	}
	// The scratch table is the connection's: it would outlive the commit.
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+rebuildProjections); err != nil {
		return nil, nil, err
	}
	return built, nil, nil
}

// builtProjections is the store's record of the projections it built. A
// record that does not decode is an error: it is never read as absent.
func builtProjections(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM `+liveProjections+` WHERE projection = ?`, builtRecord)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		var b buildRecord
		if err := json.Unmarshal([]byte(value), &b); err != nil || b.Version < 1 || !projectionName.MatchString(name) {
			return nil, fmt.Errorf("eventstore: the record of projection %q's build does not decode", name)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// record records the projection as built at version.
func record(ctx context.Context, tx *sql.Tx, name string, version int) error {
	v, err := json.Marshal(buildRecord{Version: version})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+liveProjections+`(projection, key, value) VALUES (?, ?, ?)
		ON CONFLICT(projection, key) DO UPDATE SET value = excluded.value`, builtRecord, name, string(v))
	return err
}
