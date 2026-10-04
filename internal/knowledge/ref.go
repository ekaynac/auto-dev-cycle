// SPDX-License-Identifier: Apache-2.0

// Package knowledge holds refs and view fingerprints (docs/design/m0/10):
// identity digests over everything that makes a ref what it is, and view
// fingerprints over the ordered identities of a view's inputs. Assembly of
// views arrives in M1.
//
// The digests are portable (K10): the preimages and canonical forms are
// specified in schemas/records/ref.json and view-fingerprint.json, and the
// golden vectors in the tests were computed independently of this code.
package knowledge

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/gowebpki/jcs"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// ErrMismatch marks a ref or fingerprint whose recorded digest is not the
// recomputed one: regenerate, never serve.
var ErrMismatch = errors.New("knowledge: digest mismatch")

// identity is what a ref's identity digest covers: every field but the
// digest itself.
type identity struct {
	Type          types.RefType       `json:"type"`
	ID            string              `json:"id"`
	Revision      *string             `json:"revision"`
	Scope         types.RefScope      `json:"scope"`
	Lineage       []string            `json:"lineage"`
	Restrictions  []types.Restriction `json:"restrictions"`
	ContentDigest types.Digest        `json:"content_digest"`
}

// digestOf is "sha256:" + the hex sha256 of v's RFC 8785 (JCS) encoding.
// Invalid UTF-8 is refused, never replaced, so two different values never
// share a digest.
func digestOf(v any) (types.Digest, error) {
	b, err := jsonv2.Marshal(v, json.DefaultOptionsV1(), jsontext.AllowInvalidUTF8(false))
	if err != nil {
		return "", err
	}
	c, err := jcs.Transform(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return types.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Strings in sets are ordered by Unicode code point, which for valid UTF-8
// is the byte order Go's string comparison uses (not UTF-16 code units).

// compareRestrictions is the total order of restrictions: project, policy
// digest, floor, routing, then the (sorted) destinations.
func compareRestrictions(a, b types.Restriction) int {
	return cmp.Or(
		cmp.Compare(a.Project, b.Project),
		cmp.Compare(a.PolicyDigest, b.PolicyDigest),
		cmp.Compare(a.Floor, b.Floor),
		cmp.Compare(a.Routing, b.Routing),
		slices.Compare(a.AllowedDestinations, b.AllowedDestinations),
	)
}

// canonical returns a deep copy of r in canonical form: every set sorted
// (lineage, restrictions, each restriction's destinations), absent sets
// empty, no field shared with r.
func canonical(r types.Ref) types.Ref {
	if r.Revision != nil {
		v := *r.Revision
		r.Revision = &v
	}
	if r.Scope.Project != nil {
		p := *r.Scope.Project
		r.Scope.Project = &p
	}
	r.Lineage = orEmpty(slices.Sorted(slices.Values(r.Lineage)))
	rs := make([]types.Restriction, len(r.Restrictions))
	for i, x := range r.Restrictions {
		x.AllowedDestinations = orEmpty(slices.Sorted(slices.Values(x.AllowedDestinations)))
		rs[i] = x
	}
	slices.SortFunc(rs, compareRestrictions)
	r.Restrictions = rs
	return r
}

// orEmpty: an absent set is the empty set ([] in the encoding, never null).
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// isCanonical: r's sets are sorted, without duplicates, and present.
func isCanonical(r types.Ref) error {
	if r.Lineage == nil || r.Restrictions == nil {
		return errors.New("absent sets are empty, never null")
	}
	if !strictlySorted(r.Lineage, cmp.Compare[string]) {
		return errors.New("lineage must be sorted by code point, without duplicates")
	}
	if !strictlySorted(r.Restrictions, compareRestrictions) {
		return errors.New("restrictions must be in canonical order, without duplicates")
	}
	for _, x := range r.Restrictions {
		if x.AllowedDestinations == nil || !strictlySorted(x.AllowedDestinations, cmp.Compare[string]) {
			return errors.New("each restriction's destinations must be present, sorted and without duplicates")
		}
	}
	return nil
}

func strictlySorted[T any](s []T, c func(a, b T) int) bool {
	for i := 1; i < len(s); i++ {
		if c(s[i-1], s[i]) >= 0 {
			return false
		}
	}
	return true
}

// Seal returns a canonical deep copy of r with its identity digest set. A
// set with duplicates is refused (they would collapse), and the result must
// be a valid records/ref document.
func Seal(r types.Ref) (types.Ref, error) {
	c := canonical(r)
	if err := isCanonical(c); err != nil {
		return types.Ref{}, fmt.Errorf("knowledge: ref %s: %w", r.ID, err)
	}
	d, err := identityDigest(c)
	if err != nil {
		return types.Ref{}, fmt.Errorf("knowledge: ref %s: %w", r.ID, err)
	}
	c.IdentityDigest = d
	if _, err := contract.Encode("records/ref", c); err != nil {
		return types.Ref{}, fmt.Errorf("knowledge: ref %s: %w", r.ID, err)
	}
	return c, nil
}

func identityDigest(r types.Ref) (types.Digest, error) {
	return digestOf(identity{r.Type, r.ID, r.Revision, r.Scope, r.Lineage, r.Restrictions, r.ContentDigest})
}

// CheckRef reports whether r is a checked ref: a valid records/ref document,
// in canonical form, whose identity digest is the recomputed one.
func CheckRef(r types.Ref) error {
	if _, err := contract.Encode("records/ref", r); err != nil {
		return fmt.Errorf("knowledge: ref %s: %w", r.ID, err)
	}
	if err := isCanonical(r); err != nil {
		return fmt.Errorf("knowledge: ref %s: %w (use Seal)", r.ID, err)
	}
	d, err := identityDigest(r)
	if err != nil {
		return fmt.Errorf("knowledge: ref %s: %w", r.ID, err)
	}
	if d != r.IdentityDigest {
		return fmt.Errorf("%w: ref %s identity is %s, recorded %s", ErrMismatch, r.ID, d, r.IdentityDigest)
	}
	return nil
}

// fingerprint is what a view fingerprint's digest covers: the renderer and
// the ordered identity digests of the inputs (not their content digests).
type fingerprint struct {
	Renderer types.ViewFingerprintRenderer `json:"renderer"`
	Inputs   []types.Digest                `json:"inputs"`
}

func fingerprintDigest(renderer types.ViewFingerprintRenderer, inputs []types.Ref) (types.Digest, error) {
	if len(inputs) == 0 {
		return "", errors.New("knowledge: a view has at least one input")
	}
	ids := make([]types.Digest, len(inputs))
	for i, in := range inputs {
		if err := CheckRef(in); err != nil {
			return "", err
		}
		ids[i] = in.IdentityDigest
	}
	return digestOf(fingerprint{renderer, ids})
}

// Fingerprint returns the fingerprint of a view rendered by renderer from
// the inputs, in order. Every input must be a checked ref; the fingerprint
// holds deep copies, so the caller's refs cannot change it later.
func Fingerprint(renderer types.ViewFingerprintRenderer, inputs []types.Ref) (types.ViewFingerprint, error) {
	d, err := fingerprintDigest(renderer, inputs)
	if err != nil {
		return types.ViewFingerprint{}, err
	}
	own := make([]types.Ref, len(inputs))
	for i, in := range inputs {
		own[i] = canonical(in) // a checked ref is canonical: this only copies
		own[i].IdentityDigest = in.IdentityDigest
	}
	fp := types.ViewFingerprint{Renderer: renderer, Inputs: own, Digest: d}
	if _, err := contract.Encode("records/view-fingerprint", fp); err != nil {
		return types.ViewFingerprint{}, fmt.Errorf("knowledge: %w", err)
	}
	return fp, nil
}

// CheckFingerprint checks that fp is a valid records/view-fingerprint whose
// digest is the one its own renderer and inputs give: internal consistency
// only. It does not show that the view is fresh (its inputs may have changed
// since) or where the inputs came from; serving a view needs CheckServable.
func CheckFingerprint(fp types.ViewFingerprint) error {
	if _, err := contract.Encode("records/view-fingerprint", fp); err != nil {
		return fmt.Errorf("knowledge: %w", err)
	}
	d, err := fingerprintDigest(fp.Renderer, fp.Inputs)
	if err != nil {
		return err
	}
	if d != fp.Digest {
		return fmt.Errorf("%w: view fingerprint is %s, recorded %s", ErrMismatch, d, fp.Digest)
	}
	return nil
}

// CheckServable decides whether a cached view may be served. The recorded
// fingerprint must itself pass CheckFingerprint, and it must equal the
// fingerprint of the expected renderer over the current inputs, which the
// caller resolves independently from the controller's records (never from
// the cached fingerprint). Any difference, a changed input, a changed
// renderer or another order, means regenerate. Evidence applicability is
// checked separately (06).
func CheckServable(recorded types.ViewFingerprint, renderer types.ViewFingerprintRenderer, current []types.Ref) error {
	if err := CheckFingerprint(recorded); err != nil {
		return err
	}
	expected, err := Fingerprint(renderer, current) // validates the renderer and every input
	if err != nil {
		return err
	}
	d := expected.Digest
	if err != nil {
		return err
	}
	if d != recorded.Digest {
		return fmt.Errorf("%w: the view was rendered from other inputs or another renderer (now %s, recorded %s)", ErrMismatch, d, recorded.Digest)
	}
	return nil
}
