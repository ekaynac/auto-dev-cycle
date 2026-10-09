// SPDX-License-Identifier: Apache-2.0

package graph

// Merge-queue generations (13 "Merge-queue generations"; plan 15 G7b-1).
//
// Decisions:
//   - A queue generation is the nodes that try to merge one candidate
//     result onto one base head of one branch: each attempt is one (G7b-1b).
//     Its rebase node pins {branch, base, version, candidate, generation};
//     its carry_over node takes the rebase's tree and the review subject's
//     selected requirement; its one merge node takes the carry_over's
//     attestation and that requirement (G7b-2a). G7b-2b adds the refreshed
//     receipts, the verdict and the merge's compare-and-set.
//   - Generations are numbered per task and branch, from 1. The node.recorded
//     fold admits a rebase node only as the next generation, for the
//     candidate's selected result, on an observation no older than the
//     current generation's (the forge's version rises with every move), and
//     for another base or another candidate result. Earlier generations
//     stay, as history.
//   - Generate is the template: it records the next generation when the
//     observation or the candidate's result calls for it, and refuses a
//     stale observation (ErrStaleObservation).
//   - A rebase result's conflict status is its outcome: clean, it holds the
//     git tree and the rebased patch; found, their resolution too. Its refs
//     are typed. A patch's identity is its one ref's content digest.
//   - carryDecision is the one function of a carry_over's claimed inputs:
//     an attestation holding the prospective tree only when the carried
//     review took the candidate's patch under the carry_over's requirement,
//     the rebase is clean and the rebased patch has the candidate's
//     identity; else the demand for a delta or replacement review
//     (found/review_input_changed). CarryOver applies it, and the result fold
//     refuses any other outcome, reason or value; Commit refuses carry_over.
//   - The generation's candidate result is pinned through execution: the
//     claim of its rebase, the rebase's result and its carry_over's inputs
//     take exactly params.candidate (pinnedCandidate). A candidate
//     reselected after the generation was recorded is a new generation's.
//     Its inputs hold their roles (roleInput): the candidate a patch, the
//     rebase the required rebase node's tree, the review a review node's
//     review (a reused review's holder's), the requirement a requirement's.
//   - The merge waits on a roster change: its requirement serves only at the
//     subject's selected revision and a carry_over only by its attestation
//     (serves, requirement.go), so Operate's re-validation refuses its
//     intent and an unclaimed merge waits.
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

// rebaseParams are a rebase node's params: the generation it starts, the
// base head it pins with the forge's version of that observation, and the
// candidate result it tries to merge.
type rebaseParams struct {
	Branch     string         `json:"branch"`
	Base       string         `json:"base"`
	Version    int64          `json:"version"`
	Candidate  types.ResultID `json:"candidate"`
	Generation int64          `json:"generation"`
}

// Observation is a branch's head as the forge reported it, with the
// forge's version of the branch: it rises with every move of the head.
type Observation struct {
	Head    string
	Version int64
}

// Generation is a queue generation: its number, the base head it pins (and
// the version observed with it), the candidate result it tries, and its
// nodes.
type Generation struct {
	Number    int64          `json:"number"`
	Base      string         `json:"base"`
	Version   int64          `json:"version"`
	Candidate types.ResultID `json:"candidate"`
	Rebase    types.NodeID   `json:"rebase"`
	CarryOver types.NodeID   `json:"carry_over"`
	Merge     types.NodeID   `json:"merge"`
}

// mergeParams are a merge node's params: the branch it merges onto (its
// effect's target), and its generation with the base head it pins.
type mergeParams struct {
	Target     string `json:"target"`
	Base       string `json:"base"`
	Generation int64  `json:"generation"`
}

// ErrStaleObservation: an observation older than the queue's latest, or
// another head at the latest's version. The queue does not move: a newer
// observation already did, or the two contradict each other.
type ErrStaleObservation struct{ Observed, Latest Observation }

func (e ErrStaleObservation) Error() string {
	return fmt.Sprintf("graph: %s observed at version %d is not newer than the queue's latest observation, %s at %d",
		e.Observed.Head, e.Observed.Version, e.Latest.Head, e.Latest.Version)
}

// Result is the refusal an operator sees: failed/stale_expected_state. A
// queue wedged by a forge whose versions went back (a restart, or a bogus
// version) refuses every observation this way until it is re-based (13).
func (e ErrStaleObservation) Result() vocab.Result {
	return vocab.Result{Outcome: vocab.Failed, Reason: vocab.ReasonStaleExpectedState, Detail: e.Error()}
}

// observe admits obs as task and branch's latest observation of its base
// head: newer than the latest, or the latest again. The fold of a
// queue.observed event and of a rebase node's record both run it.
func observe(tx *eventstore.Tx, task types.TaskID, branch string, obs Observation) error {
	key := generationKey(task, branch)
	last, ok, err := load[Observation](tx, ObservedProjection, key)
	if err != nil || ok && (obs.Version < last.Version || obs.Version == last.Version && obs.Head != last.Head) && !mutation.Is(mutation.GenerationTakesStale) {
		return errors.Join(err, ErrStaleObservation{obs, last})
	}
	return put(tx, ObservedProjection, key, obs)
}

// queueObserved folds a queue's observation of its base head.
func queueObserved(tx *eventstore.Tx, ev eventstore.Event, p *types.QueueObserved) error {
	project, ok, err := load[types.ProjectID](tx, TaskProjection, string(p.Task))
	if err != nil || !ok || ev.Project == nil || *ev.Project != string(project) || ev.Subject != string(p.Task) {
		return errors.Join(err, errors.New("an observation is under its task, in the task's project"))
	}
	return observe(tx, p.Task, p.Branch, Observation{Head: p.Head, Version: p.Version})
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
// its task and branch, observed no earlier than the current one, for
// another base or another candidate result (the selected one).
func generationRecorded(tx *eventstore.Tx, p *types.NodeRecorded) error {
	if p.Node.Kind == types.NodeKindCarryOver {
		return carryRecorded(tx, p)
	}
	if p.Node.Kind == types.NodeKindMerge {
		return mergeRecorded(tx, p)
	}
	if p.Node.Kind != types.NodeKindRebase {
		return nil
	}
	var rp rebaseParams
	if err := contract.UnmarshalStrict(p.Node.Params, &rp); err != nil || rp.Branch == "" || rp.Base == "" {
		return errors.Join(err, errors.New("a rebase node's params are its branch, base, version, candidate and generation"))
	}
	i := slices.IndexFunc(p.Node.Inputs, func(in types.InputSpec) bool { return in.Name == "candidate" })
	src, ok := (*types.ResultSource)(nil), i >= 0
	if ok {
		src, ok = p.Node.Inputs[i].Source.Variant.(*types.ResultSource)
	}
	if !ok {
		return errors.New("a rebase node takes its candidate")
	}
	if r, why, err := selectedOf(tx, nodeRecord{Recorded: *p}, src); err != nil || why != "" || r.Result.ID != rp.Candidate {
		return errors.Join(err, fmt.Errorf("the generation's candidate %s is not its producer's selected result %s", rp.Candidate, why))
	}
	if err := observe(tx, p.Task, rp.Branch, Observation{Head: rp.Base, Version: rp.Version}); err != nil {
		return err
	}
	key := generationKey(p.Task, rp.Branch)
	cur, ok, err := load[Generation](tx, GenerationProjection, key)
	if err != nil || ok && (rp.Generation != cur.Number+1 || rp.Base == cur.Base && rp.Candidate == cur.Candidate) || !ok && rp.Generation != 1 {
		return errors.Join(err, fmt.Errorf("generation %d (%s at %d) does not follow generation %d (%s at %d): only a newer base or candidate starts one",
			rp.Generation, rp.Base, rp.Version, cur.Number, cur.Base, cur.Version))
	}
	return put(tx, GenerationProjection, key, Generation{Number: rp.Generation, Base: rp.Base, Version: rp.Version, Candidate: rp.Candidate, Rebase: p.Node.ID})
}

// pinnedCandidate: rb, a rebase node, takes cand as its candidate, the
// candidate result its generation pins (params.candidate). The node.claimed
// fold checks it of a rebase's claim and the result fold again, so a
// candidate reselected after the generation was recorded is refused, not
// rebased under the generation that names another result.
func pinnedCandidate(rb nodeRecord, cand types.ResultID) error {
	var rp rebaseParams
	if err := json.Unmarshal(rb.Recorded.Node.Params, &rp); err != nil || cand != rp.Candidate {
		return errors.Join(err, fmt.Errorf("graph: generation %d's rebase %s takes candidate %s, not %s, the candidate it pins",
			rp.Generation, rb.Recorded.Node.ID, cand, rp.Candidate))
	}
	return nil
}

// rebaseClaimed: a rebase's claim resolves its candidate to the result its
// generation pins; other kinds pass.
func rebaseClaimed(rec nodeRecord, resolved map[string]Resolved) error {
	if rec.Recorded.Node.Kind != types.NodeKindRebase {
		return nil
	}
	return pinnedCandidate(rec, resolved["candidate"].Result)
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

// mergeInputs: n takes exactly a carry_over's attestation and a review
// requirement, by those names.
func mergeInputs(n types.Node) bool {
	want := map[types.Name]types.ResultKind{"carry_over": types.ResultKindCarryOverAttestation, "requirement": types.ResultKindReviewRequirement}
	seen := map[types.Name]bool{}
	for _, in := range n.Inputs {
		src, ok := in.Source.Variant.(*types.ResultSource)
		if !ok || want[in.Name] != src.ResultKind || seen[in.Name] {
			return false
		}
		seen[in.Name] = true
	}
	return len(seen) == len(want)
}

// mergeRecorded admits one merge node per generation, only on its current
// generation's carry_over, at that generation's number and base, taking
// the requirement its carry_over takes, and keeps it in that generation.
func mergeRecorded(tx *eventstore.Tx, p *types.NodeRecorded) error {
	var mp mergeParams
	if err := contract.UnmarshalStrict(p.Node.Params, &mp); err != nil || !mergeInputs(p.Node) {
		return errors.Join(err, errors.New("a merge node's params are its target, base and generation; it takes a carry_over and a review requirement"))
	}
	key := generationKey(p.Task, mp.Target)
	cur, ok, err := load[Generation](tx, GenerationProjection, key)
	co, cerr := loadNode(tx, cur.CarryOver)
	if err = errors.Join(err, cerr); err != nil || !ok || cur.Merge != "" || producerOf(p.Node, "carry_over") != cur.CarryOver || mp.Base != cur.Base ||
		mp.Generation != cur.Number || producerOf(p.Node, "requirement") != producerOf(co.Recorded.Node, "requirement") {
		return errors.Join(err, errors.New("a generation's one merge takes its current carry_over and that carry_over's requirement, at its base"))
	}
	cur.Merge = p.Node.ID
	return put(tx, GenerationProjection, key, cur)
}

// Generate is the events recording spec's next generation on obs, the
// base head observed on the forge, and that generation: each attempt to
// merge a candidate result onto a base head is one. It records none when
// the current generation is of that head and the candidate's selected
// result, and refuses an observation older than the current generation's.
func Generate(tx *eventstore.Tx, spec QueueSpec, obs Observation) ([]eventstore.Event, Generation, error) {
	if obs.Head == "" {
		return nil, Generation{}, errors.New("graph: no base head was observed")
	}
	cand, err := loadNode(tx, spec.Candidate)
	if err != nil || cand.State.Result == nil {
		return nil, Generation{}, errors.Join(err, fmt.Errorf("graph: the candidate %s has no selected result", spec.Candidate))
	}
	review, err := loadNode(tx, spec.Review)
	_, subject, serr := rosterOf(review.Recorded.Node)
	required, rerr := selectedRequirement(tx, spec.Task, subject)
	if err = errors.Join(err, serr, rerr); err != nil {
		return nil, Generation{}, err
	}
	key := generationKey(spec.Task, spec.Branch)
	last, seen, err := load[Observation](tx, ObservedProjection, key)
	if err == nil && seen && (obs.Version < last.Version || obs.Version == last.Version && obs.Head != last.Head) && !mutation.Is(mutation.GenerationTakesStale) {
		err = ErrStaleObservation{obs, last}
	}
	var evs []eventstore.Event
	if err == nil && (!seen || obs.Version > last.Version) { // the latest observation, kept even when no generation starts
		evs, err = event(QueueObserved, string(spec.Task), spec.Project, &types.QueueObserved{Type: "queue_observed", Task: spec.Task, Branch: spec.Branch, Head: obs.Head, Version: obs.Version})
	}
	cur, ok, cerr := load[Generation](tx, GenerationProjection, key)
	if err = errors.Join(err, cerr); err != nil || ok && (cur.Base == obs.Head && cur.Candidate == *cand.State.Result || mutation.Is(mutation.GenerationKeepsBase)) {
		return evs, Generation{}, err
	}
	g := Generation{Number: cur.Number + 1, Base: obs.Head, Version: obs.Version, Candidate: *cand.State.Result}
	params, err := json.Marshal(rebaseParams{Branch: spec.Branch, Base: obs.Head, Version: obs.Version, Candidate: g.Candidate, Generation: g.Number})
	mparams, merr := json.Marshal(mergeParams{Target: spec.Branch, Base: obs.Head, Generation: g.Number})
	rb, rerr := ids.New("nod")
	co, cerr := ids.New("nod")
	mg, gerr := ids.New("nod")
	if err = errors.Join(err, merr, rerr, cerr, gerr); err != nil {
		return nil, Generation{}, err
	}
	g.Rebase, g.CarryOver, g.Merge = types.NodeID(rb), types.NodeID(co), types.NodeID(mg)
	input := func(name string, producer types.NodeID, kind types.ResultKind) types.InputSpec {
		return types.InputSpec{Name: name, Source: types.InputSpecSource{Variant: &types.ResultSource{Type: "result", Producer: producer, ResultKind: kind}}}
	}
	node := func(id types.NodeID, kind types.NodeKind, params json.RawMessage, inputs ...types.InputSpec) types.NodeRecorded {
		return types.NodeRecorded{Task: spec.Task, Project: spec.Project, Node: types.Node{ID: id, Kind: kind, DefinitionVersion: "1", Round: 1,
			Params: params, Inputs: inputs, Requires: types.NodeRequires{Resources: []types.ResourceGroupKind{}}}}
	}
	candidate := input("candidate", spec.Candidate, types.ResultKindPatch)
	requirement := input("requirement", required.Node, types.ResultKindReviewRequirement)
	merge := node(g.Merge, types.NodeKindMerge, mparams, input("carry_over", g.CarryOver, types.ResultKindCarryOverAttestation), requirement)
	effect := types.Name("merge")
	merge.Node.Effect = &effect
	for _, n := range []types.NodeRecorded{
		node(g.Rebase, types.NodeKindRebase, params, candidate),
		node(g.CarryOver, types.NodeKindCarryOver, json.RawMessage(`{}`), input("review", spec.Review, types.ResultKindReview), candidate,
			input("rebase", g.Rebase, types.ResultKindTree), requirement),
		merge,
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

// carried is what a carry_over's claim fixed: the candidate's patch
// result, the rebase's result, the result the carried review's claim took
// from the candidate's producer (nil: none), and whether that claim took
// the carry_over's requirement.
type carried struct {
	cand, rb types.Result
	reviewed *types.Result
	required bool
}

// carryDecision is what a carry_over records, from what its claim fixed:
// an attestation holding the prospective tree only when the carried review
// took the candidate's patch under the carry_over's requirement (the
// subject's selected revision), the rebase is clean and its rebased patch
// has that patch's identity; otherwise the demand for a delta review, or
// for the replacement review after a roster change.
func carryDecision(c carried) (carryOutcome, error) {
	cand, rb, reviewed := c.cand, c.rb, c.reviewed
	clean, err := rebaseShape(cand, rb)
	if err != nil {
		return carryOutcome{}, err
	}
	why, demand := "", "a delta review"
	switch {
	case reviewed == nil || len(reviewed.Value) != 1 || reviewed.Value[0].ContentDigest != cand.Value[0].ContentDigest:
		why = "the carried review is not of the candidate's patch"
	case !c.required:
		why, demand = "the carried review was not made under the selected requirement revision", "the replacement review"
	case rb.Value[1].ContentDigest != cand.Value[0].ContentDigest && !mutation.Is(mutation.CarryOverIgnoresPatch):
		why = "the rebased patch is not the reviewed one"
	case !clean:
		why = "the rebase resolved conflicts"
	default:
		return carryOutcome{types.OutcomeEmpty, vocab.ReasonCompletedClean, "", []types.Ref{rb.Value[0]}}, nil
	}
	return carryOutcome{types.OutcomeFound, vocab.ReasonReviewInputChanged, why + ": " + demand + " is demanded", []types.Ref{}}, nil
}

// inputResult is the recorded result a claim fixed for input name.
func inputResult(tx *eventstore.Tx, c claim, name string) (types.ResultRecorded, error) {
	r, ok, err := load[types.ResultRecorded](tx, ResultProjection, string(resolutions(c.Inputs)[name].Result))
	if err == nil && !ok {
		err = fmt.Errorf("graph: input %s resolved to no recorded result", name)
	}
	return r, err
}

// roleInput is the result c fixed for rec's input name, in its role: a
// result of kind rk from rec's declared producer of name, a node of kind nk
// ("": any). A carry_over's inputs are declared by whoever records it, and
// selection checks only the kind declared, so the fold holds each input to
// its role: the candidate a patch, the rebase the required rebase node's
// tree, the review a review node's review, the requirement a
// review_requirement node's. A declared producer bound by reuse gives its
// holder's result (reused): the holder recorded it, and its claim is the
// one read.
func roleInput(tx *eventstore.Tx, c claim, rec nodeRecord, name string, rk types.ResultKind, nk types.NodeKind) (types.ResultRecorded, error) {
	r, err := inputResult(tx, c, name)
	if err != nil {
		return r, err
	}
	producer, err := loadNode(tx, r.Node)
	declared := producerOf(rec.Recorded.Node, name)
	if err == nil && r.Node != declared {
		err = reusedFrom(tx, declared, r)
	}
	if err == nil && (r.Result.Kind != rk || nk != "" && producer.Recorded.Node.Kind != nk) {
		err = fmt.Errorf("graph: a carry_over's %s is a %s result of its %s producer, not a %s result of a %s node", name, rk, map[bool]types.NodeKind{true: nk, false: "declared"}[nk != ""], r.Result.Kind, producer.Recorded.Node.Kind)
	}
	return r, err
}

// reusedFrom: declared, a producer bound by reuse (G4a), gives r, its
// holder's selected result: it is completed on r, of the key r answers,
// among that key's reusers in its task.
func reusedFrom(tx *eventstore.Tx, declared types.NodeID, r types.ResultRecorded) error {
	d, err := loadNode(tx, declared)
	if err != nil || d.State.Result == nil || *d.State.Result != r.Result.ID || d.State.Key == nil || d.State.Key.Key != r.Result.Computation {
		return errors.Join(err, fmt.Errorf("graph: %s does not give %s's result %s", declared, r.Node, r.Result.ID))
	}
	reusers, _, err := load[[]types.NodeID](tx, ReuseProjection, keyIndex(d.Recorded.Task, d.State.Key.Key))
	if err == nil && !slices.Contains(reusers, declared) {
		err = fmt.Errorf("graph: %s is not bound by reuse to %s's key", declared, r.Node)
	}
	return err
}

// carryInputs are what rec's claim fixed, from the log (carried). The
// carried review's claim is its result holder's: the review itself, or the
// node whose key a reused review is bound to.
func carryInputs(tx *eventstore.Tx, rec nodeRecord) (out carried, err error) {
	c, ok, err := load[claim](tx, ClaimProjection, string(rec.Recorded.Node.ID))
	if err != nil || !ok || rec.Recorded.Node.Kind != types.NodeKindCarryOver {
		return out, errors.Join(err, fmt.Errorf("graph: %s is not a claimed carry_over node", rec.Recorded.Node.ID))
	}
	cr, err := roleInput(tx, c, rec, "candidate", types.ResultKindPatch, "")
	rr, rerr := roleInput(tx, c, rec, "rebase", types.ResultKindTree, types.NodeKindRebase)
	vr, verr := roleInput(tx, c, rec, "review", types.ResultKindReview, types.NodeKindReview)
	qr, qerr := roleInput(tx, c, rec, "requirement", types.ResultKindReviewRequirement, types.NodeKindReviewRequirement)
	review, nerr := loadNode(tx, vr.Node)
	rc, _, cerr := load[claim](tx, ClaimProjection, string(vr.Node))
	if err = errors.Join(err, rerr, verr, qerr, nerr, cerr); err != nil {
		return out, err
	}
	rbn, err := loadNode(tx, rr.Node)
	if err = errors.Join(err, pinnedCandidate(rbn, cr.Result.ID)); err != nil {
		return out, err
	}
	out.cand, out.rb = cr.Result, rr.Result
	for _, in := range review.Recorded.Node.Inputs {
		src, ok := in.Source.Variant.(*types.ResultSource)
		if !ok || src.Producer != producerOf(rec.Recorded.Node, "candidate") && src.ResultKind != types.ResultKindReviewRequirement {
			continue
		}
		r, err := inputResult(tx, rc, in.Name)
		if err != nil {
			return out, err
		}
		if src.ResultKind == types.ResultKindReviewRequirement {
			out.required = r.Result.ID == qr.Result.ID
		} else {
			out.reviewed = &r.Result
		}
	}
	return out, nil
}

// CarryOver commits the result of node, a claimed carry_over node, from
// its claimed inputs (re-validated, as by Commit), and returns it.
func CarryOver(tx *eventstore.Tx, node types.NodeID, env Env) (types.Result, error) {
	rec, err := loadNode(tx, node)
	if err != nil {
		return types.Result{}, err
	}
	in, err := carryInputs(tx, rec)
	if err != nil {
		return types.Result{}, err
	}
	d, err := carryDecision(in)
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
			err = pinnedCandidate(rec, cand.Result.ID)
		}
		if err == nil {
			_, err = rebaseShape(cand.Result, r)
		}
		return err
	case rec.Recorded.Node.Kind == types.NodeKindCarryOver:
		in, err := carryInputs(tx, rec)
		d, derr := carryDecision(in)
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
