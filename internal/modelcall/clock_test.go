// SPDX-License-Identifier: Apache-2.0

package modelcall

import (
	"testing"
	"time"
)

// L1 (Opus on #259): the Clock notes a floor before it commits; a floor that fails is forgotten, unless it was
// noted already; a committed count ends the request; a count that fails changes nothing.
func TestTheClockNotesAFloorBeforeItCommits(t *testing.T) {
	now := time.Now()
	c := &Clock{Now: func() time.Time { return now }}
	key := floorKey("cal_1", 0)
	committed := c.noting("cal_1", 0, false)
	if !c.young(key) {
		t.Fatal("a floor about to commit is not noted")
	}
	committed(false)
	if c.young(key) {
		t.Fatal("a floor that failed is still noted")
	}
	c.noting("cal_1", 0, false)(true)
	c.noting("cal_1", 0, false)(false) // a second floor at the sequence fails: the first stays
	c.noting("cal_1", 0, true)(false)  // a count that fails
	if !c.young(key) {
		t.Fatal("a committed floor was forgotten")
	}
	c.noting("cal_1", 0, true)(true)
	if c.young(key) {
		t.Fatal("a counted request is still noted")
	}
	var none *Clock
	none.noting("cal_1", 0, false)(true)
	if none.young(key) {
		t.Fatal("a nil Clock notes")
	}
}
