// SPDX-License-Identifier: Apache-2.0

package identity

// The one-time bootstrap (04 "Bootstrap", 14 R4): the first start records
// the hash of a fresh setup secret and its fixed expiry; the secret, once,
// creates the first operator and its first token. Consumption is the
// projector's one-time first-operator rule, so there is no separate
// "bootstrapped" truth.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// BootstrapFile is the setup secret's file in the state directory.
const BootstrapFile = "bootstrap.secret"

// BootstrapIssuedKind records the first bootstrap secret (its payload is a
// BootstrapRenewed); only a renewal replaces it.
const BootstrapIssuedKind = "identity.bootstrap_issued/v1"

// NewBootstrapSecret makes a setup secret and its hash of the issuance
// profile. The secret prints as a placeholder; only Reveal gives it.
func NewBootstrapSecret() (Secret, string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Secret{}, "", err
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	hash, err := hashOf(s)
	return Secret{func() string { return s }}, hash, err
}

// IssueBootstrap records hash, of the first secret made at now, expiring
// BootstrapSecretExpiry later. Its subject is the system actor, who
// records it.
func IssueBootstrap(system types.ActorID, hash string, now time.Time) ([]eventstore.Event, error) {
	return bootstrapSecret(BootstrapIssuedKind, system, hash, now)
}

// BootstrapState is the bootstrap as the projections hold it.
type BootstrapState struct {
	Consumed bool                    // the first operator exists
	Secret   *types.BootstrapRenewed // the live secret's record; nil when none was issued
}

// BootstrapOf reads the bootstrap state through tx.
func BootstrapOf(tx *eventstore.Tx) (BootstrapState, error) {
	_, consumed, err := bootstrapped(tx, "operator")
	rec, ok, err2 := load[types.BootstrapRenewed](tx, BootstrapProjection, BootstrapSecretKey)
	st := BootstrapState{Consumed: consumed}
	if ok {
		st.Secret = &rec
	}
	return st, errors.Join(err, err2)
}

// Live reports whether the secret is issued, unconsumed and unexpired at now.
func (s BootstrapState) Live(now time.Time) bool {
	if s.Consumed || s.Secret == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, string(s.Secret.ExpiresAt))
	return err == nil && now.Before(expires)
}
