// SPDX-License-Identifier: Apache-2.0

package webauthn_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/fakeauthn"
	"github.com/ekaynac/auto-dev-cycle/internal/authority/webauthn"
)

var rp = webauthn.RelyingParty{ID: "adc.example", Origin: "https://adc.example"}

func challenge(t *testing.T) []byte {
	t.Helper()
	c, err := webauthn.Challenge(strings.Repeat("07", 32), types.Digest("sha256:"+strings.Repeat("ab", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChallenge(t *testing.T) {
	c := challenge(t)
	if len(c) != 64 || c[0] != 7 || c[63] != 0xab {
		t.Fatalf("challenge %x", c)
	}
	for _, bad := range []struct {
		nonce  string
		digest types.Digest
	}{{"07", "sha256:" + types.Digest(strings.Repeat("ab", 32))}, {strings.Repeat("07", 32), "sha256:ab"},
		{strings.Repeat("07", 32), types.Digest("md5:" + strings.Repeat("ab", 32))}} {
		if _, err := webauthn.Challenge(bad.nonce, bad.digest); err == nil {
			t.Errorf("a challenge from %q %q", bad.nonce, bad.digest)
		}
	}
}

// refusedBy is the check each faulty assertion must fail.
var refusedBy = map[string]string{
	"replayed": "counter", "another challenge": "another challenge", "another credential": "another credential",
	"a bad signature": "signature", "a wrong rp id": "RP ID", "a wrong origin": "origin", "no user presence": "not present",
	"no user verification": "not verified", "a create type": "type", "cross-origin": "origin",
	"short authenticator data": "authenticator data", "bad client data": "client data",
}

func TestVerify(t *testing.T) {
	for _, ed := range []bool{false, true} {
		auth, err := fakeauthn.New("act_01m3250vm7f6ktw39dszqnyddr", rp, ed)
		if err != nil {
			t.Fatal(err)
		}
		cred := auth.Credential()
		ch := challenge(t)
		a, err := auth.Assert(ch)
		if err != nil {
			t.Fatal(err)
		}
		count, err := webauthn.Verify(rp, cred, ch, a)
		if err != nil || count != 1 {
			t.Fatalf("ed25519 %t: a fresh assertion: %d %v", ed, count, err)
		}
		cred.SignCount = count
		fault := func(edit func(*fakeauthn.Authenticator), after func(*webauthn.Assertion)) error {
			f := *auth
			edit(&f)
			a, err := f.Assert(ch)
			if err != nil {
				t.Fatal(err)
			}
			after(&a)
			_, err = webauthn.Verify(rp, cred, ch, a)
			return err
		}
		none := func(*fakeauthn.Authenticator) {}
		keep := func(*webauthn.Assertion) {}
		for name, err := range map[string]error{
			"replayed":             func() error { _, err := webauthn.Verify(rp, cred, ch, a); return err }(),
			"another challenge":    func() error { _, err := webauthn.Verify(rp, cred, bytes.Repeat([]byte{1}, 64), a); return err }(),
			"another credential":   fault(none, func(a *webauthn.Assertion) { a.CredentialID = []byte("x") }),
			"a bad signature":      fault(none, func(a *webauthn.Assertion) { a.Signature[len(a.Signature)/2] ^= 1 }),
			"a wrong rp id":        fault(func(f *fakeauthn.Authenticator) { f.RPID = "evil.example" }, keep),
			"a wrong origin":       fault(func(f *fakeauthn.Authenticator) { f.Origin = "https://evil.example" }, keep),
			"no user presence":     fault(func(f *fakeauthn.Authenticator) { f.Flags = webauthn.FlagUV }, keep),
			"no user verification": fault(func(f *fakeauthn.Authenticator) { f.Flags = webauthn.FlagUP }, keep),
			"a create type": fault(none, func(a *webauthn.Assertion) {
				a.ClientDataJSON = bytes.Replace(a.ClientDataJSON, []byte(webauthn.TypeGet), []byte("webauthn.create"), 1)
			}),
			"cross-origin": fault(none, func(a *webauthn.Assertion) {
				a.ClientDataJSON = bytes.Replace(a.ClientDataJSON, []byte(`"crossOrigin":false`), []byte(`"crossOrigin":true`), 1)
			}),
			"short authenticator data": fault(none, func(a *webauthn.Assertion) { a.AuthenticatorData = a.AuthenticatorData[:36] }),
			"bad client data":          fault(none, func(a *webauthn.Assertion) { a.ClientDataJSON = []byte("{") }),
		} {
			var e *webauthn.Error
			if !errors.As(err, &e) || !strings.Contains(e.Detail, refusedBy[name]) {
				t.Errorf("ed25519 %t: %s: %v, want %q", ed, name, err, refusedBy[name])
			}
		}
	}
}

// An authenticator without a counter reports zero every time: that is
// accepted while the stored counter is zero too, and refused once it is
// not (§7.2 step 21).
func TestZeroCounter(t *testing.T) {
	auth, err := fakeauthn.New("act_01m3250vm7f6ktw39dszqnyddr", rp, false)
	if err != nil {
		t.Fatal(err)
	}
	auth.Static = true
	ch := challenge(t)
	cred := auth.Credential()
	for range 2 {
		a, err := auth.Assert(ch)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := webauthn.Verify(rp, cred, ch, a); err != nil || count != 0 {
			t.Fatalf("a zero counter: %d %v", count, err)
		}
	}
	a, err := auth.Assert(ch)
	if err != nil {
		t.Fatal(err)
	}
	cred.SignCount = 1
	if _, err := webauthn.Verify(rp, cred, ch, a); err == nil {
		t.Error("a zero counter after 1")
	}
}
