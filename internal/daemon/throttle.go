// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"maps"
	"sync"
	"time"
)

// throttle holds off a peer (api.Peer.Key) whose bearers keep failing, so
// wrong bearers cannot keep the verification slots busy with Argon2id:
// after free failures each further one doubles the peer's wait,
// from a second up to maxBackoff; a peer quiet for forgetAfter starts
// over. At most maxTracked peers are tracked: past that, an untracked
// peer's failures are not recorded, and the slots alone cap it (14 R3).
// A full table is swept for quiet peers at most once per sweepEvery, so
// failures from ever new peers cannot make each failure a full scan.
type throttle struct {
	mu     sync.Mutex
	free   int
	peers  map[any]*strikes
	now    func() time.Time
	swept  time.Time
	sweeps int // how many sweeps ran; tests read it
}

type strikes struct {
	n           int
	last, until time.Time
}

const (
	maxBackoff  = 30 * time.Second
	forgetAfter = 10 * time.Minute
	maxTracked  = 4096
	sweepEvery  = time.Second
)

func newThrottle(free int) *throttle {
	return &throttle{free: free, peers: map[any]*strikes{}, now: time.Now}
}

// wait is how long key is still held off; zero or less when it is not.
func (t *throttle) wait(key any) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.peers[key]; s != nil {
		return s.until.Sub(t.now())
	}
	return 0
}

// forgive clears key's failures when it is served (a 200) and its last
// failure is at least maxBackoff old, so clients behind one NAT are not
// left a single failure from a hold-off by earlier ones. A recent failure
// is kept: a caller holding one live bearer cannot interleave 200s with
// guesses (or race one against a failure) to stay clear of the hold-off,
// and a held-off key, whose last failure is under maxBackoff old, stays
// held off.
func (t *throttle) forgive(key any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.peers[key]; s != nil && t.now().Sub(s.last) >= maxBackoff {
		delete(t.peers, key)
	}
}

// fail records a failed authentication by key.
func (t *throttle) fail(key any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := t.peers[key]
	if s == nil || now.Sub(s.last) > forgetAfter {
		if s == nil && len(t.peers) >= maxTracked {
			if now.Sub(t.swept) < sweepEvery {
				return
			}
			t.swept = now
			t.sweeps++
			maps.DeleteFunc(t.peers, func(_ any, s *strikes) bool { return now.Sub(s.last) > forgetAfter })
			if len(t.peers) >= maxTracked {
				return
			}
		}
		s = &strikes{}
		t.peers[key] = s
	}
	s.n++
	s.last = now
	if s.n > t.free {
		s.until = now.Add(min(time.Second<<min(s.n-t.free-1, 5), maxBackoff))
	}
}
