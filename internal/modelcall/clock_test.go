// SPDX-License-Identifier: Apache-2.0

package modelcall

import (
	"testing"
	"time"
)

// clockAt is a Clock reading t0 + *at, and the key of cal_1's request 0.
func clockAt(at *time.Duration) (*Clock, string) {
	t0 := time.Now()
	return &Clock{Now: func() time.Time { return t0.Add(*at) }}, floorKey("cal_1", 0)
}

// Astra on #259, H1: a delayed commit. The floor is noted at 0 and waits for the writer; it commits, and the request
// is sent, at 2 min. A pending floor is young however old; once committed, its age counts from the commit, so at 11
// min, with a wall clock stepped forward and the request still outstanding, it is young; at 13 min it is old.
func TestAFloorAgesFromItsCommit(t *testing.T) {
	var at time.Duration
	c, key := clockAt(&at)
	committed := c.noting("cal_1", 0, false)
	if at = 11 * time.Minute; !c.young(key) {
		t.Fatal("a pending floor is old")
	}
	at = 2 * time.Minute
	committed(true)
	for _, k := range []struct {
		at    time.Duration
		young bool
	}{{11 * time.Minute, true}, {13*time.Minute + time.Second, false}} {
		if at = k.at; c.young(key) != k.young {
			t.Errorf("at %v: young %v", k.at, !k.young)
		}
	}
}

// Astra on #259, M2 (and Opus L1): overlapping submissions of one floor key. A failure withdraws only its own
// submission: it never deletes the protection another, successful submission holds, committed or pending. A floor
// whose every submission failed is forgotten; a committed count ends the request; a nil Clock notes nothing.
func TestOverlappingSubmissionsOfOneFloor(t *testing.T) {
	var at time.Duration
	c, key := clockAt(&at)
	a, b := c.noting("cal_1", 0, false), c.noting("cal_1", 0, false)
	b(true)
	a(false)
	if !c.young(key) {
		t.Fatal("a failed submission deleted another's committed floor")
	}
	c2, key2 := clockAt(&at)
	a, b = c2.noting("cal_1", 0, false), c2.noting("cal_1", 0, false)
	a(false)
	if !c2.young(key2) {
		t.Fatal("a failed submission deleted another's pending floor")
	}
	if b(false); c2.young(key2) {
		t.Fatal("a floor whose every submission failed is noted")
	}
	c.noting("cal_1", 0, true)(false)
	if !c.young(key) {
		t.Fatal("a failed count ended the request")
	}
	if c.noting("cal_1", 0, true)(true); c.young(key) {
		t.Fatal("a counted request is still noted")
	}
	var none *Clock
	if none.noting("cal_1", 0, false)(true); none.young(key) {
		t.Fatal("a nil Clock notes")
	}
}
