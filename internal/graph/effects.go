// SPDX-License-Identifier: Apache-2.0

package graph

// Effects as operations (13 [G4] "The keying rule"; 03 "External effects";
// plan 15 G7a).
//
// Decisions:
//   - An effect node's goal is one effect operation, identified by its
//     request's digest: the node instance, its claim's key and inputs, the
//     target its params name and the target's want (the content digest of
//     the one ref its result input holds). The computation key alone never
//     finds an operation, so deploy A again is a new one; a resume
//     re-derives the request from the log and finds its effect.
//   - One operation is live (pending or uncertain) per target: Operate
//     intends nothing while another is, so a stale operation never lands
//     over a newer one. A new operation re-validates its claim's inputs
//     before its intent, as Commit does; a resume does not, since its
//     reconciliation must finish.
//   - RunEffect reconciles against the target before any egress, and its
//     apply is a compare-and-set on the value it observed.
//   - A kind that reconciles from its request keeps it (03 "Kept
//     requests"): the caller publishes EffectRequestOf before Operate. The
//     intent is recorded with node.operated, the graph's link from the node
//     to its effect; an invalidation waits while that effect is live, and
//     RunEffect runs only the node's current request.
//   - The node records no graph result: the effect's state is the outcome,
//     and no node takes a deploy's result until G7b defines effect-node
//     completion. A failed_confirmed effect stays the node's until an
//     invalidation; concurrent runners of one effect are M1's (13).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// EffectTarget is where an effect lands (a deploy target, 03): what a
// target holds now, and making it hold want under an effect id while it
// still holds expected, the value the caller observed (compare-and-set:
// applied is false, and nothing changed, when it no longer does).
type EffectTarget interface {
	Holds(target string) (string, error)
	Apply(target, effect, want, expected string) (applied bool, err error)
}

// EffectRequest is an effect operation's request, its identity.
type EffectRequest struct {
	Node   types.NodeID         `json:"node"`
	Key    types.ComputationKey `json:"key"`
	Inputs []types.ClaimedInput `json:"inputs"`
	Target string               `json:"target"`
	Want   types.Digest         `json:"want"`
}

// Operation is node's effect operation: its effect, of Kind, for Request.
type Operation struct {
	Node    types.NodeID
	Effect  string
	Kind    string
	Request EffectRequest
}

// ErrTargetBusy: effect Live on Target is pending or uncertain, so no
// operation is intended there; the caller waits for it, structurally.
type ErrTargetBusy struct{ Target, Live string }

func (e ErrTargetBusy) Error() string {
	return fmt.Sprintf("graph: effect %s is live on target %s", e.Live, e.Target)
}

// ErrTargetMoved: the target no longer held what RunEffect observed, so
// nothing was applied; the effect is uncertain and reconciles when run again.
type ErrTargetMoved struct{ Target string }

func (e ErrTargetMoved) Error() string {
	return "graph: target " + e.Target + " moved before the apply"
}

// ErrAmbiguousEffect: two recorded effects where one is allowed.
type ErrAmbiguousEffect struct{ Detail string }

func (e ErrAmbiguousEffect) Error() string { return "graph: ambiguous_state: " + e.Detail }

// Result is the refusal: could_not_run/ambiguous_state (03 rule 2).
func (e ErrAmbiguousEffect) Result() vocab.Result {
	return vocab.Result{Outcome: vocab.CouldNotRun, Reason: vocab.ReasonAmbiguousState, Detail: e.Detail}
}

func ambiguous(err error) error {
	if errors.Is(err, eventstore.ErrAmbiguousEffect) {
		return ErrAmbiguousEffect{err.Error()}
	}
	return err
}

// effectTarget is the target an effect node's params name (read
// non-strictly: only a non-empty target is required).
func effectTarget(n types.Node) (string, error) {
	var p struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(n.Params, &p); err != nil || p.Target == "" {
		return "", errors.Join(err, errors.New("graph: an effect node's params name its target"))
	}
	return p.Target, nil
}

// effectRequest is node's request, from its claim, and its effect kind.
func effectRequest(tx *eventstore.Tx, node types.NodeID) (EffectRequest, string, error) {
	rec, err := loadNode(tx, node)
	c, claimed, cerr := load[claim](tx, ClaimProjection, string(node))
	if err = errors.Join(err, cerr); err != nil || !claimed || rec.State.State != types.NodeStateStateClaimed || rec.Recorded.Node.Effect == nil {
		return EffectRequest{}, "", errors.Join(err, fmt.Errorf("graph: %s is not a claimed effect node", node))
	}
	target, err := effectTarget(rec.Recorded.Node)
	if err != nil {
		return EffectRequest{}, "", err
	}
	r := EffectRequest{Node: node, Key: c.Key, Inputs: c.Inputs, Target: target}
	for _, in := range c.Inputs {
		if in.Result == nil {
			continue
		}
		res, _, err := load[types.ResultRecorded](tx, ResultProjection, string(*in.Result))
		if err != nil || len(res.Result.Value) != 1 || r.Want != "" {
			return EffectRequest{}, "", errors.Join(err, errors.New("graph: an effect node's one result input holds one ref, what its target is to hold"))
		}
		r.Want = res.Result.Value[0].ContentDigest
	}
	if r.Want == "" {
		return EffectRequest{}, "", errors.New("graph: an effect node takes the result its target is to hold")
	}
	if mutation.Is(mutation.EffectKeyedByComputation) {
		r.Node = ""
	}
	return r, string(*rec.Recorded.Node.Effect), nil
}

// Operate is node's effect operation in tx, a command's: the effect
// already intended for its request, or a new one, intended once the
// target has no live effect and node's inputs re-validate. node is a
// claimed effect node.
func Operate(tx *eventstore.Tx, node types.NodeID, env Env) (Operation, error) {
	if tx.Now().IsZero() {
		return Operation{}, errors.New("graph: an operation runs in a command's transaction")
	}
	r, kind, err := effectRequest(tx, node)
	if err != nil {
		return Operation{}, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return Operation{}, err
	}
	op := Operation{Node: node, Kind: kind, Request: r}
	if e, ok, err := tx.EffectFor(kind, b); err != nil || ok && !mutation.Is(mutation.EffectResumeReallocates) {
		op.Effect = e.ID
		return op, ambiguous(err)
	}
	live, busy, err := tx.LiveEffect(kind, r.Target)
	if err != nil {
		return op, ambiguous(err)
	}
	if busy && !mutation.Is(mutation.EffectTargetUnfenced) {
		return op, ErrTargetBusy{r.Target, live.ID}
	}
	if err := revalidate(tx, node, env); err != nil {
		return op, err
	}
	if op.Effect, err = ids.New("eff"); err != nil {
		return Operation{}, err
	}
	evs, err := onNode(tx, node, NodeOperated, string(node), &types.NodeOperated{Type: "node_operated", Node: node, Key: r.Key, Effect: types.EffectID(op.Effect)})
	if err == nil {
		err = tx.IntendEffect(op.Effect, kind, r.Target, b)
	}
	if err != nil {
		return op, err
	}
	return op, tx.Emit(evs...)
}

// EffectRequestOf is the RFC 8785 form of node's effect request: what a
// kind that keeps its request publishes as a blob before Operate.
func EffectRequestOf(tx *eventstore.Tx, node types.NodeID) ([]byte, error) {
	r, _, err := effectRequest(tx, node)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return eventstore.Canonical(b)
}

// ErrEffectLive: node's operation has an effect pending or uncertain, so
// the node is not invalidated until it settles.
type ErrEffectLive struct {
	Node   types.NodeID
	Effect string
}

func (e ErrEffectLive) Error() string {
	return fmt.Sprintf("graph: %s's effect %s is live", e.Node, e.Effect)
}

// settledOperation: node's operation, if any, has no live effect.
func settledOperation(tx *eventstore.Tx, node types.NodeID) error {
	op, ok, err := load[types.NodeOperated](tx, EffectProjection, string(node))
	if err != nil || !ok {
		return err
	}
	e, err := tx.LoadEffect(string(op.Effect))
	if err == nil && (e.State == eventstore.EffectPending || e.State == eventstore.EffectUncertain) {
		err = ErrEffectLive{node, e.ID}
	}
	return err
}

// operated folds node.operated: an operation of a claimed effect node at
// its claim's key, once per key.
func operated(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeOperated) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeOperated || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("an operation is under its node"))
	}
	if rec.State.State != types.NodeStateStateClaimed || rec.Recorded.Node.Effect == nil || *rec.State.Key != p.Key {
		return errors.New("an operation is a claimed effect node's, at its claim's key")
	}
	if prev, ok, err := load[types.NodeOperated](tx, EffectProjection, string(p.Node)); err != nil || ok && prev.Key == p.Key {
		return errors.Join(err, errors.New("the node's operation at this key is recorded"))
	}
	// The intent's row is in the transaction (the writer inserts it before
	// the folds run), and an effect's kind and target never change.
	e, err := tx.LoadEffect(string(p.Effect))
	target, terr := effectTarget(rec.Recorded.Node)
	if err = errors.Join(err, terr); err != nil || e.Kind != string(*rec.Recorded.Node.Effect) || e.Target != target {
		return errors.Join(err, errors.New("the operation's effect is not of the node's kind, on its target"))
	}
	return put(tx, EffectProjection, string(p.Node), p)
}

// takesNoDeploy: no input of n is a deploy's result, until G7b defines
// effect-node completion (13): n takes no recorded deploy, and a deploy n
// is taken by no recorded node (a consumer recorded before its producer).
// The scan for the latter is linear in the graph's nodes; M0's graphs are
// small, and an index by producer can come with M1's wiring.
func takesNoDeploy(tx *eventstore.Tx, n types.Node) error {
	if n.Kind == types.NodeKindDeploy {
		keys, err := tx.Keys(NodeProjection, "")
		for _, k := range keys {
			rec, _, lerr := load[nodeRecord](tx, NodeProjection, k)
			if err = errors.Join(err, lerr); err == nil && slices.ContainsFunc(rec.Recorded.Node.Inputs, func(in types.InputSpec) bool {
				src, ok := in.Source.Variant.(*types.ResultSource)
				return ok && src.Producer == n.ID
			}) {
				err = fmt.Errorf("node %s takes this deploy's result, which no node takes before G7b", k)
			}
			if err != nil {
				return err
			}
		}
	}
	for _, in := range n.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok {
			prod, found, err := load[nodeRecord](tx, NodeProjection, string(src.Producer))
			if err != nil || found && prod.Recorded.Node.Kind == types.NodeKindDeploy {
				return errors.Join(err, fmt.Errorf("input %s takes a deploy's result, which no node takes before G7b", in.Name))
			}
		}
	}
	return nil
}

// RunEffect carries op's effect to target, by actor, after the command that
// intended it committed, and returns the effect's state.
func RunEffect(ctx context.Context, s *eventstore.Store, actor string, op Operation, target EffectTarget) (eventstore.Effect, error) {
	record := func(tr eventstore.Transition) (eventstore.Effect, error) {
		return s.RecordEffect(ctx, actor, op.Effect, tr)
	}
	b, err := json.Marshal(op.Request)
	d, derr := eventstore.ContentDigest(b)
	e, lerr := s.LoadEffect(ctx, op.Effect)
	if err = errors.Join(err, derr, lerr); err == nil && (e.RequestDigest != d || e.Kind != op.Kind) {
		err = fmt.Errorf("graph: effect %s was intended for another request", op.Effect)
	}
	// The current request is read in a view, apart from the transitions
	// below. That is safe because the node's claim cannot change while its
	// effect is live: the invalidation of a node with a live effect is
	// refused, by its builder and its fold (settledOperation), and only a
	// pending or uncertain effect goes on past the state gate.
	var cur []byte
	if err == nil {
		err = s.View(ctx, func(tx *eventstore.Tx) (err error) { cur, err = EffectRequestOf(tx, op.Node); return err })
	}
	if c, cerr := eventstore.ContentDigest(cur); err == nil && (cerr != nil || c != d) {
		err = errors.Join(cerr, fmt.Errorf("graph: effect %s's request is not %s's current one", op.Effect, op.Node))
	}
	if err != nil || e.State != eventstore.EffectPending && e.State != eventstore.EffectUncertain {
		return e, err
	}
	holds, err := target.Holds(op.Request.Target)
	if err != nil {
		return e, err
	}
	want := string(op.Request.Want)
	if e.State == eventstore.EffectUncertain {
		if holds == want {
			return record(eventstore.Transition{Kind: "reconciled", Outcome: "completed", Observation: "the target holds the request's want"})
		}
		const missing = "the target does not hold the request's want"
		if k, _ := s.EffectKind(e.Kind); !k.Idempotent { // never blindly retried
			if e.LastObservation == missing {
				return e, nil // nothing changed: nothing to record
			}
			return record(eventstore.Transition{Kind: "reconciled", Outcome: "still_uncertain", Observation: missing})
		}
		if e, err = record(eventstore.Transition{Kind: "retried", Observation: missing}); err != nil {
			return e, err
		}
	}
	if holds == want && !mutation.Is(mutation.EffectPendingSkipsTarget) {
		return record(eventstore.Transition{Kind: "observed", Observation: "the target already holds the request's want"})
	}
	expected := holds
	if mutation.Is(mutation.EffectApplyBlind) {
		expected, _ = target.Holds(op.Request.Target)
	}
	applied, err := target.Apply(op.Request.Target, op.Effect, want, expected)
	if err == nil && applied {
		return record(eventstore.Transition{Kind: "observed", Observation: "applied"})
	}
	obs := "the target moved from the value observed"
	if err != nil {
		obs = "the apply failed: " + err.Error()
	} else {
		err = ErrTargetMoved{op.Request.Target}
	}
	e, rerr := record(eventstore.Transition{Kind: "uncertain", Observation: obs})
	return e, errors.Join(err, rerr)
}
