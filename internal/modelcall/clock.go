// SPDX-License-Identifier: Apache-2.0

package modelcall

import (
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Clock ages this process's floors by its monotonic clock (#226 entry 77): a wall-clock step forward never makes an
// in-flight request's floor look older than FloorGrace. The Recorder notes each floor as it commits, before its
// send; the Reconciler asks only once a floor this process recorded is old by the Clock too. An older floor's sender
// died with its process. P22a shares one Clock between them; nil keeps the recorded time alone.
type Clock struct {
	Now func() time.Time // nil: time.Now, with its monotonic reading
	at  sync.Map         // floor key → the time.Time this process committed it
}

func (c *Clock) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// noting notes the first floor of call's request sequence before it commits, so no read between the commit and the
// note finds it unnoted, and returns what follows the commit: a floor that failed is forgotten, and a committed
// count ends the request.
func (c *Clock) noting(call types.CallID, sequence int64, counted bool) func(committed bool) {
	key := floorKey(call, sequence)
	switch {
	case c == nil:
		return func(bool) {}
	case counted:
		return func(ok bool) {
			if ok {
				c.at.Delete(key)
			}
		}
	}
	_, loaded := c.at.LoadOrStore(key, c.now())
	return func(ok bool) {
		if !ok && !loaded {
			c.at.Delete(key)
		}
	}
}

// young: this process committed the floor at key less than FloorGrace ago.
func (c *Clock) young(key string) bool {
	if c == nil {
		return false
	}
	at, ok := c.at.Load(key)
	return ok && c.now().Sub(at.(time.Time)) < FloorGrace
}
