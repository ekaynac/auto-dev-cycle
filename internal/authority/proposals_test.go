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
)

// 14 PR 6a: proposal and approval persistence.

// proposeCmd is note.add of obj on project A that the derivation makes a
// grant change for grantee: a supervisor's needs a human, so it is proposed.
func (b *boundary) proposeCmd(p identity.Principal, obj string, grantee types.ActorID, extra map[string]any) eventstore.Command {
	b.t.Helper()
	in := map[string]any{"project": prjA, "object": obj, "classes": []string{"grant.create"}, "grantee": grantee}
	for k, v := range extra {
		in[k] = v
	}
	raw, err := json.Marshal(in)
	if err != nil {
		b.t.Fatal(err)
	}
	key := newID(b.t, "cmd")
	return p.Command(key, "note.add", key, raw, nil)
}

// submitProposal submits c with logic that must never run, and returns
// the proposal its result names.
func (b *boundary) submitProposal(c eventstore.Command) (eventstore.Committed, types.Proposal) {
	b.t.Helper()
	out, err := b.s.Submit(context.Background(), c, eventstore.Applies(func(*eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
		b.t.Error("the logic of a proposed command ran")
		return nil, json.RawMessage(`{}`), nil
	}))
	if err != nil || out.Disposition != eventstore.DispositionProposed {
		b.t.Fatalf("a proposed command: %+v %v", out, err)
	}
	var res struct {
		Proposal      types.ProposalID `json:"proposal"`
		Policy        string           `json:"policy"`
		BindingDigest types.Digest     `json:"binding_digest"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil {
		b.t.Fatal(err)
	}
	p := b.proposal(res.Proposal)
	if res.Policy != string(p.Policy) || res.BindingDigest != p.BindingDigest {
		b.t.Errorf("the result %s does not name the proposal %+v", out.Result, p)
	}
	return out, p
}

func (b *boundary) proposal(id types.ProposalID) (p types.Proposal) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, ok, err := tx.Get(authority.ProposalProjection, string(id))
		if err == nil && !ok {
			err = errors.New("no proposal " + string(id))
		}
		if err == nil {
			err = json.Unmarshal(raw, &p)
		}
		return err
	})
	return p
}

func TestProposeCommitsAProposalAndNoEffect(t *testing.T) {
	b := openBoundary(t)
	obj, op := newID(t, "prj"), b.op.Actor()
	h := b.head()
	c := b.proposeCmd(b.sup, obj, op, nil)
	out, p := b.submitProposal(c)
	if b.head() != h+1 || out.FirstSeq != h+1 {
		t.Errorf("a proposal appends exactly its own event: head %d -> %d", h, b.head())
	}
	zero := int64(0)
	if p.Status != types.ProposalStatusOpen || p.Action != "note.add" || p.ProposedBy != b.sup.Actor() || *p.Grantee != op ||
		*p.Grant != b.supGrant.ID || p.Policy != types.ProposalPolicyHuman || *p.Project != prjA || p.InvalidatedReason != nil ||
		p.Object.ID != obj || p.Object.Type != "note" || *p.ExpectedState.Version != zero || p.ExpectedState.Digest != nil ||
		len(p.ChangeClass) != 1 || p.ChangeClass[0] != "grant.create" || p.CreatedAt != types.Time(b.recorded[string(p.ID)].At) {
		t.Errorf("the proposal binds another operation: %+v", p)
	}
	b.view(func(tx *eventstore.Tx) error {
		if _, ok, err := tx.Get(notes, obj); err != nil || ok {
			t.Errorf("the proposed note was applied: %v", err)
		}
		return nil
	})
	// A replay of the key replays the proposal; nothing is appended.
	if again, err := b.note(c); err != nil || !again.Replayed || again.Disposition != eventstore.DispositionProposed || b.head() != h+1 {
		t.Errorf("a replay: %+v %v", again, err)
	}
	b.rebuilds()
}

// The proposal binds the exact operation: the same operation again, under
// another key, returns the pending proposal without an event; changing any
// bound field makes another proposal.
func TestProposalBindsTheExactOperation(t *testing.T) {
	b := openBoundary(t)
	obj, op := newID(t, "prj"), b.op.Actor()
	_, first := b.submitProposal(b.proposeCmd(b.sup, obj, op, nil))
	h := b.head()
	if _, same := b.submitProposal(b.proposeCmd(b.sup, obj, op, nil)); same.ID != first.ID || b.head() != h {
		t.Errorf("the same operation pending made %s, appending %d", same.ID, b.head()-h)
	}
	sup2 := b.supervisor().Actor()
	seen := map[types.ProposalID]bool{first.ID: true}
	for name, c := range map[string]eventstore.Command{
		"the object":  b.proposeCmd(b.sup, newID(t, "prj"), op, nil),
		"the grantee": b.proposeCmd(b.sup, obj, sup2, nil),
		"the input":   b.proposeCmd(b.sup, obj, op, map[string]any{"reason": "other"}),
	} {
		if _, p := b.submitProposal(c); seen[p.ID] {
			t.Errorf("changing %s kept proposal %s", name, p.ID)
		} else {
			seen[p.ID] = true
		}
	}
	// The object's state: an edit invalidates the pending proposal, and the
	// same input then proposes against the new state.
	b.applied("edit", obj, `{"text":"x"}`, nil)
	if got := b.proposal(first.ID); got.Status != types.ProposalStatusInvalidated {
		t.Errorf("a proposal whose object moved is %s", got.Status)
	}
	if _, p := b.submitProposal(b.proposeCmd(b.sup, obj, op, nil)); seen[p.ID] || p.ExpectedState.Digest == nil {
		t.Errorf("the moved object's proposal: %+v", p)
	}
	b.rebuilds()
}

// The fold refuses what the Authorizer never writes: a second pending
// proposal of one operation, one dated after its event, one created by
// another than its proposer, not open, or not matching its binding digest.
// Liveness is judged at the event's recorded time: one past the pending
// one's expiry is admitted, a future-dated one never displaces it.
func TestProposalCreationGuards(t *testing.T) {
	b := openBoundary(t)
	_, p := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), b.op.Actor(), nil))
	created := func(p types.Proposal) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			raw, _ := json.Marshal(p)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProposalCreated, Subject: string(p.ID), Payload: raw}}, nil
		}
	}
	now := suiteNow().UTC()
	at := func(d time.Duration) types.Time { return types.Time(now.Add(d).Format(time.RFC3339Nano)) }
	fresh := func(change func(*types.Proposal)) types.Proposal {
		n := p
		n.ID = types.ProposalID(newID(t, "prp"))
		change(&n)
		return n
	}
	// valid is a proposal of another object, consistent with its digest
	// after change: only the guard named refuses it.
	obj := newID(t, "prj")
	valid := func(change func(*types.Proposal)) types.Proposal {
		return fresh(func(n *types.Proposal) {
			n.Object.ID, n.CreatedAt, n.ExpiresAt = obj, at(-48*time.Hour), at(-24*time.Hour)
			change(n)
			n.BindingDigest = bindingOf(t, *n)
		})
	}
	same := func(*types.Proposal) {}
	lies := valid(same)
	lies.Scope = json.RawMessage(`{"x":1}`)
	for name, c := range map[string]struct {
		by identity.Principal
		p  types.Proposal
	}{
		"a duplicate pending":   {b.sup, fresh(same)},
		"future-dated":          {b.sup, fresh(func(n *types.Proposal) { n.CreatedAt, n.ExpiresAt = at(1000*time.Hour), at(1100*time.Hour) })},
		"future-dated, free":    {b.sup, valid(func(n *types.Proposal) { n.CreatedAt, n.ExpiresAt = at(time.Hour), at(73*time.Hour) })},
		"another proposer":      {b.op, valid(same)},
		"created confirmed":     {b.sup, valid(func(n *types.Proposal) { n.Status = types.ProposalStatusConfirmed })},
		"a digest that lies":    {b.sup, lies},
		"expiring when created": {b.sup, valid(func(n *types.Proposal) { n.CreatedAt = n.ExpiresAt })},
	} {
		if err := b.try(c.by, created(c.p)); err == nil {
			t.Errorf("%s was admitted", name)
		}
	}
	// The future-dated one displaced nothing: the operation is still p's.
	if _, again := b.submitProposal(b.proposeCmd(b.sup, p.Object.ID, b.op.Actor(), nil)); again.ID != p.ID {
		t.Errorf("the pending proposal was displaced by %s", again.ID)
	}
	expired := valid(same)
	if err := b.try(b.sup, created(expired)); err != nil {
		t.Fatalf("a proposal of a free operation: %v", err)
	}
	pending := valid(func(n *types.Proposal) { n.CreatedAt, n.ExpiresAt = at(-time.Hour), at(72*time.Hour) })
	if err := b.try(b.sup, created(pending)); err != nil {
		t.Errorf("a proposal past the pending one's expiry: %v", err)
	}
	// Settling the expired one leaves the digest to the pending one.
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.InvalidateProposal(tx, expired.ID, types.ProposalInvalidatedReasonValueProposalInvalidated)
	})
	if err := b.try(b.sup, created(valid(func(n *types.Proposal) { n.CreatedAt, n.ExpiresAt = at(-time.Hour), at(72*time.Hour) }))); err == nil {
		t.Error("a duplicate of the pending proposal after the expired one settled")
	}
	b.rebuilds()
}

// bindingOf is the binding digest of p's bound fields.
func bindingOf(t *testing.T, p types.Proposal) types.Digest {
	t.Helper()
	var classes []string
	for _, c := range p.ChangeClass {
		classes = append(classes, string(c))
	}
	d, err := authority.BindingDigest(authority.Bound{Action: string(p.Action), Classes: classes, Project: p.Project, Object: p.Object,
		ExpectedState: p.ExpectedState, Scope: p.Scope, ProposedBy: p.ProposedBy, Grantee: p.Grantee})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// approve records approval of p by principal, built by ApprovalOf from
// p (change may then tamper with it).
func (b *boundary) approve(by identity.Principal, kind types.ActorKind, p types.Proposal, change func(*types.Approval)) error {
	chl, evt := types.ChallengeID(newID(b.t, "chl")), types.EventID(newID(b.t, "evt"))
	a := authority.ApprovalOf(types.Approval{ID: types.ApprovalID(newID(b.t, "apr")), Confirmation: types.ApprovalConfirmation{Challenge: &chl,
		SatisfiedBy: []types.EventID{evt}}, Answers: []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: "approved",
		At: types.Time(t0.Format(time.RFC3339))}, p, types.Actor{ID: by.Actor(), Kind: kind})
	if change != nil {
		change(&a)
	}
	return b.try(by, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.ApprovalEvent(a) })
}

// An approval answers exactly one live proposal, copies it, is never the
// proposer's or the grantee's, is an operator's under a human policy, and
// is the only one.
func TestApprovalLinkage(t *testing.T) {
	b := openBoundary(t)
	sup2 := b.supervisor()
	_, p := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), sup2.Actor(), nil))
	_, other := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), sup2.Actor(), nil))
	op, sup := types.ActorKindOperator, types.ActorKindSupervisor
	for name, c := range map[string]struct {
		by     identity.Principal
		kind   types.ActorKind
		change func(*types.Approval)
	}{
		"a missing proposal":    {b.op, op, func(a *types.Approval) { id := types.ProposalID(newID(t, "prp")); a.Proposal = &id }},
		"no proposal":           {b.op, op, func(a *types.Approval) { a.Proposal = nil }},
		"another proposal's id": {b.op, op, func(a *types.Approval) { a.Proposal = &other.ID }},
		"a supplied policy": {b.op, op, func(a *types.Approval) {
			a.Confirmation.Policy = types.ApprovalConfirmationPolicyIndependentReview
		}},
		"a supplied scope":    {b.op, op, func(a *types.Approval) { a.Scope = json.RawMessage(`{"x":1}`) }},
		"a claimed kind":      {b.sup, op, nil},
		"the proposer":        {b.sup, sup, nil},
		"the grantee":         {sup2, sup, nil},
		"another supervisor":  {b.supervisor(), sup, nil}, // a human policy is an operator's
		"another actor named": {b.op, op, func(a *types.Approval) { a.Actor = sup2.Actor() }},
	} {
		if err := b.approve(c.by, c.kind, p, c.change); err == nil {
			t.Errorf("%s: approved", name)
		}
	}
	if err := b.approve(b.op, op, p, nil); err != nil {
		t.Fatalf("the operator's approval: %v", err)
	}
	first := b.approvalOf(p.ID)
	if err := b.approve(b.op, op, p, func(a *types.Approval) { a.Reason = "approved again" }); err == nil {
		t.Error("a second approval of one proposal")
	}
	if got := b.approvalOf(p.ID); got != first {
		t.Errorf("a second approval replaced the first: %s, was %s", got, first)
	}
	// An approval id is one record: reusing it for another proposal is refused.
	var firstID types.ApprovalID
	if err := json.Unmarshal([]byte(first), &struct {
		ID *types.ApprovalID `json:"id"`
	}{&firstID}); err != nil {
		t.Fatal(err)
	}
	if err := b.approve(b.op, op, other, func(a *types.Approval) { a.ID = firstID }); err == nil {
		t.Error("an approval id reused for another proposal")
	}
	if got := b.approvalOf(p.ID); got != first {
		t.Errorf("the reused id changed the first approval: %s, was %s", got, first)
	}
	if err := b.approve(b.op, op, other, nil); err != nil {
		t.Errorf("the other proposal's own approval: %v", err)
	}
	b.rebuilds()
}

// Separation of duties under independent_review: neither the proposer nor
// the grantee approves; another live actor may (the receipt carries the
// review's authority, 04:190, checked at confirmation in 6c).
func TestSeparateUnderIndependentReview(t *testing.T) {
	sup, grantee, other := types.ActorID("act_sup"), types.ActorID("act_grantee"), types.ActorID("act_other")
	p := types.Proposal{ID: "prp_x", ProposedBy: sup, Grantee: &grantee, Policy: types.ProposalPolicyIndependentReview}
	for name, by := range map[string]types.Actor{
		"the proposer":            {ID: sup, Kind: types.ActorKindSupervisor},
		"the grantee":             {ID: grantee, Kind: types.ActorKindSupervisor},
		"the grantee as operator": {ID: grantee, Kind: types.ActorKindOperator},
	} {
		if err := authority.Separate(p, by); err == nil {
			t.Errorf("%s approved", name)
		}
	}
	if err := authority.Separate(p, types.Actor{ID: other, Kind: types.ActorKindSupervisor}); err != nil {
		t.Errorf("an independent approver: %v", err)
	}
	// KA1 (04:98): an operator's own human-policy proposal is confirmed by
	// that operator's presence, and only under a human policy.
	own := types.Proposal{ID: "prp_y", ProposedBy: "act_op", Policy: types.ProposalPolicyHuman}
	if err := authority.Separate(own, types.Actor{ID: "act_op", Kind: types.ActorKindOperator}); err != nil {
		t.Errorf("an operator's presence check of its own proposal: %v", err)
	}
	own.Policy = types.ProposalPolicyIndependentReview
	if err := authority.Separate(own, types.Actor{ID: "act_op", Kind: types.ActorKindOperator}); err == nil {
		t.Error("an operator approved its own proposal under independent review")
	}
}

// The fold invalidates a pending proposal when its grant or its proposer
// is revoked; an explicit invalidation commits with the invalidated
// disposition, and only as exactly the current record invalidated. An
// approval of an invalidated proposal is refused.
func TestProposalInvalidation(t *testing.T) {
	b := openBoundary(t)
	op := b.supervisor().Actor() // the grantee; the operator approves
	_, explicit := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), op, nil))
	forged := explicit
	forged.Status, forged.Scope = types.ProposalStatusInvalidated, json.RawMessage(`{"x":1}`)
	reason := types.ProposalInvalidatedReasonValueProposalInvalidated
	forged.InvalidatedReason = &reason
	if err := b.try(b.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		raw, _ := json.Marshal(forged)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProposalInvalidated, Subject: string(forged.ID), Payload: raw}}, nil
	}); err == nil {
		t.Error("an invalidation that changes the scope")
	}
	invalidate := func() (eventstore.Committed, error) {
		cmd := newID(t, "cmd")
		return b.s.Submit(context.Background(), b.op.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), func(tx *eventstore.Tx) (eventstore.Decision, error) {
			evs, err := authority.InvalidateProposal(tx, explicit.ID, reason)
			return eventstore.Decision{Disposition: eventstore.DispositionInvalidated, Events: evs,
				Result: json.RawMessage(`{"outcome":"failed","reason":"proposal_invalidated"}`)}, err
		})
	}
	if out, err := invalidate(); err != nil || out.Disposition != eventstore.DispositionInvalidated {
		t.Fatalf("an explicit invalidation: %+v %v", out, err)
	}
	if got := b.proposal(explicit.ID); got.Status != types.ProposalStatusInvalidated || *got.InvalidatedReason != reason {
		t.Errorf("the explicitly invalidated proposal: %+v", got)
	}
	if _, err := invalidate(); err == nil {
		t.Error("an invalidated proposal invalidated again")
	}
	if err := b.approve(b.op, types.ActorKindOperator, explicit, nil); err == nil {
		t.Error("an invalidated proposal approved")
	}
	// Released: the same operation proposes afresh.
	_, again := b.submitProposal(b.proposeCmd(b.sup, explicit.Object.ID, op, nil))
	if again.ID == explicit.ID {
		t.Error("an invalidated proposal returned as pending")
	}
	_, byGrant := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), op, nil))
	b.seed(b.op, revokeGrant(b.supGrant, t0))
	for id, want := range map[types.ProposalID]types.ProposalInvalidatedReasonValue{
		again.ID: types.ProposalInvalidatedReasonValueAuthorityRevoked, byGrant.ID: types.ProposalInvalidatedReasonValueAuthorityRevoked,
	} {
		if got := b.proposal(id); got.InvalidatedReason == nil || *got.InvalidatedReason != want {
			t.Errorf("proposal %s after its grant's revocation: %+v", id, got)
		}
	}
	b.rebuilds()
}

func TestProposerRevocationInvalidates(t *testing.T) {
	b := openBoundary(t)
	_, p := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), b.op.Actor(), nil))
	b.seed(b.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, b.sup.Actor(), suiteNow())
	})
	if got := b.proposal(p.ID); got.InvalidatedReason == nil || *got.InvalidatedReason != types.ProposalInvalidatedReasonValueActorRevoked {
		t.Errorf("a revoked proposer's proposal: %+v", got)
	}
	b.rebuilds()
}

// A handler that derives no object type cannot propose: a proposal binds it.
func TestProposalNeedsAnObjectType(t *testing.T) {
	b := openBoundary(t)
	b.az.Handlers = map[string]authority.Handler{"note.add": untyped{}}
	b.refusedUnrun("an untyped proposal", b.proposeCmd(b.sup, newID(t, "prj"), b.op.Actor(), nil), func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "no object type")
	})
}

type untyped struct{}

func (untyped) Derive(tx *eventstore.Tx, a types.Actor, in json.RawMessage) (authority.Derived, error) {
	d, err := (&noteHandler{}).Derive(tx, a, in)
	d.ObjectType = ""
	return d, err
}

// The fold's separation of duties under independent_review (02:45;
// 04:45, :96, :190): the proposer and the grantee are refused, another
// live supervisor is admitted.
func TestApprovalSeparationInTheFold(t *testing.T) {
	b := openBoundary(t)
	grantee, other := b.supervisor(), b.supervisor()
	review := map[string]any{"classes": []string{"knowledge.accept_claim"}}
	_, p := b.submitProposal(b.proposeCmd(b.sup, newID(t, "prj"), grantee.Actor(), review))
	if p.Policy != types.ProposalPolicyIndependentReview {
		t.Fatalf("the proposal's policy is %s", p.Policy)
	}
	sup := types.ActorKindSupervisor
	if err := b.approve(b.sup, sup, p, nil); err == nil {
		t.Error("the proposer approved its own proposal")
	}
	if err := b.approve(grantee, sup, p, nil); err == nil {
		t.Error("the grantee approved under independent review")
	}
	if err := b.approve(b.op, sup, p, func(a *types.Approval) { a.Actor = other.Actor() }); err == nil {
		t.Error("an approval recorded by one actor in another's name")
	}
	if err := b.approve(other, sup, p, nil); err != nil {
		t.Errorf("an independent approver: %v", err)
	}
	b.rebuilds()
}

// approvalOf is the recorded approval answering proposal id.
func (b *boundary) approvalOf(id types.ProposalID) (rec string) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, ok, err := tx.Get(authority.ProposalProjection, "approval/"+string(id))
		if err == nil && !ok {
			err = errors.New("no approval of " + string(id))
		}
		rec = string(raw)
		return err
	})
	return rec
}
