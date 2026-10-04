// SPDX-License-Identifier: Apache-2.0

// Package knowledge holds refs and view fingerprints (docs/design/m0/10):
// identity digests over everything that makes a ref what it is, and view
// fingerprints over the ordered identities of a view's inputs. Assembly of
// views arrives in M1.
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

// digestOf is the sha256 of v's RFC 8785 (JCS) encoding. Invalid UTF-8 is
// refused, never replaced, so two different values never share a digest.
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

// compareRestrictions orders restrictions by project, then policy digest.
func compareRestrictions(a, b types.Restriction) int {
	return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.PolicyDigest, b.PolicyDigest))
}

// Seal returns r in canonical form (lineage and restrictions sorted: they
// are sets) with its identity digest set. r itself is not changed.
func Seal(r types.Ref) (types.Ref, error) {
	r.Lineage = orEmpty(slices.Sorted(slices.Values(r.Lineage)))
	r.Restrictions = orEmpty(slices.SortedFunc(slices.Values(r.Restrictions), compareRestrictions))
	d, err := identityDigest(r)
	if err != nil {
		return types.Ref{}, err
	}
	r.IdentityDigest = d
	return r, nil
}

func identityDigest(r types.Ref) (types.Digest, error) {
	return digestOf(identity{r.Type, r.ID, r.Revision, r.Scope, orEmpty(r.Lineage), orEmpty(r.Restrictions), r.ContentDigest})
}

// orEmpty: an absent set is the empty set ([] in the encoding, never null),
// so nil and empty never give two identities.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// CheckRef reports whether r is canonical and its identity digest is the
// recomputed one.
func CheckRef(r types.Ref) error {
	if !slices.IsSorted(r.Lineage) || !slices.IsSortedFunc(r.Restrictions, compareRestrictions) {
		return fmt.Errorf("knowledge: ref %s: lineage and restrictions must be sorted (use Seal)", r.ID)
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
// the inputs, in order. Every input must be a checked ref.
func Fingerprint(renderer types.ViewFingerprintRenderer, inputs []types.Ref) (types.ViewFingerprint, error) {
	d, err := fingerprintDigest(renderer, inputs)
	if err != nil {
		return types.ViewFingerprint{}, err
	}
	return types.ViewFingerprint{Renderer: renderer, Inputs: slices.Clone(inputs), Digest: d}, nil
}

// CheckFingerprint recomputes fp from its renderer and inputs. A mismatch
// means the view must be regenerated, never served. It proves the view
// matches its inputs, nothing more: evidence applicability is checked
// separately (06).
func CheckFingerprint(fp types.ViewFingerprint) error {
	d, err := fingerprintDigest(fp.Renderer, fp.Inputs)
	if err != nil {
		return err
	}
	if d != fp.Digest {
		return fmt.Errorf("%w: view fingerprint is %s, recorded %s", ErrMismatch, d, fp.Digest)
	}
	return nil
}
