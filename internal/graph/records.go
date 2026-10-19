// SPDX-License-Identifier: Apache-2.0

package graph

import (
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

// Records is task's RefRecords, from the event store: every ref a recorded
// result of the task holds, with its producer's kind, and the extra refs
// the controller rendered from its authority (Context's approvals). An id
// recorded with two identities or lineages is ambiguous and resolves to
// nothing. It reads the store once, at the call.
func Records(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (RefRecords, error) {
	byID, ambiguous := map[string]RefRecord{}, map[string]bool{}
	add := func(r types.Ref, producer types.NodeKind) {
		rec := RefRecord{ID: r.ID, ContentDigest: r.ContentDigest, Lineage: slices.Clone(r.Lineage), Producer: producer}
		if prior, ok := byID[r.ID]; ok && (prior.ContentDigest != rec.ContentDigest || !slices.Equal(prior.Lineage, rec.Lineage) || prior.Producer != rec.Producer) {
			ambiguous[r.ID] = true
		}
		byID[r.ID] = rec
	}
	keys, err := tx.Keys(ResultProjection, "")
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		rr, _, err := load[types.ResultRecorded](tx, ResultProjection, k)
		if err != nil {
			return nil, err
		}
		n, ok, err := load[nodeRecord](tx, NodeProjection, string(rr.Node))
		if err != nil {
			return nil, err
		}
		if !ok || n.Recorded.Task != task {
			continue
		}
		for _, r := range rr.Result.Value {
			add(r, n.Recorded.Node.Kind)
		}
	}
	for _, r := range extra {
		add(r, "")
	}
	return func(id string) (RefRecord, bool, error) {
		rec, ok := byID[id]
		return rec, ok && !ambiguous[id], nil
	}, nil
}
