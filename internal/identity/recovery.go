// SPDX-License-Identifier: Apache-2.0

package identity

// Offline recovery (14 R7, R8): two transitions only the system actor
// records, each its own kind and schema, so the ordinary issuance rule
// (the system issues only the first operator's first token) stays as it is.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/ids"
)

// The recovery kinds and their payload schemas.
const (
	TokenRecoveredKind    = "identity.token_recovered/v1"
	BootstrapRenewedKind  = "identity.bootstrap_renewed/v1"
	TokenRecoveredSchema  = "records/token-recovered"
	BootstrapRenewSchema  = "records/bootstrap-renewed"
	BootstrapSecretKey    = "secret" // under BootstrapProjection: the current BootstrapRenewed
	BootstrapSecretExpiry = 24 * time.Hour
)

func recordEvent(kind, subject string, payload any) ([]eventstore.Event, error) {
	id, err := ids.New("evt")
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []eventstore.Event{{ID: id, Kind: kind, Subject: subject, Payload: raw}}, nil
}

// RecoverToken records i, minted by the system actor for an existing live
// operator, and revokes that operator's tokens revoke in the same event.
// The secret goes out only through i.Deliver.
func RecoverToken(i Issued, revoke []types.TokenID) ([]eventstore.Event, error) {
	return recordEvent(TokenRecoveredKind, string(i.Token.ID), types.TokenRecovered{Token: i.Token, Revoke: append([]types.TokenID{}, revoke...)})
}

// RenewBootstrap records hash, of a new bootstrap secret made at now, as
// the only one, expiring BootstrapSecretExpiry later. Its subject is the
// system actor, who records it.
func RenewBootstrap(system types.ActorID, hash string, now time.Time) ([]eventstore.Event, error) {
	return recordEvent(BootstrapRenewedKind, string(system), types.BootstrapRenewed{Hash: hash,
		CreatedAt: types.Time(now.UTC().Format(time.RFC3339Nano)), ExpiresAt: types.Time(now.Add(BootstrapSecretExpiry).UTC().Format(time.RFC3339Nano))})
}

// projectRecovery applies a recovery event; ok is false for any other kind.
func projectRecovery(tx *eventstore.Tx, ev eventstore.Event) (ok bool, err error) {
	switch ev.Kind {
	case TokenRecoveredKind:
		var p types.TokenRecovered
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return true, err
		}
		return true, recoverToken(tx, ev.Subject, types.ActorID(ev.Actor), p)
	case BootstrapRenewedKind:
		var p types.BootstrapRenewed
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return true, err
		}
		return true, renewBootstrap(tx, ev, p)
	}
	return false, nil
}

// systemActor checks that by is the live system actor.
func systemActor(tx *eventstore.Tx, by types.ActorID) error {
	a, err := liveActor(tx, by)
	if err != nil {
		return err
	}
	if a.Kind != types.ActorKindSystem {
		return fmt.Errorf("%s is a %s, not the system actor", by, a.Kind)
	}
	return nil
}

func recoverToken(tx *eventstore.Tx, subject string, by types.ActorID, p types.TokenRecovered) error {
	t := p.Token
	if err := systemActor(tx, by); err != nil {
		return fmt.Errorf("token %s: only the system actor recovers a credential: %w", t.ID, err)
	}
	switch {
	case subject != string(t.ID):
		return fmt.Errorf("subject %s is not token %s", subject, t.ID)
	case t.CreatedBy != by:
		return fmt.Errorf("token %s: created_by %s is not the command's actor %s", t.ID, t.CreatedBy, by)
	case t.RevokedAt != nil:
		return fmt.Errorf("token %s is issued revoked", t.ID)
	case !issuedProfile(t.Hash):
		return fmt.Errorf("token %s: the hash is not of the issuance profile", t.ID)
	}
	if _, ok, err := load[types.Token](tx, TokenProjection, string(t.ID)); err != nil || ok {
		return errors.Join(err, fmt.Errorf("token %s exists", t.ID))
	}
	if holder, err := liveActor(tx, t.Actor); err != nil || holder.Kind != types.ActorKindOperator {
		return errors.Join(err, fmt.Errorf("token %s: the recipient %s is not a live operator", t.ID, t.Actor))
	}
	for i, id := range p.Revoke {
		cur, ok, err := load[types.Token](tx, TokenProjection, string(id))
		if err != nil || !ok || cur.RevokedAt != nil || cur.Actor != t.Actor || slices.Contains(p.Revoke[:i], id) {
			return errors.Join(err, fmt.Errorf("token %s: %s is not another live token of %s", t.ID, id, t.Actor))
		}
		at := t.CreatedAt
		cur.RevokedAt = &at
		if err := put(tx, TokenProjection, string(id), cur); err != nil {
			return err
		}
	}
	return put(tx, TokenProjection, string(t.ID), t)
}

func renewBootstrap(tx *eventstore.Tx, ev eventstore.Event, p types.BootstrapRenewed) error {
	if err := systemActor(tx, types.ActorID(ev.Actor)); err != nil {
		return fmt.Errorf("only the system actor renews the bootstrap: %w", err)
	}
	if ev.Subject != ev.Actor {
		return fmt.Errorf("subject %s is not the system actor %s", ev.Subject, ev.Actor)
	}
	for _, key := range []string{"operator", "token"} {
		if _, done, err := bootstrapped(tx, key); err != nil || done {
			return errors.Join(err, errors.New("the bootstrap is consumed; it is not renewed"))
		}
	}
	if !issuedProfile(p.Hash) {
		return fmt.Errorf("the bootstrap hash is not of the issuance profile")
	}
	made, errC := time.Parse(time.RFC3339Nano, string(p.CreatedAt))
	expires, errE := time.Parse(time.RFC3339Nano, string(p.ExpiresAt))
	at, errA := time.Parse(time.RFC3339Nano, ev.At)
	if errC != nil || errE != nil || errA != nil || !expires.Equal(made.Add(BootstrapSecretExpiry)) || made.After(at) || !at.Before(expires) {
		return fmt.Errorf("the bootstrap secret expires %s after it is made and before it is recorded at %s", BootstrapSecretExpiry, ev.At)
	}
	return put(tx, BootstrapProjection, BootstrapSecretKey, p)
}
