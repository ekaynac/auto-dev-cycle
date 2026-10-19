// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// RefRecord is a ref's authoritative record (I18, P16a): its identity (id
// and content digest), its complete lineage, and the kind of the node
// whose result holds it ("" for one the controller rendered from its
// authority, an answer's approval).
type RefRecord struct {
	ID            string
	ContentDigest types.Digest
	Lineage       []string
	Producer      types.NodeKind
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

// maxTaskRefs bounds the refs Records keeps for one task; past it, Records
// refuses (Opus #289 L3). A var, so a test can lower it.
var maxTaskRefs = 100_000

// Records is task's Recorded, from the event store: every ref a recorded
// result of the task holds, with its producer's kind, and the extra refs
// the controller rendered from its authority (Context's approvals). An id
// recorded with two identities, lineages or producers is ambiguous and
// resolves to nothing. It reads the store once, at the call.
func Records(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (Recorded, error) {
	byID, ambiguous, reviewed := map[string]RefRecord{}, map[string]bool{}, map[types.Digest]bool{}
	n := 0
	add := func(r types.Ref, producer types.NodeKind) error {
		if n++; n > maxTaskRefs {
			return fmt.Errorf("graph: task %s holds over %d refs", task, maxTaskRefs)
		}
		rec := RefRecord{ID: r.ID, ContentDigest: r.ContentDigest, Lineage: slices.Clone(r.Lineage), Producer: producer}
		if prior, ok := byID[r.ID]; ok && (prior.ContentDigest != rec.ContentDigest || !slices.Equal(prior.Lineage, rec.Lineage) || prior.Producer != rec.Producer) {
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
		rr, _, err := load[types.ResultRecorded](tx, ResultProjection, k)
		if err != nil {
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
			if err := add(r, node.Recorded.Node.Kind); err != nil {
				return Recorded{}, err
			}
		}
	}
	for _, r := range extra {
		if err := add(r, ""); err != nil {
			return Recorded{}, err
		}
	}
	return Recorded{Resolve: func(id string) (RefRecord, bool, error) {
		rec, ok := byID[id]
		return rec, ok && !ambiguous[id], nil
	}, Reviewed: reviewed}, nil
}
