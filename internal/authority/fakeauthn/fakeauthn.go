// SPDX-License-Identifier: Apache-2.0

// Package fakeauthn is a fake WebAuthn authenticator for tests and the M0
// contract fixtures (docs/design/m0/11 "a replayed/expired WebAuthn
// challenge (with a fake authenticator)"): it holds a generated key and
// signs assertions as a platform authenticator would, with fields a test
// sets to model a wrong RP ID, a wrong origin or missing flags.
package fakeauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/webauthn"
)

// Counter is how the fake's signature counter behaves.
type Counter int

const (
	// Counting advances the counter on every assertion.
	Counting Counter = iota
	// Counterless models an authenticator without a counter: always 0.
	Counterless
	// Frozen repeats the last counter, as a cloned authenticator would.
	Frozen
)

// Authenticator is one fake credential. RPID, Origin, Flags, UserHandle
// and Counter shape the next assertion; ClientData, when set, is signed
// verbatim in place of the client data the fake builds, and Extra is
// appended to the authenticator data.
type Authenticator struct {
	RPID       string
	Origin     string
	Flags      byte
	UserHandle []byte
	Counter    Counter
	ClientData []byte
	Extra      []byte
	cred       webauthn.Credential
	key        crypto.Signer
	ed         bool
	count      uint32
}

// New registers a fake credential for actor with rp: ES256, or Ed25519
// when ed is set. It asserts with the user present and verified.
func New(actor types.ActorID, rp webauthn.RelyingParty, ed bool) (*Authenticator, error) {
	var key crypto.Signer
	var err error
	if ed {
		_, key, err = ed25519.GenerateKey(rand.Reader)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, err
	}
	id, handle := make([]byte, 16), make([]byte, 16)
	for _, b := range [][]byte{id, handle} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	return &Authenticator{RPID: rp.ID, Origin: rp.Origin, Flags: webauthn.FlagUP | webauthn.FlagUV, UserHandle: handle,
		cred: webauthn.Credential{ID: id, Actor: actor, PublicKey: spki, UserHandle: handle}, key: key, ed: ed}, nil
}

// Credential is the credential as registered.
func (a *Authenticator) Credential() webauthn.Credential { return a.cred }

// Assert signs an assertion for challenge.
func (a *Authenticator) Assert(challenge []byte) (webauthn.Assertion, error) {
	cd, err := json.Marshal(map[string]any{"type": webauthn.TypeGet, "challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"origin": a.Origin, "crossOrigin": false})
	if err != nil {
		return webauthn.Assertion{}, err
	}
	if a.ClientData != nil {
		cd = a.ClientData
	}
	switch a.Counter {
	case Counting:
		a.count++
	case Counterless:
		a.count = 0
	}
	rpHash := sha256.Sum256([]byte(a.RPID))
	auth := append(binary.BigEndian.AppendUint32(append(rpHash[:], a.Flags), a.count), a.Extra...)
	cdHash := sha256.Sum256(cd)
	signed := append(append([]byte{}, auth...), cdHash[:]...)
	var sig []byte
	if a.ed {
		sig, err = a.key.Sign(rand.Reader, signed, crypto.Hash(0))
	} else {
		h := sha256.Sum256(signed)
		sig, err = a.key.Sign(rand.Reader, h[:], crypto.SHA256)
	}
	if err != nil {
		return webauthn.Assertion{}, err
	}
	return webauthn.Assertion{CredentialID: a.cred.ID, ClientDataJSON: cd, AuthenticatorData: auth, Signature: sig,
		UserHandle: a.UserHandle}, nil
}
