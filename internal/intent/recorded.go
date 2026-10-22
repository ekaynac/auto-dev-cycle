// SPDX-License-Identifier: Apache-2.0

package intent

// The builder's recorded manifest.intent (P22a-5c-2b-1; I18, I10
// consumption; W17): the controller records it in the launch command, after
// the attempt.started it binds, so no build attempt starts without one, and
// the fold checks it as it lands:
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
//     M1, M2): a builder's only (a reviewer's is 22a-6's), its inputs and
//     included refs exactly its node's rendered context at its claim
//     (graph.Context), the brief rendered into the prompt whose digest and
//     size it states, its executor its attempt's lease's, its tokens
//     within that lease's token reservation, its included bytes within its
//     byte budget, its data policy digest the project's adopted policy's
//     and each listed restriction's, and its destination and routing
//     allowed by each listed restriction.
//
// Its call is bound as it is first used: the attempt's submitted result and
// each usage measurement of the attempt name the call its intent recorded
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
	"io"
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
		if ev.Kind == lease.ResultSubmitted+"/v1" || ev.Kind == ledger.Measured {
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

// maxBrief bounds the brief blob bind reads.
const maxBrief = 1 << 20

// bind checks mi, a recorded intent CheckRecorded passed, against what the
// log holds of its attempt (the file's comment).
func (p Projector) bind(tx *eventstore.Tx, mi types.ManifestIntent) error {
	n, _, err := graph.AttemptNode(tx, *mi.Attempt)
	if err != nil {
		return err
	}
	if n.Node.Kind != types.NodeKindBuild {
		return fmt.Errorf("%w: only a builder's intent is recorded yet", ErrUnbound)
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
	prompt, err := p.prompt(rendered)
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
	case len(mi.Inputs) != 1 || len(mi.Included) != 1 || len(rendered.Omitted) != 0 || mi.Included[0].Ref.IdentityDigest != rendered.Included[0].IdentityDigest:
		return fmt.Errorf("%w: its inputs are not its node's rendered context, the brief included", ErrUnbound)
	case mi.Included[0].Bytes != int64(len(prompt)) || mi.Included[0].Bytes > mi.Budget.Bytes:
		return fmt.Errorf("%w: the brief renders into %d bytes, within its budget's %d", ErrUnbound, len(prompt), mi.Budget.Bytes)
	case mi.RenderedDigest != types.Digest("sha256:"+hex.EncodeToString(sum[:])):
		return fmt.Errorf("%w: its rendered digest is not the rendered brief's", ErrUnbound)
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

// prompt is the builder's prompt rendered's one included ref, the brief,
// renders into (brief.Prompt).
func (p Projector) prompt(rendered graph.Rendered) ([]byte, error) {
	if len(rendered.Included) != 1 {
		return nil, fmt.Errorf("%w: a build renders one brief, not %d refs", ErrUnbound, len(rendered.Included))
	}
	rc, err := p.Env.Blobs.Open(string(rendered.Included[0].ContentDigest))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxBrief+1))
	if err = errors.Join(err, rc.Close()); err != nil {
		return nil, err
	}
	if len(b) > maxBrief {
		return nil, fmt.Errorf("%w: the brief is over %d bytes", ErrUnbound, maxBrief)
	}
	pb, err := brief.Decode(b)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(brief.Prompt(pb)), nil
}

// calledAs refuses ev, an attempt's submitted result or a usage
// measurement, when its attempt recorded an intent under another call.
func calledAs(tx *eventstore.Tx, ev eventstore.Event) error {
	var attempt types.AttemptID
	var call types.CallID
	if ev.Kind == ledger.Measured {
		var m types.UsageMeasurement
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			return err
		}
		attempt, call = m.Attempt, m.Call
	} else {
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
