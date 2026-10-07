// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
)

// Slots caps concurrent holders in total and per peer (Peer.Key), so no one peer
// takes them all. Each listener has its own, so one listener's clients
// never starve another's. A taker that finds no slot is refused at once,
// never queued. Bearer verifications (an Argon2id hash, 64 MiB, each) and
// connections are capped this way.
type Slots struct {
	mu             sync.Mutex
	total, perPeer int // perPeer 0: no per-peer cap
	used           int
	byPeer         map[any]int // a uid, or Peer.Key
	unlimited      bool
}

// NewSlots is total slots (at least one), at most perPeer per peer.
func NewSlots(total, perPeer int) *Slots {
	return &Slots{total: max(total, 1), perPeer: perPeer, byPeer: map[any]int{}}
}

// Unlimited is Slots that cap nothing, for callers that bound load
// elsewhere: tests, and the temporary loopback fixture server that `adc
// dev fixtures --contract` starts (internal/fixtures). No daemon listener
// uses it.
func Unlimited() *Slots { return &Slots{unlimited: true} }

// Take takes a slot for peer (a uid, or Peer.Key): release gives it back
// (once, whatever the number of calls); false when none is free. Nil
// Slots admit no one.
func (s *Slots) Take(peer any) (release func(), ok bool) {
	switch {
	case s == nil:
		return nil, false
	case s.unlimited:
		return func() {}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used >= s.total || (s.perPeer > 0 && s.byPeer[peer] >= s.perPeer) {
		return nil, false
	}
	s.used++
	s.byPeer[peer]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.used--
			if s.byPeer[peer]--; s.byPeer[peer] == 0 {
				delete(s.byPeer, peer)
			}
		})
	}, true
}

// enter takes a verification slot for r's peer (-1 when none is recorded);
// none when the listener verified r's bearer (WithPrincipal).
func (s *Slots) enter(r *http.Request) (func(), bool) {
	if _, ok := principalOf(r); ok {
		return func() {}, true
	}
	return s.Take(SlotKey(r.Context()))
}

// SlotKey is the key ctx's peer takes slots under (Peer.Key), -1 when no
// peer is recorded: never a real uid's, so a caller without one cannot
// spend root's (uid 0) slots, nor root theirs.
func SlotKey(ctx context.Context) any {
	p, ok := PeerOf(ctx)
	if !ok {
		p.UID = -1
	}
	return p.Key()
}

// refuseBusy is the fixed-text refusal of a full gate.
func refuseBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "too many authentications in progress; retry", http.StatusServiceUnavailable)
}
