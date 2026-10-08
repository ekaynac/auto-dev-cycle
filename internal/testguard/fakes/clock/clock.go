// SPDX-License-Identifier: Apache-2.0

// Package clock is the fake clock of docs/design/m0/11 ("Contract-faithful
// fakes"): the time a test sets and moves, against which the code under test
// judges deadlines, resets, TTLs and challenge expiry. Now has the signature
// of every injected clock here (eventstore.Config.Now, api.Events.Now).
//
// M0 scope (15 D4): a fixed clock, or one that follows the wall clock at an
// offset, moved forward or back by the test. It has no timers: code that
// waits still waits in wall time, so a fixture that polls follows the wall
// clock and advances it to expire what it holds.
package clock

import (
	"sync"
	"time"
)

// Clock is safe for concurrent use.
type Clock struct {
	mu     sync.Mutex
	at     time.Time     // a fixed clock's time
	offset time.Duration // a wall-following clock's lead over the wall clock
	wall   bool
}

// Fixed returns a clock that reads at until the test moves it.
func Fixed(at time.Time) *Clock { return &Clock{at: at} }

// Wall returns a clock that follows the wall clock until the test moves it.
func Wall() *Clock { return &Clock{wall: true} }

// Now reads the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall {
		return time.Now().Add(c.offset)
	}
	return c.at
}

// Advance moves the clock by d, back when d is negative (a clock step), and
// returns the new reading.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall {
		c.offset += d
		return time.Now().Add(c.offset)
	}
	c.at = c.at.Add(d)
	return c.at
}

// Set moves the clock to t; a wall-following clock keeps following from there.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall {
		c.offset = time.Until(t)
	} else {
		c.at = t
	}
}
