// SPDX-License-Identifier: Apache-2.0

package graph

// Graph events and their projections (13 "Evaluation", plan 15 G3-1).
//
// Decisions:
//   - A claim is a node instance's (13 [G10]): a repeat demand of a claimed
//     node joins its claim and appends nothing. Two instances never share
//     a claim: build, plan and effect nodes are not reused (13 "v1 scope",
//     [G4]), and reuse of a memoizable kind's result across instances is
//     the resolver's memoization (G4a), not a claim.
//   - For the memoizable kinds (gate, runtime_verify, evidence, review,
//     carry_over) a key is claimed once per task: a second instance whose
//     key is claimed in its task gets ErrKeyClaimed, nothing is appended,
//     and it stays as it was for the resolver to reuse or wait. Every check
//     is scoped to the task, which also scopes it to the project.
//   - The fold re-derives each claim: the claimed inputs are exactly the
//     items Claim builds for the declared inputs, and the key is Key's. So
//     a forged or replayed claim binds no other computation or resolution.
//     Two claims of one key in one command fail, as the second's fold sees
//     the first.
//   - A task's nodes are all in one project (graph_tasks): the first node
//     of a task pins its project, until tasks are records (M1) and the
//     fold checks the task's own project.
//   - A node has one successor (succeeds); a replacement, which keeps its
//     predecessor's round (13 supersession), is its own event
//     (review.superseded, supersede.go) and takes the successor slot.
//   - A missing answer input leaves the node waiting; the evaluator raises
//     awaiting_input against its owed input (evaluate.go).
//   - The supersession fold reads the lease projections (the attempts it
//     lists and who stopped them), so a store composes the lease projector
//     before the graph's in eventstore.ProjectEach.
//   - The fold reads only the events and these projections, so a rebuild
//     reaches the same state. A claim keeps its command (ev.Command), as
//     grant lineages do.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// The graph event kinds and projections.
const (
	NodeRecorded    = "node.recorded/v1"
	NodeClaimed     = "node.claimed/v1"
	NodeInvalidated = "node.invalidated/v1"
	NodeReused      = "node.reused/v1"
	ReuseReleased   = "node.reuse_released/v1"
	NodeRenewed     = "node.renewed/v1"
	HoldRaised      = "node.hold_raised/v1"
	HoldPaged       = "node.hold_paged/v1"
	HoldCleared     = "node.hold_cleared/v1"
	ResultRecorded  = "result.recorded/v1"
	ResultSelected  = "result.selected/v1"
	NodeOperated    = "node.operated/v1"
	QueueObserved   = "queue.observed/v1"
	EventSchema     = "records/graph-event"

	NodeProjection      = "graph_nodes"      // node id → nodeRecord
	ClaimProjection     = "graph_claims"     // node id → claim
	KeyProjection       = "graph_keys"       // task/computation key → the memoizable node that claimed it
	ReuseProjection     = "graph_reuses"     // task/computation key → the nodes bound to it by reuse
	RenewalProjection   = "graph_renewals"   // node id → its selection boundary (renewal)
	SuccessorProjection = "graph_successors" // node id → its one successor
	TaskProjection      = "graph_tasks"      // task id → its project
	TaskNodeProjection  = "graph_task_nodes" // "<task>/<node>": each node of a task (Astra #316 M3)
	ResultProjection    = "graph_results"    // result id → the result.recorded payload
	GenProjection       = "graph_gens"       // task/identity → its current generation (memoizable kinds)
	HoldProjection      = "graph_holds"      // hold id → the node that last held it (heldBy)
	RetryProjection     = "graph_retries"    // could_not_run result id → its retry wait's hold id
	EffectProjection    = "graph_effects"    // effect node id → its operation (node.operated)
	ObservedProjection  = "graph_observed"   // task/branch → the latest observation of its base head
)

// memoizable are the kinds whose results are reused (13 "v1 scope").
var memoizable = []types.NodeKind{types.NodeKindGate, types.NodeKindRuntimeVerify, types.NodeKindEvidence, types.NodeKindReview, types.NodeKindCarryOver}

// ErrKeyClaimed: another node of the task holds the key's claim.
type ErrKeyClaimed struct{ Node types.NodeID }

func (e ErrKeyClaimed) Error() string { return "graph: the key is claimed by " + string(e.Node) }

// ErrKeyStale: a memoizable claim at a generation other than its identity's
// current one in the task.
type ErrKeyStale struct{ Current int64 }

func (e ErrKeyStale) Error() string {
	return fmt.Sprintf("graph: the key is not of the task's current generation %d", e.Current)
}

// ErrReused: the node is bound by reuse to its holder's key; the key is
// invalidated through its holder.
type ErrReused struct{ Holder types.NodeID }

func (e ErrReused) Error() string {
	if e.Holder == "" {
		return "graph: the node reuses a key whose holder was invalidated"
	}
	return "graph: the node reuses the key of " + string(e.Holder)
}

// Kinds registers the graph event kinds.
var kinds = []string{NodeRecorded, NodeClaimed, NodeReused, ReuseReleased, NodeRenewed, NodeInvalidated, HoldRaised, HoldPaged, HoldCleared, ResultRecorded, ResultSelected, NodeOperated, QueueObserved, ReviewSuperseded}

func Kinds() eventstore.Kinds {
	k := eventstore.Kinds{}
	for _, n := range kinds {
		k[n] = eventstore.Kind{Schema: EventSchema}
	}
	return k
}

// nodeRecord is a node as recorded, with its state. A replacement records
// the review it replaces, which its lineage includes.
type nodeRecord struct {
	Recorded types.NodeRecorded `json:"recorded"`
	Replaces *types.NodeID      `json:"replaces,omitempty"`
	State    types.NodeState    `json:"state"`
}

// claim is a node's claim: its key, its inputs as they resolved (for the
// commit-time re-validation, 13 [G4]) and the command that made it.
type claim struct {
	Key     types.ComputationKey `json:"key"`
	Inputs  []types.ClaimedInput `json:"inputs"`
	Command *string              `json:"command"`
}

func keyIndex(task types.TaskID, key types.Digest) string { return string(task) + "/" + string(key) }

func load[T any](tx *eventstore.Tx, projection, key string) (T, bool, error) {
	var v T
	raw, ok, err := tx.Get(projection, key)
	if err != nil || !ok {
		return v, false, err
	}
	return v, true, json.Unmarshal(raw, &v)
}

func put(tx *eventstore.Tx, projection, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(projection, key, b)
}

// putNode stores a node with its state, which must be a valid NodeState.
func putNode(tx *eventstore.Tx, rec nodeRecord) error {
	if _, err := contract.Encode("records/node-state", rec.State); err != nil {
		return err
	}
	return errors.Join(put(tx, TaskNodeProjection, string(rec.Recorded.Task)+"/"+string(rec.Recorded.Node.ID), true),
		put(tx, NodeProjection, string(rec.Recorded.Node.ID), rec))
}

func event(kind, subject string, project types.ProjectID, payload types.GraphEventVariant) ([]eventstore.Event, error) {
	b, err := json.Marshal(types.GraphEvent{Variant: payload})
	id, ierr := ids.New("evt")
	p := string(project)
	return []eventstore.Event{{ID: id, Kind: kind, Subject: subject, Project: &p, Payload: b}}, errors.Join(err, ierr)
}

func loadNode(tx *eventstore.Tx, id types.NodeID) (nodeRecord, error) {
	rec, ok, err := load[nodeRecord](tx, NodeProjection, string(id))
	if err == nil && !ok {
		err = fmt.Errorf("graph: no node %s", id)
	}
	return rec, err
}

// Record is the event that adds r's node to its task's graph. Its caller
// (M1's graph commands) takes r's project from the actor's authorization
// before it calls Record; the event's project is never the actor's claim.
//
// A review or review_requirement node is recorded through RecordReview,
// with its roster's families from the executor registry.
func Record(r types.NodeRecorded) ([]eventstore.Event, error) {
	if rostered(r.Node.Kind) {
		return nil, errors.Join(fmt.Errorf("graph: a %s node is recorded with the registry's families (RecordReview)", r.Node.Kind))
	}
	return record(r)
}

func record(r types.NodeRecorded) ([]eventstore.Event, error) {
	r.Type = "node_recorded"
	if err := errors.Join(checkEvidence(r.Node), checkReview(r.Node)); err != nil {
		return nil, err
	}
	return event(NodeRecorded, string(r.Node.ID), r.Project, &r)
}

// checkEvidence holds an evidence node to what its params schema cannot
// say (12): the node leases its evidence target, its per-demand cap is
// within its per-subject cap, and its subject is a declared result input.
// The store validates the params themselves when the event is appended.
func checkEvidence(n types.Node) error {
	if n.Kind != types.NodeKindEvidence {
		return nil
	}
	p, err := n.ParamsAsEvidenceNodeParams()
	if err != nil {
		return fmt.Errorf("graph: evidence params: %w", err)
	}
	subject := func(in types.InputSpec) bool {
		_, result := in.Source.Variant.(*types.ResultSource)
		return result && in.Name == p.SubjectInput
	}
	switch {
	case !slices.Contains(n.Requires.Resources, types.ResourceGroupKindEvidenceTarget):
		return errors.New("graph: an evidence node must lease an evidence_target")
	case p.Limits.MaxAttemptsPerDemand > p.Limits.MaxAttemptsPerSubject:
		return fmt.Errorf("graph: evidence cap per demand %d exceeds the cap per subject %d", p.Limits.MaxAttemptsPerDemand, p.Limits.MaxAttemptsPerSubject)
	case !slices.ContainsFunc(n.Inputs, subject):
		return fmt.Errorf("graph: evidence subject_input %s is not a declared result input", p.SubjectInput)
	}
	return nil
}

// items are the claim's inputs: one per declared input, in declared order,
// each a result or an approval with its revision, as its source says.
func items(n types.Node, resolved map[string]Resolved) []types.ClaimedInput {
	out := []types.ClaimedInput{}
	for _, in := range n.Inputs {
		r, item := resolved[in.Name], types.ClaimedInput{Name: in.Name}
		if _, answer := in.Source.Variant.(*types.AnswerSource); answer {
			item.Approval, item.Revision = &r.Approval, &r.Revision
		} else {
			item.Result = &r.Result
		}
		out = append(out, item)
	}
	return out
}

// Claim claims node's computation key at generation from its resolved
// inputs; generation 0 is the current one: for a memoizable kind its
// identity's in the task, 1 for the other kinds' first claim. A claimed node's repeat demand joins its claim: no event. A
// memoizable node whose key another node of its task claimed gets
// ErrKeyClaimed.
//
// Claim sees only what earlier commands committed, so one command claims a
// given node at most once and a given memoizable key at most once; a second
// claim in the same command fails the command in the fold. The evaluator
// (G4b) deduplicates its demands before it claims. As for Record, M1's
// graph commands take the project from the actor's authorization before
// they call Claim.
func Claim(tx *eventstore.Tx, node types.NodeID, resolved map[string]Resolved, generation int64) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	if err != nil || rec.Recorded.Node.Kind == types.NodeKindEvidence || rostered(rec.Recorded.Node.Kind) {
		return nil, errors.Join(err, errors.New("graph: an evidence node is claimed under its contract (ClaimEvidence), a review under the registry (ClaimReview)"))
	}
	return claimNode(tx, rec, resolved, generation)
}

func claimNode(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved, generation int64) ([]eventstore.Event, error) {
	node := rec.Recorded.Node.ID
	if _, claimed, err := load[claim](tx, ClaimProjection, string(node)); err != nil || claimed && !mutation.Is(mutation.ClaimPerDemand) {
		return nil, err
	}
	cur, err := current(tx, rec, resolved)
	if err != nil {
		return nil, err
	}
	if generation == 0 {
		generation = cur
	}
	if generation != cur {
		return nil, ErrKeyStale{cur}
	}
	key, err := Key(rec.Recorded.Node, resolved, generation)
	if err != nil {
		return nil, err
	}
	if slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		holder, taken, err := load[types.NodeID](tx, KeyProjection, keyIndex(rec.Recorded.Task, key.Key))
		if err != nil {
			return nil, err
		}
		if taken {
			// The refused instance never claims and never runs: it completes
			// through a reuse binding (G4a, node.reused/v1), which sets its
			// key and result from the holder's selected result, of the same
			// task, kind and key.
			return nil, ErrKeyClaimed{holder}
		}
	}
	c := types.NodeClaimed{Type: "node_claimed", Node: node, Key: key, Inputs: items(rec.Recorded.Node, resolved)}
	return event(NodeClaimed, string(node), rec.Recorded.Project, &c)
}

// ProjectM0 is M0's eventstore.Hooks.Project fold for the graph
// projections; other kinds pass through. It reads no authority, so it
// checks no recorded answer: only the M0 fixtures, whose answers are the
// owed-answer fake's, and tests use it. adcd folds with Projector
// (eligible.go).
func ProjectM0(tx *eventstore.Tx, evs []eventstore.Event) error {
	return project(tx, evs, nil, nil, nil)
}

// project folds evs, checking each event's answers first under auth (nil:
// none, M0's fold), then the task a claim or reuse takes under tasks (nil:
// none).
func project(tx *eventstore.Tx, evs []eventstore.Event, auth Authority, allowed []types.NodeKind, tasks Tasks) error {
	for _, ev := range evs {
		if !slices.Contains(kinds, ev.Kind) {
			continue
		}
		var g types.GraphEvent
		err := json.Unmarshal(ev.Payload, &g)
		if r, ok := g.Variant.(*types.NodeRecorded); err == nil && ok && allowed != nil && !slices.Contains(allowed, r.Node.Kind) {
			err = fmt.Errorf("deferred surface: a %s node is not among this build's kinds", r.Node.Kind)
		}
		if err == nil && auth != nil {
			err = eligible(tx, ev, g.Variant, auth)
		}
		if err == nil {
			err = taken(tx, ev, g.Variant, tasks)
		}
		if err == nil {
			err = fold(tx, ev, g.Variant, auth != nil)
		}
		if err != nil {
			return fmt.Errorf("graph: event %s (%s): %w", ev.ID, ev.Kind, err)
		}
	}
	return nil
}

// fold folds v; removals: a supersession may remove an answer, whose
// question eligible has checked moved.
func fold(tx *eventstore.Tx, ev eventstore.Event, v types.GraphEventVariant, removals bool) error {
	switch p := v.(type) {
	case *types.NodeRecorded:
		return recorded(tx, ev, p)
	case *types.NodeClaimed:
		return claimed(tx, ev, p)
	case *types.NodeReused:
		return reused(tx, ev, p)
	case *types.NodeReuseReleased:
		return reuseReleased(tx, ev, p)
	case *types.NodeRenewed:
		return renewed(tx, ev, p)
	case *types.NodeInvalidated:
		return invalidated(tx, ev, p)
	case *types.HoldRaised:
		return holdRaised(tx, ev, p)
	case *types.HoldPaged:
		return holdPaged(tx, ev, p)
	case *types.HoldCleared:
		return holdCleared(tx, ev, p)
	case *types.ResultRecorded:
		return resultRecorded(tx, ev, p)
	case *types.ResultSelected:
		return selected(tx, ev, p)
	case *types.NodeOperated:
		return operated(tx, ev, p)
	case *types.QueueObserved:
		return queueObserved(tx, ev, p)
	case *types.ReviewSuperseded:
		return supersededFold(tx, ev, p, removals)
	}
	return errors.New("no payload")
}

// nodeOf loads the node an event names, which must be in the event's project.
func nodeOf(tx *eventstore.Tx, ev eventstore.Event, id types.NodeID) (nodeRecord, error) {
	rec, err := loadNode(tx, id)
	if err == nil && (ev.Project == nil || *ev.Project != string(rec.Recorded.Project)) {
		err = fmt.Errorf("node %s is not recorded in the event's project", id)
	}
	return rec, err
}

func recorded(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeRecorded) error {
	n := p.Node
	if ev.Kind != NodeRecorded || ev.Subject != string(n.ID) || ev.Project == nil || *ev.Project != string(p.Project) {
		return errors.New("a node is recorded under its id, in its project")
	}
	if _, exists, err := load[nodeRecord](tx, NodeProjection, string(n.ID)); err != nil || exists {
		return errors.Join(err, errors.New("the node is recorded"))
	}
	if project, ok, err := load[types.ProjectID](tx, TaskProjection, string(p.Task)); err != nil || ok && project != p.Project {
		return errors.Join(err, errors.New("the task's nodes are in another project"))
	} else if err := put(tx, TaskProjection, string(p.Task), p.Project); err != nil {
		return err
	}
	if err := errors.Join(takesNoDeploy(tx, n), generationRecorded(tx, p)); err != nil {
		return err
	}
	round := int64(1)
	if p.Predecessor == nil && slices.ContainsFunc(n.Inputs, func(in types.InputSpec) bool { _, a := in.Source.Variant.(*types.AnswerSource); return a }) {
		return errors.New("a first round answers nothing: an answer is a successor's input (13)")
	}
	if p.Predecessor != nil {
		pred, err := nodeOf(tx, ev, *p.Predecessor)
		if err != nil || pred.Recorded.Task != p.Task || pred.Recorded.Node.Kind != n.Kind {
			return errors.Join(err, errors.New("the predecessor is not a node of the task of the same kind"))
		}
		if len(pred.State.Holds) > 0 {
			return fmt.Errorf("the predecessor holds %s: a hold moves to a successor only with G6b", pred.State.Holds[0].ID)
		}
		if err := succeeds(tx, pred.Recorded.Node, n); err != nil {
			return err
		}
		round = pred.Recorded.Node.Round + 1
	}
	if err := errors.Join(checkEvidence(n), checkReview(n)); err != nil {
		return err
	}
	if n.Round != round {
		return fmt.Errorf("round %d, want %d: a first round is 1, a successor its predecessor's plus one", n.Round, round)
	}
	st := types.NodeState{Node: n.ID, WaitingOn: waitingOn(n), Holds: []types.NodeHold{}}
	settle(&st)
	return putNode(tx, nodeRecord{Recorded: *p, State: st})
}

// succeeds: n may be p's one successor (13 "Successor rounds"): the same
// definition version, params, requirements and effect, p's inputs first
// (an inherited answer may move to a newer revision), and no other
// successor yet.
func succeeds(tx *eventstore.Tx, p, n types.Node) error {
	pp, err1 := ParamsDigest(p.Params)
	np, err2 := ParamsDigest(n.Params)
	if err := errors.Join(err1, err2); err != nil || p.DefinitionVersion != n.DefinitionVersion || pp != np ||
		!reflect.DeepEqual(p.Requires, n.Requires) || !reflect.DeepEqual(p.Effect, n.Effect) || len(n.Inputs) < len(p.Inputs) {
		return errors.Join(err, errors.New("a successor keeps its predecessor's definition, params, requirements, effect and inputs"))
	}
	for i, in := range p.Inputs {
		if !reflect.DeepEqual(in, n.Inputs[i]) && !newerAnswer(in, n.Inputs[i]) {
			return fmt.Errorf("a successor's input %d is not its predecessor's", i)
		}
	}
	if _, forked, err := load[types.NodeID](tx, SuccessorProjection, string(p.ID)); err != nil || forked {
		return errors.Join(err, errors.New("the predecessor has a successor"))
	}
	return put(tx, SuccessorProjection, string(p.ID), n.ID)
}

// waitingOn is what a node waits for before its inputs resolve: all of them.
func waitingOn(n types.Node) []types.Name {
	out := []types.Name{}
	for _, in := range n.Inputs {
		out = append(out, in.Name)
	}
	return out
}

// resolutions reads a claim's inputs back.
func resolutions(inputs []types.ClaimedInput) map[string]Resolved {
	out := map[string]Resolved{}
	for _, in := range inputs {
		r := Resolved{}
		if in.Result != nil {
			r.Result = *in.Result
		}
		if in.Approval != nil && in.Revision != nil {
			r.Approval, r.Revision = *in.Approval, *in.Revision
		}
		out[in.Name] = r
	}
	return out
}

// selectedInputs: every result input is the selected result of its
// declared producer (selectedOf).
func selectedInputs(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved) error {
	for _, in := range rec.Recorded.Node.Inputs {
		src, ok := in.Source.Variant.(*types.ResultSource)
		if !ok {
			continue
		}
		r, why, err := selectedOf(tx, rec, src)
		if err != nil || why != "" || r.Result.ID != resolved[in.Name].Result {
			return errors.Join(err, fmt.Errorf("input %s is not the selected %s result of its producer %s in the project", in.Name, src.ResultKind, src.Producer))
		}
	}
	return nil
}

// selectedOf is the result src's producer gives rec, or why it gives none:
// it is a node of rec's project, completed at its identity's current
// generation (a memoizable producer whose key an invalidation made stale
// gives nothing, even before its reuse is released), its selected result
// is of the declared kind, and it serves rec (serves: a requirement at its
// subject's selected revision). A producer bound by reuse (G4a) gives its
// holder's result.
func selectedOf(tx *eventstore.Tx, rec nodeRecord, src *types.ResultSource) (types.ResultRecorded, string, error) {
	var r types.ResultRecorded
	producer, err := loadNode(tx, src.Producer)
	if err != nil {
		return r, "", err
	}
	if producer.Recorded.Project != rec.Recorded.Project {
		return r, fmt.Sprintf("%s is not a node of the project", src.Producer), nil
	}
	if producer.State.State != types.NodeStateStateCompleted {
		return r, fmt.Sprintf("%s is %s", src.Producer, producer.State.State), nil
	}
	if slices.Contains(memoizable, producer.Recorded.Node.Kind) {
		c, _, err := load[claim](tx, ClaimProjection, string(src.Producer))
		cur, cerr := current(tx, producer, resolutions(c.Inputs))
		if err = errors.Join(err, cerr); err != nil || producer.State.Key.Generation != cur {
			return r, fmt.Sprintf("%s's key is of a stale generation", src.Producer), err
		}
	}
	r, _, err = load[types.ResultRecorded](tx, ResultProjection, string(*producer.State.Result))
	if err == nil && r.Result.Kind != src.ResultKind {
		return r, fmt.Sprintf("%s's result is a %s", src.Producer, r.Result.Kind), nil
	}
	if err != nil {
		return r, "", err
	}
	why, err := serves(tx, rec, producer, r.Result)
	return r, why, err
}

// current is the current generation of rec's identity in its task: the
// node with its inputs, without a generation (its generation-1 key).
func current(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved) (int64, error) {
	if !slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		return 1, nil // a first claim; invalidations move the claim itself
	}
	identity, err := Key(rec.Recorded.Node, resolved, 1)
	if err != nil {
		return 0, err
	}
	gen, ok, err := load[int64](tx, GenProjection, keyIndex(rec.Recorded.Task, identity.Key))
	if !ok {
		gen = 1
	}
	return gen, err
}

// claimKey claims key in rec's task for a memoizable kind: the key must be
// of its identity's current generation and held by no node, nor stale.
func claimKey(tx *eventstore.Tx, rec nodeRecord, resolved map[string]Resolved, key types.ComputationKey) error {
	cur, err := current(tx, rec, resolved)
	if err != nil || key.Generation != cur {
		return errors.Join(err, fmt.Errorf("the key is of generation %d, the current is %d", key.Generation, cur))
	}
	if !slices.Contains(memoizable, rec.Recorded.Node.Kind) {
		return nil
	}
	index := keyIndex(rec.Recorded.Task, key.Key)
	if _, taken, err := load[types.NodeID](tx, KeyProjection, index); err != nil || taken {
		return errors.Join(err, errors.New("the key is claimed in the task"))
	}
	return put(tx, KeyProjection, index, rec.Recorded.Node.ID)
}

func claimed(tx *eventstore.Tx, ev eventstore.Event, p *types.NodeClaimed) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != NodeClaimed || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a claim is under its node"))
	}
	if s := rec.State.State; s != types.NodeStateStateWaiting && s != types.NodeStateStateRunnable && !mutation.Is(mutation.ClaimPerDemand) {
		return fmt.Errorf("a %s node is not claimed", s)
	}
	resolved := resolutions(p.Inputs)
	if !reflect.DeepEqual(items(rec.Recorded.Node, resolved), p.Inputs) {
		return errors.New("the claim's inputs are not one resolution per declared input, in order")
	}
	if err := errors.Join(selectedInputs(tx, rec, resolved), bindsRequirement(tx, rec, resolved), rebaseClaimed(rec, resolved), carryClaimed(tx, rec, resolved)); err != nil {
		return err
	}
	key, err := Key(rec.Recorded.Node, resolved, p.Key.Generation)
	if err != nil || key != p.Key {
		return errors.Join(err, errors.New("the claim's key is not its node's key from its inputs"))
	}
	if err := claimKey(tx, rec, resolved, key); err != nil {
		return err
	}
	if err := put(tx, ClaimProjection, string(p.Node), claim{Key: key, Inputs: p.Inputs, Command: ev.Command}); err != nil {
		return err
	}
	rec.State.State, rec.State.WaitingOn, rec.State.Key = types.NodeStateStateClaimed, []types.Name{}, &key
	return putNode(tx, rec)
}
