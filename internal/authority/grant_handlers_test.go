// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 7c: grant.create and grant.revoke through the real Authorizer and
// writer.

// grantKit is a boundary with the grant handlers wired, a second
// supervisor (the grantee) and the seated supervisor granted grant.create
// on prjA under creator, which the operator created through grant.create.
type grantKit struct {
	*boundary
	grantee identity.Principal
	creator types.GrantID
}

func openGrantKit(t *testing.T) *grantKit {
	b := openBoundary(t)
	b.az.Handlers[authority.GrantCreateAction] = authority.GrantCreateHandler{}
	b.az.Handlers[authority.GrantRevokeAction] = authority.GrantRevokeHandler{}
	b.az.Handlers[authority.GrantExtendAction] = authority.GrantExtendHandler{}
	b.az.Handlers[authority.ConfirmAction] = authority.ConfirmHandler{}
	k := &grantKit{boundary: b, grantee: b.supervisor(), creator: types.GrantID(newID(t, "grt"))}
	k.mustCreate(b.op, k.scope(k.creator, b.sup.Actor(), `["`+string(prjA)+`"]`, `["grant.create"]`))
	return k
}

// scope is a grant.create scope: id for actor on projects, with actions,
// reading the same projects, expiring in an hour.
func (k *grantKit) scope(id types.GrantID, actor types.ActorID, projects, actions string) string {
	return `{"id":"` + string(id) + `","actor":"` + string(actor) + `","projects":` + projects + `,"actions":` + actions +
		`,"reads":{"projects":"same","kinds":["grant"]},"expires_at":"` + suiteNow().Add(time.Hour).UTC().Format(time.RFC3339) +
		`","confirmation_overrides":{}}`
}

// scopeUntil is an every-project grant.create scope for actor expiring at until.
func (k *grantKit) scopeUntil(id types.GrantID, actor types.ActorID, until time.Time) string {
	return strings.Replace(k.scope(id, actor, `"*"`, `[]`), suiteNow().Add(time.Hour).UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339Nano), 1)
}

// submit sends action's command under key, with input and expect, through
// the wired handler's logic.
func (k *grantKit) submit(p identity.Principal, action, key, input string, expect *eventstore.State) (eventstore.Committed, error) {
	c := p.Command(newID(k.t, "cmd"), action, key, []byte(input), nil)
	c.Expect = expect
	logic := map[string]func(eventstore.Command) eventstore.Decide{
		authority.GrantCreateAction: authority.GrantCreateHandler{}.Logic, authority.GrantRevokeAction: authority.GrantRevokeHandler{}.Logic,
		authority.GrantExtendAction: authority.GrantExtendHandler{}.Logic}
	return k.s.Submit(context.Background(), c, logic[action](c))
}

func (k *grantKit) mustCreate(p identity.Principal, scope string) eventstore.Committed {
	k.t.Helper()
	out, err := k.submit(p, authority.GrantCreateAction, newID(k.t, "cmd"), scope, nil)
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		k.t.Fatalf("grant.create: %+v %v", out, err)
	}
	return out
}

// none: the command fails as check accepts and nothing is appended.
func (k *grantKit) none(name string, p identity.Principal, action, input string, expect *eventstore.State, check func(error) bool) {
	k.t.Helper()
	h := k.head()
	if out, err := k.submit(p, action, newID(k.t, "cmd"), input, expect); !check(err) || out.CommandID != "" || k.head() != h {
		k.t.Errorf("%s: %+v %v", name, out, err)
	}
}

func (k *grantKit) grant(id types.GrantID) (e types.GrantEvent) {
	k.t.Helper()
	k.view(func(tx *eventstore.Tx) error {
		raw, ok, err := authority.GrantRecord(id)(tx)
		if err == nil && !ok {
			err = errors.New("no grant " + string(id))
		}
		return errors.Join(err, json.Unmarshal(raw, &e))
	})
	return e
}

// detail is a refusal's detail ("" for any other error).
func detail(err error) string {
	var r *authority.Refusal
	if errors.As(err, &r) {
		return r.Detail
	}
	return ""
}

// An operator's grant.create applies at once: valid from the command's
// recorded time, issued by the operator, with no approval; it replays, and
// its id is never created again, for any grantee.
func TestOperatorCreatesAGrant(t *testing.T) {
	k := openGrantKit(t)
	id := types.GrantID(newID(t, "grt"))
	scope := k.scope(id, k.grantee.Actor(), `["`+string(prjA)+`"]`, `["note.add"]`)
	out, err := k.submit(k.op, authority.GrantCreateAction, "create", scope, version(0))
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("an operator's grant: %+v %v", out, err)
	}
	g := k.grant(id).Grant
	if g.Actor != k.grantee.Actor() || g.IssuedBy != k.op.Actor() || g.Approval != nil || string(g.ValidFrom) != k.recorded[string(id)].At ||
		!slices.Equal(g.Actions, []types.ActionKind{"note.add"}) {
		t.Errorf("the grant: %+v, event at %s", g, k.recorded[string(id)].At)
	}
	if again, err := k.submit(k.op, authority.GrantCreateAction, "create", scope, version(0)); err != nil || !again.Replayed || again.LastSeq != out.LastSeq {
		t.Errorf("the replay: %+v %v", again, err)
	}
	unissuable := isReason(vocab.ReasonGrantNotIssuable)
	k.none("the same id again", k.op, authority.GrantCreateAction, scope, nil, unissuable)
	k.none("the same id for another grantee", k.op, authority.GrantCreateAction, k.scope(id, k.sup.Actor(), `"*"`, `[]`), nil, unissuable)
	k.none("a grant for an operator", k.op, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), k.op.Actor(), `"*"`, `[]`), nil, unissuable)
	k.none("a grant for an unknown actor", k.op, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), types.ActorID(newID(t, "act")), `"*"`, `[]`), nil, unissuable)
	k.none("an expired grant", k.op, authority.GrantCreateAction, k.scopeUntil(types.GrantID(newID(t, "grt")), k.grantee.Actor(), suiteNow().Add(-time.Second)), nil, unissuable)
	// The fold, too, refuses the id for another grantee.
	dup := g
	dup.Actor = k.sup.Actor()
	if k.try(k.op, createGrant(dup)) == nil {
		t.Error("the fold admitted a grant id twice")
	}
	// Every grant is valid from the command that issues it.
	early := g
	early.ID = types.GrantID(newID(t, "grt"))
	if k.try(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := authority.CreateGrant(tx, early)
		if err == nil {
			evs[0].Payload, err = mutated(evs[0].Payload, func(g *types.Grant) {
				g.ValidFrom = types.Time(suiteNow().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
			})
		}
		return evs, err
	}) == nil {
		t.Error("the fold admitted a grant valid from another time")
	}
	revoked := k.supervisor().Actor()
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, revoked, suiteNow())
	})
	k.none("a grant for a revoked supervisor", k.op, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), revoked, `"*"`, `[]`), nil, unissuable)
	k.rebuilds()
}

// The scopes are checked against their schemas before the derivation:
// valid_from, issued_by and approval are never the caller's.
func TestGrantScopeSchemas(t *testing.T) {
	k := openGrantKit(t)
	isSchema := func(err error) bool { var e *contract.Error; return errors.As(err, &e) }
	base := k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `"*"`, `[]`)
	with := func(field, value string) string {
		return strings.TrimSuffix(base, "}") + `,"` + field + `":` + value + `}`
	}
	for name, in := range map[string]string{
		"a valid_from":  with("valid_from", `"2026-10-07T00:00:00Z"`),
		"an issuer":     with("issued_by", `"`+string(k.op.Actor())+`"`),
		"an approval":   with("approval", `"`+newID(t, "apr")+`"`),
		"no expiry":     strings.Replace(base, `"expires_at"`, `"expires"`, 1),
		"a weak policy": strings.Replace(base, `"confirmation_overrides":{}`, `"confirmation_overrides":{"note.add":"none"}`, 1),
	} {
		k.none(name, k.op, authority.GrantCreateAction, in, nil, isSchema)
	}
	// The derivation itself refuses an invalid scope, before any logic.
	k.view(func(tx *eventstore.Tx) error {
		if _, err := (authority.GrantCreateHandler{}).Derive(tx, types.Actor{}, json.RawMessage(with("issued_by", `"`+string(k.op.Actor())+`"`))); !isSchema(err) {
			t.Errorf("the derivation took an issuer: %v", err)
		}
		return nil
	})
	k.none("a revocation naming its grantee", k.op, authority.GrantRevokeAction, `{"grant":"`+string(k.creator)+`","actor":"`+string(k.sup.Actor())+`"}`, nil, isSchema)
}

// A supervisor's grant.create is a human-policy proposal under its own
// grant, never applied by a review (14 R6); its own grant is refused; its
// grant must cover the one project the new grant names (every project when
// it names several); and it needs the seat.
func TestSupervisorGrantCreateIsProposed(t *testing.T) {
	k := openGrantKit(t)
	id := types.GrantID(newID(t, "grt"))
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "propose",
		[]byte(k.scope(id, k.grantee.Actor(), `["`+string(prjA)+`"]`, `["note.add"]`)), nil))
	if p.Policy != types.ProposalPolicyHuman || *p.Grantee != k.grantee.Actor() || *p.Grant != k.creator || *p.Project != prjA ||
		p.Object.Type != authority.GrantObjectType || p.Object.ID != string(id) || !slices.Equal(p.ChangeClass, []types.ChangeClass{"grant.create"}) {
		t.Errorf("the proposal: %+v", p)
	}
	k.view(func(tx *eventstore.Tx) error {
		if _, ok, err := authority.GrantRecord(id)(tx); ok || err != nil {
			t.Errorf("the proposed grant exists: %v", err)
		}
		return nil
	})
	key := newID(t, "cmd")
	confirm := k.sys.Command(key, authority.ConfirmAction, key, []byte(`{"proposal":"`+string(p.ID)+`","review":"`+newID(t, "rcp")+`"}`), nil)
	if _, err := k.s.Submit(context.Background(), confirm, k.az.Confirm(confirm)); !isReason(vocab.ReasonConfirmationRefused)(err) {
		t.Errorf("a review confirming a grant: %v", err)
	}
	other := types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	k.none("its own grant", k.sup, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), `["`+string(prjA)+`"]`, `[]`), nil,
		isReason(vocab.ReasonSelfGrantRefused))
	k.none("two projects", k.sup, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `["`+string(prjA)+`","`+string(other)+`"]`, `[]`), nil,
		isReason(vocab.ReasonActionNotGranted))
	reads := strings.Replace(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`), `"same"`, `["`+string(other)+`"]`, 1)
	k.none("reads in another project", k.sup, authority.GrantCreateAction, reads, nil, isReason(vocab.ReasonActionNotGranted))
	// No existence oracle: a supervisor without the grant is refused its
	// authorization whatever the id or grantee; an operator learns why a
	// grant cannot be issued.
	taken := k.scope(k.creator, k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`)
	unknown := k.scope(types.GrantID(newID(t, "grt")), types.ActorID(newID(t, "act")), `["`+string(prjA)+`"]`, `[]`)
	ungranted := k.supervisor()
	k.none("the system's grant", k.sys, authority.GrantCreateAction, taken, nil, isReason(vocab.ReasonOperatorOnly))
	var details []string
	for _, in := range []string{taken, unknown} {
		k.none("an ungranted supervisor's grant", ungranted, authority.GrantCreateAction, in, nil, isReason(vocab.ReasonActionNotGranted))
		_, err := k.submit(k.op, authority.GrantCreateAction, newID(t, "cmd"), in, nil)
		if !isReason(vocab.ReasonGrantNotIssuable)(err) {
			t.Errorf("an operator's unissuable grant: %v", err)
		}
		details = append(details, detail(err))
	}
	if !strings.Contains(details[0], "taken") || !strings.Contains(details[1], "not a live supervisor") {
		t.Errorf("the operator's details: %q", details)
	}
	k.mustCreate(k.op, k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `"*"`, `["grant.create"]`))
	k.none("without the seat", k.grantee, authority.GrantCreateAction, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), `"*"`, `[]`), nil,
		isReason(vocab.ReasonSeatNotHeld))
	k.rebuilds()
}

// A confirmed grant proposal is applied for its proposer, revalidated,
// and issued by its confirmer under the approval the same command records;
// the fold admits no grant naming an approval that is not its own.
func TestConfirmedGrantNamesItsApproval(t *testing.T) {
	k := openGrantKit(t)
	id := types.GrantID(newID(t, "grt"))
	scope := k.scope(id, k.grantee.Actor(), `["`+string(prjA)+`"]`, `["note.add"]`)
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "propose", []byte(scope), nil))
	key, chl, evt := newID(t, "cmd"), types.ChallengeID(newID(t, "chl")), types.EventID(newID(t, "evt"))
	confirm := k.op.Command(key, authority.ConfirmAction, key, []byte(`{"proposal":"`+string(p.ID)+`","review":"`+newID(t, "rcp")+`"}`), nil)
	h := k.head()
	out, err := k.s.Submit(context.Background(), confirm, k.az.ApplyHuman(confirm, p.ID, chl, evt))
	var res struct {
		Approval types.ApprovalID `json:"approval"`
	}
	if err != nil || out.Disposition != eventstore.DispositionApplied || json.Unmarshal(out.Result, &res) != nil {
		t.Fatalf("the confirmed grant: %+v %v", out, err)
	}
	var kinds []string
	k.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(h, 10)
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		return err
	})
	if !slices.Equal(kinds, []string{authority.ApprovalRecorded, authority.ProposalConfirmed, "grant.created/v1", authority.ProposalApplied}) {
		t.Errorf("the confirmation committed %v", kinds)
	}
	g := k.grant(id).Grant
	if g.Approval == nil || *g.Approval != res.Approval || g.IssuedBy != k.op.Actor() || g.Actor != k.grantee.Actor() || string(g.ValidFrom) != k.recorded[string(id)].At {
		t.Errorf("the grant: %+v, approval %s", g, res.Approval)
	}
	if k.proposal(p.ID).Status != types.ProposalStatusApplied {
		t.Errorf("the proposal is %s", k.proposal(p.ID).Status)
	}
	// The fold: an approval not recorded, or recorded for another grant.
	forged := func(name string, mut func(*types.Grant)) {
		t.Helper()
		f := g
		f.ID = types.GrantID(newID(t, "grt"))
		mut(&f)
		if k.try(k.op, createGrant(f)) == nil {
			t.Errorf("a grant with %s was admitted", name)
		}
	}
	bogus := types.ApprovalID(newID(t, "apr"))
	forged("an unrecorded approval", func(f *types.Grant) { f.Approval = &bogus })
	forged("another grant's approval", func(f *types.Grant) {})
	k.rebuilds()
}

// grant.revoke: an operator's alone, at once; the proposals made under the
// grant are invalidated and a read scope held under it no longer holds.
func TestGrantRevoke(t *testing.T) {
	k := openGrantKit(t)
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "propose",
		[]byte(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`)), nil))
	// Even a grant naming grant.revoke does not make a supervisor a revoker.
	k.mustCreate(k.op, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), `"*"`, `["grant.revoke"]`))
	var scope authority.ReadScope
	k.view(func(tx *eventstore.Tx) (err error) {
		var a types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(k.sup.Actor()))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		if err == nil {
			scope, err = authority.ReadScopeOf(tx, a, tx.Now())
		}
		return err
	})
	in := `{"grant":"` + string(k.creator) + `"}`
	k.none("a supervisor's revocation", k.sup, authority.GrantRevokeAction, in, nil, isReason(vocab.ReasonOperatorOnly))
	k.none("an unknown grant", k.op, authority.GrantRevokeAction, `{"grant":"`+newID(t, "grt")+`"}`, nil, is(authority.ErrNoGrant))
	out, err := k.submit(k.op, authority.GrantRevokeAction, "revoke", in, nil)
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the revocation: %+v %v", out, err)
	}
	e := k.grant(k.creator)
	if e.Transition != types.GrantEventTransitionGrantRevoked || e.RevokedAt == nil || string(*e.RevokedAt) != k.recorded[string(k.creator)].At {
		t.Errorf("the revoked grant: %+v", e)
	}
	if q := k.proposal(p.ID); q.Status != types.ProposalStatusInvalidated || *q.InvalidatedReason != types.ProposalInvalidatedReasonValueAuthorityRevoked {
		t.Errorf("the proposal under the revoked grant: %+v", q)
	}
	k.view(func(tx *eventstore.Tx) error {
		if holds, err := scope.Holds(tx, tx.Now()); holds || err != nil {
			t.Errorf("the read scope under the revoked grant holds: %v", err)
		}
		return nil
	})
	if again, err := k.submit(k.op, authority.GrantRevokeAction, "revoke", in, nil); err != nil || !again.Replayed || again.LastSeq != out.LastSeq {
		t.Errorf("the replay: %+v %v", again, err)
	}
	k.none("a second revocation", k.op, authority.GrantRevokeAction, in, nil, is(authority.ErrNoGrant))
	k.rebuilds()
}

// --expect on a grant: version 0 before it exists, then its version and
// its record's digest.
func TestGrantExpect(t *testing.T) {
	k := openGrantKit(t)
	id := types.GrantID(newID(t, "grt"))
	scope := k.scope(id, k.grantee.Actor(), `"*"`, `[]`)
	isStale := func(err error) bool { return errors.Is(err, eventstore.ErrStaleExpectedState) }
	k.none("a grant expected to exist", k.op, authority.GrantCreateAction, scope, version(1), isStale)
	out := k.mustCreate(k.op, scope)
	k.none("version 0 of an existing grant", k.op, authority.GrantCreateAction, scope, version(0), isReason(vocab.ReasonGrantNotIssuable))
	var dg string
	k.view(func(tx *eventstore.Tx) error {
		raw, _, err := authority.GrantRecord(id)(tx)
		if err == nil {
			dg, err = eventstore.ContentDigest(raw)
		}
		return err
	})
	in := `{"grant":"` + string(id) + `"}`
	k.none("an old version", k.op, authority.GrantRevokeAction, in, version(out.LastSeq-1), isStale)
	k.none("another digest", k.op, authority.GrantRevokeAction, in, digestOf(contentDigest(t, `{"x":1}`)), isStale)
	if out, err := k.submit(k.op, authority.GrantRevokeAction, "revoke", in, both(out.LastSeq, dg)); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("a revocation at the inspected grant: %+v %v", out, err)
	}
	k.rebuilds()
}

// confirmHuman applies p through the human path as the operator.
func (k *grantKit) confirmHuman(p types.ProposalID) (eventstore.Committed, error) {
	key := newID(k.t, "cmd")
	c := k.op.Command(key, authority.ConfirmAction, key, []byte(`{"proposal":"`+string(p)+`","review":"`+newID(k.t, "rcp")+`"}`), nil)
	return k.s.Submit(context.Background(), c, k.az.ApplyHuman(c, p, types.ChallengeID(newID(k.t, "chl")), types.EventID(newID(k.t, "evt"))))
}

// A grant that can no longer be issued when its proposal is confirmed
// (expired by then, its grantee revoked, its id taken) leaves the proposal
// invalidated, never applied and never open.
func TestGrantProposalInvalidatedAtApply(t *testing.T) {
	k := openGrantKit(t)
	defer func() { storeShift = 0 }()
	propose := func(scope string) types.ProposalID {
		_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, newID(t, "cmd"), []byte(scope), nil))
		return p.ID
	}
	invalidatedAtApply := func(name string, p types.ProposalID) {
		t.Helper()
		out, err := k.confirmHuman(p)
		if q := k.proposal(p); err != nil || out.Disposition != eventstore.DispositionInvalidated || q.Status != types.ProposalStatusInvalidated ||
			*q.InvalidatedReason != types.ProposalInvalidatedReasonValueProposalInvalidated || !strings.Contains(string(out.Result), "grant_not_issuable") {
			t.Errorf("%s: %+v %v, proposal %+v", name, out, err, q)
		}
	}
	other := k.supervisor().Actor()
	revoked := propose(k.scope(types.GrantID(newID(t, "grt")), other, `["`+string(prjA)+`"]`, `[]`))
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, other, suiteNow())
	})
	invalidatedAtApply("its grantee revoked", revoked)
	taken := types.GrantID(newID(t, "grt"))
	p := propose(k.scope(taken, k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`))
	k.mustCreate(k.op, k.scope(taken, k.grantee.Actor(), `"*"`, `[]`))
	if q := k.proposal(p); q.Status != types.ProposalStatusInvalidated {
		t.Errorf("its id taken: %+v", q)
	}
	if _, err := k.confirmHuman(p); !isReason(vocab.ReasonProposalNotOpen)(err) {
		t.Errorf("its id taken, confirmed: %v", err)
	}
	soon := strings.Replace(k.scopeUntil(types.GrantID(newID(t, "grt")), k.grantee.Actor(), suiteNow().Add(30*time.Minute)), `"*"`, `["`+string(prjA)+`"]`, 1)
	// An id already taken when proposed: the proposal is recorded, and its
	// application finds the grant cannot be issued.
	existing := types.GrantID(newID(t, "grt"))
	k.mustCreate(k.op, k.scope(existing, k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`))
	invalidatedAtApply("its id taken before", propose(k.scope(existing, k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`)))
	expired := propose(soon)
	storeShift = 45 * time.Minute // the proposer's grant (an hour) still covers it
	invalidatedAtApply("expired by then", expired)
	k.rebuilds()
}

// operator is a second live operator.
func (k *grantKit) operator() identity.Principal {
	k.t.Helper()
	a := types.Actor{ID: types.ActorID(newID(k.t, "act")), Kind: types.ActorKindOperator, Name: "second operator", CreatedBy: k.op.Actor()}
	var tok identity.Secret
	k.seed(k.op, create(a))
	k.seed(k.op, issueTo(a.ID, k.op.Actor(), &tok))
	return k.login(tok)
}

// approve records by's approval of the open proposal p, without applying it.
func (k *grantKit) approve(by identity.Principal, p types.Proposal) types.ApprovalID {
	k.t.Helper()
	id := types.ApprovalID(newID(k.t, "apr"))
	chl := types.ChallengeID(newID(k.t, "chl"))
	a := types.Approval{ID: id, Confirmation: types.ApprovalConfirmation{Challenge: &chl, SatisfiedBy: []types.EventID{types.EventID(newID(k.t, "evt"))}},
		Answers: []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: "approved", At: types.Time(suiteNow().UTC().Format(time.RFC3339Nano))}
	k.seed(by, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var actor types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(by.Actor()))
		if err == nil {
			err = json.Unmarshal(raw, &actor)
		}
		if err != nil {
			return nil, err
		}
		return authority.ApprovalEvent(authority.ApprovalOf(a, p, actor))
	})
	return id
}

// mutated is a grant event's payload with mut applied to its grant.
func mutated(payload json.RawMessage, mut func(*types.Grant)) (json.RawMessage, error) {
	var e types.GrantEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, err
	}
	mut(&e.Grant)
	return json.Marshal(e)
}

// forge is the grant p's scope asks for, valid from the command's time,
// issued by by under approval, as seed events; damage runs first, and mut
// changes the grant.
func forge(p types.Proposal, by types.ActorID, approval types.ApprovalID, damage func(*eventstore.Tx) error, mut func(*types.Grant)) applyFn {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var g types.Grant
		if err := json.Unmarshal(p.Scope, &g); err != nil {
			return nil, err
		}
		g.IssuedBy, g.Approval = by, &approval
		if damage != nil {
			if err := damage(tx); err != nil {
				return nil, err
			}
		}
		evs, err := authority.CreateGrant(tx, g)
		if err == nil && mut != nil {
			evs[0].Payload, err = mutated(evs[0].Payload, mut)
		}
		return evs, err
	}
}

// The fold admits a grant naming an approval only when it is its issuer's
// approval of a grant.create, under an index that names that approval.
func TestGrantFoldChecksItsApproval(t *testing.T) {
	k := openGrantKit(t)
	k.az.Handlers["agent.enrol"] = authority.GrantCreateHandler{} // a proposal of another action, with a grant's scope
	k.mustCreate(k.op, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), `["`+string(prjA)+`"]`, `["agent.enrol"]`))
	proposal := func(action string) types.Proposal {
		_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), action, newID(t, "cmd"),
			[]byte(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`)), nil))
		return p
	}
	create, extend := proposal(authority.GrantCreateAction), proposal("agent.enrol")
	op2 := k.operator()
	apr, other := k.approve(k.op, create), k.approve(k.op, extend)
	refused := func(name string, by identity.Principal, f applyFn) {
		t.Helper()
		if k.try(by, f) == nil {
			t.Errorf("a grant with %s was admitted", name)
		}
	}
	refused("another operator's approval", op2, forge(create, op2.Actor(), apr, nil, nil))
	refused("an approval of another action", k.op, forge(extend, k.op.Actor(), other, nil, nil))
	refused("an index naming another approval", k.op, forge(create, k.op.Actor(), "apr_01m3250vkwffc979nzjw0g2zz9", func(tx *eventstore.Tx) error {
		return tx.Put(authority.ProposalProjection, "approval_id/apr_01m3250vkwffc979nzjw0g2zz9", json.RawMessage(`"`+string(create.ID)+`"`))
	}, nil))
	refused("an index without its approval", k.op, forge(create, k.op.Actor(), apr, func(tx *eventstore.Tx) error {
		return tx.Delete(authority.ProposalProjection, "approval/"+string(create.ID))
	}, nil))
	if err := k.try(k.op, forge(create, k.op.Actor(), apr, nil, nil)); err != nil {
		t.Errorf("the approved grant, by its approver: %v", err)
	}
	k.rebuilds()
}

// twoWays is a handler that is an Applier and a ConfirmedApplier at once.
type twoWays struct{ authority.GrantCreateHandler }

func (twoWays) Apply(*eventstore.Tx, types.Actor, json.RawMessage) ([]eventstore.Event, json.RawMessage, error) {
	return nil, nil, errors.New("never")
}

// A handler implements exactly one way of applying: one with both is not wired.
func TestApplierIsExactlyOne(t *testing.T) {
	k := openGrantKit(t)
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "propose",
		[]byte(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), `["`+string(prjA)+`"]`, `[]`)), nil))
	k.az.Handlers[authority.GrantCreateAction] = twoWays{}
	if _, err := k.confirmHuman(p.ID); !errors.Is(err, authority.ErrActionNotWired) {
		t.Errorf("a handler with both: %v", err)
	}
}
