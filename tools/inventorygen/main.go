// SPDX-License-Identifier: Apache-2.0

// Command inventorygen writes the invariant inventory's count line from its
// tables (internal/inventory; make gen): inventorygen IN OUT.
package main

import (
	"fmt"
	"os"

	"github.com/ekaynac/auto-dev-cycle/internal/inventory"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: inventorygen <inventory> <output file>")
		os.Exit(64)
	}
	doc, err := os.ReadFile(os.Args[1])
	if err == nil {
		doc, err = inventory.Rewrite(doc)
	}
	if err == nil {
		err = os.WriteFile(os.Args[2], doc, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "inventorygen:", err)
		os.Exit(1)
	}
}
