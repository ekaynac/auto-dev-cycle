// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 6c: proposal.confirm, revalidation and atomic application.

// applyingNotes is note.add with logic: it notes the object. Its record is
// the note's shadow (shadowOf), which an event of another subject can
// change, so a stale state reaches the revalidation at apply rather than
// only the fold.
type applyingNotes struct {
	noteHandler
	applied int
}

// shadows maps an object to its shadow's id.
var shadows = map[string]string{}

func shadowOf(obj string) string {
	if _, ok := shadows[obj]; !ok {
		shadows[obj], _ = ids.New("prj")
	}
	return shadows[obj]
}

func (h *applyingNotes) Derive(tx *eventstore.Tx, a types.Actor, in json.RawMessage) (authority.Derived, error) {
	d, err := h.noteHandler.Derive(tx, a, in)
	d.Record = func(tx *eventstore.Tx) (json.RawMessage, bool, error) { return tx.Get(notes, shadowOf(d.Object)) }
	return d, err
}

func (h *applyingNotes) Apply(tx *eventstore.Tx, a types.Actor, in json.RawMessage) ([]eventstore.Event, json.RawMessage, error) {
	d, err := h.Derive(tx, a, in)
	id, err2 := ids.New("evt")
	h.applied++
	return []eventstore.Event{{ID: id, Kind: "test.noted/v1", Subject: d.Object, Payload: json.RawMessage(`{"by":"` + string(a.ID) + `"}`)}},
		json.RawMessage(`{"noted":true}`), errors.Join(err, err2)
}

// reviews is the test's authoritative review source.
type reviews map[types.ReceiptID]authority.Review

func (r reviews) Review(_ *eventstore.Tx, id types.ReceiptID, _ types.Proposal) (authority.Review, error) {
	if rv, ok := r[id]; ok {
		return rv, nil
	}
	return authority.Review{}, errors.New("no review " + string(id))
}

type confirmKit struct {
	*boundary
	base    time.Time // the real time at open; the grants are around it
	now     time.Time // the store clock's time, which only moves forward (advance)
	notes   *applyingNotes
	reviews reviews
	grant   types.Grant // the supervisor's, around the real time
}

// openConfirm is the boundary at the real time (event times are the
// store's clock), with the supervisor granted note.add and proposal.confirm
// under k.grant only.
func openConfirm(t *testing.T) *confirmKit { return openConfirmAt(t, 0) }

// openConfirmAt opens the kit with the store clock start from the real
// time; every command decides at the store clock (Tx.Now).
func openConfirmAt(t *testing.T, start time.Duration) *confirmKit {
	storeShift = start
	t.Cleanup(func() { storeShift = 0 })
	base := suiteNow().UTC()
	k := &confirmKit{boundary: openBoundary(t), base: base, now: base.Add(start), notes: &applyingNotes{}, reviews: reviews{}}
	k.az.Reviews = k.reviews
	k.az.Handlers = map[string]authority.Handler{"note.add": k.notes, authority.ConfirmAction: authority.ConfirmHandler{}}
	k.grant = k.supGrant
	k.grant.ID, k.grant.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{"note.add", authority.ConfirmAction}
	k.grant.ValidFrom, k.grant.ExpiresAt = types.Time(base.Add(-100*time.Hour).Format(time.RFC3339)), types.Time(base.Add(time.Hour).Format(time.RFC3339))
	k.seed(k.op, createGrant(k.grant))
	k.seed(k.op, revokeGrant(k.supGrant, k.now)) // k.grant alone covers the supervisor
	return k
}

// advance moves the store clock forward to to (event times never go back).
func (k *confirmKit) advance(to time.Time) {
	k.now, storeShift = to, to.Sub(suiteNow())
}

var accept = map[string]any{"classes": []string{"knowledge.accept_claim"}}

// propose has the supervisor propose noting a fresh object under
// independent review (grantee: an operator, so not the confirmer).
func (k *confirmKit) propose(grantee types.ActorID) types.Proposal {
	_, p := k.submitProposal(k.proposeCmd(k.sup, newID(k.t, "prj"), grantee, accept))
	if p.Policy != types.ProposalPolicyIndependentReview {
		k.t.Fatalf("the proposal's policy is %s", p.Policy)
	}
	return p
}

// review records a GO review of p by families, returning its id.
func (k *confirmKit) review(p types.Proposal, families ...string) types.ReceiptID {
	k.t.Helper()
	c := types.InstrumentContract{Name: "review-panel", Version: "1.0.0", Digest: types.Digest("sha256:" + strings.Repeat("a", 64)),
		SubjectType: types.InstrumentContractSubjectTypeProposal, RequiredPredicates: []types.InstrumentContractRequiredPredicatesItem{{Name: "findings", EvidenceRequired: []string{}}},
		OptionalPredicates: []string{}, Preconditions: []types.Name{}, ArtifactClasses: map[string]types.InstrumentContractArtifactClassesValue{}}
	r := types.Receipt{ID: types.ReceiptID(newID(k.t, "rcp")), Kind: types.ReceiptKindReview, Instrument: types.InstrumentRef{Name: c.Name, Version: c.Version, Digest: c.Digest},
		Subject:    types.Subject{Variant: &types.ProposalSubject{Type: "proposal", Proposal: p.ID, BindingDigest: p.BindingDigest}},
		Provenance: types.ReceiptProvenance{Command: types.CommandID(newID(k.t, "cmd")), Calls: []types.CallID{}, ModelFamilies: families, Contributors: []types.ActorID{}},
		ProducedAt: types.Time(k.now.Format(time.RFC3339)), Predicates: []types.Predicate{{Name: "findings", Outcome: types.Outcome(vocab.Empty),
			Reason: vocab.ReasonCompletedClean, Detail: "findings", Evidence: []types.PredicateEvidenceItem{}}}}
	d, err := receipt.Derive(c, r, nil)
	if err != nil {
		k.t.Fatal(err)
	}
	r.Outcome, r.Reason, r.Detail, r.Verdict, r.ConclusionsDigest = types.Outcome(d.Outcome), d.Reason, d.Detail, d.Verdict, d.ConclusionsDigest
	k.reviews[r.ID] = authority.Review{Receipt: r, Contract: c, Excluded: []string{"gpt"}, Event: types.EventID(newID(k.t, "evt"))}
	return r.ID
}

func (k *confirmKit) confirmCmd(by identity.Principal, p types.ProposalID, r types.ReceiptID) eventstore.Command {
	key := newID(k.t, "cmd")
	return by.Command(key, authority.ConfirmAction, key, []byte(`{"proposal":"`+string(p)+`","review":"`+string(r)+`"}`), nil)
}

func (k *confirmKit) confirm(c eventstore.Command) (eventstore.Committed, error) {
	return k.s.Submit(context.Background(), c, k.az.Confirm(c))
}

// refused: c is refused for reason, nothing committed or applied, and the
// proposal stays as it was.
func (k *confirmKit) refused(name string, c eventstore.Command, p types.ProposalID, check func(error) bool) {
	k.t.Helper()
	h, n, before := k.head(), k.notes.applied, k.proposal(p).Status
	if _, err := k.confirm(c); !check(err) {
		k.t.Errorf("%s: %v", name, err)
	}
	if k.head() != h || k.notes.applied != n || k.proposal(p).Status != before {
		k.t.Errorf("%s: committed or applied", name)
	}
}

// outcome is a committed confirmation's result.
type outcome struct {
	Proposal types.ProposalID `json:"proposal"`
	Approval types.ApprovalID `json:"approval"`
	Reason   string           `json:"reason"`
}

func (k *confirmKit) committed(c eventstore.Command, want eventstore.Disposition) outcome {
	k.t.Helper()
	out, err := k.confirm(c)
	if err != nil || out.Disposition != want {
		k.t.Fatalf("want %s: %+v %v", want, out, err)
	}
	var o outcome
	if err := json.Unmarshal(out.Result, &o); err != nil {
		k.t.Fatal(err)
	}
	return o
}

// Propose, an independent confirmation: approval, confirmation, the
// operation and its application commit together, exactly once; a replay
// returns the stored result and applies nothing.
func TestConfirmAppliesOnce(t *testing.T) {
	k := openConfirm(t)
	p := k.propose(k.op.Actor())
	rcp := k.review(p, "claude")
	c := k.confirmCmd(k.sys, p.ID, rcp)
	h := k.head()
	out, err := k.confirm(c)
	if err != nil || out.Disposition != eventstore.DispositionApplied || k.notes.applied != 1 {
		t.Fatalf("an independent confirmation: %+v %v", out, err)
	}
	var kinds []string
	k.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(h, 10)
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
			// The operation's event is the confirming command's: its actor.
			if e.Kind == "test.noted/v1" && (e.Actor != string(k.sys.Actor()) || e.Command == nil || *e.Command != out.CommandID) {
				t.Errorf("the operation's event is stamped %s by %v, want the confirmer %s", e.Actor, e.Command, k.sys.Actor())
			}
		}
		return err
	})
	if strings.Join(kinds, " ") != "approval.recorded/v1 proposal.confirmed/v1 test.noted/v1 proposal.applied/v1" {
		t.Errorf("the confirmation committed %v", kinds)
	}
	if got := k.proposal(p.ID); got.Status != types.ProposalStatusApplied {
		t.Errorf("the applied proposal is %s", got.Status)
	}
	var a types.Approval
	if err := json.Unmarshal([]byte(k.approvalOf(p.ID)), &a); err != nil || a.Actor != k.sys.Actor() || len(a.Confirmation.SatisfiedBy) != 1 ||
		a.Confirmation.SatisfiedBy[0] != k.reviews[rcp].Event || a.Confirmation.Policy != types.ApprovalConfirmationPolicyIndependentReview {
		t.Errorf("the approval: %+v %v", a, err)
	}
	k.view(func(tx *eventstore.Tx) error {
		if rec, ok, err := tx.Get(notes, p.Object.ID); err != nil || !ok || !strings.Contains(string(rec), string(k.sup.Actor())) {
			t.Errorf("the note is not the proposer's: %s %v", rec, err)
		}
		return nil
	})
	head := k.head()
	if again, err := k.confirm(c); err != nil || !again.Replayed || again.CommandID != out.CommandID || string(again.Result) != string(out.Result) ||
		again.FirstSeq != out.FirstSeq || again.LastSeq != out.LastSeq || k.head() != head || k.notes.applied != 1 {
		t.Errorf("a replay: %+v %v", again, err)
	}
	k.refused("a second confirmation", k.confirmCmd(k.op, p.ID, k.review(p, "claude")), p.ID, isReason(vocab.ReasonProposalNotOpen))
	k.rebuilds()
}

// The proposer, the grantee, a supervisor without the seat, an unrelated
// or dependent review and a human policy are refused; nothing commits.
func TestConfirmRefusals(t *testing.T) {
	k := openConfirm(t)
	op2 := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "second operator", CreatedBy: k.op.Actor()}
	var tok identity.Secret
	k.seed(k.op, create(op2))
	k.seed(k.op, issueTo(op2.ID, k.op.Actor(), &tok))
	grantee := k.login(tok)
	p := k.propose(grantee.Actor())
	good, other := k.review(p, "claude"), k.review(k.propose(k.op.Actor()), "claude")
	refused := isReason(vocab.ReasonConfirmationRefused)
	k.refused("the proposer", k.confirmCmd(k.sup, p.ID, good), p.ID, refused)
	k.refused("the grantee", k.confirmCmd(grantee, p.ID, good), p.ID, refused)
	seatless := k.supervisor()
	g := k.grant
	g.ID, g.Actor = types.GrantID(newID(t, "grt")), seatless.Actor()
	k.seed(k.op, createGrant(g))
	k.refused("a supervisor without the seat", k.confirmCmd(seatless, p.ID, good), p.ID, isReason(vocab.ReasonSeatNotHeld))
	// The operation is re-authorized as of now: its proposer's seat included.
	sup := k.sup.Actor()
	k.seed(k.sup, seat(types.SeatEventTransitionSeatReleased, &sup, nil, nil))
	k.refused("a proposer without the seat", k.confirmCmd(k.op, p.ID, good), p.ID, isReason(vocab.ReasonSeatNotHeld))
	k.seed(k.sup, seat(types.SeatEventTransitionSeatTaken, nil, &sup, nil))
	k.refused("another proposal's review", k.confirmCmd(k.op, p.ID, other), p.ID, func(err error) bool { return err != nil })
	k.refused("an unknown review", k.confirmCmd(k.op, p.ID, types.ReceiptID(newID(t, "rcp"))), p.ID, func(err error) bool { return err != nil })
	k.refused("a dependent review", k.confirmCmd(k.op, p.ID, k.review(p, "gpt")), p.ID, func(err error) bool {
		var r *receipt.Refusal
		return errors.As(err, &r) && r.Reason == vocab.ReasonReviewerNotIndependent
	})
	k.az.Reviews = nil
	k.refused("no review source", k.confirmCmd(k.op, p.ID, good), p.ID, func(err error) bool {
		return refused(err) && strings.Contains(err.Error(), authority.NoReviewSource)
	})
	k.az.Reviews = k.reviews
	deferred := func(err error) bool {
		return refused(err) && strings.Contains(err.Error(), authority.HumanConfirmationDeferred)
	}
	_, human := k.submitProposal(k.proposeCmd(k.sup, newID(t, "prj"), grantee.Actor(), nil))
	k.refused("a human policy", k.confirmCmd(k.op, human.ID, k.review(human, "claude")), human.ID, deferred)
	// KA1: an operator's own presence check needs WebAuthn too.
	_, own := k.submitProposal(k.proposeCmd(k.op, newID(t, "prj"), grantee.Actor(), map[string]any{"classes": []string{"knowledge.export_framework_prose"}}))
	k.refused("an operator's KA1 self-confirmation", k.confirmCmd(k.op, own.ID, k.review(own, "claude")), own.ID, deferred)
	// The logic confirms only under its own action (test.seed skips the Authorizer).
	seed := k.confirmCmd(k.op, p.ID, good)
	seed.Action = "test.seed"
	if _, err := k.confirm(seed); err == nil || k.proposal(p.ID).Status != types.ProposalStatusOpen {
		t.Error("the confirmation logic ran for another action")
	}
	if out := k.committed(k.confirmCmd(k.op, p.ID, good), eventstore.DispositionApplied); out.Proposal != p.ID {
		t.Errorf("the independent operator's confirmation: %+v", out)
	}
	k.rebuilds()
}

// A changed object, a revoked or lapsed grant and a tightened policy
// invalidate the proposal, committed with the invalidated disposition and
// its registered reason; nothing is applied.
func TestConfirmRevalidates(t *testing.T) {
	k := openConfirm(t)
	invalid := func(name string, p types.Proposal, want types.ProposalInvalidatedReasonValue) {
		t.Helper()
		if out := k.committed(k.confirmCmd(k.sys, p.ID, k.review(p, "claude")), eventstore.DispositionInvalidated); out.Reason != string(want) {
			t.Errorf("%s: the result's reason is %s", name, out.Reason)
		}
		if got := k.proposal(p.ID); got.Status != types.ProposalStatusInvalidated || *got.InvalidatedReason != want || k.notes.applied != 0 {
			t.Errorf("%s: %+v, applied %d", name, got, k.notes.applied)
		}
	}
	noted := func(subject string) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: "test.noted/v1", Subject: subject, Payload: json.RawMessage(`{"x":1}`)}}, nil
		}
	}
	// The object's record changed between propose and confirm.
	p := k.propose(k.op.Actor())
	k.seed(k.op, noted(shadowOf(p.Object.ID)))
	invalid("a changed object", p, types.ProposalInvalidatedReasonValueStaleExpectedState)
	// The object moved: the fold already invalidated it.
	p = k.propose(k.op.Actor())
	k.seed(k.op, noted(p.Object.ID))
	k.refused("a moved object", k.confirmCmd(k.op, p.ID, k.review(p, "claude")), p.ID, isReason(vocab.ReasonProposalNotOpen))
	// A tightened policy: a grant overriding note.add with human.
	p = k.propose(k.op.Actor())
	tight := k.grant
	tight.ID, tight.ConfirmationOverrides = types.GrantID(newID(t, "grt")), map[string]types.GrantConfirmationOverridesValue{"note.add": "human"}
	k.seed(k.op, createGrant(tight))
	invalid("a tightened policy", p, types.ProposalInvalidatedReasonValueProposalInvalidated)
	k.seed(k.op, revokeGrant(tight, k.now))
	// The grant revoked: the fold invalidates it.
	p = k.propose(k.op.Actor())
	k.seed(k.op, revokeGrant(k.grant, k.now))
	if got := k.proposal(p.ID); got.Status != types.ProposalStatusInvalidated || *got.InvalidatedReason != types.ProposalInvalidatedReasonValueAuthorityRevoked {
		t.Errorf("a revoked grant's proposal: %+v", got)
	}
	k.refused("a revoked grant", k.confirmCmd(k.op, p.ID, k.review(p, "claude")), p.ID, isReason(vocab.ReasonProposalNotOpen))
	// The grant lapsed (last: the store clock only moves forward).
	again := k.grant
	again.ID = types.GrantID(newID(t, "grt"))
	k.seed(k.op, createGrant(again))
	p = k.propose(k.op.Actor())
	k.advance(k.now.Add(2 * time.Hour))
	invalid("a lapsed grant", p, types.ProposalInvalidatedReasonValueProposalInvalidated)
	k.rebuilds()
}

// Confirmed past its expiry, a proposal is closed as expired: committed,
// refused, nothing applied, and never confirmed after.
func TestConfirmAfterExpiry(t *testing.T) {
	k := openConfirmAt(t, -authority.ProposalTTL-time.Minute)
	p := k.propose(k.op.Actor())
	k.advance(k.base)
	c := k.confirmCmd(k.op, p.ID, k.review(p, "claude"))
	if out := k.committed(c, eventstore.DispositionRefused); out.Reason != vocab.ReasonProposalExpired.String() {
		t.Errorf("the expired result: %+v", out)
	}
	if got := k.proposal(p.ID); got.Status != types.ProposalStatusExpired || k.notes.applied != 0 {
		t.Errorf("the expired proposal: %+v", got)
	}
	k.refused("an expired proposal", k.confirmCmd(k.op, p.ID, k.review(p, "claude")), p.ID, isReason(vocab.ReasonProposalNotOpen))
	k.rebuilds()
}

// The fold moves a proposal only one step at a time, as the record it is:
// no confirmation without its approval, no application of an open one, no
// expiry before its time.
func TestProposalTransitionGuards(t *testing.T) {
	k := openConfirmAt(t, -authority.ProposalTTL-time.Minute)
	old := k.propose(k.op.Actor())
	k.advance(k.base)
	p := k.propose(k.op.Actor())
	step := func(p types.Proposal, kind string, status types.ProposalStatus, change func(*types.Proposal)) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			n := p
			n.Status = status
			if change != nil {
				change(&n)
			}
			raw, _ := json.Marshal(n)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: kind, Subject: string(p.ID), Payload: raw}}, nil
		}
	}
	for name, f := range map[string]applyFn{
		"confirmed without an approval": step(p, authority.ProposalConfirmed, types.ProposalStatusConfirmed, nil),
		"applied while open":            step(p, authority.ProposalApplied, types.ProposalStatusApplied, nil),
		"expired before its time":       step(p, authority.ProposalExpired, types.ProposalStatusExpired, nil),
		"confirmed as applied":          step(p, authority.ProposalConfirmed, types.ProposalStatusApplied, nil),
		"expired with another scope": step(old, authority.ProposalExpired, types.ProposalStatusExpired, func(n *types.Proposal) {
			n.Scope = json.RawMessage(`{"x":1}`)
		}),
	} {
		if err := k.try(k.op, f); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
	if got, was := k.proposal(p.ID), k.proposal(old.ID); got.Status != types.ProposalStatusOpen || was.Status != types.ProposalStatusOpen {
		t.Errorf("the proposals moved to %s and %s", got.Status, was.Status)
	}
	if err := k.try(k.op, step(old, authority.ProposalExpired, types.ProposalStatusExpired, nil)); err != nil {
		t.Errorf("the expiry of an expired proposal: %v", err)
	}
	k.rebuilds()
}
