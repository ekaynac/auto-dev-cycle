// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/blobstore"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

func tokenID(s identity.Secret) types.TokenID {
	id, _, _ := strings.Cut(s.Reveal(), ".")
	return types.TokenID(id)
}

func rawEvent(t *testing.T, kind, subject string, payload any) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) {
		raw, err := json.Marshal(payload)
		return []eventstore.Event{{ID: id(t, "evt"), Kind: kind, Subject: subject, Payload: raw}}, err
	}
}

func mintFor(t *testing.T, holder, issuer types.ActorID) identity.Issued {
	t.Helper()
	i, err := identity.Mint(holder, issuer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func recoverApply(i identity.Issued, revoke ...types.TokenID) applyFn {
	return func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.RecoverToken(i, revoke) }
}

// snapshot is every key of a projection with its raw record.
func (f *fixture) snapshot(projection string) map[string]string {
	f.t.Helper()
	out := map[string]string{}
	if err := f.s.View(context.Background(), func(tx *eventstore.Tx) error {
		keys, err := tx.Keys(projection, "")
		for _, k := range keys {
			raw, _, gerr := tx.Get(projection, k)
			if gerr != nil {
				return gerr
			}
			out[k] = string(raw)
		}
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) rebuildsSame() {
	f.t.Helper()
	if diffs, err := f.s.RebuildProjections(context.Background()); err != nil || len(diffs) != 0 {
		f.t.Fatalf("rebuild: %v %v", diffs, err)
	}
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Recovery issues the operator a token and revokes the named others in one
// event; it touches no actor and no bootstrap record, and the system still
// issues nothing ordinary.
func TestRecoverToken(t *testing.T) {
	f := open(t)
	sup := actor(t, types.ActorKindSupervisor, f.op.ID)
	f.must(f.op.ID, create(sup))
	var second, supTok identity.Secret
	f.must(f.op.ID, issue(f.op.ID, f.op.ID, &second))
	f.must(f.op.ID, issue(sup.ID, f.op.ID, &supTok))
	actors, boot := f.snapshot(identity.ActorProjection), f.snapshot(identity.BootstrapProjection)

	i := mintFor(t, f.op.ID, f.sys.ID)
	f.must(f.sys.ID, recoverApply(i, tokenID(f.token), tokenID(second)))
	if a, err := f.authenticate(identity.Plaintext(i)); err != nil || a.ID != f.op.ID {
		t.Fatalf("the recovered token: %v %v", a.ID, err)
	}
	for name, s := range map[string]identity.Secret{"first": f.token, "second": second} {
		if _, err := f.authenticate(s.Reveal()); err == nil {
			t.Errorf("the %s revoked token still authenticates", name)
		}
	}
	if _, err := f.authenticate(supTok.Reveal()); err != nil {
		t.Errorf("another actor's token: %v", err)
	}
	// Without revocations, the operator's tokens stay.
	j := mintFor(t, f.op.ID, f.sys.ID)
	f.must(f.sys.ID, recoverApply(j))
	if _, err := f.authenticate(identity.Plaintext(i)); err != nil {
		t.Errorf("a recovery without revocations revoked: %v", err)
	}
	if !sameMap(actors, f.snapshot(identity.ActorProjection)) || !sameMap(boot, f.snapshot(identity.BootstrapProjection)) {
		t.Error("recovery changed an actor or the bootstrap")
	}
	// A revocation is stamped with the recovered token's created_at, exactly.
	var third identity.Secret
	f.must(f.op.ID, issue(f.op.ID, f.op.ID, &third))
	fixed, err := identity.Mint(f.op.ID, f.sys.ID, time.Date(2026, 10, 6, 12, 0, 0, 5, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	f.must(f.sys.ID, recoverApply(fixed, tokenID(third)))
	if at := liveRecord(f, tokenID(third)).RevokedAt; at == nil || *at != "2026-10-06T12:00:00.000000005Z" {
		t.Errorf("revoked_at = %s, want the recovered token's created_at", json.RawMessage(mustJSON(t, at)))
	}
	if err := f.submit(f.sys.ID, issue(f.op.ID, f.sys.ID, nil)); err == nil {
		t.Error("an ordinary system issuance after recovery: accepted")
	}
	f.rebuildsSame()
}

// The projector refuses a recovery that is not the system's, for a live
// operator, revoking only that operator's live tokens; a refused batch
// revokes nothing.
func TestRecoverTokenRefuses(t *testing.T) {
	f := open(t)
	sup := actor(t, types.ActorKindSupervisor, f.op.ID)
	gone := actor(t, types.ActorKindOperator, f.op.ID)
	for _, a := range []types.Actor{sup, gone} {
		f.must(f.op.ID, create(a))
	}
	var supTok, dead, goneTok identity.Secret
	f.must(f.op.ID, issue(sup.ID, f.op.ID, &supTok))
	f.must(f.op.ID, issue(f.op.ID, f.op.ID, &dead))
	f.must(f.op.ID, revokeToken(dead))
	f.must(f.op.ID, issue(gone.ID, f.op.ID, &goneTok))
	f.must(f.op.ID, func(tx *eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RevokeActor(tx, gone.ID, time.Now())
	})
	ghost := types.ActorID(id(t, "act"))
	moved := mintFor(t, f.op.ID, f.sys.ID)
	cheap := mintFor(t, f.op.ID, f.sys.ID)
	cheap.Token.Hash = identity.Hash("whatever", 1, 32, 4)
	live := tokenID(f.token)
	for name, c := range map[string]struct {
		by    types.ActorID
		apply applyFn
	}{
		"a missing recipient":               {f.sys.ID, recoverApply(mintFor(t, ghost, f.sys.ID))},
		"a revoked recipient":               {f.sys.ID, recoverApply(mintFor(t, gone.ID, f.sys.ID))},
		"a supervisor recipient":            {f.sys.ID, recoverApply(mintFor(t, sup.ID, f.sys.ID))},
		"the system actor as recipient":     {f.sys.ID, recoverApply(mintFor(t, f.sys.ID, f.sys.ID))},
		"an operator as issuer":             {f.op.ID, recoverApply(mintFor(t, f.op.ID, f.op.ID))},
		"a supervisor as issuer":            {sup.ID, recoverApply(mintFor(t, f.op.ID, sup.ID))},
		"created_by not the issuer":         {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.op.ID))},
		"a hash of another profile":         {f.sys.ID, recoverApply(cheap)},
		"another subject":                   {f.sys.ID, rawEvent(t, identity.TokenRecoveredKind, string(live), types.TokenRecovered{Token: moved.Token, Revoke: []types.TokenID{}})},
		"revoking another actor's token":    {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.sys.ID), tokenID(supTok))},
		"revoking a revoked token":          {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.sys.ID), tokenID(dead))},
		"revoking an unknown token":         {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.sys.ID), types.TokenID(id(t, "tok")))},
		"revoking a token twice":            {f.sys.ID, rawEvent(t, identity.TokenRecoveredKind, string(moved.Token.ID), types.TokenRecovered{Token: moved.Token, Revoke: []types.TokenID{live, live}})},
		"a live revocation and a bad one":   {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.sys.ID), live, tokenID(supTok))},
		"a bad revocation and a live one":   {f.sys.ID, recoverApply(mintFor(t, f.op.ID, f.sys.ID), tokenID(dead), live)},
		"revoking the token it recovers":    {f.sys.ID, recoverApply(moved, moved.Token.ID)},
		"a token that exists":               {f.sys.ID, rawEvent(t, identity.TokenRecoveredKind, string(live), types.TokenRecovered{Token: liveRecord(f, live), Revoke: []types.TokenID{}})},
		"an ordinary second system token":   {f.sys.ID, issue(f.op.ID, f.sys.ID, nil)},
		"a revoked operator's own recovery": {f.sys.ID, recoverApply(mintFor(t, gone.ID, f.sys.ID), tokenID(goneTok))},
	} {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A new token that is already revoked, in an otherwise valid recovery,
	// is refused and changes nothing.
	before := map[string]map[string]string{}
	for _, p := range []string{identity.ActorProjection, identity.TokenProjection, identity.BootstrapProjection} {
		before[p] = f.snapshot(p)
	}
	bornRevoked := mintFor(t, f.op.ID, f.sys.ID)
	at := bornRevoked.Token.CreatedAt
	bornRevoked.Token.RevokedAt = &at
	if err := f.submit(f.sys.ID, recoverApply(bornRevoked, live)); err == nil {
		t.Error("a recovered token issued revoked: accepted")
	}
	for p, snap := range before {
		if !sameMap(snap, f.snapshot(p)) {
			t.Errorf("a refused recovery changed %s", p)
		}
	}
	for name, s := range map[string]identity.Secret{"operator": f.token, "supervisor": supTok} {
		if _, err := f.authenticate(s.Reveal()); err != nil {
			t.Errorf("the %s token after refused recoveries: %v", name, err)
		}
	}
	f.rebuildsSame()
}

func liveRecord(f *fixture, id types.TokenID) types.Token {
	f.t.Helper()
	var tk types.Token
	if err := json.Unmarshal([]byte(f.snapshot(identity.TokenProjection)[string(id)]), &tk); err != nil {
		f.t.Fatal(err)
	}
	return tk
}

// Renewal replaces the bootstrap secret while no operator exists, with a
// 24 h expiry from when it was made; never after the bootstrap.
func TestRenewBootstrap(t *testing.T) {
	f := empty(t)
	sys := actor(t, types.ActorKindSystem, "")
	f.must(sys.ID, create(sys))
	hash := func() string { return mintFor(t, sys.ID, sys.ID).Token.Hash }
	renew := func(h string, now time.Time) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.RenewBootstrap(sys.ID, h, now) }
	}
	first, second := hash(), hash()
	f.must(sys.ID, renew(first, time.Now()))
	f.must(sys.ID, renew(second, time.Now()))
	var cur types.BootstrapRenewed
	if err := json.Unmarshal([]byte(f.snapshot(identity.BootstrapProjection)[identity.BootstrapSecretKey]), &cur); err != nil || cur.Hash != second {
		t.Fatalf("the current secret is not the latest: %v", err)
	}
	now := time.Now().UTC()
	stamp := func(d time.Duration) types.Time { return types.Time(now.Add(d).Format(time.RFC3339Nano)) }
	stranger := types.ActorID(id(t, "act"))
	cases := map[string]struct {
		by    types.ActorID
		apply applyFn
	}{
		"an unknown issuer":         {stranger, renew(hash(), now)},
		"a hash of another profile": {sys.ID, renew(identity.Hash("whatever", 1, 32, 4), now)},
		"another subject":           {sys.ID, rawEvent(t, identity.BootstrapRenewedKind, id(t, "act"), types.BootstrapRenewed{Hash: hash(), CreatedAt: stamp(0), ExpiresAt: stamp(24 * time.Hour)})},
		"an expiry not 24 h later":  {sys.ID, rawEvent(t, identity.BootstrapRenewedKind, string(sys.ID), types.BootstrapRenewed{Hash: hash(), CreatedAt: stamp(0), ExpiresAt: stamp(48 * time.Hour)})},
		"made after it is recorded": {sys.ID, renew(hash(), now.Add(time.Hour))},
		"recorded after it expires": {sys.ID, renew(hash(), now.Add(-25*time.Hour))},
	}
	for name, c := range cases {
		if err := f.submit(c.by, c.apply); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The first operator consumes the bootstrap: no renewal after it.
	op := actor(t, types.ActorKindOperator, sys.ID)
	f.must(sys.ID, create(op))
	if err := f.submit(sys.ID, renew(hash(), time.Now())); err == nil {
		t.Error("a renewal after the first operator: accepted")
	}
	f.must(sys.ID, issue(op.ID, sys.ID, nil))
	if err := f.submit(sys.ID, renew(hash(), time.Now())); err == nil {
		t.Error("a renewal after the bootstrap: accepted")
	}
	if err := f.submit(op.ID, renew(hash(), time.Now())); err == nil {
		t.Error("an operator's renewal: accepted")
	}
	f.rebuildsSame()
}

// seededKind is a test-only kind whose payload is put straight into the
// actor projection, so a projector test can start from a state the
// identity transitions alone would not reach.
const seededKind = "test.seeded/v1"

// isolated starts a store whose projector first applies seeded actors and,
// when *at is set, records every event at *at; it is never rebuilt.
func isolated(t *testing.T, at *string) *fixture {
	dir := filepath.Join(t.TempDir(), "state")
	lock, err := eventstore.AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release() })
	blobs, err := blobstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := identity.Kinds()
	kinds[seededKind] = eventstore.Kind{Schema: "records/actor"}
	project := func(tx *eventstore.Tx, evs []eventstore.Event) error {
		for i, ev := range evs {
			if ev.Kind == seededKind {
				if err := tx.Put(identity.ActorProjection, ev.Subject, ev.Payload); err != nil {
					return err
				}
			}
			if *at != "" {
				evs[i].At = *at
			}
		}
		return identity.Projector{}.Project(tx, evs)
	}
	s, err := eventstore.Open(context.Background(), lock, eventstore.Config{Kinds: kinds, Blobs: blobs,
		Hooks: eventstore.Hooks{Identify: func(*eventstore.Tx, eventstore.Command) error { return nil }, Authorize: func(*eventstore.Tx, eventstore.Command) error { return nil }, Project: project}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &fixture{t: t, s: s, dir: dir}
}

// Only the system actor renews: a live supervisor, before any bootstrap,
// renewing under its own subject with an admissible payload is refused.
// The renewal is accepted while the event is recorded before the expiry
// and from the moment the secret is made, and refused exactly at expiry.
func TestRenewBootstrapIssuerAndExpiry(t *testing.T) {
	var at string
	f := isolated(t, &at)
	sys := actor(t, types.ActorKindSystem, "")
	sup := actor(t, types.ActorKindSupervisor, sys.ID)
	for _, a := range []types.Actor{sys, sup} {
		f.must(a.ID, rawEvent(t, seededKind, string(a.ID), a))
	}
	made := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	hash := mintFor(t, sys.ID, sys.ID).Token.Hash
	renewal := func(by types.ActorID) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) { return identity.RenewBootstrap(by, hash, made) }
	}
	for _, c := range []struct {
		by     types.ActorID
		at     time.Time
		accept bool
	}{
		{sup.ID, made, false},
		{sys.ID, made.Add(-time.Nanosecond), false},
		{sys.ID, made, true},
		{sys.ID, made.Add(identity.BootstrapSecretExpiry - time.Nanosecond), true},
		{sys.ID, made.Add(identity.BootstrapSecretExpiry), false},
	} {
		at = c.at.Format(time.RFC3339Nano)
		if err := f.submit(c.by, renewal(c.by)); (err == nil) != c.accept {
			t.Errorf("a renewal by %s recorded at %s: accepted %v, want %v (%v)", c.by, at, err == nil, c.accept, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
