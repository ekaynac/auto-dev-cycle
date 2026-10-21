// SPDX-License-Identifier: Apache-2.0

// Command inventorycount prints the invariant inventory's counts, by
// status, from its tables (internal/inventory; make inventory-count):
// inventorycount [INVENTORY].
package main

import (
	"fmt"
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/inventory"
)

func main() {
	path := inventory.Path
	switch len(os.Args) {
	case 1:
	case 2:
		path = os.Args[1]
	default:
		fmt.Fprintln(os.Stderr, "usage: inventorycount [inventory]")
		os.Exit(64)
	}
	doc, err := os.ReadFile(path)
	var c inventory.Counts
	if err == nil {
		c, err = inventory.Count(doc)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "inventorycount:", err)
		os.Exit(1)
	}
	fmt.Println(c.Line())
}
