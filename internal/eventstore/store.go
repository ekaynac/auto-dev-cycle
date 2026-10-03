// SPDX-License-Identifier: Apache-2.0

// Package eventstore is the append-only event log (docs/design/m0/03): one
// SQLite database per controller, WAL with synchronous=FULL, opened only by
// the holder of the state-directory lock, every row hash-chained to the one
// before it from a genesis record. Writes go through one connection; reads
// use a separate read-only pool, so a read inside a writer transaction never
// waits on it.
package eventstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/gowebpki/jcs"
	_ "modernc.org/sqlite" // pure-Go driver: release binaries stay static

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// DBFile is the database file in the state directory; Format is the store
// format this code reads and writes.
const (
	DBFile = "adc.db"
	Format = "adc.eventstore/v1"
)

const ddl = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS meta_immutable_update BEFORE UPDATE ON meta
  BEGIN SELECT RAISE(ABORT, 'meta is immutable'); END;
CREATE TRIGGER IF NOT EXISTS meta_immutable_delete BEFORE DELETE ON meta
  BEGIN SELECT RAISE(ABORT, 'meta is immutable'); END;
CREATE TABLE IF NOT EXISTS events (
  seq        INTEGER PRIMARY KEY,
  id         TEXT UNIQUE NOT NULL,
  at         TEXT NOT NULL,
  kind       TEXT NOT NULL,
  subject    TEXT NOT NULL,
  project    TEXT,
  actor      TEXT NOT NULL,
  command    TEXT,
  causation  TEXT,
  schema_set TEXT NOT NULL,
  payload    BLOB NOT NULL,
  prev_hash  TEXT NOT NULL,
  hash       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_subject ON events(subject, seq);
CREATE INDEX IF NOT EXISTS events_command ON events(command, seq);
CREATE TRIGGER IF NOT EXISTS events_append_only_update BEFORE UPDATE ON events
  BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_append_only_delete BEFORE DELETE ON events
  BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
`

// Kinds maps every registered event kind (<domain>.<name>/v<n>) to the
// contract schema its payload must satisfy (e.g. "records/effect-intent").
// An unregistered kind is never appended.
type Kinds map[string]string

// BlobChecker reports whether a referenced blob is published (03
// "publish-before-reference"); the blob store implements it.
type BlobChecker interface {
	Has(digest string) (bool, error)
}

// Store is an open event store.
type Store struct {
	w, r      *sql.DB // one writer connection; a read-only pool
	lock      *Lock
	genesis   string
	schemaSet string
	kinds     Kinds
	blobs     BlobChecker
}

// Config is what Open needs besides the lock.
type Config struct {
	Kinds Kinds       // event kinds the controller may write
	Blobs BlobChecker // required: referenced blobs must be published first
}

// Open opens (creating on first use) the store in the lock's state
// directory. There is no other way to obtain a Store.
func Open(ctx context.Context, lock *Lock, cfg Config) (*Store, error) {
	if !lock.Held() {
		return nil, errors.New("eventstore: the state directory lock must be held before the database is opened")
	}
	if cfg.Blobs == nil {
		return nil, errors.New("eventstore: a blob checker is required (publish-before-reference)")
	}
	set, err := schemas.SetDigest()
	if err != nil {
		return nil, err
	}
	file := filepath.Join(lock.dir, DBFile)
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", "file:"+file+"?_txlock=immediate&"+pragmas)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(ctx, ddl); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", "file:"+file+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		w.Close()
		return nil, err
	}
	kinds := Kinds{}
	for k, v := range cfg.Kinds {
		kinds[k] = v
	}
	s := &Store{w: w, r: r, lock: lock, schemaSet: set, kinds: kinds, blobs: cfg.Blobs}
	if s.genesis, err = s.ensureGenesis(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes both pools. The lock is released separately by its holder.
func (s *Store) Close() error {
	return errors.Join(s.w.Close(), s.r.Close())
}

// ensureGenesis checks the store format and creates the creation record
// once; its hash is the prev_hash of the first event.
func (s *Store) ensureGenesis(ctx context.Context) (string, error) {
	var format string
	err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'format'`).Scan(&format)
	if err == nil {
		if format != Format {
			return "", fmt.Errorf("eventstore: store format %q, this code reads %q", format, Format)
		}
		var g string
		return g, s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'genesis_hash'`).Scan(&g)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	record, err := json.Marshal(map[string]string{"format": Format, "store_id": hex.EncodeToString(id), "created_at": now()})
	if err != nil {
		return "", err
	}
	g, err := digest(record)
	if err != nil {
		return "", err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{{"format", Format}, {"genesis_record", string(record)}, {"genesis_hash", g}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	return g, tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

const maxSafeInt = 9007199254740991

var integerRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// canonical refuses any number that is not an integer within ±(2^53−1)
// (RFC 8785 canonicalisation goes through float64, which would silently
// round larger values), and trailing data, then returns the RFC 8785 form.
func canonical(doc []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the document")
	}
	if err := safeNumbers(v); err != nil {
		return nil, err
	}
	return jcs.Transform(doc)
}

func safeNumbers(v any) error {
	switch t := v.(type) {
	case json.Number:
		if !integerRe.MatchString(string(t)) {
			return fmt.Errorf("number %s is not an integer", t)
		}
		n, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil || n > maxSafeInt || n < -maxSafeInt {
			return fmt.Errorf("integer %s is outside ±(2^53−1)", t)
		}
	case map[string]any:
		for _, x := range t {
			if err := safeNumbers(x); err != nil {
				return err
			}
		}
	case []any:
		for _, x := range t {
			if err := safeNumbers(x); err != nil {
				return err
			}
		}
	}
	return nil
}

// digest is sha256 over the canonical form of a JSON document.
func digest(doc []byte) (string, error) {
	c, err := canonical(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Event is one row of the log. Seq, At, SchemaSet, PrevHash and Hash are
// assigned by append; Command by the writer.
type Event struct {
	Seq       int64           `json:"seq"`
	ID        string          `json:"id"`
	At        string          `json:"at"`
	Kind      string          `json:"kind"`
	Subject   string          `json:"subject"`
	Project   *string         `json:"project"`
	Actor     string          `json:"actor"`
	Command   *string         `json:"command"`
	Causation *string         `json:"causation"`
	SchemaSet string          `json:"schema_set"`
	Payload   json.RawMessage `json:"payload"`
	PrevHash  string          `json:"prev_hash"`
	Hash      string          `json:"-"`
	Blobs     []string        `json:"-"` // digests the payload references; they must be published first
}

// rowHash is sha256 over the canonical encoding of every column but hash.
func rowHash(e Event) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return digest(b)
}

// append adds e inside the writer's tx: it checks the kind, the payload
// against the kind's schema, the optional ids and the referenced blobs, then
// assigns the gapless seq, the controller time, the writer's schema set, the
// canonical payload and the chain hashes. Only the writer calls it.
func (s *Store) append(ctx context.Context, tx *sql.Tx, e Event) (Event, error) {
	if err := ids.Check(e.ID, "evt"); err != nil {
		return Event{}, fmt.Errorf("eventstore: %w", err)
	}
	schema, ok := s.kinds[e.Kind]
	if !ok {
		return Event{}, fmt.Errorf("eventstore: unregistered event kind %q", e.Kind)
	}
	if e.Subject == "" || e.Actor == "" {
		return Event{}, errors.New("eventstore: subject and actor are required")
	}
	for _, opt := range []struct {
		v      *string
		prefix string
	}{{e.Project, "prj"}, {e.Command, "cmd"}, {e.Causation, "evt"}} {
		if opt.v != nil {
			if err := ids.Check(*opt.v, opt.prefix); err != nil {
				return Event{}, fmt.Errorf("eventstore: %w", err)
			}
		}
	}
	payload, err := canonical(e.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("eventstore: %s payload: %w", e.Kind, err)
	}
	if err := contract.Validate(schema, payload); err != nil {
		return Event{}, fmt.Errorf("eventstore: %s payload: %w", e.Kind, err)
	}
	for _, d := range e.Blobs {
		present, err := s.blobs.Has(d)
		if err != nil {
			return Event{}, err
		}
		if !present {
			return Event{}, fmt.Errorf("eventstore: %s references blob %s before it is published", e.Kind, d)
		}
	}
	e.Payload, e.SchemaSet = payload, s.schemaSet
	var last sql.NullInt64
	var prev sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&last, &prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Event{}, err
	}
	e.Seq, e.PrevHash, e.At = last.Int64+1, s.genesis, now()
	if prev.Valid {
		e.PrevHash = prev.String
	}
	if e.Hash, err = rowHash(e); err != nil {
		return Event{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(seq, id, at, kind, subject, project, actor, command, causation, schema_set, payload, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Seq, e.ID, e.At, e.Kind, e.Subject, e.Project, e.Actor, e.Command, e.Causation, e.SchemaSet, []byte(e.Payload), e.PrevHash, e.Hash)
	if err != nil {
		return Event{}, err
	}
	return e, nil
}
