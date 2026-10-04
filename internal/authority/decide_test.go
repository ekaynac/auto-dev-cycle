// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	opID   = types.ActorID("act_01m3250vm7f6ktw39dszqnyddr")
	supID  = types.ActorID("act_01m3250v0aer9ss2097svmefj8")
	sup2ID = types.ActorID("act_01m3250vz2ezn81cgvbkcn17jd")
	prjA   = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	prjB   = types.ProjectID("prj_01m3250v57fhm9v1z9xd1wk8gw")
	grtA   = types.GrantID("grt_01m3250vkwffc979nzjw0g2sf9")
	grtB   = types.GrantID("grt_01m3250vnfeyrv67x6w0ddqr6a")
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func registry(t *testing.T) *vocab.Registry {
	t.Helper()
	reg, err := vocab.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func actor(id types.ActorID, kind types.ActorKind) types.Actor {
	return types.Actor{ID: id, Kind: kind, Name: "x", CreatedBy: opID}
}

func grant(id types.GrantID, projects types.ProjectScope, actions ...types.ActionKind) types.Grant {
	return types.Grant{ID: id, Actor: supID, Projects: projects, Actions: actions, ValidFrom: "2026-10-04T00:00:00Z",
		ExpiresAt: "2026-10-11T00:00:00Z", ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: opID}
}

func on(p ...types.ProjectID) types.ProjectScope { return types.ProjectScope{List: p} }

var everywhere = types.ProjectScope{All: true}

func ptr[T any](v T) *T { return &v }

func supRequest(action string, classes []string, grants ...types.Grant) Request {
	return Request{Actor: actor(supID, types.ActorKindSupervisor), Action: action, Project: ptr(prjA), Classes: classes, Grants: grants, Now: now}
}

func expect(t *testing.T, name string, d Decision, want Disposition, policy Policy, reason vocab.Reason) {
	t.Helper()
	if d.Disposition != want || (want == Propose && d.Policy != policy) || (want == Refuse && d.Reason != reason) {
		t.Errorf("%s: %+v", name, d)
	}
}

func TestRefusals(t *testing.T) {
	reg := registry(t)
	revoked := actor(opID, types.ActorKindOperator)
	revoked.RevokedAt = ptr(types.Time("2026-10-05T11:00:00Z"))
	expect(t, "a revoked operator", Decide(reg, Request{Actor: revoked, Action: "task.create", Now: now}), Refuse, None, vocab.ReasonActorRevoked)
	later := actor(opID, types.ActorKindOperator)
	later.RevokedAt = ptr(types.Time("2026-10-06T00:00:00Z"))
	expect(t, "a revocation still ahead", Decide(reg, Request{Actor: later, Action: "task.create", Now: now}), Allow, None, vocab.Reason{})
	op := actor(opID, types.ActorKindOperator)
	expect(t, "an unregistered action", Decide(reg, Request{Actor: op, Action: "repo.delete", Now: now}), Refuse, None, vocab.ReasonActionUnregistered)
	dep := registry(t)
	a := dep.Actions["task.create"]
	a.Deprecated = ptr(types.Semver("0.2.0"))
	dep.Actions["task.create"] = a
	expect(t, "a deprecated action", Decide(dep, Request{Actor: op, Action: "task.create", Now: now}), Refuse, None, vocab.ReasonActionUnregistered)
	expect(t, "an unknown class", Decide(reg, Request{Actor: op, Action: "profile.adopt", Classes: []string{"policy.make_coffee"}, Now: now}),
		Refuse, None, vocab.ReasonChangeClassUnknown)
	expect(t, "an unknown actor kind", Decide(reg, Request{Actor: actor(opID, "robot"), Action: "task.create", Now: now}),
		Refuse, None, vocab.ReasonActionNotGranted)
}

// D31: operators (and adcd's system actor) are unrestricted, except where a
// class binds every actor kind (KA1).
func TestOperatorsAndSystem(t *testing.T) {
	reg := registry(t)
	for _, kind := range []types.ActorKind{types.ActorKindOperator, types.ActorKindSystem} {
		r := Request{Actor: actor(opID, kind), Action: "profile.adopt", Classes: []string{"policy.widen_destinations"}, Now: now}
		expect(t, string(kind)+" widening destinations", Decide(reg, r), Allow, None, vocab.Reason{})
		r.Action, r.Classes = "profile.adopt", []string{"knowledge.export_framework_prose"}
		expect(t, string(kind)+" exporting framework prose", Decide(reg, r), Propose, Human, vocab.Reason{})
	}
}

func TestSupervisorGrants(t *testing.T) {
	reg := registry(t)
	g := grant(grtA, on(prjA), "task.create", "profile.adopt", "grant.create")
	d := Decide(reg, supRequest("task.create", nil, g))
	expect(t, "a covered action", d, Allow, None, vocab.Reason{})
	if d.Grant == nil || *d.Grant != grtA {
		t.Errorf("acts under %v", d.Grant)
	}
	expired := g
	expired.ExpiresAt = "2026-10-05T12:00:00Z" // expiry is exclusive
	early := g
	early.ValidFrom = "2026-10-05T12:00:01Z"
	other := g
	other.Actor = sup2ID
	noTask := grant(grtA, on(prjA), "profile.adopt")
	elsewhere := grant(grtA, on(prjB), "task.create")
	for name, r := range map[string]Request{
		"no grant":            supRequest("task.create", nil),
		"an expired grant":    supRequest("task.create", nil, expired),
		"a grant not yet due": supRequest("task.create", nil, early),
		"another's grant":     supRequest("task.create", nil, other),
		"an ungranted action": supRequest("task.create", nil, noTask),
		"another project":     supRequest("task.create", nil, elsewhere),
		"a revoked grant": func() Request {
			r := supRequest("task.create", nil, g)
			r.Revoked = map[types.GrantID]bool{grtA: true}
			return r
		}(),
		"no project under a listed grant": func() Request { r := supRequest("task.create", nil, g); r.Project = nil; return r }(),
	} {
		expect(t, name, Decide(reg, r), Refuse, None, vocab.ReasonActionNotGranted)
	}
	all := grant(grtA, everywhere, "task.create")
	r := supRequest("task.create", nil, all)
	r.Project = nil
	expect(t, "an every-project grant outside a project", Decide(reg, r), Allow, None, vocab.Reason{})

	// The class default applies; an override only tightens.
	expect(t, "a widening", Decide(reg, supRequest("profile.adopt", []string{"policy.widen_destinations"}, g)), Propose, Human, vocab.Reason{})
	tight := g
	tight.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"task.create": "independent_review"}
	expect(t, "a tightening override", Decide(reg, supRequest("task.create", nil, tight)), Propose, IndependentReview, vocab.Reason{})
	loose := g
	loose.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"profile.adopt": "independent_review"}
	expect(t, "an override weaker than the default", Decide(reg, supRequest("profile.adopt", []string{"policy.widen_data"}, loose)),
		Propose, Human, vocab.Reason{})
	expect(t, "two classes, the strongest", Decide(reg, supRequest("profile.adopt", []string{"install.self", "agent.enrol"}, g)),
		Propose, Human, vocab.Reason{})
	expect(t, "two classes, strongest first", Decide(reg, supRequest("profile.adopt", []string{"agent.enrol", "install.self"}, g)),
		Propose, Human, vocab.Reason{})
	// Several grants in force: the strictest override wins.
	strict := grant(grtB, on(prjA), "task.create")
	strict.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{"task.create": "human"}
	d = Decide(reg, supRequest("task.create", nil, g, strict))
	expect(t, "the strictest grant", d, Propose, Human, vocab.Reason{})
	if d.Grant == nil || *d.Grant != grtB {
		t.Errorf("the strictest grant: acts under %v", d.Grant)
	}
}

// A supervisor never creates or extends its own grant; for another
// supervisor it proposes, under human confirmation (D32).
func TestSelfGrant(t *testing.T) {
	reg := registry(t)
	g := grant(grtA, on(prjA), "grant.create", "grant.extend")
	for _, action := range []string{"grant.create", "grant.extend"} {
		self := supRequest(action, []string{action}, g)
		self.Grantee = ptr(supID)
		expect(t, action+" for itself", Decide(reg, self), Refuse, None, vocab.ReasonSelfGrantRefused)
		unnamed := supRequest(action, []string{action}, g)
		expect(t, action+" for nobody named", Decide(reg, unnamed), Refuse, None, vocab.ReasonSelfGrantRefused)
		peer := supRequest(action, []string{action}, g)
		peer.Grantee = ptr(sup2ID)
		expect(t, action+" for another supervisor", Decide(reg, peer), Propose, Human, vocab.Reason{})
	}
}
