// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// A fixed clock stands still until moved, and moves back as well as forward
// (a clock step), so a deadline or TTL can be crossed in either direction.
func TestFixed(t *testing.T) {
	c := Fixed(t0)
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now = %s, want %s", got, t0)
	}
	time.Sleep(time.Millisecond)
	if got := c.Now(); !got.Equal(t0) {
		t.Errorf("a fixed clock moved by itself to %s", got)
	}
	if got := c.Advance(time.Hour); !got.Equal(t0.Add(time.Hour)) || !c.Now().Equal(got) {
		t.Errorf("Advance(1h) = %s, Now = %s, want %s", got, c.Now(), t0.Add(time.Hour))
	}
	if got := c.Advance(-2 * time.Hour); !got.Equal(t0.Add(-time.Hour)) {
		t.Errorf("Advance(-2h) = %s, want %s", got, t0.Add(-time.Hour))
	}
	c.Set(t0)
	if got := c.Now(); !got.Equal(t0) {
		t.Errorf("after Set, Now = %s, want %s", got, t0)
	}
}

// A wall-following clock keeps running, at the lead the test gave it.
func TestWall(t *testing.T) {
	c := Wall()
	within := func(lead time.Duration) {
		t.Helper()
		before := time.Now().Add(lead)
		got := c.Now()
		after := time.Now().Add(lead)
		if got.Before(before) || got.After(after) {
			t.Errorf("Now = %s, want within [%s, %s]", got, before, after)
		}
	}
	within(0)
	c.Advance(2 * time.Hour)
	within(2 * time.Hour)
	c.Advance(-3 * time.Hour)
	within(-time.Hour)
	target := time.Now().Add(24 * time.Hour)
	c.Set(target)
	first := c.Now()
	if first.Before(target) || first.Sub(target) > time.Second {
		t.Errorf("after Set(%s), Now = %s", target, first)
	}
	time.Sleep(10 * time.Millisecond)
	if second := c.Now(); !second.After(first) {
		t.Errorf("a wall-following clock stopped after Set: %s, then %s", first, second)
	}
}

// Readers and movers may run on different goroutines (go test -race).
func TestConcurrent(t *testing.T) {
	for _, c := range []*Clock{Fixed(t0), Wall()} {
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 100 {
					c.Advance(time.Second)
					c.Now()
				}
			})
		}
		wg.Wait()
		if c.wall {
			continue
		}
		if got := c.Now(); !got.Equal(t0.Add(400 * time.Second)) {
			t.Errorf("concurrent advances lost: Now = %s, want %s", got, t0.Add(400*time.Second))
		}
	}
}
