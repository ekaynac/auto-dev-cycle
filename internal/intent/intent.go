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
//   - Declassification is the record's, never the caller's (#226 entries
//     91(3), 94(2); 22a-6L), and granted only to a verified safe output
//     (Astra #321 H1): a reviewer's input whose record is a Sanitised
//     candidate (a gate's gate_result holding exactly its receipt and a
//     blob) whose receipt is strictly a gate receipt under its id, its
//     details bounded, or whose report blob is a gate report's bytes
//     exactly, its content checked (gate.CheckReport; Opus #321 r2 M2),
//     and a builder's whose record is a BuilderSafe
//     candidate (a review's view) whose bytes are the findings view
//     review.Result publishes of the no_go decision its lineage names,
//     recorded by a review, with the ids of the gate reports that
//     decision's lineage holds (review.CheckView), each blob read and its
//     digest checked, is checked itself against the acceptance blobs, and
//     its ancestry is not walked. A candidate that fails is walked as any
//     other input; any other consumer walks it. A lineage id that is an
//     acceptance digest refuses before it is resolved.
//   - Check re-reads a recorded intent the same way: the fold-side I18
//     check (#226 entry 91(1)). The consumer's role is never the intent's
//     own word (Opus #321 H1): CheckRecorded derives it from the intent's
//     attempt's node (a build's builder, a review's reviewer), refuses an
//     intent of another task, with no attempt, or stating another role,
//     and walks under the derived role.
//   - Records are Load's only, and immutable (Opus #321 L4, Astra #321
//     M4): their resolver, acceptance and reviewed digests are private,
//     read through methods that return copies; Records built otherwise
//     (the zero value) are refused.
//   - Check is the I18 check only (Opus #321 L6): it does not compare an
//     intent's Included with the node's rendered context, so a restricted
//     artifact that is not acceptance is the renderer's (graph.Context).
package intent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/gate"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/review"
)

var (
	// ErrAcceptanceInput: the context would carry a task's acceptance. The
	// intent is refused, never written with it omitted.
	ErrAcceptanceInput = errors.New("intent: an acceptance-derived ref among the inputs")
	// ErrAncestryUnresolved: an input's ancestry is not fully known, so it
	// cannot be shown free of acceptance; it refuses as ErrAcceptanceInput.
	ErrAncestryUnresolved = fmt.Errorf("%w: its ancestry is not resolvable", ErrAcceptanceInput)
	// ErrBlobDamaged: a blob's stored bytes are not its digest's. The
	// host's store, never the content: the run retries it, bounded, then
	// holds visibly, never "runner: failed" (Opus 22a-6d H1 re-check L2).
	ErrBlobDamaged = errors.New("intent: a blob's stored bytes are not its digest's")
	// ErrNotFresh: a reviewer's context would carry a review's result.
	ErrNotFresh = errors.New("intent: a review's result among a reviewer's inputs (a fresh context per round)")
	// ErrConsumer: a recorded intent's task, attempt or role is not its
	// consumer's.
	ErrConsumer = errors.New("intent: a recorded intent's task, attempt or role is not its consumer's")
	// ErrNotTheBrief: a plan node's ref that is not the plan brief of the
	// task's own spec, a root (#226 entry 117): refused as an input, or as
	// an ancestor, whatever the walk.
	ErrNotTheBrief = fmt.Errorf("%w: a plan node's ref is not its task's own brief", ErrAcceptanceInput)
)

// The walk's bounds, per intent (var: tests lower them).
var maxRecords, maxEdges, maxIDBytes = 10_000, 100_000, 4 << 20

// maxSafeBytes bounds a declassified output's blob verify reads: a gate
// report, a findings view or its decision.
const maxSafeBytes = 1 << 20

// Records are the authoritative records an intent is checked against: the
// task's ref records, its acceptance blobs' digests and the digests a
// review's result holds (Load), and the check that a declassification
// candidate is the safe output it claims to be. All private and never
// changed after Load (Astra #321 M4): read through Resolve, Acceptance and
// Reviewed, each a copy.
type Records struct {
	resolve    graph.RefRecords
	reviewed   map[types.Digest]bool
	acceptance []types.Digest // nil: not read
	safe       func(graph.RefRecord) (bool, error)
	brief      func(graph.RefRecord) error // a plan node's ref: nil when it is the task's own brief
}

// Load is task's Records in tx: graph.Records (with the controller's extra
// refs), the acceptance its own record holds, the task's own plan brief
// (its digest and its project's restriction), and blobs, which hold the
// declassified outputs verify reads. A task with no record is an error.
func Load(tx *eventstore.Tx, blobs receipt.Blobs, task types.TaskID, extra ...types.Ref) (Records, error) {
	rec, err := graph.Records(tx, task, extra...)
	if err != nil {
		return Records{}, err
	}
	if blobs == nil {
		return Records{}, errors.New("intent: no blobs to verify declassified outputs")
	}
	acc, err := authority.TaskAcceptance(tx, task)
	if err != nil {
		return Records{}, err
	}
	reviewed := map[types.Digest]bool{}
	for d, ok := range rec.Reviewed {
		reviewed[d] = ok
	}
	b, err := ownBrief(tx, task)
	if err != nil {
		return Records{}, err
	}
	return Records{resolve: rec.Resolve, reviewed: reviewed, acceptance: append([]types.Digest{}, acc...), safe: verifier(blobs, rec.Resolve),
		brief: b.check(blobs)}, nil
}

// loaded is whether r are Load's.
func (r Records) loaded() bool {
	return r.resolve != nil && r.acceptance != nil && r.safe != nil && r.brief != nil
}

// own is the task's own plan brief (#226 entry 117; Opus 22a-5c-3 H1, M1;
// r2 M1, L2): its project, the restriction its ref must carry (the
// project's data policy now, brief.Restriction over the recorded bytes),
// and its digest (brief.Of over the task's record and that policy, which
// binds the task, its spec digest, title and requirement and the policy's
// digest, as bytes). With no adopted profile (noPolicy), or a task record
// that makes no brief, a record of no spec (noBrief), there is no own
// brief: refused only once a plan node's ref is met.
type own struct {
	project     types.ProjectID
	digest      types.Digest
	restriction types.Restriction
	noPolicy    error
	noBrief     error
}

// ownBrief is task's own brief in tx: a task with no record, and a record
// or a profile the store cannot read or decode, is an error (r2 L1).
func ownBrief(tx *eventstore.Tx, task types.TaskID) (own, error) {
	t, err := authority.TaskOf(tx, task)
	if err != nil {
		return own{}, err
	}
	o := own{project: t.Project}
	policy, ok, err := authority.ProjectDataPolicy(tx, t.Project)
	switch {
	case err != nil:
		return own{}, err
	case !ok:
		o.noPolicy = fmt.Errorf("project %s has no adopted profile", t.Project)
		return o, nil
	}
	if o.restriction, err = brief.Restriction(t.Project, policy); err != nil {
		return own{}, err
	}
	_, o.digest, o.noBrief = brief.Of(t, o.restriction.PolicyDigest)
	return o, nil
}

// check is the plan-brief check of the task's records: a plan node's ref
// is honoured only as the task's own brief, a root blob in the task's
// project's scope whose id is its content digest and whose content digest
// is the own brief's (so its bytes are exactly the brief of the task's spec
// under the project's data policy now: never another task's, another
// spec's, a requirement that differs, another policy's, or other bytes of
// the same fields), with exactly that policy as its one restriction. A
// policy changed since the plan refuses the brief; no re-plan exists, so
// the recovery today is to revoke the task's approval, which abandons it.
// The bytes are read as an integrity check.
func (o own) check(blobs receipt.Blobs) func(graph.RefRecord) error {
	return func(rec graph.RefRecord) error {
		switch {
		case rec.Type != types.RefTypeBlob || len(rec.Lineage) != 0 || rec.ID != string(rec.ContentDigest):
			return fmt.Errorf("%w: %s is not a root blob", ErrNotTheBrief, rec.ID)
		case rec.Project != o.project || rec.Framework:
			return fmt.Errorf("%w: %s is not in the task's project's scope alone", ErrNotTheBrief, rec.ID)
		case o.noPolicy != nil:
			return fmt.Errorf("%w: %s: no data policy to check it against: %w", ErrNotTheBrief, rec.ID, o.noPolicy)
		case o.noBrief != nil:
			return fmt.Errorf("%w: %s: the task's record makes no brief: %w", ErrNotTheBrief, rec.ID, o.noBrief)
		case rec.ContentDigest != o.digest:
			return fmt.Errorf("%w: %s is not the brief of the task's spec under its project's data policy", ErrNotTheBrief, rec.ID)
		case len(rec.Restrictions) != 1 || !graph.SameRestriction(rec.Restrictions[0], o.restriction):
			return fmt.Errorf("%w: %s does not carry its project's data policy as its restriction", ErrNotTheBrief, rec.ID)
		}
		_, ok, err := readBlob(blobs, rec.ContentDigest, maxSafeBytes)
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%w: %s's bytes are not its digest's, or over %d bytes", ErrNotTheBrief, rec.ID, maxSafeBytes)
		}
		return nil
	}
}

// Resolve is id's records (graph.RefRecords), their declassifications the
// verified ones only.
func (r Records) Resolve(id string) ([]graph.RefRecord, bool, error) {
	if !r.loaded() {
		return nil, false, fmt.Errorf("%w: the records are not Load's", ErrAncestryUnresolved)
	}
	recs, ok, err := r.resolve(id)
	if err != nil || !ok {
		return nil, ok, err
	}
	for i, rec := range recs {
		if rec.Sanitised || rec.BuilderSafe {
			safe, err := r.safe(rec)
			if err != nil {
				return nil, false, err
			}
			recs[i].Sanitised, recs[i].BuilderSafe = rec.Sanitised && safe, rec.BuilderSafe && safe
		}
	}
	return recs, true, nil
}

// Acceptance is a copy of the task's acceptance blobs' digests (nil: not
// read).
func (r Records) Acceptance() []types.Digest { return slices.Clone(r.acceptance) }

// Reviewed is whether a review's result holds content d.
func (r Records) Reviewed(d types.Digest) bool { return r.reviewed[d] }

// verifier is the check that a declassification candidate is its safe
// output (Astra #321 H1), reading blobs: a Sanitised receipt whose bytes
// are a gate receipt exactly as gate.Result makes one (gate.CheckReceipt;
// Mythos #355 M1); a Sanitised blob whose bytes are a
// gate report's, its content checked (gate.CheckReport); a BuilderSafe
// view whose bytes are the findings view of the no_go decision its one
// lineage entry names, a blob a review recorded, redacted of the ids of
// the gate reports that decision's lineage holds (review.CheckView), each
// of that lineage's entries resolved (Opus #321 r2 L3). Each blob's
// digest is checked. False: not that output (it is walked); an error: a
// blob not read.
func verifier(blobs receipt.Blobs, resolve graph.RefRecords) func(graph.RefRecord) (bool, error) {
	report := func(rec graph.RefRecord) ([]byte, bool, error) {
		b, ok, err := readBlob(blobs, rec.ContentDigest, gate.MaxReportBytes)
		if err != nil || !ok || gate.CheckReport(b) != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	return func(rec graph.RefRecord) (bool, error) {
		switch {
		case rec.Sanitised && rec.Type == types.RefTypeReceipt:
			b, ok, err := readBlob(blobs, rec.ContentDigest, maxSafeBytes)
			if err != nil || !ok {
				return false, err
			}
			return gate.CheckReceipt(b, rec.ID) == nil, nil
		case rec.Sanitised && rec.Type == types.RefTypeBlob:
			_, ok, err := report(rec)
			return ok, err
		case !rec.BuilderSafe || rec.Type != types.RefTypeView || rec.Producer != types.NodeKindReview || len(rec.Lineage) != 1:
			return false, nil
		}
		decs, ok, err := resolve(rec.Lineage[0])
		if err != nil || !ok {
			return false, err
		}
		view, ok, err := readBlob(blobs, rec.ContentDigest, maxSafeBytes)
		if err != nil || !ok {
			return false, err
		}
		for _, dec := range decs { // a decision repeated by rounds: the view must be every record's (#226 entry 161, fail closed)
			if safe, err := viewOf(blobs, resolve, report, view, dec, rec.Lineage[0]); err != nil || !safe {
				return false, err
			}
		}
		return true, nil
	}
}

// viewOf is whether view is the findings view of dec, one record of the
// decision id names, redacted of the ids of the gate reports dec's lineage
// holds, each resolved.
func viewOf(blobs receipt.Blobs, resolve graph.RefRecords, report func(graph.RefRecord) ([]byte, bool, error), view []byte, dec graph.RefRecord, id string) (bool, error) {
	if dec.ID != id || dec.Producer != types.NodeKindReview || dec.Type != types.RefTypeBlob || dec.BuilderSafe || dec.Sanitised {
		return false, nil
	}
	gateIDs := []string{}
	for _, p := range dec.Lineage {
		prs, ok, err := resolve(p)
		if err != nil || !ok {
			return false, err
		}
		pr := prs[0] // a report's records differ in their lineage alone: one content
		if !pr.Sanitised || pr.Type != types.RefTypeBlob {
			continue
		}
		b, ok, err := report(pr)
		if err != nil || !ok {
			return false, err
		}
		ids, err := gate.ReportIDs(b)
		if err != nil {
			return false, nil
		}
		for _, id := range ids {
			if !slices.Contains(gateIDs, id) {
				gateIDs = append(gateIDs, id)
			}
		}
	}
	decision, ok, err := readBlob(blobs, dec.ContentDigest, maxSafeBytes)
	if err != nil || !ok {
		return false, err
	}
	return review.CheckView(view, decision, dec.ID, gateIDs) == nil, nil
}

// readBlob is blob d's bytes, at most limit+1 of them kept, false when
// they are over limit or are not d's content (then none: its digest is
// taken over the whole stream first); the one bounded, digest-checked
// reader of the package (Opus 22a-6b M3, 22a-6c L2). It reads the blob to
// its end: a blob is bounded where it is published (the store's upload
// bound), never by this read.
func readBlob(blobs receipt.Blobs, d types.Digest, limit int64) ([]byte, bool, error) {
	rc, err := blobs.Open(string(d))
	if err != nil {
		return nil, false, err
	}
	h := sha256.New() // over the whole stream: a blob past limit is still checked to be d's (Opus 22a-6c L2)
	b, err := io.ReadAll(io.LimitReader(io.TeeReader(rc, h), limit+1))
	if err == nil {
		_, err = io.Copy(h, rc)
	}
	if err = errors.Join(err, rc.Close()); err != nil {
		return nil, false, err
	}
	sum := h.Sum(nil)
	if !bytes.Equal([]byte(d), fmt.Appendf(nil, "sha256:%x", sum)) {
		return nil, false, nil // not d's: no bytes, whatever their size
	}
	return b, int64(len(b)) <= limit, nil
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
	if !in.Records.loaded() {
		return types.ManifestIntent{}, fmt.Errorf("%w: the records are not Load's", ErrAncestryUnresolved)
	}
	all := slices.Clone(in.Context.Included)
	for _, o := range in.Context.Omitted {
		all = append(all, o.Ref)
	}
	if err := walkAll(in.Records, role, all); err != nil {
		return types.ManifestIntent{}, err
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

// walkAll walks refs as role's inputs.
func walkAll(r Records, role string, refs []types.Ref) error {
	w := newWalk(r, role)
	for _, ref := range refs {
		rec, err := w.start(ref)
		if err != nil {
			return err
		}
		if role == "reviewer" && (rec.Producer == types.NodeKindReview || r.reviewed[rec.ContentDigest]) {
			return fmt.Errorf("%w: %s", ErrNotFresh, ref.ID)
		}
	}
	return nil
}

// Check is the fold-side I18 check of a recorded intent (#226 entry
// 91(1)): every ref it lists, as an input, included or omitted, walked
// under role, its consumer's, as Builder and Reviewer walk theirs. An
// intent stating another role is refused.
func Check(r Records, role string, mi types.ManifestIntent) error {
	switch {
	case !r.loaded():
		return fmt.Errorf("%w: no records", ErrAncestryUnresolved)
	case role != "builder" && role != "reviewer":
		return fmt.Errorf("%w: %q is no builder or reviewer", ErrConsumer, role)
	case mi.Consumer.Role != role:
		return fmt.Errorf("%w: it states %q, its consumer is a %s", ErrConsumer, mi.Consumer.Role, role)
	}
	refs := slices.Clone(mi.Inputs)
	for _, x := range mi.Included {
		refs = append(refs, x.Ref)
	}
	for _, x := range mi.Omitted {
		refs = append(refs, x.Ref)
	}
	return walkAll(r, role, refs)
}

// CheckRecorded is Check against task's records in tx (Load, verifying
// declassified outputs in blobs), under the role of the intent's attempt's
// node (consumer): what an intent's fold calls.
func CheckRecorded(tx *eventstore.Tx, blobs receipt.Blobs, task types.TaskID, mi types.ManifestIntent, extra ...types.Ref) error {
	role, err := consumer(tx, task, mi)
	if err != nil {
		return err
	}
	r, err := Load(tx, blobs, task, extra...)
	if err != nil {
		return err
	}
	return Check(r, role, mi)
}

// roleOf is the consumer role of each node kind that has one.
var roleOf = map[types.NodeKind]string{types.NodeKindBuild: "builder", types.NodeKindReview: "reviewer"}

// consumer is the role of mi's consumer, derived from what is recorded of
// its attempt (its node's kind), never from the intent's own word: an
// intent of another task than task, with no attempt, or whose attempt ran
// for no build or review node of task, is refused.
func consumer(tx *eventstore.Tx, task types.TaskID, mi types.ManifestIntent) (string, error) {
	switch {
	case mi.Task == nil || *mi.Task != task:
		return "", fmt.Errorf("%w: it is not task %s's", ErrConsumer, task)
	case mi.Attempt == nil:
		return "", fmt.Errorf("%w: it names no attempt", ErrConsumer)
	}
	n, ok, err := graph.AttemptNode(tx, *mi.Attempt)
	switch {
	case err != nil:
		return "", err
	case !ok || n.Task != task:
		return "", fmt.Errorf("%w: attempt %s ran for no node of task %s", ErrConsumer, *mi.Attempt, task)
	}
	role, ok := roleOf[n.Node.Kind]
	if !ok {
		return "", fmt.Errorf("%w: attempt %s ran for a %s node", ErrConsumer, *mi.Attempt, n.Node.Kind)
	}
	return role, nil
}

// walk is one intent's ancestry walk, shared by its inputs: an id walked
// for one input is not walked again, and an input already walked as
// another's ancestor is compared with its cached record, not charged
// again (Astra #289 M2).
type walk struct {
	role              string
	records           graph.RefRecords
	safe              func(graph.RefRecord) (bool, error) // a declassification candidate verified
	brief             func(graph.RefRecord) error         // a plan node's ref checked as the task's brief
	hidden            map[string]bool                     // acceptance digests
	walked            map[string]bool                     // ids scheduled: queued or resolved, each counted once
	shallow           map[string]bool                     // inputs declassified, so not expanded: expanded when reached as an ancestor
	resolved          map[string][]graph.RefRecord        // ids resolved and expanded: each id's records
	n, edges, idBytes int                                 // n: the ids scheduled
	peak              int                                 // the queue's longest
}

func newWalk(r Records, role string) *walk {
	w := &walk{role: role, records: r.resolve, safe: r.safe, brief: r.brief, hidden: map[string]bool{}, walked: map[string]bool{}, shallow: map[string]bool{}, resolved: map[string][]graph.RefRecord{}}
	for _, d := range r.acceptance {
		w.hidden[string(d)] = true
	}
	return w
}

// start resolves r's own records, one of which must be r (its content
// digest and lineage), then walks the ancestry of each (#226 entry 161:
// every record of an id, fail closed). An r already walked (an ancestor of
// an earlier input) is compared with its records only.
func (w *walk) start(r types.Ref) (graph.RefRecord, error) {
	recs, done := w.resolved[r.ID]
	if !done {
		w.idBytes += len(r.ID)
		var err error
		if err = w.schedule(r.ID, r.ID); err == nil {
			recs, err = w.resolve(r.ID, r.ID)
		}
		if err != nil {
			return graph.RefRecord{}, err
		}
	}
	rec := recs[0] // its records differ in their lineage alone
	// The input as listed is its record: its bytes, lineage, scope and
	// restrictions, so no copy of a record under fewer restrictions or
	// another scope passes as it (Mythos 22a-5c-3 M1).
	if rec.ContentDigest != r.ContentDigest || !slices.ContainsFunc(recs, func(x graph.RefRecord) bool { return slices.Equal(x.Lineage, r.Lineage) }) ||
		rec.Project != graph.ScopeProject(r.Scope) || rec.Framework != r.Scope.Framework || !slices.EqualFunc(rec.Restrictions, r.Restrictions, graph.SameRestriction) {
		return rec, fmt.Errorf("%w: %s is not the input its record names", ErrAncestryUnresolved, r.ID)
	}
	if done {
		return rec, nil
	}
	if w.role == "reviewer" && rec.Sanitised || w.role == "builder" && rec.BuilderSafe {
		safe := true
		for _, x := range recs { // every record verified
			ok, err := w.safe(x)
			if err != nil {
				return rec, err
			}
			safe = safe && ok
		}
		if safe {
			w.shallow[r.ID] = true
			return rec, nil // declassified for this consumer, verified: its own identity checked, its ancestry not walked
		}
	}
	var queue []string
	var err error
	for _, x := range recs {
		if queue, err = w.enqueue(r.ID, queue, x.Lineage); err != nil {
			return rec, err
		}
	}
	for err == nil && len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		var parents []graph.RefRecord
		if parents, err = w.resolve(r.ID, id); err == nil {
			for _, p := range parents {
				if queue, err = w.enqueue(r.ID, queue, p.Lineage); err != nil {
					break
				}
			}
		}
	}
	return rec, err
}

// resolve is id's record, its identity checked against the acceptance
// blobs: a digest check, apart from walking. It is cached.
func (w *walk) resolve(input, id string) ([]graph.RefRecord, error) {
	if w.hidden[id] { // an acceptance blob, which has no record: refused, never unresolved
		return nil, fmt.Errorf("%w: %s (ancestor %s is an acceptance blob)", ErrAcceptanceInput, input, id)
	}
	recs, ok, err := w.records(id)
	if err != nil {
		return nil, err
	}
	if !ok || len(recs) == 0 {
		return nil, fmt.Errorf("%w: %s (ancestor %s)", ErrAncestryUnresolved, input, id)
	}
	// An id's records differ in their lineage alone (graph.RefRecords), so
	// its first stands for each in the checks of its identity and content;
	// each lineage is walked (start), and the brief's check, which reads
	// the lineage (a root), runs on each (Opus 22a-6d H1 re-check L1).
	switch rec := recs[0]; {
	case rec.ID != id:
		return nil, fmt.Errorf("%w: %s (ancestor %s)", ErrAncestryUnresolved, input, id)
	case w.hidden[string(rec.ContentDigest)]:
		return nil, fmt.Errorf("%w: %s (ancestor %s holds acceptance %s)", ErrAcceptanceInput, input, id, rec.ContentDigest)
	case rec.Producer == types.NodeKindPlan:
		for _, r := range recs {
			if err := w.brief(r); err != nil {
				return nil, fmt.Errorf("%s (ancestor %s): %w", input, id, err)
			}
		}
	}
	w.resolved[id] = recs
	return recs, nil
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
		switch {
		case !w.walked[p]:
			if err := w.schedule(input, p); err != nil {
				return nil, err
			}
			queue = append(queue, p)
		case w.shallow[p]: // a declassified input, now an ancestor: walked through, whatever the inputs' order (Opus #321 L1)
			delete(w.shallow, p)
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
