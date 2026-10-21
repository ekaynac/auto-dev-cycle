// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
)

// RefRecord is a ref's authoritative record (I18, P16a): its identity (id
// and content digest), its type, its complete lineage, and the kind of
// the node whose result holds it ("" for one the controller rendered from
// its authority, an answer's approval). Sanitised and BuilderSafe are
// declassification candidates (#226 entries 91(3), 94(2)), set only here,
// by what recorded the ref and its result's shape: Sanitised for the two
// refs of a gate node's gate_result that holds exactly its receipt and a
// blob (gate.Result's receipt and report: test ids and outcomes, never
// failure text), which a reviewer may take whatever its ancestry;
// BuilderSafe for a review node's view (review.Result's findings view:
// severities and titles), which a builder may take whatever its ancestry.
// A candidate grants nothing alone: its one consumer (intent) honours it
// only once the bytes are verified to be that output (Astra #321 H1).
type RefRecord struct {
	ID            string
	Type          types.RefType
	ContentDigest types.Digest
	Lineage       []string
	Producer      types.NodeKind
	Sanitised     bool
	BuilderSafe   bool
}

// GateShape is whether value is what a gate node's gate_result may hold:
// at most its receipt and at most its report (a blob), in either order,
// nothing else (Astra #321 H1: an attachment beside them is refused when
// it is recorded); full when it holds both, the one shape Records makes a
// Sanitised candidate.
func GateShape(value []types.Ref) (ok, full bool) {
	receipts, blobs := 0, 0
	for _, r := range value {
		switch r.Type {
		case types.RefTypeReceipt:
			receipts++
		case types.RefTypeBlob:
			blobs++
		default:
			return false, false
		}
	}
	return receipts <= 1 && blobs <= 1, receipts == 1 && blobs == 1
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
// (22a-6L): a stored result past either is refused before it is read.
var maxTaskRefs, maxTaskLineage, maxTaskIDBytes = 100_000, 1_000_000, 64 << 20

// The results' bounds, the task's alone (Opus #321 M1): one stored result
// (enforced as well when one is recorded: recordResult), and every one of
// the task's Records reads (TaskResultProjection: another task's results
// are never read). maxNodeBytes bounds a result's node record, read the
// same way (Opus #321 L5).
var maxResultBytes, maxResultsBytes, maxNodeBytes = 4 << 20, 256 << 20, 4 << 20

// maxTaskResults bounds the results Records lists (Astra #321 M5): it
// reads the task's index range only, at most one key past the bound, and
// refuses past it before reading any result. A var, so a test can lower
// it.
var maxTaskResults = 100_000

// Records is task's Recorded, from the event store: every ref a recorded
// result of the task holds, with its producer's kind, and the extra refs
// the controller rendered from its authority (Context's approvals). An id
// recorded with two identities, lineages or producers is ambiguous and
// resolves to nothing. It reads the task's results only, at the call.
func Records(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (Recorded, error) {
	byID, ambiguous, reviewed := map[string]RefRecord{}, map[string]bool{}, map[types.Digest]bool{}
	n, lineage, idBytes := 0, 0, 0
	var read int64
	add := func(r types.Ref, producer types.NodeKind, sanitised, safe bool) error {
		n, lineage, idBytes = n+1, lineage+len(r.Lineage), idBytes+len(r.ID)
		for _, p := range r.Lineage {
			idBytes += len(p)
		}
		if n > maxTaskRefs || lineage > maxTaskLineage || idBytes > maxTaskIDBytes {
			return fmt.Errorf("graph: task %s holds over %d refs, %d lineage entries or %d bytes of ids", task, maxTaskRefs, maxTaskLineage, maxTaskIDBytes)
		}
		rec := RefRecord{ID: r.ID, Type: r.Type, ContentDigest: r.ContentDigest, Lineage: slices.Clone(r.Lineage), Producer: producer,
			Sanitised: sanitised, BuilderSafe: safe}
		if prior, ok := byID[r.ID]; ok && (prior.Type != rec.Type || prior.ContentDigest != rec.ContentDigest || !slices.Equal(prior.Lineage, rec.Lineage) || prior.Producer != rec.Producer ||
			prior.Sanitised != rec.Sanitised || prior.BuilderSafe != rec.BuilderSafe) {
			ambiguous[r.ID] = true
		}
		if producer == types.NodeKindReview {
			reviewed[r.ContentDigest] = true
		}
		byID[r.ID] = rec
		return nil
	}
	prefix := string(task) + "/"
	keys, err := tx.KeysAtMost(TaskResultProjection, prefix, maxTaskResults+1)
	switch {
	case err != nil:
		return Recorded{}, err
	case len(keys) > maxTaskResults:
		return Recorded{}, fmt.Errorf("graph: task %s holds over %d results", task, maxTaskResults)
	}
	for _, k := range keys {
		id := strings.TrimPrefix(k, prefix)
		size, ok, err := tx.Size(ResultProjection, id)
		switch {
		case err != nil:
			return Recorded{}, err
		case !ok:
			return Recorded{}, fmt.Errorf("graph: task %s's result %s is not recorded", task, id)
		}
		if read += size; size > int64(maxResultBytes) || read > int64(maxResultsBytes) {
			return Recorded{}, fmt.Errorf("graph: result %s is over %d bytes, or task %s's results over %d", id, maxResultBytes, task, maxResultsBytes)
		}
		rr, _, err := load[types.ResultRecorded](tx, ResultProjection, id)
		if err != nil {
			return Recorded{}, err
		}
		if size, ok, err := tx.Size(NodeProjection, string(rr.Node)); err != nil || !ok || size > int64(maxNodeBytes) {
			return Recorded{}, errors.Join(err, fmt.Errorf("graph: result %s's node %s is not recorded, or is over %d bytes", id, rr.Node, maxNodeBytes))
		}
		node, _, err := load[nodeRecord](tx, NodeProjection, string(rr.Node))
		if err != nil {
			return Recorded{}, err
		}
		if node.Recorded.Task != task {
			return Recorded{}, fmt.Errorf("graph: result %s is indexed under task %s, its node under %s", id, task, node.Recorded.Task)
		}
		kind := node.Recorded.Node.Kind
		_, gate := GateShape(rr.Result.Value)
		gate = gate && kind == types.NodeKindGate && rr.Result.Kind == types.ResultKindGateResult
		for _, r := range rr.Result.Value {
			safe := kind == types.NodeKindReview && rr.Result.Kind == types.ResultKindReview && r.Type == types.RefTypeView
			if err := add(r, kind, gate, safe); err != nil {
				return Recorded{}, err
			}
		}
	}
	for _, r := range extra {
		if err := add(r, "", false, false); err != nil {
			return Recorded{}, err
		}
	}
	return Recorded{Resolve: func(id string) (RefRecord, bool, error) {
		rec, ok := byID[id]
		rec.Lineage = slices.Clone(rec.Lineage)
		return rec, ok && !ambiguous[id], nil
	}, Reviewed: reviewed}, nil
}

// AttemptNode is the node attempt ran for, as recorded (its task and
// kind): false when attempt was never started or its unit is no node's.
func AttemptNode(tx *eventstore.Tx, attempt types.AttemptID) (types.NodeRecorded, bool, error) {
	a, _, ok, err := lease.AttemptOf(tx, attempt)
	if err != nil || !ok {
		return types.NodeRecorded{}, false, err
	}
	n, ok := NodeOf(a.Unit)
	if !ok {
		return types.NodeRecorded{}, false, nil
	}
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(n))
	return rec.Recorded, ok, err
}
