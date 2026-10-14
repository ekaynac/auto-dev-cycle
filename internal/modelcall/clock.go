// SPDX-License-Identifier: Apache-2.0

package modelcall

import (
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Clock ages this process's floors by its monotonic clock (#226 entry 77): a wall-clock step forward never makes an
// in-flight request's floor look older than FloorGrace. The Recorder notes each floor once committed, before its
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

// floored notes the first floor of call's request sequence; its count ends the request.
func (c *Clock) floored(call types.CallID, sequence int64, counted bool) {
	if key := floorKey(call, sequence); c != nil && counted {
		c.at.Delete(key)
	} else if c != nil {
		c.at.LoadOrStore(key, c.now())
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
