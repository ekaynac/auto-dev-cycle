// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"sync"
)

// Slots caps concurrent holders in total and per peer uid, so no one peer
// takes them all. Each listener has its own, so one listener's clients
// never starve another's. A taker that finds no slot is refused at once,
// never queued. Bearer verifications (an Argon2id hash, 64 MiB, each) and
// connections are capped this way.
type Slots struct {
	mu             sync.Mutex
	total, perPeer int // perPeer 0: no per-peer cap
	used           int
	byPeer         map[int]int
}

// NewSlots is total slots (at least one), at most perPeer per peer uid.
func NewSlots(total, perPeer int) *Slots {
	return &Slots{total: max(total, 1), perPeer: perPeer, byPeer: map[int]int{}}
}

// Take takes a slot for peer: release gives it back (once, whatever the
// number of calls); false when none is free. Nil Slots caps nothing.
func (s *Slots) Take(peer int) (release func(), ok bool) {
	if s == nil {
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

// enter takes a verification slot for r's peer (-1 when none is recorded).
func (s *Slots) enter(r *http.Request) (func(), bool) {
	p, ok := PeerOf(r.Context())
	if !ok {
		p.UID = -1
	}
	return s.Take(p.UID)
}

// refuseBusy is the fixed-text refusal of a full gate.
func refuseBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "too many authentications in progress; retry", http.StatusServiceUnavailable)
}
