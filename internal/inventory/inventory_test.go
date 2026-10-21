// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"os"
	"strings"
	"testing"
)

const sample = `# T

## I1. One

| Check | Today | Status | Item |
|---|---|---|---|
| admission | a | kept | — |
| fold/replay | b | GAP | P1 |
| effect | b2 | GAP | P1 |

## I2. Two

| Check | Today | Status | Item |
|---|---|---|---|
| admission | c | kept + GAP | P2 |
| effect | d | n/a | — |

## Count

| not | an | invariant | row |
`

// Count counts the invariants and their rows by status, never a table
// outside an invariant's section; an unknown status is refused.
func TestCount(t *testing.T) {
	c, err := Count([]byte(sample))
	want := "5 rows across 2 invariants. 3 rows have a gap: 2 are GAP and 1 are kept + GAP. 1 are kept and 1 are n/a."
	if err != nil || c.Line() != want {
		t.Fatalf("count: %v: %s", err, c.Line())
	}
	if _, err := Count([]byte(strings.Replace(sample, "| n/a |", "| maybe |", 1))); err == nil {
		t.Error("an unknown status was counted")
	}
}

// The committed inventory holds only known statuses, so make
// inventory-count counts it.
func TestTheInventoryCounts(t *testing.T) {
	doc, err := os.ReadFile("../../" + Path)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Count(doc); err != nil || c.Rows() == 0 || c.Invariants == 0 {
		t.Fatalf("the inventory: %+v, %v", c, err)
	}
}
