// SPDX-License-Identifier: Apache-2.0

package webauthn_test

import (
	"bytes"
	"encoding/base64"
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

// signed is client data with b64 as its challenge and extra fields, for a
// signed probe.
func signed(b64, fields string) []byte {
	return []byte(`{"type":"webauthn.get","challenge":"` + b64 + `","origin":"https://adc.example"` + fields + `}`)
}

// nonCanonical re-encodes b64's last character with nonzero padding bits,
// so it decodes to the same bytes under a lenient decoder.
func nonCanonical(b64 string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, b64[len(b64)-1])
	return b64[:len(b64)-1] + string(alphabet[last|1])
}

func TestVerify(t *testing.T) {
	ch := challenge(t)
	b64 := base64.RawURLEncoding.EncodeToString(ch)
	cd := func(json string) func(*fakeauthn.Authenticator) {
		return func(f *fakeauthn.Authenticator) { f.ClientData = []byte(json) }
	}
	flags := func(fl byte) func(*fakeauthn.Authenticator) {
		return func(f *fakeauthn.Authenticator) { f.Flags = fl }
	}
	const upuv = webauthn.FlagUP | webauthn.FlagUV
	// Each fault with the check it must fail; "" is an assertion that
	// verifies.
	faults := []struct {
		name, want string
		edit       func(*fakeauthn.Authenticator)
		tamper     func(*webauthn.Assertion)
	}{
		{"an honest assertion", "", func(*fakeauthn.Authenticator) {}, nil},
		{"an extension field", "", cd(string(signed(b64, `,"other_keys_can_be_added_here":"x","crossOrigin":false`))), nil},
		{"no user handle", "", func(f *fakeauthn.Authenticator) { f.UserHandle = nil }, nil},
		{"backed up and eligible", "", flags(upuv | webauthn.FlagBE | webauthn.FlagBS), nil},
		{"another credential", "another credential", nil, func(a *webauthn.Assertion) { a.CredentialID = []byte("x") }},
		{"a bad signature", "signature", nil, func(a *webauthn.Assertion) { a.Signature[len(a.Signature)/2] ^= 1 }},
		{"another user handle", "user handle", func(f *fakeauthn.Authenticator) { f.UserHandle = []byte("someone") }, nil},
		{"a wrong rp id", "RP ID", func(f *fakeauthn.Authenticator) { f.RPID = "evil.example" }, nil},
		{"a wrong origin", "origin", func(f *fakeauthn.Authenticator) { f.Origin = "https://evil.example" }, nil},
		{"no user presence", "not present", flags(webauthn.FlagUV), nil},
		{"no user verification", "not verified", flags(webauthn.FlagUP), nil},
		{"attested credential data", "attested", flags(upuv | webauthn.FlagAT), nil},
		{"extension data", "extension", func(f *fakeauthn.Authenticator) { f.Flags, f.Extra = upuv|webauthn.FlagED, []byte{0xa0} }, nil},
		{"trailing bytes", "authenticator data", func(f *fakeauthn.Authenticator) { f.Extra = []byte{0} }, nil},
		{"backed up, not eligible", "backup", flags(upuv | webauthn.FlagBS), nil},
		{"a create type", "type", cd(strings.Replace(string(signed(b64, "")), "webauthn.get", "webauthn.create", 1)), nil},
		{"an uppercase TYPE", "no type", cd(strings.Replace(string(signed(b64, "")), `"type"`, `"TYPE"`, 1)), nil},
		{"a repeated type", "repeats type", cd(string(signed(b64, `,"type":null`))), nil},
		{"a null type", "not a string", cd(strings.Replace(string(signed(b64, "")), `"webauthn.get"`, "null", 1)), nil},
		{"a null crossOrigin", "crossOrigin", cd(string(signed(b64, `,"crossOrigin":null`))), nil},
		{"cross-origin", "origin", cd(string(signed(b64, `,"crossOrigin":true`))), nil},
		{"a top origin", "top origin", cd(string(signed(b64, `,"topOrigin":"https://adc.example"`))), nil},
		{"no origin", "no origin", cd(`{"type":"webauthn.get","challenge":"` + b64 + `"}`), nil},
		{"trailing content", "trailing", cd(string(signed(b64, "")) + "{}"), nil},
		{"not an object", "not a JSON object", cd(`["webauthn.get"]`), nil},
		{"a challenge with a newline", "another challenge", cd(string(signed(b64[:10]+`\n`+b64[10:], ""))), nil},
		{"a non-canonical challenge", "another challenge", cd(string(signed(nonCanonical(b64), ""))), nil},
		{"another challenge", "another challenge", cd(string(signed(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)), ""))), nil},
	}
	for _, ed := range []bool{false, true} {
		auth, err := fakeauthn.New("act_01m3250vm7f6ktw39dszqnyddr", rp, ed)
		if err != nil {
			t.Fatal(err)
		}
		cred := auth.Credential()
		a, err := auth.Assert(ch)
		if err != nil {
			t.Fatal(err)
		}
		count, err := webauthn.Verify(rp, cred, ch, a)
		if err != nil || count != 1 {
			t.Fatalf("ed25519 %t: a fresh assertion: %d %v", ed, count, err)
		}
		cred.SignCount = count
		if _, err := webauthn.Verify(rp, cred, ch, a); err == nil || !strings.Contains(err.Error(), "counter") {
			t.Errorf("ed25519 %t: a replay: %v", ed, err)
		}
		for _, c := range faults {
			f := *auth
			if c.edit != nil {
				c.edit(&f)
			}
			a, err := f.Assert(ch)
			if err != nil {
				t.Fatal(err)
			}
			if c.tamper != nil {
				c.tamper(&a)
			}
			_, err = webauthn.Verify(rp, cred, ch, a)
			var e *webauthn.Error
			switch {
			case c.want == "" && err != nil:
				t.Errorf("ed25519 %t: %s: %v", ed, c.name, err)
			case c.want != "" && (!errors.As(err, &e) || !strings.Contains(e.Detail, c.want)):
				t.Errorf("ed25519 %t: %s: %v, want %q", ed, c.name, err, c.want)
			}
		}
	}
}

// A counterless authenticator reports 0 every time, even after counting:
// accepted while the stored counter is 0 too, refused once it is not; a
// frozen counter repeats its last value and is refused (§7.2 step 21).
func TestCounterModes(t *testing.T) {
	auth, err := fakeauthn.New("act_01m3250vm7f6ktw39dszqnyddr", rp, false)
	if err != nil {
		t.Fatal(err)
	}
	ch := challenge(t)
	cred := auth.Credential()
	assert := func() webauthn.Assertion {
		a, err := auth.Assert(ch)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if count, err := webauthn.Verify(rp, cred, ch, assert()); err != nil || count != 1 {
		t.Fatalf("a counting assertion: %d %v", count, err)
	}
	auth.Counter = fakeauthn.Counterless
	for range 2 {
		if count, err := webauthn.Verify(rp, cred, ch, assert()); err != nil || count != 0 {
			t.Fatalf("a zero counter: %d %v", count, err)
		}
	}
	cred.SignCount = 1
	if _, err := webauthn.Verify(rp, cred, ch, assert()); err == nil {
		t.Error("a zero counter after 1")
	}
	cred.SignCount = 0
	auth.Counter = fakeauthn.Counting
	if count, err := webauthn.Verify(rp, cred, ch, assert()); err != nil || count != 1 {
		t.Fatalf("counting again from 0: %d %v", count, err)
	}
	cred.SignCount = 0
	auth.Counter = fakeauthn.Frozen
	for _, want := range []bool{true, false} {
		count, err := webauthn.Verify(rp, cred, ch, assert())
		if (err == nil) != want || (want && count != 1) {
			t.Fatalf("a frozen counter at 1 after %d: %d %v", cred.SignCount, count, err)
		}
		cred.SignCount = count
	}
}
