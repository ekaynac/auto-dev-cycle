// SPDX-License-Identifier: Apache-2.0

package brief

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/knowledge"
)

var (
	task = types.Task{ID: "tsk_01m3250v9xft08ssr4ygq6g2np", Project: "prj_01m3250vkwffc979nzjw0g2sa1",
		Spec: types.TaskCreateScope{Project: "prj_01m3250vkwffc979nzjw0g2sa1", Title: "add a flag", Requirement: "the CLI takes --verbose",
			Attachments: []types.Attachment{{Name: "accept_test.go", Digest: "sha256:" + strings.Repeat("a", 64)}}},
		SpecDigest: "sha256:" + strings.Repeat("b", 64),
		Acceptance: []types.AcceptanceBlob{{Name: "accept_test.go", Digest: "sha256:" + strings.Repeat("a", 64), Class: types.AcceptanceBlobClassRestricted}}}
	policy = types.Digest("sha256:" + strings.Repeat("d", 64))
)

// A task's brief holds its spec's title, requirement and digest, and its
// task, never its acceptance; its bytes are canonical, so one task's spec
// has one brief digest, and another task, another spec or another
// requirement has another (intent's walk binds the digest, Opus 22a-5c-3
// H1), and so has another data policy (r2 M1: a policy change is a new
// brief id).
func TestTheBrief(t *testing.T) {
	b, d, err := Of(task, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(d), "sha256:") || !bytes.Contains(b, []byte("the CLI takes --verbose")) || !bytes.Contains(b, []byte(task.SpecDigest)) ||
		!bytes.Contains(b, []byte(policy)) ||
		bytes.Contains(b, []byte("accept_test.go")) || bytes.Contains(b, []byte(strings.Repeat("a", 64))) {
		t.Fatalf("the brief: %s (%s)", b, d)
	}
	if again, d2, _ := Of(task, policy); !bytes.Equal(again, b) || d2 != d {
		t.Error("the brief is not deterministic")
	}
	edits := map[string]func(*types.Task){
		"another task":        func(x *types.Task) { x.ID = "tsk_01m3250v9xft08ssr4ygq6g2nq" },
		"another spec":        func(x *types.Task) { x.SpecDigest = "sha256:" + types.Digest(strings.Repeat("c", 64)) },
		"another requirement": func(x *types.Task) { x.Spec.Requirement = "CANARY-ACCEPTANCE" },
	}
	for name, edit := range edits {
		other := task
		edit(&other)
		if _, od, err := Of(other, policy); err != nil || od == d {
			t.Errorf("%s: the same digest (%v)", name, err)
		}
	}
	if _, od, err := Of(task, "sha256:"+types.Digest(strings.Repeat("e", 64))); err != nil || od == d {
		t.Errorf("another policy: the same digest (%v)", err)
	}
}

// The ref is a sealed root blob in the project's scope, under one
// restriction: the project's data policy, its destinations sorted and each
// once, its digest the canonical digest of the recorded policy's bytes
// (Opus 22a-5c-3 r2 M1): their key order and spacing change nothing, nor
// does a member the Go struct does not know (a struct that gains a field
// leaves an unchanged stored policy's digest as it was); another policy
// has another digest.
func TestTheBriefsRef(t *testing.T) {
	stored := []byte(`{"floor":"contracted","routing":"verified","allowed_destinations":["stub","api","stub"]}`)
	r, err := Restriction(task.Project, stored)
	if err != nil {
		t.Fatal(err)
	}
	c, err := eventstore.Canonical(stored)
	want, derr := eventstore.ContentDigest(c)
	if err != nil || derr != nil {
		t.Fatal(err, derr)
	}
	if r.Project != task.Project || r.Floor != types.RestrictionFloorContracted || r.Routing != types.RestrictionRoutingVerified ||
		!slices.Equal(r.AllowedDestinations, []string{"api", "stub"}) || string(r.PolicyDigest) != want {
		t.Fatalf("the restriction: %+v, want the digest %s", r, want)
	}
	reordered := []byte(`{ "allowed_destinations": ["stub","api","stub"], "routing": "verified", "floor": "contracted" }`)
	if again, err := Restriction(task.Project, reordered); err != nil || again.PolicyDigest != r.PolicyDigest {
		t.Errorf("the same policy reordered: %+v %v", again, err)
	}
	// A stored policy holding a member today's struct lacks: its digest is
	// its own bytes', never the struct's re-encoding, which drops the
	// member, so the struct gaining that field changes nothing.
	unknown := []byte(`{"floor":"contracted","routing":"verified","allowed_destinations":["api"],"future":1}`)
	var dp types.ProjectProfileDataPolicy
	if err := json.Unmarshal(unknown, &dp); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(dp)
	if err != nil {
		t.Fatal(err)
	}
	cu, err := eventstore.Canonical(unknown)
	wantU, derr := eventstore.ContentDigest(cu)
	cr, rerr := eventstore.Canonical(reencoded)
	struc, serr := eventstore.ContentDigest(cr)
	if err = errors.Join(err, derr, rerr, serr); err != nil {
		t.Fatal(err)
	}
	if u, err := Restriction(task.Project, unknown); err != nil || string(u.PolicyDigest) != wantU || wantU == struc {
		t.Errorf("a stored policy with a member the struct lacks: %+v %v (its bytes' digest %s, the struct's %s)", u, err, wantU, struc)
	}
	other, _ := Restriction(task.Project, []byte(`{"floor":"contracted","routing":"verified","allowed_destinations":["api"]}`))
	if other.PolicyDigest == r.PolicyDigest {
		t.Error("another policy has the same digest")
	}
	_, d, err := Of(task, r.PolicyDigest)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Ref(d, task.Project, r)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Type != types.RefTypeBlob || ref.ID != string(d) || ref.ContentDigest != d || len(ref.Lineage) != 0 || *ref.Scope.Project != task.Project ||
		len(ref.Restrictions) != 1 || knowledge.CheckRef(ref) != nil {
		t.Errorf("the ref: %+v", ref)
	}
}
