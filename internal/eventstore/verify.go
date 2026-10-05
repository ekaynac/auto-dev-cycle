// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"modernc.org/sqlite"

	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// Head names the last event: the trusted head a backup carries outside the
// database (03 rule 3).
type Head struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// ErrChainBroken marks a log that fails verification.
var ErrChainBroken = errors.New("event log integrity")

// Head returns the current head. An empty log's head is seq 0 and the
// genesis hash, so even an empty store is anchored.
func (s *Store) Head(ctx context.Context) (h Head, err error) {
	err = s.do(func() error {
		err := s.r.QueryRowContext(ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&h.Seq, &h.Hash)
		if errors.Is(err, sql.ErrNoRows) {
			h, err = Head{Seq: 0, Hash: s.genesis}, nil
		}
		return err
	})
	return h, err
}

// Verify checks the whole chain: seq is gapless from 1, every row links to
// the hash before it (the genesis hash for the first), and every hash
// matches its recomputed canonical encoding. With a trusted head it also
// detects truncation and a rewritten tail: the log must reach trusted.Seq
// and carry trusted.Hash there. It cannot detect a complete, consistent
// rewrite by someone who also replaced the trusted head (03 rule 3).
func (s *Store) Verify(ctx context.Context, trusted *Head) (h Head, err error) {
	err = s.do(func() error {
		var err error
		h, err = s.verify(ctx, s.r, trusted)
		return err
	})
	return h, err
}

// VerifyExisting verifies the store in a probe's state directory without
// opening it as a Store: nothing is created, migrated or repaired (no DDL,
// no genesis). The database must already exist; it is read through a
// read-only connection in one read transaction, on a private copy of the
// database and its write-ahead log: no file in the state directory is
// opened for writing, created or removed. A database whose tables,
// indexes and triggers differ from the format's, an empty file among them,
// fails verification like a broken chain. Take the lock with ProbeLock.
func VerifyExisting(ctx context.Context, lock *Lock, trusted *Head) (h Head, err error) {
	defer func() {
		if corrupt(err) {
			err = fmt.Errorf("%w: %v", ErrChainBroken, err)
		}
	}()
	if lock == nil {
		return Head{}, errors.New("eventstore: the state directory lock must be held before the database is opened")
	}
	claim, err := lock.claim()
	if err != nil {
		return Head{}, err
	}
	defer lock.unclaim(claim)
	file := filepath.Join(lock.dir, DBFile)
	if _, err := fileIdentity(file); err != nil {
		return Head{}, err
	}
	if err := checkDBFiles(file); err != nil {
		return Head{}, err
	}
	// SQLite opens only a private copy: whatever it does to a log it takes
	// as stale, or to a file another process changes meanwhile, it does to
	// the copy. The store's files are only read.
	snap, err := snapshot(file)
	if err != nil {
		return Head{}, err
	}
	defer func() {
		// A copy left behind is reported, never silently kept; a verdict
		// stays a verdict (errors.Is still finds ErrChainBroken).
		if rerr := removeSnapshot(snap); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	snapDB := filepath.Join(snap.path, DBFile)
	if err := checkHeader(snapDB); err != nil {
		return Head{}, err
	}
	db, err := sql.Open("sqlite", fileURI(snapDB, "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"))
	if err != nil {
		return Head{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Head{}, err
	}
	defer func() { _ = tx.Rollback() }() // a read changes nothing to keep
	if err := checkObjects(ctx, tx); err != nil {
		return Head{}, err
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return Head{}, err
	}
	var genesis string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'genesis_hash'`).Scan(&genesis); err != nil {
		return Head{}, fmt.Errorf("%w: genesis hash: %v", ErrChainBroken, err)
	}
	return (&Store{genesis: genesis}).verify(ctx, tx, trusted)
}

// corrupt reports SQLite's verdicts on the file itself: a malformed database
// (SQLITE_CORRUPT, 11) or a file that is not a database (SQLITE_NOTADB, 26),
// in whichever query SQLite first finds it. They fail verification; they do
// not mean the check could not run.
func corrupt(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case 11, 26:
		return true
	}
	return false
}

// maxSnapshotBytes bounds the database and log VerifyExisting copies. A
// larger store is not checked (could_not_run), so a sparse or runaway file
// can neither fill the disk nor be handed to SQLite.
var maxSnapshotBytes int64 = 16 << 30

// snapshot copies the database and its write-ahead log, where present, into
// a new private temporary directory, opening each read-only without
// following links, and returns the copy's database path.
//
// The copy is a single moment of the store: the database and the log are
// each recorded (identity, size, modification and change times, presence)
// before the copying starts and must be unchanged after it, so a process
// that writes them meanwhile (the probe lock excludes only cooperating
// controllers) makes the check could_not_run instead of verifying a mixture
// of two moments. The temporary directory must lie outside the state
// directory, so nothing is written there.
func snapshot(file string) (copyDir, error) {
	paths := []string{file, file + "-wal"}
	before := make([]sourceState, len(paths))
	var total int64
	for i, p := range paths {
		st, err := stateOf(p)
		if err != nil {
			return copyDir{}, err
		}
		before[i] = st
		total += st.size
	}
	if !before[0].exists {
		return copyDir{}, fmt.Errorf("eventstore: %s is missing", file)
	}
	if total > maxSnapshotBytes {
		return copyDir{}, fmt.Errorf("eventstore: %s and its log hold %d bytes, more than the %d verification copies", DBFile, total, maxSnapshotBytes)
	}
	root, err := tempRoot(filepath.Dir(file))
	if err != nil {
		return copyDir{}, err
	}
	if testHooks.resolved != nil {
		testHooks.resolved()
	}
	dir, err := os.MkdirTemp(root, "adc-verify-*")
	if err != nil {
		return copyDir{}, err
	}
	made, err := stateOf(dir)
	if err != nil || !made.exists {
		return copyDir{}, errors.Join(fmt.Errorf("eventstore: the verification copy in %s vanished", dir), err)
	}
	cd := copyDir{path: dir, dev: made.dev, ino: made.ino}
	fail := func(err error) (copyDir, error) {
		return copyDir{}, errors.Join(err, removeSnapshot(cd))
	}
	budget := maxSnapshotBytes
	for i, p := range paths {
		if !before[i].exists {
			continue
		}
		n, err := copyFile(p, filepath.Join(dir, filepath.Base(p)), budget)
		if err != nil {
			return fail(err)
		}
		budget -= n
		if testHooks.copied != nil {
			testHooks.copied(p)
		}
	}
	for i, p := range paths {
		st, err := stateOf(p)
		if err != nil {
			return fail(err)
		}
		if st != before[i] {
			return fail(fmt.Errorf("eventstore: %s changed while it was copied; nothing was verified", p))
		}
	}
	return cd, nil
}

// sourceState is what snapshot records of a file to tell that it changed.
type sourceState struct {
	exists             bool
	dev, ino           uint64
	size, mtime, ctime int64
}

func stateOf(p string) (sourceState, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); errors.Is(err, syscall.ENOENT) {
		return sourceState{}, nil
	} else if err != nil {
		return sourceState{}, &os.PathError{Op: "lstat", Path: p, Err: err}
	}
	return sourceState{exists: true, dev: uint64(st.Dev), ino: uint64(st.Ino), size: st.Size, mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano()}, nil
}

// tempRoot is the canonical directory the verification copy is made in:
// TMPDIR resolved once, links and ".." in filesystem order (a relative
// TMPDIR is joined to the working directory without cleaning it first), so
// the copy's path holds no alias that could be retargeted later. It must
// exist and lie outside the state directory.
func tempRoot(state string) (string, error) {
	resolve := func(p string) (string, error) {
		if !filepath.IsAbs(p) {
			wd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			p = wd + string(filepath.Separator) + p
		}
		return filepath.EvalSymlinks(p)
	}
	tmp, err := resolve(os.TempDir())
	if err != nil {
		return "", fmt.Errorf("eventstore: the temporary directory: %w", err)
	}
	st, err := resolve(state)
	if err != nil {
		return "", err
	}
	if tmp == st || strings.HasPrefix(tmp, st+string(filepath.Separator)) {
		return "", fmt.Errorf("eventstore: the temporary directory %s is inside the state directory; verification writes nothing there (set TMPDIR elsewhere)", tmp)
	}
	return tmp, nil
}

// copyDir is a verification copy's directory: its canonical path and the
// identity it was made with.
type copyDir struct {
	path     string
	dev, ino uint64
}

// removeSnapshot removes a verification copy, naming it when it cannot: a
// directory no longer the one made (moved or replaced) is not touched, and
// a removal that leaves anything behind is reported.
func removeSnapshot(d copyDir) error {
	if testHooks.cleanup != nil {
		testHooks.cleanup(d.path)
	}
	st, err := stateOf(d.path)
	if err != nil {
		return fmt.Errorf("eventstore: the verification copy in %s could not be checked: %w", d.path, err)
	}
	if !st.exists || st.dev != d.dev || st.ino != d.ino {
		return fmt.Errorf("eventstore: the verification copy made in %s is no longer there; it may remain elsewhere", d.path)
	}
	if err := os.RemoveAll(d.path); err != nil {
		return fmt.Errorf("eventstore: the verification copy in %s could not be removed: %w", d.path, err)
	}
	if st, err := stateOf(d.path); err != nil || st.exists {
		return fmt.Errorf("eventstore: the verification copy in %s was not removed", d.path)
	}
	return nil
}

// testHooks are seams for tests only: after the temporary root is resolved,
// after a source is copied, and before a copy is removed.
var testHooks struct {
	resolved func()
	copied   func(src string)
	cleanup  func(dir string)
}

// copyFile copies src to a new file dst, at most budget bytes: a file that
// grows past it while being copied is refused.
func copyFile(src, dst string, budget int64) (int64, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > budget {
		err = fmt.Errorf("eventstore: %s grew past the %d bytes verification copies", src, maxSnapshotBytes)
	}
	return n, err
}

// checkIntegrity runs SQLite's structural check: a store whose pages or
// indexes are malformed fails verification even when every row's hash holds.
func checkIntegrity(ctx context.Context, q querier) error {
	rows, err := q.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("%w: integrity check: %v", ErrChainBroken, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: integrity check: %v", ErrChainBroken, err)
	}
	if len(lines) != 1 || lines[0] != "ok" {
		return fmt.Errorf("%w: integrity check: %s", ErrChainBroken, strings.Join(lines, "; "))
	}
	return nil
}

// checkHeader reads the database header before SQLite opens the file: a
// store is an SQLite 3 database in WAL mode. Anything else fails
// verification untouched, because SQLite would take a write-ahead log beside
// a database that is not in WAL mode (an emptied file, another database) as
// stale and delete it on close, even on a read-only connection.
func checkHeader(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := make([]byte, 100)
	if _, err := io.ReadFull(f, h); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %s is shorter than a database header", ErrChainBroken, DBFile)
	} else if err != nil {
		return err
	}
	if string(h[:16]) != "SQLite format 3\x00" || h[18] != 2 || h[19] != 2 {
		return fmt.Errorf("%w: %s is not an SQLite database in WAL mode", ErrChainBroken, DBFile)
	}
	return nil
}

// checkObjects compares the database's schema objects with the ones the
// format's DDL creates, by type, name, table and statement: a dropped or
// rewritten append-only trigger, a missing table or an extra object fails.
func checkObjects(ctx context.Context, q querier) error {
	ref, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return err
	}
	defer ref.Close()
	ref.SetMaxOpenConns(1) // one connection, one in-memory database
	if _, err := ref.ExecContext(ctx, ddl); err != nil {
		return err
	}
	want, err := objects(ctx, ref)
	if err != nil {
		return err
	}
	got, err := objects(ctx, q)
	if err != nil {
		return err
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return fmt.Errorf("%w: store object %s is missing or differs from format %s", ErrChainBroken, k, Format)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			return fmt.Errorf("%w: store object %s is not part of format %s", ErrChainBroken, k, Format)
		}
	}
	return nil
}

// objects maps "type name" to "table: statement" for every schema object,
// the automatic indexes behind PRIMARY KEY and UNIQUE included; only
// statistics tables, which ANALYZE may add, are left out.
func objects(ctx context.Context, q querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite\_stat%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var typ, name, table, stmt string
		if err := rows.Scan(&typ, &name, &table, &stmt); err != nil {
			return nil, err
		}
		m[typ+" "+name] = table + ": " + stmt
	}
	return m, rows.Err()
}

// verify reads through q: the reader pool, or a rebuild's transaction.
func (s *Store) verify(ctx context.Context, q querier, trusted *Head) (Head, error) {
	// meta holds exactly the format and the genesis record and hash.
	var n int
	var format string
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM meta`).Scan(&n); err != nil {
		return Head{}, err
	}
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'format'`).Scan(&format); err != nil || n != 3 || format != Format {
		return Head{}, fmt.Errorf("%w: meta holds %d keys, format %q", ErrChainBroken, n, format)
	}
	var record, genesisHash string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'genesis_record'`).Scan(&record); err != nil {
		return Head{}, fmt.Errorf("%w: genesis record: %v", ErrChainBroken, err)
	}
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'genesis_hash'`).Scan(&genesisHash); err != nil {
		return Head{}, fmt.Errorf("%w: genesis hash: %v", ErrChainBroken, err)
	}
	if got, err := digest([]byte(record)); err != nil || got != genesisHash || genesisHash != s.genesis {
		return Head{}, fmt.Errorf("%w: the genesis record does not match its hash", ErrChainBroken)
	}
	rows, err := q.QueryContext(ctx, `SELECT seq, id, at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash, hash FROM events ORDER BY seq`)
	if err != nil {
		return Head{}, err
	}
	defer rows.Close()
	head := Head{Hash: s.genesis}
	var atTrusted string
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.ID, &e.At, &e.Kind, &e.Subject, &e.Project, &e.Actor, &e.Command, &e.Causation, &e.SchemaSet, &payload, &e.PrevHash, &e.Hash); err != nil {
			return Head{}, err
		}
		e.Payload = payload
		if c, err := canonical(payload); err != nil || !bytes.Equal(c, payload) {
			return Head{}, fmt.Errorf("%w: seq %d payload is not stored in its canonical form", ErrChainBroken, e.Seq)
		}
		if e.Seq != head.Seq+1 {
			return Head{}, fmt.Errorf("%w: gap or reorder at seq %d (expected %d)", ErrChainBroken, e.Seq, head.Seq+1)
		}
		if e.PrevHash != head.Hash {
			return Head{}, fmt.Errorf("%w: seq %d does not link to the previous hash", ErrChainBroken, e.Seq)
		}
		got, err := rowHash(e)
		if err != nil {
			return Head{}, fmt.Errorf("%w: seq %d cannot be re-encoded: %v", ErrChainBroken, e.Seq, err)
		}
		if got != e.Hash && !mutation.Is(mutation.ChainHashUnchecked) {
			return Head{}, fmt.Errorf("%w: seq %d was edited (hash mismatch)", ErrChainBroken, e.Seq)
		}
		head = Head{Seq: e.Seq, Hash: e.Hash}
		if trusted != nil && e.Seq == trusted.Seq {
			atTrusted = e.Hash
		}
	}
	if err := rows.Err(); err != nil {
		return Head{}, err
	}
	if trusted != nil && trusted.Seq == 0 && trusted.Hash != genesisHash {
		return Head{}, fmt.Errorf("%w: the genesis differs from the trusted head", ErrChainBroken)
	}
	if trusted != nil && trusted.Seq > 0 {
		if head.Seq < trusted.Seq {
			return Head{}, fmt.Errorf("%w: truncated: log ends at seq %d, trusted head is seq %d", ErrChainBroken, head.Seq, trusted.Seq)
		}
		if atTrusted != trusted.Hash {
			return Head{}, fmt.Errorf("%w: seq %d differs from the trusted head", ErrChainBroken, trusted.Seq)
		}
	}
	return head, nil
}
