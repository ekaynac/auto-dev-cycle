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
	dgC = types.Digest("sha256:" + strings.Repeat("c", 64))
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

// Golden vectors (computed independently from the canonical-form rules in
// schemas/records/ref.json): code-point ordering of non-ASCII lineage (a
// UTF-16 sort would put U+10000 before U+E000), two restrictions of one
// project under two policy revisions, unsorted destinations, and a
// complete fingerprint.
func TestGoldenVectors(t *testing.T) {
	rev := "3"
	r := types.Ref{Type: types.RefTypeClaim, ID: "lsn_x", Revision: &rev, Scope: types.RefScope{Framework: true},
		Lineage: []string{"lsn_\U00010000", "lsn_b", "lsn_\ue000", "lsn_\u00e9"},
		Restrictions: []types.Restriction{
			{Project: prjA, PolicyDigest: dgC, Floor: types.RestrictionFloorOwn, Routing: types.RestrictionRoutingVerified, AllowedDestinations: []string{"local-gpu", "eu-api"}},
			{Project: prjA, PolicyDigest: dgB, Floor: types.RestrictionFloorContracted, Routing: types.RestrictionRoutingReportedOk, AllowedDestinations: []string{}},
		},
		ContentDigest: dgA}
	s := seal(t, r)
	if want := types.Digest("sha256:e14d576d463f48db7cddc610536ceecd841f0c96d7c84ad162358724293626dd"); s.IdentityDigest != want {
		t.Fatalf("identity %s, want %s", s.IdentityDigest, want)
	}
	if got := s.Lineage; got[2] != "lsn_\ue000" || got[3] != "lsn_\U00010000" {
		t.Fatalf("lineage order %q", got)
	}
	fp, err := Fingerprint(types.ViewFingerprintRenderer{Name: "review-packet", Version: "1", ConfigDigest: dgB}, []types.Ref{seal(t, blob()), s})
	if err != nil {
		t.Fatal(err)
	}
	if want := types.Digest("sha256:7e41a68accd5191fb7d39e713e721c3fa05133bfbf02cd44e8de2aa7ec387f57"); fp.Digest != want {
		t.Fatalf("fingerprint %s, want %s", fp.Digest, want)
	}
}

// Astra #31: nested sets and comparator ties are canonical: any order of
// destinations, or of restrictions differing only in destinations, gives
// one identity; an unsorted ref is refused; duplicates are refused.
func TestNestedSetsAreCanonical(t *testing.T) {
	a := onPrem(prjA)
	a.AllowedDestinations = []string{"local-a", "local-b"}
	b := onPrem(prjA)
	b.AllowedDestinations = []string{"local-b", "local-a"}
	ra, rb := blob(), blob()
	ra.Restrictions, rb.Restrictions = []types.Restriction{a}, []types.Restriction{b}
	if seal(t, ra).IdentityDigest != seal(t, rb).IdentityDigest {
		t.Fatal("reversed destinations give another identity")
	}
	x, y := onPrem(prjA), onPrem(prjA)
	x.AllowedDestinations, y.AllowedDestinations = []string{"one"}, []string{"two"}
	r1, r2 := blob(), blob()
	r1.Restrictions, r2.Restrictions = []types.Restriction{x, y}, []types.Restriction{y, x}
	if seal(t, r1).IdentityDigest != seal(t, r2).IdentityDigest {
		t.Fatal("restrictions tied on project and policy give two identities")
	}
	unsorted := seal(t, ra)
	unsorted.Restrictions[0].AllowedDestinations = []string{"local-b", "local-a"}
	if err := CheckRef(unsorted); err == nil {
		t.Fatal("unsorted destinations were accepted")
	}
	dupLineage := blob()
	dupLineage.Lineage = []string{"lsn_a", "lsn_a"}
	if _, err := Seal(dupLineage); err == nil {
		t.Fatal("duplicate lineage was sealed")
	}
	dupRestriction := blob()
	dupRestriction.Restrictions = []types.Restriction{onPrem(prjA), onPrem(prjA)}
	if _, err := Seal(dupRestriction); err == nil {
		t.Fatal("a duplicate restriction was sealed")
	}
	nilDest := blob()
	r := onPrem(prjA)
	r.AllowedDestinations = nil
	nilDest.Restrictions = []types.Restriction{r}
	emptyDest := blob()
	r.AllowedDestinations = []string{}
	emptyDest.Restrictions = []types.Restriction{r}
	if seal(t, nilDest).IdentityDigest != seal(t, emptyDest).IdentityDigest {
		t.Fatal("nil and empty destinations give two identities")
	}
}

// Astra #31: a checked ref and a fingerprint are schema-valid.
func TestCheckedMeansSchemaValid(t *testing.T) {
	if _, err := Seal(types.Ref{}); err == nil {
		t.Fatal("an empty ref was sealed")
	}
	if _, err := Fingerprint(types.ViewFingerprintRenderer{}, []types.Ref{seal(t, blob())}); err == nil {
		t.Fatal("a fingerprint with an empty renderer")
	}
}

// Astra #31: serving compares the recorded fingerprint with the current
// inputs; an untouched cached fingerprint over a changed finding is stale.
func TestCheckServable(t *testing.T) {
	tree := seal(t, types.Ref{Type: types.RefTypeGitTree, ID: "git:" + strings.Repeat("c", 40), ContentDigest: dgA})
	finding := seal(t, types.Ref{Type: types.RefTypeFinding, ID: "fnd_1", ContentDigest: dgB})
	cached, err := Fingerprint(renderer(), []types.Ref{tree, finding})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckServable(cached, renderer(), []types.Ref{tree, finding}); err != nil {
		t.Fatalf("unchanged inputs: %v", err)
	}
	rev := "2"
	changed := finding
	changed.Revision = &rev
	changed = seal(t, changed)
	if err := CheckFingerprint(cached); err != nil {
		t.Fatalf("the cached fingerprint is internally consistent: %v", err)
	}
	if err := CheckServable(cached, renderer(), []types.Ref{tree, changed}); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a cached view over a changed finding was servable: %v", err)
	}
	other := renderer()
	other.Version = "2"
	if err := CheckServable(cached, other, []types.Ref{tree, finding}); !errors.Is(err, ErrMismatch) {
		t.Fatalf("another renderer was servable: %v", err)
	}
}

// Astra #31: a fingerprint holds deep copies; changing the caller's nested
// slices or pointers changes nothing.
func TestFingerprintOwnsDeepCopies(t *testing.T) {
	r := blob()
	rev := "1"
	r.Revision = &rev
	r.Restrictions = []types.Restriction{onPrem(prjA)}
	s := seal(t, r)
	fp, err := Fingerprint(renderer(), []types.Ref{s})
	if err != nil {
		t.Fatal(err)
	}
	s.Restrictions[0].AllowedDestinations[0] = "elsewhere"
	*s.Revision = "9"
	if err := CheckFingerprint(fp); err != nil {
		t.Fatalf("a caller's nested change reached the fingerprint: %v", err)
	}
}

// The restriction order is total: restrictions tied on project and policy
// but differing in floor have one identity in any order, and a correctly
// digested ref whose restrictions are out of order is still refused.
func TestRestrictionOrderIsTotal(t *testing.T) {
	own, contracted := onPrem(prjA), onPrem(prjA)
	contracted.Floor = types.RestrictionFloorContracted
	r1, r2 := blob(), blob()
	r1.Restrictions, r2.Restrictions = []types.Restriction{own, contracted}, []types.Restriction{contracted, own}
	s := seal(t, r1)
	if s.IdentityDigest != seal(t, r2).IdentityDigest {
		t.Fatal("restrictions tied on project and policy give two identities")
	}
	swapped := s
	swapped.Restrictions = []types.Restriction{s.Restrictions[1], s.Restrictions[0]}
	d, err := identityDigest(swapped)
	if err != nil {
		t.Fatal(err)
	}
	swapped.IdentityDigest = d
	if err := CheckRef(swapped); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("out-of-order restrictions with a matching digest: %v", err)
	}
}
