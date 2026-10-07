// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 6d: proposal.withdraw.

// openWithdraw is openConfirm with proposal.withdraw wired, and granted
// to the supervisor on project A by k.withdrawGrant (beside k.grant, which
// its proposals stay under).
func openWithdraw(t *testing.T) (*confirmKit, types.Grant) { return openWithdrawAt(t, 0) }

// openWithdrawAt: with the store clock starting at start from the real time.
func openWithdrawAt(t *testing.T, start time.Duration) (*confirmKit, types.Grant) {
	k := openConfirmAt(t, start)
	k.az.Handlers[authority.WithdrawAction] = authority.WithdrawHandler{}
	g := k.grant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.WithdrawAction}
	k.seed(k.op, createGrant(g))
	return k, g
}

func (k *confirmKit) withdrawCmd(by identity.Principal, p types.ProposalID) eventstore.Command {
	key := newID(k.t, "cmd")
	return by.Command(key, authority.WithdrawAction, key, []byte(`{"proposal":"`+string(p)+`"}`), nil)
}

func (k *confirmKit) withdraw(c eventstore.Command) (eventstore.Committed, error) {
	return k.s.Submit(context.Background(), c, k.az.Withdraw(c))
}

// withdrawn: c withdraws p, applied, as one proposal.withdrawn event in
// p's project, its result naming p and its new status.
func (k *confirmKit) withdrawn(name string, c eventstore.Command, p types.ProposalID) eventstore.Committed {
	k.t.Helper()
	h := k.head()
	out, err := k.withdraw(c)
	if err != nil || out.Disposition != eventstore.DispositionApplied || out.FirstSeq != h+1 || out.LastSeq != h+1 {
		k.t.Fatalf("%s: %+v %v", name, out, err)
	}
	if want := `{"proposal":"` + string(p) + `","status":"withdrawn"}`; string(out.Result) != want {
		k.t.Errorf("%s: the result is %s, want %s", name, out.Result, want)
	}
	project := k.proposal(p).Project
	k.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(h, 2)
		if err == nil && (len(evs) != 1 || evs[0].Kind != authority.ProposalWithdrawn || evs[0].Subject != string(p) ||
			project == nil || evs[0].Project == nil || *evs[0].Project != string(*project)) {
			k.t.Errorf("%s committed %+v", name, evs)
		}
		return err
	})
	if got := k.proposal(p); got.Status != types.ProposalStatusWithdrawn {
		k.t.Errorf("%s: the proposal is %s", name, got.Status)
	}
	return out
}

// refusedWithdraw: c is refused, nothing committed, p as it was.
func (k *confirmKit) refusedWithdraw(name string, c eventstore.Command, p types.ProposalID, check func(error) bool) {
	k.t.Helper()
	h, before := k.head(), k.proposal(p)
	if _, err := k.withdraw(c); !check(err) {
		k.t.Errorf("%s: %v", name, err)
	}
	if k.head() != h || k.proposal(p).Status != before.Status {
		k.t.Errorf("%s: committed", name)
	}
}

// pending: the pending indexes still name p.
func (k *confirmKit) pending(p types.Proposal) (named bool) {
	k.view(func(tx *eventstore.Tx) error {
		for _, prefix := range []string{"digest/" + string(p.BindingDigest), "proposer/" + string(p.ProposedBy) + "/", "object/" + p.Object.ID + "/", "grant/"} {
			keys, err := tx.Keys(authority.PendingProjection, prefix)
			for _, key := range keys {
				raw, _, _ := tx.Get(authority.PendingProjection, key)
				named = named || string(raw) == `"`+string(p.ID)+`"`
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return named
}

// The proposer withdraws its open proposal, without the seat: it leaves
// the pending indexes, a replay commits nothing, a second withdrawal is
// refused, and the same exact operation can be proposed afresh.
func TestWithdrawByProposer(t *testing.T) {
	k, _ := openWithdraw(t)
	obj := newID(t, "prj")
	_, p := k.submitProposal(k.proposeCmd(k.sup, obj, k.op.Actor(), accept))
	if !k.pending(p) {
		t.Fatal("the proposal is not pending")
	}
	sup := k.sup.Actor()
	k.seed(k.sup, seat(types.SeatEventTransitionSeatReleased, &sup, nil, nil))
	c := k.withdrawCmd(k.sup, p.ID)
	out := k.withdrawn("the proposer", c, p.ID)
	if k.pending(p) {
		t.Error("the withdrawn proposal is still pending")
	}
	h := k.head()
	if again, err := k.withdraw(c); err != nil || !again.Replayed || string(again.Result) != string(out.Result) || k.head() != h {
		t.Errorf("a replay: %+v %v", again, err)
	}
	k.refusedWithdraw("withdrawn", k.withdrawCmd(k.sup, p.ID), p.ID, isReason(vocab.ReasonProposalNotOpen))
	k.seed(k.sup, seat(types.SeatEventTransitionSeatTaken, nil, &sup, nil))
	if _, fresh := k.submitProposal(k.proposeCmd(k.sup, obj, k.op.Actor(), accept)); fresh.ID == p.ID || fresh.BindingDigest != p.BindingDigest ||
		fresh.Status != types.ProposalStatusOpen {
		t.Errorf("the same operation after the withdrawal: %+v", fresh)
	}
	k.rebuilds()
}

// An operator withdraws another's proposal; another supervisor, the
// system, and a proposer without a grant for proposal.withdraw may not.
func TestWithdrawWho(t *testing.T) {
	k, g := openWithdraw(t)
	p := k.propose(k.op.Actor())
	other := k.supervisor()
	og := g
	og.ID, og.Actor = types.GrantID(newID(t, "grt")), other.Actor()
	k.seed(k.op, createGrant(og))
	notGranted := isReason(vocab.ReasonActionNotGranted)
	k.refusedWithdraw("another supervisor", k.withdrawCmd(other, p.ID), p.ID, notGranted)
	k.refusedWithdraw("the system", k.withdrawCmd(k.sys, p.ID), p.ID, notGranted)
	k.seed(k.op, revokeGrant(g, k.now))
	k.refusedWithdraw("a proposer without the grant", k.withdrawCmd(k.sup, p.ID), p.ID, notGranted)
	k.withdrawn("an operator", k.withdrawCmd(k.op, p.ID), p.ID)
	// The logic withdraws only under its own action, and only a scope its
	// schema admits.
	q := k.propose(k.op.Actor())
	seed := k.withdrawCmd(k.op, q.ID)
	seed.Action = "test.seed"
	if _, err := k.withdraw(seed); err == nil || k.proposal(q.ID).Status != types.ProposalStatusOpen {
		t.Errorf("the withdrawal logic ran for another action: %v", err)
	}
	// The logic rechecks who withdraws on its own: run without the
	// Authorizer, it refuses another supervisor's withdrawal itself.
	k.view(func(tx *eventstore.Tx) error {
		if out, err := k.az.Withdraw(k.withdrawCmd(other, q.ID))(tx); !isReason(vocab.ReasonActionNotGranted)(err) {
			t.Errorf("the withdrawal logic alone let another supervisor withdraw: %+v %v", out, err)
		}
		return nil
	})
	k.refusedWithdraw("an unknown proposal", k.withdrawCmd(k.op, types.ProposalID(newID(t, "prp"))), q.ID, isReason(vocab.ReasonProposalNotOpen))
	for name, in := range map[string]string{"no proposal": `{}`, "another field": `{"proposal":"` + string(q.ID) + `","x":1}`, "not a proposal": `{"proposal":"` + newID(t, "grt") + `"}`} {
		key := newID(t, "cmd")
		k.refusedWithdraw(name, k.op.Command(key, authority.WithdrawAction, key, []byte(in), nil), q.ID, func(err error) bool { return err != nil })
	}
	k.rebuilds()
}

// A confirmed, applied, invalidated, expired or withdrawn proposal is not
// withdrawn; an open one past its expiry is closed as expired (refused).
func TestWithdrawOnlyOpen(t *testing.T) {
	k, _ := openWithdrawAt(t, -authority.ProposalTTL-time.Minute)
	old := k.propose(k.op.Actor())
	k.advance(k.base)
	notOpen := isReason(vocab.ReasonProposalNotOpen)
	applied := k.propose(k.op.Actor())
	k.committed(k.confirmCmd(k.sys, applied.ID, k.review(applied, "claude")), eventstore.DispositionApplied)
	k.refusedWithdraw("applied", k.withdrawCmd(k.sup, applied.ID), applied.ID, notOpen)
	invalid := k.propose(k.op.Actor())
	k.seed(k.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: "test.noted/v1", Subject: invalid.Object.ID, Payload: json.RawMessage(`{"x":1}`)}}, nil
	})
	k.refusedWithdraw("invalidated", k.withdrawCmd(k.sup, invalid.ID), invalid.ID, notOpen)
	confirmed := k.propose(k.op.Actor())
	if err := k.approve(k.sys, types.ActorKindSystem, confirmed, nil); err != nil {
		t.Fatal(err)
	}
	k.seed(k.op, transition(t, confirmed, authority.ProposalConfirmed, types.ProposalStatusConfirmed))
	k.refusedWithdraw("confirmed", k.withdrawCmd(k.sup, confirmed.ID), confirmed.ID, notOpen)
	// Only who may withdraw it gets as far as recording its expiry.
	k.refusedWithdraw("another's, past its expiry", k.withdrawCmd(k.sys, old.ID), old.ID, isReason(vocab.ReasonActionNotGranted))
	if out, err := k.withdraw(k.withdrawCmd(k.sup, old.ID)); err != nil || out.Disposition != eventstore.DispositionRefused || k.proposal(old.ID).Status != types.ProposalStatusExpired {
		t.Errorf("past its expiry: %+v %v", out, err)
	}
	k.refusedWithdraw("expired", k.withdrawCmd(k.sup, old.ID), old.ID, notOpen)
	k.rebuilds()
}

// transition is proposal p's event of kind, moved to status.
func transition(t *testing.T, p types.Proposal, kind string, status types.ProposalStatus) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		n := p
		n.Status = status
		raw, err := json.Marshal(n)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: kind, Subject: string(p.ID), Payload: raw}}, err
	}
}

// The fold admits a withdrawal only by the proposer or an operator, of the
// exact open record, before its expiry.
func TestWithdrawalFoldGuards(t *testing.T) {
	k, _ := openWithdrawAt(t, -authority.ProposalTTL-time.Minute)
	old := k.propose(k.op.Actor())
	k.advance(k.base)
	p := k.propose(k.op.Actor())
	applied := k.propose(k.op.Actor())
	k.committed(k.confirmCmd(k.sys, applied.ID, k.review(applied, "claude")), eventstore.DispositionApplied)
	q := k.propose(k.op.Actor())
	withdrawn := types.ProposalStatusWithdrawn
	other := k.supervisor()
	op2 := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "second operator", CreatedBy: k.op.Actor()}
	var tok identity.Secret
	k.seed(k.op, create(op2))
	k.seed(k.op, issueTo(op2.ID, k.op.Actor(), &tok))
	revoked := k.login(tok)
	rescoped, unknown := p, p
	rescoped.Scope, unknown.ID = json.RawMessage(`{"x":1}`), types.ProposalID(newID(t, "prp"))
	for name, try := range map[string]func() error{
		"by another supervisor": func() error { return k.try(other, transition(t, p, authority.ProposalWithdrawn, withdrawn)) },
		"by an operator revoked before it": func() error {
			return k.try(revoked, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				evs, err := identity.RevokeActor(tx, op2.ID, k.now)
				w, err2 := transition(t, p, authority.ProposalWithdrawn, withdrawn)(tx)
				return append(evs, w...), errors.Join(err, err2)
			})
		},
		"by the system":   func() error { return k.try(k.sys, transition(t, p, authority.ProposalWithdrawn, withdrawn)) },
		"past its expiry": func() error { return k.try(k.sup, transition(t, old, authority.ProposalWithdrawn, withdrawn)) },
		"as another status": func() error {
			return k.try(k.sup, transition(t, p, authority.ProposalWithdrawn, types.ProposalStatusExpired))
		},
		"of an applied one": func() error {
			return k.try(k.sup, transition(t, k.proposal(applied.ID), authority.ProposalWithdrawn, withdrawn))
		},
		"under another subject": func() error {
			return k.try(k.sup, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
				evs, err := transition(t, p, authority.ProposalWithdrawn, withdrawn)(tx)
				evs[0].Subject = string(q.ID)
				return evs, err
			})
		},
		"with another scope":     func() error { return k.try(k.sup, transition(t, rescoped, authority.ProposalWithdrawn, withdrawn)) },
		"of an unknown proposal": func() error { return k.try(k.sup, transition(t, unknown, authority.ProposalWithdrawn, withdrawn)) },
	} {
		if err := try(); err == nil {
			t.Errorf("a withdrawal %s was admitted", name)
		}
	}
	if got, other := k.proposal(p.ID), k.proposal(q.ID); got.Status != types.ProposalStatusOpen || other.Status != types.ProposalStatusOpen || !k.pending(p) {
		t.Errorf("the proposals moved to %s and %s", got.Status, other.Status)
	}
	if err := k.try(k.sup, transition(t, p, authority.ProposalWithdrawn, withdrawn)); err != nil || k.pending(p) {
		t.Errorf("the proposer's withdrawal: %v", err)
	}
	k.rebuilds()
}

// An expectation on the proposal (--expect) is checked against its
// version and record.
func TestWithdrawExpectation(t *testing.T) {
	k, _ := openWithdraw(t)
	p := k.propose(k.op.Actor())
	var v int64
	k.view(func(tx *eventstore.Tx) (err error) {
		v, err = tx.SubjectVersion(string(p.ID))
		return err
	})
	raw, _ := json.Marshal(k.proposal(p.ID))
	for name, e := range map[string]*eventstore.State{"a stale version": version(v + 1), "another record": digestOf(contentDigest(t, `{"x":1}`))} {
		c := k.withdrawCmd(k.sup, p.ID)
		c.Expect = e
		k.refusedWithdraw(name, c, p.ID, is(eventstore.ErrStaleExpectedState))
	}
	c := k.withdrawCmd(k.sup, p.ID)
	c.Expect = both(v, contentDigest(t, string(raw)))
	k.withdrawn("the current state", c, p.ID)
}

// A grant that tightens proposal.withdraw to a confirmation never makes it
// a proposal: a stranger is refused before Decide, the proposer after it,
// and nothing is recorded.
func TestWithdrawNeverProposed(t *testing.T) {
	k, g := openWithdraw(t)
	p := k.propose(k.op.Actor())
	tight := g
	tight.ID, tight.ConfirmationOverrides = types.GrantID(newID(t, "grt")), map[string]types.GrantConfirmationOverridesValue{authority.WithdrawAction: "independent_review"}
	k.seed(k.op, createGrant(tight))
	other := k.supervisor()
	og := tight
	og.ID, og.Actor = types.GrantID(newID(t, "grt")), other.Actor()
	k.seed(k.op, createGrant(og))
	k.refusedWithdraw("a stranger under a tightening grant", k.withdrawCmd(other, p.ID), p.ID, isReason(vocab.ReasonActionNotGranted))
	k.refusedWithdraw("the proposer under a tightening grant", k.withdrawCmd(k.sup, p.ID), p.ID, isReason(vocab.ReasonActionNotProposable))
	k.withdrawn("an operator", k.withdrawCmd(k.op, p.ID), p.ID)
	k.rebuilds()
}

// The expiry boundary, to the nanosecond: at its expires_at a proposal is
// no longer withdrawn, by the logic (closed as expired) or by the fold.
func TestWithdrawAtExpiry(t *testing.T) {
	k, _ := openWithdraw(t)
	t.Cleanup(func() { storeAt = time.Time{} })
	at := suiteNow().Add(time.Minute).UTC()
	storeAt = at
	var ps [4]types.Proposal
	for i := range ps {
		if ps[i] = k.propose(k.op.Actor()); string(ps[i].ExpiresAt) != at.Add(authority.ProposalTTL).Format(time.RFC3339Nano) {
			t.Fatalf("the proposal expires at %s", ps[i].ExpiresAt)
		}
	}
	storeAt = at.Add(authority.ProposalTTL - time.Nanosecond)
	k.withdrawn("1 ns before its expiry", k.withdrawCmd(k.op, ps[0].ID), ps[0].ID)
	if err := k.try(k.op, transition(t, ps[1], authority.ProposalWithdrawn, types.ProposalStatusWithdrawn)); err != nil {
		t.Errorf("the fold, 1 ns before its expiry: %v", err)
	}
	storeAt = at.Add(authority.ProposalTTL)
	if out, err := k.withdraw(k.withdrawCmd(k.op, ps[2].ID)); err != nil || out.Disposition != eventstore.DispositionRefused || k.proposal(ps[2].ID).Status != types.ProposalStatusExpired {
		t.Errorf("the logic at its expiry: %+v %v", out, err)
	}
	if err := k.try(k.op, transition(t, ps[3], authority.ProposalWithdrawn, types.ProposalStatusWithdrawn)); err == nil || k.proposal(ps[3].ID).Status != types.ProposalStatusOpen {
		t.Errorf("the fold admitted a withdrawal at its expiry: %v", err)
	}
	k.rebuilds()
}
