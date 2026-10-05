// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The argon2id cost of a new token (RFC 9106 §4, the second recommended
// option: 64 MiB, 3 passes, 4 lanes) and its sizes.
const (
	argonMemory  = 64 * 1024 // KiB
	argonTime    = 3
	argonThreads = 4
	saltBytes    = 16
	keyBytes     = 32
	secretBytes  = 32
)

// Bounds on the cost a stored hash may name, so a damaged record cannot make
// a verification arbitrarily expensive.
const (
	maxArgonMemory  = 256 * 1024
	maxArgonTime    = 10
	maxArgonThreads = 16
)

// The canonical encodings, matched whole before anything is decoded: a
// bearer secret is 32 bytes of base64url, a hash is the current argon2id
// profile's PHC string (16-byte salt, 32-byte key, canonical trailing bits;
// no other characters, CR and LF included, which base64 would skip).
var (
	secretForm = regexp.MustCompile(`^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$`)
	phcForm    = regexp.MustCompile(`^\$argon2id\$v=19\$m=([1-9][0-9]{0,6}),t=([1-9][0-9]{0,2}),p=([1-9][0-9]{0,2})\$([A-Za-z0-9+/]{21}[AQgw])\$([A-Za-z0-9+/]{42}[AEIMQUYcgkosw048])$`)
)

// Secret is a bearer token as issued: tok_<id>.<secret>. It is shown once and
// never logged: printing or marshalling it gives a placeholder, and only
// Reveal returns the value (04 "logs redact tokens by type").
// The value sits behind a pointer: fmt prints %p of a non-pointer by
// reflection, without Format, and a pointer field prints only as an address.
type Secret struct{ value *string }

const redacted = "[redacted token]"

func (Secret) String() string   { return redacted }
func (Secret) GoString() string { return redacted }

// Format writes the placeholder for every verb and flag (%d, %x, %c and the
// rest would otherwise print the underlying string).
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON refuses: a secret never reaches a document by accident.
func (Secret) MarshalJSON() ([]byte, error) {
	return nil, errors.New("identity: a token secret is never marshalled")
}

// Reveal returns the bearer string, for the one place that shows it.
func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}
	return *s.value
}

// mint makes a token for actor: its record (with the hash) and its secret.
func mint(actor, createdBy types.ActorID, now time.Time) (types.Token, Secret, error) {
	id, err := ids.New("tok")
	if err != nil {
		return types.Token{}, Secret{}, err
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return types.Token{}, Secret{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return types.Token{}, Secret{}, err
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, keyBytes)
	hash := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	t := types.Token{ID: types.TokenID(id), Actor: actor, Hash: hash, CreatedBy: createdBy,
		CreatedAt: now.UTC().Format(time.RFC3339Nano), RevokedAt: nil}
	bearer := id + "." + secret
	return t, Secret{&bearer}, nil
}

// parse splits a bearer string into its token id and secret.
func parse(bearer string) (types.TokenID, string, bool) {
	id, secret, ok := strings.Cut(bearer, ".")
	// The canonical form fixes the decoded size (secretBytes) too.
	if !ok || ids.Check(id, "tok") != nil || !secretForm.MatchString(secret) {
		return "", "", false
	}
	return types.TokenID(id), secret, true
}

// verify reports whether secret matches the PHC argon2id hash, in constant
// time for a given hash. The whole hash must be the canonical form, its
// cost within the bounds and RFC 9106's m >= 8p, before any hashing.
func verify(hash, secret string) bool {
	m := phcForm.FindStringSubmatch(hash)
	if m == nil {
		return false
	}
	memory, errM := strconv.ParseUint(m[1], 10, 32)
	passes, errT := strconv.ParseUint(m[2], 10, 32)
	threads, errP := strconv.ParseUint(m[3], 10, 8)
	if errM != nil || errT != nil || errP != nil ||
		memory > maxArgonMemory || passes > maxArgonTime || threads > maxArgonThreads || memory < 8*threads {
		return false
	}
	// The canonical form fixes the decoded sizes: saltBytes and keyBytes.
	salt, errS := base64.RawStdEncoding.Strict().DecodeString(m[4])
	key, errK := base64.RawStdEncoding.Strict().DecodeString(m[5])
	if errS != nil || errK != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, uint32(passes), uint32(memory), uint8(threads), keyBytes)
	return subtle.ConstantTimeCompare(got, key) == 1
}
