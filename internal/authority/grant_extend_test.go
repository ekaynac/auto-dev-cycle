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
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// 14 PR 7d (N6, P6): grant.extend, an atomic replacement, through the real
// Authorizer and writer.

// extension is a grant.extend scope replacing old by id on projects, with
// actions, expiring in two hours.
func extension(old, id types.GrantID, projects, actions string) string {
	return `{"replaces":"` + string(old) + `","id":"` + string(id) + `","projects":` + projects + `,"actions":` + actions +
		`,"reads":{"projects":"same","kinds":["grant"]},"expires_at":"` + suiteNow().Add(2*time.Hour).UTC().Format(time.RFC3339) +
		`","confirmation_overrides":{}}`
}

func onA() string { return `["` + string(prjA) + `"]` }

// kindsAfter are the kinds of the events after seq h.
func (k *grantKit) kindsAfter(h int64) (kinds []string) {
	k.view(func(tx *eventstore.Tx) error {
		evs, err := tx.EventsAfter(h, 20)
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
		}
		return err
	})
	return kinds
}

func (k *grantKit) versionOf(subject string) (v int64) {
	k.view(func(tx *eventstore.Tx) (err error) { v, err = tx.SubjectVersion(subject); return })
	return v
}

func (k *grantKit) readScope(p identity.Principal) (s authority.ReadScope) {
	k.view(func(tx *eventstore.Tx) error {
		var a types.Actor
		raw, _, err := tx.Get(identity.ActorProjection, string(p.Actor()))
		if err == nil {
			err = json.Unmarshal(raw, &a)
		}
		if err == nil {
			s, err = authority.ReadScopeOf(tx, a, suiteNow())
		}
		return err
	})
	return s
}

func (k *grantKit) holds(s authority.ReadScope) (ok bool) {
	k.view(func(tx *eventstore.Tx) (err error) { ok, err = s.Holds(tx, suiteNow()); return })
	return ok
}

// An operator's grant.extend, bound to the old grant's state, revokes it,
// creates the replacement for the same grantee and records the lineage in
// one command; both grants' versions advance; proposals made under the old
// grant are invalidated; a stream held under it closes, and one taken
// again holds under the replacement. It replays; the old grant is never
// replaced again.
func TestOperatorExtendsAGrant(t *testing.T) {
	k := openGrantKit(t)
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "propose",
		[]byte(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), onA(), `[]`)), nil))
	held := k.readScope(k.sup)
	next := types.GrantID(newID(t, "grt"))
	in := extension(k.creator, next, onA(), `["grant.create","note.add"]`)
	k.none("a stale expectation", k.op, authority.GrantExtendAction, in, version(k.versionOf(string(k.creator))-1), is(eventstore.ErrStaleExpectedState))
	k.none("another digest", k.op, authority.GrantExtendAction, in, digestOf(contentDigest(t, `{"x":1}`)), is(eventstore.ErrStaleExpectedState))
	h, at := k.head(), version(k.versionOf(string(k.creator)))
	out, err := k.submit(k.op, authority.GrantExtendAction, "extend", in, at)
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("an operator's extension: %+v %v", out, err)
	}
	if kinds := k.kindsAfter(h); !slices.Equal(kinds, []string{"grant.revoked/v1", "grant.created/v1", authority.GrantReplaced}) {
		t.Errorf("the extension committed %v", kinds)
	}
	if k.versionOf(string(k.creator)) != out.LastSeq || k.versionOf(string(next)) != out.LastSeq-1 {
		t.Errorf("versions: old %d, new %d, command %d..%d", k.versionOf(string(k.creator)), k.versionOf(string(next)), out.FirstSeq, out.LastSeq)
	}
	old, g := k.grant(k.creator), k.grant(next).Grant
	if old.RevokedAt == nil || g.Actor != k.sup.Actor() || g.IssuedBy != k.op.Actor() || g.Approval != nil ||
		string(g.ValidFrom) != k.recorded[string(next)].At || !slices.Equal(g.Actions, []types.ActionKind{"grant.create", "note.add"}) {
		t.Errorf("old %+v, new %+v", old, g)
	}
	k.view(func(tx *eventstore.Tx) error {
		raw, _, err := tx.Get(authority.GrantLineageProjection, string(k.creator))
		back, _, err2 := tx.Get(authority.GrantLineageProjection, authority.ReplacementKey(next))
		if string(raw) != `"`+string(next)+`"` || string(back) != `"`+string(k.creator)+`"` {
			t.Errorf("the lineage: %s, back %s", raw, back)
		}
		return errors.Join(err, err2)
	})
	if q := k.proposal(p.ID); q.Status != types.ProposalStatusInvalidated || *q.InvalidatedReason != types.ProposalInvalidatedReasonValueAuthorityRevoked {
		t.Errorf("the proposal under the old grant: %+v", q)
	}
	if k.holds(held) {
		t.Error("a scope held under the replaced grant still holds")
	}
	reach := func(s authority.ReadScope) (gs []types.GrantID) {
		for _, r := range s.Reach() {
			gs = append(gs, r.Grant)
		}
		return gs
	}
	if again := k.readScope(k.sup); !k.holds(again) || !slices.Contains(reach(again), next) || slices.Contains(reach(again), k.creator) || !slices.Contains(reach(held), k.creator) {
		t.Errorf("reconnected: %+v", again.Reach())
	}
	if again, err := k.submit(k.op, authority.GrantExtendAction, "extend", in, at); err != nil || !again.Replayed || again.LastSeq != out.LastSeq {
		t.Errorf("the replay: %+v %v", again, err)
	}
	k.none("an unknown grant", k.op, authority.GrantExtendAction, extension(types.GrantID(newID(t, "grt")), types.GrantID(newID(t, "grt")), onA(), `[]`), nil, is(authority.ErrNoGrant))
	// The logic replaces only the grant its derivation bound.
	h2 := k.head()
	unbound := k.op.Command(newID(t, "cmd"), "test.seed", newID(t, "cmd"), []byte(extension(next, types.GrantID(newID(t, "grt")), onA(), `[]`)), nil)
	if _, err := k.s.Submit(context.Background(), unbound, authority.GrantExtendHandler{}.Logic(unbound)); err == nil || k.head() != h2 {
		t.Errorf("an unbound replacement: %v", err)
	}
	k.none("a replaced grant again", k.op, authority.GrantExtendAction, extension(k.creator, types.GrantID(newID(t, "grt")), onA(), `[]`), nil, is(authority.ErrNoGrant))
	k.none("a taken id", k.op, authority.GrantExtendAction, extension(next, k.creator, onA(), `[]`), nil, isReason(vocab.ReasonGrantNotIssuable))
	k.none("an expired replacement", k.op, authority.GrantExtendAction,
		strings.Replace(extension(next, types.GrantID(newID(t, "grt")), onA(), `[]`), suiteNow().Add(2*time.Hour).UTC().Format(time.RFC3339), suiteNow().Add(-time.Second).UTC().Format(time.RFC3339Nano), 1),
		nil, isReason(vocab.ReasonGrantNotIssuable))
	k.none("the system's extension", k.sys, authority.GrantExtendAction, extension(next, types.GrantID(newID(t, "grt")), onA(), `[]`), nil, isReason(vocab.ReasonOperatorOnly))
	isSchema := func(err error) bool {
		var e *contract.Error
		return errors.As(err, &e) && errors.Is(err, authority.ErrInvalidScope)
	}
	withActor := strings.Replace(extension(next, types.GrantID(newID(t, "grt")), onA(), `[]`), `"projects"`, `"actor":"`+string(k.grantee.Actor())+`","projects"`, 1)
	k.none("a scope naming the grantee", k.op, authority.GrantExtendAction, withActor, nil, isSchema)
	k.rebuilds()
}

// A supervisor's grant.extend of another's grant is a human proposal bound
// to the old grant, with its grantee; its own grant is refused. No
// existence oracle: an absent grant is refused exactly as an uncovered one,
// before any issuable check. Confirmed, the replacement names the approval
// and its confirmer; an issuable failure by then invalidates it.
func TestSupervisorGrantExtend(t *testing.T) {
	k := openGrantKit(t)
	defer func() { storeShift = 0 }()
	k.mustCreate(k.op, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), onA(), `["grant.extend"]`))
	target := types.GrantID(newID(t, "grt"))
	k.mustCreate(k.op, k.scope(target, k.grantee.Actor(), onA(), `[]`))
	onB := types.GrantID(newID(t, "grt"))
	k.mustCreate(k.op, k.scope(onB, k.grantee.Actor(), `["prj_01m3250vkwffc979nzjw0g2sb2"]`, `[]`))
	notGranted, ungranted := isReason(vocab.ReasonActionNotGranted), k.supervisor()
	var details []string
	for _, c := range []struct {
		name string
		p    identity.Principal
		old  types.GrantID
	}{
		{"an ungranted supervisor, an existing grant", ungranted, target},
		{"an ungranted supervisor, no grant", ungranted, types.GrantID(newID(t, "grt"))},
		{"a granted supervisor, another project's grant", k.sup, onB},
		{"a granted supervisor, no grant", k.sup, types.GrantID(newID(t, "grt"))},
	} {
		_, err := k.submit(c.p, authority.GrantExtendAction, newID(t, "cmd"), extension(c.old, k.creator, onA(), `[]`), nil)
		if !notGranted(err) {
			t.Errorf("%s: %v", c.name, err)
		}
		details = append(details, detail(err))
	}
	if details[0] != details[1] || details[2] != details[3] {
		t.Errorf("the refusals differ: %q", details)
	}
	k.none("its own grant", k.sup, authority.GrantExtendAction, extension(k.creator, types.GrantID(newID(t, "grt")), onA(), `[]`), nil, isReason(vocab.ReasonSelfGrantRefused))
	// Scoped to A, it replaces only a grant naming A alone by one naming A alone.
	b, ab := `["prj_01m3250vkwffc979nzjw0g2sb2"]`, `["`+string(prjA)+`","prj_01m3250vkwffc979nzjw0g2sb2"]`
	for _, c := range []struct{ name, from, to string }{
		{"A to B", onA(), b}, {"A to every project", onA(), `"*"`}, {"A to A and B", onA(), ab}, {"A and B to A", ab, onA()}, {"every project to A", `"*"`, onA()},
	} {
		old := types.GrantID(newID(t, "grt"))
		k.mustCreate(k.op, k.scope(old, k.grantee.Actor(), c.from, `[]`))
		k.none(c.name, k.sup, authority.GrantExtendAction, extension(old, types.GrantID(newID(t, "grt")), c.to, `[]`), nil, notGranted)
	}
	propose := func(old types.GrantID, until time.Duration) types.Proposal {
		in := extension(old, types.GrantID(newID(t, "grt")), onA(), `["note.add"]`)
		in = strings.Replace(in, suiteNow().Add(2*time.Hour).UTC().Format(time.RFC3339), suiteNow().Add(until).UTC().Format(time.RFC3339Nano), 1)
		_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantExtendAction, newID(t, "cmd"), []byte(in), nil))
		return p
	}
	p := propose(target, 2*time.Hour)
	if p.Policy != types.ProposalPolicyHuman || p.Object.ID != string(target) || *p.Grantee != k.grantee.Actor() || *p.Project != prjA ||
		!slices.Equal(p.ChangeClass, []types.ChangeClass{"grant.extend"}) {
		t.Errorf("the proposal: %+v", p)
	}
	h := k.head()
	out, err := k.confirmHuman(p.ID)
	var res struct {
		Approval types.ApprovalID `json:"approval"`
		Result   struct {
			Grant types.GrantID `json:"grant"`
		} `json:"result"`
	}
	if err != nil || out.Disposition != eventstore.DispositionApplied || json.Unmarshal(out.Result, &res) != nil {
		t.Fatalf("the confirmed extension: %+v %v", out, err)
	}
	if kinds := k.kindsAfter(h); !slices.Equal(kinds, []string{authority.ApprovalRecorded, authority.ProposalConfirmed, "grant.revoked/v1", "grant.created/v1",
		authority.GrantReplaced, authority.ProposalApplied}) {
		t.Errorf("the confirmation committed %v", kinds)
	}
	if g := k.grant(res.Result.Grant).Grant; g.Approval == nil || *g.Approval != res.Approval || g.IssuedBy != k.op.Actor() || g.Actor != k.grantee.Actor() {
		t.Errorf("the replacement: %+v", g)
	}
	// Unissuable by the confirmation: the proposal is invalidated.
	other := k.supervisor().Actor()
	theirs := types.GrantID(newID(t, "grt"))
	k.mustCreate(k.op, k.scope(theirs, other, onA(), `[]`))
	revoked, expired := propose(theirs, 2*time.Hour), propose(res.Result.Grant, 30*time.Minute)
	mine := types.GrantID(newID(t, "grt"))
	k.mustCreate(k.op, k.scope(mine, k.grantee.Actor(), onA(), `[]`))
	takenBy := propose(mine, 2*time.Hour)
	var taken types.Grant
	if err := json.Unmarshal(takenBy.Scope, &taken); err != nil {
		t.Fatal(err)
	}
	k.mustCreate(k.op, k.scope(taken.ID, k.grantee.Actor(), onA(), `[]`)) // its new id, meanwhile
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, other, suiteNow())
	})
	storeShift = 45 * time.Minute
	for name, q := range map[string]types.Proposal{"its grantee revoked": revoked, "expired by then": expired, "its new id taken": takenBy} {
		out, err := k.confirmHuman(q.ID)
		if err != nil || out.Disposition != eventstore.DispositionInvalidated || !strings.Contains(string(out.Result), "grant_not_issuable") {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	k.rebuilds()
}

// lineageOf is a grant.replaced/v1 event replacing old by g.
func lineageOf(t *testing.T, old types.GrantID, g types.Grant) eventstore.Event {
	b, err := json.Marshal(types.GrantReplaced{Transition: "grant.replaced", Replaces: old, Grant: g})
	id, err2 := ids.New("evt")
	if err := errors.Join(err, err2); err != nil {
		t.Fatal(err)
	}
	return eventstore.Event{ID: id, Kind: authority.GrantReplaced, Subject: string(old), Payload: b}
}

// The fold admits a lineage only for the old grant revoked and the new one
// created, for the same grantee, by the same command, once per old and per
// new grant; and a grant naming an extension's approval only with that
// extension's old grant revoked by the same command.
func TestGrantLineageFold(t *testing.T) {
	k := openGrantKit(t)
	fresh := func(actor types.ActorID) types.Grant {
		var g types.Grant
		if err := json.Unmarshal([]byte(k.scope(types.GrantID(newID(t, "grt")), actor, onA(), `[]`)), &g); err != nil {
			t.Fatal(err)
		}
		g.IssuedBy = k.op.Actor()
		return g
	}
	old := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, old) })
	old = k.grant(old.ID).Grant
	replace := func(revoke bool, g types.Grant, mut func(*types.Grant)) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			var evs []eventstore.Event
			if revoke {
				r, err := authority.RevokeGrant(tx, old.Actor, old.ID, tx.Now())
				if err != nil {
					return nil, err
				}
				evs = r
			}
			c, err := authority.CreateGrant(tx, g)
			if err != nil {
				return nil, err
			}
			g.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
			if mut != nil {
				mut(&g)
			}
			return append(append(evs, c...), lineageOf(t, old.ID, g)), nil
		}
	}
	refused := func(name string, f applyFn) {
		t.Helper()
		if k.try(k.op, f) == nil {
			t.Errorf("a lineage with %s was admitted", name)
		}
	}
	refused("its old grant live", replace(false, fresh(k.grantee.Actor()), nil))
	refused("another grantee", replace(true, fresh(k.sup.Actor()), nil))
	refused("another grant than created", replace(true, fresh(k.grantee.Actor()), func(g *types.Grant) { g.Actions = []types.ActionKind{"note.add"} }))
	refused("another subject", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := replace(true, fresh(k.grantee.Actor()), nil)(tx)
		if err == nil {
			evs[2].Subject = string(evs[1].Subject)
		}
		return evs, err
	})
	earlier := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, earlier) })
	earlier = k.grant(earlier.ID).Grant
	refused("a new grant created before", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, err := authority.RevokeGrant(tx, old.Actor, old.ID, tx.Now())
		return append(r, lineageOf(t, old.ID, earlier)), err
	})
	// An extension's approval for a grant whose old grant is not revoked.
	k.mustCreate(k.op, k.scope(types.GrantID(newID(t, "grt")), k.sup.Actor(), onA(), `["grant.extend"]`))
	_, p := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantExtendAction, "p", []byte(extension(old.ID, types.GrantID(newID(t, "grt")), onA(), `[]`)), nil))
	apr := k.approve(k.op, p)
	var g types.Grant
	if err := json.Unmarshal(p.Scope, &g); err != nil {
		t.Fatal(err)
	}
	g.Actor, g.IssuedBy, g.Approval = old.Actor, k.op.Actor(), &apr
	refused("an extension's approval, the old grant live", func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, g) })
	// The approval's grant, and another old grant revoked with it: the
	// lineage names the other one.
	other := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, other) })
	refused("an approval of another replacement", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r1, err1 := authority.RevokeGrant(tx, old.Actor, old.ID, tx.Now())
		r2, err2 := authority.RevokeGrant(tx, other.Actor, other.ID, tx.Now())
		c, err3 := authority.CreateGrant(tx, g)
		n := g
		n.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return append(append(append(r1, r2...), c...), lineageOf(t, other.ID, n)), errors.Join(err1, err2, err3)
	})
	if err := k.try(k.op, replace(true, fresh(k.grantee.Actor()), nil)); err != nil {
		t.Fatalf("a replacement: %v", err)
	}
	twice := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, twice) })
	refused("two lineages of one grant", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := authority.RevokeGrant(tx, twice.Actor, twice.ID, tx.Now())
		for _, g := range []types.Grant{fresh(k.grantee.Actor()), fresh(k.grantee.Actor())} {
			c, err2 := authority.CreateGrant(tx, g)
			g.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
			evs, err = append(append(evs, c...), lineageOf(t, twice.ID, g)), errors.Join(err, err2)
		}
		return evs, err
	})
	// One new grant claimed by two lineages, each old grant revoked and the
	// new grant created by the same command.
	first, second := fresh(k.grantee.Actor()), fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, first) })
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, second) })
	refused("one new grant claimed by two lineages", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r1, err1 := authority.RevokeGrant(tx, first.Actor, first.ID, tx.Now())
		r2, err2 := authority.RevokeGrant(tx, second.Actor, second.ID, tx.Now())
		n := fresh(k.grantee.Actor())
		c, err3 := authority.CreateGrant(tx, n)
		n.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return append(append(append(append(r1, r2...), c...), lineageOf(t, first.ID, n)), lineageOf(t, second.ID, n)), errors.Join(err1, err2, err3)
	})
	// A lineage by an actor no longer a live operator.
	op2 := k.operator()
	byOp2 := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, byOp2) })
	if k.try(op2, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, err := authority.RevokeGrant(tx, byOp2.Actor, byOp2.ID, tx.Now())
		n := fresh(k.grantee.Actor())
		n.IssuedBy = op2.Actor()
		c, err2 := authority.CreateGrant(tx, n)
		n.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		gone, err3 := identity.RevokeActor(tx, op2.Actor(), tx.Now())
		return append(append(append(r, c...), gone...), lineageOf(t, byOp2.ID, n)), errors.Join(err, err2, err3)
	}) == nil {
		t.Error("a lineage by a revoked operator was admitted")
	}
	// A lineage carrying a grant.create approval.
	_, cp := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantCreateAction, "c", []byte(k.scope(types.GrantID(newID(t, "grt")), k.grantee.Actor(), onA(), `[]`)), nil))
	aprC := k.approve(k.op, cp)
	byCreate := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, byCreate) })
	refused("a grant.create approval", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		var n types.Grant
		err := json.Unmarshal(cp.Scope, &n)
		n.IssuedBy, n.Approval = k.op.Actor(), &aprC
		r, err2 := authority.RevokeGrant(tx, byCreate.Actor, byCreate.ID, tx.Now())
		c, err3 := authority.CreateGrant(tx, n)
		n.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return append(append(r, c...), lineageOf(t, byCreate.ID, n)), errors.Join(err, err2, err3)
	})
	// An extension's approval of a still-live old grant, for the case below.
	freshOld := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, freshOld) })
	_, pf := k.submitProposal(k.sup.Command(newID(t, "cmd"), authority.GrantExtendAction, "pf", []byte(extension(freshOld.ID, types.GrantID(newID(t, "grt")), onA(), `[]`)), nil))
	aprF := k.approve(k.op, pf)
	var gf types.Grant
	if err := json.Unmarshal(pf.Scope, &gf); err != nil {
		t.Fatal(err)
	}
	gf.Actor, gf.IssuedBy, gf.Approval = freshOld.Actor, k.op.Actor(), &aprF
	// "The same command" is the command, not its time: with the clock
	// trailing, consecutive commands share one recorded time.
	storeShift = -time.Hour
	defer func() { storeShift = 0 }()
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, freshOld.Actor, freshOld.ID, tx.Now())
	})
	refused("an extension's approval, its old grant revoked by an earlier command", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.CreateGrant(tx, gf)
	})
	early := fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, early) })
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, early.Actor, early.ID, tx.Now())
	})
	refused("an old grant revoked by an earlier command", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		n := fresh(k.grantee.Actor())
		c, err := authority.CreateGrant(tx, n)
		n.ValidFrom = types.Time(tx.Now().UTC().Format(time.RFC3339Nano))
		return append(c, lineageOf(t, early.ID, n)), err
	})
	late, made := fresh(k.grantee.Actor()), fresh(k.grantee.Actor())
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, late) })
	k.seed(k.op, func(tx *eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(tx, made) })
	made = k.grant(made.ID).Grant
	refused("a new grant created by an earlier command", func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		r, err := authority.RevokeGrant(tx, late.Actor, late.ID, tx.Now())
		return append(r, lineageOf(t, late.ID, made)), err
	})
	k.rebuilds()
}

// The endpoint is reached through the daemon's tests; here, the extension
// result names both grants.
func TestExtensionResult(t *testing.T) {
	k := openGrantKit(t)
	next := types.GrantID(newID(t, "grt"))
	out, err := k.submit(k.op, authority.GrantExtendAction, "x", extension(k.creator, next, onA(), `[]`), nil)
	var r struct {
		Grant, Replaces types.GrantID
	}
	if err != nil || json.Unmarshal(out.Result, &r) != nil || r.Grant != next || r.Replaces != k.creator {
		t.Errorf("%s %v", out.Result, err)
	}
}
