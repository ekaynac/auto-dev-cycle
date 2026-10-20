// SPDX-License-Identifier: Apache-2.0

// Package intent builds the builder's and the reviewer's minimal
// manifest.intent (10; I10, I18; P16a): exactly the node's rendered
// context, with no input that is, or derives at any depth from, one of the
// task's acceptance blobs (withheld from builder and reviewer).
//
// Decisions (#226 entry 71; Astra #252 r2 H1 and M3; Opus #289):
//   - Ancestry is resolved from authoritative records (graph.Records),
//     never from the refs themselves: each record carries a ref's identity
//     (id and content digest) and its complete lineage, the starting input
//     included. An input whose own record differs from it, an unknown or
//     ambiguous ancestor, and a missing resolver refuse the intent.
//   - The acceptance digests come from the task's own record
//     (authority.TaskAcceptance, through Load), never from the caller;
//     Records with no acceptance read refuse the intent.
//   - Checking a digest is apart from walking an ancestry: every resolved
//     record's id and content digest are checked against the acceptance
//     blobs, and an id is walked (its lineage resolved) once, whatever
//     digest equals it. An ancestor id equal to an input's content digest
//     is resolved.
//   - The walk is bounded: a parent is deduplicated before it is enqueued,
//     so the queue never holds an id twice; each distinct id is counted
//     once against maxRecords (inclusive), before it enters the walk, and
//     each distinct record expanded once, however many inputs reach it; a
//     lineage is refused before any of it is enqueued when it would take
//     the walk past maxEdges lineage entries or maxIDBytes bytes of ids.
//   - The resolver is trusted controller code: a resolver that hides a
//     record or a parent defeats this check, which cannot see what it is
//     not shown. graph.Records answers from the event store's results.
//   - Fresh context per round: a reviewer's input is never a review's
//     result (a prior round's decision): neither a ref a review produced
//     nor, under whatever id, one whose content a review's result holds.
//     Each round renders its own claim.
package intent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

var (
	// ErrAcceptanceInput: the context would carry a task's acceptance. The
	// intent is refused, never written with it omitted.
	ErrAcceptanceInput = errors.New("intent: an acceptance-derived ref among the inputs")
	// ErrAncestryUnresolved: an input's ancestry is not fully known, so it
	// cannot be shown free of acceptance; it refuses as ErrAcceptanceInput.
	ErrAncestryUnresolved = fmt.Errorf("%w: its ancestry is not resolvable", ErrAcceptanceInput)
	// ErrNotFresh: a reviewer's context would carry a review's result.
	ErrNotFresh = errors.New("intent: a review's result among a reviewer's inputs (a fresh context per round)")
)

// The walk's bounds, per intent (var: tests lower them).
var maxRecords, maxEdges, maxIDBytes = 10_000, 100_000, 4 << 20

// Records are the authoritative records an intent is checked against: the
// task's ref records and its acceptance blobs' digests (Load).
type Records struct {
	graph.Recorded
	Acceptance []types.Digest // nil: not read
}

// Load is task's Records in tx: graph.Records (with the controller's extra
// refs) and the acceptance its own record holds.
func Load(tx *eventstore.Tx, task types.TaskID, extra ...types.Ref) (Records, error) {
	rec, err := graph.Records(tx, task, extra...)
	if err != nil {
		return Records{}, err
	}
	acc, err := authority.TaskAcceptance(tx, task)
	return Records{Recorded: rec, Acceptance: append([]types.Digest{}, acc...)}, err
}

// In is what an intent is built from: the call's record (id, call,
// consumer, policy, budget, rendered digest, destination, routing), the
// node's rendered context (graph.Context), each included input's size in
// bytes by identity digest, and the task's records.
type In struct {
	Call    types.ManifestIntent
	Context graph.Rendered
	Bytes   map[types.Digest]int64
	Records Records
}

// Builder is a build node's manifest.intent.
func Builder(in In) (types.ManifestIntent, error) { return build(in, "builder", "build input") }

// Reviewer is a review node's manifest.intent: as Builder's, and no input
// is a review's result.
func Reviewer(in In) (types.ManifestIntent, error) { return build(in, "reviewer", "review input") }

func build(in In, role, purpose string) (types.ManifestIntent, error) {
	switch {
	case in.Records.Resolve == nil:
		return types.ManifestIntent{}, fmt.Errorf("%w: no resolver", ErrAncestryUnresolved)
	case in.Records.Acceptance == nil:
		return types.ManifestIntent{}, fmt.Errorf("%w: the task's acceptance was not read", ErrAncestryUnresolved)
	}
	all := slices.Clone(in.Context.Included)
	for _, o := range in.Context.Omitted {
		all = append(all, o.Ref)
	}
	w := newWalk(in.Records)
	for _, r := range all {
		rec, err := w.start(r)
		if err != nil {
			return types.ManifestIntent{}, err
		}
		if role == "reviewer" && (rec.Producer == types.NodeKindReview || in.Records.Reviewed[rec.ContentDigest]) {
			return types.ManifestIntent{}, fmt.Errorf("%w: %s", ErrNotFresh, r.ID)
		}
	}
	out := in.Call
	out.Consumer.Role = role
	out.Inputs, out.Included, out.Omitted = all, []types.ManifestIntentIncludedItem{}, append([]types.ManifestIntentOmittedItem{}, in.Context.Omitted...)
	for i, r := range in.Context.Included {
		size, sized := in.Bytes[r.IdentityDigest]
		if !sized {
			return types.ManifestIntent{}, fmt.Errorf("intent: no size for included input %s", r.ID)
		}
		out.Included = append(out.Included, types.ManifestIntentIncludedItem{Ref: r, Order: int64(i), Bytes: size, Purpose: purpose})
	}
	return out, knowledge.CheckIntent(out)
}

// walk is one intent's ancestry walk, shared by its inputs: an id walked
// for one input is not walked again, and an input already walked as
// another's ancestor is compared with its cached record, not charged
// again (Astra #289 M2).
type walk struct {
	records           graph.RefRecords
	hidden            map[string]bool            // acceptance digests
	walked            map[string]bool            // ids scheduled: queued or resolved, each counted once
	resolved          map[string]graph.RefRecord // ids resolved and expanded
	n, edges, idBytes int                        // n: the ids scheduled
	peak              int                        // the queue's longest
}

func newWalk(r Records) *walk {
	w := &walk{records: r.Resolve, hidden: map[string]bool{}, walked: map[string]bool{}, resolved: map[string]graph.RefRecord{}}
	for _, d := range r.Acceptance {
		w.hidden[string(d)] = true
	}
	return w
}

// start resolves r's own record, which must be r (its content digest and
// lineage), then walks its ancestry. An r already walked (an ancestor of
// an earlier input) is compared with its record only.
func (w *walk) start(r types.Ref) (graph.RefRecord, error) {
	rec, done := w.resolved[r.ID]
	if !done {
		w.idBytes += len(r.ID)
		var err error
		if err = w.schedule(r.ID, r.ID); err == nil {
			rec, err = w.resolve(r.ID, r.ID)
		}
		if err != nil {
			return rec, err
		}
	}
	if rec.ContentDigest != r.ContentDigest || !slices.Equal(rec.Lineage, r.Lineage) {
		return rec, fmt.Errorf("%w: %s is not the input its record names", ErrAncestryUnresolved, r.ID)
	}
	if done {
		return rec, nil
	}
	queue, err := w.enqueue(r.ID, nil, rec.Lineage)
	for err == nil && len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		var parent graph.RefRecord
		if parent, err = w.resolve(r.ID, id); err == nil {
			queue, err = w.enqueue(r.ID, queue, parent.Lineage)
		}
	}
	return rec, err
}

// resolve is id's record, its identity checked against the acceptance
// blobs: a digest check, apart from walking. It is cached.
func (w *walk) resolve(input, id string) (graph.RefRecord, error) {
	rec, ok, err := w.records(id)
	switch {
	case err != nil:
		return rec, err
	case !ok || rec.ID != id:
		return rec, fmt.Errorf("%w: %s (ancestor %s)", ErrAncestryUnresolved, input, id)
	case w.hidden[id] || w.hidden[string(rec.ContentDigest)]:
		return rec, fmt.Errorf("%w: %s (ancestor %s holds acceptance %s)", ErrAcceptanceInput, input, id, rec.ContentDigest)
	}
	w.resolved[id] = rec
	return rec, nil
}

// schedule counts id, a new one, against the inclusive record bound before
// it enters the walk (Astra #289 M1).
func (w *walk) schedule(input, id string) error {
	if w.n+1 > maxRecords {
		return w.overErr(input)
	}
	w.n++
	w.walked[id] = true
	return nil
}

// enqueue appends a resolved record's parents not yet scheduled: each id
// is enqueued once. A lineage that would take the walk past its edge or
// byte bound is refused before any of it is enqueued, and each new parent
// is counted against the record bound before it is.
func (w *walk) enqueue(input string, queue []string, parents []string) ([]string, error) {
	w.edges += len(parents)
	for _, p := range parents {
		w.idBytes += len(p)
	}
	if err := w.over(input); err != nil {
		return nil, err
	}
	for _, p := range parents {
		if !w.walked[p] {
			if err := w.schedule(input, p); err != nil {
				return nil, err
			}
			queue = append(queue, p)
		}
	}
	w.peak = max(w.peak, len(queue))
	return queue, nil
}

// over refuses a walk past its edge or byte bound.
func (w *walk) over(input string) error {
	if w.edges > maxEdges || w.idBytes > maxIDBytes {
		return w.overErr(input)
	}
	return nil
}

func (w *walk) overErr(input string) error {
	return fmt.Errorf("%w: %s's ancestry is over %d records, %d edges or %d bytes of ids", ErrAncestryUnresolved, input, maxRecords, maxEdges, maxIDBytes)
}
