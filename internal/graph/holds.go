// SPDX-License-Identifier: Apache-2.0

package graph

// Node holds (02 "Hold reasons and resume signals"; 13 "Evaluation",
// [G2], supersession; plan 15 G3-3).
//
// Decisions:
//   - The builders check a hold against the registry and record what is in
//     force when it is raised: its owner, escalation and page_after. The
//     fold checks events against those recorded values only, so a later
//     registry edit changes no recorded hold and the log still rebuilds.
//   - A hold has an id no other hold had. It is raised at its event's time
//     with no page. A completed node holds nothing: records/node-state has
//     no held state with a selected result. A held node keeps its key and
//     missing inputs; once it holds nothing it is claimed, waiting or
//     runnable again, as they say.
//   - A could_not_run result retries on its signal (13 [G2]): Retry holds
//     the node on the hold the result's reason names, with that reason and
//     the result, one hold per result, of the node's current key. A reason
//     that names no hold leaves the retry to the rails (05, 12).
//   - A page is recorded at its event's time, once per owner and ref: to
//     the hold's owner, or, from since + page_after on, to the recorded
//     escalate_to and its exact ref, which then owns the hold. A hold with
//     no page_after is never paged.
//   - A hold leaves with a cause: its signal (the hold's signal instance),
//     or retired: as invalidated, a retry wait of a key the node no longer
//     holds, or as revised, an awaiting_input wait on an answer its
//     successor moves to a newer revision of the owed input (successor.go;
//     the fold cannot check the revision until M1 records owed inputs). An
//     invalidation retires its retry waits in the same command and keeps
//     every other wait, with its id, since and pages.
//   - Transferring a hold to a replacement is G6b's, inside its
//     supersession. The evaluator ties awaiting_input to an owed input the
//     node declares (evaluate.go); this fold does not, until M1 records owed
//     inputs. As for Claim, M1's commands take the project from the actor's
//     authorization before they call these builders.

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// Raise is the event raising hold on node now (it stamps since), a
// registered hold by its registry owner. Raise records the registry's escalation (for an
// environment owner, envOwner names it; none falls back to the operator,
// 02) and page_after, unless hold carries a project's override of it.
func Raise(tx *eventstore.Tx, node types.NodeID, hold types.NodeHold, envOwner *string) ([]eventstore.Event, error) {
	return raise(tx, node, hold, envOwner, nil)
}

func raise(tx *eventstore.Tx, node types.NodeID, h types.NodeHold, envOwner *string, result *types.ResultID) ([]eventstore.Event, error) {
	reg, err := registry()
	if err != nil {
		return nil, err
	}
	entry, ok := reg.Holds[h.Hold]
	if !ok || h.Owner != entry.Owner {
		return nil, fmt.Errorf("graph: %s is no registered hold of owner %s", h.Hold, h.Owner)
	}
	if err := ownerRef(h.Owner, h.OwnerRef); err != nil {
		return nil, err
	}
	h.Since = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
	h.EscalateTo, h.EscalateRef = entry.EscalateTo, nil
	if h.PageAfter == nil {
		h.PageAfter = entry.PageAfter
	}
	if e := entry.EscalateTo; e != nil && *e == types.OwnerEnvironmentOwner {
		if h.EscalateRef = envOwner; envOwner == nil {
			h.EscalateTo = ptrOf(types.OwnerOperator)
		}
	}
	return onNode(tx, node, HoldRaised, string(node), &types.HoldRaised{Type: "hold_raised", Node: node, Hold: h, Result: result})
}

func ptrOf[T any](v T) *T { return &v }

// ownerRef: a grant holder is named by an actor id (adc-typeid), a named
// or environment owner by a non-null ref; the other owners need none.
func ownerRef(owner types.Owner, ref *string) error {
	switch {
	case owner == types.OwnerGrantHolder && ref != nil:
		return ids.Check(*ref, "act")
	case owner == types.OwnerGrantHolder || owner == types.OwnerNamedOwner || owner == types.OwnerEnvironmentOwner:
		if ref == nil {
			return fmt.Errorf("graph: a %s hold names its owner", owner)
		}
	}
	return nil
}

// Retry is the event holding node on the signal of its could_not_run
// result of its current key: the hold the result's reason names, with that
// reason; hold carries the rest (id, signal instance, owner).
func Retry(tx *eventstore.Tx, node types.NodeID, result types.ResultID, hold types.NodeHold, envOwner *string) ([]eventstore.Event, error) {
	r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(result))
	if _, built := builtWaits[r.Result.Reason.String()]; err != nil || built {
		return nil, errors.Join(err, fmt.Errorf("graph: a %s wait is raised by Probe or Exhaust", r.Result.Reason))
	}
	return retry(tx, node, result, hold, envOwner)
}

func retry(tx *eventstore.Tx, node types.NodeID, result types.ResultID, hold types.NodeHold, envOwner *string) ([]eventstore.Event, error) {
	rec, err := loadNode(tx, node)
	r, ok, rerr := load[types.ResultRecorded](tx, ResultProjection, string(result))
	reg, gerr := registry()
	if err = errors.Join(err, rerr, gerr); err != nil || !ok || r.Node != node || rec.State.Key == nil || rec.State.Key.Key != r.Result.Computation {
		return nil, errors.Join(err, fmt.Errorf("graph: %s is no result of %s's current key", result, node))
	}
	// Only could_not_run reasons name a hold (reasons.yaml); the fold checks
	// the outcome itself.
	entry := reg.Reasons[r.Result.Reason.String()]
	if entry.Hold == nil {
		return nil, fmt.Errorf("graph: reason %s names no hold; the rails decide its retry", r.Result.Reason)
	}
	hold.Hold, hold.Reason = string(*entry.Hold), &r.Result.Reason
	return raise(tx, node, hold, envOwner, &result)
}

// Page is the event recording a page of node's hold. The fold admits only
// the recorded owner or escalation, refs included, so the raise checked them.
func Page(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, owner types.Owner, ownerRef *string, channel *types.Name) ([]eventstore.Event, error) {
	return onNode(tx, node, HoldPaged, string(node), &types.HoldPaged{Type: "hold_paged", Node: node, Hold: hold, Owner: owner, OwnerRef: ownerRef, Channel: channel})
}

// Clear is the event clearing node's hold on its signal.
func Clear(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, signal string) ([]eventstore.Event, error) {
	return cleared(tx, node, hold, &types.Signalled{Type: "signal", SignalInstance: signal})
}

func cleared(tx *eventstore.Tx, node types.NodeID, hold types.HoldID, cause types.HoldClearedCauseVariant) ([]eventstore.Event, error) {
	return onNode(tx, node, HoldCleared, string(node), &types.HoldCleared{Type: "hold_cleared", Node: node, Hold: hold, Cause: types.HoldClearedCause{Variant: cause}})
}

// heldBy is a hold's index entry: the node that last held it, and the
// could_not_run result of a retry wait.
type heldBy struct {
	Node   types.NodeID    `json:"node"`
	Result *types.ResultID `json:"result"`
}

// settle is a node's state once it holds nothing: claimed with a key,
// waiting with missing inputs, runnable otherwise.
func settle(st *types.NodeState) {
	switch {
	case len(st.Holds) > 0:
		st.State = types.NodeStateStateHeld
	case st.Key != nil:
		st.State = types.NodeStateStateClaimed
	case len(st.WaitingOn) > 0:
		st.State = types.NodeStateStateWaiting
	default:
		st.State = types.NodeStateStateRunnable
	}
}

// sameTime: a and b are one instant.
func sameTime(a, b string) bool {
	x, err1 := time.Parse(time.RFC3339Nano, a)
	y, err2 := time.Parse(time.RFC3339Nano, b)
	return err1 == nil && err2 == nil && x.Equal(y)
}

// isoDuration bounds each component to 9 digits, so its seconds fit an
// int64; a longer Duration the schema admits is an error, never "due".
var isoDuration = regexp.MustCompile(`^P(?:(\d{1,9})D)?(?:T(?:(\d{1,9})H)?(?:(\d{1,9})M)?(?:(\d{1,9})S)?)?$`)

// maxSeconds is the longest Duration a time.Duration holds.
const maxSeconds = int64(math.MaxInt64 / int64(time.Second))

// due: at is at or after since + after (a records Duration).
func due(since types.Time, after types.Duration, at string) (bool, error) {
	m := isoDuration.FindStringSubmatch(string(after))
	s, err1 := time.Parse(time.RFC3339Nano, string(since))
	t, err2 := time.Parse(time.RFC3339Nano, at)
	if m == nil || err1 != nil || err2 != nil {
		return false, fmt.Errorf("page_after %s or a time is unreadable", after)
	}
	var secs int64
	for i, unit := range []int64{86400, 3600, 60, 1} {
		n, _ := strconv.ParseInt("0"+m[i+1], 10, 64) // at most 9 digits
		secs += n * unit
	}
	if secs > maxSeconds {
		return false, fmt.Errorf("page_after %s is longer than the clock holds", after)
	}
	return !t.Before(s.Add(time.Duration(secs) * time.Second)), nil
}

func holdRaised(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldRaised) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != HoldRaised || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a hold is raised under its node"))
	}
	h := p.Hold
	if !sameTime(string(h.Since), ev.At) || len(h.Pages) != 0 || (h.Reason == nil) != (p.Result == nil) {
		return errors.New("a hold is raised at its event's time with no page, with a result exactly when it has a reason")
	}
	if p.Result != nil {
		if err := builtWait(*h.Reason, h.SignalInstance); err != nil {
			return err
		}
		r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(*p.Result))
		if err != nil || r.Node != p.Node || r.Result.Outcome != types.OutcomeCouldNotRun || r.Result.Reason != *h.Reason ||
			rec.State.Key == nil || rec.State.Key.Key != r.Result.Computation {
			return errors.Join(err, errors.New("a retry wait is of the node's could_not_run result of its current key, with its reason"))
		}
		if _, retried, err := load[types.HoldID](tx, RetryProjection, string(*p.Result)); err != nil || retried {
			return errors.Join(err, errors.New("the result has a retry wait"))
		}
		if err := put(tx, RetryProjection, string(*p.Result), h.ID); err != nil {
			return err
		}
	}
	if _, used, err := load[heldBy](tx, HoldProjection, string(h.ID)); err != nil || used {
		return errors.Join(err, errors.New("the hold id is taken"))
	}
	rec.State.State, rec.State.Holds = types.NodeStateStateHeld, append(rec.State.Holds, h)
	if err := put(tx, HoldProjection, string(h.ID), heldBy{Node: p.Node, Result: p.Result}); err != nil {
		return err
	}
	return putNode(tx, rec)
}

func holdPaged(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldPaged) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != HoldPaged || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a page is under its node"))
	}
	i := slices.IndexFunc(rec.State.Holds, func(h types.NodeHold) bool { return h.ID == p.Hold })
	if i < 0 {
		return fmt.Errorf("node %s does not hold %s", p.Node, p.Hold)
	}
	h := &rec.State.Holds[i]
	same := func(o types.Owner, ref *string, want types.Owner, wantRef *string) bool {
		return o == want && (ref == nil) == (wantRef == nil) && (ref == nil || *ref == *wantRef)
	}
	switch {
	case h.PageAfter == nil:
		return errors.New("a hold with no page_after is never paged")
	case h.EscalateTo != nil && same(p.Owner, p.OwnerRef, *h.EscalateTo, h.EscalateRef):
		if ok, err := due(h.Since, *h.PageAfter, ev.At); !ok { // an unreadable page_after is not due
			return errors.Join(err, errors.New("an escalation comes after since + page_after"))
		}
		h.Owner, h.OwnerRef = p.Owner, p.OwnerRef
	case !same(p.Owner, p.OwnerRef, h.Owner, h.OwnerRef):
		return errors.New("a page goes to the hold's owner, or after page_after to its escalation")
	}
	if slices.ContainsFunc(h.Pages, func(x types.NodeHoldPagesItem) bool { return same(x.Owner, x.OwnerRef, p.Owner, p.OwnerRef) }) {
		return errors.New("the owner was paged for this signal")
	}
	h.Pages = append(h.Pages, types.NodeHoldPagesItem{At: types.Time(ev.At), Owner: p.Owner, OwnerRef: p.OwnerRef, Channel: p.Channel, SignalInstance: h.SignalInstance})
	return putNode(tx, rec)
}

func holdCleared(tx *eventstore.Tx, ev eventstore.Event, p *types.HoldCleared) error {
	rec, err := nodeOf(tx, ev, p.Node)
	if err != nil || ev.Kind != HoldCleared || ev.Subject != string(p.Node) {
		return errors.Join(err, errors.New("a hold is cleared under its node"))
	}
	i := slices.IndexFunc(rec.State.Holds, func(h types.NodeHold) bool { return h.ID == p.Hold })
	if i < 0 {
		return fmt.Errorf("node %s does not hold %s", p.Node, p.Hold)
	}
	switch c := p.Cause.Variant.(type) {
	case *types.Signalled:
		if c.SignalInstance != rec.State.Holds[i].SignalInstance {
			return errors.New("a signal clears the hold it is the signal of")
		}
		if by, _, err := load[heldBy](tx, HoldProjection, string(p.Hold)); err != nil || by.Result != nil && mutation.Is(mutation.RetryNeedsInvalidation) {
			return errors.Join(err, errors.New("only an invalidation ends a retry wait"))
		}
	case *types.Retired:
		by, _, err := load[heldBy](tx, HoldProjection, string(p.Hold))
		if err != nil {
			return err
		}
		switch c.Reason {
		case types.RetiredReasonInvalidated: // a retry wait of a key the node no longer holds
			if by.Result == nil {
				return errors.New("only a retry wait is retired as invalidated")
			}
			r, _, err := load[types.ResultRecorded](tx, ResultProjection, string(*by.Result))
			if err != nil || r.Result.Computation == rec.State.Key.Key {
				return errors.Join(err, errors.New("a retry wait of the current key is not retired"))
			}
		case types.RetiredReasonRevised: // a wait on an answer its successor moves to a newer revision (successor.go)
			if rec.State.Holds[i].Hold != "awaiting_input" { // never a retry wait: no reason names it
				return errors.New("only an awaiting_input wait is retired as revised")
			}
		default:
			return fmt.Errorf("a hold is not retired as %s", c.Reason)
		}
	}
	rec.State.Holds = slices.Delete(rec.State.Holds, i, i+1)
	settle(&rec.State)
	return putNode(tx, rec)
}
