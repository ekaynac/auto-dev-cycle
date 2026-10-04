// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		if got != e.Hash {
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
