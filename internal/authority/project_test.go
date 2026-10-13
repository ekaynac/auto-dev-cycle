// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/profile"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// pyMini is the M1 fixture profile (fixtures/m1/profiles/py-mini.json).
const pyMini = "../../fixtures/m1/profiles/py-mini.json"

// openProjects is the boundary with the production project.create handler.
func openProjects(t *testing.T, seated bool) *boundary {
	b := openBoundaryGrace(t, 0, seated)
	b.az.Handlers[authority.ProjectCreateAction] = authority.ProjectCreateHandler{}
	return b
}

// profileDoc is the fixture profile as a document, edited by edit.
func profileDoc(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(pyMini)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(doc)
	}
	return must(json.Marshal(doc))
}

func withID(id types.ProjectID) func(map[string]any) { return func(d map[string]any) { d["id"] = id } }

func (b *boundary) createCmd(p identity.Principal, key string, raw []byte) eventstore.Command {
	return p.Command(newID(b.t, "cmd"), authority.ProjectCreateAction, key, raw, nil)
}

// createProject submits c with project.create's logic; ran reports whether
// the logic ran.
func (b *boundary) createProject(c eventstore.Command) (out eventstore.Committed, ran bool, err error) {
	out, err = b.s.Submit(context.Background(), c, func(tx *eventstore.Tx) (eventstore.Decision, error) {
		ran = true
		return authority.CreateProject(c)(tx)
	})
	return out, ran, err
}

// trySeed is seed, returning the writer's error.
func (b *boundary) trySeed(p identity.Principal, apply applyFn) error {
	cmd := newID(b.t, "cmd")
	_, err := b.s.SubmitApplied(context.Background(), p.Command(cmd, "test.seed", cmd, []byte(`{}`), nil), eventstore.Applies(
		func(tx *eventstore.Tx) ([]eventstore.Event, json.RawMessage, error) {
			evs, err := apply(tx)
			return evs, json.RawMessage(`{}`), err
		}))
	return err
}

func (b *boundary) storedProject(id types.ProjectID) (r authority.ProjectRecord, ok bool) {
	b.t.Helper()
	b.view(func(tx *eventstore.Tx) error {
		raw, found, err := tx.Get(authority.ProjectProjection, id)
		if err != nil || !found {
			return err
		}
		ok = true
		return json.Unmarshal(raw, &r)
	})
	return r, ok
}

// The fixture is a complete project profile: its schema and profile.Check
// pass it, and it holds what the M1 stub cycle runs py-mini with.
func TestPyMiniProfileFixture(t *testing.T) {
	raw, err := os.ReadFile(pyMini)
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(profile.Schema, raw); err != nil {
		t.Fatalf("the schema: %v", err)
	}
	if err := contract.Validate(authority.ProjectCreateScope, raw); err != nil {
		t.Fatalf("the project.create scope: %v", err)
	}
	p, err := profile.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Check(p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "py-mini" || p.Repo.BaseRef != "refs/heads/main" || p.Forge.Kind != "git" || p.Gate.ResultAdapter != "junit_xml" ||
		len(p.Gate.Environments) != 1 || p.Gate.Environments[0].Facts.Network != "none" || len(p.Toolchain.Images) != 1 ||
		p.Integration.Plugin != "none" || p.Integration.Topology != nil || p.RuntimeVerification.Command != nil {
		t.Fatalf("the fixture: %+v", p)
	}
	if !slices.Equal(p.ProtectedPaths, []string{"conftest.py", "pyproject.toml", "requirements-dev.txt"}) || p.Plugins.Records != "json_ledger" {
		t.Fatalf("the fixture's protected paths %v, records %s", p.ProtectedPaths, p.Plugins.Records)
	}
}

// An operator registers py-mini at --expect 0: one project.created event,
// in the project, and a record with revision 1, the profile's content
// digest and the profile as sent; a repeat replays; a second create is
// stale; the projects rebuild.
func TestProjectCreate(t *testing.T) {
	b := openProjects(t, true)
	raw := profileDoc(t, nil)
	p, err := profile.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := b.createCmd(b.op, "k", raw)
	c.Expect = version(0)
	out, ran, err := b.createProject(c)
	if err != nil || !ran || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the create: %+v %v", out, err)
	}
	dg, err := eventstore.ContentDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Project       types.ProjectID `json:"project"`
		Revision      int64           `json:"revision"`
		ProfileDigest string          `json:"profile_digest"`
	}
	if err := json.Unmarshal(out.Result, &res); err != nil || res.Project != p.ID || res.Revision != 1 || res.ProfileDigest != dg {
		t.Fatalf("the result %s: %v", out.Result, err)
	}
	ev := b.lastEvent()
	if ev.Kind != authority.ProjectCreated || ev.Subject != p.ID || ev.Project == nil || *ev.Project != p.ID || ev.Actor != string(b.op.Actor()) {
		t.Fatalf("the event %+v", ev)
	}
	r, ok := b.storedProject(p.ID)
	if !ok || r.ID != p.ID || r.Name != "py-mini" || r.Revision != 1 || r.ProfileDigest != dg || r.CreatedBy != b.op.Actor() || r.CreatedAt != ev.At {
		t.Fatalf("the record %+v", r)
	}
	if got, err := eventstore.ContentDigest(r.Profile); err != nil || got != dg {
		t.Fatalf("the stored profile's digest %s: %v", got, err)
	}
	h := b.head()
	repeat := b.createCmd(b.op, "k", raw)
	repeat.Expect = version(0)
	again, _, err := b.createProject(repeat)
	if err != nil || !again.Replayed || again.CommandID != out.CommandID || b.head() != h {
		t.Fatalf("the repeat: %+v %v", again, err)
	}
	for name, x := range map[string]*eventstore.State{"at --expect 0": version(0), "with no expectation": nil} {
		dup := b.createCmd(b.op, "dup "+name, raw)
		dup.Expect = x
		if _, _, err := b.createProject(dup); !errors.Is(err, eventstore.ErrStaleExpectedState) || b.head() != h {
			t.Errorf("a second create %s: %v", name, err)
		}
	}
	b.rebuilds()
}

// An invalid profile is refused before authority and the logic, with no
// event: one its schema refuses, and one only profile.Check refuses.
func TestProjectCreateRefusesInvalidProfiles(t *testing.T) {
	b := openProjects(t, true)
	for name, edit := range map[string]func(map[string]any){
		"no gate":        func(d map[string]any) { delete(d, "gate") },
		"an unknown key": func(d map[string]any) { d["owner"] = "me" },
		"not a project":  withID(types.ProjectID(newID(t, "tsk"))),
		"a bad base ref": func(d map[string]any) { d["repo"].(map[string]any)["base_ref"] = "refs/heads/a..b" },
		"an unknown env": func(d map[string]any) {
			d["runtime_verification"] = map[string]any{"command": []any{"x"}, "facts_from": "prod"}
		},
		"a NUL in a argv": func(d map[string]any) { d["gate"].(map[string]any)["command"] = []any{"pytest\x00"} },
	} {
		h := b.head()
		_, ran, err := b.createProject(b.createCmd(b.op, name, profileDoc(t, edit)))
		if !errors.Is(err, authority.ErrInvalidScope) || ran || b.head() != h {
			t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
		}
	}
	// Not JSON at all, and a YAML profile (the scope is JSON).
	for name, raw := range map[string][]byte{"not JSON": []byte("{"), "YAML": []byte("schema: adc.project-profile/v1\n")} {
		h := b.head()
		if _, ran, err := b.createProject(b.createCmd(b.op, name, raw)); err == nil || ran || b.head() != h {
			t.Errorf("%s: %v (logic ran: %t)", name, err, ran)
		}
	}
	// Only operators and supervisors create projects.
	if _, ran, err := b.createProject(b.createCmd(b.sys, "sys", profileDoc(t, nil))); !isReason(vocab.ReasonActorKindRefused)(err) || ran {
		t.Errorf("the system: %v", err)
	}
}

// F (P1): a supervisor's create with no covering grant is refused
// action_not_granted, with no event and no project: a grant on the project
// for another action, and a grant for project.create on another project.
// The same supervisor creates the project once a grant covers it.
func TestProjectCreateNeedsAGrant(t *testing.T) {
	b := openProjects(t, true)
	// The operator's prjB exists: an ungranted create of it is refused
	// action_not_granted, never stale, so it learns nothing of prjB.
	if out, _, err := b.createProject(b.createCmd(b.op, "prjB", profileDoc(t, withID(prjB)))); err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the operator's prjB: %+v %v", out, err)
	}
	// b.supGrant covers note.add on prjA only.
	for name, id := range map[string]types.ProjectID{"another action on the project": prjA, "no grant on the existing project": prjB} {
		for xname, x := range map[string]*eventstore.State{"--expect 0": version(0), "no expectation": nil} {
			h := b.head()
			c := b.createCmd(b.sup, name+xname, profileDoc(t, withID(id)))
			c.Expect = x
			_, ran, err := b.createProject(c)
			if !isReason(vocab.ReasonActionNotGranted)(err) || ran || b.head() != h {
				t.Errorf("%s, %s: %v (logic ran: %t)", name, xname, err, ran)
			}
		}
	}
	if _, ok := b.storedProject(prjA); ok {
		t.Error("prjA was recorded")
	}
	if r, _ := b.storedProject(prjB); r.CreatedBy != b.op.Actor() {
		t.Errorf("prjB was changed: %+v", r)
	}
	g := b.supGrant
	g.ID, g.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.ProjectCreateAction}
	b.seed(b.op, createGrant(g))
	h := b.head()
	if _, ran, err := b.createProject(b.createCmd(b.sup, "other project", profileDoc(t, withID(prjB)))); !isReason(vocab.ReasonActionNotGranted)(err) || ran || b.head() != h {
		t.Errorf("a grant on another project: %v (logic ran: %t)", err, ran)
	}
	out, _, err := b.createProject(b.createCmd(b.sup, "granted", profileDoc(t, withID(prjA))))
	if err != nil || out.Disposition != eventstore.DispositionApplied {
		t.Fatalf("the granted create: %+v %v", out, err)
	}
	if r, ok := b.storedProject(prjA); !ok || r.CreatedBy != b.sup.Actor() {
		t.Fatalf("the granted project %+v", r)
	}
	// Without the seat, a granted supervisor is refused too.
	unseated := openProjects(t, false)
	unseated.seed(unseated.op, createGrant(types.Grant{ID: types.GrantID(newID(t, "grt")), Actor: unseated.sup.Actor(), Projects: unseated.supGrant.Projects,
		Actions: []types.ActionKind{authority.ProjectCreateAction}, Reads: unseated.supGrant.Reads, ValidFrom: unseated.supGrant.ValidFrom,
		ExpiresAt: unseated.supGrant.ExpiresAt, ConfirmationOverrides: map[string]types.GrantConfirmationOverridesValue{}, IssuedBy: unseated.op.Actor()}))
	if _, ran, err := unseated.createProject(unseated.createCmd(unseated.sup, "unseated", profileDoc(t, withID(prjA)))); !isReason(vocab.ReasonSeatNotHeld)(err) || ran {
		t.Errorf("no seat: %v", err)
	}
	b.rebuilds()
}

// A grant tightening project.create to a review would make it a proposal,
// which M1 can never apply: it is refused, with no proposal and no project.
func TestProjectCreateIsNeverProposed(t *testing.T) {
	b := openProjects(t, true)
	tight := b.supGrant
	tight.ID, tight.Actions = types.GrantID(newID(t, "grt")), []types.ActionKind{authority.ProjectCreateAction}
	tight.ConfirmationOverrides = map[string]types.GrantConfirmationOverridesValue{authority.ProjectCreateAction: "independent_review"}
	b.seed(b.op, createGrant(tight))
	_, ran, err := b.createProject(b.createCmd(b.sup, "tight", profileDoc(t, withID(prjA))))
	var r *authority.Refusal
	if !isReason(vocab.ReasonActionNotProposable)(err) || ran || !errors.As(err, &r) ||
		r.Detail != "project.create has no Applier in M1, so a proposal of it could never be applied" {
		t.Errorf("a tightened create: %v", err)
	}
	if _, ok := b.storedProject(prjA); ok {
		t.Error("the project was recorded")
	}
	b.view(func(tx *eventstore.Tx) error {
		if keys, err := tx.Keys(authority.ProposalProjection, ""); err != nil || len(keys) != 0 {
			t.Errorf("a proposal was recorded: %v %v", keys, err)
		}
		return nil
	})
}

// The logic is project.create's alone, writes only under the derived id,
// and the fold refuses an event its command could not have produced.
func TestProjectCreateLogicAndFold(t *testing.T) {
	b := openProjects(t, true)
	raw := profileDoc(t, nil)
	b.view(func(tx *eventstore.Tx) error {
		if _, err := authority.CreateProject(b.createCmd(b.op, "x", raw))(tx); err == nil || !strings.Contains(err.Error(), "no derived") {
			t.Errorf("the logic ran without a derivation: %v", err)
		}
		return nil
	})
	// A profile event under another subject is refused by the fold.
	h := b.head()
	err := b.trySeed(b.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		p := string(prjA)
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: string(prjB), Project: &p, Payload: raw}}, nil
	})
	if err == nil || b.head() != h {
		t.Errorf("a misfiled project event was admitted: %v", err)
	}
	created := func(payload []byte) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			var p struct{ ID string }
			_ = json.Unmarshal(payload, &p)
			return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: p.ID, Project: &p.ID, Payload: payload}}, nil
		}
	}
	// One by the system is refused by the fold.
	if err := b.trySeed(b.sys, created(raw)); err == nil || b.head() != h {
		t.Errorf("by the system: admitted: %v", err)
	}
	// A duplicate project.created is refused.
	b.seed(b.op, created(raw))
	h = b.head()
	if err := b.trySeed(b.op, created(raw)); err == nil || !strings.Contains(err.Error(), "the project exists") || b.head() != h {
		t.Errorf("a duplicate project.created was admitted: %v", err)
	}
}

// project.created/v1 events already stored must keep replaying: a golden
// payload, frozen apart from the fixture, is admitted and rebuilt. A later
// change to the profile schema or profile.Check that refuses it fails here;
// such a change needs project.created/v2.
func TestProjectCreatedV1Replays(t *testing.T) {
	golden, err := os.ReadFile("testdata/project-created-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	b := openProjects(t, true)
	id := "prj_01m3250vkwffc979nzjw0gpym1"
	b.seed(b.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: id, Project: &id, Payload: golden}}, nil
	})
	if r, ok := b.storedProject(id); !ok || r.Name != "py-mini" || r.Revision != 1 {
		t.Fatalf("the golden project %+v", r)
	}
	b.rebuilds()
}

// Replay never re-runs profile.Load: a stored v1 payload that today's
// profile.Check refuses (a well-formed profile with a bad base ref, as a
// later tightening would leave behind) still replays to its record.
func TestProjectCreatedReplaysWithoutProfileCheck(t *testing.T) {
	b := openProjects(t, true)
	bad := profileDoc(t, func(d map[string]any) { d["repo"].(map[string]any)["base_ref"] = "refs/heads/a..b" })
	if _, err := profile.Load(bad); err == nil {
		t.Fatal("profile.Load admits the payload; it proves nothing")
	}
	id := "prj_01m3250vkwffc979nzjw0gpym1"
	b.seed(b.op, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: authority.ProjectCreated, Subject: id, Project: &id, Payload: bad}}, nil
	})
	if r, ok := b.storedProject(id); !ok || r.Name != "py-mini" || r.Revision != 1 {
		t.Fatalf("the stored project %+v", r)
	}
	b.rebuilds()
}
