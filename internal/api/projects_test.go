// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// project commits by's project.created for the M1 fixture profile under
// id, its repository URL carrying a credential.
func (f *fixture) project(by types.ActorID, id string) {
	raw, err := os.ReadFile("../../fixtures/m1/profiles/py-mini.json")
	if err != nil {
		f.t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		f.t.Fatal(err)
	}
	doc["id"], doc["repo"].(map[string]any)["url"] = id, "https://user:s3cret@example.com/py-mini.git"
	f.must(by, func(*eventstore.Tx) ([]eventstore.Event, error) {
		payload, err := json.Marshal(doc)
		return []eventstore.Event{{ID: newID(f.t, "evt"), Kind: authority.ProjectCreated, Subject: id, Project: &id, Payload: payload}}, err
	})
}

// Projects are read per project: a supervisor reading projects on A sees
// A, and not B; one reading other kinds sees none; the operator sees both.
// A project is sent with its revision and profile digest, never its
// profile (the repository URL may carry a credential).
func TestProjectsRead(t *testing.T) {
	f := open(t)
	f.grantOn([]string{prjA}, nil, "project")
	_, others := f.another([]string{prjA}, "note", "grant")
	f.project(f.op.ID, prjA)
	f.project(f.op.ID, prjB)
	srv := f.registeredServer(nil)
	sup := items(t, srv, "projects", f.supTk.Reveal())
	if !slices.Equal(keysOf(sup), []string{prjA}) {
		t.Fatalf("the supervisor read %v, want project A", keysOf(sup))
	}
	if got := items(t, srv, "projects", others); len(got) != 0 {
		t.Errorf("a supervisor reading no projects read %v", keysOf(got))
	}
	if want := sorted(prjA, prjB); !slices.Equal(keysOf(items(t, srv, "projects", f.opTok.Reveal())), want) {
		t.Errorf("the operator did not read every project")
	}
	if p := sup[prjA]; p["name"] != "py-mini" || p["revision"] != float64(1) || p["created_by"] != string(f.op.ID) ||
		!strings.HasPrefix(p["profile_digest"].(string), "sha256:") {
		t.Fatalf("the project as read: %v", p)
	}
	if _, b := read(t, srv, "/v1/projects", f.opTok.Reveal()); strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "profile\"") {
		t.Fatalf("the projects read sent the profile: %s", b)
	}
}
