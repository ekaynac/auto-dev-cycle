// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

type grantFixture struct {
	t            *testing.T
	s            *eventstore.Store
	sys, op, sup types.Actor
	damaged      bool // a test wrote projection records directly
}

type applyFn = func(*eventstore.Tx) ([]eventstore.Event, error)

func newID(t *testing.T, prefix string) string {
	t.Helper()
	v, err := ids.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *grantFixture) submit(by types.ActorID, apply applyFn) error {
	cmd := newID(f.t, "cmd")
	_, err := f.s.SubmitApplied(context.Background(), eventstore.Command{ID: cmd, Actor: string(by), Action: "test.command", IdempotencyKey: cmd, Input: []byte(`{}`)}, eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (f *grantFixture) must(by types.ActorID, apply applyFn) {
	f.t.Helper()
	if err := f.submit(by, apply); err != nil {
		f.t.Fatal(err)
	}
}

func (f *grantFixture) view(fn func(*eventstore.Tx) error) {
	f.t.Helper()
	if err := f.s.View(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}

func create(a types.Actor) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) }
}

func createGrant(g types.Grant) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(g) }
}

func revokeGrant(g types.Grant, at time.Time) applyFn {
	return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, g.Actor, g.ID, at)
	}
}

// openGrants starts a store with the identity and grant kinds and
// projectors, and a system actor, an operator and a supervisor.
func openGrants(t *testing.T) *grantFixture {
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := identity.Kinds()
	maps.Copy(kinds, authority.GrantKinds())
	kinds["fixture.appended/v1"] = eventstore.Kind{Schema: "common/defs"}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs, Now: suiteNow,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil },
			Project: eventstore.ProjectEach(identity.Projector{}.Project, authority.GrantProjector{}.Project)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &grantFixture{t: t, s: s}
	// Every history a test admitted rebuilds to the same projections
	// (cleanups run last-registered first: before the store closes).
	t.Cleanup(func() {
		if diffs, err := s.RebuildProjections(context.Background()); !f.damaged && (err != nil || len(diffs) != 0) {
			t.Errorf("the admitted history does not rebuild: %v %v", diffs, err)
		}
	})
	f.sys = types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSystem, Name: "system"}
	f.sys.CreatedBy = f.sys.ID
	f.op = types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindOperator, Name: "operator", CreatedBy: f.sys.ID}
	f.sup = types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: f.op.ID}
	f.must(f.sys.ID, create(f.sys))
	f.must(f.sys.ID, create(f.op))
	f.must(f.op.ID, create(f.sup))
	return f
}

var (
	prjA = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sa1")
	prjB = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sb2")
	// t0 is the suite's base time, from the store's clock (suiteNow).
	t0 = suiteNow().UTC().Truncate(time.Second)
)

// grantFor is a grant to f.sup reading kinds on projects A, valid around t0.
func (f *grantFixture) grantFor(kinds ...types.Name) types.Grant {
	return types.Grant{ID: types.GrantID(newID(f.t, "grt")), Actor: f.sup.ID, Projects: types.ProjectScope{List: []types.ProjectID{prjA}},
		Actions: []types.ActionKind{}, Reads: types.GrantReads{Projects: types.GrantReadsProjects{Same: true}, Kinds: kinds},
		ValidFrom: types.Time(t0.Add(-time.Hour).Format(time.RFC3339)), ExpiresAt: types.Time(t0.Add(time.Hour).Format(time.RFC3339)),
		ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: f.op.ID, Approval: types.ApprovalID(newID(f.t, "apr"))}
}

// The projector admits a grant created once, by a live operator as its
// issuer, for a live supervisor, and revoked once, as issued.
func TestGrantTransitions(t *testing.T) {
	f := openGrants(t)
	g := f.grantFor("fixture")
	forOp := g
	forOp.ID, forOp.Actor = types.GrantID(newID(t, "grt")), f.op.ID
	bySup := g
	bySup.ID, bySup.IssuedBy = types.GrantID(newID(t, "grt")), f.sup.ID
	inverted := g
	inverted.ID, inverted.ValidFrom, inverted.ExpiresAt = types.GrantID(newID(t, "grt")), g.ExpiresAt, g.ValidFrom
	for name, c := range map[string]struct {
		by    types.ActorID
		apply applyFn
	}{
		"a grant by its supervisor":     {f.sup.ID, createGrant(bySup)},
		"a grant issued by another":     {f.op.ID, createGrant(bySup)},
		"a grant for an operator":       {f.op.ID, createGrant(forOp)},
		"a validity that is no range":   {f.op.ID, createGrant(inverted)},
		"revoking an unknown grant":     {f.op.ID, revokeGrant(g, t0)},
		"a grant written by the system": {f.sys.ID, createGrant(g)},
	} {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	f.must(f.op.ID, createGrant(g))
	if err := f.submit(f.op.ID, createGrant(g)); err == nil {
		t.Error("a second creation: accepted")
	}
	if err := f.submit(f.sup.ID, revokeGrant(g, t0)); err == nil {
		t.Error("a supervisor revoked its grant: accepted")
	}
	altered := g
	altered.Reads.Kinds = []types.Name{"other"}
	if err := f.submit(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		evs, err := authority.CreateGrant(altered)
		if err == nil {
			evs[0].Kind = "grant.revoked/v1"
		}
		return evs, err
	}); err == nil {
		t.Error("a revocation whose kind and payload disagree: accepted")
	}
	f.must(f.op.ID, revokeGrant(g, t0))
	if err := f.submit(f.op.ID, revokeGrant(g, t0)); err == nil {
		t.Error("a second revocation: accepted")
	}
	f.view(func(tx *eventstore.Tx) error {
		grants, revoked, err := authority.GrantsOf(tx, f.sup.ID)
		if len(grants) != 1 || grants[0].ID != g.ID || !revoked[g.ID] {
			t.Errorf("grants %v, revoked %v", grants, revoked)
		}
		return err
	})
	// The projection is a fold: a rebuild finds the same records.
	if diffs, err := f.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

func event(kind string, project *types.ProjectID) eventstore.Event {
	e := eventstore.Event{Kind: kind}
	if project != nil {
		p := string(*project)
		e.Project = &p
	}
	return e
}

// A scope reads what some grant in force reads: its projects (same, or
// listed) and its kinds; an event outside any project only under an
// every-project read. An operator reads everything.
func TestReadScope(t *testing.T) {
	f := openGrants(t)
	g := f.grantFor("fixture")
	listed := f.grantFor("lease")
	listed.Reads.Projects = types.GrantReadsProjects{List: []types.ProjectID{prjB}}
	expired := f.grantFor("token")
	expired.ExpiresAt = types.Time(t0.Add(-time.Minute).Format(time.RFC3339))
	everywhere := f.grantFor("grant")
	everywhere.Projects = types.ProjectScope{All: true}
	revoked := f.grantFor("identity")
	revoked.Projects = types.ProjectScope{All: true}
	for _, x := range []types.Grant{g, listed, expired, everywhere, revoked} {
		f.must(f.op.ID, createGrant(x))
	}
	f.must(f.op.ID, revokeGrant(revoked, t0))
	f.view(func(tx *eventstore.Tx) error {
		s, err := authority.ReadScopeOf(tx, f.sup, t0)
		for name, c := range map[string]struct {
			e    eventstore.Event
			want bool
		}{
			"its kind on its project":         {event("fixture.appended/v1", &prjA), true},
			"its kind on another project":     {event("fixture.appended/v1", &prjB), false},
			"another kind on its project":     {event("lease.granted/v1", &prjA), false},
			"a listed read project":           {event("lease.granted/v1", &prjB), true},
			"no project, under a listed read": {event("fixture.appended/v1", nil), false},
			"no project, under every project": {event("grant.created/v1", nil), true},
			"an expired grant's kind":         {event("token.issued/v1", &prjA), false},
			"a revoked grant's kind":          {event("identity.x/v1", nil), false},
		} {
			if got := s.Reads(c.e); got != c.want {
				t.Errorf("%s: reads %v, want %v", name, got, c.want)
			}
		}
		op, err2 := authority.ReadScopeOf(tx, f.op, t0)
		if op.Empty() || !op.Reads(event("token.issued/v1", nil)) {
			t.Error("an operator does not read everything")
		}
		return errors.Join(err, err2)
	})
}

// A scope holds while every grant it was taken under is in force: a
// revocation, an expiry or a narrowing (a revocation and a new grant) ends
// it; a new grant does not.
func TestReadScopeHolds(t *testing.T) {
	f := openGrants(t)
	g := f.grantFor("fixture")
	f.must(f.op.ID, createGrant(g))
	var s authority.ReadScope
	holds := func(at time.Time) bool {
		var ok bool
		f.view(func(tx *eventstore.Tx) (err error) { ok, err = s.Holds(tx, at); return })
		return ok
	}
	f.view(func(tx *eventstore.Tx) (err error) { s, err = authority.ReadScopeOf(tx, f.sup, t0); return })
	if s.Empty() || !holds(t0) {
		t.Fatal("a scope in force does not hold")
	}
	f.must(f.op.ID, createGrant(f.grantFor("lease")))
	if !holds(t0) {
		t.Error("a new grant ended the scope")
	}
	if holds(t0.Add(time.Hour)) {
		t.Error("an expired grant holds")
	}
	f.must(f.op.ID, revokeGrant(g, t0))
	f.must(f.op.ID, createGrant(f.grantFor("fixture")))
	if holds(t0) {
		t.Error("a narrowed (revoked and reissued) grant holds")
	}
	var none authority.ReadScope
	f.view(func(tx *eventstore.Tx) (err error) {
		none, err = authority.ReadScopeOf(tx, f.sup, t0.Add(2*time.Hour))
		return
	})
	if !none.Empty() {
		t.Error("a supervisor with no grant in force reads something")
	}
}

// plant writes a projection record directly, as damage would: no event.
func (f *grantFixture) plant(key, raw string) {
	f.damaged = true
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		if raw == "" {
			return nil, tx.Delete(authority.GrantProjection, key)
		}
		return nil, tx.Put(authority.GrantProjection, key, json.RawMessage(raw))
	})
}

func record(t *testing.T, g types.Grant, transition string, revokedAt any) string {
	b, err := json.Marshal(map[string]any{"transition": transition, "grant": g, "revoked_at": revokedAt})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every unknown, damaged, boundary or empty case denies: no scope at open,
// a scope that no longer holds, or an event not read. Nothing fails open.
func TestReadScopeFailsClosed(t *testing.T) {
	key := func(g types.Grant) string { return string(g.Actor) + "/" + string(g.ID) }
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	type check struct {
		actor func(f *grantFixture) types.Actor
		now   func(g types.Grant) time.Time
	}
	sup := func(f *grantFixture) types.Actor { return f.sup }
	inside := func(types.Grant) time.Time { return t0 }
	// Opening: the scope is empty (and an error, for damage).
	for name, c := range map[string]struct {
		setup func(f *grantFixture, g types.Grant)
		check
		wantErr bool
	}{
		"an actor of unknown kind": {nil, check{func(f *grantFixture) types.Actor { a := f.sup; a.Kind = "robot"; return a }, inside}, false},
		"an actor of no kind":      {nil, check{func(f *grantFixture) types.Actor { a := f.sup; a.Kind = ""; return a }, inside}, false},
		"at the expiry instant":    {nil, check{sup, func(g types.Grant) time.Time { return at(string(g.ExpiresAt)) }}, false},
		"before valid_from":        {nil, check{sup, func(g types.Grant) time.Time { return at(string(g.ValidFrom)).Add(-time.Nanosecond) }}, false},
		"a zero clock":             {nil, check{sup, func(types.Grant) time.Time { return time.Time{} }}, false},
		"an unparseable expiry": {func(f *grantFixture, g types.Grant) {
			g.ExpiresAt = "soon"
			f.plant(key(g), record(t, g, "grant.created", nil))
		}, check{sup, inside}, false},
		"revoked without a time": {func(f *grantFixture, g types.Grant) {
			f.plant(key(g), record(t, g, "grant.revoked", nil))
		}, check{sup, inside}, false},
		"an unknown status": {func(f *grantFixture, g types.Grant) {
			f.plant(key(g), record(t, g, "grant.paused", nil))
		}, check{sup, inside}, false},
		"an unparseable record": {func(f *grantFixture, g types.Grant) { f.plant(key(g), `{"grant": 7}`) }, check{sup, inside}, true},
		"a record under another grant's key": {func(f *grantFixture, g types.Grant) {
			other := g
			other.ID = types.GrantID(newID(t, "grt"))
			f.plant(key(other), record(t, g, "grant.created", nil))
		}, check{sup, inside}, true},
		"a revoked grant created again": {func(f *grantFixture, g types.Grant) {
			f.must(f.op.ID, revokeGrant(g, t0))
			if err := f.submit(f.op.ID, createGrant(g)); err == nil {
				t.Error("a revoked grant was created again")
			}
		}, check{sup, inside}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := openGrants(t)
			g := f.grantFor("fixture")
			f.must(f.op.ID, createGrant(g))
			if c.setup != nil {
				c.setup(f, g)
			}
			f.view(func(tx *eventstore.Tx) error {
				s, err := authority.ReadScopeOf(tx, c.actor(f), c.now(g))
				if !s.Empty() || (err != nil) != c.wantErr || s.Reads(event("fixture.appended/v1", &prjA)) {
					t.Errorf("scope empty %v, err %v (want error %v)", s.Empty(), err, c.wantErr)
				}
				return nil
			})
		})
	}
	// Open: the scope stops holding.
	for name, change := range map[string]func(f *grantFixture, g types.Grant){
		"the record changed under the same id": func(f *grantFixture, g types.Grant) {
			g.Reads.Kinds = []types.Name{"fixture", "token"}
			f.plant(key(g), record(t, g, "grant.created", nil))
		},
		"the record vanished":       func(f *grantFixture, g types.Grant) { f.plant(key(g), "") },
		"the record is damaged":     func(f *grantFixture, g types.Grant) { f.plant(key(g), `[]`) },
		"the status became unknown": func(f *grantFixture, g types.Grant) { f.plant(key(g), record(t, g, "grant.paused", nil)) },
	} {
		t.Run(name, func(t *testing.T) {
			f := openGrants(t)
			g := f.grantFor("fixture")
			f.must(f.op.ID, createGrant(g))
			var s authority.ReadScope
			f.view(func(tx *eventstore.Tx) (err error) { s, err = authority.ReadScopeOf(tx, f.sup, t0); return })
			change(f, g)
			f.view(func(tx *eventstore.Tx) error {
				if ok, _ := s.Holds(tx, t0); ok {
					t.Error("the scope still holds")
				}
				return nil
			})
		})
	}
	// A zero scope holds nothing and reads nothing.
	f := openGrants(t)
	f.view(func(tx *eventstore.Tx) error {
		var zero authority.ReadScope
		if ok, err := zero.Holds(tx, t0); ok || err != nil || !zero.Empty() || zero.Reads(event("grant.created/v1", nil)) {
			t.Error("a zero scope holds or reads")
		}
		return nil
	})
	// Empty kinds or projects read nothing; they never mean "all".
	noKinds := f.grantFor()
	noKinds.Reads.Kinds = []types.Name{}
	noReadProjects := f.grantFor("fixture")
	noReadProjects.Reads.Projects = types.GrantReadsProjects{List: []types.ProjectID{}}
	noProjects := f.grantFor("fixture")
	noProjects.Projects = types.ProjectScope{List: []types.ProjectID{}}
	for _, g := range []types.Grant{noKinds, noReadProjects, noProjects} {
		f.must(f.op.ID, createGrant(g))
	}
	f.view(func(tx *eventstore.Tx) error {
		s, err := authority.ReadScopeOf(tx, f.sup, t0)
		for _, e := range []eventstore.Event{event("fixture.appended/v1", &prjA), event("fixture.appended/v1", &prjB), event("fixture.appended/v1", nil), event("grant.created/v1", nil)} {
			if s.Reads(e) {
				t.Errorf("empty kinds or projects read %s on %v", e.Kind, e.Project)
			}
		}
		return err
	})
}

// One command may create a supervisor and its grant, in that order; a
// grant before its grantee is refused, as a rebuild replaying event by
// event would refuse it.
func TestMixedIdentityAndGrantBatch(t *testing.T) {
	f := openGrants(t)
	both := func(first, second applyFn) applyFn {
		return func(tx *eventstore.Tx) ([]eventstore.Event, error) {
			a, err1 := first(tx)
			b, err2 := second(tx)
			return append(a, b...), errors.Join(err1, err2)
		}
	}
	for name, c := range map[string]struct {
		grantFirst bool
		ok         bool
	}{"the actor, then its grant": {false, true}, "a grant, then its actor": {true, false}} {
		sup := types.Actor{ID: types.ActorID(newID(t, "act")), Kind: types.ActorKindSupervisor, Name: "supervisor", CreatedBy: f.op.ID}
		g := f.grantFor("fixture")
		g.Actor = sup.ID
		apply := both(create(sup), createGrant(g))
		if c.grantFirst {
			apply = both(createGrant(g), create(sup))
		}
		if err := f.submit(f.op.ID, apply); (err == nil) != c.ok {
			t.Errorf("%s: %v, want admitted %v", name, err, c.ok)
		}
	}
	// The rebuild check runs at cleanup.
}
