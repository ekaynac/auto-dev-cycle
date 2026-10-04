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
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
) WITHOUT ROWID;
CREATE TRIGGER IF NOT EXISTS meta_immutable_update BEFORE UPDATE ON meta
  BEGIN SELECT RAISE(ABORT, 'meta is immutable'); END;
CREATE TRIGGER IF NOT EXISTS meta_immutable_delete BEFORE DELETE ON meta
  BEGIN SELECT RAISE(ABORT, 'meta is immutable'); END;
CREATE TRIGGER IF NOT EXISTS meta_no_replace BEFORE INSERT ON meta
  WHEN EXISTS (SELECT 1 FROM meta WHERE key = NEW.key)
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
CREATE TRIGGER IF NOT EXISTS events_no_replace BEFORE INSERT ON events
  WHEN EXISTS (SELECT 1 FROM events WHERE seq = NEW.seq OR id = NEW.id)
  BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
`

// Kind is a registered event kind: the contract schema its payload must
// satisfy (e.g. "records/effect-intent") and the payload fields that hold
// blob digests (at any depth, e.g. "blob", "payload_schema"). The store
// derives a payload's blob references from them; a caller cannot omit one.
type Kind struct {
	Schema     string
	BlobFields []string
}

// Kinds maps every registered event kind (<domain>.<name>/v<n>) to its Kind.
// An unregistered kind is never appended.
type Kinds map[string]Kind

// BlobChecker reports whether a referenced blob is published (03
// "publish-before-reference"); the blob store implements it.
type BlobChecker interface {
	Has(digest string) (bool, error)
}

// Store is an open event store.
type Store struct {
	w, r      *sql.DB // one writer connection; a read-only pool
	lock      *Lock
	claim     uint64 // this store's claim on the lock
	file      string // the database file
	ident     fileID // its identity when opened
	genesis   string
	schemaSet string
	kinds     Kinds
	blobs     BlobChecker

	// Operation tracking: each operation is admitted while the store is
	// open and counted; Close stops admissions and waits for the count to
	// reach zero. Nothing blocks on admission, so an operation nested in
	// another (a read inside a transaction) never deadlocks with Close.
	mu      sync.Mutex
	idle    *sync.Cond // signalled when active reaches zero
	active  int
	closing bool
}

// Config is what Open needs besides the lock.
type Config struct {
	Kinds Kinds       // event kinds the controller may write
	Blobs BlobChecker // required: referenced blobs must be published first
}

// Open opens (creating on first use) the store in the lock's state
// directory. There is no other way to obtain a Store.
func Open(ctx context.Context, lock *Lock, cfg Config) (s *Store, err error) {
	if lock == nil {
		return nil, errors.New("eventstore: the state directory lock must be held before the database is opened")
	}
	if cfg.Blobs == nil {
		return nil, errors.New("eventstore: a blob checker is required (publish-before-reference)")
	}
	claim, err := lock.claim()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.unclaim(claim)
		}
	}()
	set, err := schemas.SetDigest()
	if err != nil {
		return nil, err
	}
	file := filepath.Join(lock.dir, DBFile)
	if err := checkDBFiles(file); err != nil {
		return nil, err
	}
	const pragmas = "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	w, err := sql.Open("sqlite", fileURI(file, "_txlock=immediate&"+pragmas))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.ExecContext(ctx, ddl); err != nil {
		w.Close()
		return nil, err
	}
	if err := checkMetaTable(ctx, w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", fileURI(file, "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"))
	if err != nil {
		w.Close()
		return nil, err
	}
	// A deep copy: the caller's configuration cannot change the kinds later.
	kinds := Kinds{}
	for k, v := range cfg.Kinds {
		kinds[k] = Kind{Schema: v.Schema, BlobFields: slices.Clone(v.BlobFields)}
	}
	s = &Store{w: w, r: r, lock: lock, claim: claim, file: file, schemaSet: set, kinds: kinds, blobs: cfg.Blobs}
	s.idle = sync.NewCond(&s.mu)
	if s.genesis, err = s.ensureGenesis(ctx); err == nil {
		err = checkDBFiles(file)
	}
	if err == nil {
		s.ident, err = fileIdentity(file)
	}
	if err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return s, nil
}

// fileURI is the SQLite URI for a file path: the path is escaped, so ?, #
// and % in a directory name never become URI syntax (which would open
// another file or change connection options).
func fileURI(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
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
}

// rowHash is sha256 over the canonical encoding of every column but hash.
// A text column that is not valid UTF-8 is an error, never replaced: two
// different stored values must never share an encoding.
func rowHash(e Event) (string, error) {
	b, err := jsonv2.Marshal(e, json.DefaultOptionsV1(), jsontext.AllowInvalidUTF8(false))
	if err != nil {
		return "", err
	}
	return digest(b)
}

// blobRefs returns the digests held by the given fields anywhere in a
// (validated) payload.
func blobRefs(payload []byte, fields []string) ([]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, f := range fields {
		want[f] = true
	}
	var refs []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := x.(string); ok && want[k] {
					refs = append(refs, s)
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	return refs, nil
}

// append adds e inside the writer's tx: it checks the kind, the payload
// against the kind's schema, the optional ids and the referenced blobs, then
// assigns the gapless seq, the controller time, the writer's schema set, the
// canonical payload and the chain hashes. Only the writer calls it.
func (s *Store) append(ctx context.Context, tx *sql.Tx, e Event) (Event, error) {
	if err := ids.Check(e.ID, "evt"); err != nil {
		return Event{}, fmt.Errorf("eventstore: %w", err)
	}
	kind, ok := s.kinds[e.Kind]
	if !ok {
		return Event{}, fmt.Errorf("eventstore: unregistered event kind %q", e.Kind)
	}
	if err := ids.Check(e.Actor, "act"); err != nil {
		return Event{}, fmt.Errorf("eventstore: actor: %w", err)
	}
	prefix, _, _ := strings.Cut(e.Subject, "_")
	if err := ids.Check(e.Subject, prefix); err != nil {
		return Event{}, fmt.Errorf("eventstore: subject: %w", err)
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
	// The payload as given is validated first: canonicalisation must not
	// hide what the contract refuses (-0, for one).
	if err := contract.Validate(kind.Schema, e.Payload); err != nil {
		return Event{}, fmt.Errorf("eventstore: %s payload: %w", e.Kind, err)
	}
	payload, err := canonical(e.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("eventstore: %s payload: %w", e.Kind, err)
	}
	refs, err := blobRefs(payload, kind.BlobFields)
	if err != nil {
		return Event{}, fmt.Errorf("eventstore: %s payload: %w", e.Kind, err)
	}
	for _, d := range refs {
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
