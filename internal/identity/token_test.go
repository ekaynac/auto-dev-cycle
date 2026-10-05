// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	_ "github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const (
	actA = "act_01m3250v0aer9ss2097svmefj8"
	actB = "act_01m3250vm7f6ktw39dszqnyddr"
)

func TestMint(t *testing.T) {
	tok, secret, err := mint(actA, actB, time.Date(2026, 10, 5, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^tok_[0-7][0-9a-hjkmnp-tv-z]{25}\.[A-Za-z0-9_-]{43}$`).MatchString(secret.Reveal()) ||
		!strings.HasPrefix(secret.Reveal(), string(tok.ID)+".") {
		t.Fatalf("bearer %q for %s", secret.Reveal(), tok.ID)
	}
	if tok.Actor != actA || tok.CreatedBy != actB || tok.CreatedAt != "2026-10-05T01:02:03Z" || tok.RevokedAt != nil {
		t.Fatalf("%+v", tok)
	}
	b, _ := json.Marshal(tok)
	if err := contract.Validate("records/token", b); err != nil {
		t.Fatal(err)
	}
	// The record never holds the secret.
	_, s, _ := strings.Cut(secret.Reveal(), ".")
	if strings.Contains(string(b), s) {
		t.Fatal("the record carries the secret")
	}
	// The current profile, exactly.
	if !strings.HasPrefix(tok.Hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash %s", tok.Hash)
	}
	_, again, err := mint(actA, actB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, s2, _ := strings.Cut(again.Reveal(), "."); s2 == s {
		t.Fatal("two tokens share a secret")
	}
}

// phc encodes an argon2id hash of secret with any profile, as a damaged or
// foreign record might carry it.
func phc(secret string, salt []byte, passes, memory uint32, lanes uint8, keyLen uint32) string {
	key := argon2.IDKey([]byte(secret), salt, passes, memory, lanes, keyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, passes, lanes,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// A secret prints and marshals as a placeholder whatever the verb, flag or
// nesting; only Reveal gives it.
func TestSecretIsRedacted(t *testing.T) {
	_, secret, err := mint(actA, actA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	idPart, secretPart, _ := strings.Cut(secret.Reveal(), ".")
	safe := func(what, out string) {
		t.Helper()
		if strings.Contains(out, idPart) || strings.Contains(out, secretPart) || strings.Contains(out, secretPart[:8]) {
			t.Errorf("%s printed %q", what, out)
		}
	}
	type nested struct {
		Name   string
		Secret Secret
		Ptr    *Secret
	}
	values := map[string]any{"value": secret, "pointer": &secret, "field": nested{"x", secret, &secret},
		"pointer to struct": &nested{"x", secret, &secret}, "slice": []Secret{secret}, "map": map[string]Secret{"k": secret}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%c", "%U", "%f", "%e", "%g", "%t", "%b", "%o", "%p",
		"%10d", "%-#x", "%+q", "%08.3f", "%[1]v"} {
		for name, v := range values {
			safe(verb+" of a "+name, fmt.Sprintf(verb, v))
		}
	}
	if out := fmt.Sprint(secret); out != redacted {
		t.Errorf("Sprint gave %q", out)
	}
	// Called directly, as a template or a logger may.
	if secret.String() != redacted || secret.GoString() != redacted {
		t.Errorf("String %q, GoString %q", secret.String(), secret.GoString())
	}
	if _, err := json.Marshal(struct{ S Secret }{secret}); err == nil {
		t.Error("a secret was marshalled")
	}
}

func TestVerify(t *testing.T) {
	tok, secret, err := mint(actA, actA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, s, _ := strings.Cut(secret.Reveal(), ".")
	if !verify(tok.Hash, s) {
		t.Fatal("the right secret is refused")
	}
	other := []byte(s)
	other[0] ^= 1
	parts := strings.Split(tok.Hash, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for name, c := range map[string]struct{ hash, secret string }{
		"a wrong secret":        {tok.Hash, string(other)},
		"an empty secret":       {tok.Hash, ""},
		"argon2i, not argon2id": {with(1, "argon2i"), s},
		"another version":       {with(2, "v=16"), s},
		"a changed cost":        {with(3, "m=65536,t=2,p=4"), s},
		"zero memory":           {with(3, "m=0,t=3,p=4"), s},
		"padded base64":         {with(5, parts[5]+"="), s},
		"garbage after v=19":    {with(2, "v=19garbage"), s},
		"garbage after p=4":     {with(3, parts[3]+"x"), s},
		"a newline in the key":  {with(5, parts[5]+"\n"), s},
		"a newline in the salt": {with(4, parts[4][:11]+"\n"+parts[4][11:]), s},
		"a leading zero":        {with(3, "m=065536,t=3,p=4"), s},
		"an oversized salt":     {with(4, parts[4]+parts[4]), s},
		"an empty key":          {with(5, ""), s},
		"a noncanonical salt":   {with(4, parts[4][:21]+"B"), s},
		"too few fields":        {strings.Join(parts[:5], "$"), s},
		"not PHC":               {"plain", s},
	} {
		if verify(c.hash, c.secret) {
			t.Errorf("%s: verified", name)
		}
	}
	// A correctly computed hash of the right secret with a profile outside
	// the policy is refused: a one-byte key (8 bits to guess), a long salt,
	// and costs RFC 9106 forbids (m < 8p).
	salt := make([]byte, 16)
	for name, hash := range map[string]string{
		"a one-byte key":  phc(s, salt, 3, 65536, 4, 1),
		"a 16-byte key":   phc(s, salt, 3, 65536, 4, 16),
		"a 32-byte salt":  phc(s, make([]byte, 32), 3, 65536, 4, 32),
		"m below 8p":      phc(s, salt, 1, 1, 1, 32),
		"m just below 8p": phc(s, salt, 1, 31, 4, 32),
		"17 lanes":        phc(s, salt, 1, 136, 17, 32),
	} {
		if verify(hash, s) {
			t.Errorf("%s: verified", name)
		}
	}
	if !verify(phc(s, salt, 1, 32, 4, 32), s) {
		t.Error("m = 8p, an allowed cheap profile, is refused")
	}
	if !verify(phc(s, salt, 1, 128, 16, 32), s) {
		t.Error("16 lanes, the bound, is refused")
	}
	// A cost over the bounds is refused before any hashing: a damaged
	// record cannot make a verification take gigabytes or minutes.
	for name, cost := range map[string]string{
		"memory over the bound": "m=9999999,t=3,p=4",
		"passes over the bound": "m=65536,t=999,p=4",
		"lanes over the bound":  "m=65536,t=3,p=17",
	} {
		start := time.Now()
		if verify(with(3, cost), s) {
			t.Errorf("%s: verified", name)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v", name, d)
		}
	}
}

func TestParse(t *testing.T) {
	_, secret, err := mint(actA, actA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b := secret.Reveal()
	id, s, ok := parse(b)
	if !ok || string(id)+"."+s != b {
		t.Fatalf("%q: %s %s %v", b, id, s, ok)
	}
	idPart, sPart, _ := strings.Cut(b, ".")
	for name, bearer := range map[string]string{
		"no separator":       idPart + sPart,
		"another prefix":     "act" + strings.TrimPrefix(b, "tok"),
		"a short secret":     b[:len(b)-1],
		"a long secret":      b + "A",
		"base64 with + or /": idPart + "." + "+" + sPart[1:],
		"empty":              "",
		"a bearer prefix":    "Bearer " + b,
		"newlines":           idPart + "." + strings.Repeat("\n", 43),
		"a CR inside":        idPart + "." + sPart[:20] + "\r" + sPart[21:],
		"noncanonical bits":  idPart + "." + sPart[:42] + "B",
	} {
		if _, _, ok := parse(bearer); ok {
			t.Errorf("%s: parsed", name)
		}
	}
}

// The record schema admits only canonical encodings of the current sizes.
func TestTokenSchemaRefusesMalformedHashes(t *testing.T) {
	tok, _, err := mint(actA, actA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok.Hash, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for name, hash := range map[string]string{
		"a 25-character salt":    with(4, parts[4]+"AAA"),
		"a 45-character key":     with(5, parts[5]+"AA"),
		"zero memory":            with(3, "m=0,t=3,p=4"),
		"noncanonical salt bits": with(4, parts[4][:21]+"B"),
		"noncanonical key bits":  with(5, parts[5][:42]+"B"),
		"argon2i":                with(1, "argon2i"),
		"a newline":              tok.Hash + "\n",
	} {
		bad := tok
		bad.Hash = hash
		b, _ := json.Marshal(bad)
		if contract.Validate("records/token", b) == nil {
			t.Errorf("%s: valid", name)
		}
	}
}
