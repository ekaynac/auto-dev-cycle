// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

const (
	prjA = types.ProjectID("prj_01m3250vkwffc979nzjw0g2sf9")
	prjB = types.ProjectID("prj_01m3250vm7f6ktw39dszqnyddr")
)

var (
	dgA = types.Digest("sha256:" + strings.Repeat("a", 64))
	dgB = types.Digest("sha256:" + strings.Repeat("b", 64))
)

func blob() types.Ref {
	return types.Ref{Type: types.RefTypeBlob, ID: string(dgA), Scope: types.RefScope{Framework: true},
		Lineage: []string{}, Restrictions: []types.Restriction{}, ContentDigest: dgA}
}

func onPrem(p types.ProjectID) types.Restriction {
	return types.Restriction{Project: p, PolicyDigest: dgB, Floor: types.RestrictionFloorOwn,
		Routing: types.RestrictionRoutingVerified, AllowedDestinations: []string{"local-gpu"}}
}

func seal(t *testing.T, r types.Ref) types.Ref {
	t.Helper()
	s, err := Seal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRef(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The identity encoding is pinned: a second implementation (InfraMedic's)
// must reach the same digest. Computed independently from the JCS bytes
// {"content_digest":…,"id":…,"lineage":[],"restrictions":[],"revision":null,
// "scope":{"framework":true,"project":null},"type":"blob"}.
func TestIdentityDigestIsPinned(t *testing.T) {
	const want = "sha256:2fa19757603c283c4ad7150deb6510ae91cd1696214a13c6848b503f6da2030b"
	if got := seal(t, blob()).IdentityDigest; got != want {
		t.Fatalf("identity digest %s, want %s", got, want)
	}
}

// Identity covers revision, scope, lineage and restrictions, so identical
// bytes with a different history are a different ref: deduplication never
// merges restrictions.
func TestIdentityCoversEverythingButTheBytes(t *testing.T) {
	base := seal(t, blob())
	rev := "3"
	variants := map[string]func(*types.Ref){
		"revision":    func(r *types.Ref) { r.Revision = &rev },
		"scope":       func(r *types.Ref) { p := prjA; r.Scope = types.RefScope{Project: &p} },
		"lineage":     func(r *types.Ref) { r.Lineage = []string{"lsn_a"} },
		"restriction": func(r *types.Ref) { r.Restrictions = []types.Restriction{onPrem(prjA)} },
		"type":        func(r *types.Ref) { r.Type = types.RefTypeView },
		"content":     func(r *types.Ref) { r.ContentDigest = dgB },
	}
	seen := map[types.Digest]string{base.IdentityDigest: "base"}
	for name, change := range variants {
		r := blob()
		change(&r)
		s := seal(t, r)
		if s.ContentDigest == base.ContentDigest && s.IdentityDigest == base.IdentityDigest {
			t.Errorf("%s: identical bytes, different %s, same identity", name, name)
		}
		if prev, dup := seen[s.IdentityDigest]; dup {
			t.Errorf("%s shares an identity with %s", name, prev)
		}
		seen[s.IdentityDigest] = name
	}
}

// Lineage and restrictions are sets: Seal orders them, without changing its
// argument, and an unordered ref is refused.
func TestCanonicalOrder(t *testing.T) {
	r := blob()
	r.Lineage = []string{"lsn_b", "lsn_a"}
	r.Restrictions = []types.Restriction{onPrem(prjB), onPrem(prjA)}
	s := seal(t, r)
	if r.Lineage[0] != "lsn_b" || r.Restrictions[0].Project != prjB {
		t.Fatal("Seal changed its argument")
	}
	q := blob()
	q.Lineage = []string{"lsn_a", "lsn_b"}
	q.Restrictions = []types.Restriction{onPrem(prjA), onPrem(prjB)}
	if seal(t, q).IdentityDigest != s.IdentityDigest {
		t.Fatal("the same sets in another order have another identity")
	}
	r.IdentityDigest = s.IdentityDigest
	if err := CheckRef(r); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("an unordered ref: %v", err)
	}
}

func TestCheckRefDetectsEdits(t *testing.T) {
	s := seal(t, blob())
	s.Scope.Framework = false
	if err := CheckRef(s); !errors.Is(err, ErrMismatch) {
		t.Fatalf("an edited ref: %v", err)
	}
	empty := blob()
	empty.Lineage, empty.Restrictions = nil, nil
	if seal(t, empty).IdentityDigest != seal(t, blob()).IdentityDigest {
		t.Fatal("nil and empty sets have different identities")
	}
	bad := blob()
	bad.ID = "x\xff"
	if _, err := Seal(bad); err == nil {
		t.Fatal("invalid UTF-8 was sealed")
	}
}

// A sealed ref and a fingerprint are valid contract documents.
func TestSchemas(t *testing.T) {
	r := blob()
	r.Restrictions = []types.Restriction{onPrem(prjA)}
	s := seal(t, r)
	if _, err := contract.Encode("records/ref", s); err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(renderer(), []types.Ref{s})
	if err != nil {
		t.Fatal(err)
	}
	b, err := contract.Encode("records/view-fingerprint", fp)
	if err != nil {
		t.Fatal(err)
	}
	back, err := contract.Decode[types.ViewFingerprint]("records/view-fingerprint", b)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFingerprint(back); err != nil {
		t.Fatalf("a decoded fingerprint: %v", err)
	}
}

func renderer() types.ViewFingerprintRenderer {
	return types.ViewFingerprintRenderer{Name: "review-packet", Version: "1", ConfigDigest: dgA}
}

func TestFingerprint(t *testing.T) {
	tree := seal(t, types.Ref{Type: types.RefTypeGitTree, ID: "git:" + strings.Repeat("c", 40), Lineage: []string{},
		Restrictions: []types.Restriction{}, ContentDigest: dgA})
	finding := seal(t, types.Ref{Type: types.RefTypeFinding, ID: "fnd_1", Lineage: []string{},
		Restrictions: []types.Restriction{}, ContentDigest: dgB})
	fp, err := Fingerprint(renderer(), []types.Ref{tree, finding})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFingerprint(fp); err != nil {
		t.Fatal(err)
	}

	// A stale view: the finding changed (a new revision) at the same tree.
	rev := "2"
	changed := finding
	changed.Revision = &rev
	changed = seal(t, changed)
	stale := fp
	stale.Inputs = []types.Ref{tree, changed}
	if err := CheckFingerprint(stale); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a view over a changed finding: %v", err)
	}
	// Order, renderer version and the recorded digest all count.
	swapped := fp
	swapped.Inputs = []types.Ref{finding, tree}
	if err := CheckFingerprint(swapped); !errors.Is(err, ErrMismatch) {
		t.Fatalf("reordered inputs: %v", err)
	}
	other := fp
	other.Renderer.Version = "2"
	if err := CheckFingerprint(other); !errors.Is(err, ErrMismatch) {
		t.Fatalf("another renderer version: %v", err)
	}
	edited := fp
	edited.Digest = dgB
	if err := CheckFingerprint(edited); !errors.Is(err, ErrMismatch) {
		t.Fatalf("an edited digest: %v", err)
	}
	// Inputs must themselves be checked refs.
	forged := finding
	forged.ContentDigest = dgA
	if _, err := Fingerprint(renderer(), []types.Ref{tree, forged}); !errors.Is(err, ErrMismatch) {
		t.Fatalf("an unchecked input: %v", err)
	}
	if _, err := Fingerprint(renderer(), nil); err == nil {
		t.Fatal("a view without inputs")
	}
	// The fingerprint keeps its own copy of the inputs.
	inputs := []types.Ref{tree, finding}
	own, err := Fingerprint(renderer(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	inputs[0] = finding
	if err := CheckFingerprint(own); err != nil {
		t.Fatalf("the caller's slice changed the fingerprint: %v", err)
	}
}
