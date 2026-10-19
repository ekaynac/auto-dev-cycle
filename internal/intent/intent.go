// SPDX-License-Identifier: Apache-2.0

// Package intent builds the builder's and the reviewer's minimal
// manifest.intent (10; I10, I18; P16a): exactly the node's rendered
// context, with no input that is, or derives at any depth from, one of the
// task's acceptance blobs (withheld from builder and reviewer).
//
// Decisions (#226 entry 71; Astra #252 r2 H1 and M3):
//   - Ancestry is resolved from authoritative records (graph.RefRecords),
//     never from the refs themselves: each record carries a ref's identity
//     (id and content digest) and its complete lineage, the starting input
//     included. An input whose own record differs from it, an unknown or
//     ambiguous ancestor, and a missing resolver refuse the intent.
//   - Checking a digest is apart from walking an ancestry: every resolved
//     record's content digest is checked against the acceptance blobs (an
//     acceptance blob that has no record is unresolved, and refused), and
//     an id is walked (its lineage resolved) once, whatever digest equals
//     it. An ancestor id equal to an input's content digest is resolved.
//   - The walk is bounded: an id is enqueued once (deduplicated before
//     enqueueing); at most maxRecords records (inclusive), maxEdges lineage
//     entries and maxIDBytes bytes of ids are examined; past any, refused.
//   - The resolver is trusted controller code: a resolver that hides a
//     record or a parent defeats this check, which cannot see what it is
//     not shown. graph.RefRecords answers from the event store's results.
//   - Fresh context per round: a reviewer's input is never a review's
//     result (a prior round's decision); each round renders its own claim.
package intent

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
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

// In is what an intent is built from: the call's record (id, call,
// consumer, policy, budget, rendered digest, destination, routing), the
// node's rendered context (graph.Context), each included input's size in
// bytes by identity digest, the task's acceptance blobs, and the resolver.
type In struct {
	Call       types.ManifestIntent
	Context    graph.Rendered
	Bytes      map[types.Digest]int64
	Acceptance []types.Digest
	Records    graph.RefRecords
}

// Builder is a build node's manifest.intent.
func Builder(in In) (types.ManifestIntent, error) { return build(in, "builder", "build input") }

// Reviewer is a review node's manifest.intent: as Builder's, and no input
// is a review's result.
func Reviewer(in In) (types.ManifestIntent, error) { return build(in, "reviewer", "review input") }

func build(in In, role, purpose string) (types.ManifestIntent, error) {
	if in.Records == nil {
		return types.ManifestIntent{}, fmt.Errorf("%w: no resolver", ErrAncestryUnresolved)
	}
	all := slices.Clone(in.Context.Included)
	for _, o := range in.Context.Omitted {
		all = append(all, o.Ref)
	}
	w := walk{records: in.Records, hidden: map[string]bool{}, walked: map[string]bool{}}
	for _, d := range in.Acceptance {
		w.hidden[string(d)] = true
	}
	for _, r := range all {
		rec, err := w.start(r)
		if err != nil {
			return types.ManifestIntent{}, err
		}
		if role == "reviewer" && rec.Producer == types.NodeKindReview {
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
// for one input is not walked again.
type walk struct {
	records           graph.RefRecords
	hidden            map[string]bool // acceptance digests (a blob ref's id is its digest)
	walked            map[string]bool // ids whose lineage is enqueued
	n, edges, idBytes int
}

// start resolves r's own record, which must be r (its content digest and
// lineage), then walks its ancestry.
func (w *walk) start(r types.Ref) (graph.RefRecord, error) {
	rec, err := w.resolve(r.ID, r.ID)
	if err != nil {
		return rec, err
	}
	if rec.ContentDigest != r.ContentDigest || !slices.Equal(rec.Lineage, r.Lineage) {
		return rec, fmt.Errorf("%w: %s is not the input its record names", ErrAncestryUnresolved, r.ID)
	}
	queue := w.enqueue(nil, r.ID, rec.Lineage)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		parent, err := w.resolve(r.ID, id)
		if err != nil {
			return rec, err
		}
		queue = w.enqueue(queue, id, parent.Lineage)
	}
	return rec, w.over(r.ID)
}

// resolve is id's record, its identity checked against the acceptance
// blobs: a digest check, apart from walking.
func (w *walk) resolve(input, id string) (graph.RefRecord, error) {
	w.idBytes += len(id)
	if err := w.over(input); err != nil {
		return graph.RefRecord{}, err
	}
	rec, ok, err := w.records(id)
	switch {
	case err != nil:
		return rec, err
	case !ok || rec.ID != id:
		return rec, fmt.Errorf("%w: %s (ancestor %s)", ErrAncestryUnresolved, input, id)
	case w.hidden[string(rec.ContentDigest)]:
		return rec, fmt.Errorf("%w: %s (ancestor %s holds acceptance %s)", ErrAcceptanceInput, input, id, rec.ContentDigest)
	}
	w.n++
	return rec, w.over(input)
}

// enqueue marks id walked and appends its parents not yet walked or
// queued: each id is enqueued once.
func (w *walk) enqueue(queue []string, id string, parents []string) []string {
	w.walked[id] = true
	w.edges += len(parents)
	for _, p := range parents {
		if !w.walked[p] {
			w.walked[p] = true // queued: its own lineage follows once it resolves
			queue = append(queue, p)
		}
	}
	return queue
}

// over refuses a walk past its bounds; the record bound is inclusive.
func (w *walk) over(input string) error {
	if w.n > maxRecords || w.edges > maxEdges || w.idBytes > maxIDBytes {
		return fmt.Errorf("%w: %s's ancestry is over %d records, %d edges or %d bytes of ids", ErrAncestryUnresolved, input, maxRecords, maxEdges, maxIDBytes)
	}
	return nil
}
