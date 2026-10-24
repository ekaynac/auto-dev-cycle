// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
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
	Restrictions  []types.Restriction // the ref's, as recorded: a plan brief's is checked (intent, Opus 22a-5c-3 M1)
	Project       types.ProjectID     // the ref's scope's project, "" when none: a plan brief's is checked (r2 L2)
	Framework     bool                // the ref's scope is the framework's: never a plan brief's (Mythos L2)
	Producer      types.NodeKind
	Sanitised     bool
	BuilderSafe   bool
	Identity      types.Digest // knowledge.IdentityApartFromLineage of the ref: its every canonical field but its lineage (Astra chunk 4b H2)
}

// BuilderSafe is whether r, a ref of rr, a result of a producer of kind,
// is a builder-safe candidate: a review's findings view (review.Result's).
// The one rule (Opus 22a-6c M1, #226 entry 157): Records marks it, and a
// build's context renders a review's result as it alone (renderContext).
func BuilderSafe(kind types.NodeKind, rr types.ResultRecorded, r types.Ref) bool {
	return kind == types.NodeKindReview && rr.Result.Kind == types.ResultKindReview && r.Type == types.RefTypeView
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

// RefRecords resolves a ref id to its records: one, or several that differ
// in their lineage alone (RefRecord.Identity), recorded by producers of one kind (a
// content-addressed id a task's rounds record again: a review's decision
// repeated round after round; #226 entry 161). Every check over an id's
// record checks each (fail closed). ok false when the id is unknown or
// ambiguous: recorded with another type, digest, producer kind, scope,
// restrictions or declassification.
type RefRecords func(id string) ([]RefRecord, bool, error)

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
// recorded again with another lineage, all else equal, is several records
// (RefRecords); recorded with another identity, producer kind, scope or
// restrictions, it is ambiguous and resolves to nothing. It reads the task's results only, at the call.
func Records(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (Recorded, error) {
	byID, ambiguous, reviewed := map[string][]RefRecord{}, map[string]bool{}, map[types.Digest]bool{}
	n, lineage, idBytes := 0, 0, 0
	add := func(r types.Ref, producer types.NodeKind, sanitised, safe bool) error {
		n, lineage, idBytes = n+1, lineage+len(r.Lineage), idBytes+len(r.ID)
		for _, p := range r.Lineage {
			idBytes += len(p)
		}
		if n > maxTaskRefs || lineage > maxTaskLineage || idBytes > maxTaskIDBytes {
			return fmt.Errorf("graph: task %s holds over %d refs, %d lineage entries or %d bytes of ids", task, maxTaskRefs, maxTaskLineage, maxTaskIDBytes)
		}
		identity, err := knowledge.IdentityApartFromLineage(r)
		if err != nil {
			return err
		}
		rec := RefRecord{ID: r.ID, Type: r.Type, ContentDigest: r.ContentDigest, Lineage: slices.Clone(r.Lineage), Restrictions: cloneRestrictions(r.Restrictions),
			Project: ScopeProject(r.Scope), Framework: r.Scope.Framework, Producer: producer, Sanitised: sanitised, BuilderSafe: safe, Identity: identity}
		recs := byID[r.ID]
		if len(recs) > 0 {
			prior := recs[0]
			if prior.Identity != rec.Identity || prior.Producer != rec.Producer || // every canonical field but the lineage, one shared comparison (Astra chunk 4b H2)
				prior.Producer == "" && !slices.Equal(prior.Lineage, rec.Lineage) || // the controller's own refs: one lineage
				prior.Sanitised != rec.Sanitised || prior.BuilderSafe != rec.BuilderSafe {
				ambiguous[r.ID] = true
			}
		}
		if producer == types.NodeKindReview {
			reviewed[r.ContentDigest] = true
		}
		if !slices.ContainsFunc(recs, func(o RefRecord) bool { return slices.Equal(o.Lineage, rec.Lineage) }) {
			byID[r.ID] = append(recs, rec)
		}
		return nil
	}
	if err := taskResults(tx, task, func(rr types.ResultRecorded, node nodeRecord) error {
		kind := node.Recorded.Node.Kind
		_, gate := GateShape(rr.Result.Value)
		gate = gate && kind == types.NodeKindGate && rr.Result.Kind == types.ResultKindGateResult
		for _, r := range rr.Result.Value {
			if err := add(r, kind, gate, BuilderSafe(kind, rr, r)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Recorded{}, err
	}
	for _, r := range extra {
		if err := add(r, "", false, false); err != nil {
			return Recorded{}, err
		}
	}
	return Recorded{Resolve: func(id string) ([]RefRecord, bool, error) {
		recs, ok := byID[id]
		if !ok || ambiguous[id] {
			return nil, false, nil
		}
		out := make([]RefRecord, len(recs))
		for i, rec := range recs {
			rec.Lineage, rec.Restrictions = slices.Clone(rec.Lineage), cloneRestrictions(rec.Restrictions)
			out[i] = rec
		}
		return out, true, nil
	}, Reviewed: reviewed}, nil
}

// SameRestriction: a and b are one restriction, an empty list of allowed
// destinations equal to a missing one (Opus 22a-5c-3 r2 L3).
func SameRestriction(a, b types.Restriction) bool {
	return a.Project == b.Project && a.PolicyDigest == b.PolicyDigest && a.Floor == b.Floor && a.Routing == b.Routing &&
		slices.Equal(a.AllowedDestinations, b.AllowedDestinations)
}

// ScopeProject is s's project, "" when it names none.
func ScopeProject(s types.RefScope) types.ProjectID {
	if s.Project == nil {
		return ""
	}
	return *s.Project
}

// cloneRestrictions is a deep copy of rs.
func cloneRestrictions(rs []types.Restriction) []types.Restriction {
	out := make([]types.Restriction, len(rs))
	for i, r := range rs {
		out[i] = r
		out[i].AllowedDestinations = slices.Clone(r.AllowedDestinations)
	}
	return out
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

// taskResults calls fn with each result task's index lists, in result id
// order, with its node's record: the task's index range alone, within
// maxTaskResults, each result within maxResultBytes and all within
// maxResultsBytes, each node within maxNodeBytes, all checked before a
// record is decoded. Each is checked against the hop that named it: the
// result is the one its key names, and its node, under its own id, is of
// task (Records' check; Opus #432 r3's rule for every read).
func taskResults(tx *eventstore.Tx, task types.TaskID, fn func(types.ResultRecorded, nodeRecord) error) error {
	prefix := string(task) + "/"
	keys, err := tx.KeysAtMost(TaskResultProjection, prefix, maxTaskResults+1)
	switch {
	case err != nil:
		return err
	case len(keys) > maxTaskResults:
		return fmt.Errorf("graph: task %s holds over %d results", task, maxTaskResults)
	}
	var read int64
	for _, k := range keys {
		id := strings.TrimPrefix(k, prefix)
		size, ok, err := tx.Size(ResultProjection, id)
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("graph: task %s's result %s is not recorded", task, id)
		}
		if read += size; size > int64(maxResultBytes) || read > int64(maxResultsBytes) {
			return fmt.Errorf("graph: result %s is over %d bytes, or task %s's results over %d", id, maxResultBytes, task, maxResultsBytes)
		}
		rr, _, err := load[types.ResultRecorded](tx, ResultProjection, id)
		if err != nil {
			return err
		}
		if string(rr.Result.ID) != id {
			return fmt.Errorf("%w: task %s's result index names %s, whose record is %s's", ErrIndexDamaged, task, id, rr.Result.ID)
		}
		if size, ok, err := tx.Size(NodeProjection, string(rr.Node)); err != nil || !ok || size > int64(maxNodeBytes) {
			return errors.Join(err, fmt.Errorf("graph: result %s's node %s is not recorded, or is over %d bytes", id, rr.Node, maxNodeBytes))
		}
		node, _, err := load[nodeRecord](tx, NodeProjection, string(rr.Node))
		if err != nil {
			return err
		}
		if node.Recorded.Node.ID != rr.Node || node.Recorded.Task != task {
			return fmt.Errorf("%w: result %s is indexed under task %s, its node %s under %s", ErrIndexDamaged, id, task, node.Recorded.Node.ID, node.Recorded.Task)
		}
		if err := fn(rr, node); err != nil {
			return err
		}
	}
	return nil
}

// ResultsOf are every result recorded on task's nodes (result.recorded),
// selected or not, by node, each in result id order (taskResults' reads
// and checks): the benchmark's per-node results (adc dev bench record).
func ResultsOf(tx *eventstore.Tx, task types.TaskID) (map[types.NodeID][]types.ResultRecorded, error) {
	out := map[types.NodeID][]types.ResultRecorded{}
	err := taskResults(tx, task, func(rr types.ResultRecorded, _ nodeRecord) error {
		out[rr.Node] = append(out[rr.Node], rr)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
