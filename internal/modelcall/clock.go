// SPDX-License-Identifier: Apache-2.0

package modelcall

import (
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Clock ages this process's floors by its monotonic clock (#226 entry 77): a wall-clock step forward never makes an
// in-flight request's floor look older than FloorGrace. The Recorder notes each floor before it commits: a pending
// floor is young, whatever its age, and its age starts when its commit succeeds. The gateway's request deadline
// starts before the floor (gateway.ServeHTTP), so the request ends within RequestDeadline of that stamp. The
// Reconciler asks only once a floor this process recorded is old by the Clock too. An older floor's sender died with
// its process. P22a shares one Clock between them; nil keeps the recorded time alone.
type Clock struct {
	Now func() time.Time // nil: time.Now, with its monotonic reading

	mu sync.Mutex
	at map[string]*note // floor key → its submissions in flight and its commit
}

// note is a floor key's state: submissions pending, and when one first committed.
type note struct {
	pending   int
	committed bool
	at        time.Time
}

func (c *Clock) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// noting registers a submission of call's request sequence before it commits, and returns what follows the commit:
// a floor's first successful commit stamps its age; a failure withdraws only its own submission, never another's
// commit; a committed count ends the request.
func (c *Clock) noting(call types.CallID, sequence int64, counted bool) func(committed bool) {
	key := floorKey(call, sequence)
	if c == nil {
		return func(bool) {}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if counted {
		return func(ok bool) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if ok {
				delete(c.at, key)
			}
		}
	}
	if c.at == nil {
		c.at = map[string]*note{}
	}
	n := c.at[key]
	if n == nil {
		n = &note{}
		c.at[key] = n
	}
	n.pending++
	return func(ok bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if n.pending--; ok && !n.committed {
			n.committed, n.at = true, c.now()
		}
		if n.pending == 0 && !n.committed && c.at[key] == n {
			delete(c.at, key)
		}
	}
}

// young: a floor of this process at key is pending, or committed less than FloorGrace ago.
func (c *Clock) young(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.at[key]
	return n != nil && (n.pending > 0 || n.committed && c.now().Sub(n.at) < FloorGrace)
}
