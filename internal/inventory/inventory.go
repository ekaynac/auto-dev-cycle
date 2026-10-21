// SPDX-License-Identifier: Apache-2.0

// Package inventory counts the rows of the invariant-enforcement inventory
// (docs/design/m1/01-invariant-inventory.md) by status, on demand (make
// inventory-count, tools/inventorycount). No count is committed: a figure
// kept in the file would conflict whenever two branches change a row's
// status, and the tables already say everything the count does.
package inventory

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// Path is the inventory, from the repository root.
const Path = "docs/design/m1/01-invariant-inventory.md"

// The statuses a row may hold (the inventory's "How to read this").
const (
	Kept       = "kept"
	Gap        = "GAP"
	KeptAndGap = "kept + GAP"
	NA         = "n/a"
)

// Counts are an inventory's invariants and its rows by status.
type Counts struct {
	Invariants int
	ByStatus   map[string]int
}

// Rows is every row.
func (c Counts) Rows() int {
	n := 0
	for _, v := range c.ByStatus {
		n += v
	}
	return n
}

// Line is the count's text.
func (c Counts) Line() string {
	return fmt.Sprintf("%d rows across %d invariants. %d rows have a gap: %d are GAP and %d are kept + GAP. %d are kept and %d are n/a.",
		c.Rows(), c.Invariants, c.ByStatus[Gap]+c.ByStatus[KeptAndGap], c.ByStatus[Gap], c.ByStatus[KeptAndGap], c.ByStatus[Kept], c.ByStatus[NA])
}

// Count counts doc's invariants (its "## I<n>." sections) and the rows of
// their tables by status (the third column). A row of another status is an
// error.
func Count(doc []byte) (Counts, error) {
	c := Counts{ByStatus: map[string]int{}}
	in := false
	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "## ") {
			in = strings.HasPrefix(l, "## I") && strings.Contains(l, ".")
			if in {
				c.Invariants++
			}
			continue
		}
		if !in || !strings.HasPrefix(l, "| ") || strings.HasPrefix(l, "| Check") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) < 4 {
			continue
		}
		switch s := strings.TrimSpace(cells[2]); s {
		case Kept, Gap, KeptAndGap, NA:
			c.ByStatus[s]++
		default:
			return c, fmt.Errorf("inventory: a row's status is %q: %s", s, l)
		}
	}
	return c, sc.Err()
}
