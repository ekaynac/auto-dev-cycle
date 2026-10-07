// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// ErrBusy: every verification slot of the listener is taken.
var ErrBusy = errors.New("api: too many authentications in progress")

// Authenticate verifies r's bearer through gate's verification slots, as
// a principal of the listener that admitted the connection (none recorded:
// no listener, which authenticates nothing). An unknown, revoked or
// malformed bearer is identity.ErrUnauthenticated.
func Authenticate(store *eventstore.Store, r *http.Request, gate *Slots) (identity.Principal, error) {
	leave, ok := gate.enter(r)
	if !ok {
		return identity.Principal{}, ErrBusy
	}
	defer leave()
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	peer, _ := PeerOf(r.Context())
	var p identity.Principal
	err := store.View(r.Context(), func(tx *eventstore.Tx) (err error) {
		p, err = identity.Authenticated(tx, bearer, peer.Via)
		return err
	})
	return p, err
}

// RefuseBusy is the fixed-text refusal of a full gate (ErrBusy).
func RefuseBusy(w http.ResponseWriter) { refuseBusy(w) }
