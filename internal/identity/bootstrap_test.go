// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

func bootSecret(t *testing.T) (identity.Secret, string) {
	t.Helper()
	s, hash, err := identity.NewBootstrapSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s, hash
}

func (f *fixture) bootState() identity.BootstrapState {
	f.t.Helper()
	var st identity.BootstrapState
	f.view(func(tx *eventstore.Tx) (err error) {
		st, err = identity.BootstrapOf(tx)
		return err
	})
	return st
}

// The first secret is issued once, by the system actor, and is live until
// its expiry; a renewal may replace it; the first operator consumes it.
func TestIssueBootstrap(t *testing.T) {
	f := empty(t)
	sys := actor(t, types.ActorKindSystem, "")
	f.must(sys.ID, create(sys))
	_, first := bootSecret(t)
	_, second := bootSecret(t)
	issue := func(h string) applyFn {
		return func(*eventstore.Tx) ([]eventstore.Event, error) {
			return identity.IssueBootstrap(sys.ID, h, time.Now())
		}
	}
	if st := f.bootState(); st.Consumed || st.Secret != nil || st.Live(time.Now()) {
		t.Fatalf("none issued: %+v", st)
	}
	if err := f.submit(types.ActorID(id(t, "act")), issue(first)); err == nil {
		t.Fatal("an unknown issuer: accepted")
	}
	f.must(sys.ID, issue(first))
	if err := f.submit(sys.ID, issue(second)); err == nil {
		t.Fatal("a second first secret: accepted")
	}
	st := f.bootState()
	if st.Secret == nil || st.Secret.Hash != first || !st.Live(time.Now()) || st.Live(time.Now().Add(identity.BootstrapSecretExpiry)) {
		t.Fatalf("issued: %+v", st)
	}
	f.must(sys.ID, func(*eventstore.Tx) ([]eventstore.Event, error) {
		return identity.RenewBootstrap(sys.ID, second, time.Now())
	})
	if st := f.bootState(); st.Secret.Hash != second {
		t.Fatal("the renewal did not replace the secret")
	}
	f.must(sys.ID, create(actor(t, types.ActorKindOperator, sys.ID)))
	if st := f.bootState(); !st.Consumed || st.Live(time.Now()) {
		t.Fatalf("consumed: %+v", st)
	}
	f.rebuildsSame()
}

// The secret never prints.
func TestBootstrapSecretPrints(t *testing.T) {
	s, hash := bootSecret(t)
	if out := fmt.Sprintf("%v %s %+v %d", s, s, s, s); strings.Contains(out, s.Reveal()) || strings.Contains(hash, s.Reveal()) {
		t.Fatal("the secret printed")
	}
}
