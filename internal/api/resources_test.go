// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/api"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// registeredServer serves the registered resources, each List passed
// through wrap (nil: as registered).
func (f *fixture) registeredServer(wrap func(name string, rec map[string]any)) *httptest.Server {
	res := api.Registered()
	for name, r := range res {
		if wrap != nil {
			list := r.List
			r.List = func(tx *eventstore.Tx) ([]any, error) {
				recs, err := list(tx)
				for _, rec := range recs {
					wrap(name, rec.(map[string]any))
				}
				return recs, err
			}
		}
		res[name] = r
	}
	return f.serveReads(identity.SharedSocket, api.Reads{Gate: api.Unlimited(), Resources: res})
}

// items reads name as bearer: each item, checked against the resource's
// record schema (no field beyond it), by id.
func items(t *testing.T, srv *httptest.Server, name, bearer string) map[string]map[string]any {
	t.Helper()
	status, b := read(t, srv, "/v1/"+name, bearer)
	if status != http.StatusOK {
		t.Fatalf("%s: %d %s", name, status, b)
	}
	l, err := contract.Decode[types.ReadList]("api/read-list", b)
	if err != nil || l.Resource != name {
		t.Fatalf("%s: %v", b, err)
	}
	schema := strings.TrimSuffix(api.Registered()[name].Schema, ".json")
	out := map[string]map[string]any{}
	for _, it := range l.Items {
		if err := contract.Validate(schema, it); err != nil {
			t.Fatalf("%s: %s: %v", name, it, err)
		}
		var doc map[string]any
		_ = json.Unmarshal(it, &doc)
		out[doc["id"].(string)] = doc
	}
	return out
}

func keysOf(m map[string]map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

func sorted(v ...string) []string { return slices.Sorted(slices.Values(v)) }

// grantOn creates a grant for the supervisor on projects (none: every
// project), its reads listed (nil: same), reading kinds.
func (f *fixture) grantOn(projects []string, reads []string, kinds ...types.Name) types.Grant {
	return f.grantWith(nil, projects, reads, kinds...)
}

// grantWith is grantOn, the grant changed by edit (nil: none) before it is
// created.
func (f *fixture) grantWith(edit func(*types.Grant), projects []string, reads []string, kinds ...types.Name) types.Grant {
	g := f.grantFor(kinds...)
	g.Projects = types.ProjectScope{All: projects == nil}
	for _, p := range projects {
		g.Projects.List = append(g.Projects.List, types.ProjectID(p))
	}
	if reads != nil {
		g.Reads.Projects = types.GrantReadsProjects{}
		for _, p := range reads {
			g.Reads.Projects.List = append(g.Reads.Projects.List, types.ProjectID(p))
		}
	}
	if edit != nil {
		edit(&g)
	}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(g) })
	return g
}

// supervisor enrols another live supervisor.
func (f *fixture) supervisor() types.Actor {
	a := types.Actor{ID: types.ActorID(newID(f.t, "act")), Kind: types.ActorKindSupervisor, Name: "another supervisor", CreatedBy: f.op.ID}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.CreateActor(a) })
	return a
}

// A grant's project is every project it names: one is that project's,
// several or every project installation-wide. A supervisor reading grants
// on project A sees A's grants, revoked ones with their revocation, and
// nothing installation-wide; the operator sees all; no record carries its
// approval. Another supervisor's grant on A is A's record like any other;
// its grant on B is not. An expired grant stays listed.
func TestGrantsRead(t *testing.T) {
	f := open(t)
	reader := f.grantOn([]string{prjA}, nil, "grant")
	onB := f.grantOn([]string{prjB}, nil, "lease")
	every := f.grantOn(nil, nil, "lease")
	twoProjects := f.grantOn([]string{prjA, prjB}, nil, "lease")
	readsB := f.grantOn([]string{prjA}, []string{prjB}, "lease")
	readsA := f.grantOn([]string{prjA}, []string{prjA}, "lease")
	everyReadsA := f.grantOn(nil, []string{prjA}, "lease")
	revoked := f.grantOn([]string{prjA}, nil, "lease")
	expired := f.grantWith(func(g *types.Grant) {
		g.ValidFrom, g.ExpiresAt = types.Time(t0.Add(-2*time.Hour).Format(time.RFC3339)), types.Time(t0.Add(-time.Hour).Format(time.RFC3339))
	}, []string{prjA}, nil, "lease")
	other := f.supervisor()
	othersOnA := f.grantWith(func(g *types.Grant) { g.Actor = other.ID }, []string{prjA}, nil, "lease")
	othersOnB := f.grantWith(func(g *types.Grant) { g.Actor = other.ID }, []string{prjB}, nil, "lease")
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return authority.RevokeGrant(tx, f.sup.ID, revoked.ID, t0)
	})
	srv := f.registeredServer(nil)
	sup := items(t, srv, "grants", f.supTk.Reveal())
	if want := sorted(string(f.grant.ID), string(reader.ID), string(readsA.ID), string(revoked.ID), string(expired.ID), string(othersOnA.ID)); !slices.Equal(keysOf(sup), want) {
		t.Fatalf("the supervisor read %v, want project A's %v", keysOf(sup), want)
	}
	if sup[string(revoked.ID)]["revoked_at"] == nil || sup[string(reader.ID)]["revoked_at"] != nil || sup[string(othersOnA.ID)]["actor"] != string(other.ID) {
		t.Fatalf("revocation: %v", sup)
	}
	op := items(t, srv, "grants", f.opTok.Reveal())
	if want := sorted(string(f.grant.ID), string(reader.ID), string(onB.ID), string(every.ID), string(twoProjects.ID), string(readsB.ID),
		string(readsA.ID), string(everyReadsA.ID), string(revoked.ID), string(expired.ID), string(othersOnA.ID), string(othersOnB.ID)); !slices.Equal(keysOf(op), want) {
		t.Fatalf("the operator read %v, want %v", keysOf(op), want)
	}
	for id, g := range op {
		if _, ok := g["approval"]; ok || len(g) != len(api.Registered()["grants"].Fields) {
			t.Fatalf("grant %s beyond its allow-list: %v", id, g)
		}
	}
}

// propose commits by's proposal on project (none when empty) with scope,
// as the action handler would, and returns it.
func (f *fixture) propose(by types.ActorID, project, scope string) types.Proposal {
	return f.proposeWith(by, project, scope, time.Now().Add(-time.Minute), nil)
}

// proposeWith is propose, created at created and its binding changed by
// edit (nil: none).
func (f *fixture) proposeWith(by types.ActorID, project, scope string, created time.Time, edit func(*authority.Bound)) types.Proposal {
	var zero int64
	b := authority.Bound{Action: "grant.create", Object: types.ObjectRef{Type: "grant", ID: newID(f.t, "grt"), Version: &zero},
		ExpectedState: types.ExpectedState{Version: &zero}, Scope: json.RawMessage(scope), ProposedBy: by}
	if project != "" {
		p := types.ProjectID(project)
		b.Project = &p
	}
	if edit != nil {
		edit(&b)
	}
	p, err := authority.NewProposal(types.ProposalID(newID(f.t, "prp")), authority.Decision{Disposition: authority.Propose, Policy: authority.Human,
		Classes: []string{"grant.create"}}, b, created, created.Add(authority.ProposalTTL))
	if err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	f.must(by, func(*eventstore.Tx) ([]eventstore.Event, error) {
		e := eventstore.Event{ID: newID(f.t, "evt"), Kind: authority.ProposalCreated, Subject: string(p.ID), Project: (*string)(p.Project), Payload: raw}
		return []eventstore.Event{e}, nil
	})
	return p
}

// A proposal's project is the one derived when it was proposed, never one
// its scope (the client's input) names: a supervisor reading proposals on
// A sees A's, whatever their scopes say, and no other project's nor any
// outside a project. No record carries its scope or binding digest; the
// approvals kept beside the proposals are no proposal records.
func TestProposalsRead(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "proposal")
	const secret = "s3cret-in-the-scope"
	onA := f.propose(f.sup.ID, prjA, `{"project":"`+prjB+`","token":"`+secret+`"}`)
	onB := f.propose(f.op.ID, prjB, `{"project":"`+prjA+`"}`)
	outside := f.propose(f.op.ID, "", `{"project":"`+prjA+`"}`)
	chl, evt := types.ChallengeID(newID(t, "chl")), types.EventID(newID(t, "evt"))
	a := authority.ApprovalOf(types.Approval{ID: types.ApprovalID(newID(t, "apr")), Confirmation: types.ApprovalConfirmation{Challenge: &chl,
		SatisfiedBy: []types.EventID{evt}}, Answers: []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: "approved",
		At: types.Time(t0.Format(time.RFC3339))}, onB, f.op)
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.ApprovalEvent(a) })
	srv := f.registeredServer(nil)
	if got := items(t, srv, "proposals", f.supTk.Reveal()); !slices.Equal(keysOf(got), []string{string(onA.ID)}) {
		t.Fatalf("the supervisor read %v, want only project A's %s", keysOf(got), onA.ID)
	}
	op := items(t, srv, "proposals", f.opTok.Reveal())
	if want := sorted(string(onA.ID), string(onB.ID), string(outside.ID)); !slices.Equal(keysOf(op), want) {
		t.Fatalf("the operator read %v, want %v", keysOf(op), want)
	}
	if p := op[string(onA.ID)]; p["project"] != prjA || p["status"] != "open" || p["proposed_by"] != string(f.sup.ID) {
		t.Fatalf("the proposal as read: %v", p)
	}
	_, raw := read(t, srv, "/v1/proposals", f.opTok.Reveal())
	for _, leak := range []string{secret, `"scope"`, `"binding_digest"`, `"expected_state"`, string(onA.BindingDigest)} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the read carries %s: %s", leak, raw)
		}
	}
}

// moved commits p moved to status by a kind event, as the confirm path does.
func (f *fixture) moved(p types.Proposal, kind string, status types.ProposalStatus) types.Proposal {
	p.Status = status
	raw, _ := json.Marshal(p)
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: newID(f.t, "evt"), Kind: kind, Subject: string(p.ID), Project: (*string)(p.Project), Payload: raw}}, nil
	})
	return p
}

// approve records the operator's approval of p.
func (f *fixture) approve(p types.Proposal) {
	chl, evt := types.ChallengeID(newID(f.t, "chl")), types.EventID(newID(f.t, "evt"))
	a := authority.ApprovalOf(types.Approval{ID: types.ApprovalID(newID(f.t, "apr")), Confirmation: types.ApprovalConfirmation{Challenge: &chl,
		SatisfiedBy: []types.EventID{evt}}, Answers: []types.OwedInputID{}, Supersedes: []types.ApprovalID{}, Reason: "approved",
		At: types.Time(t0.Format(time.RFC3339))}, p, f.op)
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.ApprovalEvent(a) })
}

// Proposals are listed in every state main reaches (open, confirmed,
// applied, expired, invalidated), each with its status.
func TestProposalsReadEveryState(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "proposal")
	want := map[string]string{}
	pending := f.propose(f.sup.ID, prjA, `{}`)
	want[string(pending.ID)] = "open"
	confirmed := f.propose(f.sup.ID, prjA, `{"n":1}`)
	f.approve(confirmed)
	f.moved(confirmed, authority.ProposalConfirmed, types.ProposalStatusConfirmed)
	want[string(confirmed.ID)] = "confirmed"
	applied := f.propose(f.sup.ID, prjA, `{"n":2}`)
	f.approve(applied)
	f.moved(f.moved(applied, authority.ProposalConfirmed, types.ProposalStatusConfirmed), authority.ProposalApplied, types.ProposalStatusApplied)
	want[string(applied.ID)] = "applied"
	expired := f.proposeWith(f.sup.ID, prjA, `{"n":3}`, time.Now().Add(-200*time.Hour), nil)
	f.moved(expired, authority.ProposalExpired, types.ProposalStatusExpired)
	want[string(expired.ID)] = "expired"
	stale := f.propose(f.sup.ID, prjA, `{"n":4}`)
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return []eventstore.Event{{ID: newID(t, "evt"), Kind: "fixture.appended/v1", Subject: stale.Object.ID, Payload: []byte(`{}`)}}, nil
	})
	want[string(stale.ID)] = "invalidated"
	got := items(t, f.registeredServer(nil), "proposals", f.supTk.Reveal())
	for id, status := range want {
		if got[id]["status"] != status {
			t.Errorf("%s: %v, want %s", id, got[id], status)
		}
	}
	if got[string(stale.ID)]["invalidated_reason"] != "stale_expected_state" || len(got) != len(want) {
		t.Errorf("read %v", got)
	}
}

// A proposal to confirm another proposal binds that proposal as its
// object, whose content digest covers the scope a read withholds: the
// object is read without its digest, and nothing that depends on the
// withheld scope is sent.
func TestProposalObjectCarriesNoDigest(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "proposal")
	const pin = "4821"
	target := f.propose(f.op.ID, prjA, `{"pin":"`+pin+`"}`)
	raw, _ := json.Marshal(target)
	digest, err := eventstore.ContentDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	version := int64(1)
	confirm := f.proposeWith(f.sup.ID, prjA, `{"proposal":"`+string(target.ID)+`"}`, time.Now().Add(-time.Minute), func(b *authority.Bound) {
		b.Action = "proposal.confirm"
		b.Object = types.ObjectRef{Type: "proposal", ID: string(target.ID), Version: &version, Digest: (*types.Digest)(&digest)}
		b.ExpectedState = types.ExpectedState{Version: &version, Digest: (*types.Digest)(&digest)}
	})
	srv := f.registeredServer(nil)
	_, body := read(t, srv, "/v1/proposals", f.supTk.Reveal())
	for _, leak := range []string{pin, digest, string(target.BindingDigest), string(confirm.BindingDigest), `"digest"`} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the read carries %s: %s", leak, body)
		}
	}
	obj, _ := items(t, srv, "proposals", f.supTk.Reveal())[string(confirm.ID)]["object"].(map[string]any)
	if obj["id"] != string(target.ID) || obj["type"] != "proposal" || obj["version"] != float64(1) || len(obj) != 3 {
		t.Fatalf("the confirm proposal's object: %v", obj)
	}
}

// Who reads a record is decided from its projection, never from the
// listed document: a document that claims project A, or carries a field
// its resource never sends, changes nothing that is sent.
func TestReadsDecideFromTheProjection(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "grant", "proposal")
	onB := f.grantOn([]string{prjB}, nil, "lease")
	every := f.grantOn(nil, nil, "lease")
	pB := f.propose(f.op.ID, prjB, `{}`)
	srv := f.registeredServer(func(_ string, rec map[string]any) {
		rec["project"], rec["projects"], rec["added"] = prjA, []any{prjA}, "a-new-field"
	})
	for name, hidden := range map[string][]string{"grants": {string(onB.ID), string(every.ID)}, "proposals": {string(pB.ID)}} {
		got := items(t, srv, name, f.supTk.Reveal())
		for _, id := range hidden {
			if _, ok := got[id]; ok {
				t.Errorf("%s: %s read as its document claims", name, id)
			}
		}
		for id, rec := range items(t, srv, name, f.opTok.Reveal()) {
			if _, ok := rec["added"]; ok {
				t.Errorf("%s: %s carries a field beyond its allow-list", name, id)
			}
		}
	}
}
