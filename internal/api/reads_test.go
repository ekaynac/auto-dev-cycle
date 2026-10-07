// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

// fixtures is the test-only resource: every fixture.appended event, read
// from the snapshot, its project the event's; its subject is never sent.
var fixtures = api.Resource{
	Domain: "fixture",
	List: func(tx *eventstore.Tx) ([]any, error) {
		evs, err := tx.EventsAfter(0, 1000)
		var out []any
		for _, e := range evs {
			if e.Kind == "fixture.appended/v1" {
				out = append(out, map[string]any{"id": e.ID, "project": e.Project, "subject": e.Subject})
			}
		}
		return out, err
	},
	Project: func(_ *eventstore.Tx, rec any) (*types.ProjectID, error) {
		p, _ := rec.(map[string]any)["project"].(*string)
		if p == nil {
			return nil, nil
		}
		id := types.ProjectID(*p)
		return &id, nil
	},
	Fields: []string{"id", "project"},
}

// readServer serves the reads over f's store, every connection admitted
// as via says (identity.Listener(0): no peer recorded).
func (f *fixture) readServer(via identity.Listener) *httptest.Server {
	h := api.Reads{Store: f.s, Gate: api.Unlimited(), Now: func() time.Time { return time.Unix(0, f.now.Load()) },
		Resources: map[string]api.Resource{"fixtures": fixtures}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.WhoamiPath, h.Whoami)
	mux.HandleFunc("GET "+api.ReadPath, h.Resource)
	srv := httptest.NewUnstartedServer(mux)
	if via != 0 {
		srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
			return api.WithPeer(ctx, api.Peer{Via: via, UID: 1000})
		}
	}
	srv.Start()
	f.t.Cleanup(srv.Close)
	return srv
}

func read(t *testing.T, srv *httptest.Server, path, bearer string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// readList reads the fixtures as bearer: the ids returned, every item
// checked against the response schema and the allow-list.
func readList(t *testing.T, srv *httptest.Server, bearer string) []string {
	t.Helper()
	status, b := read(t, srv, "/v1/fixtures", bearer)
	if status != http.StatusOK {
		t.Fatalf("read: %d %s", status, b)
	}
	l, err := contract.Decode[types.ReadList]("api/read-list", b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	ids := []string{}
	for _, it := range l.Items {
		var doc map[string]any
		_ = json.Unmarshal(it, &doc)
		if _, leaked := doc["subject"]; leaked || len(doc) != 2 {
			t.Fatalf("an item beyond its allow-list: %s", it)
		}
		ids = append(ids, doc["id"].(string))
	}
	if (l.Outcome == types.ReadListOutcomeFound) != (len(ids) > 0) {
		t.Fatalf("outcome %s for %d items", l.Outcome, len(ids))
	}
	return ids
}

// fixtureIDs lists the fixture events by project ("" outside any).
func (f *fixture) fixtureIDs() map[string]string {
	out := map[string]string{}
	f.view(func(tx *eventstore.Tx) {
		evs, _ := tx.EventsAfter(0, 1000)
		for _, e := range evs {
			if e.Kind == "fixture.appended/v1" {
				p := ""
				if e.Project != nil {
					p = *e.Project
				}
				out[p] = e.ID
			}
		}
	})
	return out
}

func TestReadRefusals(t *testing.T) {
	f := open(t)
	srv := f.readServer(identity.PrivateSocket)
	for name, c := range map[string]struct {
		path, bearer string
		status       int
	}{
		"no bearer":                 {"/v1/fixtures", "", http.StatusUnauthorized},
		"a wrong bearer":            {"/v1/fixtures", f.supTk.Reveal() + "x", http.StatusUnauthorized},
		"no bearer for whoami":      {api.WhoamiPath, "", http.StatusUnauthorized},
		"an unknown resource":       {"/v1/widgets", f.opTok.Reveal(), http.StatusNotFound},
		"a client filter":           {"/v1/fixtures?project=" + prjB, f.supTk.Reveal(), http.StatusBadRequest},
		"a client filter on whoami": {api.WhoamiPath + "?all=1", f.supTk.Reveal(), http.StatusBadRequest},
	} {
		if status, b := read(t, srv, c.path, c.bearer); status != c.status || strings.Contains(string(b), f.supTk.Reveal()) {
			t.Errorf("%s: %d %s, want %d", name, status, b, c.status)
		}
	}
	// No listener recorded is no listener: nothing authenticates.
	if status, _ := read(t, f.readServer(0), "/v1/fixtures", f.opTok.Reveal()); status != http.StatusUnauthorized {
		t.Errorf("no listener: %d", status)
	}
}

// Every record is authorized by its domain and project: a supervisor reads
// its granted project's records, never another project's, and a record
// outside any project only under an every-project read; an operator reads
// all.
func TestReadAuthorizesEachRecord(t *testing.T) {
	f := open(t)
	f.appendTo("fixture.appended", prjA)
	f.appendTo("fixture.appended", prjB)
	f.appendTo("fixture.appended", "")
	ids := f.fixtureIDs()
	srv := f.readServer(identity.SharedSocket)
	if got := readList(t, srv, f.supTk.Reveal()); !slices.Equal(got, []string{ids[prjA]}) {
		t.Fatalf("the supervisor read %v, want only project A's %s", got, ids[prjA])
	}
	if got := readList(t, srv, f.opTok.Reveal()); len(got) != 3 {
		t.Fatalf("the operator read %v", got)
	}
	// Another domain's grant on every project reads none of them.
	other := f.grantFor("lease")
	other.Projects = types.ProjectScope{All: true}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(other) })
	if got := readList(t, srv, f.supTk.Reveal()); !slices.Equal(got, []string{ids[prjA]}) {
		t.Fatalf("under another domain's grant: %v", got)
	}
	global := f.grantFor("fixture")
	global.Projects = types.ProjectScope{All: true}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(global) })
	if got := readList(t, srv, f.supTk.Reveal()); len(got) != 3 {
		t.Fatalf("under an every-project grant: %v", got)
	}
}

// The scope is taken with the records: a grant revoked between two reads
// changes the next one.
func TestReadScopeIsTheSnapshots(t *testing.T) {
	f := open(t)
	f.appendTo("fixture.appended", prjA)
	srv := f.readServer(identity.PrivateSocket)
	if got := readList(t, srv, f.supTk.Reveal()); len(got) != 1 {
		t.Fatalf("before the revocation: %v", got)
	}
	f.revokeGrant()
	if status, b := read(t, srv, "/v1/fixtures", f.supTk.Reveal()); status != http.StatusForbidden || !strings.Contains(string(b), "action_not_granted") {
		t.Fatalf("after the revocation: %d %s", status, b)
	}
	narrow := f.grantFor("fixture")
	narrow.Projects = types.ProjectScope{List: []types.ProjectID{types.ProjectID(prjB)}}
	f.must(f.op.ID, func(*eventstore.Tx) ([]eventstore.Event, error) { return authority.CreateGrant(narrow) })
	if got := readList(t, srv, f.supTk.Reveal()); len(got) != 0 {
		t.Fatalf("under a grant on another project: %v", got)
	}
}

func whoami(t *testing.T, srv *httptest.Server, bearer string) (types.Whoami, string) {
	t.Helper()
	status, b := read(t, srv, api.WhoamiPath, bearer)
	if status != http.StatusOK {
		t.Fatalf("whoami: %d %s", status, b)
	}
	w, err := contract.Decode[types.Whoami]("api/whoami", b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return w, string(b)
}

func TestWhoami(t *testing.T) {
	f := open(t)
	sup, raw := whoami(t, f.readServer(identity.SharedSocket), f.supTk.Reveal())
	tok, secret, _ := strings.Cut(f.supTk.Reveal(), ".")
	if sup.Actor != f.sup.ID || sup.Kind != types.WhoamiKindSupervisor || string(sup.Token) != tok || sup.Listener != types.WhoamiListenerSharedSocket ||
		sup.Reads.Everything || len(sup.Reads.Grants) != 1 || strings.Contains(raw, secret) || strings.Contains(raw, "argon2") {
		t.Fatalf("the supervisor's whoami: %s", raw)
	}
	g := sup.Reads.Grants[0]
	if g.Grant != f.grant.ID || g.Projects.All || !slices.Equal(g.Projects.List, []types.ProjectID{types.ProjectID(prjA)}) || !slices.Equal(g.Kinds, []types.Name{"fixture"}) {
		t.Fatalf("the grant's reach (same projects resolved): %s", raw)
	}
	op, raw := whoami(t, f.readServer(identity.PrivateSocket), f.opTok.Reveal())
	if op.Actor != f.op.ID || op.Kind != types.WhoamiKindOperator || op.Listener != types.WhoamiListenerPrivateSocket || !op.Reads.Everything || len(op.Reads.Grants) != 0 {
		t.Fatalf("the operator's whoami: %s", raw)
	}
	// whoami is no grant domain: a supervisor without a grant still reads its own.
	f.revokeGrant()
	if sup, raw := whoami(t, f.readServer(identity.PrivateSocket), f.supTk.Reveal()); len(sup.Reads.Grants) != 0 || sup.Reads.Everything {
		t.Fatalf("after the revocation: %s", raw)
	}
}
