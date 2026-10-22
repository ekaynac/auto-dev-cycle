// SPDX-License-Identifier: Apache-2.0

package intent

// The builder's and the reviewer's recorded manifest.intent (P22a-5c-2b-1,
// P22a-6b; I18, I10 consumption; W17): the controller records it in the
// launch command, after the attempt.started it binds, so no build or review
// attempt starts without one, and the fold checks it as it lands:
//   - a records/manifest-intent (the event's schema) whose refs are all
//     checked refs, each input named once, included or omitted
//     (knowledge.CheckIntent);
//   - for its own task and its own attempt, stated in the consumer's role
//     its attempt's node gives (a build's: builder), every ref it lists
//     walked by the I18 walk over the task's records (CheckRecorded): an
//     input derived from the acceptance, or a plan ref that is not the
//     task's own brief, is refused;
//   - one per attempt (RecordedProjection);
//   - bound to what the log holds of its attempt (bind; Opus 22a-5c-2b-1
//     M1, M2): its inputs, included and omitted refs exactly its node's
//     rendered context at its claim (graph.Context) as its role renders it
//     (rendersAs: a builder's brief into brief.Prompt; a reviewer's brief,
//     patch and gate report into review.Prompt, the gate's receipt omitted,
//     read from the log by Review), the prompt's digest and sizes as it
//     states them, its executor its attempt's lease's, its tokens
//     within that lease's token reservation, its byte budget MaxPrompt
//     (which a brief of at most MaxBrief renders well within), its data policy digest the project's adopted policy's
//     and each listed restriction's, and its destination and routing
//     allowed by each listed restriction.
//
// Its call is bound as it is first used: the attempt's model call binding,
// its submitted result and each usage measurement of the attempt name the
// call its intent recorded
// (a fact of another call is refused). A refused intent refuses its
// command: the launch starts nothing.
//
// Replay folds the intents again, so it reads their blobs (the brief, and
// any declassified output CheckRecorded walks): a missing or damaged blob
// refuses the replay, as the seal fold's do.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/brief"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity/ledger"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
	"github.com/ekaynac/auto-dev-cycle/internal/lease"
	"github.com/ekaynac/auto-dev-cycle/internal/modelcall"
)

// The recorded intent's event kind and projection.
const (
	Recorded           = "manifest.intent/v1"
	RecordedProjection = "intent_recorded" // attempt id → the intent it recorded
	recordSchema       = "records/manifest-intent"
)

// Kinds are the recorded intent's events.
func Kinds() eventstore.Kinds {
	return eventstore.Kinds{Recorded: {Schema: recordSchema}}
}

// Record is the event recording mi, its subject its manifest id.
func Record(mi types.ManifestIntent) (eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return eventstore.Event{}, err
	}
	raw, err := json.Marshal(mi)
	return eventstore.Event{ID: id, Kind: Recorded, Subject: string(mi.ID), Payload: raw}, err
}

// Projector folds the recorded intents, checking each under Env: its Blobs
// (the brief, the declassified outputs CheckRecorded's walk verifies) and
// its Authority (the answers a rendered context includes).
type Projector struct{ Env graph.Env }

// Project folds evs' recorded intents (the file's comment).
func (p Projector) Project(tx *eventstore.Tx, evs []eventstore.Event) error {
	for _, ev := range evs {
		if ev.Kind == lease.ResultSubmitted+"/v1" || ev.Kind == ledger.Measured || ev.Kind == modelcall.Intended {
			if err := calledAs(tx, ev); err != nil {
				return err
			}
			continue
		}
		if ev.Kind != Recorded {
			continue
		}
		var mi types.ManifestIntent
		if err := json.Unmarshal(ev.Payload, &mi); err != nil {
			return err
		}
		switch {
		case ev.Subject != string(mi.ID) || mi.Task == nil || mi.Attempt == nil:
			return fmt.Errorf("intent: a recorded intent is under its manifest id and names its task and attempt")
		case p.Env.Blobs == nil:
			return fmt.Errorf("intent: no blobs to check a recorded intent")
		}
		if err := knowledge.CheckIntent(mi); err != nil {
			return err
		}
		if _, dup, err := tx.Get(RecordedProjection, string(*mi.Attempt)); err != nil || dup {
			return errors.Join(err, fmt.Errorf("intent: attempt %s already recorded its intent", *mi.Attempt))
		}
		if err := CheckRecorded(tx, p.Env.Blobs, *mi.Task, mi); err != nil {
			return err
		}
		if err := p.bind(tx, mi); err != nil {
			return err
		}
		raw, err := json.Marshal(mi)
		if err == nil {
			err = tx.Put(RecordedProjection, string(*mi.Attempt), raw)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RecordedOf is the intent attempt recorded, if any.
func RecordedOf(tx *eventstore.Tx, attempt types.AttemptID) (types.ManifestIntent, bool, error) {
	raw, ok, err := tx.Get(RecordedProjection, string(attempt))
	var mi types.ManifestIntent
	if err == nil && ok {
		err = json.Unmarshal(raw, &mi)
	}
	return mi, ok, err
}

// ErrUnbound is a recorded intent that misstates what the log holds of its
// attempt, or a fact of its attempt under another call than its intent's.
var ErrUnbound = errors.New("intent: the intent does not state what its attempt was given")

// MaxBrief bounds the brief blob a builder's run and bind read; MaxPrompt
// bounds the prompt a brief renders into, a builder intent's byte budget:
// 09's text bound (adapterhost.BoundText).
const (
	MaxBrief  = 1 << 20
	MaxPrompt = 16 << 20
)

// bind checks mi, a recorded intent CheckRecorded passed, against what the
// log holds of its attempt (the file's comment).
func (p Projector) bind(tx *eventstore.Tx, mi types.ManifestIntent) error {
	n, _, err := graph.AttemptNode(tx, *mi.Attempt)
	if err != nil {
		return err
	}
	a, _, _, err := lease.AttemptAt(tx, *mi.Attempt)
	if err != nil {
		return err
	}
	l, _, ok, err := lease.LeaseOf(tx, a.Lease)
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("%w: attempt %s's lease is not recorded", ErrUnbound, *mi.Attempt))
	}
	rendered, err := graph.Context(tx, n.Node.ID, p.Env)
	if err != nil {
		return err
	}
	prompt, err := p.rendersAs(tx, n.Node, rendered, mi)
	if err != nil {
		return err
	}
	raw, _, err := authority.ProjectDataPolicy(tx, n.Project)
	if err != nil {
		return err
	}
	policy, err := authority.PolicyDigest(raw)
	if err != nil {
		return err
	}
	var tokens int64
	for _, res := range l.Reservations {
		if res.Unit == types.CapacityUnitTokens {
			tokens += int64(res.Amount)
		}
	}
	sum := sha256.Sum256(prompt)
	switch {
	case mi.Budget.Bytes != MaxPrompt:
		return fmt.Errorf("%w: its byte budget is %d, not MaxPrompt %d", ErrUnbound, mi.Budget.Bytes, MaxPrompt)
	case mi.RenderedDigest != types.Digest("sha256:"+hex.EncodeToString(sum[:])):
		return fmt.Errorf("%w: its rendered digest is not its rendered prompt's", ErrUnbound)
	case mi.Consumer.Executor != l.Executor:
		return fmt.Errorf("%w: its executor is not its lease's %s", ErrUnbound, l.Executor)
	case mi.Budget.Tokens > tokens:
		return fmt.Errorf("%w: its %d tokens are over its lease's reservation of %d", ErrUnbound, mi.Budget.Tokens, tokens)
	case mi.Policy.DataPolicyDigest != policy:
		return fmt.Errorf("%w: its data policy digest is not project %s's", ErrUnbound, n.Project)
	}
	for _, r := range mi.Inputs {
		for _, x := range r.Restrictions {
			switch {
			case x.Project == n.Project && string(x.PolicyDigest) != policy:
				return fmt.Errorf("%w: input %s is under another data policy than its intent's", ErrUnbound, r.ID)
			case !slices.Contains(x.AllowedDestinations, mi.Destination):
				return fmt.Errorf("%w: input %s may not go to destination %s", ErrUnbound, r.ID, mi.Destination)
			case x.Routing == types.RestrictionRoutingVerified && mi.Routing != types.ManifestIntentRoutingVerified:
				return fmt.Errorf("%w: input %s needs verified routing", ErrUnbound, r.ID)
			}
		}
	}
	return nil
}

// rendersAs is the prompt n's context, rendered, renders into, for n's
// role, once mi states exactly that context (P22a-6b: per role):
//   - a build: its one input, the brief, included at the prompt's size
//     (brief.Prompt);
//   - a review: its rendered context as Review reads it from the log, the
//     brief, the patch and the gate's report included at their sizes and
//     the gate's receipt omitted (review.Prompt).
//
// Any other node's intent is refused.
func (p Projector) rendersAs(tx *eventstore.Tx, n types.Node, rendered graph.Rendered, mi types.ManifestIntent) ([]byte, error) {
	switch n.Kind {
	case types.NodeKindBuild:
		prompt, err := p.prompt(rendered)
		switch {
		case err != nil:
			return nil, err
		case len(mi.Inputs) != 1 || len(mi.Included) != 1 || len(rendered.Omitted) != 0 || mi.Included[0].Ref.IdentityDigest != rendered.Included[0].IdentityDigest:
			return nil, fmt.Errorf("%w: its inputs are not its node's rendered context, the brief included", ErrUnbound)
		case mi.Included[0].Bytes != int64(len(prompt)):
			return nil, fmt.Errorf("%w: the brief renders into %d bytes, its intent states %d", ErrUnbound, len(prompt), mi.Included[0].Bytes)
		}
		return prompt, nil
	case types.NodeKindReview:
		rc, err := Review(tx, p.Env.Blobs, n.ID, rendered)
		if err != nil {
			return nil, err
		}
		inputs := slices.Clone(rc.Included)
		for _, o := range rc.Omitted {
			inputs = append(inputs, o.Ref)
		}
		included := func(x, y types.ManifestIntentIncludedItem) bool {
			return x.Ref.IdentityDigest == y.Ref.IdentityDigest && x.Bytes == y.Bytes
		}
		want := []types.ManifestIntentIncludedItem{}
		for _, r := range rc.Included {
			want = append(want, types.ManifestIntentIncludedItem{Ref: r, Bytes: rc.Bytes[r.IdentityDigest]})
		}
		omitted := func(x, y types.ManifestIntentOmittedItem) bool {
			return x.Ref.IdentityDigest == y.Ref.IdentityDigest && x.Reason == y.Reason
		}
		if !sameRefs(mi.Inputs, inputs) || !slices.EqualFunc(mi.Included, want, included) || !slices.EqualFunc(mi.Omitted, rc.Omitted, omitted) {
			return nil, fmt.Errorf("%w: its inputs are not its review's rendered context: the brief, the patch and the gate's report included at their sizes, its receipt omitted", ErrUnbound)
		}
		return rc.Prompt, nil
	}
	return nil, fmt.Errorf("%w: a %s node has no consumer's intent", ErrUnbound, n.Kind)
}

// prompt is the builder's prompt rendered's one included ref, the brief,
// renders into (brief.Prompt).
func (p Projector) prompt(rendered graph.Rendered) ([]byte, error) {
	if len(rendered.Included) != 1 {
		return nil, fmt.Errorf("%w: a build renders one brief, not %d refs", ErrUnbound, len(rendered.Included))
	}
	b, ok, err := readBlob(p.Env.Blobs, rendered.Included[0].ContentDigest, MaxBrief)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("%w: the brief is over %d bytes, or not its digest's", ErrUnbound, MaxBrief)
	}
	pb, err := brief.Decode(b)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(brief.Prompt(pb)), nil
}

// calledAs refuses ev, an attempt's submitted result, a usage measurement
// or a model call's binding (modelcall.Intended; Mythos 22a-5c-2b-1 L1),
// when its attempt recorded an intent under another call.
func calledAs(tx *eventstore.Tx, ev eventstore.Event) error {
	var attempt types.AttemptID
	var call types.CallID
	switch ev.Kind {
	case modelcall.Intended:
		var m types.ModelCall
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return err
		}
		attempt, call = m.Attempt, m.Call
	case ledger.Measured:
		var m types.UsageMeasurement
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return err
		}
		attempt, call = m.Attempt, m.Call
	default:
		var s struct {
			Result *types.SubmittedResult `json:"result"`
		}
		if err := json.Unmarshal(ev.Payload, &s); err != nil {
			return err
		}
		if s.Result == nil {
			return nil
		}
		attempt, call = types.AttemptID(ev.Subject), s.Result.Call
	}
	mi, ok, err := RecordedOf(tx, attempt)
	if err != nil || !ok || mi.Call == call {
		return err
	}
	return fmt.Errorf("%w: attempt %s's call %s is not its intent's %s", ErrUnbound, attempt, call, mi.Call)
}
