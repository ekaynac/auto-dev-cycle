// SPDX-License-Identifier: Apache-2.0

package api

import "net/http"

// AuthGate caps concurrent bearer verifications: each costs an Argon2id
// hash (64 MiB, identity), so unbounded concurrency is a memory exhaustion
// any client could cause. A request that finds every slot taken is refused
// at once, never queued.
type AuthGate chan struct{}

// NewAuthGate is a gate of n slots (at least one).
func NewAuthGate(n int) AuthGate { return make(AuthGate, max(n, 1)) }

// enter takes a slot without waiting: false when none is free. A nil gate
// caps nothing.
func (g AuthGate) enter() (leave func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	select {
	case g <- struct{}{}:
		return func() { <-g }, true
	default:
		return nil, false
	}
}

// refuseBusy is the fixed-text refusal of a full gate.
func refuseBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "too many authentications in progress; retry", http.StatusServiceUnavailable)
}
