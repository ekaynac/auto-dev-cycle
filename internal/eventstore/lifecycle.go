// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// ErrClosed: the store was closed.
var ErrClosed = errors.New("eventstore: the store is closed")

// fileID identifies a file (device and inode).
type fileID struct{ dev, ino uint64 }

// fileIdentity is the identity of a database file, which must be a regular
// file owned by this user with one link: a symlink or a hardlink would let
// two state directories, under two locks, share one database.
func fileIdentity(path string) (fileID, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return fileID{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return fileID{}, fmt.Errorf("eventstore: %s must be a regular file owned by this user with one link; refused", path)
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// checkDBFiles checks the database and its SQLite sidecars, where present.
func checkDBFiles(file string) error {
	for _, p := range []string{file, file + "-wal", file + "-shm", file + "-journal"} {
		if _, err := fileIdentity(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// checkMetaTable refuses a meta table with a rowid: replacing a row through
// its rowid would free a key the triggers guard.
func checkMetaTable(ctx context.Context, db *sql.DB) error {
	var ddl string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToUpper(strings.TrimSpace(ddl)), "WITHOUT ROWID") {
		return fmt.Errorf("eventstore: the meta table is not WITHOUT ROWID; store format %s requires it", Format)
	}
	return nil
}

// do runs one operation while the store is open, on the database file it
// opened; Close waits for it. Once Close has begun, new operations, nested
// ones included, are refused with ErrClosed rather than blocked. An
// operation must not call Close.
func (s *Store) do(fn func() error) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return ErrClosed
	}
	s.active++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active--; s.active == 0 {
			s.idle.Broadcast()
		}
		s.mu.Unlock()
	}()
	if err := checkDBFiles(s.file); err != nil {
		return err
	}
	if id, err := fileIdentity(s.file); err != nil || id != s.ident {
		return fmt.Errorf("eventstore: %s is no longer the database this store opened; refused (%v)", s.file, err)
	}
	return fn()
}

// inTx runs fn in a writer transaction, committed when fn succeeds and
// rolled back otherwise, a panic included: the transaction is finished
// before the operation ends, so it never outlives the store (Close waits
// for the operation). fn must not start another writer transaction.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.do(func() error {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = tx.Rollback() // only on a panic, which keeps unwinding
			}
		}()
		err = fn(tx)
		finished = true
		if err != nil {
			return errors.Join(err, tx.Rollback())
		}
		return tx.Commit()
	})
}

// View runs fn in one read transaction on the read-only pool: a snapshot of
// the live projections and subject versions, taken when View begins, for
// reads (docs/design/m0/08). fn's Tx refuses Put, Delete and effects, and
// the transaction always rolls back. A view sees committed state only: a
// decision that must hold when a command commits (authorization) runs on
// the writer's Tx, never on a view. Keep views short: an open view pins the
// log's write-ahead file and Close waits for it.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return s.do(func() error {
		tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a read changes nothing to keep
		// SQLite takes a deferred transaction's snapshot at its first read:
		// read now, so the snapshot is View's, not fn's first statement's.
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM events LIMIT 1`).Scan(&one); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return fn(&Tx{ctx: ctx, tx: tx, readOnly: true, owner: s})
	})
}

// Close stops new operations, waits for running ones, closes both pools and
// ends this store's claim on the lock, once; later calls do nothing.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	for s.active > 0 {
		s.idle.Wait()
	}
	s.mu.Unlock()
	err := errors.Join(s.w.Close(), s.r.Close())
	s.lock.unclaim(s.claim)
	return err
}
