// SPDX-License-Identifier: Apache-2.0

// Package webauthn verifies the WebAuthn assertion that answers a human
// confirmation challenge (docs/design/m0/04 "Proposals and confirmation",
// D37), per W3C Web Authentication Level 2 §7.2: the credential, the
// client data's type, challenge and origin, the authenticator data's RP ID
// hash, user-presence and user-verification flags and signature counter,
// and the signature over authenticatorData ‖ SHA-256(clientDataJSON).
//
// A credential is registered with its public key as a DER
// SubjectPublicKeyInfo, the form a browser's
// AuthenticatorAttestationResponse.getPublicKey() returns, so no COSE or
// CBOR is parsed here. ES256 (ECDSA P-256) and Ed25519 are accepted.
package webauthn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
)

// Authenticator data flags (§6.1).
const (
	FlagUP = 0x01 // user present
	FlagUV = 0x04 // user verified
)

// TypeGet is the client data type of an assertion.
const TypeGet = "webauthn.get"

// authDataMin is the RP ID hash (32), the flags (1) and the counter (4).
const authDataMin = 37

// RelyingParty is the dashboard's RP ID and the exact origin it serves.
type RelyingParty struct {
	ID     string
	Origin string
}

// Credential is an operator's registered public-key credential: its id,
// its actor, its DER SubjectPublicKeyInfo and the last signature counter
// a verified assertion carried.
type Credential struct {
	ID        []byte
	Actor     types.ActorID
	PublicKey []byte
	SignCount uint32
}

// Assertion is a raw assertion as the dashboard receives it from
// navigator.credentials.get().
type Assertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
}

// Error is a check an assertion failed.
type Error struct{ Detail string }

func (e *Error) Error() string { return "webauthn: " + e.Detail }

func fail(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// Challenge is the challenge a confirmation requests (04): the challenge's
// 32 nonce bytes followed by the 32 bytes of the binding digest it is
// bound to.
func Challenge(nonce string, binding types.Digest) ([]byte, error) {
	n, err := hex.DecodeString(nonce)
	if err != nil || len(n) != 32 {
		return nil, fmt.Errorf("webauthn: a nonce is 32 hex-encoded bytes")
	}
	d, err := hex.DecodeString(strings.TrimPrefix(string(binding), "sha256:"))
	if err != nil || len(d) != sha256.Size || !strings.HasPrefix(string(binding), "sha256:") {
		return nil, fmt.Errorf("webauthn: binding digest %q is not a sha256 digest", binding)
	}
	return append(n, d...), nil
}

// Verify checks a for challenge against the registered credential and rp,
// and returns the signature counter it carries, for the caller to record
// on the credential with the challenge consumed. User verification is
// required (04).
func Verify(rp RelyingParty, cred Credential, challenge []byte, a Assertion) (uint32, error) {
	if !bytes.Equal(a.CredentialID, cred.ID) {
		return 0, fail("the assertion is from another credential")
	}
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(a.ClientDataJSON, &cd); err != nil {
		return 0, fail("client data: %v", err)
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	switch {
	case cd.Type != TypeGet:
		return 0, fail("client data type is %q, not %s", cd.Type, TypeGet)
	case err != nil || subtle.ConstantTimeCompare(got, challenge) != 1:
		return 0, fail("the assertion answers another challenge")
	case cd.Origin != rp.Origin || cd.CrossOrigin:
		return 0, fail("origin %q is not %s", cd.Origin, rp.Origin)
	case len(a.AuthenticatorData) < authDataMin:
		return 0, fail("authenticator data is %d bytes", len(a.AuthenticatorData))
	}
	rpHash := sha256.Sum256([]byte(rp.ID))
	flags := a.AuthenticatorData[32]
	count := binary.BigEndian.Uint32(a.AuthenticatorData[33:authDataMin])
	switch {
	case subtle.ConstantTimeCompare(a.AuthenticatorData[:32], rpHash[:]) != 1:
		return 0, fail("the RP ID hash is not for %s", rp.ID)
	case flags&FlagUP == 0:
		return 0, fail("the user was not present")
	case flags&FlagUV == 0:
		return 0, fail("the user was not verified")
	}
	if err := verifySignature(cred.PublicKey, a); err != nil {
		return 0, err
	}
	// §7.2 step 21: a counter that does not advance (when either is
	// nonzero) signals a cloned authenticator or a replay.
	if (count != 0 || cred.SignCount != 0) && count <= cred.SignCount {
		return 0, fail("the signature counter %d does not advance past %d", count, cred.SignCount)
	}
	return count, nil
}

// verifySignature checks the signature over authenticatorData ‖
// SHA-256(clientDataJSON) with the credential's key.
func verifySignature(spki []byte, a Assertion) error {
	if mutation.Is(mutation.SignatureUnchecked) {
		return nil
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return fail("credential key: %v", err)
	}
	cdHash := sha256.Sum256(a.ClientDataJSON)
	signed := append(append([]byte{}, a.AuthenticatorData...), cdHash[:]...)
	ok := false
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		h := sha256.Sum256(signed)
		ok = k.Curve == elliptic.P256() && ecdsa.VerifyASN1(k, h[:], a.Signature)
	case ed25519.PublicKey:
		ok = ed25519.Verify(k, signed, a.Signature)
	default:
		return fail("credential key type %T is not ES256 or Ed25519", pub)
	}
	if !ok {
		return fail("the signature does not verify")
	}
	return nil
}
