// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// RefRecord is a ref's authoritative record (I18, P16a): its identity (id
// and content digest), its complete lineage, and the kind of the node
// whose result holds it ("" for one the controller rendered from its
// authority, an answer's approval). Sanitised and BuilderSafe are the
// controller's declassifications on the record (#226 entries 91(3),
// 94(2)), set only here, by what recorded the ref: Sanitised for a gate
// node's gate_result (gate.Result's receipt and report: test ids and
// outcomes, never failure text), which a reviewer may take whatever its
// ancestry; BuilderSafe for a review node's view (review.Result's findings
// view: severities and titles), which a builder may take whatever its
// ancestry. Each is honoured by its one consumer only (intent).
type RefRecord struct {
	ID            string
	ContentDigest types.Digest
	Lineage       []string
	Producer      types.NodeKind
	Sanitised     bool
	BuilderSafe   bool
}

// RefRecords resolves a ref id to its record; ok false when the id is
// unknown or ambiguous.
type RefRecords func(id string) (RefRecord, bool, error)

// Recorded is what Records read of one task: its resolver, and the content
// digests a review's result holds (a reviewer's input never has one, under
// whatever id: Opus #289 L2).
type Recorded struct {
	Resolve  RefRecords
	Reviewed map[types.Digest]bool
}

// Records' bounds for one task (Opus #289 L3, Astra #289 L3): the refs it
// keeps, and their lineage entries and bytes of ids in all, each checked
// before a ref's lineage is copied; past any, Records refuses. Vars, so a
// test can lower them. The results themselves are decoded whole before
// these apply, each within maxResultBytes and all within maxResultsBytes
// (22a-6L): a stored result past either is refused before it is decoded.
var maxTaskRefs, maxTaskLineage, maxTaskIDBytes = 100_000, 1_000_000, 64 << 20

// The results' bounds: one stored result, and every one Records reads
// (results are keyed by id, so it reads the store's: the bound is the
// store's, not the task's).
var maxResultBytes, maxResultsBytes = 4 << 20, 256 << 20

// Records is task's Recorded, from the event store: every ref a recorded
// result of the task holds, with its producer's kind, and the extra refs
// the controller rendered from its authority (Context's approvals). An id
// recorded with two identities, lineages or producers is ambiguous and
// resolves to nothing. It reads the store once, at the call.
func Records(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (Recorded, error) {
	byID, ambiguous, reviewed := map[string]RefRecord{}, map[string]bool{}, map[types.Digest]bool{}
	n, lineage, idBytes, read := 0, 0, 0, 0
	add := func(r types.Ref, producer types.NodeKind, kind types.ResultKind) error {
		n, lineage, idBytes = n+1, lineage+len(r.Lineage), idBytes+len(r.ID)
		for _, p := range r.Lineage {
			idBytes += len(p)
		}
		if n > maxTaskRefs || lineage > maxTaskLineage || idBytes > maxTaskIDBytes {
			return fmt.Errorf("graph: task %s holds over %d refs, %d lineage entries or %d bytes of ids", task, maxTaskRefs, maxTaskLineage, maxTaskIDBytes)
		}
		rec := RefRecord{ID: r.ID, ContentDigest: r.ContentDigest, Lineage: slices.Clone(r.Lineage), Producer: producer,
			Sanitised:   producer == types.NodeKindGate && kind == types.ResultKindGateResult,
			BuilderSafe: producer == types.NodeKindReview && kind == types.ResultKindReview && r.Type == types.RefTypeView}
		if prior, ok := byID[r.ID]; ok && (prior.ContentDigest != rec.ContentDigest || !slices.Equal(prior.Lineage, rec.Lineage) || prior.Producer != rec.Producer ||
			prior.Sanitised != rec.Sanitised || prior.BuilderSafe != rec.BuilderSafe) {
			ambiguous[r.ID] = true
		}
		if producer == types.NodeKindReview {
			reviewed[r.ContentDigest] = true
		}
		byID[r.ID] = rec
		return nil
	}
	keys, err := tx.Keys(ResultProjection, "")
	if err != nil {
		return Recorded{}, err
	}
	for _, k := range keys {
		raw, _, err := tx.Get(ResultProjection, k)
		if err != nil {
			return Recorded{}, err
		}
		if read += len(raw); len(raw) > maxResultBytes || read > maxResultsBytes {
			return Recorded{}, fmt.Errorf("graph: result %s is over %d bytes, or the results over %d", k, maxResultBytes, maxResultsBytes)
		}
		var rr types.ResultRecorded
		if err := json.Unmarshal(raw, &rr); err != nil {
			return Recorded{}, err
		}
		node, ok, err := load[nodeRecord](tx, NodeProjection, string(rr.Node))
		if err != nil {
			return Recorded{}, err
		}
		if !ok || node.Recorded.Task != task {
			continue
		}
		for _, r := range rr.Result.Value {
			if err := add(r, node.Recorded.Node.Kind, rr.Result.Kind); err != nil {
				return Recorded{}, err
			}
		}
	}
	for _, r := range extra {
		if err := add(r, "", ""); err != nil {
			return Recorded{}, err
		}
	}
	return Recorded{Resolve: func(id string) (RefRecord, bool, error) {
		rec, ok := byID[id]
		return rec, ok && !ambiguous[id], nil
	}, Reviewed: reviewed}, nil
}
