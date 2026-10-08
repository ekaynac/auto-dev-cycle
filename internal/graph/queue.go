// SPDX-License-Identifier: Apache-2.0

package graph

// Merge-queue generations (13 "Merge-queue generations"; plan 15 G7b-1).
//
// Decisions:
//   - A queue generation is the nodes that try to merge one candidate onto
//     one base head of one branch. Its rebase node pins the base head in its
//     params {branch, base, generation}; its carry_over node takes the
//     rebase's tree. G7b-2 adds the refreshed receipts and the merge.
//   - Generations are numbered per task and branch, from 1. The node.recorded
//     fold admits a rebase node only as the next generation and only on
//     another base than the current one: a base move starts a generation,
//     and nothing else does. Earlier generations stay, as history.
//   - Generate is the template: it records the next generation when the
//     base head observed on the forge is not the current generation's.
//   - A rebase result's conflict status is its outcome: clean, it holds the
//     git tree and the rebased patch; found, their resolution too. Its refs
//     are typed. A patch's identity is its one ref's content digest.
//   - carryDecision is the one function of a carry_over's claimed inputs:
//     an attestation holding the prospective tree only when the carried
//     review took the candidate's patch, the rebase is clean and the rebased
//     patch has the candidate's identity; else the demand for a delta review
//     (found/review_input_changed). CarryOver applies it, and the result fold
//     refuses any other outcome, reason or value; Commit refuses carry_over.
//   - Effect-node completion: CompleteEffect records and selects an effect
//     node's result once its operation's effect is completed, holding the
//     input ref the effect applied. Its inputs were re-validated at the
//     intent, so the result fold checks the completed operation and that
//     value, not the inputs; Commit refuses effect nodes. A deploy has no
//     result kind, so it records none and is still never a producer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// GenerationProjection holds each task and branch's current generation,
// keyed task/branch: a queue is per project and branch (13), and a task's
// generations are its candidate's attempts on that branch.
const GenerationProjection = "graph_generations"

// rebaseParams are a rebase node's params: the generation it starts.
type rebaseParams struct {
	Branch     string `json:"branch"`
	Base       string `json:"base"`
	Generation int64  `json:"generation"`
}

// Generation is a queue generation: its number, the base head it pins and
// its nodes.
type Generation struct {
	Number    int64        `json:"number"`
	Base      string       `json:"base"`
	Rebase    types.NodeID `json:"rebase"`
	CarryOver types.NodeID `json:"carry_over"`
}

// QueueSpec is what a queue generation tries to merge: the candidate's
// patch producer and its review, onto a branch, in a task of a project.
type QueueSpec struct {
	Task      types.TaskID
	Project   types.ProjectID
	Branch    string
	Candidate types.NodeID
	Review    types.NodeID
}

func generationKey(task types.TaskID, branch string) string { return string(task) + "/" + branch }

// generationRecorded folds a rebase node's record: the next generation of
// its task and branch, on another base than the current one.
func generationRecorded(tx *eventstore.Tx, p *types.NodeRecorded) error {
	if p.Node.Kind == types.NodeKindCarryOver {
		return carryRecorded(tx, p)
	}
	if p.Node.Kind != types.NodeKindRebase {
		return nil
	}
	var rp rebaseParams
	if err := contract.UnmarshalStrict(p.Node.Params, &rp); err != nil || rp.Branch == "" || rp.Base == "" {
		return errors.Join(err, errors.New("a rebase node's params are its branch, base and generation"))
	}
	key := generationKey(p.Task, rp.Branch)
	cur, ok, err := load[Generation](tx, GenerationProjection, key)
	if err != nil || ok && (rp.Generation != cur.Number+1 || rp.Base == cur.Base) || !ok && rp.Generation != 1 {
		return errors.Join(err, fmt.Errorf("generation %d on %s does not follow generation %d on %s: only a base move starts one", rp.Generation, rp.Base, cur.Number, cur.Base))
	}
	return put(tx, GenerationProjection, key, Generation{Number: rp.Generation, Base: rp.Base, Rebase: p.Node.ID})
}

// producerOf is the producer of n's result input name ("": none).
func producerOf(n types.Node, name string) types.NodeID {
	for _, in := range n.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && in.Name == name {
			return src.Producer
		}
	}
	return ""
}

// carryRecorded admits a carry_over node only on its current generation's
// rebase, a rebase of the carry_over's own candidate producer, and keeps it
// in that generation.
func carryRecorded(tx *eventstore.Tx, p *types.NodeRecorded) error {
	rbID, cand := producerOf(p.Node, "rebase"), producerOf(p.Node, "candidate")
	rb, found, err := load[nodeRecord](tx, NodeProjection, string(rbID))
	var rp rebaseParams
	if err != nil || !found || cand == "" || rb.Recorded.Node.Kind != types.NodeKindRebase || producerOf(rb.Recorded.Node, "candidate") != cand ||
		json.Unmarshal(rb.Recorded.Node.Params, &rp) != nil {
		return errors.Join(err, errors.New("a carry_over takes a rebase node of its own candidate"))
	}
	key := generationKey(p.Task, rp.Branch)
	cur, ok, err := load[Generation](tx, GenerationProjection, key)
	if err != nil || !ok || cur.Rebase != rbID {
		return errors.Join(err, errors.New("a carry_over takes its current generation's rebase"))
	}
	cur.CarryOver = p.Node.ID
	return put(tx, GenerationProjection, key, cur)
}

// Generate is the events recording spec's next generation on head, the
// base head observed on the forge, and that generation; none when head is
// the current generation's base.
func Generate(tx *eventstore.Tx, spec QueueSpec, head string) ([]eventstore.Event, Generation, error) {
	if head == "" {
		return nil, Generation{}, errors.New("graph: no base head was observed")
	}
	cur, ok, err := load[Generation](tx, GenerationProjection, generationKey(spec.Task, spec.Branch))
	if err != nil || ok && (cur.Base == head || mutation.Is(mutation.GenerationKeepsBase)) {
		return nil, Generation{}, err
	}
	g := Generation{Number: cur.Number + 1, Base: head}
	params, err := json.Marshal(rebaseParams{Branch: spec.Branch, Base: head, Generation: g.Number})
	rb, rerr := ids.New("nod")
	co, cerr := ids.New("nod")
	if err = errors.Join(err, rerr, cerr); err != nil {
		return nil, Generation{}, err
	}
	g.Rebase, g.CarryOver = types.NodeID(rb), types.NodeID(co)
	input := func(name string, producer types.NodeID, kind types.ResultKind) types.InputSpec {
		return types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}}}
	}
	node := func(id types.NodeID, kind types.NodeKind, params json.RawMessage, inputs ...types.InputSpec) types.NodeRecorded {
		return types.NodeRecorded{Task: spec.Task, Project: spec.Project, Node: types.Node{ID: id, Kind: kind, DefinitionVersion: "1", Round: 1,
			Params: params, Inputs: inputs, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	}
	candidate := input("candidate", spec.Candidate, types.ResultKindPatch)
	var evs []eventstore.Event
	for _, n := range []types.NodeRecorded{
		node(g.Rebase, types.NodeKindRebase, params, candidate),
		node(g.CarryOver, types.NodeKindCarryOver, json.RawMessage(`{}`), input("review", spec.Review, types.ResultKindReview), candidate,
			input("rebase", g.Rebase, types.ResultKindTree)),
	} {
		ev, err := Record(n)
		if err != nil {
			return nil, Generation{}, err
		}
		evs = append(evs, ev...)
	}
	return evs, g, nil
}

// carryOutcome is what a carry_over records.
type carryOutcome struct {
	Outcome types.Outcome
	Reason  vocab.Reason
	Detail  string
	Value   []types.Ref
}

// rebaseShape checks rb, a rebase of cand's patch: a clean rebase
// (empty/completed_clean) holds the prospective tree and the rebased patch;
// one that found conflicts (found) holds their resolution too. The tree is
// a git tree and the rebased patch is of the candidate patch's type, so no
// ref reads as another, and only the outcome says clean.
func rebaseShape(cand, rb types.Result) (clean bool, err error) {
	clean = rb.Outcome == types.OutcomeEmpty
	if len(cand.Value) != 1 || len(rb.Value) != map[bool]int{true: 2, false: 3}[clean] || !clean && rb.Outcome != types.OutcomeFound ||
		rb.Value[0].Type != types.RefTypeGitTree || rb.Value[1].Type != cand.Value[0].Type {
		return false, errors.New("graph: a rebase holds its git tree, its rebased patch of the candidate's type and, when it found conflicts, their resolution")
	}
	return clean, nil
}

// carryDecision is what a carry_over records, from what its claim fixed:
// an attestation holding the prospective tree only when the carried review
// took the candidate's patch, the rebase is clean and its rebased patch has
// that patch's identity; otherwise the demand for a delta review.
func carryDecision(cand, rb types.Result, reviewed *types.Result) (carryOutcome, error) {
	clean, err := rebaseShape(cand, rb)
	if err != nil {
		return carryOutcome{}, err
	}
	why := ""
	switch {
	case reviewed == nil || len(reviewed.Value) != 1 || reviewed.Value[0].ContentDigest != cand.Value[0].ContentDigest:
		why = "the carried review is not of the candidate's patch"
	case rb.Value[1].ContentDigest != cand.Value[0].ContentDigest && !mutation.Is(mutation.CarryOverIgnoresPatch):
		why = "the rebased patch is not the reviewed one"
	case !clean:
		why = "the rebase resolved conflicts"
	default:
		return carryOutcome{types.OutcomeEmpty, vocab.ReasonCompletedClean, "", []types.Ref{rb.Value[0]}}, nil
	}
	return carryOutcome{types.OutcomeFound, vocab.ReasonReviewInputChanged, why + ": a delta review is demanded", []types.Ref{}}, nil
}

// inputResult is the recorded result a claim fixed for input name.
func inputResult(tx *eventstore.Tx, c claim, name string) (types.ResultRecorded, error) {
	r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(resolutions(c.Inputs)[name].Result))
	if err == nil && !ok {
		err = fmt.Errorf("graph: input %s resolved to no recorded result", name)
	}
	return r, err
}

// carryInputs are what rec's claim fixed, from the log: the candidate's
// patch result, the rebase's result, and the result the carried review's
// claim took from the candidate's producer (nil: none).
func carryInputs(tx *eventstore.Tx, rec nodeRecord) (cand, rb types.Result, reviewed *types.Result, err error) {
	c, ok, err := load[claim](tx, ClaimProjection, string(rec.Recorded.Node.ID))
	if err != nil || !ok || rec.Recorded.Node.Kind != types.NodeKindCarryOver {
		return cand, rb, nil, errors.Join(err, fmt.Errorf("graph: %s is not a claimed carry_over node", rec.Recorded.Node.ID))
	}
	cr, err := inputResult(tx, c, "candidate")
	rr, rerr := inputResult(tx, c, "rebase")
	vr, verr := inputResult(tx, c, "review")
	review, nerr := loadNode(tx, vr.Node)
	rc, _, cerr := load[claim](tx, ClaimProjection, string(vr.Node))
	if err = errors.Join(err, rerr, verr, nerr, cerr); err != nil {
		return cand, rb, nil, err
	}
	for _, in := range review.Recorded.Node.Inputs {
		if src, ok := in.Source.Variant.(*types.ResultSource); ok && src.Producer == producerOf(rec.Recorded.Node, "candidate") {
			r, err := inputResult(tx, rc, in.Name)
			if err != nil {
				return cand, rb, nil, err
			}
			reviewed = &r.Result
		}
	}
	return cr.Result, rr.Result, reviewed, nil
}

// CarryOver commits the result of node, a claimed carry_over node, from
// its claimed inputs (re-validated, as by Commit), and returns it.
func CarryOver(tx *eventstore.Tx, node types.NodeID, env Env) (types.Result, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return types.Result{}, err
	}
	cand, rb, reviewed, err := carryInputs(tx, rec)
	if err != nil {
		return types.Result{}, err
	}
	d, err := carryDecision(cand, rb, reviewed)
	id, ierr := ids.New("res")
	if err = errors.Join(err, ierr); err != nil {
		return types.Result{}, err
	}
	r := types.Result{ID: types.ResultID(id), Computation: rec.State.Key.Key, Kind: types.ResultKindCarryOverAttestation,
		Value: d.Value, Outcome: d.Outcome, Reason: d.Reason, Detail: d.Detail, Completed: true}
	return r, commitResult(tx, node, r, env)
}

// effectValue is what an effect node's result holds: the input ref its
// operation's effect applied, once that effect, for the node's current
// request (its key among it), is completed. Effect states are monotone, so
// a replay reads the same.
func effectValue(tx *eventstore.Tx, rec nodeRecord) ([]types.Ref, error) {
	op, ok, err := load[types.NodeOperated](tx, EffectProjection, string(rec.Recorded.Node.ID))
	if err != nil || !ok {
		return nil, errors.Join(err, errors.New("an effect node records a result only of its operation"))
	}
	e, err := tx.LoadEffect(string(op.Effect))
	r, _, rerr := effectRequest(tx, rec.Recorded.Node.ID)
	b, merr := json.Marshal(r)
	d, derr := eventstore.ContentDigest(b)
	if err = errors.Join(err, rerr, merr, derr); err == nil && (e.State != eventstore.EffectCompleted || e.RequestDigest != d) {
		err = fmt.Errorf("the operation's effect %s is %s, for request %s, not completed for the node's request", e.ID, e.State, e.RequestDigest)
	}
	return []types.Ref{r.applied}, err
}

// sameRefs: a and b name the same refs, in order.
func sameRefs(a, b []types.Ref) bool {
	return slices.EqualFunc(a, b, func(x, y types.Ref) bool {
		return x.Type == y.Type && x.ID == y.ID && x.ContentDigest == y.ContentDigest && x.IdentityDigest == y.IdentityDigest
	})
}

// resultSemantics is the result folds' check of what a rebase, a
// carry_over or an effect node records, from the log alone: a rebase of
// its candidate's shape; the carry_over's decision; an effect node's
// empty/completed_clean result holding what its effect applied.
func resultSemantics(tx *eventstore.Tx, rec nodeRecord, r types.Result) error {
	c, _, err := load[claim](tx, ClaimProjection, string(rec.Recorded.Node.ID))
	switch {
	case err != nil:
		return err
	case rec.Recorded.Node.Kind == types.NodeKindRebase:
		cand, err := inputResult(tx, c, "candidate")
		if err == nil {
			_, err = rebaseShape(cand.Result, r)
		}
		return err
	case rec.Recorded.Node.Kind == types.NodeKindCarryOver:
		cand, rb, reviewed, err := carryInputs(tx, rec)
		d, derr := carryDecision(cand, rb, reviewed)
		if err = errors.Join(err, derr); err == nil && (r.Outcome != d.Outcome || r.Reason != d.Reason || !sameRefs(r.Value, d.Value)) {
			err = fmt.Errorf("the carry_over records %s/%s, not its inputs' %s/%s", r.Outcome, r.Reason.String(), d.Outcome, d.Reason.String())
		}
		return err
	case rec.Recorded.Node.Effect != nil:
		v, err := effectValue(tx, rec)
		if err == nil && (r.Outcome != types.OutcomeEmpty || r.Reason != vocab.ReasonCompletedClean || !sameRefs(r.Value, v)) {
			err = errors.New("an effect node's result is empty/completed_clean, holding what its effect applied")
		}
		return err
	}
	return nil
}

// CompleteEffect records and selects the result of node, an effect node
// whose operation's effect is completed: the one result kind its node kind
// produces, holding what the effect applied. tx is a command's (Emit
// refuses any other).
func CompleteEffect(tx *eventstore.Tx, node types.NodeID, blobs receipt.Blobs) error {
	rec, err := loadNode(tx, node)
	if err != nil || rec.Recorded.Node.Effect == nil || len(produces[rec.Recorded.Node.Kind]) != 1 {
		return errors.Join(err, fmt.Errorf("graph: %s records no effect result", node))
	}
	value, err := effectValue(tx, rec)
	id, ierr := ids.New("res")
	if err = errors.Join(err, ierr); err != nil {
		return err
	}
	r := types.Result{ID: types.ResultID(id), Computation: rec.State.Key.Key, Kind: produces[rec.Recorded.Node.Kind][0], Value: value,
		Outcome: types.OutcomeEmpty, Reason: vocab.ReasonCompletedClean, Completed: true}
	evs, rerr := recordResult(tx, types.ResultRecorded{Node: node, Result: r}, blobs)
	sel, serr := selectResult(tx, node, r.ID)
	if err = errors.Join(rerr, serr); err != nil {
		return err
	}
	return tx.Emit(append(evs, sel...)...)
}

// operationCompleted: rec's operation at its current key has a completed
// effect, for its selection.
func operationCompleted(tx *eventstore.Tx, rec nodeRecord) error {
	_, err := effectValue(tx, rec)
	return err
}
