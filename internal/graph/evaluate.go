// SPDX-License-Identifier: Apache-2.0

package graph

// The evaluator (13 "Evaluation", [G4], [G10]; plan 15 G4b).
//
// Decisions:
//   - Evaluation is demand-driven: from its goals it walks each node's
//     producers first, once per node, and claims every node whose inputs
//     all resolve. In one command it claims a node once and a memoizable
//     key once (the fold would refuse a second, 13 [G10]); another instance
//     of a key this command claimed waits, and every instance of a key
//     whose holder is completed reuses its result.
//   - A demanded completed memoizable node whose selected receipt has
//     passed its bound valid_until at the transaction's time is renewed,
//     through the holder of its key (13 [G1] renewal); its dependants wait
//     for the fresher result.
//   - A missing internal input is a structural wait: the node stays waiting
//     and records nothing. A missing answer raises awaiting_input, tied to
//     its owed input: the hold's signal instance is "<owed input>@<revision>"
//     of an answer input the node declares, and its owner is the owed
//     input's grant holder. Once the authority answers, the evaluation
//     clears the hold on that signal and claims in the same command.
//   - Commit re-validates authority and applicability in the writer
//     transaction that records the node's result (13 [G4]): every input must
//     still resolve, at the transaction's time, to what the claim fixed,
//     checked in declared order. An answer whose approval no longer answers
//     its owed input is authority_revoked; a receipt that no longer applies,
//     the resolver's refusal; a result no longer selected,
//     stale_expected_state. Commit emits the events itself (Tx.Emit) and
//     hands none back, so no caller holds validated events past their
//     transaction; it is the only exported way to record a result. The
//     result folds refuse a result whose claim's result inputs are no
//     longer selected; the authority is not checked by a fold until M1
//     records approvals.
//   - Owed inputs and their answers are the authority's (04): an Authority
//     the caller passes, until M1's input.answer binds them (15 D1). A
//     receipt owed input is not an answer: its node declares the evidence
//     node's result instead, and the owed input names that node (E1).

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Authority answers owed inputs (04 "Owed inputs"). It reads the current
// state of the transaction it is given, and it alone decides who answers:
// only an input.answer approval for this owed input at this revision, from
// a responder authorised in rec's project, counts.
type Authority interface {
	// Answer is the approval in force that answers owed at revision for
	// rec's node, if any.
	Answer(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64) (types.ApprovalID, bool, error)
	// Owner is the actor owed waits on: the grant holder of its hold.
	Owner(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID) (types.ActorID, error)
	// Owed are the owed inputs rec's node asked for (their requested_by), at
	// their current revisions, each as the answer source a successor
	// declares for it.
	Owed(tx *eventstore.Tx, rec types.NodeRecorded) ([]types.AnswerSource, error)
	// Approval is the sealed Ref to approval's answer artifact (04), which
	// must answer owed at revision in rec's project.
	Approval(tx *eventstore.Tx, rec types.NodeRecorded, owed types.OwedInputID, revision int64, approval types.ApprovalID) (types.Ref, error)
}

// Env is what evaluation reads besides the store: the consuming kinds'
// requirements, the receipt blobs and the authority.
type Env struct {
	Need      Requirements
	Blobs     receipt.Blobs
	Authority Authority
	Contracts Contracts // the instrument contracts evidence nodes run under
}

// ErrRevalidated: an input no longer resolves to what the claim fixed, so
// the result is not committed.
type ErrRevalidated struct {
	Input  types.Name
	Reason vocab.Reason
	Detail string
}

func (e ErrRevalidated) Error() string {
	return fmt.Sprintf("graph: input %s no longer holds at commit (%s): %s", e.Input, e.Reason.String(), e.Detail)
}

// awaitSignal is the signal instance of an awaiting_input hold on ask.
func awaitSignal(ask *types.AnswerSource) string {
	return fmt.Sprintf("%s@%d", ask.OwedInput, ask.Revision)
}

// answers are rec's answer inputs the authority answers now. Each must
// answer an owed input rec's lineage asked for, at its current revision
// (ErrNotOwed), whether rec declared it or inherited it.
func answers(tx *eventstore.Tx, rec nodeRecord, auth Authority) (map[string]Resolved, error) {
	out := map[string]Resolved{}
	owed, err := lineageOwed(tx, rec.Recorded.Predecessor, auth)
	if err != nil {
		return nil, err
	}
	for _, in := range rec.Recorded.Node.Inputs {
		if ask, ok := in.Source.Variant.(*types.AnswerSource); ok {
			if !owedIn(owed, ask) && !mutation.Is(mutation.AnswerLineageUnchecked) {
				return nil, ErrNotOwed{ask.OwedInput}
			}
			apr, answered, err := auth.Answer(tx, rec.Recorded, ask.OwedInput, ask.Revision)
			if err != nil {
				return nil, err
			}
			if answered {
				out[in.Name] = Resolved{Approval: apr, Revision: ask.Revision}
			}
		}
	}
	return out, nil
}

type evaluation struct {
	tx   *eventstore.Tx
	env  Env
	seen map[types.NodeID]bool
	keys map[string]bool // keys this command claimed
	// renewed: the nodes this command renewed, holders and the demanded
	// nodes bound to them, which are no producer's result until they
	// complete again.
	renewed map[types.NodeID]bool
	evs     []eventstore.Event
}

// Evaluate is the events that advance goals: claims, reuses, and the
// awaiting_input holds raised or cleared, producers first.
func Evaluate(tx *eventstore.Tx, goals []types.NodeID, env Env) ([]eventstore.Event, error) {
	e := &evaluation{tx: tx, env: env, seen: map[types.NodeID]bool{}, keys: map[string]bool{}, renewed: map[types.NodeID]bool{}}
	for _, g := range goals {
		if err := e.visit(g); err != nil {
			return nil, err
		}
	}
	return e.evs, nil
}

func (e *evaluation) visit(n types.NodeID) error {
	if e.seen[n] {
		return nil
	}
	e.seen[n] = true
	rec, err := loadNode(e.tx, n)
	switch {
	case err != nil || rec.State.State == types.NodeStateStateClaimed:
		return err
	case rec.State.State == types.NodeStateStateCompleted:
		return e.renew(rec)
	}
	waits := false
	for _, in := range rec.Recorded.Node.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok {
			if err := e.visit(src.Producer); err != nil {
				return err
			}
			waits = waits || e.renewed[src.Producer]
		}
	}
	if waits {
		return nil // a producer renewed in this command: a structural wait
	}
	answered, err := answers(e.tx, rec, e.env.Authority)
	if err != nil {
		return err
	}
	if held, err := e.clear(rec, answered); err != nil || held {
		return err
	}
	resolved, missing, err := Resolve(e.tx, n, answered, e.env.Need, e.env.Blobs, e.tx.Now())
	if err != nil || len(missing) > 0 {
		return errors.Join(err, e.await(rec, missing))
	}
	return e.claim(rec, resolved)
}

// clear clears the awaiting_input holds whose answer arrived; held: a hold
// remains.
func (e *evaluation) clear(rec nodeRecord, answered map[string]Resolved) (held bool, err error) {
	for _, h := range rec.State.Holds {
		i := slices.IndexFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool {
			ask, ok := in.Source.Variant.(*types.AnswerSource)
			return ok && h.Hold == "awaiting_input" && h.SignalInstance == awaitSignal(ask)
		})
		if i < 0 || answered[rec.Recorded.Node.Inputs[i].Name].Approval == "" {
			held = true
			continue
		}
		evs, err := Clear(e.tx, rec.Recorded.Node.ID, h.ID, h.SignalInstance)
		if err != nil {
			return true, err
		}
		e.evs = append(e.evs, evs...)
	}
	return held, nil
}

// await raises awaiting_input for each missing answer not yet awaited.
func (e *evaluation) await(rec nodeRecord, missing []Missing) error {
	for _, m := range missing {
		i := slices.IndexFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool { return in.Name == m.Name })
		ask, ok := rec.Recorded.Node.Inputs[i].Source.Variant.(*types.AnswerSource)
		if !ok {
			continue // a structural wait; a held node is not here (clear)
		}
		owner, err := e.env.Authority.Owner(e.tx, rec.Recorded, ask.OwedInput)
		id, ierr := ids.New("hld")
		if err = errors.Join(err, ierr); err != nil {
			return err
		}
		evs, err := Raise(e.tx, rec.Recorded.Node.ID, types.NodeHold{ID: types.HoldID(id), Hold: "awaiting_input", SignalInstance: awaitSignal(ask),
			Owner: types.OwnerGrantHolder, OwnerRef: ptrOf(string(owner)), Pages: []types.NodeHoldPagesItem{}}, nil)
		if err != nil {
			return err
		}
		e.evs = append(e.evs, evs...)
	}
	return nil
}

// claim claims rec, once per memoizable key in the command, or binds it to
// the completed holder of its key.
func (e *evaluation) claim(rec nodeRecord, resolved map[string]Resolved) error {
	index := ""
	if slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		cur, err := current(e.tx, rec, resolved)
		key, kerr := Key(rec.Recorded.Node, resolved, cur)
		if err = errors.Join(err, kerr); err != nil {
			return err
		}
		if index = keyIndex(rec.Recorded.Task, key.Key); e.keys[index] {
			return nil // claimed by this command: reuse once its holder completes
		}
	}
	var evs []eventstore.Event
	var err error
	if rec.Recorded.Node.Kind == types.NodeKindEvidence {
		evs, err = ClaimEvidence(e.tx, rec.Recorded.Node.ID, resolved, 0, e.env.Contracts)
	} else {
		evs, err = Claim(e.tx, rec.Recorded.Node.ID, resolved, 0)
	}
	var taken ErrKeyClaimed
	if errors.As(err, &taken) {
		holder, herr := loadNode(e.tx, taken.Node)
		if herr != nil || holder.State.State != types.NodeStateStateCompleted {
			return herr // the holder runs: wait for it
		}
		evs, err = Reuse(e.tx, rec.Recorded.Node.ID, resolved)
	} else if index != "" { // claimed now: the node was unclaimed (visit)
		e.keys[index] = true
	}
	e.evs = append(e.evs, evs...)
	return err
}

// renew renews rec's key through its holder once the holder's selected
// receipt has passed the validity it bound, at the transaction's time.
// Only a memoizable node's key has a holder.
func (e *evaluation) renew(rec nodeRecord) error {
	holder, _, err := load[types.NodeID](e.tx, KeyProjection, keyIndex(rec.Recorded.Task, rec.State.Key.Key))
	r, _, rerr := load[types.ResultRecorded](e.tx, ResultProjection, string(*rec.State.Result))
	if err = errors.Join(err, rerr); err != nil || holder == "" || r.ReceiptValidUntil == nil {
		return err
	}
	if expired, err := expiredAt(*r.ReceiptValidUntil, e.tx.Now().UTC().Format(time.RFC3339Nano)); err != nil || !expired {
		return err
	}
	if rec.Recorded.Node.Kind == types.NodeKindEvidence { // only a due renewal asks for the contract
		if _, err := contractOf(rec, e.env.Contracts); err != nil {
			return err
		}
	}
	again := e.renewed[holder]
	e.renewed[holder], e.renewed[rec.Recorded.Node.ID] = true, true
	if again {
		return nil
	}
	evs, err := Renew(e.tx, holder, e.env.Blobs)
	e.evs = append(e.evs, evs...)
	return err
}

// Commit records r, node's result, in tx, after re-validating there that
// every input still resolves to what its claim fixed: the result is
// recorded, and selected unless it is could_not_run. It emits the events
// itself (Tx.Emit), so it runs only in a command's transaction.
func Commit(tx *eventstore.Tx, node types.NodeID, r types.Result, env Env) error {
	if tx.Now().IsZero() {
		return errors.New("graph: a commit runs in a command's transaction")
	}
	if err := revalidate(tx, node, env); err != nil {
		return err
	}
	evs, err := recordResult(tx, types.ResultRecorded{Node: node, Result: r}, env.Blobs)
	if err == nil && r.Outcome != types.OutcomeCouldNotRun {
		var sel []eventstore.Event
		sel, err = selectResult(tx, node, r.ID)
		evs = append(evs, sel...)
	}
	if err != nil {
		return err
	}
	return tx.Emit(evs...)
}

// revalidate: every input of node's claim still resolves, in tx at its
// time, to what the claim fixed (13 [G4]), checked in declared order; else
// ErrRevalidated. Commit and Operate (before egress) run it.
func revalidate(tx *eventstore.Tx, node types.NodeID, env Env) error {
	c, ok, err := load[claim](tx, ClaimProjection, string(node))
	rec, rerr := loadNode(tx, node)
	if err = errors.Join(err, rerr); err != nil || !ok {
		return errors.Join(err, fmt.Errorf("graph: %s is not claimed", node))
	}
	answered, err := answers(tx, rec, env.Authority)
	if err != nil {
		return err
	}
	resolved, missing, err := Resolve(tx, node, answered, env.Need, env.Blobs, tx.Now())
	if err != nil {
		return err
	}
	fixed := resolutions(c.Inputs)
	for _, in := range c.Inputs {
		name, want := string(in.Name), fixed[string(in.Name)]
		if got, ok := resolved[name]; ok && got == want || want.Approval != "" && mutation.Is(mutation.CommitSkipsAuthority) {
			continue
		}
		reason, detail := vocab.ReasonStaleExpectedState, "the input is not the claimed one"
		if i := slices.IndexFunc(missing, func(m Missing) bool { return string(m.Name) == name }); i >= 0 {
			if detail = missing[i].Detail; missing[i].Refusal != nil {
				reason = missing[i].Refusal.Reason
			}
		}
		if want.Approval != "" {
			reason = vocab.ReasonAuthorityRevoked
		}
		return ErrRevalidated{Input: types.Name(name), Reason: reason, Detail: detail}
	}
	return nil
}
